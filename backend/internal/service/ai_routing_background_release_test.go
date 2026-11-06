//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type backgroundReleaseRepo struct {
	AIRoutingBackgroundRepository
	backgroundReservationReaderStub
}

func (r *backgroundReleaseRepo) Reserved(ctx context.Context, userID, subID, keyID int64) (AIRoutingBackgroundReserved, error) {
	return r.backgroundReservationReaderStub.Reserved(ctx, userID, subID, keyID)
}

func TestAIRoutingBackgroundProductionProviderDoesNotStartWorker(t *testing.T) {
	require.False(t, AIRoutingBackgroundReleased())
	repo := &backgroundReleaseRepo{}
	// Complete dependencies prove that release closure, rather than an
	// accidentally unready service, prevents production worker startup.
	gateway := &OpenAIGatewayService{
		accountRepo:         &openAIRecordUsageAccountRepoStub{},
		httpUpstream:        &aiControlHTTP{},
		billingService:      &BillingService{},
		billingCacheService: &BillingCacheService{},
		usageLogRepo:        &openAIRecordUsageLogRepoStub{},
	}
	development := NewAIRoutingResponsesBackgroundService(repo, gateway)
	require.True(t, development.Ready(), "direct construction remains available to isolated development tests")
	require.Nil(t, development.cancel)
	require.Nil(t, development.done)
	production := ProvideAIRoutingResponsesBackgroundService(repo, gateway)
	if production != nil {
		defer production.Stop()
	}
	require.Nil(t, production, "production must not construct or start a background worker")
	require.Nil(t, ProvideAIRoutingResponsesBackgroundService(nil, nil))
}

func TestAIRoutingBackgroundReleaseClosurePreservesOrdinaryHoldProtection(t *testing.T) {
	repo := &backgroundReleaseRepo{backgroundReservationReaderStub: backgroundReservationReaderStub{snapshot: AIRoutingBackgroundReserved{
		UserID: 1, APIKeyID: 2, UserBalance: 100, KeyQuotaUsed: 2, KeyQuotaUSD: 8,
	}}}
	svc := ProvideBillingCacheService(nil, nil, nil, nil, nil, nil, &config.Config{}, nil, repo)
	defer svc.Stop()
	require.False(t, AIRoutingBackgroundReleased())
	require.ErrorIs(t, svc.CheckBillingEligibility(t.Context(), &User{ID: 1}, &APIKey{ID: 2, Quota: 10}, nil, nil, ""), ErrAPIKeyQuotaExhausted)
	require.Equal(t, 1, repo.calls)
}
