//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func aiFinalTestContext() context.Context {
	return WithAIRoutingExecutionRequirements(context.Background(), AIRoutingExecutionRequirements{
		InputModalities: []string{"text"}, InputBytes: 1, MaxOutputTokens: 1, ContextWindowTokens: 1000000,
	})
}

func aiFinalTestRequest(t *testing.T, endpoint, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://synthetic.invalid"+endpoint, strings.NewReader(body))
	require.NoError(t, err)
	return req
}

func aiFinalTestCheck(t *testing.T, endpoint, body string) AIRoutingCapabilityCheck {
	t.Helper()
	return CheckAIRoutingFinalPayload(aiFinalTestContext(), aiCapabilityAccount(), "gpt-5.1", aiFinalTestRequest(t, endpoint, body))
}

func TestAIRoutingFinalPayloadUsesFinalBytesAndPreservesBody(t *testing.T) {
	body := `{"model":"gpt-5.1","max_tokens":200,"messages":[{"role":"user","content":"` + strings.Repeat(`\"`, 400) + `"}],"system":"added by adapter"}`
	req := aiFinalTestRequest(t, "/v1/messages", body)
	r, _ := AIRoutingExecutionRequirementsFromContext(aiFinalTestContext())
	r.ContextWindowTokens = int64(len(body)) + 200
	ctx := WithAIRoutingExecutionRequirements(context.Background(), r)
	check := CheckAIRoutingFinalPayload(ctx, aiCapabilityAccount(), "gpt-5.1", req)
	require.True(t, check.Allowed, check.Reason)
	require.True(t, check.FinalPayloadChecked)
	require.False(t, check.CapacityVerified)
	require.Equal(t, "final_payload_bytes_plus_output", check.CapacityMethod)
	require.EqualValues(t, len(body), check.InputBytes)
	require.EqualValues(t, 200, check.OutputReserveTokens)
	r.ContextWindowTokens--
	check = CheckAIRoutingFinalPayload(WithAIRoutingExecutionRequirements(ctx, r), aiCapabilityAccount(), "gpt-5.1", req)
	require.Equal(t, "execution_context_window_exceeded", check.Reason)
	require.True(t, check.FinalPayloadChecked)
	r.ContextWindowTokens = 1000000
	r.MaxInputTokens = int64(len(body)) - 1
	check = CheckAIRoutingFinalPayload(WithAIRoutingExecutionRequirements(ctx, r), aiCapabilityAccount(), "gpt-5.1", req)
	require.Equal(t, "execution_input_limit_exceeded", check.Reason)
	remaining, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, body, string(remaining), "checks must not consume, normalize, or rewrite the body being sent")
	original, _ := AIRoutingExecutionRequirementsFromContext(ctx)
	require.EqualValues(t, 1, original.InputBytes)
	require.EqualValues(t, 1, original.MaxOutputTokens)
}

func TestAIRoutingFinalPayloadRealMessagesConversions(t *testing.T) {
	input := &apicompat.AnthropicRequest{Model: "gpt-5.1", MaxTokens: 1, Messages: []apicompat.AnthropicMessage{{Role: "user", Content: json.RawMessage(`"hello"`)}}}
	responses, err := apicompat.AnthropicToResponses(input)
	require.NoError(t, err)
	chat, err := apicompat.AnthropicToChatCompletionsRequest(input)
	require.NoError(t, err)
	for _, tc := range []struct {
		endpoint string
		payload  any
	}{{"/v1/responses", responses}, {"/v1/chat/completions", chat}} {
		t.Run(tc.endpoint, func(t *testing.T) {
			body, marshalErr := json.Marshal(tc.payload)
			require.NoError(t, marshalErr)
			check := aiFinalTestCheck(t, tc.endpoint, string(body))
			require.True(t, check.Allowed, check.Reason)
			require.EqualValues(t, apicompat.DefaultMinMaxOutputTokens, check.OutputReserveTokens)
			r, _ := AIRoutingExecutionRequirementsFromContext(aiFinalTestContext())
			r.CandidateMaxOutputTokens = 64
			check = CheckAIRoutingFinalPayload(WithAIRoutingExecutionRequirements(context.Background(), r), aiCapabilityAccount(), input.Model, aiFinalTestRequest(t, tc.endpoint, string(body)))
			require.Equal(t, "execution_output_limit_exceeded", check.Reason)
		})
	}
	claude := &antigravity.ClaudeRequest{
		Model: "claude-sonnet-4-5", MaxTokens: 100,
		Messages: []antigravity.ClaudeMessage{{Role: "user", Content: json.RawMessage(`"hello"`)}},
		Thinking: &antigravity.ThinkingConfig{Type: "enabled", BudgetTokens: 2000},
	}
	agyBody, err := antigravity.TransformClaudeToGemini(claude, "synthetic-project", claude.Model)
	require.NoError(t, err)
	account := &Account{ID: 1, Platform: PlatformAntigravity, Type: AccountTypeOAuth}
	check := CheckAIRoutingFinalPayload(aiFinalTestContext(), account, claude.Model, aiFinalTestRequest(t, "/v1internal:streamGenerateContent?alt=sse", string(agyBody)))
	require.True(t, check.Allowed, check.Reason)
	require.EqualValues(t, 3000, check.OutputReserveTokens, "reserve the adjusted thinking budget, not the original 100")
	require.EqualValues(t, len(agyBody), check.InputBytes, "include the entire wrapper, system additions and generated metadata")
}

