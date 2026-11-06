//go:build unit

package service

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/stretchr/testify/require"
)

func TestAIRoutingResponsesCapabilitiesNativeAndStoredAreSeparate(t *testing.T) {
	for _, tc := range []struct {
		name           string
		account        *Account
		native, stored bool
	}{
		{"missing account", nil, false, false},
		{"API key native", aiCapabilityAccount(), true, true},
		{"custom compatible base is not excluded by host", &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": "https://synthetic.invalid/custom/v1"}}, true, true},
		{"explicit raw chat", &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{openai_compat.ExtraKeyResponsesMode: string(openai_compat.ResponsesSupportModeForceChatCompletions)}}, false, false},
		{"probe says no Responses", &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{openai_compat.ExtraKeyResponsesSupported: false}}, false, false},
		{"multi protocol pinned native", &Account{Platform: PlatformKimi, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_protocol": APIProtocolResponses}}, true, false},
		{"multi protocol pinned chat", &Account{Platform: PlatformKimi, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_protocol": APIProtocolChatCompletions}}, false, false},
		{"multi protocol pinned Anthropic", &Account{Platform: PlatformKimi, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_protocol": APIProtocolAnthropic}}, false, false},
		{"OpenAI OAuth native without stored API", &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}, true, false},
		{"Anthropic API key is not Responses wire", &Account{Platform: PlatformAnthropic, Type: AccountTypeAPIKey}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.native, AIRoutingAccountSupportsNativeResponses(tc.account, "gpt-5.1"))
			require.Equal(t, tc.stored, AIRoutingAccountSupportsStoredResponses(tc.account, "gpt-5.1"))
		})
	}
}

func TestAIRoutingResponsesCapabilitiesRequirementsAndPerAccountReserve(t *testing.T) {
	request, err := NewAIRoutingResponsesRequest([]byte(`{"model":"alias","input":"task","store":false}`), "metadata")
	require.NoError(t, err)
	candidate := AIRoutingCandidate{ID: "pool", Model: "gpt-5.1", ContextWindowTokens: 1000, MaxOutputTokens: 400}
	requirements := AIRoutingRequirementsForCandidate(request, candidate)
	require.Equal(t, CompositeRouteEndpointResponses, requirements.Endpoint)
	require.True(t, requirements.OutputLimitOmitted)
	require.False(t, requirements.RequiresNativeResponses)
	require.False(t, requirements.RequiresStoredResponses)
	require.EqualValues(t, len(`{"model":"alias","input":"task","store":false}`), requirements.InputBytes)
	require.Zero(t, requirements.MaxOutputTokens)
	require.EqualValues(t, 400, requirements.CandidateMaxOutputTokens)
	ctx := WithAIRoutingExecutionRequirements(context.Background(), requirements)
	known := aiCapabilityAccount()
	aiCapabilityMetadata(known, 1000, 300, "text")
	check := CheckAIRoutingExecutionAccount(ctx, known, "gpt-5.1")
	require.True(t, check.Allowed, check.Reason)
	require.EqualValues(t, 300, check.OutputReserveTokens)
	require.Contains(t, check.Warnings, "output_reserve_from_metadata_maximum")
	require.False(t, check.CapacityVerified)
	require.False(t, check.FinalPayloadChecked)
	unknown := aiCapabilityAccount()
	unknown.ID = 2
	check = CheckAIRoutingExecutionAccount(ctx, unknown, "gpt-5.1")
	require.Equal(t, "execution_output_reserve_unknown", check.Reason, "a sibling account and the candidate ceiling cannot supply missing metadata")
	require.Zero(t, check.OutputReserveTokens)
	unchanged, _ := AIRoutingExecutionRequirementsFromContext(ctx)
	require.Zero(t, unchanged.MaxOutputTokens, "one account's reserve must not leak into another account's check")
	aiCapabilityMetadata(unknown, 1000, 450, "text")
	check = CheckAIRoutingExecutionAccount(ctx, unknown, "gpt-5.1")
	require.Equal(t, "execution_output_limit_exceeded", check.Reason)
	require.EqualValues(t, 450, check.OutputReserveTokens, "reserve the model maximum rather than clamping to the candidate ceiling")
	aiCapabilityMetadata(unknown, 300, 280, "text")
	check = CheckAIRoutingExecutionAccount(ctx, unknown, "gpt-5.1")
	require.Equal(t, "execution_context_window_exceeded", check.Reason)
	for _, change := range []func(*UpstreamModelMetadataSnapshot){
		func(snapshot *UpstreamModelMetadataSnapshot) { snapshot.Source = "" },
		func(snapshot *UpstreamModelMetadataSnapshot) { snapshot.SyncedAt = "unknown" },
		func(snapshot *UpstreamModelMetadataSnapshot) {
			model := snapshot.Models["gpt-5.1"]
			model.ID = "another-model"
			snapshot.Models["gpt-5.1"] = model
		},
		func(snapshot *UpstreamModelMetadataSnapshot) { delete(snapshot.Models, "gpt-5.1") },
	} {
		account := aiCapabilityAccount()
		aiCapabilityMetadata(account, 1000, 300, "text")
		snapshot := account.GetUpstreamModelMetadataSnapshot()
		change(snapshot)
		account.SetUpstreamModelMetadataSnapshot(*snapshot)
		check = CheckAIRoutingExecutionAccount(ctx, account, "gpt-5.1")
		require.Equal(t, "execution_output_reserve_unknown", check.Reason)
	}
}

func TestAIRoutingResponsesCapabilitiesNativeStoredRequirements(t *testing.T) {
	candidate := AIRoutingCandidate{ID: "a", Model: "gpt-5.1", ContextWindowTokens: 1000}
	oauth := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	for _, tc := range []struct {
		name, body, reason string
		account            *Account
		native, stored     bool
	}{
		{"OAuth inline reasoning", `{"model":"alias","input":[{"type":"reasoning","encrypted_content":"synthetic"},{"role":"user","content":"continue"}],"store":false,"max_output_tokens":100}`, "", oauth, true, false},
		{"OAuth default stored request", `{"model":"alias","input":"hello","max_output_tokens":100}`, "execution_stored_responses_required", oauth, true, true},
		{"OAuth explicit previous", `{"model":"alias","input":"continue","previous_response_id":"resp_original","store":false,"max_output_tokens":100}`, "execution_stored_responses_required", oauth, true, true},
		{"API key stored request", `{"model":"alias","input":"hello","max_output_tokens":100}`, "", aiCapabilityAccount(), true, true},
		{"explicit chat cannot promise native", `{"model":"alias","input":"hello","max_output_tokens":100}`, "execution_native_responses_required", &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{openai_compat.ExtraKeyResponsesSupported: false}}, true, true},
		{"stateless ordinary request permits bridge", `{"model":"alias","input":"hello","store":false,"max_output_tokens":100}`, "", &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{openai_compat.ExtraKeyResponsesSupported: false}}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, err := NewAIRoutingResponsesRequest([]byte(tc.body), "metadata")
			require.NoError(t, err)
			r := AIRoutingRequirementsForCandidate(request, candidate)
			require.Equal(t, tc.native, r.RequiresNativeResponses)
			require.Equal(t, tc.stored, r.RequiresStoredResponses)
			check := CheckAIRoutingExecutionAccount(WithAIRoutingExecutionRequirements(context.Background(), r), tc.account, candidate.Model)
			require.Equal(t, tc.reason, check.Reason)
			require.Equal(t, tc.reason == "", check.Allowed)
			require.False(t, check.CapacityVerified)
		})
	}
}

