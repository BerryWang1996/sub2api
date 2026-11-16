package service

import (
	"context"
	"math"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
)

type modelQuotaAdmission struct {
	reasons       []string
	missing       []string
	recovery      time.Time
	recoveryKnown bool
}

func (a *modelQuotaAdmission) block(reason string, reset *time.Time, now time.Time) {
	a.reasons = append(a.reasons, reason)
	if reset == nil || !reset.After(now) {
		a.recoveryKnown = false
	} else if reset.After(a.recovery) {
		a.recovery = *reset
	}
}

func (a *modelQuotaAdmission) result(now time.Time, eligibleUntil *time.Time, unevaluated, notApplicable []string) ModelQuotaKeyAdmission {
	status := modelQuotaNotBlocked
	var recovery *ModelQuotaRecovery
	switch {
	case len(a.reasons) > 0:
		status = modelQuotaBlocked
		if a.recoveryKnown && len(a.missing) == 0 {
			if eligibleUntil != nil && !eligibleUntil.After(a.recovery) {
				a.reasons = append(a.reasons, "eligibility_ends_before_recovery")
			} else {
				recovery = newModelQuotaRecovery(now, a.recovery)
			}
		}
	case len(a.missing) > 0:
		status = modelQuotaUnknown
	}
	return ModelQuotaKeyAdmission{
		Status:        status,
		Reasons:       uniqueModelQuotaStrings(a.reasons),
		Unevaluated:   uniqueModelQuotaStrings(append(unevaluated, a.missing...)),
		NotApplicable: notApplicable,
		Recovery:      recovery,
	}
}

type modelQuotaBillingWindow struct {
	reason string
	limit  float64
	usage  float64
	reset  *time.Time
}

// ReadModelQuotaBilling observes the shared admission gates and the separate
// platform gates. It never calls an admission method: those methods may
// increment RPM, fill Redis, or reset a usage window.
func (s *BillingCacheService) ReadModelQuotaBilling(ctx context.Context, key *APIKey, sub *UserSubscription, platforms []string, now time.Time) (ModelQuotaKeyAdmission, map[string]modelQuotaConstraintSet) {
	result := modelQuotaAdmission{recoveryKnown: true}
	byPlatform := make(map[string]modelQuotaConstraintSet)
	unevaluated := []string{"request_cost", "concurrency"}
	if s == nil || key == nil || key.Group == nil || key.UserID <= 0 || key.ID <= 0 {
		result.missing = append(result.missing, "billing_evidence_unavailable")
		for _, platform := range platforms {
			byPlatform[platform] = missingModelQuotaPlatform("billing_evidence_unavailable")
		}
		return result.result(now, nil, unevaluated, nil), byPlatform
	}

	simple := s.cfg != nil && s.cfg.RunMode == config.RunModeSimple
	end := quotaEligibilityEnd(key, sub)
	var notApplicable []string
	if simple {
		notApplicable = []string{"user_balance", "subscription_windows", "api_key_total", "user_platform", "rpm"}
		if s.cfg.SimpleModeKeyRateLimitEnabled && key.HasRateLimits() {
			s.readModelQuotaKeyWindows(ctx, &result, key, true, now)
			s.readModelQuotaHeld(ctx, &result, key, nil, true, now)
		} else {
			notApplicable = append(notApplicable, "api_key_windows")
		}
		return result.result(now, end, unevaluated, notApplicable), byPlatform
	}

	unevaluated = append(unevaluated, "rpm")
	if s.circuitBreaker != nil {
		unevaluated = append(unevaluated, "billing_circuit_breaker")
	}
	if !validModelQuotaNumber(key.Quota) || !validModelQuotaNumber(key.QuotaUsed) {
		result.missing = append(result.missing, "api_key_total_evidence_unavailable")
	}
	if key.Status == StatusAPIKeyQuotaExhausted || key.IsQuotaExhausted() {
		result.block("api_key_quota_exhausted", nil, now)
	}
	if key.HasRateLimits() {
		s.readModelQuotaKeyWindows(ctx, &result, key, false, now)
	} else {
		notApplicable = append(notApplicable, "api_key_windows")
	}

	if key.Group.IsSubscriptionType() {
		notApplicable = append(notApplicable, "user_balance", "user_platform")
		if sub == nil || sub.UserID != key.UserID || sub.GroupID != key.Group.ID || sub.ID <= 0 {
			result.missing = append(result.missing, "subscription_evidence_unavailable")
		} else {
			s.readModelQuotaSubscription(ctx, &result, key, sub, now)
			s.readModelQuotaHeld(ctx, &result, key, sub, false, now)
		}
	} else {
		notApplicable = append(notApplicable, "subscription_windows")
		if s.InflightReservationEnabled() {
			unevaluated = append(unevaluated, "inflight_request_reservations")
		}
		s.readModelQuotaBalance(ctx, &result, key, now)
		s.readModelQuotaHeld(ctx, &result, key, nil, false, now)
		for _, platform := range platforms {
			if _, seen := byPlatform[platform]; platform != "" && !seen {
				byPlatform[platform] = s.readModelQuotaPlatform(ctx, key.UserID, platform, now)
			}
		}
	}
	return result.result(now, end, unevaluated, notApplicable), byPlatform
}

