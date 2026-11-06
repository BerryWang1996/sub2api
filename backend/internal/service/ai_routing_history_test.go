//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func aiHistoryTestBody(t *testing.T, messages ...map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"model": "public-alias", "messages": messages})
	require.NoError(t, err)
	return body
}

func aiHistoryTestMessage(role, text string) map[string]any {
	return map[string]any{"role": role, "content": text}
}

func aiHistoryTestLimits() AIRoutingContextLimits {
	return AIRoutingContextLimits{MaxInputBytes: 8192, MaxSegments: 128, MaxDecisionCalls: 129}
}

func aiHistoryTestDecode(t *testing.T, task string) aiHistoryTask {
	t.Helper()
	var view aiHistoryTask
	require.NoError(t, json.Unmarshal([]byte(task), &view))
	return view
}

func aiHistoryTestReason(t *testing.T, err error, reason string) {
	t.Helper()
	var historyErr *AIRoutingHistoryError
	require.ErrorAs(t, err, &historyErr)
	require.Equal(t, reason, historyErr.Reason)
}

func TestAIRoutingHistoryBudgets(t *testing.T) {
	defaults, err := NormalizeAIRoutingContextLimits(AIRoutingContextLimits{})
	require.NoError(t, err)
	require.Equal(t, AIRoutingContextLimits{MaxHistoryBytes: 128 << 10, MaxHistoryTurns: 256, MaxSegments: 32, MaxDecisionCalls: 33, MaxParallelCalls: 1, MaxInputBytes: 24 << 10, MaxTotalInputBytes: 512 << 10, MaxOutputBytes: 64 << 10, MaxTotalOutputBytes: 2 << 20}, defaults)
	for _, invalid := range []AIRoutingContextLimits{
		{MaxHistoryBytes: -1}, {MaxHistoryTurns: -1}, {MaxSegments: 257},
		{MaxDecisionCalls: -1}, {MaxParallelCalls: 9}, {MaxInputBytes: 1023},
		{MaxInputBytes: 256<<10 + 1}, {MaxTotalInputBytes: 1024}, {MaxRefinementPasses: 1}, {MaxRefinementPasses: -1},
		{MaxOutputBytes: 1023}, {MaxOutputBytes: aiDecisionMaxResponse + 1}, {MaxTotalOutputBytes: 1024},
	} {
		_, err := NormalizeAIRoutingContextLimits(invalid)
		aiHistoryTestReason(t, err, "invalid_context_budget")
	}
}

func TestAIRoutingHistoryParseVisibleTextAndStableReferences(t *testing.T) {
	body := []byte(`{"model":"auto","system":"system-private","tools":[{"name":"shell","description":"tool-schema-private"}],"messages":[{"role":"user","content":[{"type":"text","text":"first requirement"},{"type":"image","source":{"data":"image-private"}}]},{"role":"assistant","content":[{"type":"thinking","thinking":"hidden-private"},{"type":"tool_use","id":"call-1","name":"shell","input":{"text":"input-private"}},{"type":"text","text":"visible proposal"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"tool-private"},{"type":"text","text":"password=secretvalue"}]},{"role":"user","content":"continue"},{"role":"assistant","content":"visible prefill"}]}`)
	before := append([]byte(nil), body...)
	h, err := ParseAIRoutingMessagesHistory(body, AIRoutingContextLimits{})
	require.NoError(t, err)
	require.Equal(t, before, body)
	require.Equal(t, 4, h.HistoryTurns)
	require.Equal(t, "t3", h.CurrentTurnID)
	require.Equal(t, 4, h.ExcludedBlocks)
	require.Equal(t, 1, h.RedactedSpans)
	require.Len(t, h.Spans, 5)
	require.Equal(t, "m1b0:0-17", h.Spans[0].ID)
	require.Equal(t, "t3", h.Spans[2].TurnID)
	require.Equal(t, "t3", h.Spans[3].TurnID)
	require.Equal(t, "assistant", h.Spans[4].Role)
	encoded, err := json.Marshal(h.Spans)
	require.NoError(t, err)
	for _, secret := range []string{"system-private", "tool-schema-private", "image-private", "hidden-private", "input-private", "tool-private", "secretvalue"} {
		require.NotContains(t, string(encoded), secret)
	}
	require.Contains(t, string(encoded), "visible proposal")
	require.Equal(t, "[redacted]", h.Spans[2].Text)
	repeated, err := ParseAIRoutingMessagesHistory(body, AIRoutingContextLimits{})
	require.NoError(t, err)
	require.Equal(t, h, repeated)
}

