package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestModelQuotaCatalogRetainsConcreteAllowedAliases(t *testing.T) {
	group := &Group{ID: 7, Platform: PlatformComposite, ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"gemini-alias", "my-claude-*"}}}
	account := Account{ID: 42, Platform: PlatformAnthropic, Type: AccountTypeOAuth,
		Credentials: map[string]any{"model_mapping": map[string]any{"my-claude-current": "claude-sonnet", "my-claude-*": "claude-*", "unlisted": "claude-sonnet"}}}
	inventory := ModelQuotaCatalogInventory{
		Accounts: []Account{account},
		Routes: []CompositeModelRoute{{ID: 7, GroupID: group.ID, MatchType: CompositeRouteMatchExact, TargetPlatform: PlatformGemini,
			PublicModel: "gemini-alias", Endpoint: CompositeRouteEndpointGemini, Enabled: true}},
	}
	entries, err := modelQuotaCatalog(group, inventory)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.Equal(t, "gemini-alias", entries[0].id)
	require.Equal(t, "my-claude-current", entries[1].id)
	require.JSONEq(t, `"gemini-alias"`, string(entries[0].row["display_name"]))
}

func TestModelQuotaCatalogMatchesOrdinaryOpenAIListingForPassthroughFilter(t *testing.T) {
	groupID := int64(17)
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			account := Account{
				ID: groupID, Platform: PlatformOpenAI, Type: AccountTypeAPIKey,
				Credentials: map[string]any{"model_mapping": map[string]any{"mapped-only": "upstream"}},
				Extra:       map[string]any{"openai_passthrough": true, "openai_passthrough_model_filter": enabled},
			}
			repo := &modelsListAccountRepoStub{byGroup: map[int64][]Account{groupID: []Account{account}}}
			ordinary := (&GatewayService{accountRepo: repo}).GetAvailableModels(context.Background(), &groupID, PlatformOpenAI)
			ordinary = GatewayModelListingSource(PlatformOpenAI, ordinary, GatewayDefaultModelIDs(PlatformOpenAI))

			entries, err := modelQuotaCatalog(&Group{ID: groupID, Platform: PlatformOpenAI}, ModelQuotaCatalogInventory{Accounts: []Account{account}})
			require.NoError(t, err)
			quota := make([]string, 0, len(entries))
			for _, entry := range entries {
				quota = append(quota, entry.id)
			}
			require.Equal(t, ordinary, quota)
			if enabled {
				require.Equal(t, []string{"mapped-only"}, quota)
			} else {
				require.NotContains(t, quota, "mapped-only")
				require.Equal(t, GatewayDefaultModelIDs(PlatformOpenAI), quota)
			}
		})
	}
}

func TestModelQuotaCompositeEndpointPrecedenceAndPinFallback(t *testing.T) {
	pin := int64(101)
	fallback := int64(77)
	group := &Group{ID: 7, Platform: PlatformComposite, ClaudeCodeOnly: true, FallbackGroupID: &fallback}
	inventory := ModelQuotaCatalogInventory{Routes: []CompositeModelRoute{
		{ID: 1, GroupID: group.ID, Enabled: true, MatchType: CompositeRouteMatchPrefix, PublicModel: "opus", TargetPlatform: PlatformAnthropic,
			Endpoint: CompositeRouteEndpointAny},
		{ID: 2, GroupID: group.ID, Enabled: true, MatchType: CompositeRouteMatchExact, PublicModel: "opus-custom", UpstreamModel: "claude-opus",
			TargetPlatform: PlatformAnthropic, TargetAccountID: &pin, Endpoint: CompositeRouteEndpointChatCompletions},
	}}
	chat := planModelQuotaRoute(group, inventory, "opus-custom", CompositeRouteEndpointChatCompletions)
	require.Equal(t, "claude_code_only", chat.reason)
	require.False(t, chat.readFailed)
	require.Nil(t, chat.decision)
	responses := planModelQuotaRoute(group, inventory, "opus-custom", CompositeRouteEndpointResponses)
	require.True(t, responses.readFailed)
	require.Equal(t, "claude_code_only_fallback_not_evaluated", responses.reason)
	require.Nil(t, responses.decision)
	group.ClaudeCodeOnly = false
	chat = planModelQuotaRoute(group, inventory, "opus-custom", CompositeRouteEndpointChatCompletions)
	require.NotNil(t, chat.decision)
	require.Equal(t, pin, *chat.decision.TargetAccountID)
	require.Equal(t, "claude-opus", chat.decision.UpstreamModel)
}

func TestModelQuotaRouteExcludesExpiredAccountAndReportsCooldown(t *testing.T) {
	now := time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)
	reset := now.Add(30 * time.Minute)
	group := &Group{ID: 7, Platform: PlatformAnthropic}
	key := &APIKey{Group: group, GroupID: &group.ID}
	account := &Account{ID: 9, Platform: PlatformAnthropic, Status: StatusActive, Type: AccountTypeOAuth,
		Schedulable: true, AutoPauseOnExpired: true, ExpiresAt: &now, RateLimitResetAt: &reset}
	decision := CompositeRouteDecision{GroupID: group.ID, TargetPlatform: PlatformAnthropic,
		PublicModel: "claude-sonnet", UpstreamModel: "claude-sonnet", Endpoint: CompositeRouteEndpointMessages}
	gateway := &GatewayService{}
	input, accepted := gateway.modelQuotaAccountPath(context.Background(), key, account, decision, nil, nil, nil, now)
	require.False(t, accepted)
	require.Equal(t, "account_expired", input.blockedReason)

	account.ExpiresAt = nil
	input, accepted = gateway.modelQuotaAccountPath(context.Background(), key, account, decision, nil, nil, nil, now)
	require.True(t, accepted)
	model := aggregateModelQuota([]modelQuotaCandidate{input}, "", make(modelQuotaPoolIDs), now)
	require.Equal(t, modelQuotaExhausted, model.Status)
	require.Equal(t, "2026-11-15T16:30:00+08:00", model.RecoversAt.Format(time.RFC3339))
	require.Equal(t, 1, model.AccountsTotal)
	require.Zero(t, model.AccountsAvailable)
	require.Empty(t, model.Windows)

	account.RateLimitResetAt = nil
	later := now.Add(time.Hour)
	account.OverloadUntil, account.TempUnschedulableUntil = &reset, &later
	input, accepted = gateway.modelQuotaAccountPath(context.Background(), key, account, decision, nil, nil, nil, now)
	require.True(t, accepted)
	model = aggregateModelQuota([]modelQuotaCandidate{input}, "", make(modelQuotaPoolIDs), now)
	require.Equal(t, modelQuotaBlocked, model.Status)
	require.Equal(t, "2026-11-15T17:00:00+08:00", model.RecoversAt.Format(time.RFC3339))
}
