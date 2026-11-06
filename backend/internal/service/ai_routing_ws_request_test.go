//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAIRoutingWSWarmupKeepsRawFrameAndNoInventedHistory(t *testing.T) {
	body := []byte(`{"type":"response.create","generate":false,"model":"alias","instructions":"private instructions only","store":false}`)
	r, err := NewAIRoutingWSRequest(body, "bounded_history")
	require.NoError(t, err)
	require.True(t, r.Warmup)
	require.Equal(t, body, r.HistoryBody)
	require.Equal(t, len(body), r.InputBytes)
	require.False(t, gjson.GetBytes(r.HistoryBody, "input").Exists())
	history, err := ParseAIRoutingWSHistory(body, AIRoutingContextLimits{})
	require.NoError(t, err)
	require.Zero(t, history.HistoryBytes)
	require.Zero(t, history.HistoryTurns)
	_, err = NewAIRoutingWSRequest([]byte(`{"type":"response.create","generate":"false","model":"alias","input":[]}`), "metadata")
	require.Error(t, err)
	_, err = NewAIRoutingWSRequest([]byte(`{"type":"response.create","generate":true,"model":"alias"}`), "metadata")
	require.Error(t, err)
}

func TestAIRoutingWSWarmupFinalGuardRequiresNoGenerationButChecksInputWindow(t *testing.T) {
	body := `{"type":"response.create","generate":false,"model":"gpt-5.1","instructions":"warmup only","store":false}`
	r := AIRoutingExecutionRequirements{Endpoint: AIRoutingEndpointResponsesWS, Warmup: true, RequiresNativeResponses: true, InputBytes: int64(len(body)), InputModalities: []string{"text"}, ContextWindowTokens: int64(len(body)), OutputLimitOmitted: true}
	ctx := WithAIRoutingExecutionRequirements(context.Background(), r)
	check := CheckAIRoutingFinalPayload(ctx, aiCapabilityAccount(), "gpt-5.1", aiFinalTestRequest(t, "/v1/responses", body))
	require.True(t, check.Allowed, check.Reason)
	require.Zero(t, check.OutputReserveTokens)
	require.Equal(t, "final_warmup_input_bytes", check.CapacityMethod)
	r.ContextWindowTokens--
	check = CheckAIRoutingFinalPayload(WithAIRoutingExecutionRequirements(ctx, r), aiCapabilityAccount(), "gpt-5.1", aiFinalTestRequest(t, "/v1/responses", body))
	require.Equal(t, "execution_context_window_exceeded", check.Reason)
	r.ContextWindowTokens = 100000
	check = CheckAIRoutingFinalPayload(WithAIRoutingExecutionRequirements(ctx, r), aiCapabilityAccount(), "gpt-5.1", aiFinalTestRequest(t, "/v1/responses", `{"type":"response.create","generate":true,"model":"gpt-5.1","input":[]}`))
	require.Equal(t, "execution_warmup_changed", check.Reason)
}

func TestAIRoutingWSNativeTransportWarmupKeepsGenerateFalseAndActualUsage(t *testing.T) {
	for _, unexpected := range []bool{false, true} {
		body := `{"type":"response.create","generate":false,"model":"gpt-5.1","input":[],"max_output_tokens":64,"store":false}`
		turn, conn, _ := aiNativeWSTransportFixture(t, nil, body)
		conn.events <- []byte(`{"type":"response.created","response":{"id":"resp_warm"}}`)
		if unexpected {
			conn.events <- []byte(`{"type":"response.output_text.delta","delta":"must not expose this generated output"}`)
		}
		conn.events <- []byte(`{"type":"response.completed","response":{"id":"resp_warm","output":[],"usage":{"input_tokens":3,"output_tokens":0}}}`)
		var output [][]byte
		result, err := turn.Run(func(event []byte) error { output = append(output, event); return nil })
		if unexpected {
			require.ErrorContains(t, err, "generated output")
			require.Len(t, output, 1)
		} else {
			require.NoError(t, err)
			require.Len(t, output, 2)
		}
		require.Equal(t, gjson.False, gjson.GetBytes(conn.writes[0], "generate").Type)
		require.Equal(t, 3, result.Usage.InputTokens, "never erase reported warmup usage")
	}
}
