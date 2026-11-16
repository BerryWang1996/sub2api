package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestModelQuotaStoredWindowAndRecovery(t *testing.T) {
	zone := modelQuotaDisplayZone
	observed := time.Date(2026, 11, 16, 10, 0, 0, 0, zone)
	reset := observed.Add(3 * time.Hour)
	now := observed.Add(6 * time.Minute)
	account := &Account{ID: 7, Platform: PlatformAnthropic, Type: AccountTypeOAuth, Extra: map[string]any{
		"session_window_utilization":   1.0,
		"passive_usage_5h_sampled_at":  observed.Format(time.RFC3339),
		"passive_usage_5h_reset":       reset.Unix(),
		"passive_usage_7d_utilization": 0.3,
		"passive_usage_sampled_at":     now.Format(time.RFC3339),
		"passive_usage_7d_reset":       observed.Add(7 * 24 * time.Hour).Unix(),
	}}
	constraints := projectAccountModelQuota(account, "claude-opus", now)
	candidates := []modelQuotaCandidate{{account: account, constraints: constraints}}
	summary := aggregateModelQuota(candidates, "", make(modelQuotaPoolIDs), now)
	require.Equal(t, modelQuotaExhausted, summary.Status)
	require.Equal(t, reset.Format(time.RFC3339), summary.RecoversAt.Format(time.RFC3339))
	require.Len(t, summary.Windows, 2)
	require.Equal(t, 100.0, summary.Windows[0].UsedPercent)
	require.Equal(t, observed.Format(time.RFC3339), summary.Windows[0].ObservedAt.Format(time.RFC3339))
	require.Nil(t, summary.Windows[1].ObservedAt, "new account-wide write cannot re-date legacy 7d evidence")

	after := aggregateModelQuota(candidates, "", make(modelQuotaPoolIDs), reset)
	require.Equal(t, modelQuotaAvailable, after.Status)
	require.Nil(t, after.RecoversAt)
	require.Len(t, after.Windows, 1)
	require.Equal(t, "7d", after.Windows[0].Window)
}

func TestModelQuotaOlderSnapshotStillDisplayedAndAliasesSharePool(t *testing.T) {
	zone := modelQuotaDisplayZone
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, zone)
	observed := now.Add(-40 * time.Minute)
	account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
		"codex_5h_used_percent": 30.0,
		"codex_5h_observed_at":  observed.Format(time.RFC3339),
		"codex_5h_reset_at":     now.Add(3 * time.Hour).Format(time.RFC3339),
	}}
	ids := make(modelQuotaPoolIDs)
	candidates := []modelQuotaCandidate{{account: account, constraints: projectAccountModelQuota(account, "gpt-first", now)}}
	a := aggregateModelQuota(candidates, "", ids, now)
	b := aggregateModelQuota([]modelQuotaCandidate{{account: account, constraints: projectAccountModelQuota(account, "gpt-second", now)}}, "", ids, now)
	require.Equal(t, modelQuotaAvailable, a.Status)
	require.Len(t, a.Windows, 1)
	require.Equal(t, 30.0, a.Windows[0].UsedPercent)
	require.Equal(t, observed.Format(time.RFC3339), a.Windows[0].ObservedAt.Format(time.RFC3339))
	require.Equal(t, a.Windows[0].Pool, b.Windows[0].Pool)
}

func TestModelQuotaPicksRepresentativeAcrossAccounts(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, modelQuotaDisplayZone)
	earlier, later := now.Add(time.Hour), now.Add(3*time.Hour)
	percent1, percent2 := 100.0, 30.0
	a := modelQuotaCandidate{constraints: modelQuotaConstraintSet{complete: true, windows: []modelQuotaConstraint{
		{identity: "a", window: "5h", usedPercent: &percent1, exhausted: true, resetsAt: &earlier},
		{identity: "a-week", window: "7d", usedPercent: &percent1, exhausted: true, resetsAt: &later},
	}}}
	b := modelQuotaCandidate{constraints: modelQuotaConstraintSet{complete: true, windows: []modelQuotaConstraint{
		{identity: "b", window: "5h", usedPercent: &percent2, resetsAt: &earlier},
	}}}
	result := aggregateModelQuota([]modelQuotaCandidate{a, b}, "", make(modelQuotaPoolIDs), now)
	require.Equal(t, modelQuotaAvailable, result.Status)
	require.Equal(t, 1, result.AccountsAvailable)
	require.Equal(t, 2, result.AccountsTotal)
	require.Len(t, result.Windows, 1)
	require.Equal(t, 30.0, result.Windows[0].UsedPercent)

	b.constraints.windows[0].exhausted, b.constraints.windows[0].usedPercent = true, &percent1
	result = aggregateModelQuota([]modelQuotaCandidate{a, b}, "", make(modelQuotaPoolIDs), now)
	require.Equal(t, modelQuotaExhausted, result.Status)
	require.Equal(t, earlier, *result.RecoversAt)
	require.Len(t, result.Windows, 1)
}

func TestModelQuotaAntigravityReadsResolvedModel(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, modelQuotaDisplayZone)
	reset := now.Add(2 * time.Hour)
	account := &Account{ID: 5, Platform: PlatformAntigravity}
	read := modelQuotaAntigravityRead{usage: &UsageInfo{UpdatedAt: &now, AntigravityQuota: map[string]*AntigravityModelQuota{
		"gemini-4-argon": {Utilization: 100, ResetTime: reset.Format(time.RFC3339)},
		"gemini-flash":   {Utilization: 20},
	}}}
	result := projectAntigravityModelQuota(account, "models/gemini-4-argon", read)
	summary := aggregateModelQuota([]modelQuotaCandidate{{account: account, constraints: result}}, "", make(modelQuotaPoolIDs), now)
	require.Equal(t, modelQuotaExhausted, summary.Status)
	require.Equal(t, reset, *summary.RecoversAt)
	require.Equal(t, 100.0, summary.Windows[0].UsedPercent)

	failed := projectAntigravityModelQuota(account, "gemini-4-argon", modelQuotaAntigravityRead{err: errModelQuotaAntigravityUnavailable})
	summary = aggregateModelQuota([]modelQuotaCandidate{{account: account, constraints: failed, cooldownUntil: &reset}}, "", make(modelQuotaPoolIDs), now)
	require.Equal(t, modelQuotaUnknown, summary.Status)
	require.Nil(t, summary.RecoversAt)
	require.Contains(t, summary.Reasons, "antigravity_quota_read_failed")
}
