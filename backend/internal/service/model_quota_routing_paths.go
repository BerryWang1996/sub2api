package service

import (
	"context"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
)

func planModelQuotaRoute(group *Group, inventory ModelQuotaCatalogInventory, model, endpoint string) modelQuotaPlannedRoute {
	result := modelQuotaPlannedRoute{}
	decision := CompositeRouteDecision{GroupID: group.ID, PublicModel: model, UpstreamModel: model, Endpoint: endpoint, TargetPlatform: group.Platform}
	if group.Platform == PlatformComposite {
		if rule, matched := matchCompositeRoute(inventory.Routes, model, endpoint); matched {
			var err error
			decision, err = applyExplicitCompositeDecision(decision, rule)
			if err != nil {
				result.reason = "invalid_route_destination"
				return result
			}
		} else {
			ownership := compositeOwnershipFromAccounts(inventory.Accounts, model)
			platform, detected := DetectModelPlatform(model)
			switch {
			case ownership.Matched:
				decision.TargetPlatform = ownership.TargetPlatform
				decision.Source = CompositeRouteSourceAccount
			case endpoint != CompositeRouteEndpointGemini && ownership.Ambiguous && ownership.OpenAIGatewayOAuthClaimed && domain.UsesOpenAIGateway(platform):
				result.reason = "ambiguous_oauth_ownership"
				return result
			case endpoint == CompositeRouteEndpointGemini && (ownership.Ambiguous || !detected):
				decision.TargetPlatform = PlatformGemini
			case detected:
				decision.TargetPlatform = platform
				decision.Source = CompositeRouteSourceDetector
			default:
				result.reason = "no_model_route"
				return result
			}
			decision.Matched = true
		}
	}
	if !quotaEndpointAllowsTarget(group, decision) {
		result.reason = "model_not_supported_on_endpoint"
		return result
	}
	if group.ClaudeCodeOnly && !domain.UsesOpenAIGateway(decision.TargetPlatform) &&
		(endpoint == CompositeRouteEndpointChatCompletions || endpoint == CompositeRouteEndpointResponses) {
		result.reason = "claude_code_only"
		if group.FallbackGroupID != nil && decision.TargetGroupID == nil && decision.TargetAccountID == nil {
			result.reason = "claude_code_only_fallback_not_evaluated"
			result.readFailed = true
		}
		return result
	}
	result.decision = &decision
	return result
}

func quotaEndpointAllowsTarget(group *Group, decision CompositeRouteDecision) bool {
	platform := decision.TargetPlatform
	if !isConcreteRequestPlatform(platform) || platform == PlatformTypeSafe {
		return false
	}
	switch decision.Endpoint {
	case CompositeRouteEndpointGemini:
		return IsSafeGeminiModelPathSegment(decision.PublicModel) && (platform == PlatformGemini ||
			group.Platform == PlatformComposite && platform == PlatformAntigravity && isAntigravityGeminiModel(decision.UpstreamModel))
	case CompositeRouteEndpointMessages:
		return (platform != PlatformOpenAI || group.AllowMessagesDispatch) && !IsGPTImageGenerationModel(decision.UpstreamModel)
	case CompositeRouteEndpointChatCompletions, CompositeRouteEndpointResponses:
		return !IsGPTImageGenerationModel(decision.UpstreamModel)
	default:
		return false
	}
}

