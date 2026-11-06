package service

import (
	"context"
	"math"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/google/uuid"
)

type backgroundPlatformReader interface {
	ReservedPlatform(context.Context, int64, string, AIRoutingBackgroundPlatformWindows) (AIRoutingBackgroundPlatformReserved, error)
}

type aiBackgroundPlatformHoldKey struct{}
type aiBackgroundPlatformHold struct {
	userID   int64
	platform string
	amount   float64
}

// ReserveAIRoutingBackgroundPlatformHold retains a short ordinary in-flight
// reservation until SQL owns the durable hold. It reuses the existing platform
// limit/cache/window reader and adds the complete proposed hold to admission.
func (s *BillingCacheService) ReserveAIRoutingBackgroundPlatformHold(ctx context.Context, userID int64, platform string, amount float64) (*InflightReservation, error) {
	if userID <= 0 || platform == "" || amount < 0 || math.IsNaN(amount) || math.IsInf(amount, 0) || s.backgroundReservations == nil || s.userPlatformQuotaRepo == nil {
		return nil, ErrBillingServiceUnavailable
	}
	if amount == 0 {
		return nil, nil
	}
	record, err := s.userPlatformQuotaRepo.GetByUserPlatform(ctx, userID, platform)
	if err != nil {
		return nil, ErrBillingServiceUnavailable.WithCause(err)
	}
	if record == nil || !record.HasAnyLimit() {
		return nil, nil
	}
	// A durable background hold must not inherit ordinary fail-open behavior or
	// its optional feature flag. Any configured platform limit requires Redis.
	sc, ok := s.cache.(InflightScopeReservationCache)
	renewer, renewOK := s.cache.(InflightScopeReservationRenewer)
	if !ok || !renewOK || sc == nil || renewer == nil {
		return nil, ErrBillingServiceUnavailable
	}
	ttl := defaultInflightReservationTTL
	if s.cfg != nil && s.cfg.Billing.InflightReservation.TTLSeconds > 0 {
		ttl = time.Duration(s.cfg.Billing.InflightReservation.TTLSeconds) * time.Second
	}
	scope := "background-platform:" + strconv.FormatInt(userID, 10) + ":" + platform
	res := &InflightReservation{userID: userID, requestID: uuid.NewString(), amount: amount, ttl: ttl, scopeCache: sc, scopes: []string{scope}, strictScopeRenewer: renewer}
	res.refs.Store(1)
	reserveCtx, cancel := context.WithTimeout(ctx, inflightReservationReserveTimeout)
	pending, err := sc.ReserveInflightScope(reserveCtx, scope, res.requestID, amount, ttl)
	cancel()
	if err != nil || math.IsNaN(pending) || math.IsInf(pending, 0) || pending < 0 {
		res.Release()
		return nil, ErrBillingServiceUnavailable
	}
	res.startStrictScopeRenewal()
	// Read ordinary dirty usage without writing or invalidating its cache. Keep
	// limits from the fresh DB row, so an old unlimited sentinel cannot bypass a
	// newly configured limit. Cache read failures cannot hide unflushed usage.
	view := *record
	entry, hit, err := s.cache.GetUserPlatformQuotaCache(ctx, userID, platform)
	if err != nil {
		res.Release()
		return nil, ErrBillingServiceUnavailable.WithCause(err)
	}
	if hit && entry != nil && entry.SchemaVersion == UserPlatformQuotaCacheSchemaV1 && (entry.DailyLimitUSD != nil || entry.WeeklyLimitUSD != nil || entry.MonthlyLimitUSD != nil) {
		view.DailyUsageUSD, view.WeeklyUsageUSD, view.MonthlyUsageUSD = entry.DailyUsageUSD, entry.WeeklyUsageUSD, entry.MonthlyUsageUSD
		view.DailyWindowStart, view.WeeklyWindowStart, view.MonthlyWindowStart = entry.DailyWindowStart, entry.WeeklyWindowStart, entry.MonthlyWindowStart
	}
	now := time.Now()
	if quotaWindowExpired(view.DailyWindowStart, timezone.StartOfDay(now)) {
		view.DailyUsageUSD = 0
	}
	if quotaWindowExpired(view.WeeklyWindowStart, timezone.StartOfWeek(now)) {
		view.WeeklyUsageUSD = 0
	}
	if monthlyQuotaWindowExpired(view.MonthlyWindowStart, now) {
		view.MonthlyUsageUSD = 0
	}
	ctx = context.WithValue(ctx, aiBackgroundPlatformHoldKey{}, aiBackgroundPlatformHold{userID, platform, amount + pending})
	if err := s.checkBackgroundPlatform(ctx, view, now); err != nil {
		res.Release()
		return nil, err
	}
	if err := res.checkStrictScope(ctx); err != nil {
		res.Release()
		return nil, err
	}
	return res, nil
}