func TestAIRoutingHistoryParseRejectsAmbiguousAndOverLimitInputs(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`{"model":"x","messages":[{"role":"user","content":[{"type":"text","text":7}]}]}`),
		[]byte(`{"model":"x","messages":[{"role":"user","content":[{"type":"text","text":"a","Text":"b"}]}]}`),
		[]byte(`{"model":"x","messages":[{"role":"user","content":"a","content":"b"}]}`),
		append([]byte(`{"model":"x","messages":[{"role":"user","content":"`), 0xff),
	} {
		_, err := ParseAIRoutingMessagesHistory(raw, AIRoutingContextLimits{})
		aiHistoryTestReason(t, err, "history_invalid_request")
	}
	body := aiHistoryTestBody(t, aiHistoryTestMessage("user", "first"), aiHistoryTestMessage("assistant", "next"), aiHistoryTestMessage("user", "last"))
	_, err := ParseAIRoutingMessagesHistory(body, AIRoutingContextLimits{MaxHistoryTurns: 2})
	aiHistoryTestReason(t, err, "history_turn_limit")
	_, err = ParseAIRoutingMessagesHistory(body, AIRoutingContextLimits{MaxHistoryBytes: 3})
	aiHistoryTestReason(t, err, "history_byte_limit")
	// The raw visible input budget applies before redaction, not after a large
	// secret-bearing block has collapsed to a short marker.
	body = aiHistoryTestBody(t, aiHistoryTestMessage("user", strings.Repeat("x", 200)+"-----BEGIN PRIVATE KEY-----"))
	_, err = ParseAIRoutingMessagesHistory(body, AIRoutingContextLimits{MaxHistoryBytes: 100})
	aiHistoryTestReason(t, err, "history_byte_limit")
}

func TestAIRoutingHistoryCompleteInputSkipsEvidenceCalls(t *testing.T) {
	body := aiHistoryTestBody(t, aiHistoryTestMessage("user", "audit payments"), aiHistoryTestMessage("assistant", "plan"), aiHistoryTestMessage("user", "correction: only translate"), aiHistoryTestMessage("assistant", "ok"), aiHistoryTestMessage("user", "continue"))
	h, err := ParseAIRoutingMessagesHistory(body, AIRoutingContextLimits{})
	require.NoError(t, err)
	r, err := BuildAIRoutingHistoryTask(context.Background(), h, AIRoutingContextLimits{MaxDecisionCalls: 1}, 1000, func(context.Context, AIRoutingHistoryClassificationRequest) (AIRoutingHistoryRelevance, error) {
		t.Fatal("complete history must not call an evidence model")
		return "", nil
	})
	require.NoError(t, err)
	require.Equal(t, "full", r.Diagnostics.Mode)
	require.Equal(t, "complete", r.Diagnostics.Coverage)
	require.Zero(t, r.Diagnostics.ClassifierCalls)
	require.Equal(t, 1, r.RemainingCalls)
	require.Equal(t, h.Spans, aiHistoryTestDecode(t, r.Task).Spans)
	// Public serialization cannot accidentally write the request text to traces.
	encoded, err := json.Marshal(r)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "payments")
	require.NotContains(t, string(encoded), "correction")
	require.Contains(t, string(encoded), "selected_span_ids")
}

