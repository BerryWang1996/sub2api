package service

import (
	"context"
	"errors"
	"math"
)

// OutputEventBytes deliberately counts complete received frames, including
// repeated terminal snapshots. This is a conservative context estimate, never
// an exact tokenizer result or proof of provider-retained history capacity.
type AIRoutingWSSteeringBudget struct {
	PriorInputBytes      int64
	AdditionalInputBytes int64
	OutputEventBytes     int64
}

func (b AIRoutingWSSteeringBudget) TotalContextBytes() (int64, error) {
	if b.PriorInputBytes < 0 || b.AdditionalInputBytes <= 0 || b.OutputEventBytes < 0 || b.PriorInputBytes > math.MaxInt64-b.AdditionalInputBytes || b.PriorInputBytes+b.AdditionalInputBytes > math.MaxInt64-b.OutputEventBytes {
		return 0, errors.New("invalid websocket steering budget")
	}
	return b.PriorInputBytes + b.AdditionalInputBytes + b.OutputEventBytes, nil
}

func (s *GatewayService) CheckAIRoutingWSSteeringCapacity(ctx context.Context, accountID int64, model string, budget AIRoutingWSSteeringBudget) error {
	r, ok := AIRoutingExecutionRequirementsFromContext(ctx)
	if !ok || r.Endpoint != AIRoutingEndpointResponsesWS || r.Warmup {
		return errors.New("websocket steering requires a generative execution budget")
	}
	inputBytes, err := budget.TotalContextBytes()
	if err != nil {
		return err
	}
	account, err := s.aiRoutingResponseAccount(ctx, accountID)
	if err != nil || account == nil || !account.IsSchedulable() || !CompositeRouteAccountAllowed(ctx, account) {
		return errors.New("websocket execution account unavailable")
	}
	r.InputBytes = inputBytes // Retain the full original remaining-output reserve.
	check := CheckAIRoutingExecutionAccount(WithAIRoutingExecutionRequirements(ctx, r), account, model)
	if !check.Allowed {
		return &AIRoutingCapacityError{Check: check}
	}
	return ctx.Err()
}

// EstimateAIRoutingWSSteeringReservation uses the ordinary resolver, group
// multipliers and configured inflight caps twice with the SAME output reserve.
// Only the positive input delta is held; each steer does not buy another copy
// of the original max_output_tokens. The estimate retains the ordinary cache's
// configured caps and is not an exact promise of final provider cost.
func (s *OpenAIGatewayService) EstimateAIRoutingWSSteeringReservation(ctx context.Context, key *APIKey, model string, budget AIRoutingWSSteeringBudget, maxOutput, choices int) (float64, bool) {
	if s == nil || budget.PriorInputBytes < 0 || budget.AdditionalInputBytes <= 0 || budget.PriorInputBytes > int64(math.MaxInt)-budget.AdditionalInputBytes {
		return 0, false
	}
	d := s.inflightEstimateDeps()
	d.pricingAt, _ = openAIPricingAtFromContext(ctx)
	if d.pricingAt.IsZero() {
		d.pricingAt, _ = gatewayTokenRequestPricingAtFromContext(ctx)
	}
	before, knownBefore := d.estimate(ctx, key, InflightEstimateRequest{Model: model, BodyBytes: int(budget.PriorInputBytes), MaxTokens: maxOutput, OutputChoices: choices})
	after, knownAfter := d.estimate(ctx, key, InflightEstimateRequest{Model: model, BodyBytes: int(budget.PriorInputBytes + budget.AdditionalInputBytes), MaxTokens: maxOutput, OutputChoices: choices})
	if !knownBefore || !knownAfter {
		return 0, false
	}
	return math.Max(0, after-before), true
}