func TestAIRoutingFinalPayloadOutputRemovedRequiresModelMaximum(t *testing.T) {
	body := `{"model":"gpt-5.1","input":[{"role":"user","content":"hello"}]}`
	req := aiFinalTestRequest(t, "/v1/responses", body)
	account := aiCapabilityAccount()
	r, _ := AIRoutingExecutionRequirementsFromContext(aiFinalTestContext())
	r.MaxOutputTokens, r.CandidateMaxOutputTokens = 20, 25
	ctx := WithAIRoutingExecutionRequirements(context.Background(), r)
	check := CheckAIRoutingFinalPayload(ctx, account, "gpt-5.1", req)
	require.Equal(t, "execution_output_reserve_unknown", check.Reason)
	require.True(t, check.FinalPayloadChecked)
	require.Zero(t, check.OutputReserveTokens)
	require.Contains(t, check.Warnings, "output_reserve_unknown")
	aiCapabilityMetadata(account, 100000, 300, "text")
	check = CheckAIRoutingFinalPayload(ctx, account, "gpt-5.1", req)
	require.Equal(t, "execution_output_limit_exceeded", check.Reason, "the desired candidate ceiling does not bind a request whose limit was removed")
	require.EqualValues(t, 300, check.OutputReserveTokens)
	require.Contains(t, check.Warnings, "output_reserve_from_metadata_maximum")
	require.Contains(t, check.Warnings, "metadata_limits_not_request_verified")
	r.CandidateMaxOutputTokens = 300
	check = CheckAIRoutingFinalPayload(WithAIRoutingExecutionRequirements(ctx, r), account, "gpt-5.1", req)
	require.True(t, check.Allowed, check.Reason)
	require.False(t, check.CapacityVerified)
	for _, tc := range []struct {
		name string
		edit func(*UpstreamModelMetadataSnapshot)
	}{
		{"invalid time", func(s *UpstreamModelMetadataSnapshot) { s.SyncedAt = "unknown" }},
		{"missing source", func(s *UpstreamModelMetadataSnapshot) { s.Source = "" }},
		{"different model", func(s *UpstreamModelMetadataSnapshot) {
			m := s.Models["gpt-5.1"]
			m.ID = "other"
			s.Models["gpt-5.1"] = m
		}},
		{"missing output", func(s *UpstreamModelMetadataSnapshot) {
			m := s.Models["gpt-5.1"]
			m.MaxOutputTokens = 0
			s.Models["gpt-5.1"] = m
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := aiCapabilityAccount()
			aiCapabilityMetadata(a, 100000, 300, "text")
			snapshot := a.GetUpstreamModelMetadataSnapshot()
			tc.edit(snapshot)
			a.SetUpstreamModelMetadataSnapshot(*snapshot)
			check := CheckAIRoutingFinalPayload(ctx, a, "gpt-5.1", req)
			require.Equal(t, "execution_output_reserve_unknown", check.Reason)
		})
	}
}

