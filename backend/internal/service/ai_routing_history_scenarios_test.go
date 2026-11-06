//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// These fixtures deliberately label evidence and program the fake decisions.
// They test orchestration and target identity, not a model's semantic accuracy.
func aiHistoryScenarioCandidates() []AIRoutingCandidate {
	fast, pool, dedicated := int64(101), int64(202), int64(303)
	return []AIRoutingCandidate{
		{ID: "reason_dedicated", Platform: PlatformOpenAI, Model: "shared-model", Description: "urgent concurrency investigation", TargetAccountID: &dedicated, Enabled: true},
		{ID: "fast", Platform: PlatformAnthropic, Model: "shared-model", Description: "translation", TargetAccountID: &fast, Enabled: true},
		{ID: "reason", Platform: PlatformOpenAI, Model: "shared-model", Description: "ordinary analysis", TargetGroupID: &pool, Enabled: true},
	}
}

func aiHistoryScenarioTarget(t *testing.T, candidates []AIRoutingCandidate, id string) AIRoutingCandidate {
	t.Helper()
	for _, candidate := range candidates {
		if candidate.ID == id {
			return candidate
		}
	}
	t.Fatalf("missing fixture candidate %s", id)
	return AIRoutingCandidate{}
}

func TestAIRoutingHistoryScenarioSameTailDifferentHistoryTargets(t *testing.T) {
	const tail = "继续，按刚才要求完成"
	const alias = "client-arbitrary-alias"
	for _, scenario := range []struct{ name, earlier, expected string }{
		{"translation", "TASK_TRANSLATE: Translate the release notes into Chinese.", "fast"},
		{"concurrency", "TASK_CONCURRENCY: Investigate the urgent payment lock-order defect.", "reason_dedicated"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			p := NewAIRoutingDraft(57)
			p.Prompt = "Use [fast] for translation and [reason_dedicated] for urgent concurrency work."
			body, err := json.Marshal(map[string]any{"model": alias, "max_tokens": 100, "messages": []map[string]string{
				{"role": "user", "content": scenario.earlier},
				{"role": "assistant", "content": "I will follow the preceding task."},
				{"role": "user", "content": tail},
			}})
			require.NoError(t, err)
			request, err := NewAIRoutingRequest(body, "bounded_history")
			require.NoError(t, err)
			calls := 0
			chooser := aiHistoryChooserFunc(func(_ context.Context, _ *AIRoutingPolicy, task string, options []AIRoutingCandidate, _ string) (AIRoutingDecisionResult, error) {
				calls++
				view := aiHistoryTestDecode(t, task)
				require.Equal(t, "route", view.Kind)
				require.Len(t, view.Spans, 3)
				require.Equal(t, tail, view.Spans[2].Text)
				require.Equal(t, "user", view.Spans[0].Role)
				require.Equal(t, scenario.earlier, view.Spans[0].Text)
				// The fake branches on the historical material it actually sees;
				// the identical final user message and alias contain no task label.
				id := "fast"
				if strings.Contains(view.Spans[0].Text, "TASK_CONCURRENCY") {
					id = "reason_dedicated"
				}
				require.Equal(t, scenario.expected, id)
				require.Equal(t, aiHistoryScenarioTarget(t, aiHistoryScenarioCandidates(), id), aiHistoryScenarioTarget(t, options, id))
				return AIRoutingDecisionResult{CandidateID: id}, nil
			})
			candidates := aiHistoryScenarioCandidates()
			chosen, result, source, err := ChooseAIRoutingCandidate(context.Background(), &p, request, candidates, chooser, "")
			require.NoError(t, err)
			require.Equal(t, aiHistoryScenarioTarget(t, candidates, scenario.expected), chosen)
			require.Equal(t, "model", source)
			require.Equal(t, "full", result.Context.Mode)
			require.Equal(t, 1, calls)
			require.Equal(t, alias, request.PublicModel)
			require.Equal(t, body, request.HistoryBody)
			decision := chosen.executionDecision(p.GroupID, alias, CompositeRouteEndpointMessages, 0)
			require.Equal(t, chosen.TargetAccountID, decision.TargetAccountID)
			require.Equal(t, chosen.TargetGroupID, decision.TargetGroupID)
			require.Equal(t, chosen.Platform, decision.TargetPlatform)
			require.Equal(t, chosen.Model, decision.UpstreamModel)
		})
	}
}