func TestAIRoutingHistoryEvidenceScansHeadMiddleTailAndKeepsOrder(t *testing.T) {
	for _, position := range []int{0, 12, 23} {
		t.Run(fmt.Sprint(position), func(t *testing.T) {
			var messages []map[string]any
			for i := 0; i < 24; i++ {
				text := fmt.Sprintf("turn %02d ", i) + strings.Repeat("noise ", 120)
				if i == position {
					text += "NECESSARY_EVIDENCE"
				}
				role := "user"
				if i%2 != 0 {
					role = "assistant"
				}
				messages = append(messages, aiHistoryTestMessage(role, text))
			}
			messages = append(messages, aiHistoryTestMessage("user", "continue resolving the incident"))
			limits := aiHistoryTestLimits()
			h, err := ParseAIRoutingMessagesHistory(aiHistoryTestBody(t, messages...), limits)
			require.NoError(t, err)
			seen := map[string]string{}
			calls := 0
			r, err := BuildAIRoutingHistoryTask(context.Background(), h, limits, 300, func(_ context.Context, request AIRoutingHistoryClassificationRequest) (AIRoutingHistoryRelevance, error) {
				calls++
				require.Equal(t, fmt.Sprintf("s%d", calls), request.SegmentID)
				require.LessOrEqual(t, len(request.Task)+300, limits.MaxInputBytes)
				view := aiHistoryTestDecode(t, request.Task)
				require.Equal(t, "history_relevance", view.Kind)
				require.Equal(t, "continue resolving the incident", view.CurrentRequest)
				relevant := false
				for _, span := range view.Spans {
					require.NotContains(t, seen, span.ID)
					seen[span.ID] = span.Text
					relevant = relevant || strings.Contains(span.Text, "NECESSARY_EVIDENCE")
				}
				if relevant {
					return AIRoutingHistoryRelevant, nil
				}
				return AIRoutingHistoryIrrelevant, nil
			})
			require.NoError(t, err)
			require.Equal(t, len(h.Spans), len(seen))
			require.Equal(t, "complete", r.Diagnostics.Coverage)
			require.Equal(t, calls, r.Diagnostics.ScannedSegments)
			require.GreaterOrEqual(t, r.RemainingCalls, 1)
			require.GreaterOrEqual(t, r.RemainingInputBytes, limits.MaxInputBytes)
			view := aiHistoryTestDecode(t, r.Task)
			require.Equal(t, "route", view.Kind)
			require.Contains(t, r.Task, "NECESSARY_EVIDENCE")
			previous := -1
			for _, span := range view.Spans {
				index := -1
				for i, original := range h.Spans {
					if original.ID == span.ID {
						index = i
						require.Equal(t, original, span)
						break
					}
				}
				require.Greater(t, index, previous)
				previous = index
			}
		})
	}
}

func TestAIRoutingHistoryUTF8SplitHasNoGapsAndPreservesSource(t *testing.T) {
	text := strings.Repeat("中文😀\"\n", 2000)
	limits := aiHistoryTestLimits()
	h, err := ParseAIRoutingMessagesHistory(aiHistoryTestBody(t, aiHistoryTestMessage("user", text), aiHistoryTestMessage("assistant", "continue the earlier task"), aiHistoryTestMessage("user", "继续")), limits)
	require.NoError(t, err)
	original := append([]AIRoutingHistorySpan(nil), h.Spans...)
	rebuilt := map[string]string{}
	r, err := BuildAIRoutingHistoryTask(context.Background(), h, limits, 300, func(_ context.Context, request AIRoutingHistoryClassificationRequest) (AIRoutingHistoryRelevance, error) {
		for _, span := range aiHistoryTestDecode(t, request.Task).Spans {
			require.True(t, utf8.ValidString(span.Text))
			require.Equal(t, len(rebuilt[span.SourceID]), span.StartByte)
			require.Equal(t, span.StartByte+len(span.Text), span.EndByte)
			rebuilt[span.SourceID] += span.Text
		}
		return AIRoutingHistoryIrrelevant, nil
	})
	aiHistoryTestReason(t, err, "context_insufficient")
	require.Equal(t, "complete", r.Diagnostics.Coverage)
	require.Empty(t, r.Task)
	for _, span := range h.Spans {
		require.Equal(t, span.Text, rebuilt[span.SourceID])
	}
	require.Equal(t, original, h.Spans)
}

