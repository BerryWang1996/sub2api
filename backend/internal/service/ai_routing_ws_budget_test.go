//go:build unit

package service

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAIRoutingWSSteeringCapacityCountsInputOutputAndKeepsOutputReserve(t *testing.T) {
	turn, _, account := aiNativeWSTransportFixture(t, nil, aiNativeWSCreate)
	gate := &GatewayService{accountRepo: &aiOutboundAccountRepo{account: account}}
	budget := AIRoutingWSSteeringBudget{PriorInputBytes: int64(len(turn.payload)), AdditionalInputBytes: 20, OutputEventBytes: 30}
	r, _ := AIRoutingExecutionRequirementsFromContext(turn.ctx)
	r.ContextWindowTokens = budget.PriorInputBytes + 20 + 30 + 64
	ctx := WithAIRoutingExecutionRequirements(turn.ctx, r)
	require.NoError(t, gate.CheckAIRoutingWSSteeringCapacity(ctx, account.ID, "gpt-5.1", budget))
	budget.OutputEventBytes++
	require.ErrorContains(t, gate.CheckAIRoutingWSSteeringCapacity(ctx, account.ID, "gpt-5.1", budget), "context_window_exceeded")
	budget.OutputEventBytes = 30
	account.Status = "inactive"
	require.Error(t, gate.CheckAIRoutingWSSteeringCapacity(ctx, account.ID, "gpt-5.1", budget), "latest account status must be read")
	_, err := (AIRoutingWSSteeringBudget{PriorInputBytes: math.MaxInt64, AdditionalInputBytes: 1}).TotalContextBytes()
	require.Error(t, err)
}

func TestAIRoutingWSSteeringReservationAddsOnlyInputDeltaWithExistingPricing(t *testing.T) {
	base := newInflightEstimateGateway(t, nil)
	svc := &OpenAIGatewayService{cfg: base.cfg, billingService: base.billingService, resolver: base.resolver}
	key := &APIKey{User: &User{ID: 1}}
	budget := AIRoutingWSSteeringBudget{PriorInputBytes: 4000, AdditionalInputBytes: 4000, OutputEventBytes: 100000}
	first, known := svc.EstimateAIRoutingWSSteeringReservation(t.Context(), key, "claude-sonnet-4-5", budget, 100, 1)
	require.True(t, known)
	require.Positive(t, first)
	second, known := svc.EstimateAIRoutingWSSteeringReservation(t.Context(), key, "claude-sonnet-4-5", budget, 10000, 1)
	require.True(t, known)
	require.InDelta(t, first, second, 1e-12, "another output budget is not reserved for every steer")
	before, _ := svc.EstimateInflightReservation(t.Context(), key, InflightEstimateRequest{Model: "claude-sonnet-4-5", BodyBytes: 4000, MaxTokens: 100})
	after, _ := svc.EstimateInflightReservation(t.Context(), key, InflightEstimateRequest{Model: "claude-sonnet-4-5", BodyBytes: 8000, MaxTokens: 100})
	require.InDelta(t, after-before, first, 1e-12)
	_, known = svc.EstimateAIRoutingWSSteeringReservation(t.Context(), key, "synthetic-unknown-model-xyz", budget, 100, 1)
	require.False(t, known)
}

func TestAIRoutingWSSteeringReservationKeepsTurnPeakPricingTime(t *testing.T) {
	base := newInflightEstimateGateway(t, nil)
	svc := &OpenAIGatewayService{cfg: base.cfg, billingService: base.billingService, resolver: base.resolver}
	id := int64(3)
	key := &APIKey{User: &User{ID: 1}, GroupID: &id, Group: &Group{ID: id, RateMultiplier: 1, SubscriptionType: SubscriptionTypeSubscription, PeakRateEnabled: true, PeakStart: "08:00", PeakEnd: "10:00", PeakRateMultiplier: 3}}
	budget := AIRoutingWSSteeringBudget{PriorInputBytes: 4000, AdditionalInputBytes: 4000}
	peak := time.Date(2026, 11, 6, 9, 0, 0, 0, timezone.Location())
	quiet := peak.Add(4 * time.Hour)
	peakCtx := context.WithValue(t.Context(), openAIPricingAtCtxKey{}, peak)
	quietCtx := context.WithValue(t.Context(), openAIPricingAtCtxKey{}, quiet)
	one, ok := svc.EstimateAIRoutingWSSteeringReservation(peakCtx, key, "claude-sonnet-4-5", budget, 100, 1)
	require.True(t, ok)
	two, ok := svc.EstimateAIRoutingWSSteeringReservation(quietCtx, key, "claude-sonnet-4-5", budget, 100, 1)
	require.True(t, ok)
	require.InDelta(t, two*3, one, 1e-12, "steering reservation follows the frozen turn time, not wall-clock time")
}