func (s *GatewayService) populateModelQuotaRoutes(ctx context.Context, reader ModelQuotaRoutingReader, key *APIKey, groupIDs, pinIDs []int64, platformConstraints map[string]modelQuotaConstraintSet, planned []modelQuotaPlannedRoute, now time.Time) {
	if len(groupIDs) == 0 && len(pinIDs) == 0 {
		return
	}
	inventory, err := reader.ReadModelQuotaTargets(ctx, groupIDs, pinIDs, modelQuotaMaxAccounts)
	if err != nil {
		markModelQuotaRoutesFailed(planned, "target_inventory_unavailable")
		return
	}
	ids := make([]int64, 0, len(inventory.Accounts))
	byGroup := make(map[int64][]int64)
	for _, link := range inventory.Accounts {
		ids = append(ids, link.ID)
		for _, groupID := range link.Groups {
			byGroup[groupID] = append(byGroup[groupID], link.ID)
		}
	}
	accounts, err := s.accountRepo.GetByIDs(ctx, ids)
	if err != nil {
		markModelQuotaRoutesFailed(planned, "account_evidence_unavailable")
		return
	}
	byID := make(map[int64]*Account, len(accounts))
	for _, account := range accounts {
		if account != nil {
			byID[account.ID] = account
		}
	}
	var thresholds map[string]int
	if s.settingService != nil {
		thresholds = s.settingService.GetAccountSchedulingThresholds(ctx)
		ctx = withOpenAIQuotaAutoPauseSettings(ctx, s.settingService.GetOpenAIQuotaAutoPauseSettings(ctx))
	}
	channelByPlatform := make(map[string]*channelLookup)
	channelErrors := make(map[string]error)
	agCandidates := make(map[int64]*Account)
	combinations := 0
	for i := range planned {
		route := &planned[i]
		if route.decision == nil {
			continue
		}
		d := *route.decision
		scoped := WithCompositeRouteDecision(ctx, d)
		if _, present := channelErrors[d.TargetPlatform]; !present && s.channelService != nil {
			channelByPlatform[d.TargetPlatform], channelErrors[d.TargetPlatform] = s.channelService.lookupGroupChannel(scoped, key.Group.ID)
		}
		if channelErrors[d.TargetPlatform] != nil {
			route.reason, route.readFailed = "channel_configuration_unavailable", true
			continue
		}
		groupID := key.Group.ID
		if d.TargetGroupID != nil {
			groupID = *d.TargetGroupID
			group, present := inventory.Groups[groupID]
			if !present || !group.IsActive() || group.Platform != d.TargetPlatform || !isConcreteRequestPlatform(group.Platform) {
				route.reason = "invalid_route_destination"
				continue
			}
		}
		possible := byGroup[groupID]
		if d.TargetAccountID != nil {
			possible = []int64{*d.TargetAccountID}
		}
		if !inventory.Complete && (d.TargetAccountID == nil || byID[*d.TargetAccountID] == nil) {
			route.reason, route.readFailed = "quota_budget_exceeded", true
		}
		for _, id := range possible {
			combinations++
			if combinations > modelQuotaMaxCombinations || ctx.Err() != nil {
				route.reason, route.readFailed = "quota_budget_exceeded", true
				break
			}
			account := byID[id]
			if account == nil {
				continue
			}
			if d.TargetAccountID == nil && !quotaAccountHasGroup(account, groupID) {
				route.reason, route.readFailed = "target_membership_changed", true
				continue
			}
			candidate, accepted := s.modelQuotaAccountPath(scoped, key, account, d, channelByPlatform[d.TargetPlatform], platformConstraints, thresholds, now)
			if !accepted {
				if candidate.blockedReason != "" && route.reason == "" {
					route.reason = candidate.blockedReason
				}
				continue
			}
			route.candidates = append(route.candidates, candidate)
			if account.Platform == PlatformAntigravity {
				agCandidates[account.ID] = account
			}
		}
		if len(route.candidates) == 0 && route.reason == "" {
			route.reason = "no_eligible_configured_account"
		}
	}

	if len(agCandidates) != 0 {
		agAccounts := make([]*Account, 0, len(agCandidates))
		for _, account := range agCandidates {
			agAccounts = append(agAccounts, account)
		}
		reads := s.readModelQuotaAntigravity(ctx, agAccounts)
		for i := range planned {
			for j := range planned[i].candidates {
				candidate := &planned[i].candidates[j]
				if candidate.account.Platform == PlatformAntigravity {
					candidate.constraints.merge(projectAntigravityModelQuota(candidate.account, candidate.upstreamModel, reads[candidate.account.ID]))
				}
			}
		}
	}
}

func markModelQuotaRoutesFailed(planned []modelQuotaPlannedRoute, reason string) {
	for i := range planned {
		if planned[i].decision != nil {
			planned[i].reason, planned[i].readFailed = reason, true
		}
	}
}

func quotaAccountHasGroup(account *Account, groupID int64) bool {
	for _, membership := range account.AccountGroups {
		if membership.GroupID == groupID {
			return true
		}
	}
	return false
}