func TestAIRoutingHistoryUncertainEvidenceAndNeighborsAreRetained(t *testing.T) {
	var messages []map[string]any
	for i := 0; i < 24; i++ {
		messages = append(messages, aiHistoryTestMessage("assistant", fmt.Sprintf("source %d %s", i, strings.Repeat("noise ", 120))))
	}
	messages = append(messages, aiHistoryTestMessage("user", "Review the earlier proposal"))
	limits := aiHistoryTestLimits()
	h, err := ParseAIRoutingMessagesHistory(aiHistoryTestBody(t, messages...), limits)
	require.NoError(t, err)
	var chosen []AIRoutingHistorySpan
	r, err := BuildAIRoutingHistoryTask(context.Background(), h, limits, 300, func(_ context.Context, request AIRoutingHistoryClassificationRequest) (AIRoutingHistoryRelevance, error) {
		if request.SegmentID == "s3" {
			chosen = aiHistoryTestDecode(t, request.Task).Spans
			return AIRoutingHistoryUncertain, nil
		}
		return AIRoutingHistoryIrrelevant, nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, chosen)
	selected := aiHistoryTestDecode(t, r.Task).Spans
	for _, span := range chosen {
		require.Contains(t, selected, span)
	}
	require.Greater(t, len(selected), len(chosen))
}

func TestAIRoutingHistoryBudgetFailureBeforeExternalCalls(t *testing.T) {
	body := aiHistoryTestBody(t, aiHistoryTestMessage("user", strings.Repeat("old ", 6000)), aiHistoryTestMessage("assistant", "plan"), aiHistoryTestMessage("user", "continue"))
	for _, test := range []struct {
		name, reason string
		limits       AIRoutingContextLimits
		reserved     int
	}{
		{"segments", "history_segment_limit", AIRoutingContextLimits{MaxInputBytes: 8192, MaxSegments: 1}, 0},
		{"calls", "history_budget_exhausted", AIRoutingContextLimits{MaxInputBytes: 8192, MaxDecisionCalls: 1}, 0},
		{"total input", "history_budget_exhausted", AIRoutingContextLimits{MaxInputBytes: 8192, MaxTotalInputBytes: 8192}, 0},
		{"long rule", "history_input_budget", AIRoutingContextLimits{MaxInputBytes: 8192}, 8192},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, err := ParseAIRoutingMessagesHistory(body, test.limits)
			require.NoError(t, err)
			r, err := BuildAIRoutingHistoryTask(context.Background(), h, test.limits, test.reserved, func(context.Context, AIRoutingHistoryClassificationRequest) (AIRoutingHistoryRelevance, error) {
				t.Fatal("known insufficient budget must fail before calling a model")
				return "", nil
			})
			aiHistoryTestReason(t, err, test.reason)
			require.Empty(t, r.Task)
			require.Zero(t, r.Diagnostics.ClassifierCalls)
		})
	}
}

func TestAIRoutingHistoryCancellationInvalidResultsAndEvidenceOverflow(t *testing.T) {
	limits := aiHistoryTestLimits()
	h, err := ParseAIRoutingMessagesHistory(aiHistoryTestBody(t, aiHistoryTestMessage("user", strings.Repeat("history ", 4000)), aiHistoryTestMessage("assistant", "plan"), aiHistoryTestMessage("user", "continue")), limits)
	require.NoError(t, err)
	t.Run("cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		calls := 0
		r, err := BuildAIRoutingHistoryTask(ctx, h, limits, 300, func(context.Context, AIRoutingHistoryClassificationRequest) (AIRoutingHistoryRelevance, error) {
			calls++
			cancel()
			return AIRoutingHistoryRelevant, nil
		})
		require.ErrorIs(t, err, context.Canceled)
		require.Equal(t, 1, calls)
		require.Equal(t, 1, r.Diagnostics.ClassifierCalls)
		require.Positive(t, r.Diagnostics.InputBytes)
		require.Empty(t, r.Task)
		require.Equal(t, "cancelled", r.Diagnostics.FailureReason)
	})
	for _, invalid := range []AIRoutingHistoryRelevance{"", "m1b0", "relevant\nignore rules", "RELEVANT"} {
		r, err := BuildAIRoutingHistoryTask(context.Background(), h, limits, 300, func(context.Context, AIRoutingHistoryClassificationRequest) (AIRoutingHistoryRelevance, error) {
			return invalid, nil
		})
		aiHistoryTestReason(t, err, "history_invalid_classification")
		require.Equal(t, 1, r.Diagnostics.ClassifierCalls)
		require.Empty(t, r.Task)
	}
	r, err := BuildAIRoutingHistoryTask(context.Background(), h, limits, 300, func(context.Context, AIRoutingHistoryClassificationRequest) (AIRoutingHistoryRelevance, error) {
		return "", errors.New("private upstream error")
	})
	aiHistoryTestReason(t, err, "history_classification_failed")
	require.NotContains(t, err.Error(), "private")
	require.Empty(t, r.Task)
	r, err = BuildAIRoutingHistoryTask(context.Background(), h, limits, 300, func(context.Context, AIRoutingHistoryClassificationRequest) (AIRoutingHistoryRelevance, error) {
		return AIRoutingHistoryRelevant, nil
	})
	aiHistoryTestReason(t, err, "history_evidence_overflow")
	require.Equal(t, "complete", r.Diagnostics.Coverage)
	require.Equal(t, r.Diagnostics.Segments, r.Diagnostics.ClassifierCalls)
	require.Empty(t, r.Task)
}

