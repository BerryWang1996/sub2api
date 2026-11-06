//go:build unit

package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAIRoutingResponsesHistoryVisibleTextAndProtocolVersion(t *testing.T) {
	body := aiResponsesTestBody(t, []any{
		map[string]any{"role": "developer", "content": "private-developer"},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "earlier requirement"}, map[string]any{"type": "input_image", "image_url": "https://synthetic.invalid/private-image"}}},
		map[string]any{"type": "reasoning", "id": "rs_x", "encrypted_content": "private-cipher", "summary": []any{map[string]any{"type": "summary_text", "text": "private-summary"}}},
		map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "second option is reasoning", "annotations": []any{map[string]any{"text": "private-annotation"}}}}},
		map[string]any{"type": "function_call", "call_id": "call_x", "name": "f", "arguments": `{"secret":"private-arguments"}`},
		map[string]any{"type": "function_call_output", "call_id": "call_x", "output": "private-tool-result"},
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "refusal", "refusal": "visible refusal"}}},
		map[string]any{"role": "user", "content": "  now use the second option password=private-password  "},
	}, map[string]any{"instructions": "private-instructions", "tools": []any{map[string]any{"type": "function", "name": "f", "description": "private-schema"}}})
	before := append([]byte(nil), body...)
	history, err := ParseAIRoutingResponsesHistory(body, AIRoutingContextLimits{})
	require.NoError(t, err)
	require.Equal(t, before, body)
	require.Equal(t, AIRoutingResponsesHistoryProjectionVersion, history.ProjectionVersion)
	require.Len(t, history.Spans, 4)
	require.Equal(t, "r2b0", history.Spans[0].SourceID)
	require.Equal(t, "r4b0", history.Spans[1].SourceID)
	require.Equal(t, "r7b0", history.Spans[2].SourceID)
	require.Equal(t, "r8b0", history.Spans[3].SourceID)
	require.Equal(t, history.CurrentTurnID, history.Spans[3].TurnID)
	require.Equal(t, 1, history.RedactedSpans)
	require.Equal(t, 5, history.ExcludedBlocks)
	prepared, err := BuildAIRoutingHistoryTask(context.Background(), history, AIRoutingContextLimits{}, 100, nil)
	require.NoError(t, err)
	require.Equal(t, AIRoutingResponsesHistoryProjectionVersion, prepared.Diagnostics.ProjectionVersion)
	require.Equal(t, AIRoutingResponsesHistoryProjectionVersion, aiHistoryTestDecode(t, prepared.Task).ProjectionVersion)
	for _, secret := range []string{"private-developer", "private-image", "private-cipher", "private-summary", "private-annotation", "private-arguments", "private-tool-result", "private-password", "private-instructions", "private-schema"} {
		require.NotContains(t, prepared.Task, secret)
	}
	require.Contains(t, prepared.Task, "earlier requirement")
	require.Contains(t, prepared.Task, "visible refusal")
	require.Contains(t, history.Spans[3].Text, "  now use the second option")
	repeated, err := ParseAIRoutingResponsesHistory(body, AIRoutingContextLimits{})
	require.NoError(t, err)
	require.Equal(t, history, repeated)
}

func TestAIRoutingResponsesHistoryStringArrayAndDispatch(t *testing.T) {
	for _, input := range []any{"当前任务", []any{map[string]any{"role": "user", "content": "当前任务"}}} {
		body := aiResponsesTestBody(t, input, nil)
		request, err := NewAIRoutingResponsesRequest(body, "bounded_history")
		require.NoError(t, err)
		history, err := ParseAIRoutingRequestHistory(request, AIRoutingContextLimits{})
		require.NoError(t, err)
		require.Equal(t, 1, history.HistoryTurns)
		require.Equal(t, "t1", history.CurrentTurnID)
		require.Len(t, history.Spans, 1)
		require.Equal(t, "r1b0:0-12", history.Spans[0].ID)
		require.Equal(t, "当前任务", history.Spans[0].Text)
	}
	messages := aiHistoryTestBody(t, aiHistoryTestMessage("user", "Messages stays compatible"))
	history, err := ParseAIRoutingRequestHistory(AIRoutingRequest{HistoryBody: messages}, AIRoutingContextLimits{})
	require.NoError(t, err)
	require.Equal(t, AIRoutingHistoryProjectionVersion, history.ProjectionVersion)
	_, err = ParseAIRoutingRequestHistory(AIRoutingRequest{Endpoint: "unknown", HistoryBody: messages}, AIRoutingContextLimits{})
	aiHistoryTestReason(t, err, "history_protocol_unsupported")
}

func TestAIRoutingResponsesHistoryHiddenActionsDelimitCurrentTask(t *testing.T) {
	body := aiResponsesTestBody(t, []any{
		map[string]any{"role": "user", "content": "old user task"},
		map[string]any{"type": "function_call", "call_id": "call_x", "name": "f", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "call_x", "output": "private-result"},
		map[string]any{"role": "user", "content": "current task"},
	}, nil)
	history, err := ParseAIRoutingResponsesHistory(body, AIRoutingContextLimits{})
	require.NoError(t, err)
	require.Equal(t, 3, history.HistoryTurns)
	require.Equal(t, "t3", history.CurrentTurnID)
	require.NotEqual(t, history.Spans[0].TurnID, history.Spans[1].TurnID)
	require.Equal(t, history.CurrentTurnID, history.Spans[1].TurnID)
	request, err := NewAIRoutingResponsesRequest(body, "task_excerpt")
	require.NoError(t, err)
	require.Contains(t, request.Task, "current task")
	require.NotContains(t, request.Task, "old user task")
	_, err = ParseAIRoutingResponsesHistory(body, AIRoutingContextLimits{MaxHistoryTurns: 2})
	aiHistoryTestReason(t, err, "history_turn_limit")
	_, err = ParseAIRoutingResponsesHistory(body, AIRoutingContextLimits{MaxHistoryBytes: 1})
	aiHistoryTestReason(t, err, "history_byte_limit")
	_, err = ParseAIRoutingResponsesHistory([]byte(`{"model":"x","input":[{"role":"user","Content":"ambiguous"}]}`), AIRoutingContextLimits{})
	aiHistoryTestReason(t, err, "history_invalid_request")
}

