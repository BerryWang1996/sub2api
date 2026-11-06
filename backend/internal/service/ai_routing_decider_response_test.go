//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAIRoutingDecisionRejectsAmbiguousProtocolMembers(t *testing.T) {
	for _, tc := range []struct {
		name, protocol, body string
	}{
		{"chat_duplicate_finish", APIProtocolChatCompletions, `{"choices":[{"finish_reason":"length","finish_reason":"stop","message":{"content":"c1"}}]}`},
		{"chat_folded_finish", APIProtocolChatCompletions, `{"choices":[{"finish_reason":"length","Finish_Reason":"stop","message":{"content":"c1"}}]}`},
		{"chat_folded_refusal", APIProtocolChatCompletions, `{"choices":[{"finish_reason":"stop","message":{"content":"c1","refusal":"rejected","Refusal":""}}]}`},
		{"responses_folded_status", APIProtocolResponses, `{"status":"incomplete","Status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"c1"}]}]}`},
		{"claude_folded_stop", APIProtocolAnthropic, `{"stop_reason":"max_tokens","Stop_Reason":"end_turn","content":[{"type":"text","text":"c1"}]}`},
		{"jev_duplicate_choice", "systemone", `{"answers":{"route":{"type":"choice","choice":"c2","choice":"c1"}}}`},
		{"jev_folded_choice", "systemone", `{"answers":{"route":{"type":"choice","choice":"outside","Choice":"c1"}}}`},
		{"gemini_folded_finish", "gemini", `{"candidates":[{"finishReason":"MAX_TOKENS","FinishReason":"STOP","content":{"parts":[{"text":"c1"}]}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := decodeAIRoutingResponse(tc.protocol, []byte(tc.body), false)
			require.Error(t, err, "ambiguous provider response must fall back, not select a candidate")
		})
	}
	for _, field := range []string{`"type":"response.failed","type":"response.completed"`, `"type":"response.failed","Type":"response.completed"`} {
		stream := "data: {" + field + `,"response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"c1"}]}]}}` + "\n\n"
		_, _, _, err := readAIDecisionStream(strings.NewReader(stream))
		require.Error(t, err, "failed stream event must not be disguised as completion")
	}
}

func TestAIRoutingDecisionKeepsUnrelatedMetadataAndLargeJSONNumbers(t *testing.T) {
	raw := []byte(`{"status":"completed","metadata":{"type":"custom","Type":"another","n":1e1000},"tools":[{"parameters":{"type":"object","properties":{"type":{},"Type":{}}}}],"output":[{"type":"reasoning","custom":{"type":"custom","Type":"another"}},{"type":"message","content":[{"type":"output_text","text":"c1"}]}]}`)
	text, _, _, err := decodeAIRoutingResponse(APIProtocolResponses, raw, false)
	require.NoError(t, err)
	require.Equal(t, "c1", text)
}