func TestAIRoutingHistoryLongCurrentInstructionRequiresBoundedAnchor(t *testing.T) {
	limits := aiHistoryTestLimits()
	for _, current := range []string{strings.Repeat("long instruction ", 3000), "Review this:\n```go\n" + strings.Repeat("code data ", 3000), "```\n" + strings.Repeat("code data ", 3000) + "\n```"} {
		h, err := ParseAIRoutingMessagesHistory(aiHistoryTestBody(t, aiHistoryTestMessage("user", current)), limits)
		require.NoError(t, err)
		r, err := BuildAIRoutingHistoryTask(context.Background(), h, limits, 300, nil)
		aiHistoryTestReason(t, err, "history_anchor_unavailable")
		require.Zero(t, r.Diagnostics.ClassifierCalls)
	}
	current := "Review locking in this code.\n````go\n" + strings.Repeat("code data\n", 3000) + "```\nnot a matching fence\n````\nKeep the public API."
	h, err := ParseAIRoutingMessagesHistory(aiHistoryTestBody(t, aiHistoryTestMessage("user", current)), limits)
	require.NoError(t, err)
	covered := ""
	r, err := BuildAIRoutingHistoryTask(context.Background(), h, limits, 300, func(_ context.Context, request AIRoutingHistoryClassificationRequest) (AIRoutingHistoryRelevance, error) {
		view := aiHistoryTestDecode(t, request.Task)
		require.Equal(t, "Review locking in this code.\nKeep the public API.", view.CurrentRequest)
		for _, span := range view.Spans {
			covered += span.Text
		}
		return AIRoutingHistoryIrrelevant, nil
	})
	aiHistoryTestReason(t, err, "context_insufficient")
	require.Equal(t, current, covered)
	require.Equal(t, "complete", r.Diagnostics.Coverage)
}

func TestAIRoutingHistoryMissingCurrentTextCannotRouteOnOldTask(t *testing.T) {
	body := aiHistoryTestBody(t, aiHistoryTestMessage("user", "old task"), aiHistoryTestMessage("assistant", "old reply"), aiHistoryTestMessage("user", ""))
	h, err := ParseAIRoutingMessagesHistory(body, AIRoutingContextLimits{})
	require.NoError(t, err)
	r, err := BuildAIRoutingHistoryTask(context.Background(), h, AIRoutingContextLimits{}, 100, nil)
	aiHistoryTestReason(t, err, "context_insufficient")
	require.Empty(t, r.Task)
}

