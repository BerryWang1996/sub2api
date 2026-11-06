package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/stretchr/testify/require"
)

type backgroundPlatformStub struct {
	backgroundReservationReaderStub
	value   AIRoutingBackgroundPlatformReserved
	windows AIRoutingBackgroundPlatformWindows
	err     error
}

func (s *backgroundPlatformStub) ReservedPlatform(_ context.Context, _ int64, _ string, windows AIRoutingBackgroundPlatformWindows) (AIRoutingBackgroundPlatformReserved, error) {
	s.windows = windows
	return s.value, s.err
}

type backgroundQuotaViewRepo struct {
	UserPlatformQuotaRepository
	row UserPlatformQuotaRecord
}

func (r *backgroundQuotaViewRepo) ListByUser(context.Context, int64) ([]UserPlatformQuotaRecord, error) {
	return []UserPlatformQuotaRecord{r.row}, nil
}
func (r *backgroundQuotaViewRepo) GetByUserPlatform(context.Context, int64, string) (*UserPlatformQuotaRecord, error) {
	copy := r.row
	return &copy, nil
}

func TestBackgroundPlatformHoldUsesOrdinaryUsageAndDoesNotModifyCounters(t *testing.T) {
	now := time.Now()
	limit := 10.0
	row := UserPlatformQuotaRecord{UserID: 1, Platform: PlatformOpenAI, DailyUsageUSD: 2, WeeklyUsageUSD: 2, MonthlyUsageUSD: 2, DailyLimitUSD: &limit, WeeklyLimitUSD: &limit, MonthlyLimitUSD: &limit, DailyWindowStart: &now, WeeklyWindowStart: &now, MonthlyWindowStart: &now}
	repo := &backgroundQuotaViewRepo{row: row}
	reader := &backgroundPlatformStub{value: AIRoutingBackgroundPlatformReserved{PendingUSD: 3, DailyUSD: 6, WeeklyUSD: 6, MonthlyUSD: 6}}
	svc := &BillingCacheService{cfg: &config.Config{}, cache: &backgroundScopeCache{}, backgroundReservations: reader, userPlatformQuotaRepo: repo}
	// Existing ordinary use 2 + durable background 6 + exact new hold 2 fits.
	res, err := svc.ReserveAIRoutingBackgroundPlatformHold(context.Background(), 1, PlatformOpenAI, 2)
	require.NoError(t, err)
	require.NotNil(t, res)
	res.Release()
	_, err = svc.ReserveAIRoutingBackgroundPlatformHold(context.Background(), 1, PlatformOpenAI, 2.1)
	require.ErrorIs(t, err, ErrUserPlatformDailyQuotaExhausted)
	require.Equal(t, row, repo.row, "checking must not merge background costs into ordinary counters")
	reader.err = errors.New("ledger unavailable")
	_, err = svc.ReserveAIRoutingBackgroundPlatformHold(context.Background(), 1, PlatformOpenAI, 1)
	require.ErrorIs(t, err, ErrBillingServiceUnavailable)
}

func TestBackgroundPlatformViewsKeepCounterReadsAndResetAnchorsIndependent(t *testing.T) {
	now := time.Now()
	reset := now.Add(-time.Minute)
	repo := &backgroundQuotaViewRepo{row: UserPlatformQuotaRecord{UserID: 1, Platform: PlatformOpenAI, DailyUsageUSD: 4, DailyWindowStart: &reset, WeeklyWindowStart: &reset, MonthlyWindowStart: &reset}}
	reader := &backgroundPlatformStub{value: AIRoutingBackgroundPlatformReserved{PendingUSD: 2, DailyUSD: 3, WeeklyUSD: 4, MonthlyUSD: 5}}
	wrapper := WithBackgroundPlatformQuotaViews(repo, reader)
	rows, err := wrapper.ListByUser(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, &reader.value, rows[0].BackgroundUsage)
	require.Equal(t, timezone.StartOfDay(now), reader.windows.DailyStart)
	require.Equal(t, timezone.StartOfWeek(now), reader.windows.WeeklyStart)
	require.Equal(t, reset, reader.windows.MonthlyStart)
	unmerged, err := wrapper.GetByUserPlatform(context.Background(), 1, PlatformOpenAI)
	require.NoError(t, err)
	require.Nil(t, unmerged.BackgroundUsage)
	require.EqualValues(t, 4, unmerged.DailyUsageUSD)
	require.Nil(t, repo.row.BackgroundUsage)
}