func TestAIRoutingFinalPayloadURLAndWrapperModelIdentity(t *testing.T) {
	tests := []struct {
		name, endpoint, body, model string
		account                     *Account
		wantAllowed                 bool
	}{
		{"Gemini URL", "/v1beta/models/gemini-2.5-pro:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":100}}`, "gemini-2.5-pro", nil, true},
		{"Gemini different URL", "/v1beta/models/gemini-2.5-flash:generateContent", `{"contents":[],"generationConfig":{"maxOutputTokens":100}}`, "gemini-2.5-pro", nil, false},
		{"Gemini different body", "/v1beta/models/gemini-2.5-pro:generateContent", `{"model":"different","contents":[],"generationConfig":{"maxOutputTokens":100}}`, "gemini-2.5-pro", nil, false},
		{"Gemini missing URL identity cannot use body fallback", "/v1beta/models/:generateContent", `{"model":"gemini-2.5-pro","contents":[],"generationConfig":{"maxOutputTokens":100}}`, "gemini-2.5-pro", nil, false},
		{"Gemini malformed URL identity cannot use body fallback", "/v1beta/models/gemini%2F2.5-pro:generateContent", `{"model":"gemini-2.5-pro","contents":[],"generationConfig":{"maxOutputTokens":100}}`, "gemini-2.5-pro", nil, false},
		{"Gemini wrapper", "/v1internal:generateContent", `{"model":"gemini-2.5-pro","project":"synthetic","request":{"contents":[],"generationConfig":{"maxOutputTokens":100}}}`, "gemini-2.5-pro", nil, true},
		{"wrapper nested mismatch", "/v1internal:generateContent", `{"model":"gemini-2.5-pro","request":{"model":"other","contents":[],"generationConfig":{"maxOutputTokens":100}}}`, "gemini-2.5-pro", nil, false},
		{"wrapper absent model", "/v1internal:generateContent", `{"request":{"contents":[],"generationConfig":{"maxOutputTokens":100}}}`, "gemini-2.5-pro", nil, false},
		{"Vertex dated alias", "/v1/projects/p/locations/global/publishers/anthropic/models/claude-sonnet-4-5@20250929:streamRawPredict", `{"anthropic_version":"vertex-2023-10-16","messages":[],"max_tokens":100}`, "claude-sonnet-4-5-20250929", nil, true},
		{"Vertex changed date", "/v1/projects/p/locations/global/publishers/anthropic/models/claude-sonnet-4-5@20250930:rawPredict", `{"messages":[],"max_tokens":100}`, "claude-sonnet-4-5-20250929", nil, false},
		{"known Codex normalization", "/backend-api/codex/responses", `{"model":"gpt-5.4","input":[],"max_output_tokens":100}`, "gpt-5.4-high", &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}, true},
		{"different Codex model", "/backend-api/codex/responses", `{"model":"gpt-5.5","input":[],"max_output_tokens":100}`, "gpt-5.4-high", &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}, false},
	}
	bedrock := &Account{ID: 1, Platform: PlatformAnthropic, Type: AccountTypeBedrock, Credentials: map[string]any{"aws_region": "us-east-1"}}
	bedrockModel, ok := ResolveBedrockModelID(bedrock, "claude-sonnet-4-5-20250929")
	require.True(t, ok)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			account := tc.account
			if account == nil {
				account = aiCapabilityAccount()
			}
			check := CheckAIRoutingFinalPayload(aiFinalTestContext(), account, tc.model, aiFinalTestRequest(t, tc.endpoint, tc.body))
			require.Equal(t, tc.wantAllowed, check.Allowed, check.Reason)
			if !tc.wantAllowed {
				require.Equal(t, "execution_final_model_mismatch", check.Reason)
				require.False(t, check.FinalPayloadChecked)
			}
		})
	}
	for _, stream := range []bool{false, true} {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, BuildBedrockURL("us-east-1", bedrockModel, stream), strings.NewReader(`{"anthropic_version":"bedrock-2023-05-31","messages":[],"max_tokens":100}`))
		require.NoError(t, err)
		check := CheckAIRoutingFinalPayload(aiFinalTestContext(), bedrock, "claude-sonnet-4-5-20250929", req)
		require.True(t, check.Allowed, check.Reason)
		check = CheckAIRoutingFinalPayload(aiFinalTestContext(), bedrock, "claude-sonnet-5-5", req)
		require.Equal(t, "execution_final_model_mismatch", check.Reason)
	}
}

func TestAIRoutingFinalPayloadRejectsAmbiguousJSONAndLimits(t *testing.T) {
	for _, value := range []string{`0`, `-1`, `1.5`, `1e2`, `"100"`, `null`, `true`, `9223372036854775808`} {
		t.Run("output "+value, func(t *testing.T) {
			check := aiFinalTestCheck(t, "/v1/messages", `{"model":"gpt-5.1","messages":[],"max_tokens":`+value+`}`)
			require.Equal(t, "execution_final_output_invalid", check.Reason)
			require.False(t, check.FinalPayloadChecked)
		})
	}
	for _, tc := range []struct{ endpoint, body, reason string }{
		{"/v1/messages", `{"model":"gpt-5.1","messages":[],"max_tokens":10,"max_tokens":100}`, "execution_final_payload_invalid"},
		{"/v1/messages", `{"model":"gpt-5.1","messages":[{"content":"a","content":"b"}],"max_tokens":100}`, "execution_final_payload_invalid"},
		{"/v1/messages", `{"model":"gpt-5.1","messages":[],"Max_Tokens":100}`, "execution_final_payload_invalid"},
		{"/v1/messages", `{"Model":"gpt-5.1","messages":[],"max_tokens":100}`, "execution_final_payload_invalid"},
		{"/v1/messages", `{"model":"gpt-5.1","messages":[],"max_tokens":100} {}`, "execution_final_payload_invalid"},
		{"/v1/messages", "{\"model\":\"gpt-5.1\",\"messages\":[],\"max_tokens\":100,\"junk\":\"\xff\"}", "execution_final_payload_invalid"},
		{"/v1/chat/completions", `{"model":"gpt-5.1","messages":[],"max_tokens":100,"max_completion_tokens":200}`, "execution_final_output_invalid"},
		{"/v1/responses", `{"model":"gpt-5.1","input":[],"max_tokens":100}`, "execution_final_output_invalid"},
		{"/v1/chat/completions", `{"model":"gpt-5.1","messages":[],"max_tokens":100,"n":2}`, "execution_output_choices_changed"},
		{"/v1internal:generateContent", `{"model":"gpt-5.1","request":{"contents":[],"generationConfig":{"MaxOutputTokens":100}}}`, "execution_final_output_invalid"},
		{"/v1internal:generateContent", `{"model":"gpt-5.1","request":{"contents":[],"generationConfig":{"maxOutputTokens":100,"candidateCount":2}}}`, "execution_output_choices_changed"},
		{"/v1internal:generateContent", `{"model":"gpt-5.1","request":{"contents":[],"generationConfig":100}}`, "execution_final_output_invalid"},
		{"/v1internal:generateContent", `{"model":"gpt-5.1","request":{"contents":[],"generationConfig":{"maxOutputTokens":100}},"generationConfig":{"maxOutputTokens":200}}`, "execution_final_payload_invalid"},
		{"/v1internal:generateContent", `{"model":"gpt-5.1","contents":[],"generationConfig":{"maxOutputTokens":100}}`, "execution_final_protocol_unknown"},
		{"/v1/unknown", `{"model":"gpt-5.1","messages":[],"max_tokens":100}`, "execution_final_protocol_unknown"},
		{"/v1/messages", `{"model":"gpt-5.1","input":[],"max_tokens":100}`, "execution_final_protocol_unknown"},
		{"/v1/messages", `{"messages":[],"max_tokens":100}`, "execution_final_model_mismatch"},
		{"/v1/messages", `{"model":"other","messages":[],"max_tokens":100}`, "execution_final_model_mismatch"},
	} {
		t.Run(tc.endpoint+tc.body, func(t *testing.T) {
			check := aiFinalTestCheck(t, tc.endpoint, tc.body)
			require.Equal(t, tc.reason, check.Reason)
			require.False(t, check.Allowed)
			require.False(t, check.FinalPayloadChecked)
			require.False(t, check.CapacityVerified)
		})
	}
}

func TestAIRoutingFinalPayloadUnmeteredContentRemainsUnverified(t *testing.T) {
	for _, tc := range []struct{ name, endpoint, body, warning string }{
		{"image", "/v1/messages", `{"model":"gpt-5.1","messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://synthetic.invalid/image"}}]}],"max_tokens":100}`, "attachment_token_usage_unverified"},
		{"reference", "/v1/responses", `{"model":"gpt-5.1","previous_response_id":"synthetic","input":[],"max_output_tokens":100}`, "referenced_context_token_usage_unverified"},
		{"cached content", "/v1internal:generateContent", `{"model":"gpt-5.1","request":{"contents":[],"cachedContent":"cachedContents/synthetic","generationConfig":{"maxOutputTokens":100}}}`, "referenced_context_token_usage_unverified"},
		{"opaque", "/v1/responses", `{"model":"gpt-5.1","input":[{"type":"reasoning","encrypted_content":"synthetic"}],"max_output_tokens":100}`, "opaque_state_token_usage_unverified"},
		{"signature", "/v1internal:generateContent", `{"model":"gpt-5.1","request":{"contents":[{"role":"model","parts":[{"text":"hi","thoughtSignature":"synthetic"}]}],"generationConfig":{"maxOutputTokens":100}}}`, "opaque_state_token_usage_unverified"},
		{"file", "/v1internal:generateContent", `{"model":"gpt-5.1","request":{"contents":[{"role":"user","parts":[{"fileData":{"fileUri":"gs://synthetic/file"}}]}],"generationConfig":{"maxOutputTokens":100}}}`, "attachment_token_usage_unverified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := AIRoutingExecutionRequirementsFromContext(aiFinalTestContext())
			if tc.name == "image" {
				r.InputModalities = []string{"text", "image"}
				r.SupportsImages = aiCapabilityBool(true)
			}
			check := CheckAIRoutingFinalPayload(WithAIRoutingExecutionRequirements(context.Background(), r), aiCapabilityAccount(), "gpt-5.1", aiFinalTestRequest(t, tc.endpoint, tc.body))
			require.True(t, check.Allowed, check.Reason)
			require.True(t, check.FinalPayloadChecked)
			require.False(t, check.CapacityVerified)
			require.Contains(t, check.Warnings, tc.warning)
		})
	}
	check := aiFinalTestCheck(t, "/v1/messages", `{"model":"gpt-5.1","messages":[{"role":"user","content":[{"type":"text","text":"inline","cache_control":{"type":"ephemeral"}},{"type":"tool_result","tool_use_id":"synthetic","content":"done"}]}],"prompt_cache_key":"synthetic","max_tokens":100}`)
	require.True(t, check.Allowed, check.Reason)
	require.NotContains(t, check.Warnings, "referenced_context_token_usage_unverified")
}

type aiFinalReadFailure struct{}

func (aiFinalReadFailure) Read([]byte) (int, error) { return 0, errors.New("synthetic read failure") }

type aiFinalRepeatedReader struct{ read int64 }

func (r *aiFinalRepeatedReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	r.read += int64(len(p))
	return len(p), nil
}

func TestAIRoutingFinalPayloadReplayAndBoundedReading(t *testing.T) {
	base := func() *http.Request {
		return aiFinalTestRequest(t, "/v1/messages", `{"model":"gpt-5.1","messages":[],"max_tokens":100}`)
	}
	for _, tc := range []struct {
		name string
		edit func(*http.Request)
	}{
		{"absent GetBody", func(r *http.Request) { r.GetBody = nil }},
		{"GetBody failure", func(r *http.Request) {
			r.GetBody = func() (io.ReadCloser, error) { return nil, errors.New("synthetic") }
		}},
		{"nil body", func(r *http.Request) { r.GetBody = func() (io.ReadCloser, error) { return nil, nil } }},
		{"read error", func(r *http.Request) {
			r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(aiFinalReadFailure{}), nil }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := base()
			tc.edit(req)
			check := CheckAIRoutingFinalPayload(aiFinalTestContext(), aiCapabilityAccount(), "gpt-5.1", req)
			require.Equal(t, "execution_final_payload_unavailable", check.Reason)
			require.False(t, check.FinalPayloadChecked)
		})
	}
	req := base()
	reader := &aiFinalRepeatedReader{}
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(reader), nil }
	req.ContentLength = 1 // the transport's size hint is not trusted
	check := CheckAIRoutingFinalPayload(aiFinalTestContext(), aiCapabilityAccount(), "gpt-5.1", req)
	require.Equal(t, "execution_final_payload_too_large", check.Reason)
	require.Equal(t, aiRoutingFinalPayloadMaxBytes+1, reader.read)
	require.False(t, check.FinalPayloadChecked)
	req = base()
	req.ContentLength = 1 << 40
	check = CheckAIRoutingFinalPayload(aiFinalTestContext(), aiCapabilityAccount(), "gpt-5.1", req)
	require.True(t, check.Allowed, "check the actual copied body rather than trusting an inflated size hint")
	ctx, cancel := context.WithCancel(aiFinalTestContext())
	req.GetBody = func() (io.ReadCloser, error) { cancel(); return io.NopCloser(bytes.NewReader([]byte(`{}`))), nil }
	check = CheckAIRoutingFinalPayload(ctx, aiCapabilityAccount(), "gpt-5.1", req)
	require.Equal(t, "execution_final_payload_canceled", check.Reason)
	require.False(t, check.FinalPayloadChecked)
}

func TestAIRoutingFinalPayloadNoAINoop(t *testing.T) {
	req := &http.Request{GetBody: func() (io.ReadCloser, error) { panic("non-AI traffic must not be inspected") }}
	for _, candidate := range []*http.Request{nil, req} {
		check := CheckAIRoutingFinalPayload(context.Background(), nil, "", candidate)
		require.True(t, check.Allowed)
		require.False(t, check.FinalPayloadChecked)
		require.False(t, check.CapacityVerified)
		require.Empty(t, check.CapacityMethod)
	}
}