func TestAIRoutingResponsesHistorySameTailDifferentHistoryChoosesDifferentTargets(t *testing.T) {
	firstID, secondID := int64(41), int64(42)
	candidates := []AIRoutingCandidate{
		{ID: "translate", Platform: PlatformOpenAI, Model: "same-model", TargetAccountID: &firstID, Enabled: true},
		{ID: "reason", Platform: PlatformOpenAI, Model: "same-model", TargetAccountID: &secondID, Enabled: true},
	}
	for _, tc := range []struct{ material, want string }{{"task category: translation", "translate"}, {"task category: proof", "reason"}} {
		t.Run(tc.want, func(t *testing.T) {
			body := aiResponsesTestBody(t, []any{
				map[string]any{"role": "user", "content": tc.material},
				map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "second option follows the earlier task"}}},
				map[string]any{"role": "user", "content": "继续第二项"},
			}, map[string]any{"instructions": "excluded-system"})
			before := append([]byte(nil), body...)
			request, err := NewAIRoutingResponsesRequest(body, "bounded_history")
			require.NoError(t, err)
			policy := NewAIRoutingDraft(9)
			policy.ContextMode, policy.Prompt = "bounded_history", "Choose by the earlier task category."
			calls := 0
			chooser := aiHistoryChooserFunc(func(_ context.Context, _ *AIRoutingPolicy, task string, options []AIRoutingCandidate, _ string) (AIRoutingDecisionResult, error) {
				calls++
				view := aiHistoryTestDecode(t, task)
				require.Equal(t, AIRoutingResponsesHistoryProjectionVersion, view.ProjectionVersion)
				require.Equal(t, "继续第二项", view.Spans[len(view.Spans)-1].Text)
				require.NotContains(t, task, "excluded-system")
				material, err := json.Marshal(view.Spans)
				require.NoError(t, err)
				id := "translate"
				if strings.Contains(string(material), "task category: proof") {
					id = "reason"
				}
				return AIRoutingDecisionResult{CandidateID: id}, nil
			})
			chosen, result, source, err := ChooseAIRoutingCandidate(context.Background(), &policy, request, candidates, chooser, "synthetic.invalid")
			require.NoError(t, err)
			require.Equal(t, tc.want, chosen.ID)
			require.Equal(t, tc.want, result.CandidateID)
			require.Equal(t, "model", source)
			require.Equal(t, 1, calls)
			require.Equal(t, AIRoutingResponsesHistoryProjectionVersion, result.Context.ProjectionVersion)
			expected := firstID
			if tc.want == "reason" {
				expected = secondID
			}
			require.Equal(t, expected, *chosen.TargetAccountID)
			require.Equal(t, before, request.HistoryBody)
			require.Equal(t, before, body, "the native execution request must never become the evidence task")
		})
	}
}

func TestAIRoutingResponsesHistoryLongEvidenceKeepsHeadMiddleTail(t *testing.T) {
	input := make([]any, 0, 61)
	for i := 0; i < 60; i++ {
		role, text := "user", strings.Repeat("ordinary text ", 45)
		if i%2 == 1 {
			role = "assistant"
		}
		switch i {
		case 0:
			text = "HEAD original requirement: translation."
		case 30:
			text = "MIDDLE correction: perform a proof instead."
		case 59:
			text = "TAIL second option is the corrected proof task."
		}
		input = append(input, map[string]any{"role": role, "content": text})
	}
	input = append(input, map[string]any{"role": "user", "content": "继续第二项"})
	history, err := ParseAIRoutingResponsesHistory(aiResponsesTestBody(t, input, nil), AIRoutingContextLimits{})
	require.NoError(t, err)
	calls := 0
	limits := AIRoutingContextLimits{MaxInputBytes: 12000, MaxSegments: 128, MaxDecisionCalls: 129}
	prepared, err := BuildAIRoutingHistoryTask(context.Background(), history, limits, 200, func(_ context.Context, segment AIRoutingHistoryClassificationRequest) (AIRoutingHistoryRelevance, error) {
		calls++
		view := aiHistoryTestDecode(t, segment.Task)
		for _, span := range view.Spans {
			if strings.Contains(span.Text, "HEAD ") || strings.Contains(span.Text, "MIDDLE ") || strings.Contains(span.Text, "TAIL ") {
				return AIRoutingHistoryRelevant, nil
			}
		}
		return AIRoutingHistoryIrrelevant, nil
	})
	require.NoError(t, err)
	require.Greater(t, calls, 1)
	view := aiHistoryTestDecode(t, prepared.Task)
	require.Equal(t, AIRoutingResponsesHistoryProjectionVersion, view.ProjectionVersion)
	joined := ""
	for _, span := range view.Spans {
		joined += span.Text + "\n"
	}
	head, middle, tail := strings.Index(joined, "HEAD "), strings.Index(joined, "MIDDLE "), strings.Index(joined, "TAIL ")
	require.GreaterOrEqual(t, head, 0)
	require.Greater(t, middle, head)
	require.Greater(t, tail, middle)
	require.Equal(t, prepared.Diagnostics.Segments, prepared.Diagnostics.ScannedSegments)
}
