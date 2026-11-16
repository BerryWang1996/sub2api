//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestModelQuotaBillingSimpleUsesDBAndDurableHold(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	key := testModelQuotaBillingKey()
	key.Quota, key.QuotaUsed = 1, 1
	key.Status = StatusAPIKeyQuotaExhausted
	key.RateLimit5h = 10
	start, reset := now.Add(-time.Hour), now.Add(4*time.Hour)
	db := &modelQuotaTestRateLoader{data: APIKeyRateLimitData{Usage5h: 4, Window5hStart: &start}}
	ledger := &modelQuotaTestLedger{
		reserved: AIRoutingBackgroundReserved{
			UserID: key.UserID, APIKeyID: key.ID, KeyEffective5hUsage: 4,
			KeyQuotaUSD: 6, KeyRateUSD: 6, Key5hResetAt: &reset,
		},
	}
	service := &BillingCacheService{
		cfg:                    &config.Config{RunMode: config.RunModeSimple},
		cache:                  &modelQuotaTestReadCache{},
		apiKeyRateLimitLoader:  db,
		backgroundReservations: ledger,
	}

	admission, routes := service.ReadModelQuotaBilling(context.Background(), key, nil, []string{PlatformOpenAI}, now)
	require.Equal(t, modelQuotaNotBlocked, admission.Status)
	require.Contains(t, admission.NotApplicable, "api_key_total")
	require.Contains(t, admission.NotApplicable, "api_key_windows")
	require.Empty(t, routes)
	require.Zero(t, db.reads)
	require.Zero(t, ledger.reads)

	service.cfg.SimpleModeKeyRateLimitEnabled = true
	admission, _ = service.ReadModelQuotaBilling(context.Background(), key, nil, nil, now)
	require.Equal(t, modelQuotaBlocked, admission.Status)
	require.Equal(t, []string{"api_key_5h_quota_exhausted"}, admission.Reasons)
	require.Equal(t, reset, admission.Recovery.At.UTC())
	require.Equal(t, 1, db.reads)
	require.Equal(t, 1, ledger.reads)

	ledger.reserved.KeyRateUSD = 10
	admission, _ = service.ReadModelQuotaBilling(context.Background(), key, nil, nil, now)
	require.Equal(t, modelQuotaBlocked, admission.Status)
	require.Nil(t, admission.Recovery)
}

func TestModelQuotaBillingFullReadsRedisAndDoesNotDeductBalanceHoldAgain(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	key := testModelQuotaBillingKey()
	key.User.Balance = 3
	key.Quota = 50
	key.RateLimit5h = 10
	ledgerReset := now.Add(90 * time.Minute)
	ledger := &modelQuotaTestLedger{reserved: AIRoutingBackgroundReserved{
		UserID: key.UserID, APIKeyID: key.ID, UserBalance: 3, KeyQuotaUSD: 4, KeyRateUSD: 4,
		KeyEffective5hUsage: 6, Key5hResetAt: &ledgerReset,
	}}
	db := &modelQuotaTestRateLoader{}
	cache := &modelQuotaTestReadCache{balance: 3, keyRate: &APIKeyRateLimitCacheData{
		Usage5h: 10, Window5h: now.Add(-time.Hour).Unix(),
	}}
	cfg := &config.Config{}
	cfg.Billing.MinimumBalanceReserve = 2
	service := &BillingCacheService{cfg: cfg, cache: cache, apiKeyRateLimitLoader: db, backgroundReservations: ledger}

	admission, _ := service.ReadModelQuotaBilling(context.Background(), key, nil, nil, now)
	require.Equal(t, modelQuotaBlocked, admission.Status)
	require.Equal(t, []string{"api_key_5h_quota_exhausted"}, admission.Reasons)
	require.Equal(t, now.Add(4*time.Hour), admission.Recovery.At.UTC())
	require.Zero(t, db.reads)

	key.ExpiresAt = &admission.Recovery.At
	admission, _ = service.ReadModelQuotaBilling(context.Background(), key, nil, nil, now)
	require.Nil(t, admission.Recovery)
	require.Contains(t, admission.Reasons, "eligibility_ends_before_recovery")

	cache.keyRate.Usage5h = 0
	ledger.reserved.KeyEffective5hUsage = 0
	admission, _ = service.ReadModelQuotaBilling(context.Background(), key, nil, nil, now)
	require.Equal(t, modelQuotaNotBlocked, admission.Status)
	require.Empty(t, admission.Reasons)
}

