//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAIRoutingGeminiFinalCapacityCountsAllCandidates(t *testing.T) {
	account := aiCapabilityAccount()
	account.Platform = PlatformGemini
	for _, body := range []string{
		`{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"generationConfig":{"maxOutputTokens":100,"candidateCount":3}}`,
		`{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"generation_config":{"max_output_tokens":100,"candidate_count":3}}`,
	} {
		r := AIRoutingExecutionRequirements{Endpoint: CompositeRouteEndpointGemini, RequiresNativeGemini: true, InputModalities: []string{"text"}, InputBytes: 1, MaxOutputTokens: 100, OutputChoices: 3, ContextWindowTokens: 10000}
		ctx := WithAIRoutingExecutionRequirements(context.Background(), r)
		check := CheckAIRoutingFinalPayload(ctx, account, "gpt-5.1", aiFinalTestRequest(t, "/v1beta/models/gpt-5.1:generateContent", body))
		require.True(t, check.Allowed, check.Reason)
		require.EqualValues(t, 100, check.OutputReserveTokens)
		require.EqualValues(t, 300, check.TotalOutputReserveTokens)
		r.OutputChoices = 1
		check = CheckAIRoutingFinalPayload(WithAIRoutingExecutionRequirements(ctx, r), account, "gpt-5.1", aiFinalTestRequest(t, "/v1beta/models/gpt-5.1:generateContent", body))
		require.Equal(t, "execution_output_choices_changed", check.Reason)
	}
}

func TestAIRoutingGeminiCountFinalWireCannotBecomeGeneration(t *testing.T) {
	account := aiCapabilityAccount()
	account.Platform = PlatformGemini
	r := AIRoutingExecutionRequirements{Endpoint: CompositeRouteEndpointGemini, Operation: AIRoutingGeminiCountTokens, RequiresNativeGemini: true, InputModalities: []string{"text"}, InputBytes: 1}
	ctx := WithAIRoutingExecutionRequirements(context.Background(), r)
	body := `{"generateContentRequest":{"model":"models/gpt-5.1","contents":[{"role":"user","parts":[{"text":"count this"}]}]}}`
	check := CheckAIRoutingFinalPayload(ctx, account, "gpt-5.1", aiFinalTestRequest(t, "/v1beta/models/gpt-5.1:countTokens", body))
	require.True(t, check.Allowed, check.Reason)
	require.Zero(t, check.OutputReserveTokens)
	check = CheckAIRoutingFinalPayload(ctx, account, "gpt-5.1", aiFinalTestRequest(t, "/v1beta/models/gpt-5.1:generateContent", body))
	require.Equal(t, "execution_final_protocol_unknown", check.Reason)
}
