package service

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type backgroundReservationReaderStub struct {
	snapshot AIRoutingBackgroundReserved
	err      error
	calls    int
}

func (r *backgroundReservationReaderStub) Reserved(_ context.Context, userID, subscriptionID, keyID int64) (AIRoutingBackgroundReserved, error) {
	r.calls++
	return r.snapshot, r.err
}

func TestBackgroundReservationAdmissionSeesHoldAndSettledUsage(t *testing.T) {
	for _, field := range []string{"subscription_daily", "subscription_weekly", "subscription_monthly", "key_quota", "key_5h", "key_1d", "key_7d"} {
		for _, settled := range []bool{false, true} {
			t.Run(field+map[bool]string{false: "_active", true: "_settled"}[settled], func(t *testing.T) {
				limit := 10.0
				key := &APIKey{ID: 2, Quota: limit, RateLimit5h: limit, RateLimit1d: limit, RateLimit7d: limit}
				group := &Group{SubscriptionType: SubscriptionTypeSubscription, DailyLimitUSD: &limit, WeeklyLimitUSD: &limit, MonthlyLimitUSD: &limit}
				sub := &UserSubscription{ID: 3}
				snapshot := AIRoutingBackgroundReserved{UserID: 1, APIKeyID: 2, SubscriptionID: 3, UserBalance: 100}
				actual, held := 2.0, 8.0
				if settled {
					actual, held = 10, 0
				}
				var expected error
				switch field {
				case "subscription_daily":
					snapshot.SubscriptionDailyUsage, snapshot.SubscriptionUSD = actual, held
					expected = ErrDailyLimitExceeded
				case "subscription_weekly":
					snapshot.SubscriptionWeeklyUsage, snapshot.SubscriptionUSD = actual, held
					expected = ErrWeeklyLimitExceeded
				case "subscription_monthly":
					snapshot.SubscriptionMonthlyUsage, snapshot.SubscriptionUSD = actual, held
					expected = ErrMonthlyLimitExceeded
				case "key_quota":
					snapshot.KeyQuotaUsed, snapshot.KeyQuotaUSD = actual, held
					expected = ErrAPIKeyQuotaExhausted
				case "key_5h":
					snapshot.KeyEffective5hUsage, snapshot.KeyRateUSD = actual, held
					expected = ErrAPIKeyRateLimit5hExceeded
				case "key_1d":
					snapshot.KeyEffective1dUsage, snapshot.KeyRateUSD = actual, held
					expected = ErrAPIKeyRateLimit1dExceeded
				case "key_7d":
					snapshot.KeyEffective7dUsage, snapshot.KeyRateUSD = actual, held
					expected = ErrAPIKeyRateLimit7dExceeded
				}
				svc := &BillingCacheService{cfg: &config.Config{}, backgroundReservations: &backgroundReservationReaderStub{snapshot: snapshot}}
				// Public entry rejects before reading a potentially stale cache.
				require.ErrorIs(t, svc.CheckBillingEligibility(context.Background(), &User{ID: 1}, key, group, sub, ""), expected)
			})
		}
	}
}

func TestBackgroundReservationFixedChargeAndIdentity(t *testing.T) {
	reader := &backgroundReservationReaderStub{snapshot: AIRoutingBackgroundReserved{UserID: 1, APIKeyID: 2, UserBalance: 4, KeyQuotaUsed: 2, KeyQuotaUSD: 4}}
	svc := &BillingCacheService{cfg: &config.Config{}, backgroundReservations: reader}
	user, key := &User{ID: 1}, &APIKey{ID: 2, Quota: 10}
	check := func(amount float64) error {
		return svc.checkBackgroundReservations(context.Background(), user, key, nil, nil, amount, amount, amount, true)
	}
	require.NoError(t, check(4))
	require.ErrorIs(t, check(4.01), ErrInsufficientBalance)
	reader.snapshot.UserBalance = 100
	require.ErrorIs(t, check(4.01), ErrAPIKeyQuotaExhausted)
	reader.snapshot.APIKeyID = 8
	require.ErrorIs(t, check(1), ErrBillingServiceUnavailable)
	reader.snapshot.APIKeyID = 2
	reader.snapshot.KeyQuotaUSD = math.NaN()
	require.ErrorIs(t, check(1), ErrBillingServiceUnavailable)
	reader.err = errors.New("ledger read unavailable")
	require.ErrorIs(t, check(1), ErrBillingServiceUnavailable)
}

func TestBackgroundReservationSimpleModePreservesExplicitKeyWindows(t *testing.T) {
	reader := &backgroundReservationReaderStub{snapshot: AIRoutingBackgroundReserved{UserID: 1, APIKeyID: 2, UserBalance: -3, KeyQuotaUsed: 99, KeyRateUSD: 3, KeyEffective5hUsage: 2}}
	svc := &BillingCacheService{cfg: &config.Config{RunMode: config.RunModeSimple}, backgroundReservations: reader}
	user, key := &User{ID: 1}, &APIKey{ID: 2, Quota: 1, RateLimit5h: 5}
	require.NoError(t, svc.checkBackgroundReservations(context.Background(), user, key, nil, nil, 0, 0, 0, false))
	require.Zero(t, reader.calls)
	svc.cfg.SimpleModeKeyRateLimitEnabled = true
	require.ErrorIs(t, svc.checkBackgroundReservations(context.Background(), user, key, nil, nil, 0, 0, 0, false), ErrAPIKeyRateLimit5hExceeded)
	reader.snapshot.KeyRateUSD = 2
	require.NoError(t, svc.checkBackgroundReservations(context.Background(), user, key, nil, nil, 0, 0, 0, false))
}
