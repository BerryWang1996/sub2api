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