func TestAIRoutingAmbiguousDecisionUsesConfiguredFallback(t *testing.T) {
	account := &Account{ID: 41, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"api_key": "synthetic-key"}}
	policy := aiTestDecisionPolicy(PlatformOpenAI, "generic", "custom-decider")
	policy.FallbackID = "b"
	calls := 0
	decider, slots := aiTestDecider(account, func(*http.Request) (*http.Response, error) {
		calls++
		body := `{"status":"incomplete","Status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"c1"}]}]}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	chosen, result, reason, err := ChooseAIRoutingCandidate(context.Background(), policy, AIRoutingRequest{Task: "metadata"}, aiTestDecisionCandidates(), decider, "app.example")
	require.NoError(t, err)
	require.Equal(t, "b", chosen.ID)
	require.Equal(t, "fallback", reason)
	require.Equal(t, "invalid_response", result.FailureReason)
	require.Equal(t, 1, calls)
	require.Equal(t, []int64{41}, slots.releasedAccountIDs)
}

func aiDecisionReviewStreamEvent(kind, response string) string {
	return `data: {"type":"` + kind + `","response":` + response + "}\n\n"
}

func TestAIRoutingDecisionStreamTerminalUsageCompleteness(t *testing.T) {
	complete := `"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"c1"}]}]`
	progress := aiDecisionReviewStreamEvent("response.in_progress", `{"model":"progress-model","usage":{"input_tokens":12,"output_tokens":2}}`)
	tests := []struct {
		name, stream, text, model string
		input, output             int
		known, wantError          bool
	}{
		{name: "complete", stream: aiDecisionReviewStreamEvent("response.completed", `{`+complete+`,"model":"actual-model","usage":{"input_tokens":12,"output_tokens":3}}`), text: "c1", model: "actual-model", input: 12, output: 3, known: true},
		{name: "missing output", stream: aiDecisionReviewStreamEvent("response.completed", `{`+complete+`,"usage":{"input_tokens":12}}`), text: "c1", input: 12},
		{name: "missing input", stream: aiDecisionReviewStreamEvent("response.completed", `{`+complete+`,"usage":{"output_tokens":3}}`), text: "c1", output: 3},
		{name: "missing usage", stream: aiDecisionReviewStreamEvent("response.completed", `{`+complete+`}`), text: "c1"},
		{name: "explicit zero", stream: aiDecisionReviewStreamEvent("response.completed", `{`+complete+`,"usage":{"input_tokens":0,"output_tokens":0}}`), text: "c1", known: true},
		{name: "null count unknown", stream: aiDecisionReviewStreamEvent("response.completed", `{`+complete+`,"usage":{"input_tokens":12,"output_tokens":null}}`), text: "c1", input: 12},
		{name: "failed with complete usage", stream: aiDecisionReviewStreamEvent("response.failed", `{"status":"failed","model":"failed-model","usage":{"input_tokens":12,"output_tokens":3}}`), model: "failed-model", input: 12, output: 3, known: true, wantError: true},
		{name: "incomplete with complete usage", stream: aiDecisionReviewStreamEvent("response.incomplete", `{"status":"incomplete","model":"incomplete-model","usage":{"input_tokens":12,"output_tokens":3}}`), model: "incomplete-model", input: 12, output: 3, known: true, wantError: true},
		{name: "selection invalid but usage complete", stream: aiDecisionReviewStreamEvent("response.completed", `{"status":"completed","output":[{"type":"function_call"}],"model":"tool-model","usage":{"input_tokens":12,"output_tokens":3}}`), model: "tool-model", input: 12, output: 3, known: true, wantError: true},
		{name: "progress then disconnect", stream: progress, input: 12, output: 2, wantError: true},
		{name: "progress cannot fill missing terminal field", stream: progress + aiDecisionReviewStreamEvent("response.completed", `{`+complete+`,"model":"final-model","usage":{"input_tokens":15}}`), text: "c1", model: "final-model", input: 15, output: 2},
		{name: "terminal zero replaces progress", stream: progress + aiDecisionReviewStreamEvent("response.completed", `{`+complete+`,"usage":{"input_tokens":0,"output_tokens":0}}`), text: "c1", known: true},
		{name: "failure without terminal usage", stream: progress + aiDecisionReviewStreamEvent("response.failed", `{"status":"failed","model":"final-model"}`), model: "final-model", input: 12, output: 2, wantError: true},
		{name: "oversized reported model omitted", stream: aiDecisionReviewStreamEvent("response.completed", `{`+complete+`,"model":"`+strings.Repeat("m", 257)+`","usage":{"input_tokens":1,"output_tokens":1}}`), text: "c1", input: 1, output: 1, known: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := readAIDecisionStreamResult(strings.NewReader(tt.stream))
			if tt.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tt.text, result.Text)
			require.Equal(t, tt.input, result.InputTokens)
			require.Equal(t, tt.output, result.OutputTokens)
			require.Equal(t, tt.model, result.ReportedModel)
			require.Equal(t, tt.known, result.UsageKnown)
			text, input, output, oldErr := readAIDecisionStream(strings.NewReader(tt.stream))
			require.Equal(t, err != nil, oldErr != nil)
			require.Equal(t, result.Text, text)
			require.Equal(t, result.InputTokens, input)
			require.Equal(t, result.OutputTokens, output)
		})
	}
}

func TestAIRoutingDecisionStreamRejectsInvalidUsage(t *testing.T) {
	for _, usage := range []string{
		`{"input_tokens":-1,"output_tokens":1}`,
		`{"input_tokens":1,"output_tokens":-1}`,
		`{"input_tokens":1073741825,"output_tokens":1}`,
		`{"input_tokens":1e1000,"output_tokens":1}`,
		`{"input_tokens":1.5,"output_tokens":1}`,
		`{"input_tokens":"12","output_tokens":1}`,
		`{"input_tokens":12,"input_tokens":0,"output_tokens":1}`,
		`{"input_tokens":12,"Input_Tokens":0,"output_tokens":1}`,
	} {
		t.Run(usage, func(t *testing.T) {
			stream := aiDecisionReviewStreamEvent("response.completed", `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"c1"}]}],"usage":`+usage+`}`)
			result, err := readAIDecisionStreamResult(strings.NewReader(stream))
			require.Error(t, err)
			require.Empty(t, result.Text)
			require.False(t, result.UsageKnown)
			require.NotContains(t, err.Error(), usage, "operational errors never echo upstream data")
		})
	}
}
