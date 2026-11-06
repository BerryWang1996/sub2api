package service

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
)

type aiBackgroundSubmissionKey struct{}
type aiBackgroundBillingOwnerKey struct{}
type aiBackgroundSubmission struct {
	used       atomic.Bool
	beforeSend func(*http.Request, int64) error
}

// WithAIRoutingBackgroundSubmission is installed only for an admitted native
// background create. Unlike a reusable capacity check this is consumed once,
// immediately before the transport, after the durable reserve and source checks.
func WithAIRoutingBackgroundSubmission(ctx context.Context, beforeSend func(*http.Request, int64) error) context.Context {
	ctx = context.WithValue(ctx, aiBackgroundBillingOwnerKey{}, true)
	return context.WithValue(ctx, aiBackgroundSubmissionKey{}, &aiBackgroundSubmission{beforeSend: beforeSend})
}

func AIRoutingBackgroundOwnsBilling(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	owned, _ := ctx.Value(aiBackgroundBillingOwnerKey{}).(bool)
	return owned
}

// Copy only accounting ownership into worker contexts, never a live HTTP
// submission callback or its credentials/request state.
func CopyAIRoutingBackgroundBillingOwner(dst, src context.Context) context.Context {
	if AIRoutingBackgroundOwnsBilling(src) {
		return context.WithValue(dst, aiBackgroundBillingOwnerKey{}, true)
	}
	return dst
}

func HasAIRoutingBackgroundSubmission(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	state, ok := ctx.Value(aiBackgroundSubmissionKey{}).(*aiBackgroundSubmission)
	return ok && state != nil
}

func BeginAIRoutingBackgroundSubmission(req *http.Request, accountID int64) error {
	if req == nil {
		return nil
	}
	state, _ := req.Context().Value(aiBackgroundSubmissionKey{}).(*aiBackgroundSubmission)
	if state == nil {
		return nil
	}
	if state.beforeSend == nil || !state.used.CompareAndSwap(false, true) || req.Context().Err() != nil {
		return &AIRoutingCapacityError{Check: AIRoutingCapabilityCheck{Reason: "execution_background_submission_unavailable"}}
	}
	if err := state.beforeSend(req, accountID); err != nil {
		reason := "execution_background_submission_unavailable"
		switch {
		case errors.Is(err, ErrAIBackgroundUnsupported):
			reason = "execution_background_unsupported"
		case errors.Is(err, ErrAIBackgroundCapacity), errors.Is(err, ErrAPIKeyRateLimit5hExceeded), errors.Is(err, ErrAPIKeyRateLimit1dExceeded), errors.Is(err, ErrAPIKeyRateLimit7dExceeded), errors.Is(err, ErrUserPlatformDailyQuotaExhausted), errors.Is(err, ErrUserPlatformWeeklyQuotaExhausted), errors.Is(err, ErrUserPlatformMonthlyQuotaExhausted):
			reason = "execution_background_capacity_exceeded"
		case errors.Is(err, ErrInsufficientBalance), errors.Is(err, ErrDailyLimitExceeded), errors.Is(err, ErrWeeklyLimitExceeded), errors.Is(err, ErrMonthlyLimitExceeded), errors.Is(err, ErrAPIKeyQuotaExhausted):
			reason = "execution_background_quota_exceeded"
		case errors.Is(err, ErrAIRoutingPrincipalRevoked), errors.Is(err, ErrAIRoutingDecisionRevoked):
			reason = "execution_background_authorization_revoked"
		}
		return &AIRoutingCapacityError{Check: AIRoutingCapabilityCheck{Reason: reason}}
	}
	return nil
}