func (s *BillingCacheService) readModelQuotaBalance(ctx context.Context, result *modelQuotaAdmission, key *APIKey, now time.Time) {
	if key.User == nil || key.User.ID != key.UserID || !finiteModelQuotaBalance(key.User.Balance) {
		result.missing = append(result.missing, "user_balance_auth_evidence_unavailable")
	} else if key.User.Balance <= 0 {
		result.block("user_balance_insufficient", nil, now)
	}

	var balance float64
	var available bool
	if s.cache != nil {
		value, err := s.cache.GetUserBalance(ctx, key.UserID)
		balance, available = value, err == nil
	}
	if !available && s.userRepo != nil {
		user, err := s.userRepo.GetByID(ctx, key.UserID)
		if err == nil && user != nil && user.ID == key.UserID {
			balance, available = user.Balance, true
		}
	}
	if !available || !finiteModelQuotaBalance(balance) {
		result.missing = append(result.missing, "user_balance_billing_evidence_unavailable")
	} else if s.balanceBelowEligibilityThreshold(balance) {
		result.block("user_balance_insufficient", nil, now)
	}
}

func finiteModelQuotaBalance(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func (s *BillingCacheService) readModelQuotaKeyWindows(ctx context.Context, result *modelQuotaAdmission, key *APIKey, simple bool, now time.Time) {
	var data *APIKeyRateLimitData
	if !simple && s.cache != nil {
		entry, err := s.cache.GetAPIKeyRateLimit(ctx, key.ID)
		if err == nil && entry != nil {
			data = &APIKeyRateLimitData{
				Usage5h:       entry.Usage5h,
				Usage1d:       entry.Usage1d,
				Usage7d:       entry.Usage7d,
				Window5hStart: modelQuotaUnixStart(entry.Window5h),
				Window1dStart: modelQuotaUnixStart(entry.Window1d),
				Window7dStart: modelQuotaUnixStart(entry.Window7d),
			}
		}
	}
	if data == nil && s.apiKeyRateLimitLoader != nil {
		var err error
		data, err = s.apiKeyRateLimitLoader.GetRateLimitData(ctx, key.ID)
		if err != nil {
			data = nil
		}
	}
	if data == nil {
		result.missing = append(result.missing, "api_key_windows_evidence_unavailable")
		return
	}
	for _, window := range []struct {
		reason   string
		limit    float64
		usage    float64
		start    *time.Time
		duration time.Duration
	}{
		{"api_key_5h_quota_exhausted", key.RateLimit5h, data.Usage5h, data.Window5hStart, RateLimitWindow5h},
		{"api_key_1d_quota_exhausted", key.RateLimit1d, data.Usage1d, data.Window1dStart, RateLimitWindow1d},
		{"api_key_7d_quota_exhausted", key.RateLimit7d, data.Usage7d, data.Window7dStart, RateLimitWindow7d},
	} {
		if window.start == nil || !now.Before(window.start.Add(window.duration)) {
			continue
		}
		if window.start.After(now) && window.limit > 0 {
			result.missing = append(result.missing, "api_key_windows_evidence_unavailable")
			continue
		}
		reset := window.start.Add(window.duration)
		result.check(modelQuotaBillingWindow{window.reason, window.limit, window.usage, &reset}, 0, now)
	}
}

func modelQuotaUnixStart(value int64) *time.Time {
	if value <= 0 {
		return nil
	}
	start := time.Unix(value, 0)
	return &start
}

func (a *modelQuotaAdmission) check(window modelQuotaBillingWindow, held float64, now time.Time) {
	switch {
	case window.limit == 0:
		return
	case !validModelQuotaNumber(window.limit) || !validModelQuotaNumber(window.usage) ||
		!validModelQuotaNumber(held) || !validModelQuotaNumber(window.usage+held):
		a.missing = append(a.missing, "invalid_billing_window_measurement")
	case window.usage+held >= window.limit:
		if held >= window.limit {
			window.reset = nil
		}
		a.block(window.reason, window.reset, now)
	}
}

func modelQuotaSubscriptionWindows(group *Group, sub *UserSubscription) []modelQuotaBillingWindow {
	return []modelQuotaBillingWindow{
		{"subscription_daily_quota_exhausted", modelQuotaOptionalLimit(group.DailyLimitUSD), sub.DailyUsageUSD, sub.DailyResetTime()},
		{"subscription_weekly_quota_exhausted", modelQuotaOptionalLimit(group.WeeklyLimitUSD), sub.WeeklyUsageUSD, sub.WeeklyResetTime()},
		{"subscription_monthly_quota_exhausted", modelQuotaOptionalLimit(group.MonthlyLimitUSD), sub.MonthlyUsageUSD, sub.MonthlyResetTime()},
	}
}

func modelQuotaOptionalLimit(limit *float64) float64 {
	if limit == nil {
		return 0
	}
	return *limit
}

func (s *BillingCacheService) readModelQuotaSubscription(ctx context.Context, result *modelQuotaAdmission, key *APIKey, sub *UserSubscription, now time.Time) {
	if sub.Status != SubscriptionStatusActive || !sub.ExpiresAt.After(now) {
		result.missing = append(result.missing, "subscription_evidence_unavailable")
		return
	}
	windows := modelQuotaSubscriptionWindows(key.Group, sub)
	if !sub.IsWindowActivated() || sub.canAutomaticallyResetDailyAt(now) ||
		sub.canAutomaticallyResetWeeklyAt(now) || sub.canAutomaticallyResetMonthlyAt(now) {
		// Ordinary middleware performs DB maintenance and reloads before checking
		// the billing cache. A raw pre-maintenance read cannot establish the result.
		result.missing = append(result.missing, "subscription_window_maintenance_pending")
		return
	}
	for _, window := range windows {
		if !validModelQuotaNumber(window.limit) || !validModelQuotaNumber(window.usage) {
			result.missing = append(result.missing, "invalid_billing_window_measurement")
		} else if window.limit > 0 && window.usage > window.limit {
			result.block(window.reason, window.reset, now)
		}
	}

	if s.cache != nil {
		entry, err := s.cache.GetSubscriptionCache(ctx, key.UserID, key.Group.ID)
		if err == nil && entry != nil {
			if entry.Status != SubscriptionStatusActive || !entry.ExpiresAt.Equal(sub.ExpiresAt) {
				result.missing = append(result.missing, "subscription_cache_evidence_disagrees")
				return
			}
			windows[0].usage, windows[1].usage, windows[2].usage = entry.DailyUsage, entry.WeeklyUsage, entry.MonthlyUsage
			if entry.Version != sub.UpdatedAt.Unix() {
				// The cache has no window anchors. Keep its actual gate and do not
				// use a reset timestamp taken from a different subscription version.
				for i := range windows {
					windows[i].reset = nil
				}
			}
		}
	}
	for _, window := range windows {
		result.check(window, 0, now)
	}
}

func (s *BillingCacheService) readModelQuotaHeld(ctx context.Context, result *modelQuotaAdmission, key *APIKey, sub *UserSubscription, simple bool, now time.Time) {
	if s.backgroundReservations == nil {
		return
	}
	var subID int64
	if sub != nil {
		subID = sub.ID
	}
	snapshot, err := s.backgroundReservations.Reserved(ctx, key.UserID, subID, key.ID)
	if err != nil || snapshot.UserID != key.UserID || snapshot.SubscriptionID != subID || snapshot.APIKeyID != key.ID {
		result.missing = append(result.missing, "background_reservation_evidence_unavailable")
		return
	}
	if !simple && sub != nil {
		for _, window := range []modelQuotaBillingWindow{
			{"subscription_daily_quota_exhausted", modelQuotaOptionalLimit(key.Group.DailyLimitUSD), snapshot.SubscriptionDailyUsage, snapshot.SubscriptionDailyResetAt},
			{"subscription_weekly_quota_exhausted", modelQuotaOptionalLimit(key.Group.WeeklyLimitUSD), snapshot.SubscriptionWeeklyUsage, snapshot.SubscriptionWeeklyResetAt},
			{"subscription_monthly_quota_exhausted", modelQuotaOptionalLimit(key.Group.MonthlyLimitUSD), snapshot.SubscriptionMonthlyUsage, snapshot.SubscriptionMonthlyResetAt},
		} {
			result.check(window, snapshot.SubscriptionUSD, now)
		}
	}
	if !simple {
		if sub == nil {
			// The durable reserve already deducted holds from this balance.
			if !finiteModelQuotaBalance(snapshot.UserBalance) {
				result.missing = append(result.missing, "background_reservation_evidence_unavailable")
			} else if s.balanceBelowEligibilityThreshold(snapshot.UserBalance) {
				result.block("user_balance_insufficient", nil, now)
			}
		}
		result.check(modelQuotaBillingWindow{"api_key_quota_exhausted", key.Quota, snapshot.KeyQuotaUsed, nil}, snapshot.KeyQuotaUSD, now)
	}
	for _, window := range []modelQuotaBillingWindow{
		{"api_key_5h_quota_exhausted", key.RateLimit5h, snapshot.KeyEffective5hUsage, snapshot.Key5hResetAt},
		{"api_key_1d_quota_exhausted", key.RateLimit1d, snapshot.KeyEffective1dUsage, snapshot.Key1dResetAt},
		{"api_key_7d_quota_exhausted", key.RateLimit7d, snapshot.KeyEffective7dUsage, snapshot.Key7dResetAt},
	} {
		result.check(window, snapshot.KeyRateUSD, now)
	}
}

func missingModelQuotaPlatform(reason string) modelQuotaConstraintSet {
	return modelQuotaConstraintSet{reasons: []string{reason}}
}

func (s *BillingCacheService) readModelQuotaPlatform(ctx context.Context, userID int64, platform string, now time.Time) modelQuotaConstraintSet {
	if s.userPlatformQuotaRepo == nil {
		return modelQuotaConstraintSet{complete: true}
	}
	record, err := s.userPlatformQuotaRepo.GetByUserPlatform(ctx, userID, platform)
	if err != nil || record != nil && (record.UserID != userID || record.Platform != platform) {
		return missingModelQuotaPlatform("user_platform_quota_evidence_unavailable")
	}
	view := UserPlatformQuotaRecord{UserID: userID, Platform: platform}
	if record != nil {
		view = *record
	}
	if s.cache != nil {
		entry, hit, err := s.cache.GetUserPlatformQuotaCache(ctx, userID, platform)
		switch {
		case err != nil:
			return missingModelQuotaPlatform("user_platform_quota_cache_unavailable")
		case hit && entry != nil && entry.SchemaVersion == UserPlatformQuotaCacheSchemaV1:
			if !equalModelQuotaValue(entry.DailyLimitUSD, view.DailyLimitUSD) ||
				!equalModelQuotaValue(entry.WeeklyLimitUSD, view.WeeklyLimitUSD) ||
				!equalModelQuotaValue(entry.MonthlyLimitUSD, view.MonthlyLimitUSD) {
				return missingModelQuotaPlatform("user_platform_quota_limits_disagree")
			}
			view.DailyUsageUSD, view.WeeklyUsageUSD, view.MonthlyUsageUSD = entry.DailyUsageUSD, entry.WeeklyUsageUSD, entry.MonthlyUsageUSD
			view.DailyWindowStart, view.WeeklyWindowStart, view.MonthlyWindowStart = entry.DailyWindowStart, entry.WeeklyWindowStart, entry.MonthlyWindowStart
		}
	}
	result := modelQuotaConstraintSet{complete: true}
	if !view.HasAnyLimit() {
		return result
	}
	var contribution AIRoutingBackgroundPlatformReserved
	if s.backgroundReservations != nil {
		reader, ok := s.backgroundReservations.(backgroundPlatformReader)
		if !ok {
			return missingModelQuotaPlatform("user_platform_reservation_evidence_unavailable")
		}
		contribution, err = readBackgroundPlatform(ctx, reader, view, now)
		if err != nil {
			return missingModelQuotaPlatform("user_platform_reservation_evidence_unavailable")
		}
	}
	dailyReset, weeklyReset := nextDailyReset(now), nextWeeklyReset(now)
	var monthlyReset *time.Time
	monthlyExpired := monthlyQuotaWindowExpired(view.MonthlyWindowStart, now)
	if !contribution.MonthlyWindowIndependent && !monthlyExpired {
		t := nextMonthlyResetFrom(view.MonthlyWindowStart, now)
		monthlyReset = &t
	}
	for _, window := range []struct {
		name     string
		limit    *float64
		ordinary float64
		held     float64
		start    *time.Time
		reset    *time.Time
		expired  bool
		split    bool
	}{
		{"daily", view.DailyLimitUSD, view.DailyUsageUSD, contribution.DailyUSD, view.DailyWindowStart, &dailyReset, quotaWindowExpired(view.DailyWindowStart, timezone.StartOfDay(now)), false},
		{"weekly", view.WeeklyLimitUSD, view.WeeklyUsageUSD, contribution.WeeklyUSD, view.WeeklyWindowStart, &weeklyReset, quotaWindowExpired(view.WeeklyWindowStart, timezone.StartOfWeek(now)), false},
		{"monthly", view.MonthlyLimitUSD, view.MonthlyUsageUSD, contribution.MonthlyUSD, view.MonthlyWindowStart, monthlyReset, monthlyExpired, contribution.MonthlyWindowIndependent},
	} {
		if window.limit == nil {
			continue
		}
		if window.expired {
			window.ordinary = 0
		}
		used := window.ordinary + window.held
		if !validModelQuotaNumber(*window.limit) || !validModelQuotaNumber(window.ordinary) ||
			!validModelQuotaNumber(used) || window.start != nil && window.start.After(now) {
			result.complete = false
			result.reasons = append(result.reasons, "invalid_user_platform_quota_measurement")
			continue
		}
		constraint := modelQuotaConstraint{
			identity:   modelQuotaAccountPoolIdentity(userID, "user_platform:"+platform, window.name),
			window:     "user_platform_" + window.name,
			observedAt: &now, resetsAt: window.reset, exhausted: used >= *window.limit,
		}
		if *window.limit > 0 {
			percent := used / *window.limit * 100
			if validModelQuotaNumber(percent) {
				constraint.usedPercent = &percent
			}
		}
		switch {
		case *window.limit == 0:
			constraint.resetsAt = nil
			constraint.reason = "user_platform_quota_zero_limit"
		case constraint.exhausted && contribution.PendingUSD >= *window.limit:
			constraint.resetsAt = nil
			constraint.reason = "user_platform_reservation_blocks_after_reset"
		case window.split:
			constraint.reason = "user_platform_monthly_windows_independent"
		case constraint.exhausted:
			constraint.reason = "user_platform_" + window.name + "_quota_exhausted"
		}
		result.windows = append(result.windows, constraint)
	}
	return result
}