func TestModelQuotaBillingFullSubscriptionDefersMaintenance(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	key := testModelQuotaBillingKey()
	key.Group.SubscriptionType = SubscriptionTypeSubscription
	dailyLimit := 8.0
	key.Group.DailyLimitUSD = &dailyLimit
	yesterday := now.Add(-24 * time.Hour)
	sub := &UserSubscription{
		ID: 77, UserID: key.UserID, GroupID: key.Group.ID, Status: SubscriptionStatusActive,
		StartsAt: now.AddDate(0, -1, 0), ExpiresAt: now.AddDate(0, 0, 10),
		DailyWindowStart: &yesterday, DailyUsageUSD: 9, UpdatedAt: now.Add(-time.Minute),
	}
	cache := &modelQuotaTestReadCache{subscription: &SubscriptionCacheData{
		Status: SubscriptionStatusActive, ExpiresAt: sub.ExpiresAt, DailyUsage: 9, Version: sub.UpdatedAt.Unix() - 1,
	}}
	ledger := &modelQuotaTestLedger{reserved: AIRoutingBackgroundReserved{
		UserID: key.UserID, APIKeyID: key.ID, SubscriptionID: sub.ID,
	}}
	service := &BillingCacheService{cache: cache, backgroundReservations: ledger}

	admission, platforms := service.ReadModelQuotaBilling(context.Background(), key, sub, []string{PlatformOpenAI}, now)
	require.Equal(t, modelQuotaUnknown, admission.Status)
	require.Contains(t, admission.Unevaluated, "subscription_window_maintenance_pending")
	require.Empty(t, platforms)
	require.Zero(t, cache.subscriptionReads)
	require.Equal(t, 9.0, sub.DailyUsageUSD)

	sub.DailyWindowStart, sub.DailyUsageUSD = &now, 0
	admission, _ = service.ReadModelQuotaBilling(context.Background(), key, sub, nil, now)
	require.Equal(t, modelQuotaBlocked, admission.Status)
	require.Equal(t, []string{"subscription_daily_quota_exhausted"}, admission.Reasons)
	require.Nil(t, admission.Recovery)
	require.Equal(t, 1, cache.subscriptionReads)
}

func TestModelQuotaBillingPlatformsStayIndependent(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	key := testModelQuotaBillingKey()
	zero, ten := 0.0, 10.0
	repo := &modelQuotaTestPlatformRepo{records: map[string]*UserPlatformQuotaRecord{
		PlatformOpenAI: {
			UserID: key.UserID, Platform: PlatformOpenAI, DailyLimitUSD: &zero,
		},
		PlatformAnthropic: {
			UserID: key.UserID, Platform: PlatformAnthropic, MonthlyLimitUSD: &ten,
			MonthlyUsageUSD: 7, MonthlyWindowStart: &now,
		},
		PlatformZhipu: {
			UserID: key.UserID, Platform: PlatformZhipu, DailyLimitUSD: &ten,
		},
	}}
	cache := &modelQuotaTestReadCache{balance: 50, platforms: map[string]*UserPlatformQuotaCacheEntry{
		PlatformZhipu: {SchemaVersion: UserPlatformQuotaCacheSchemaV1}, // Stale unlimited sentinel.
	}}
	ledger := &modelQuotaTestLedger{
		reserved: AIRoutingBackgroundReserved{UserID: key.UserID, APIKeyID: key.ID, UserBalance: 50},
		platforms: map[string]AIRoutingBackgroundPlatformReserved{
			PlatformAnthropic: {MonthlyUSD: 3, MonthlyWindowIndependent: true},
		},
	}
	service := &BillingCacheService{cache: cache, userPlatformQuotaRepo: repo, backgroundReservations: ledger}

	admission, platforms := service.ReadModelQuotaBilling(context.Background(), key, nil,
		[]string{PlatformOpenAI, PlatformAnthropic, PlatformGemini, PlatformZhipu, PlatformOpenAI}, now)
	require.Equal(t, modelQuotaNotBlocked, admission.Status)
	require.Empty(t, admission.Reasons)
	require.True(t, platforms[PlatformOpenAI].windows[0].exhausted)
	require.Nil(t, platforms[PlatformOpenAI].windows[0].resetsAt)
	require.Equal(t, "user_platform_quota_zero_limit", platforms[PlatformOpenAI].windows[0].reason)
	month := platforms[PlatformAnthropic].windows[0]
	require.True(t, month.exhausted)
	require.Equal(t, 100.0, *month.usedPercent)
	require.Nil(t, month.resetsAt)
	require.True(t, platforms[PlatformGemini].complete)
	require.False(t, platforms[PlatformZhipu].complete)
	require.Contains(t, platforms[PlatformZhipu].reasons, "user_platform_quota_limits_disagree")
	require.Equal(t, 1, repo.reads[PlatformOpenAI])
}

