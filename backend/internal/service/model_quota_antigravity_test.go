//go:build unit

package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type modelQuotaAntigravityFetcherStub struct {
	calls atomic.Int64
	fetch func(context.Context, *Account) (*QuotaResult, error)
}

func (s *modelQuotaAntigravityFetcherStub) CanFetch(*Account) bool                       { return true }
func (s *modelQuotaAntigravityFetcherStub) GetProxyURL(context.Context, *Account) string { return "" }
func (s *modelQuotaAntigravityFetcherStub) FetchQuota(ctx context.Context, account *Account, _ string) (*QuotaResult, error) {
	s.calls.Add(1)
	return s.fetch(ctx, account)
}

func TestModelQuotaAntigravityUsesSharedCache(t *testing.T) {
	stamp := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	const model = "claude-opus-4-6-thinking"
	quota := &AntigravityModelQuota{Utilization: 100, ResetTime: stamp.Add(2 * time.Hour).Format(time.RFC3339)}
	fetcher := &modelQuotaAntigravityFetcherStub{fetch: func(context.Context, *Account) (*QuotaResult, error) {
		return &QuotaResult{UsageInfo: &UsageInfo{
			UpdatedAt:              &stamp,
			AntigravityQuota:       map[string]*AntigravityModelQuota{model: quota},
			AntigravityQuotaGroups: []*AntigravityQuotaGroup{{Key: "3p"}},
		}}, nil
	}}
	usage := &AccountUsageService{cache: NewUsageCache(), antigravityQuotaFetcher: fetcher}
	gateway := &GatewayService{}
	gateway.SetModelQuotaAccountUsage(usage)
	account := &Account{ID: 11, Platform: PlatformAntigravity}
	other := &Account{ID: 12, Platform: PlatformAnthropic}

	_, err := usage.getAntigravityUsage(t.Context(), account, false)
	require.NoError(t, err)
	result := gateway.readModelQuotaAntigravity(t.Context(), []*Account{account, account, other})
	require.Len(t, result, 1)
	require.NoError(t, result[account.ID].err)
	require.Equal(t, quota, result[account.ID].usage.AntigravityQuota[model])
	require.Equal(t, &stamp, result[account.ID].usage.UpdatedAt)
	require.Empty(t, result[account.ID].usage.AntigravityQuotaGroups, "upstream groups do not identify their individual models")
	require.EqualValues(t, 1, fetcher.calls.Load(), "fresh admin quota should serve the gateway query")
}

func TestModelQuotaAntigravityTimeoutStillFillsCache(t *testing.T) {
	const model = "claude-opus-4-6-thinking"
	started := make(chan struct{}, 2)
	finish := make(chan struct{}, 1)
	fetcher := &modelQuotaAntigravityFetcherStub{fetch: func(ctx context.Context, _ *Account) (*QuotaResult, error) {
		started <- struct{}{}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-finish:
			return &QuotaResult{UsageInfo: &UsageInfo{AntigravityQuota: map[string]*AntigravityModelQuota{
				model: {Utilization: 100},
			}}}, nil
		}
	}}
	usage := &AccountUsageService{cache: NewUsageCache(), antigravityQuotaFetcher: fetcher}
	gateway := &GatewayService{modelQuotaAccountUsage: usage}
	account := &Account{ID: 11, Platform: PlatformAntigravity}
	completed := make(chan map[int64]modelQuotaAntigravityRead, 1)
	go func() {
		completed <- gateway.readModelQuotaAntigravity(t.Context(), []*Account{account, account})
	}()
	defer close(finish)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("quota fetch did not start")
	}
	select {
	case result := <-completed:
		require.Error(t, result[account.ID].err, "an unfinished account must be unknown on return")
		require.Nil(t, result[account.ID].usage)
	case <-time.After(5 * time.Second):
		t.Fatal("catalog waited for the unfinished quota fetch past the three-second budget")
	}
	// Allow the original shared flight to finish and check that a later query reuses it.
	finish <- struct{}{}
	result := gateway.readModelQuotaAntigravity(t.Context(), []*Account{account})
	require.NoError(t, result[account.ID].err)
	require.Equal(t, 100, result[account.ID].usage.AntigravityQuota[model].Utilization)
	cached, ok := usage.cachedAntigravityUsage(account.ID, false)
	require.True(t, ok)
	require.Equal(t, 100, cached.AntigravityQuota[model].Utilization)
	require.EqualValues(t, 1, fetcher.calls.Load(), "query timeout must not start a second probe")
}

func TestModelQuotaAntigravityDoesNotExposeProviderErrors(t *testing.T) {
	fetcher := &modelQuotaAntigravityFetcherStub{fetch: func(context.Context, *Account) (*QuotaResult, error) {
		return nil, errors.New("private quota rejection")
	}}
	usage := &AccountUsageService{cache: NewUsageCache(), antigravityQuotaFetcher: fetcher}
	gateway := &GatewayService{modelQuotaAccountUsage: usage}
	account := &Account{ID: 11, Platform: PlatformAntigravity}
	for range 2 {
		result := gateway.readModelQuotaAntigravity(t.Context(), []*Account{account})
		require.ErrorIs(t, result[account.ID].err, errModelQuotaAntigravityUnavailable)
		require.Nil(t, result[account.ID].usage)
	}
	require.EqualValues(t, 1, fetcher.calls.Load(), "failed refreshes should use the existing negative cache")
}
