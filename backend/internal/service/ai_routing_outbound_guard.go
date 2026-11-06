package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
)

type aiRoutingOutboundGuardKey struct{}

// The guard is request-local and installed only after an AI execution target
// has been selected. Decision calls and ordinary/static routes do not inherit it.
func WithAIRoutingOutboundGuard(ctx context.Context, guard func(*http.Request, int64) error) context.Context {
	return context.WithValue(ctx, aiRoutingOutboundGuardKey{}, guard)
}

func HasAIRoutingOutboundGuard(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	guard, _ := ctx.Value(aiRoutingOutboundGuardKey{}).(func(*http.Request, int64) error)
	return guard != nil
}

func CheckAIRoutingOutboundRequest(req *http.Request, accountID int64) error {
	if req == nil {
		return nil
	}
	guard, _ := req.Context().Value(aiRoutingOutboundGuardKey{}).(func(*http.Request, int64) error)
	if guard == nil {
		return nil
	}
	return guard(req, accountID)
}

// AIRoutingCapacityError is a local refusal, never an upstream health failure.
// Its payload contains diagnostics only, not request text, headers or credentials.
type AIRoutingCapacityError struct {
	Check AIRoutingCapabilityCheck
}

func (e *AIRoutingCapacityError) Error() string {
	return "AI routing execution capacity rejected: " + e.Check.Reason
}

func IsAIRoutingCapacityError(err error) bool {
	var refused *AIRoutingCapacityError
	return errors.As(err, &refused)
}

// Retain the original cancellation signal: stream transports may intentionally
// detach their request context, while routing admission must still be cancellable.
func (s *GatewayService) WithAIRoutingFinalCapacityGuard(ctx context.Context) context.Context {
	if _, enabled := AIRoutingExecutionRequirementsFromContext(ctx); !enabled {
		return ctx
	}
	return WithAIRoutingOutboundGuard(ctx, func(req *http.Request, accountID int64) error {
		deny := func(reason string) error {
			return &AIRoutingCapacityError{Check: AIRoutingCapabilityCheck{Reason: reason}}
		}
		if ctx.Err() != nil {
			return deny("execution_cancelled")
		}
		if check, ok := ctx.Value(aiDecisionGuardKey{}).(func(context.Context) error); ok {
			if err := check(ctx); err != nil {
				return deny("execution_policy_changed")
			}
		}
		scope, ok := CompositeRouteScopeFromContext(ctx)
		if !ok || scope.AICandidateID == "" || accountID <= 0 || s == nil || s.accountRepo == nil {
			return deny("execution_account_unavailable")
		}
		if scope.TargetGroupID != 0 {
			if err := ValidateCompositeRouteScope(ctx, s.groupRepo, s.accountRepo); err != nil {
				return deny("execution_account_unavailable")
			}
		}
		account, err := s.accountRepo.GetByID(ctx, accountID)
		if err != nil || account == nil || account.ID != accountID || !account.IsSchedulable() || !CompositeRouteAccountAllowed(ctx, account) {
			return deny("execution_account_unavailable")
		}
		result := CheckAIRoutingFinalPayload(ctx, account, scope.ExecutionModel, req)
		if ctx.Err() != nil {
			return deny("execution_cancelled")
		}
		if !result.Allowed {
			return &AIRoutingCapacityError{Check: result}
		}
		// Inspecting a large body can outlive the destination grant. Re-read
		// the target without re-reading the body, and use the final wire budget
		// for its current limits. These are latest snapshots, not a transaction
		// lock spanning authorization changes and the subsequent network send.
		finalRequirements, _ := AIRoutingExecutionRequirementsFromContext(ctx)
		finalRequirements.InputBytes = result.InputBytes
		finalRequirements.MaxOutputTokens = result.OutputReserveTokens
		if scope.TargetGroupID != 0 {
			if err := ValidateCompositeRouteScope(ctx, s.groupRepo, s.accountRepo); err != nil {
				return deny("execution_account_unavailable")
			}
		}
		latest, err := s.accountRepo.GetByID(ctx, accountID)
		if ctx.Err() != nil {
			return deny("execution_cancelled")
		}
		if err != nil || latest == nil || latest.ID != accountID || !latest.IsSchedulable() {
			return deny("execution_account_unavailable")
		}
		if aiFinalCapacityContains(result.Warnings, "output_reserve_from_metadata_maximum") {
			// No output field was sent, so a changed catalog maximum changes the
			// required reserve as well. A candidate ceiling cannot replace it.
			metadata, known := aiFinalCapacityMetadata(latest, scope.ExecutionModel)
			if !known || metadata.MaxOutputTokens <= 0 {
				return deny("execution_output_reserve_unknown")
			}
			finalRequirements.MaxOutputTokens = metadata.MaxOutputTokens
		}
		finalCtx := WithAIRoutingExecutionRequirements(ctx, finalRequirements)
		latestCheck := CheckAIRoutingExecutionAccount(finalCtx, latest, scope.ExecutionModel)
		latestCheck.CapacityMethod = result.CapacityMethod
		latestCheck.FinalPayloadChecked = result.FinalPayloadChecked
		for _, warning := range result.Warnings {
			if !aiFinalCapacityContains(latestCheck.Warnings, warning) {
				latestCheck.Warnings = append(latestCheck.Warnings, warning)
			}
		}
		if !latestCheck.Allowed {
			return &AIRoutingCapacityError{Check: latestCheck}
		}
		if !CompositeRouteAccountAllowed(finalCtx, latest) {
			return deny("execution_account_unavailable")
		}
		// The account lookup and body inspection can wait or take time. A policy
		// revoked during that work must not reuse the earlier admission result.
		if check, ok := ctx.Value(aiDecisionGuardKey{}).(func(context.Context) error); ok {
			if err := check(ctx); err != nil {
				return deny("execution_policy_changed")
			}
		}
		if ctx.Err() != nil {
			return deny("execution_cancelled")
		}
		return nil
	})
}

