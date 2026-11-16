package service

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
)

type modelQuotaPoolIDs map[string]string

func (ids modelQuotaPoolIDs) id(identity string) string {
	if id, ok := ids[identity]; ok {
		return id
	}
	id := "p" + strconv.Itoa(len(ids)+1)
	ids[identity] = id
	return id
}

func aggregateModelQuota(candidates []modelQuotaCandidate, unobserved string, ids modelQuotaPoolIDs, now time.Time) ModelQuotaModel {
	model := ModelQuotaModel{Status: modelQuotaBlocked, AccountsTotal: len(candidates), Windows: []ModelQuotaWindow{}}
	var selected *modelQuotaCandidate
	var selectedRecovery *time.Time
	selectedScore := -1.0
	if unobserved != "" {
		model.Status = modelQuotaUnknown
		model.Reasons = append(model.Reasons, unobserved)
	}
	for i := range candidates {
		candidate := &candidates[i]
		status, recovery, reasons := modelQuotaCandidateStatus(*candidate, now)
		model.Reasons = append(model.Reasons, reasons...)
		if status == modelQuotaAvailable {
			model.AccountsAvailable++
		}
		score := modelQuotaAvailableHeadroom(candidate.constraints.windows, now)
		if modelQuotaStatusPriority(status) > modelQuotaStatusPriority(model.Status) ||
			status == model.Status && (selected == nil ||
				status == modelQuotaAvailable && score > selectedScore ||
				status != modelQuotaAvailable && recovery != nil && (selectedRecovery == nil || recovery.Before(*selectedRecovery))) {
			model.Status, selected, selectedRecovery, selectedScore = status, candidate, recovery, score
		}
	}
	if selected != nil {
		model.RecoversAt = modelQuotaDisplayTimePtr(selectedRecovery)
		seen := make(map[string]bool)
		for _, window := range selected.constraints.windows {
			if window.usedPercent == nil || !modelQuotaWindowCurrent(window, now) || seen[window.identity] {
				continue
			}
			seen[window.identity] = true
			model.Windows = append(model.Windows, ModelQuotaWindow{
				Window: window.window, UsedPercent: *window.usedPercent, Pool: ids.id(window.identity),
				ResetsAt: modelQuotaDisplayTimePtr(window.resetsAt), ObservedAt: modelQuotaDisplayTimePtr(window.observedAt),
			})
		}
	}
	model.Reasons = uniqueModelQuotaStrings(model.Reasons)
	return model
}

func modelQuotaStatusPriority(status string) int {
	switch status {
	case modelQuotaAvailable:
		return 3
	case modelQuotaUnknown:
		return 2
	case modelQuotaExhausted:
		return 1
	default:
		return 0
	}
}

func modelQuotaCandidateStatus(candidate modelQuotaCandidate, now time.Time) (string, *time.Time, []string) {
	status := modelQuotaAvailable
	recovery := candidate.cooldownUntil
	reasons := append([]string(nil), candidate.constraints.reasons...)
	exhausted := recovery != nil
	unknownReset, observed := false, false
	if exhausted {
		reasons = append(reasons, "account_rate_limited")
	}
	for _, window := range candidate.constraints.windows {
		if !modelQuotaWindowCurrent(window, now) {
			continue
		}
		observed = observed || window.usedPercent != nil
		if window.reason != "" {
			reasons = append(reasons, window.reason)
		}
		if window.exhausted {
			exhausted = true
			unknownReset = unknownReset || window.resetsAt == nil
			recovery = latestModelQuotaReset(recovery, window.resetsAt, now)
		}
	}
	if !observed {
		reasons = append(reasons, "quota_not_observed")
	}
	if candidate.blockedReason != "" {
		status = modelQuotaBlocked
		reasons = append(reasons, candidate.blockedReason)
		unknownReset = unknownReset || candidate.blockedUntil == nil
		recovery = latestModelQuotaReset(recovery, candidate.blockedUntil, now)
	}
	if exhausted {
		status = modelQuotaExhausted
	}
	if !candidate.constraints.complete {
		status = modelQuotaUnknown
	}
	if unknownReset || !candidate.constraints.complete {
		recovery = nil
	}
	return status, recovery, reasons
}

