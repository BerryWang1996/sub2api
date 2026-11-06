package service

import (
	"context"
	"math"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

type backgroundReservationReader interface {
	Reserved(context.Context, int64, int64, int64) (AIRoutingBackgroundReserved, error)
}

// SetBackgroundReservations is configured once during dependency assembly.
// A connected ledger is authoritative: an unavailable read must not silently
// turn a durable reservation into a cache-only or unreserved request.
func (s *BillingCacheService) SetBackgroundReservations(reader backgroundReservationReader) {
	s.backgroundReservations = reader
}

// Check actual usage and outstanding background holds in one DB snapshot.
// Reading hold totals and then stale cached usage would undercount a task that
// settled between those reads. The repository returns both from one statement.
func (s *BillingCacheService) checkBackgroundReservations(ctx context.Context, user *User, key *APIKey, group *Group, subscription *UserSubscription, balancePending, subscriptionPending, keyPending float64, fixed bool) error {
	if s.backgroundReservations == nil {
		return nil
	}
	simple := s.cfg != nil && s.cfg.RunMode == config.RunModeSimple
	if simple && (!s.cfg.SimpleModeKeyRateLimitEnabled || key == nil || !key.HasRateLimits()) {
		return nil
	}
	if user == nil || user.ID <= 0 {
		return ErrBillingServiceUnavailable
	}
	subscriptionID, keyID := int64(0), int64(0)
	if !simple && group != nil && group.IsSubscriptionType() && subscription != nil {
		subscriptionID = subscription.ID
	}
	if key != nil {
		keyID = key.ID
	}
	snapshot, err := s.backgroundReservations.Reserved(ctx, user.ID, subscriptionID, keyID)
	if err != nil {
		return ErrBillingServiceUnavailable.WithCause(err)
	}
	if snapshot.UserID != user.ID || snapshot.SubscriptionID != subscriptionID || snapshot.APIKeyID != keyID || math.IsNaN(snapshot.UserBalance) || math.IsInf(snapshot.UserBalance, 0) {
		return ErrBillingServiceUnavailable
	}
	for _, value := range []float64{snapshot.SubscriptionDailyUsage, snapshot.SubscriptionWeeklyUsage, snapshot.SubscriptionMonthlyUsage, snapshot.KeyQuotaUsed, snapshot.KeyEffective5hUsage, snapshot.KeyEffective1dUsage, snapshot.KeyEffective7dUsage, snapshot.SubscriptionUSD, snapshot.KeyQuotaUSD, snapshot.KeyRateUSD, balancePending, subscriptionPending, keyPending} {
		if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return ErrBillingServiceUnavailable
		}
	}
	exceeds := func(actual, held, pending, limit float64) bool {
		if limit <= 0 {
			return false
		}
		// A fixed fee may use the last dollar. Unknown future generation must
		// have positive headroom, just like ordinary eligibility checks.
		if fixed {
			return actual+held+pending > limit
		}
		return actual+held+pending >= limit
	}
	if !simple {
		if subscriptionID == 0 {
			// Reserve already moved background money out of spendable balance.
			if fixed && snapshot.UserBalance < balancePending || !fixed && s.balanceBelowEligibilityThreshold(snapshot.UserBalance) {
				return ErrInsufficientBalance
			}
		} else {
			if group.HasDailyLimit() && exceeds(snapshot.SubscriptionDailyUsage, snapshot.SubscriptionUSD, subscriptionPending, *group.DailyLimitUSD) {
				return ErrDailyLimitExceeded
			}
			if group.HasWeeklyLimit() && exceeds(snapshot.SubscriptionWeeklyUsage, snapshot.SubscriptionUSD, subscriptionPending, *group.WeeklyLimitUSD) {
				return ErrWeeklyLimitExceeded
			}
			if group.HasMonthlyLimit() && exceeds(snapshot.SubscriptionMonthlyUsage, snapshot.SubscriptionUSD, subscriptionPending, *group.MonthlyLimitUSD) {
				return ErrMonthlyLimitExceeded
			}
		}
		if key != nil && exceeds(snapshot.KeyQuotaUsed, snapshot.KeyQuotaUSD, keyPending, key.Quota) {
			return ErrAPIKeyQuotaExhausted
		}
	}
	if key != nil {
		if exceeds(snapshot.KeyEffective5hUsage, snapshot.KeyRateUSD, keyPending, key.RateLimit5h) {
			return ErrAPIKeyRateLimit5hExceeded
		}
		if exceeds(snapshot.KeyEffective1dUsage, snapshot.KeyRateUSD, keyPending, key.RateLimit1d) {
			return ErrAPIKeyRateLimit1dExceeded
		}
		if exceeds(snapshot.KeyEffective7dUsage, snapshot.KeyRateUSD, keyPending, key.RateLimit7d) {
			return ErrAPIKeyRateLimit7dExceeded
		}
	}
	return nil
}
