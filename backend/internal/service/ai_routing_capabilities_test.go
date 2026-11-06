//go:build unit

package service

import (
	"context"
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func aiCapabilityBool(value bool) *bool { return &value }

func aiCapabilityRequirements() AIRoutingExecutionRequirements {
	return AIRoutingExecutionRequirements{InputModalities: []string{"text"}, InputBytes: 800, MaxOutputTokens: 200, ContextWindowTokens: 1000}
}

func aiCapabilityAccount() *Account {
	return &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
}

func aiCapabilityMetadata(account *Account, window, output int64, modalities ...string) {
	account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{
		Source: "upstream", SyncedAt: "2026-11-06T00:00:00Z",
		Models: map[string]UpstreamModelMetadata{"gpt-5.1": {
			ID: "gpt-5.1", ContextWindow: window, MaxOutputTokens: output, InputModalities: modalities,
		}},
	})
}

func TestAIRoutingCapabilitiesDeclarationsAndMetadata(t *testing.T) {
	tests := []struct {
		name   string
		change func(*AIRoutingExecutionRequirements, *Account)
		reason string
	}{
		{name: "administrator window accepts text at combined boundary"},
		{name: "combined window exceeds by one", change: func(r *AIRoutingExecutionRequirements, _ *Account) { r.InputBytes++ }, reason: "execution_context_window_exceeded"},
		{name: "unknown window", change: func(r *AIRoutingExecutionRequirements, _ *Account) { r.ContextWindowTokens = 0 }, reason: "execution_context_window_unknown"},
		{name: "snapshot supplies unknown window", change: func(r *AIRoutingExecutionRequirements, a *Account) {
			r.ContextWindowTokens = 0
			aiCapabilityMetadata(a, 1000, 300, "text")
		}},
		{name: "snapshot cannot enlarge administrator window", change: func(r *AIRoutingExecutionRequirements, a *Account) {
			r.InputBytes++
			aiCapabilityMetadata(a, 2000, 300, "text")
		}, reason: "execution_context_window_exceeded"},
		{name: "administrator cannot enlarge snapshot window", change: func(r *AIRoutingExecutionRequirements, a *Account) {
			r.ContextWindowTokens = 2000
			aiCapabilityMetadata(a, 999, 300, "text")
		}, reason: "execution_context_window_exceeded"},
		{name: "output limit is independent", change: func(_ *AIRoutingExecutionRequirements, a *Account) { aiCapabilityMetadata(a, 1000, 199, "text") }, reason: "execution_output_limit_exceeded"},
		{name: "candidate output limit tightens", change: func(r *AIRoutingExecutionRequirements, a *Account) {
			r.CandidateMaxOutputTokens = 199
			aiCapabilityMetadata(a, 1000, 500, "text")
		}, reason: "execution_output_limit_exceeded"},
		{name: "candidate input limit tightens", change: func(r *AIRoutingExecutionRequirements, _ *Account) { r.MaxInputTokens = 799 }, reason: "execution_input_limit_exceeded"},
		{name: "missing output reserve", change: func(r *AIRoutingExecutionRequirements, _ *Account) { r.MaxOutputTokens = 0 }, reason: "execution_capacity_unknown"},
		{name: "overflow cannot bypass combined window", change: func(r *AIRoutingExecutionRequirements, _ *Account) {
			r.ContextWindowTokens = math.MaxInt64
			r.InputBytes = math.MaxInt64
			r.MaxOutputTokens = math.MaxInt64
		}, reason: "execution_context_window_exceeded"},
		{name: "undeclared tools unknown", change: func(r *AIRoutingExecutionRequirements, _ *Account) { r.UsesTools = true }, reason: "execution_tools_unknown"},
		{name: "Codex search does not prove generic tools", change: func(r *AIRoutingExecutionRequirements, a *Account) {
			r.UsesTools = true
			aiCapabilityMetadata(a, 1000, 300, "text")
			snapshot := a.GetUpstreamModelMetadataSnapshot()
			metadata := snapshot.Models["gpt-5.1"]
			metadata.CodexToolCapabilities = map[string]json.RawMessage{"supports_search_tool": json.RawMessage("true")}
			snapshot.Models["gpt-5.1"] = metadata
			a.SetUpstreamModelMetadataSnapshot(*snapshot)
		}, reason: "execution_tools_unknown"},
		{name: "administrator declares tools", change: func(r *AIRoutingExecutionRequirements, _ *Account) {
			r.UsesTools = true
			r.SupportsTools = aiCapabilityBool(true)
		}},
		{name: "disable tools overrides declaration", change: func(r *AIRoutingExecutionRequirements, _ *Account) {
			r.UsesTools = true
			r.SupportsTools = aiCapabilityBool(true)
			r.DisableTools = true
		}, reason: "execution_tools_unsupported"},
		{name: "explicit unsupported tools", change: func(r *AIRoutingExecutionRequirements, _ *Account) {
			r.UsesTools = true
			r.SupportsTools = aiCapabilityBool(false)
		}, reason: "execution_tools_unsupported"},
		{name: "undeclared image unknown", change: func(r *AIRoutingExecutionRequirements, _ *Account) { r.InputModalities = []string{"text", "image"} }, reason: "execution_images_unknown"},
		{name: "administrator declares image", change: func(r *AIRoutingExecutionRequirements, _ *Account) {
			r.InputModalities = []string{"text", "image"}
			r.SupportsImages = aiCapabilityBool(true)
		}},
		{name: "snapshot declares image", change: func(r *AIRoutingExecutionRequirements, a *Account) {
			r.InputModalities = []string{"text", "image"}
			aiCapabilityMetadata(a, 1000, 300, "text", "image")
		}},
		{name: "administrator cannot override text only snapshot", change: func(r *AIRoutingExecutionRequirements, a *Account) {
			r.InputModalities = []string{"text", "image"}
			r.SupportsImages = aiCapabilityBool(true)
			aiCapabilityMetadata(a, 1000, 300, "text")
		}, reason: "execution_modality_unsupported"},
		{name: "administrator false overrides image snapshot", change: func(r *AIRoutingExecutionRequirements, a *Account) {
			r.InputModalities = []string{"text", "image"}
			r.SupportsImages = aiCapabilityBool(false)
			aiCapabilityMetadata(a, 1000, 300, "text", "image")
		}, reason: "execution_images_unsupported"},
		{name: "disable images overrides declaration", change: func(r *AIRoutingExecutionRequirements, _ *Account) {
			r.InputModalities = []string{"image"}
			r.SupportsImages = aiCapabilityBool(true)
			r.DisableImages = true
		}, reason: "execution_images_unsupported"},
		{name: "document not covered by image declaration", change: func(r *AIRoutingExecutionRequirements, _ *Account) {
			r.InputModalities = []string{"text", "document"}
			r.SupportsImages = aiCapabilityBool(true)
		}, reason: "execution_modality_unknown"},
		{name: "unknown modality cannot be silently omitted", change: func(r *AIRoutingExecutionRequirements, _ *Account) { r.InputModalities = nil }, reason: "execution_modalities_unknown"},
		{name: "model remapping rejected", change: func(_ *AIRoutingExecutionRequirements, a *Account) {
			a.Credentials = map[string]any{"model_mapping": map[string]any{"gpt-5.1": "another-model"}}
		}, reason: "execution_model_unavailable"},
		{name: "invalid metadata time cannot supply capacity", change: func(r *AIRoutingExecutionRequirements, a *Account) {
			r.ContextWindowTokens = 0
			aiCapabilityMetadata(a, 1000, 300, "text")
			snapshot := a.GetUpstreamModelMetadataSnapshot()
			snapshot.SyncedAt = "unknown"
			a.SetUpstreamModelMetadataSnapshot(*snapshot)
		}, reason: "execution_context_window_unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, account := aiCapabilityRequirements(), aiCapabilityAccount()
			if tt.change != nil {
				tt.change(&r, account)
			}
			result := CheckAIRoutingExecutionAccount(WithAIRoutingExecutionRequirements(context.Background(), r), account, "gpt-5.1")
			require.Equal(t, tt.reason, result.Reason)
			require.Equal(t, tt.reason == "", result.Allowed)
			require.False(t, result.CapacityVerified, "no final converted payload has been measured")
			require.Equal(t, "request_bytes_plus_output", result.CapacityMethod)
			require.Contains(t, result.Warnings, "protocol_overhead_unverified")
		})
	}
}