func modelQuotaWindowCurrent(window modelQuotaConstraint, now time.Time) bool {
	return window.resetsAt == nil || window.resetsAt.After(now)
}

func modelQuotaAvailableHeadroom(windows []modelQuotaConstraint, now time.Time) float64 {
	remaining := -1.0
	for _, window := range windows {
		if window.usedPercent == nil || !modelQuotaWindowCurrent(window, now) {
			continue
		}
		value := math.Max(0, 100-*window.usedPercent)
		if remaining < 0 || value < remaining {
			remaining = value
		}
	}
	return remaining
}

func latestModelQuotaReset(current, candidate *time.Time, now time.Time) *time.Time {
	if candidate != nil && candidate.After(now) && (current == nil || candidate.After(*current)) {
		return candidate
	}
	return current
}

func uniqueModelQuotaStrings(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func quotaEligibilityEnd(key *APIKey, sub *UserSubscription) *time.Time {
	var end *time.Time
	if key != nil {
		end = key.ExpiresAt
	}
	if sub != nil && !sub.ExpiresAt.IsZero() && (end == nil || sub.ExpiresAt.Before(*end)) {
		end = &sub.ExpiresAt
	}
	return end
}

func validModelQuotaNumber(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

func equalModelQuotaValue(a, b *float64) bool {
	return a == b || a != nil && b != nil && *a == *b
}

func projectAccountModelQuota(account *Account, upstreamModel string, now time.Time) modelQuotaConstraintSet {
	result := projectLocalModelQuota(account, now)
	var provider modelQuotaConstraintSet
	switch {
	case account.Platform == PlatformAnthropic:
		windows := []modelQuotaLegacyWindow{
			{"5h", "session_window_utilization", "passive_usage_5h_sampled_at", "passive_usage_5h_reset"},
			{"7d", "passive_usage_7d_utilization", "passive_usage_7d_sampled_at", "passive_usage_7d_reset"},
		}
		if isAnthropicFableModel(upstreamModel) {
			windows = append(windows, modelQuotaLegacyWindow{"7d_oi", "passive_usage_7d_oi_utilization", "passive_usage_7d_oi_sampled_at", "passive_usage_7d_oi_reset"})
		}
		provider = projectLegacyPercentModelQuota(account, "anthropic", windows, "passive_usage_sampled_at", 100)
	case account.IsOpenAIOAuthLike() || account.IsShadow():
		provider = projectLegacyPercentModelQuota(account, "codex", []modelQuotaLegacyWindow{
			{"5h", "codex_5h_used_percent", "codex_5h_observed_at", "codex_5h_reset_at"},
			{"7d", "codex_7d_used_percent", "codex_7d_observed_at", "codex_7d_reset_at"},
		}, "codex_usage_updated_at", 1)
	case account.Platform == PlatformGrok:
		provider = projectGrokModelQuota(account, upstreamModel)
	case account.Platform == PlatformKimi || account.Platform == PlatformZhipu ||
		account.Platform == PlatformMiniMax || account.Platform == PlatformOpenCodeGo ||
		account.Platform == PlatformCommandCode || account.Platform == PlatformCline:
		provider = projectLegacyCNModelQuota(account)
	default:
		return result
	}
	result.merge(provider)
	return result
}

func projectLocalModelQuota(account *Account, now time.Time) modelQuotaConstraintSet {
	result := modelQuotaConstraintSet{complete: true}
	if !account.SupportsAccountQuota() {
		return result
	}
	for _, window := range []struct {
		name   string
		prefix string
	}{
		{"total", "quota_"},
		{"daily", "quota_daily_"},
		{"weekly", "quota_weekly_"},
	} {
		limit, validLimit := readLocalModelQuotaNumber(account.Extra, window.prefix+"limit")
		if validLimit && limit == 0 {
			continue
		}
		used, validUsed := readLocalModelQuotaNumber(account.Extra, window.prefix+"used")
		resetAt, expired, validWindow := localModelQuotaWindow(account, window.name, window.prefix, now)
		if !validLimit || !validWindow || !validUsed && !expired {
			result.complete = false
			result.reasons = append(result.reasons, "invalid_local_quota")
			continue
		}
		if expired {
			used = 0
		}
		percent := used / limit * 100
		if !validModelQuotaNumber(percent) {
			result.complete = false
			result.reasons = append(result.reasons, "invalid_local_quota")
			continue
		}
		result.windows = append(result.windows, modelQuotaConstraint{
			identity: modelQuotaAccountPoolIdentity(account.ID, "local_account", window.name),
			window:   "account_" + window.name, usedPercent: &percent, resetsAt: resetAt, observedAt: &now, exhausted: used >= limit,
		})
	}
	return result
}

func readLocalModelQuotaNumber(extra map[string]any, key string) (float64, bool) {
	raw := extra[key]
	if raw == nil {
		// Local account quotas use zero as the unconfigured/unused baseline.
		return 0, true
	}
	value, ok := cnParseF64(raw)
	return value, ok && validModelQuotaNumber(value)
}

func localModelQuotaWindow(account *Account, window, prefix string, now time.Time) (*time.Time, bool, bool) {
	if window == "total" {
		return nil, false, true
	}
	start := account.getExtraTime(prefix + "start")
	if (start.IsZero() && account.Extra[prefix+"start"] != nil) || start.After(now) {
		return nil, false, false
	}
	location, err := time.LoadLocation(account.GetQuotaResetTimezone())
	if err != nil {
		location = time.UTC
	}
	if window == "daily" && account.GetQuotaDailyResetMode() == "fixed" {
		hour := account.GetQuotaDailyResetHour()
		reset := nextFixedDailyReset(hour, location, now)
		return &reset, start.Before(lastFixedDailyReset(hour, location, now)), true
	}
	if window == "weekly" && account.GetQuotaWeeklyResetMode() == "fixed" {
		day, hour := account.GetQuotaWeeklyResetDay(), account.GetQuotaWeeklyResetHour()
		reset := nextFixedWeeklyReset(day, hour, location, now)
		return &reset, start.Before(lastFixedWeeklyReset(day, hour, location, now)), true
	}
	if start.IsZero() {
		return nil, true, true
	}
	days := 1
	if window == "weekly" {
		days = 7
	}
	reset := start.Add(time.Duration(days) * 24 * time.Hour)
	if !reset.After(now) {
		// Rolling windows do not start until the next increment.
		return nil, true, true
	}
	return &reset, false, true
}

func modelQuotaAccountPoolIdentity(accountID int64, family, window string) string {
	return fmt.Sprintf("%s:%d:%s", family, accountID, window)
}

type modelQuotaLegacyWindow struct {
	name          string
	valueKey      string
	observedAtKey string
	resetKey      string
}

func projectLegacyPercentModelQuota(account *Account, family string, windows []modelQuotaLegacyWindow, legacyObservedAt string, multiplier float64) modelQuotaConstraintSet {
	result := modelQuotaConstraintSet{complete: true}
	legacyTime := readModelQuotaTime(account.Extra[legacyObservedAt])
	for _, window := range windows {
		if window.observedAtKey != "" && account.Extra[window.observedAtKey] != nil {
			legacyTime = nil
			break
		}
	}
	// The Fable-only window can have fresh evidence even when omitted for this model.
	if family == "anthropic" && account.Extra["passive_usage_7d_oi_sampled_at"] != nil {
		legacyTime = nil
	}
	for _, window := range windows {
		value, ok := cnParseF64(account.Extra[window.valueKey])
		used := value * multiplier
		if !ok || !validModelQuotaNumber(used) {
			continue
		}
		observedAt := readModelQuotaTime(account.Extra[window.observedAtKey])
		if observedAt == nil {
			observedAt = legacyTime
		}
		reset := readModelQuotaTime(account.Extra[window.resetKey])
		if reset == nil && family == "anthropic" && window.name == "5h" {
			reset = account.SessionWindowEnd
		}
		result.windows = append(result.windows, modelQuotaConstraint{
			identity: modelQuotaAccountPoolIdentity(account.ID, family, window.name),
			window:   window.name, usedPercent: &used, resetsAt: reset, observedAt: observedAt, exhausted: used >= 100,
		})
	}
	return result
}

func readModelQuotaTime(raw any) *time.Time {
	if parsed := parseExtraTime(raw); !parsed.IsZero() {
		return &parsed
	}
	if value, ok := cnParseF64(raw); ok && validModelQuotaNumber(value) && value > 0 && value < 1e14 {
		ts := int64(value)
		if ts > 1e11 {
			t := time.UnixMilli(ts)
			return &t
		}
		t := time.Unix(ts, 0)
		return &t
	}
	return nil
}

func projectLegacyCNModelQuota(account *Account) modelQuotaConstraintSet {
	return projectLegacyPercentModelQuota(account, account.Platform, []modelQuotaLegacyWindow{
		{"5h", cnExtraKey(account.Platform, cnExtraSuffix5hUsed), "", cnExtraKey(account.Platform, cnExtraSuffix5hReset)},
		{"weekly", cnExtraKey(account.Platform, cnExtraSuffixWeeklyUsed), "", cnExtraKey(account.Platform, cnExtraSuffixWeeklyReset)},
		{"monthly", cnExtraKey(account.Platform, cnExtraSuffixMonthlyUsed), "", cnExtraKey(account.Platform, cnExtraSuffixMonthlyReset)},
	}, cnExtraKey(account.Platform, cnExtraSuffixUsageUpdated), 1)
}

func projectGrokModelQuota(account *Account, upstreamModel string) modelQuotaConstraintSet {
	snapshot, err := grokQuotaSnapshotFromExtra(account.Extra)
	if err != nil {
		return modelQuotaConstraintSet{reasons: []string{"grok_quota_read_failed"}}
	}
	result := modelQuotaConstraintSet{complete: true}
	if snapshot == nil || upstreamModel == "" || snapshot.Model != xai.ResolveGrokTextResponsesModelID(upstreamModel) {
		return result
	}
	observedAt := readModelQuotaTime(snapshot.LastHeadersSeenAt)
	for _, evidence := range []struct {
		unit   string
		window *xai.QuotaWindow
	}{
		{"requests", snapshot.Requests},
		{"tokens", snapshot.Tokens},
	} {
		window := evidence.window
		if window == nil || window.Limit == nil || window.Remaining == nil ||
			*window.Limit <= 0 || *window.Remaining < 0 || *window.Limit < *window.Remaining {
			continue
		}
		used := float64(*window.Limit-*window.Remaining) / float64(*window.Limit) * 100
		reset := readModelQuotaTime(window.ResetAt)
		if window.ResetUnix != nil && *window.ResetUnix > 0 {
			reset = timePtrIfSet(time.Unix(*window.ResetUnix, 0))
		}
		result.windows = append(result.windows, modelQuotaConstraint{
			identity: modelQuotaAccountPoolIdentity(account.ID, "grok:"+snapshot.Model, evidence.unit),
			window:   evidence.unit, usedPercent: &used, resetsAt: reset, observedAt: observedAt, exhausted: used >= 100,
		})
	}
	return result
}

func projectAntigravityModelQuota(account *Account, upstreamModel string, read modelQuotaAntigravityRead) modelQuotaConstraintSet {
	if read.err != nil || read.usage == nil {
		return modelQuotaConstraintSet{reasons: []string{"antigravity_quota_read_failed"}}
	}
	result := modelQuotaConstraintSet{complete: true}
	model := normalizeAntigravityModelName(upstreamModel)
	quota := read.usage.AntigravityQuota[model]
	if quota == nil || quota.Utilization < 0 {
		return result
	}
	used := float64(quota.Utilization)
	result.windows = append(result.windows, modelQuotaConstraint{
		identity: modelQuotaAccountPoolIdentity(account.ID, "antigravity:"+model, "model"),
		window:   "model", usedPercent: &used, resetsAt: readModelQuotaTime(quota.ResetTime), observedAt: read.usage.UpdatedAt, exhausted: used >= 100,
	})
	return result
}