func TestAIRoutingHistoryWireSizerCountsNestedJSONEscaping(t *testing.T) {
	limits := aiHistoryTestLimits()
	// This is the real generic envelope: the already-encoded task is encoded
	// into state.task, then the encoded state is encoded again as message text.
	// Quotes and backslashes make a factor-of-two shortcut underestimate it.
	wireSize := func(task string) int {
		state, err := json.Marshal(map[string]any{"task": task, "candidates": []string{"c1", "c2"}})
		require.NoError(t, err)
		body, err := json.Marshal(map[string]any{"model": "synthetic", "system": strings.Repeat("rule ", 30), "messages": []any{map[string]string{"role": "user", "content": string(state)}}})
		require.NoError(t, err)
		return len(body)
	}
	escaped := strings.Repeat("\\\" ", 1000)
	body := aiHistoryTestBody(t, aiHistoryTestMessage("user", escaped), aiHistoryTestMessage("assistant", "planned work"), aiHistoryTestMessage("user", "continue"))
	h, err := ParseAIRoutingMessagesHistory(body, limits)
	require.NoError(t, err)
	withoutSizer, err := BuildAIRoutingHistoryTask(context.Background(), h, limits, wireSize(""), nil)
	require.NoError(t, err)
	require.Equal(t, "full", withoutSizer.Diagnostics.Mode)
	require.Greater(t, wireSize(withoutSizer.Task), limits.MaxInputBytes)
	var spent int
	var reconstructed strings.Builder
	r, err := BuildAIRoutingHistoryTask(context.Background(), h, limits, wireSize(""), func(_ context.Context, request AIRoutingHistoryClassificationRequest) (AIRoutingHistoryRelevance, error) {
		cost := wireSize(request.Task)
		require.LessOrEqual(t, cost, limits.MaxInputBytes)
		spent += cost
		for _, span := range aiHistoryTestDecode(t, request.Task).Spans {
			if span.SourceID == "m1b0" {
				_, _ = reconstructed.WriteString(span.Text)
			}
		}
		if request.SegmentID == "s2" {
			return AIRoutingHistoryRelevant, nil
		}
		return AIRoutingHistoryIrrelevant, nil
	}, wireSize)
	require.NoError(t, err)
	require.Equal(t, "evidence", r.Diagnostics.Mode)
	require.Equal(t, escaped, reconstructed.String())
	require.LessOrEqual(t, wireSize(r.Task), limits.MaxInputBytes)
	require.Equal(t, spent, r.Diagnostics.InputBytes)
	defaults, err := NormalizeAIRoutingContextLimits(limits)
	require.NoError(t, err)
	require.Equal(t, defaults.MaxTotalInputBytes-spent, r.RemainingInputBytes)
	require.GreaterOrEqual(t, r.RemainingInputBytes, limits.MaxInputBytes)

	// Cumulative wire cost plus the reserved final call is checked before even
	// the first classifier attempt; task bytes alone would undercount it.
	limited := limits
	limited.MaxTotalInputBytes = spent + limits.MaxInputBytes - 1
	r, err = BuildAIRoutingHistoryTask(context.Background(), h, limited, wireSize(""), func(context.Context, AIRoutingHistoryClassificationRequest) (AIRoutingHistoryRelevance, error) {
		t.Fatal("cumulative wire budget must fail before an external attempt")
		return "", nil
	}, wireSize)
	aiHistoryTestReason(t, err, "history_budget_exhausted")
	require.Empty(t, r.Task)
	require.Zero(t, r.Diagnostics.ClassifierCalls)
}

func TestAIRoutingHistoryWireSizerFinalFitAndInvalidSizer(t *testing.T) {
	limits := aiHistoryTestLimits()
	body := aiHistoryTestBody(t, aiHistoryTestMessage("user", strings.Repeat("historical material ", 1500)), aiHistoryTestMessage("assistant", "plan"), aiHistoryTestMessage("user", "continue"))
	h, err := ParseAIRoutingMessagesHistory(body, limits)
	require.NoError(t, err)
	// A final-stage wrapper can be larger than a classification wrapper. This
	// exercises the separate final evidence-fit check, not just segmentation.
	size := func(task string) int {
		if aiHistoryTestDecode(t, task).Kind == "route" {
			return len(task) + limits.MaxInputBytes
		}
		return len(task) + 300
	}
	r, err := BuildAIRoutingHistoryTask(context.Background(), h, limits, 300, func(_ context.Context, request AIRoutingHistoryClassificationRequest) (AIRoutingHistoryRelevance, error) {
		if request.SegmentID == "s2" {
			return AIRoutingHistoryRelevant, nil
		}
		return AIRoutingHistoryIrrelevant, nil
	}, size)
	aiHistoryTestReason(t, err, "history_evidence_overflow")
	require.Empty(t, r.Task)
	require.Equal(t, "complete", r.Diagnostics.Coverage)
	for _, invalid := range []int{-1, 0, int(^uint(0) >> 1)} {
		r, err = BuildAIRoutingHistoryTask(context.Background(), h, limits, 300, func(context.Context, AIRoutingHistoryClassificationRequest) (AIRoutingHistoryRelevance, error) {
			t.Fatal("invalid wire sizing must fail before an external attempt")
			return "", nil
		}, func(string) int { return invalid })
		require.Error(t, err)
		require.Empty(t, r.Task)
		require.Zero(t, r.Diagnostics.ClassifierCalls)
	}
}