func TestAIRoutingCapabilitiesSnapshotEvidenceAndImmutableRequirements(t *testing.T) {
	r, account := aiCapabilityRequirements(), aiCapabilityAccount()
	r.UsesTools, r.SupportsTools = true, aiCapabilityBool(true)
	ctx := WithAIRoutingExecutionRequirements(context.Background(), r)
	r.InputModalities[0] = "document"
	*r.SupportsTools = false
	copy, ok := AIRoutingExecutionRequirementsFromContext(ctx)
	require.True(t, ok)
	copy.InputModalities[0] = "audio"
	*copy.SupportsTools = false
	aiCapabilityMetadata(account, 1500, 300, "text")
	result := CheckAIRoutingExecutionAccount(ctx, account, "gpt-5.1")
	require.True(t, result.Allowed)
	require.Equal(t, "upstream", result.MetadataSource)
	require.Equal(t, "2026-11-06T00:00:00Z", result.MetadataSyncedAt)
	require.EqualValues(t, 1000, result.ContextWindowTokens)
	require.Contains(t, result.Warnings, "tools_administrator_declared")
	require.True(t, CheckAIRoutingExecutionAccount(context.Background(), nil, "").Allowed, "legacy request must retain existing gates")
}

func TestAIRoutingCapabilitiesRequestModalitiesAreNotInterchangeable(t *testing.T) {
	candidate := AIRoutingCandidate{ContextWindowTokens: 2000, SupportsImages: aiCapabilityBool(true)}
	for _, tt := range []struct {
		name        string
		modalities  []string
		wantAllowed bool
	}{
		{name: "explicit image", modalities: []string{"text", "image"}, wantAllowed: true},
		{name: "explicit document", modalities: []string{"text", "document"}},
		{name: "legacy ambiguous attachment"},
		{name: "inconsistent text only attachment", modalities: []string{"text"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := AIRoutingRequest{InputBytes: 500, MaxOutput: 200, HasImages: true, InputModalities: tt.modalities}
			requirements := AIRoutingRequirementsForCandidate(r, candidate)
			result := CheckAIRoutingExecutionAccount(WithAIRoutingExecutionRequirements(context.Background(), requirements), aiCapabilityAccount(), "gpt-5.1")
			require.Equal(t, tt.wantAllowed, result.Allowed)
		})
	}
}

