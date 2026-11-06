//go:build unit

package service

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestAIRoutingChatInflightChoicesMultiplyAfterDefaultsAndCaps(t *testing.T) {
	svc := newInflightEstimateGateway(t, nil)
	svc.cfg.Billing.InflightReservation.DefaultMaxTokens = 80
	svc.cfg.Billing.InflightReservation.MaxOutputTokens = 100
	key := &APIKey{User: &User{ID: 1}, Group: &Group{RateMultiplier: 1}}
	for _, max := range []int{0, 50, 200} {
		req := InflightEstimateRequest{Model: "claude-sonnet-4-5", BodyBytes: 400, MaxTokens: max}
		legacy, priced := svc.EstimateInflightReservation(t.Context(), key, req)
		require.True(t, priced)
		req.OutputChoices = 1
		one, priced := svc.EstimateInflightReservation(t.Context(), key, req)
		require.True(t, priced)
		require.Equal(t, legacy, one)
		req.OutputChoices = 2
		two, priced := svc.EstimateInflightReservation(t.Context(), key, req)
		require.True(t, priced)
		perChoice := max
		if perChoice == 0 {
			perChoice = 80
		}
		if perChoice > 100 {
			perChoice = 100
		}
		pricing, err := svc.billingService.GetModelPricing(req.Model)
		require.NoError(t, err)
		require.InDelta(t, one+float64(perChoice)*pricing.OutputPricePerToken, two, 1e-12, "input is charged once and output limit is multiplied exactly once")
	}
	maxInt := int(^uint(0) >> 1)
	require.Equal(t, maxInt, aggregateInflightOutputTokens(maxInt, 64))
	require.Equal(t, maxInt, aggregateInflightOutputTokens(2, maxInt))
}