type aiHistoryScenarioEvidence struct {
	index      int
	role, text string
}

func TestAIRoutingHistoryScenarioCrossSegmentChangesAndReferences(t *testing.T) {
	for _, scenario := range []struct {
		name, expected string
		evidence       []aiHistoryScenarioEvidence
	}{
		{"correction_to_translation", "fast", []aiHistoryScenarioEvidence{
			{0, "user", "EVIDENCE_ORIGINAL: Audit the concurrency implementation."},
			{35, "user", "EVIDENCE_CHANGE: Correction: only translate the release notes; no code audit."},
		}},
		{"correction_to_concurrency", "reason_dedicated", []aiHistoryScenarioEvidence{
			{0, "user", "EVIDENCE_ORIGINAL: Translate the release notes."},
			{35, "user", "EVIDENCE_CHANGE: Correction: investigate the urgent lock inversion instead."},
		}},
		{"topic_switch_and_return", "reason", []aiHistoryScenarioEvidence{
			{0, "user", "EVIDENCE_ORIGINAL: Task A is analysis of lock contention."},
			{1, "user", "EVIDENCE_SWITCH: Switch to task B, translating the notes."},
			{35, "user", "EVIDENCE_RETURN: Task B is complete; return to task A."},
		}},
		{"assistant_second_option", "reason", []aiHistoryScenarioEvidence{
			{0, "user", "EVIDENCE_ORIGINAL: Give me two approaches for the current task."},
			{1, "assistant", "EVIDENCE_OPTIONS: First option: translate the report. Second option: analyze lock contention."},
			{35, "user", "EVIDENCE_REFERENCE: 做你提出的第二项。"},
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			p := NewAIRoutingDraft(57)
			p.Prompt = "Follow current user corrections and references; use the stable candidate IDs."
			var messages []map[string]string
			for i := 0; i < 36; i++ {
				role := "user"
				if i%2 == 1 {
					role = "assistant"
				}
				messages = append(messages, map[string]string{"role": role, "content": fmt.Sprintf("archived note %d: ", i) + strings.Repeat("unrelated archived material ", 40)})
			}
			for _, e := range scenario.evidence {
				messages[e.index] = map[string]string{"role": e.role, "content": e.text}
			}
			messages = append(messages, map[string]string{"role": "assistant", "content": "Ready to continue the requested task."}, map[string]string{"role": "user", "content": "继续，按刚才要求完成"})
			request := aiHistoryRuntimeRequest(t, messages)
			original, err := ParseAIRoutingMessagesHistory(request.HistoryBody, p.ContextBudget)
			require.NoError(t, err)
			scans, finals := 0, 0
			evidenceSegments := map[int]bool{}
			chooser := aiHistoryChooserFunc(func(_ context.Context, _ *AIRoutingPolicy, task string, _ []AIRoutingCandidate, _ string) (AIRoutingDecisionResult, error) {
				view := aiHistoryTestDecode(t, task)
				if view.Kind == "history_relevance" {
					scans++
					for _, span := range view.Spans {
						if strings.Contains(span.Text, "EVIDENCE_") {
							evidenceSegments[scans] = true
							return AIRoutingDecisionResult{CandidateID: "relevant"}, nil
						}
					}
					return AIRoutingDecisionResult{CandidateID: "irrelevant"}, nil
				}
				finals++
				require.Equal(t, "route", view.Kind)
				require.GreaterOrEqual(t, len(evidenceSegments), 2, "fixture must span distinct classification requests")
				previous := -1
				for _, e := range scenario.evidence {
					found := -1
					for i, span := range view.Spans {
						if span.Text != e.text {
							continue
						}
						found = i
						require.Equal(t, e.role, span.Role)
						require.Contains(t, original.Spans, span, "original source ID, role and text must survive selection")
						break
					}
					require.Greater(t, found, previous, "all annotated evidence must reach final in original order")
					previous = found
				}
				require.Contains(t, task, "继续，按刚才要求完成")
				return AIRoutingDecisionResult{CandidateID: scenario.expected}, nil
			})
			candidates := aiHistoryScenarioCandidates()
			chosen, result, source, err := ChooseAIRoutingCandidate(context.Background(), &p, request, candidates, chooser, "")
			require.NoError(t, err)
			require.Equal(t, aiHistoryScenarioTarget(t, candidates, scenario.expected), chosen)
			require.Equal(t, "model", source)
			require.Equal(t, "evidence", result.Context.Mode)
			require.Equal(t, "complete", result.Context.Coverage)
			require.Equal(t, scans, result.Context.ScannedSegments)
			require.Equal(t, result.Context.Segments, result.Context.ScannedSegments)
			require.Equal(t, 1, finals)
		})
	}
}