func TestAIRoutingCapabilitiesSurviveSchedulingAndRetries(t *testing.T) {
	for _, engine := range []string{"gateway-priority", "gateway-load", "openai-legacy-priority", "openai-legacy-load", "openai-advanced"} {
		t.Run(engine, func(t *testing.T) {
			repo, groups := scopeIsolationFixtures()
			target := int64(20)
			decision := scopeIsolationDecision(&target, nil)
			decision.Source, decision.AICandidateID = CompositeRouteSourceAI, "long-context"
			repo.accountsByID[1].GroupIDs = []int64{target}
			repo.accountsByID[1].AccountGroups = []AccountGroup{{GroupID: target}}
			aiCapabilityMetadata(repo.accountsByID[1], 999, 300, "text")
			aiCapabilityMetadata(repo.accountsByID[2], 1500, 300, "text")
			ctx := WithAIRoutingExecutionRequirements(WithCompositeRouteDecision(context.Background(), decision), aiCapabilityRequirements())
			cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{CompositeRouteStickyKey(ctx, "long-context"): 1}}
			selectAccount := scopeIsolationScheduler(t, engine, repo, groups, cache)
			result, err := selectAccount(ctx, "long-context", "", nil)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.EqualValues(t, 2, result.Account.ID)
			releaseLegacySchedulerDecisionSelection(result)
			pinned := decision
			accountID := int64(1)
			pinned.TargetGroupID, pinned.TargetAccountID = nil, &accountID
			pinnedCtx := WithCompositeRouteDecision(ctx, pinned)
			result, err = selectAccount(pinnedCtx, "", "", nil)
			releaseLegacySchedulerDecisionSelection(result)
			require.Error(t, err, "a fixed incapable account cannot fall through to another member")
			require.Nil(t, result)
			result, err = selectAccount(ctx, "long-context", "", map[int64]struct{}{2: {}})
			releaseLegacySchedulerDecisionSelection(result)
			require.Error(t, err, "retry cannot escape to a smaller member of the same group")
			require.Nil(t, result)
			aiCapabilityMetadata(repo.accountsByID[2], 900, 300, "text")
			result, err = selectAccount(ctx, "", "", nil)
			releaseLegacySchedulerDecisionSelection(result)
			require.Error(t, err, "fresh metadata changes must invalidate an old admission")
			require.Nil(t, result)
			decision.Source, decision.AICandidateID = CompositeRouteSourceExplicit, ""
			legacyCtx := WithCompositeRouteDecision(ctx, decision)
			require.True(t, CompositeRouteAccountAllowed(legacyCtx, repo.accountsByID[1]), "static Composite ignores AI-only capability state")
		})
	}
}

