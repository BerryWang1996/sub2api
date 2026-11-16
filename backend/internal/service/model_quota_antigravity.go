package service

import (
	"context"
	"errors"
	"time"
)

const modelQuotaAntigravityWait = 3 * time.Second

var errModelQuotaAntigravityUnavailable = errors.New("antigravity quota unavailable")

type modelQuotaAntigravityRead struct {
	usage *UsageInfo
	err   error
}

// SetModelQuotaAccountUsage shares the admin quota cache with the API-key catalog.
func (s *GatewayService) SetModelQuotaAccountUsage(usage *AccountUsageService) {
	s.modelQuotaAccountUsage = usage
}

// readModelQuotaAntigravityUsage reads quota for a candidate the gateway already authorized.
func (s *AccountUsageService) readModelQuotaAntigravityUsage(ctx context.Context, account *Account) (*UsageInfo, error) {
	if s == nil || s.cache == nil || s.antigravityQuotaFetcher == nil {
		return nil, errModelQuotaAntigravityUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	usage, err := s.getAntigravityUsage(ctx, account, false)
	if err != nil {
		return nil, err
	}
	if usage == nil || usage.IsForbidden || usage.ErrorCode != "" || usage.Error != "" {
		return nil, errModelQuotaAntigravityUnavailable
	}
	return &UsageInfo{AntigravityQuota: usage.AntigravityQuota, UpdatedAt: usage.UpdatedAt}, nil
}

func (s *GatewayService) readModelQuotaAntigravity(ctx context.Context, accounts []*Account) map[int64]modelQuotaAntigravityRead {
	candidates := make([]*Account, 0, len(accounts))
	results := make(map[int64]modelQuotaAntigravityRead)
	for _, account := range accounts {
		if account == nil || account.Platform != PlatformAntigravity || account.ID <= 0 {
			continue
		}
		if _, exists := results[account.ID]; exists {
			continue
		}
		results[account.ID] = modelQuotaAntigravityRead{err: errModelQuotaAntigravityUnavailable}
		candidates = append(candidates, account)
	}
	if s.modelQuotaAccountUsage == nil || len(candidates) == 0 || ctx.Err() != nil {
		return results
	}

	ctx, cancel := context.WithTimeout(ctx, modelQuotaAntigravityWait)
	defer cancel()
	type result struct {
		id int64
		modelQuotaAntigravityRead
	}
	completed := make(chan result, len(candidates))
	for _, account := range candidates {
		go func() {
			usage, err := s.modelQuotaAccountUsage.readModelQuotaAntigravityUsage(ctx, account)
			completed <- result{account.ID, modelQuotaAntigravityRead{usage, err}}
		}()
	}
	for range candidates {
		select {
		case entry := <-completed:
			results[entry.id] = entry.modelQuotaAntigravityRead
		case <-ctx.Done():
			return results
		}
	}
	return results
}
