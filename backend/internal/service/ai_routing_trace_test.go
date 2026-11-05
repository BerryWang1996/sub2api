package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

func TestAIRoutingTraceCorrelatesWithUsageBillingID(t *testing.T) {
	for _, tc := range []struct {
		name, requestID, clientID, usageID string
	}{
		{"both IDs differ", "operations-id", "client-id", "client:client-id"},
		{"client only", "", " client-id ", "client:client-id"},
		{"local fallback", " operations-id ", "", "local:operations-id"},
		{"no generated identity", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.WithValue(context.Background(), ctxkey.RequestID, tc.requestID)
			ctx = context.WithValue(ctx, ctxkey.ClientRequestID, tc.clientID)
			trace := AIRoutingTrace{}
			trace.SetRequestContext(ctx)
			require.Equal(t, tc.usageID, trace.UsageRequestID)
			if tc.usageID != "" {
				require.Equal(t, resolveUsageBillingRequestID(ctx, "provider-request-id"), trace.UsageRequestID)
			}
		})
	}
	var legacy AIRoutingTrace
	require.NoError(t, json.Unmarshal([]byte(`{"request_id":"old-operations-id","source":"model"}`), &legacy))
	require.Equal(t, "old-operations-id", legacy.RequestID)
	require.Empty(t, legacy.ClientRequestID)
	require.Empty(t, legacy.UsageRequestID)
	legacy.SetRequestContext(context.Background())
	require.Empty(t, legacy.UsageRequestID)
}

func TestAIRoutingTraceUnknownCostIsNotReportedAsFree(t *testing.T) {
	gateway := &GatewayService{}
	require.Nil(t, gateway.EstimateAIDecisionUSD(AIRoutingDecisionResult{Model: "custom", InputTokens: 100}))
	var absent *GatewayService
	require.Nil(t, absent.EstimateAIDecisionUSD(AIRoutingDecisionResult{}))
	require.Empty(t, AIRoutingBillingModel(CopyAIRoutingBillingModel(context.Background(), context.Background())))
}

func TestAIRoutingTraceWithoutStoreRemainsUnavailable(t *testing.T) {
	ctx := context.Background()
	require.EqualError(t, SaveAIRoutingTrace(ctx, nil, 5, AIRoutingTrace{}), "trace store unavailable")
	rows, err := ListAIRoutingDecisions(ctx, nil, 5)
	require.EqualError(t, err, "trace store unavailable")
	require.Nil(t, rows)
}