type backgroundScopeCache struct {
	BillingCache
	failReserve bool
	failRead    bool
	dead        atomic.Bool
	renews      atomic.Int32
	releases    atomic.Int32
}

func (c *backgroundScopeCache) ReserveInflightScope(context.Context, string, string, float64, time.Duration) (float64, error) {
	if c.failReserve {
		return 0, errors.New("synthetic reserve error")
	}
	return 0, nil
}
func (c *backgroundScopeCache) ReleaseInflightScope(context.Context, string, string) error {
	c.releases.Add(1)
	return nil
}
func (c *backgroundScopeCache) RenewInflightScope(context.Context, string, string, time.Duration) (bool, error) {
	c.renews.Add(1)
	return !c.dead.Load(), nil
}
func (c *backgroundScopeCache) GetUserPlatformQuotaCache(context.Context, int64, string) (*UserPlatformQuotaCacheEntry, bool, error) {
	if c.failRead {
		return nil, false, errors.New("synthetic dirty cache read error")
	}
	return nil, false, nil
}

func TestBackgroundPlatformLimitedHoldRequiresLiveRenewableCache(t *testing.T) {
	limit := 10.0
	repo := &backgroundQuotaViewRepo{row: UserPlatformQuotaRecord{UserID: 1, Platform: PlatformOpenAI, MonthlyLimitUSD: &limit}}
	s := &BillingCacheService{backgroundReservations: &backgroundPlatformStub{}, userPlatformQuotaRepo: repo}
	_, err := s.ReserveAIRoutingBackgroundPlatformHold(t.Context(), 1, PlatformOpenAI, 1)
	require.ErrorIs(t, err, ErrBillingServiceUnavailable)
	for _, cache := range []*backgroundScopeCache{{failReserve: true}, {failRead: true}} {
		s.cache = cache
		_, err = s.ReserveAIRoutingBackgroundPlatformHold(t.Context(), 1, PlatformOpenAI, 1)
		require.ErrorIs(t, err, ErrBillingServiceUnavailable)
		require.EqualValues(t, 1, cache.releases.Load())
	}
	cache := &backgroundScopeCache{}
	s.cache = cache
	res, err := s.ReserveAIRoutingBackgroundPlatformHold(t.Context(), 1, PlatformOpenAI, 1)
	require.NoError(t, err)
	defer res.Release()
	cache.dead.Store(true)
	require.ErrorIs(t, res.checkStrictScope(t.Context()), ErrBillingServiceUnavailable)
	cache.dead.Store(false)
	require.ErrorIs(t, res.checkStrictScope(t.Context()), ErrBillingServiceUnavailable, "lost lease must never be revived")
	repo.row.MonthlyLimitUSD = nil
	s.cache = nil
	res, err = s.ReserveAIRoutingBackgroundPlatformHold(t.Context(), 1, PlatformOpenAI, 1)
	require.NoError(t, err)
	require.Nil(t, res, "unlimited platforms do not require Redis")
}

func TestBackgroundPlatformScopeRenewsUntilDurableHandoff(t *testing.T) {
	cache := &backgroundScopeCache{}
	r := &InflightReservation{scopeCache: cache, scopes: []string{"background-platform:1:openai"}, requestID: "fake", ttl: 30 * time.Millisecond, strictScopeRenewer: cache}
	r.refs.Store(1)
	r.startStrictScopeRenewal()
	require.Eventually(t, func() bool { return cache.renews.Load() >= 3 }, time.Second, 5*time.Millisecond)
	cache.dead.Store(true)
	require.Eventually(t, func() bool { return r.strictScopeLost.Load() }, time.Second, 5*time.Millisecond)
	require.ErrorIs(t, r.checkStrictScope(t.Context()), ErrBillingServiceUnavailable)
	r.Release()
	require.EqualValues(t, 1, cache.releases.Load())
}
