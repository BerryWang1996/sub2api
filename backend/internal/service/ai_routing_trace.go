package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
)

// Operational history, scoped to the source group and readable only by admins.
// Never include prompts, task text, credentials, or remote error bodies.
type AIRoutingTrace struct {
	At                time.Time `json:"at"`
	RequestID         string    `json:"request_id"`
	ClientRequestID   string    `json:"client_request_id,omitempty"`
	UsageRequestID    string    `json:"usage_request_id,omitempty"`
	PublicModel       string    `json:"public_model"`
	CandidateID       string    `json:"candidate_id"`
	ExecutionModel    string    `json:"execution_model"`
	DecisionModel     string    `json:"decision_model"`
	DecisionAccountID int64     `json:"decision_account_id"`
	InputTokens       int       `json:"input_tokens"`
	OutputTokens      int       `json:"output_tokens"`
	DecisionCostUSD   *float64  `json:"decision_cost_usd,omitempty"`
	LatencyMS         int64     `json:"latency_ms"`
	Source            string    `json:"source"`
	FailureReason     string    `json:"failure_reason,omitempty"`
	PolicyVersion     int64     `json:"policy_version"`
}

// SetRequestContext retains the operational request ID and also records the
// existing Messages billing correlation ID. A failed request may have no usage
// row, and a context without IDs must not invent a different billing identity.
func (t *AIRoutingTrace) SetRequestContext(ctx context.Context) {
	if ctx == nil {
		return
	}
	t.RequestID, _ = ctx.Value(ctxkey.RequestID).(string)
	t.ClientRequestID, _ = ctx.Value(ctxkey.ClientRequestID).(string)
	t.RequestID = strings.TrimSpace(t.RequestID)
	t.ClientRequestID = strings.TrimSpace(t.ClientRequestID)
	if t.ClientRequestID != "" || t.RequestID != "" {
		t.UsageRequestID = resolveUsageBillingRequestID(ctx, "")
	}
}

func SaveAIRoutingTrace(ctx context.Context, store AIRoutingStore, id int64, t AIRoutingTrace) error {
	if store == nil {
		return fmt.Errorf("trace store unavailable")
	}
	return store.SaveTrace(ctx, id, t)
}
func ListAIRoutingDecisions(ctx context.Context, store AIRoutingStore, id int64) ([]AIRoutingTrace, error) {
	if store == nil {
		return nil, fmt.Errorf("trace store unavailable")
	}
	return store.ListTraces(ctx, id)
}

// Catalog estimate at unit multiplier for operator visibility; never debited to the caller.
func (s *GatewayService) EstimateAIDecisionUSD(result AIRoutingDecisionResult) *float64 {
	if s == nil || s.billingService == nil || (result.InputTokens == 0 && result.OutputTokens == 0) || !s.billingService.HasIdentifiedTokenPricing(result.Model) {
		return nil
	}
	cost, err := s.billingService.CalculateCost(result.Model, UsageTokens{InputTokens: result.InputTokens, OutputTokens: result.OutputTokens}, 1)
	if err != nil || cost == nil {
		return nil
	}
	return &cost.TotalCost
}

type aiUsageModelKey struct{}

func AIRoutingBillingModel(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if m, _ := ctx.Value(aiUsageModelKey{}).(string); m != "" {
		return m
	}
	if scope, ok := CompositeRouteScopeFromContext(ctx); ok && scope.AICandidateID != "" {
		return scope.ExecutionModel
	}
	return ""
}

// Preserve only the billing identity across the asynchronous accounting boundary.
func CopyAIRoutingBillingModel(dst, src context.Context) context.Context {
	if model := AIRoutingBillingModel(src); model != "" {
		return context.WithValue(dst, aiUsageModelKey{}, model)
	}
	return dst
}
