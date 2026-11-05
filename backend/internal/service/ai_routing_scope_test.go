//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Exercise actual selection entrypoints, including Composite scope reconstruction,
// rather than merely asserting the scope helper's individual fields.
func TestAIRoutingScopeSurvivesSchedulersAndAccountModelMapping(t *testing.T) {
	for _, engine := range []string{"gateway-priority", "gateway-load", "openai-legacy-priority", "openai-legacy-load", "openai-advanced"} {
		t.Run(engine, func(t *testing.T) {
			repo, groups := scopeIsolationFixtures()
			targetID := int64(20)
			decision := scopeIsolationDecision(&targetID, nil)
			decision.Source, decision.AICandidateID, decision.BoundAccountID = CompositeRouteSourceAI, "selected", 2
			// A faster account belongs to the same destination, so membership alone
			// cannot enforce a previous tool turn's account identity.
			repo.accountsByID[1].GroupIDs = []int64{targetID}
			repo.accountsByID[1].AccountGroups = []AccountGroup{{GroupID: targetID}}
			ctx := WithCompositeRouteDecision(context.Background(), decision)
			cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{CompositeRouteStickyKey(ctx, "ai-tool-turn"): 1}}
			selectAccount := scopeIsolationScheduler(t, engine, repo, groups, cache)
			result, err := selectAccount(ctx, "ai-tool-turn", "", nil)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, int64(2), result.Account.ID)
			releaseLegacySchedulerDecisionSelection(result)
			// A mapping change after selection may never silently alter the chosen
			// model, or escape the tool binding to the otherwise healthy account 1.
			repo.accountsByID[2].Credentials = map[string]any{"model_mapping": map[string]any{"gpt-5.1": "gpt-5.4"}}
			result, err = selectAccount(ctx, "ai-tool-turn", "", nil)
			releaseLegacySchedulerDecisionSelection(result)
			require.Error(t, err)
			require.Nil(t, result)
			// An unbound first turn may select another account in the chosen group,
			// but only one that executes the exact selected model.
			decision.BoundAccountID = 0
			ctx = WithCompositeRouteDecision(context.Background(), decision)
			result, err = selectAccount(ctx, "", "", nil)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Equal(t, int64(1), result.Account.ID)
			releaseLegacySchedulerDecisionSelection(result)
		})
	}
}

func TestAIRoutingBillingUsesSelectedModelAcrossAsyncBoundary(t *testing.T) {
	for _, source := range []string{BillingModelSourceRequested, BillingModelSourceChannelMapped, BillingModelSourceResponse} {
		t.Run(source, func(t *testing.T) {
			usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
			userRepo := &openAIRecordUsageUserRepoStub{}
			svc := newOpenAIRecordUsageServiceForTest(usageRepo, userRepo, &openAIRecordUsageSubRepoStub{}, nil)
			target := int64(20)
			decision := scopeIsolationDecision(&target, nil)
			decision.Source, decision.AICandidateID = CompositeRouteSourceAI, "chosen"
			scoped := WithCompositeRouteDecision(context.Background(), decision)
			ctx := CopyAIRoutingBillingModel(context.Background(), scoped)
			_, hasGrant := CompositeRouteScopeFromContext(ctx)
			require.False(t, hasGrant, "an accounting task must not inherit the scheduling grant")
			usage := OpenAIUsage{InputTokens: 1200, OutputTokens: 300}
			expected := expectedOpenAICost(t, svc, decision.UpstreamModel, usage, 1.1)
			err := svc.RecordUsage(ctx, &OpenAIRecordUsageInput{
				Result: &OpenAIForwardResult{RequestID: "ai-billing-" + source, Model: decision.PublicModel, BillingModel: "gpt-5.4", UpstreamModel: decision.UpstreamModel, UpstreamResponseModel: "gpt-5.4", Usage: usage, Duration: time.Second},
				APIKey: &APIKey{ID: 10}, User: &User{ID: 20}, Account: &Account{ID: 30},
				ChannelUsageFields: ChannelUsageFields{OriginalModel: "arbitrary-public-alias", ChannelMappedModel: "gpt-5.4", BillingModelSource: source},
			})
			require.NoError(t, err)
			require.NotNil(t, usageRepo.lastLog)
			require.Greater(t, expected.ActualCost, 0.0)
			require.InDelta(t, expected.ActualCost, usageRepo.lastLog.ActualCost, 1e-12)
			require.InDelta(t, expected.ActualCost, userRepo.lastAmount, 1e-12)
		})
	}
}
