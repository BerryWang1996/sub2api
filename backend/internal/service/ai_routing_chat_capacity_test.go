//go:build unit

package service

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAIRoutingChatCapacityNativeChoicesAndWireCount(t *testing.T) {
	account := aiCapabilityAccount()
	account.Extra = map[string]any{"openai_responses_mode": "force_chat_completions"}
	body := `{"model":"gpt-5.1","messages":[{"role":"user","content":"hi"}],"n":2,"max_completion_tokens":100}`
	r := AIRoutingExecutionRequirements{Endpoint: CompositeRouteEndpointChatCompletions, RequiresNativeChat: true, InputModalities: []string{"text"}, OutputChoices: 2, InputBytes: int64(len(body)), MaxOutputTokens: 100, ContextWindowTokens: int64(len(body)) + 100}
	ctx := WithAIRoutingExecutionRequirements(context.Background(), r)
	check := CheckAIRoutingFinalPayload(ctx, account, "gpt-5.1", aiFinalTestRequest(t, "/v1/chat/completions", body))
	require.True(t, check.Allowed, check.Reason)
	require.EqualValues(t, 100, check.OutputReserveTokens)
	require.EqualValues(t, 200, check.TotalOutputReserveTokens)
	require.Equal(t, 2, check.OutputChoices)
	for _, changed := range []string{strings.Replace(body, `"n":2,`, "", 1), strings.Replace(body, `"n":2`, `"n":1`, 1), strings.Replace(body, `"n":2`, `"n":3`, 1)} {
		check = CheckAIRoutingFinalPayload(ctx, account, "gpt-5.1", aiFinalTestRequest(t, "/v1/chat/completions", changed))
		require.Equal(t, "execution_output_choices_changed", check.Reason)
	}
	check = CheckAIRoutingFinalPayload(ctx, account, "gpt-5.1", aiFinalTestRequest(t, "/v1/responses", `{"model":"gpt-5.1","input":"hi","max_output_tokens":100}`))
	require.Equal(t, "execution_native_chat_required", check.Reason)
	r.MaxOutputTokens = math.MaxInt64
	r.ContextWindowTokens = math.MaxInt64
	check = CheckAIRoutingExecutionAccount(WithAIRoutingExecutionRequirements(ctx, r), account, "gpt-5.1")
	require.Equal(t, "execution_output_reserve_overflow", check.Reason)
}

func TestAIRoutingChatCapacityNativeMediaNeedsMatchingMetadata(t *testing.T) {
	for _, modality := range []string{"audio", "document"} {
		account := aiCapabilityAccount()
		account.Extra = map[string]any{"openai_responses_mode": "force_chat_completions"}
		req := AIRoutingRequest{Endpoint: CompositeRouteEndpointChatCompletions, RequiresNativeChat: true, InputModalities: []string{"text", modality}, HasImages: true, InputBytes: 100, MaxOutput: 50}
		r := AIRoutingRequirementsForCandidate(req, AIRoutingCandidate{ContextWindowTokens: 1000})
		require.NotContains(t, r.InputModalities, "attachment")
		ctx := WithAIRoutingExecutionRequirements(t.Context(), r)
		require.Equal(t, "execution_modality_unknown", CheckAIRoutingExecutionAccount(ctx, account, "gpt-5.1").Reason)
		aiCapabilityMetadata(account, 1000, 100, "text", modality)
		check := CheckAIRoutingExecutionAccount(ctx, account, "gpt-5.1")
		require.True(t, check.Allowed, check.Reason)
		require.False(t, check.CapacityVerified)
		require.Contains(t, check.Warnings, "attachment_token_usage_unverified")
		account.Extra = map[string]any{"openai_responses_mode": "force_responses"}
		require.Equal(t, "execution_native_chat_required", CheckAIRoutingExecutionAccount(ctx, account, "gpt-5.1").Reason)
	}
}