func testModelQuotaBillingKey() *APIKey {
	return &APIKey{
		ID: 8, UserID: 3, Status: StatusAPIKeyActive,
		User:  &User{ID: 3, Balance: 50},
		Group: &Group{ID: 4, Platform: PlatformComposite, SubscriptionType: SubscriptionTypeStandard},
	}
}

// Embedding a nil write-capable port makes any accidental cache write panic.
type modelQuotaTestReadCache struct {
	BillingCache
	balance           float64
	keyRate           *APIKeyRateLimitCacheData
	subscription      *SubscriptionCacheData
	subscriptionReads int
	platforms         map[string]*UserPlatformQuotaCacheEntry
}

func (c *modelQuotaTestReadCache) GetUserBalance(context.Context, int64) (float64, error) {
	return c.balance, nil
}

func (c *modelQuotaTestReadCache) GetAPIKeyRateLimit(context.Context, int64) (*APIKeyRateLimitCacheData, error) {
	return c.keyRate, nil
}

func (c *modelQuotaTestReadCache) GetSubscriptionCache(context.Context, int64, int64) (*SubscriptionCacheData, error) {
	c.subscriptionReads++
	return c.subscription, nil
}

func (c *modelQuotaTestReadCache) GetUserPlatformQuotaCache(_ context.Context, _ int64, platform string) (*UserPlatformQuotaCacheEntry, bool, error) {
	entry, ok := c.platforms[platform]
	return entry, ok, nil
}

type modelQuotaTestRateLoader struct {
	data  APIKeyRateLimitData
	reads int
}

func (r *modelQuotaTestRateLoader) GetRateLimitData(context.Context, int64) (*APIKeyRateLimitData, error) {
	r.reads++
	return &r.data, nil
}

func (r *modelQuotaTestRateLoader) ResetRateLimitWindows(context.Context, int64) error {
	panic("model quota read must not reset windows")
}

type modelQuotaTestLedger struct {
	reserved  AIRoutingBackgroundReserved
	platforms map[string]AIRoutingBackgroundPlatformReserved
	reads     int
}

func (l *modelQuotaTestLedger) Reserved(context.Context, int64, int64, int64) (AIRoutingBackgroundReserved, error) {
	l.reads++
	return l.reserved, nil
}

func (l *modelQuotaTestLedger) ReservedPlatform(_ context.Context, _ int64, platform string, _ AIRoutingBackgroundPlatformWindows) (AIRoutingBackgroundPlatformReserved, error) {
	return l.platforms[platform], nil
}

type modelQuotaTestPlatformRepo struct {
	UserPlatformQuotaRepository
	records map[string]*UserPlatformQuotaRecord
	reads   map[string]int
}

func (r *modelQuotaTestPlatformRepo) GetByUserPlatform(_ context.Context, _ int64, platform string) (*UserPlatformQuotaRecord, error) {
	if r.reads == nil {
		r.reads = make(map[string]int)
	}
	r.reads[platform]++
	return r.records[platform], nil
}