// Refusal follows the client protocol, including when a keepalive began SSE.
// Returning true tells transport wrappers not to retry, switch account, or
// record the event as an upstream fault.
func handleAIRoutingCapacityError(c *gin.Context, err error) bool {
	var refused *AIRoutingCapacityError
	if !errors.As(err, &refused) {
		return false
	}
	if c == nil {
		return true
	}
	c.Abort()
	MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalPolicyDenied)
	if c.Request != nil && c.Request.Context().Err() != nil {
		return true
	}
	if IsResponseCommitted(c) {
		return true
	}
	status, code, message := http.StatusBadRequest, "ai_execution_capacity_rejected", "The selected AI route cannot satisfy the execution capacity requirements."
	switch refused.Check.Reason {
	case "ai_source_revoked":
		status, code, message = http.StatusForbidden, "ai_source_revoked", "The credentials or source group are no longer authorized."
	case "execution_background_submission_unavailable":
		status, code, message = http.StatusServiceUnavailable, "ai_background_submission_unavailable", "Background response submission could not be confirmed."
	case "execution_background_unsupported":
		status, code, message = http.StatusBadRequest, "ai_background_unsupported", "The selected account or request features do not support durable background settlement."
	case "execution_background_capacity_exceeded":
		status, code, message = http.StatusTooManyRequests, "ai_background_capacity_exceeded", "Background task capacity or a quota window has been exceeded."
	case "execution_background_quota_exceeded":
		status, code, message = http.StatusForbidden, "ai_background_quota_exceeded", "The remaining balance or quota cannot cover the background reservation."
	case "execution_background_authorization_revoked":
		status, code, message = http.StatusForbidden, "ai_background_authorization_revoked", "The credentials or selected route are no longer authorized."
	}
	payload := gin.H{"type": "error", "error": gin.H{"type": "invalid_request_error", "code": code, "message": message}}
	responses := false
	chat := false
	gemini := false
	if c.Request != nil {
		requirements, _ := AIRoutingExecutionRequirementsFromContext(c.Request.Context())
		responses = requirements.Endpoint == CompositeRouteEndpointResponses
		chat = requirements.Endpoint == CompositeRouteEndpointChatCompletions
		gemini = requirements.Endpoint == CompositeRouteEndpointGemini
	}
	if gemini {
		googleStatus := "INVALID_ARGUMENT"
		switch status {
		case http.StatusForbidden:
			googleStatus = "PERMISSION_DENIED"
		case http.StatusTooManyRequests:
			googleStatus = "RESOURCE_EXHAUSTED"
		case http.StatusServiceUnavailable:
			googleStatus = "UNAVAILABLE"
		}
		payload = gin.H{"error": gin.H{"code": status, "status": googleStatus, "message": message}}
	}
	if c.Writer.Written() {
		if responses {
			payload = gin.H{"type": "error", "code": code, "message": message, "param": nil}
		}
		encoded, _ := json.Marshal(payload)
		if chat {
			_, _ = fmt.Fprintf(c.Writer, "data: %s\n\ndata: [DONE]\n\n", encoded)
		} else if gemini {
			_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", encoded)
		} else {
			_, _ = fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", encoded)
		}
		c.Writer.Flush()
	} else {
		c.JSON(status, payload)
	}
	MarkResponseCommitted(c)
	return true
}