func TestAIRoutingCapabilitiesEligibilityOnlyChangesBoundedHistory(t *testing.T) {
	repo, groups := scopeIsolationFixtures()
	source := groups.groups[10]
	source.AllowMessagesDispatch = true
	target := int64(20)
	candidate := AIRoutingCandidate{ID: "long", Platform: PlatformOpenAI, Model: "gpt-5.1", Enabled: true, TargetGroupID: &target}
	policy := &AIRoutingPolicy{GroupID: source.ID, ContextMode: "bounded_history", Candidates: []AIRoutingCandidate{candidate}}
	key := &APIKey{GroupID: &source.ID, Group: source}
	gateway := &GatewayService{accountRepo: repo, groupRepo: groups, billingService: &BillingService{fallbackPrices: map[string]*ModelPricing{"gpt-5.1": {}}}}
	r := AIRoutingRequest{InputBytes: 800, MaxOutput: 200}
	eligible := func() []AIRoutingCandidate {
		return gateway.EligibleAIRoutingCandidates(context.Background(), key, policy, r, CompositeRouteEndpointMessages, 0)
	}
	require.Empty(t, eligible(), "strict mode must not treat an unknown window as unlimited")
	policy.Candidates[0].ContextWindowTokens = 1000
	require.Len(t, eligible(), 1)
	aiCapabilityMetadata(repo.accountsByID[2], 999, 300, "text")
	require.Empty(t, eligible())
	for _, mode := range []string{"metadata", "task_excerpt"} {
		policy.ContextMode = mode
		require.Len(t, eligible(), 1, "published legacy policies retain their prior capacity behavior")
	}
}

func TestAIRoutingCapabilitiesCountTokensNeedsNoGenerationReserve(t *testing.T) {
	repo, groups := scopeIsolationFixtures()
	source := groups.groups[10]
	source.AllowMessagesDispatch = true
	target := int64(20)
	policy := &AIRoutingPolicy{GroupID: source.ID, ContextMode: "bounded_history", Candidates: []AIRoutingCandidate{{
		ID: "count", Platform: PlatformOpenAI, Model: "gpt-5.1", Enabled: true, TargetGroupID: &target,
		ContextWindowTokens: 10, MaxInputTokens: 5, MaxOutputTokens: 2,
	}}}
	key := &APIKey{GroupID: &source.ID, Group: source}
	gateway := &GatewayService{accountRepo: repo, groupRepo: groups, billingService: &BillingService{fallbackPrices: map[string]*ModelPricing{"gpt-5.1": {}}}}
	r, err := NewAIRoutingRequest([]byte(`{"model":"auto","messages":[{"role":"user","content":"Count this complete input even though it is larger than the generation window."}]}`), "bounded_history")
	require.NoError(t, err)
	require.Zero(t, r.MaxOutput)
	eligible := gateway.EligibleAIRoutingCandidates(context.Background(), key, policy, r, CompositeRouteEndpointCountTokens, 0)
	require.Len(t, eligible, 1, "single-target counting is not a generation request")
	chosen, _, sourceName, err := ChooseAIRoutingCandidate(context.Background(), policy, r, eligible, nil, "")
	require.NoError(t, err)
	require.Equal(t, "count", chosen.ID)
	require.Equal(t, "single", sourceName)
	require.Empty(t, gateway.EligibleAIRoutingCandidates(context.Background(), key, policy, r, CompositeRouteEndpointMessages, 0))
}