func TestAIRoutingResponsesCapabilitiesFinalWireCannotSilentlyChangeProtocol(t *testing.T) {
	account := aiCapabilityAccount()
	candidate := AIRoutingCandidate{ID: "a", Model: "gpt-5.1", ContextWindowTokens: 1000}
	for _, stored := range []bool{false, true} {
		body := `{"model":"alias","input":"task","store":false,"max_output_tokens":100}`
		if stored {
			body = `{"model":"alias","input":"task","max_output_tokens":100}`
		}
		request, err := NewAIRoutingResponsesRequest([]byte(body), "metadata")
		require.NoError(t, err)
		r := AIRoutingRequirementsForCandidate(request, candidate)
		ctx := WithAIRoutingExecutionRequirements(context.Background(), r)
		for _, endpoint := range []string{"/v1/responses", "/v1/chat/completions"} {
			final := `{"model":"gpt-5.1","input":"task","max_output_tokens":100}`
			if endpoint == "/v1/chat/completions" {
				final = `{"model":"gpt-5.1","messages":[{"role":"user","content":"task"}],"max_completion_tokens":100}`
			}
			req, buildErr := http.NewRequestWithContext(ctx, http.MethodPost, "https://synthetic.invalid"+endpoint, strings.NewReader(final))
			require.NoError(t, buildErr)
			check := CheckAIRoutingFinalPayload(ctx, account, candidate.Model, req)
			if stored && endpoint == "/v1/chat/completions" {
				require.Equal(t, "execution_native_responses_required", check.Reason)
				require.False(t, check.FinalPayloadChecked)
			} else {
				require.True(t, check.Allowed, check.Reason)
				require.True(t, check.FinalPayloadChecked)
			}
			require.False(t, check.CapacityVerified)
		}
	}
}

func TestAIRoutingResponsesCapabilitiesLegacyMessagesUnchanged(t *testing.T) {
	request, err := NewAIRoutingRequest([]byte(`{"model":"alias","max_tokens":100,"messages":[{"role":"user","content":"task"}]}`), "metadata")
	require.NoError(t, err)
	candidate := AIRoutingCandidate{ID: "a", Model: "gpt-5.1", ContextWindowTokens: 1000}
	r := AIRoutingRequirementsForCandidate(request, candidate)
	require.Equal(t, CompositeRouteEndpointMessages, r.Endpoint)
	require.False(t, r.OutputLimitOmitted)
	require.False(t, r.RequiresNativeResponses)
	require.False(t, r.RequiresStoredResponses)
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{openai_compat.ExtraKeyResponsesSupported: false}}
	check := CheckAIRoutingExecutionAccount(WithAIRoutingExecutionRequirements(context.Background(), r), account, candidate.Model)
	require.True(t, check.Allowed, check.Reason)
	require.EqualValues(t, 100, check.OutputReserveTokens)
	r.MaxOutputTokens = 0
	check = CheckAIRoutingExecutionAccount(WithAIRoutingExecutionRequirements(context.Background(), r), account, candidate.Model)
	require.Equal(t, "execution_capacity_unknown", check.Reason, "Messages missing output did not acquire Responses omission semantics")
	check = CheckAIRoutingExecutionAccount(context.Background(), nil, "")
	require.True(t, check.Allowed, "ordinary routes without requirements remain untouched")
}