func (s *GatewayService) modelQuotaAccountPath(ctx context.Context, key *APIKey, account *Account, d CompositeRouteDecision, channel *channelLookup, platformConstraints map[string]modelQuotaConstraintSet, thresholds map[string]int, now time.Time) (modelQuotaCandidate, bool) {
	result := modelQuotaCandidate{account: account}
	mixed := d.TargetGroupID == nil && d.TargetAccountID == nil && (d.TargetPlatform == PlatformGemini || d.TargetPlatform == PlatformAnthropic) && account.IsMixedSchedulingEnabled()
	if account.ExpiresAt != nil && account.AutoPauseOnExpired && !account.ExpiresAt.After(now) {
		result.blockedReason = "account_expired"
		return result, false
	}
	if !account.IsActive() || !account.Schedulable || (account.Platform != d.TargetPlatform && !mixed) || !CompositeRouteAccountAllowed(ctx, account) {
		return result, false
	}
	if d.Source == CompositeRouteSourceAccount && !explicitModelMappingClaims(*account, d.PublicModel) {
		return result, false
	}
	if key.Group.RequirePrivacySet && !account.IsPrivacySet() || key.Group.RequireOAuthOnly && account.Type == AccountTypeAPIKey {
		return result, false
	}
	requestModel := d.UpstreamModel
	forwardModel := requestModel
	if channel != nil {
		mapping := resolveMapping(channel, key.Group.ID, requestModel)
		forwardModel = mapping.MappedModel
		pricingModel := billingModelForRestriction(mapping.BillingModelSource, requestModel, forwardModel)
		if pricingModel != "" && checkRestricted(channel, key.Group.ID, pricingModel) {
			return result, false
		}
	}
	selectionModel := requestModel
	if domain.UsesOpenAIGateway(d.TargetPlatform) || d.Endpoint == CompositeRouteEndpointGemini {
		selectionModel = forwardModel
	}
	if d.TargetPlatform == PlatformOpenAI && d.Endpoint == CompositeRouteEndpointMessages {
		if mapped := key.Group.ResolveMessagesDispatchModel(requestModel); mapped != "" {
			selectionModel, forwardModel = mapped, mapped
		}
	}
	if !s.isModelSupportedByAccountWithContext(ctx, account, selectionModel) {
		return result, false
	}
	if domain.UsesOpenAIGateway(d.TargetPlatform) && !account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityChatCompletions) {
		return result, false
	}
	result.upstreamModel = quotaAccountWireModel(account, forwardModel)
	if result.upstreamModel == "" || channel != nil && channel.channel.BillingModelSource == BillingModelSourceUpstream && checkRestricted(channel, key.Group.ID, result.upstreamModel) {
		return result, false
	}
	result.constraints = projectAccountModelQuota(account, result.upstreamModel, now)
	if platformSet, ok := platformConstraints[QuotaPlatform(ctx, key)]; ok {
		result.constraints.merge(platformSet)
	}
	result.cooldownUntil = latestModelQuotaReset(nil, account.RateLimitResetAt, now)
	for _, scope := range account.modelRateLimitKeysForRequest(ctx, selectionModel) {
		result.cooldownUntil = latestModelQuotaReset(result.cooldownUntil, account.modelRateLimitResetAt(scope), now)
	}

	if account.OverloadUntil != nil && account.OverloadUntil.After(now) {
		result.blockedReason, result.blockedUntil = "account_overloaded", account.OverloadUntil
	}
	if account.TempUnschedulableUntil != nil && account.TempUnschedulableUntil.After(now) {
		if result.blockedReason == "" {
			result.blockedReason = "account_temporarily_unschedulable"
		}
		result.blockedUntil = latestModelQuotaReset(result.blockedUntil, account.TempUnschedulableUntil, now)
	}
	pause := EvaluateAccountSchedulingThreshold(account, thresholds, now)
	if isAnthropicFableModel(result.upstreamModel) {
		fablePause := evaluateAnthropicFableSchedulingThreshold(account, thresholds, now)
		if fablePause.ShouldPause && latestModelQuotaReset(pause.Until, fablePause.Until, now) == fablePause.Until {
			pause = fablePause
		}
	}
	if pause.ShouldPause {
		result.recordQuotaPolicyPause(pause.Until, now)
	}
	if paused, decision := evaluateOpenAIAccountQuotaPause(ctx, account, now); paused {
		var reset *time.Time
		if decision.reason == "" {
			reset = result.windowReset(decision.window, now)
		}
		result.recordQuotaPolicyPause(reset, now)
	}
	if paused, decision := evaluateGrokAccountQuotaPause(account, now); paused {
		result.recordQuotaPolicyPause(result.windowReset(decision.window, now), now)
	}
	return result, true
}

func (c *modelQuotaCandidate) windowReset(name string, now time.Time) *time.Time {
	for _, window := range c.constraints.windows {
		if window.window == name && modelQuotaWindowCurrent(window, now) {
			return window.resetsAt
		}
	}
	return nil
}

func (c *modelQuotaCandidate) recordQuotaPolicyPause(reset *time.Time, now time.Time) {
	if c.blockedReason == "" {
		c.blockedReason, c.blockedUntil = "quota_policy_pause", latestModelQuotaReset(nil, reset, now)
	} else if c.blockedUntil == nil || reset == nil {
		c.blockedUntil = nil
	} else {
		c.blockedUntil = latestModelQuotaReset(c.blockedUntil, reset, now)
	}
}

func quotaAccountWireModel(account *Account, model string) string {
	switch {
	case account.IsBedrock():
		resolved, _ := ResolveBedrockModelID(account, model)
		return resolved
	case account.Platform == PlatformAntigravity:
		return mapAntigravityModel(account, model)
	case domain.UsesOpenAIGateway(account.Platform):
		return resolveOpenAIAccountUpstreamModelForRequest(account, model, false)
	case account.Platform == PlatformAnthropic && account.Type != AccountTypeAPIKey:
		model = claude.NormalizeModelID(model)
		if account.Type == AccountTypeServiceAccount {
			model = normalizeVertexAnthropicModelID(model)
		}
	}
	return strings.TrimSpace(account.GetMappedModel(model))
}