func TestAIRoutingWSNativeSteeringAccumulatesBudgetAndRejectsBeforeSecondWrite(t *testing.T) {
	turn, conn, _ := aiNativeWSTransportFixture(t, nil, aiNativeWSCreate)
	var budgets []AIRoutingWSSteeringBudget
	reserves := 0
	turn.options.CheckSteer = func(_ context.Context, b AIRoutingWSSteeringBudget) error {
		budgets = append(budgets, b)
		if b.PriorInputBytes > int64(len(turn.payload)) {
			return errors.New("synthetic cumulative window exhausted")
		}
		return nil
	}
	turn.options.ReserveSteer = func(context.Context, AIRoutingWSSteeringBudget) (func(), error) { reserves++; return func() {}, nil }
	conn.onWrite = func(body []byte) {
		if gjson.GetBytes(body, "type").Str == "response.create" {
			conn.events <- []byte(`{"type":"response.created","response":{"id":"resp_before"}}`)
			return
		}
		conn.events <- []byte(`{"type":"response.incomplete","response":{"id":"resp_before","incomplete_details":{"reason":"steered"}}}`)
		conn.events <- []byte(`{"type":"response.created","response":{"id":"resp_after"}}`)
		conn.events <- []byte(`{"type":"response.completed","response":{"id":"resp_after","usage":{"input_tokens":2,"output_tokens":1}}}`)
	}
	_, err := turn.Run(func(event []byte) error {
		if gjson.GetBytes(event, "type").Str != "response.created" {
			return nil
		}
		id := gjson.GetBytes(event, "response.id").Str
		err := turn.Steer(t.Context(), []byte(`{"type":"response.steer","previous_response_id":"`+id+`","input":"additional context"}`))
		if id == "resp_after" {
			require.ErrorContains(t, err, "cumulative window")
			return nil
		}
		return err
	})
	require.NoError(t, err)
	require.Len(t, conn.writes, 2)
	require.Equal(t, 1, reserves)
	require.Len(t, budgets, 3)
	require.Positive(t, budgets[0].OutputEventBytes)
	require.Greater(t, budgets[2].PriorInputBytes, budgets[0].PriorInputBytes)
	require.Greater(t, budgets[2].OutputEventBytes, budgets[0].OutputEventBytes)
}

func TestAIRoutingWSNativeSteeringReleasesReservationWhenFinalAdmissionChanges(t *testing.T) {
	turn, conn, _ := aiNativeWSTransportFixture(t, nil, aiNativeWSCreate)
	checks, released := 0, 0
	turn.options.CheckSteer = func(context.Context, AIRoutingWSSteeringBudget) error {
		checks++
		if checks == 2 {
			return errors.New("synthetic final capacity changed")
		}
		return nil
	}
	turn.options.ReserveSteer = func(context.Context, AIRoutingWSSteeringBudget) (func(), error) { return func() { released++ }, nil }
	conn.events <- []byte(`{"type":"response.created","response":{"id":"resp_check"}}`)
	conn.events <- []byte(`{"type":"response.completed","response":{"id":"resp_check"}}`)
	_, err := turn.Run(func(event []byte) error {
		if gjson.GetBytes(event, "type").Str == "response.created" {
			require.ErrorContains(t, turn.Steer(t.Context(), []byte(`{"type":"response.steer","previous_response_id":"resp_check","input":"update"}`)), "final capacity")
		}
		return nil
	})
	require.NoError(t, err)
	require.Len(t, conn.writes, 1)
	require.Equal(t, 1, released)
}