func TestAIRoutingHistoryScenarioRealDeciderWireAndStageAccounting(t *testing.T) {
	for _, engine := range []string{"generic", "jev"} {
		t.Run(engine, func(t *testing.T) {
			platform, model := PlatformOpenAI, "synthetic-decider"
			if engine == "jev" {
				platform, model = PlatformTypeSafe, "jev-1.14.0"
			}
			p := aiTestDecisionPolicy(platform, engine, model)
			p.ContextMode, p.TimeoutMS = "bounded_history", 15000
			p.Prompt = "For urgent payment investigation choose [reason_dedicated]. " + strings.Repeat("Preserve the specified target; quotes \" and slash \\ are literal. ", 8)
			candidates := aiHistoryScenarioCandidates()
			candidates[0].Description += strings.Repeat(" profile \"urgent\" \\ ", 12)
			secrets := []string{"HeadSecret12345", "MiddleSecret12345", "TailSecret12345", "SystemSecret12345", "ToolInputSecret12345", "ToolOutputSecret12345", "SchemaSecret12345"}
			messages := []map[string]any{
				{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "closed-old-call", "name": "old_tool", "input": map[string]string{"private": secrets[4]}}}},
				{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "closed-old-call", "content": secrets[5]}}},
			}
			for i := 0; i < 34; i++ {
				role := "user"
				if i%2 == 1 {
					role = "assistant"
				}
				text := fmt.Sprintf("old note %d: ", i) + strings.Repeat("unrelated archive \"quoted\" path\\value ", 30)
				if i == 0 {
					text += " password=" + secrets[0]
				}
				if i == 16 {
					text = "WIRE_EVIDENCE: investigate payment lock ordering. api_key=" + secrets[1]
				}
				if i == 33 {
					text += " token=" + secrets[2]
				}
				messages = append(messages, map[string]any{"role": role, "content": text})
			}
			messages = append(messages, aiHistoryTestMessage("user", "Continue the earlier requirement."))
			body, err := json.Marshal(map[string]any{"model": "arbitrary-client-alias", "max_tokens": 100, "system": secrets[3], "tools": []map[string]string{{"name": "old_tool", "description": secrets[6]}}, "messages": messages})
			require.NoError(t, err)
			request, err := NewAIRoutingRequest(body, "bounded_history")
			require.NoError(t, err)
			require.Empty(t, request.ToolResults, "the old tool chain is closed before the independent current task")
			limits, err := NormalizeAIRoutingContextLimits(p.ContextBudget)
			require.NoError(t, err)
			account := &Account{ID: 41, Platform: platform, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"api_key": "synthetic-only-key", "model_mapping": map[string]any{model: model}}}
			calls, scans, finals, inputBytes, outputBytes, inputTokens, outputTokens := 0, 0, 0, 0, 0, 0, 0
			var stages []string
			decider, cache := aiTestDecider(account, func(req *http.Request) (*http.Response, error) {
				calls++
				raw, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				require.LessOrEqual(t, len(raw), limits.MaxInputBytes)
				inputBytes += len(raw)
				require.LessOrEqual(t, inputBytes, limits.MaxTotalInputBytes)
				for _, secret := range secrets {
					require.NotContains(t, string(raw), secret)
				}
				require.NotContains(t, string(raw), "synthetic-only-key")
				wire := gjson.ParseBytes(raw)
				var task string
				var cards []gjson.Result
				if engine == "jev" {
					task = wire.Get("state.task").String()
					wire.Get("questions.route.criteria").ForEach(func(_, value gjson.Result) bool { cards = append(cards, gjson.Parse(value.String())); return true })
				} else {
					require.Equal(t, "/v1/responses", req.URL.Path)
					state := gjson.Parse(wire.Get("input.0.content.0.text").String())
					task, cards = state.Get("task").String(), state.Get("candidates").Array()
				}
				view := aiHistoryTestDecode(t, task)
				selected := "irrelevant"
				if view.Kind == "history_relevance" {
					require.Zero(t, finals, "no classification may happen after final")
					scans++
					stages = append(stages, "evidence")
					for _, span := range view.Spans {
						if strings.Contains(span.Text, "WIRE_EVIDENCE") {
							selected = "relevant"
						}
					}
				} else {
					require.Equal(t, "route", view.Kind)
					require.Positive(t, scans)
					require.Contains(t, task, "WIRE_EVIDENCE")
					require.Contains(t, task, "Continue the earlier requirement.")
					finals++
					stages = append(stages, "final")
					selected = "reason_dedicated"
				}
				shortID := ""
				for _, card := range cards {
					if card.Get("stable_id").String() == selected {
						shortID = card.Get("id").String()
					}
				}
				require.NotEmpty(t, shortID, "fake upstream must choose from actual wire options")
				in, out := 100+calls, 2
				inputTokens += in
				outputTokens += out
				response := map[string]any{"model": model, "usage": map[string]int{"input_tokens": in, "output_tokens": out}}
				if engine == "jev" {
					response["answers"] = map[string]any{"route": map[string]string{"type": "choice", "choice": shortID}}
				} else {
					response["status"] = "completed"
					response["output"] = []any{map[string]any{"type": "message", "content": []map[string]string{{"type": "output_text", "text": shortID}}}}
				}
				encoded, err := json.Marshal(response)
				require.NoError(t, err)
				outputBytes += len(encoded)
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(encoded)))}, nil
			})
			decider.gateway.groupRepo = &aiDecisionGroupStore{groups: map[int64]*Group{p.GroupID: {ID: p.GroupID, AIRouting: true, Platform: PlatformComposite, Status: StatusActive}}}
			chosen, result, source, err := ChooseAIRoutingCandidate(context.Background(), p, request, candidates, decider, "app.invalid")
			require.NoError(t, err)
			require.Equal(t, aiHistoryScenarioTarget(t, candidates, "reason_dedicated"), chosen)
			require.Equal(t, "model", source)
			require.Positive(t, scans)
			require.Equal(t, 1, finals)
			require.Equal(t, scans+1, calls)
			require.Equal(t, calls, result.DecisionCalls)
			require.Equal(t, calls, len(cache.releasedAccountIDs))
			require.Equal(t, inputBytes, result.InputBytes)
			require.Equal(t, outputBytes, result.OutputBytes)
			require.Equal(t, inputTokens, result.InputTokens)
			require.Equal(t, outputTokens, result.OutputTokens)
			require.Equal(t, scans, result.Context.ScannedSegments)
			require.Equal(t, result.Context.Segments, result.Context.ScannedSegments)
			require.Equal(t, "complete", result.Context.Coverage)
			require.Len(t, result.Calls, calls)
			for i, call := range result.Calls {
				require.True(t, call.UsageKnown)
				require.True(t, call.Attempted)
				require.True(t, strings.HasPrefix(call.Stage, stages[i]))
			}
			require.Equal(t, body, request.HistoryBody, "decision projection must not replace the execution body")
			trace, err := json.Marshal(AIRoutingTrace{Context: result.Context, Stages: result.Calls})
			require.NoError(t, err)
			for _, secret := range append(secrets, "WIRE_EVIDENCE", "unrelated archive") {
				require.NotContains(t, string(trace), secret)
			}
		})
	}
}