func backgroundPlatformWindows(record UserPlatformQuotaRecord, now time.Time) AIRoutingBackgroundPlatformWindows {
	windows := AIRoutingBackgroundPlatformWindows{DailyStart: timezone.StartOfDay(now), WeeklyStart: timezone.StartOfWeek(now)}
	if record.MonthlyWindowStart != nil && !monthlyQuotaWindowExpired(record.MonthlyWindowStart, now) {
		windows.MonthlyStart = *record.MonthlyWindowStart
	}
	return windows
}

func readBackgroundPlatform(ctx context.Context, reader backgroundPlatformReader, record UserPlatformQuotaRecord, now time.Time) (AIRoutingBackgroundPlatformReserved, error) {
	contribution, err := reader.ReservedPlatform(ctx, record.UserID, record.Platform, backgroundPlatformWindows(record, now))
	if err != nil {
		return contribution, ErrBillingServiceUnavailable.WithCause(err)
	}
	for _, value := range []float64{contribution.PendingUSD, contribution.DailyUSD, contribution.WeeklyUSD, contribution.MonthlyUSD} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < contribution.PendingUSD || value < 0 {
			return contribution, ErrBillingServiceUnavailable
		}
	}
	return contribution, nil
}

// Ledger contributions never enter ordinary Redis counters or absolute DB
// snapshots. Cache retries and restarts cannot duplicate a background cost.
func (s *BillingCacheService) checkBackgroundPlatform(ctx context.Context, record UserPlatformQuotaRecord, now time.Time) error {
	if s.backgroundReservations == nil || !record.HasAnyLimit() {
		return nil
	}
	reader, ok := s.backgroundReservations.(backgroundPlatformReader)
	if !ok {
		return ErrBillingServiceUnavailable
	}
	contribution, err := readBackgroundPlatform(ctx, reader, record, now)
	if err != nil {
		return err
	}
	hold, fixed := ctx.Value(aiBackgroundPlatformHoldKey{}).(aiBackgroundPlatformHold)
	fixed = fixed && hold.userID == record.UserID && hold.platform == record.Platform
	exceeded := func(actual, background float64, limit *float64) bool {
		if limit == nil {
			return false
		}
		if fixed {
			return actual+background+hold.amount > *limit
		}
		return actual+background >= *limit
	}
	if exceeded(record.DailyUsageUSD, contribution.DailyUSD, record.DailyLimitUSD) {
		return withWindowResetsMetadata(ErrUserPlatformDailyQuotaExhausted, nextDailyReset(now))
	}
	if exceeded(record.WeeklyUsageUSD, contribution.WeeklyUSD, record.WeeklyLimitUSD) {
		return withWindowResetsMetadata(ErrUserPlatformWeeklyQuotaExhausted, nextWeeklyReset(now))
	}
	if exceeded(record.MonthlyUsageUSD, contribution.MonthlyUSD, record.MonthlyLimitUSD) {
		if contribution.MonthlyWindowIndependent {
			// Two independently expiring portions have no single truthful reset.
			return ErrUserPlatformMonthlyQuotaExhausted
		}
		if contribution.MonthlyWindowStart != nil {
			return withWindowResetsMetadata(ErrUserPlatformMonthlyQuotaExhausted, nextMonthlyResetFrom(contribution.MonthlyWindowStart, now))
		}
		return withWindowResetsMetadata(ErrUserPlatformMonthlyQuotaExhausted, nextMonthlyResetFrom(record.MonthlyWindowStart, now))
	}
	return nil
}

type backgroundPlatformQuotaViews struct {
	UserPlatformQuotaRepository
	reader backgroundPlatformReader
}

// Only list views are decorated; reads used by billing and counter/flusher
// writes retain their unmerged values.
func WithBackgroundPlatformQuotaViews(repository UserPlatformQuotaRepository, reader backgroundPlatformReader) UserPlatformQuotaRepository {
	return &backgroundPlatformQuotaViews{UserPlatformQuotaRepository: repository, reader: reader}
}

func (r *backgroundPlatformQuotaViews) ListByUser(ctx context.Context, userID int64) ([]UserPlatformQuotaRecord, error) {
	rows, err := r.UserPlatformQuotaRepository.ListByUser(ctx, userID)
	if err != nil || r.reader == nil {
		return rows, err
	}
	rows = append([]UserPlatformQuotaRecord(nil), rows...)
	now := time.Now()
	for i := range rows {
		contribution, err := readBackgroundPlatform(ctx, r.reader, rows[i], now)
		if err != nil {
			return nil, err
		}
		rows[i].BackgroundUsage = &contribution
	}
	return rows, nil
}
