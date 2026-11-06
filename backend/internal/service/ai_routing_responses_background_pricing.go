package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/tidwall/gjson"
)

var ErrAIBackgroundUnsupported = errors.New("background response pricing or capacity is unsupported")

// This is a closed DTO, not an Account/Group/Channel serialization. Price cards
// contain no credentials, arbitrary metadata, request content or live resolver.
type aiBackgroundResolvedPrice struct {
	Mode           BillingMode          `json:"mode"`
	Base           *ModelPricing        `json:"base,omitempty"`
	Intervals      []PricingInterval    `json:"intervals,omitempty"`
	RequestTiers   []PricingInterval    `json:"request_tiers,omitempty"`
	PerRequest     float64              `json:"per_request"`
	Source         string               `json:"source"`
	CacheBreakdown bool                 `json:"cache_breakdown"`
	Channel        *ChannelModelPricing `json:"channel,omitempty"`
	LongContext    bool                 `json:"long_context"`
}

func freezeAIBackgroundResolved(p *ResolvedPricing) *aiBackgroundResolvedPrice {
	if p == nil {
		return nil
	}
	return &aiBackgroundResolvedPrice{Mode: p.Mode, Base: p.BasePricing, Intervals: p.Intervals, RequestTiers: p.RequestTiers, PerRequest: p.DefaultPerRequestPrice, Source: p.Source, CacheBreakdown: p.SupportsCacheBreakdown, Channel: p.channelPricing, LongContext: p.longContextPricingEnabled}
}
func (p *aiBackgroundResolvedPrice) restore() *ResolvedPricing {
	if p == nil {
		return nil
	}
	return &ResolvedPricing{Mode: p.Mode, BasePricing: p.Base, Intervals: p.Intervals, RequestTiers: p.RequestTiers, DefaultPerRequestPrice: p.PerRequest, Source: p.Source, SupportsCacheBreakdown: p.CacheBreakdown, channelPricing: p.Channel, longContextPricingEnabled: p.LongContext}
}

type aiBackgroundPriceSnapshot struct {
	Version            int                        `json:"version"`
	Model              string                     `json:"model"`
	UpstreamModel      string                     `json:"upstream_model"`
	EndpointHash       string                     `json:"endpoint_hash"`
	PricingAt          time.Time                  `json:"pricing_at"`
	Price              *aiBackgroundResolvedPrice `json:"price"`
	Legacy             bool                       `json:"legacy"`
	Multiplier         float64                    `json:"multiplier"`
	AccountMultiplier  float64                    `json:"account_multiplier"`
	AccountType        string                     `json:"account_type"`
	AccountPlatform    string                     `json:"account_platform"`
	AccountQuota       bool                       `json:"account_quota"`
	AccountDailyQuota  bool                       `json:"account_daily_quota"`
	AccountWeeklyQuota bool                       `json:"account_weekly_quota"`
	KeyQuota           bool                       `json:"key_quota"`
	KeyRate            bool                       `json:"key_rate"`
	SentTier           string                     `json:"sent_tier"`
	FreeFast           bool                       `json:"free_fast"`
	Reasoning          string                     `json:"reasoning,omitempty"`
	AccountLongContext *bool                      `json:"account_long_context,omitempty"`
	StatsCustom        *ChannelModelPricing       `json:"stats_custom,omitempty"`
	StatsCustomer      bool                       `json:"stats_customer"`
	StatsDefault       *aiBackgroundResolvedPrice `json:"stats_default,omitempty"`
	ChannelID          int64                      `json:"channel_id,omitempty"`
}

func decodeAIBackgroundPrice(j *AIRoutingBackgroundJob) (*aiBackgroundPriceSnapshot, error) {
	if j == nil || len(j.PricingSnapshot) > 64<<10 || !requestmodel.HasUniqueJSONMembers(j.PricingSnapshot) {
		return nil, ErrAIBackgroundUnsupported
	}
	var p aiBackgroundPriceSnapshot
	d := json.NewDecoder(bytes.NewReader(j.PricingSnapshot))
	d.DisallowUnknownFields()
	if d.Decode(&p) != nil || p.Version != 1 || p.Model != j.ExecutionModel || p.Price == nil || p.PricingAt.IsZero() || p.UpstreamModel == "" || p.AccountType != AccountTypeAPIKey || p.Multiplier < 0 || p.AccountMultiplier < 0 || p.Price.Mode != BillingModeToken && p.Price.Mode != BillingModePerRequest {
		return nil, ErrAIBackgroundUnsupported
	}
	return &p, nil
}

// Prepare requires the final native request AFTER model/tier/effort rewriting.
// It only freezes identity/prices and calculates a conservative hold. It does
// not reserve money, mark submitting, call a model or persist request content.
func (s *AIRoutingResponsesBackgroundService) Prepare(ctx context.Context, job *AIRoutingBackgroundJob, key *APIKey, user *User, account *Account, subscription *UserSubscription, finalBody []byte, pricingAt time.Time) (*AIRoutingBackgroundJob, error) {
	if s == nil || s.gateway == nil || s.gateway.billingService == nil || job == nil || key == nil || key.Group == nil || key.GroupID == nil || user == nil || account == nil || account.Type != AccountTypeAPIKey || job.UserID != user.ID || job.APIKeyID != key.ID || job.SourceGroupID != key.Group.ID || job.AccountID != account.ID || !AIRoutingAccountSupportsStoredResponses(account, job.ExecutionModel) || len(finalBody) == 0 || len(finalBody) > 32<<20 || !requestmodel.HasUniqueJSONMembers(finalBody) {
		return nil, ErrAIBackgroundUnsupported
	}
	if s.gateway.cfg != nil && s.gateway.cfg.RunMode == config.RunModeSimple {
		return nil, ErrAIBackgroundUnsupported
	}
	root := gjson.ParseBytes(finalBody)
	if !root.IsObject() || !aiCanonicalFields(root, "model", "background", "store", "tools", "service_tier", "reasoning", "max_output_tokens", "input", "previous_response_id", "conversation") || root.Get("background").Type != gjson.True || root.Get("store").Type == gjson.False || root.Get("model").Type != gjson.String {
		return nil, ErrAIBackgroundUnsupported
	}
	parsed, parseErr := NewAIRoutingResponsesRequest(finalBody, "metadata")
	if parseErr != nil {
		return nil, ErrAIBackgroundUnsupported
	}
	if store := root.Get("store"); store.Exists() && store.Type != gjson.True {
		return nil, ErrAIBackgroundUnsupported
	}
	if tools := root.Get("tools"); tools.Exists() && !tools.IsArray() {
		return nil, ErrAIBackgroundUnsupported
	}
	if tier := root.Get("service_tier"); tier.Exists() && (tier.Type != gjson.String || len(tier.String()) > 64) {
		return nil, ErrAIBackgroundUnsupported
	}
	if reasoning := root.Get("reasoning"); reasoning.Exists() && reasoning.Type != gjson.Null && (!reasoning.IsObject() || !aiCanonicalFields(reasoning, "effort") || reasoning.Get("effort").Exists() && (reasoning.Get("effort").Type != gjson.String || len(reasoning.Get("effort").String()) > 64)) {
		return nil, ErrAIBackgroundUnsupported
	}
	for _, tool := range root.Get("tools").Array() {
		if !tool.IsObject() || !aiCanonicalFields(tool, "type") || tool.Get("type").String() != "function" && tool.Get("type").String() != "custom" {
			return nil, ErrAIBackgroundUnsupported
		}
	}
	if tc := root.Get("tool_choice"); tc.IsObject() && tc.Get("type").String() != "function" && tc.Get("type").String() != "custom" {
		return nil, ErrAIBackgroundUnsupported
	}
	if pricingAt.IsZero() {
		pricingAt = time.Now()
	}
	check := CheckAIRoutingExecutionAccount(ctx, account, job.ExecutionModel)
	if !check.Allowed {
		return nil, ErrAIBackgroundUnsupported
	}
	output := root.Get("max_output_tokens")
	outputReserve := check.OutputReserveTokens
	if output.Exists() {
		if output.Type != gjson.Number || output.Float() != float64(output.Int()) || output.Int() <= 0 {
			return nil, ErrAIBackgroundUnsupported
		}
		outputReserve = output.Int()
	}
	inputReserve := int64(len(finalBody))
	_, strict := AIRoutingExecutionRequirementsFromContext(ctx)
	needsContextBound := parsed.RequiresOrigin
	for _, m := range parsed.InputModalities {
		if m != "text" {
			needsContextBound = true
		}
	}
	if needsContextBound {
		if !strict || check.ContextWindowTokens <= 0 {
			return nil, ErrAIBackgroundUnsupported
		}
		inputReserve = check.ContextWindowTokens
	}
	if inputReserve <= 0 || outputReserve <= 0 || inputReserve > math.MaxInt32 || outputReserve > math.MaxInt32 {
		return nil, ErrAIBackgroundUnsupported
	}
	subID := int64(0)
	if subscription != nil {
		subID = subscription.ID
	}
	if subID != job.SubscriptionID || subID > 0 && !key.Group.IsSubscriptionType() {
		return nil, ErrAIBackgroundConflict
	}
	base := key.Group.RateMultiplier
	base = s.gateway.ResolveUserGroupRateMultiplier(ctx, user.ID, key.Group.ID, base)
	multiplier, _ := computePeakAwareMultipliers(key, base, pricingAt)
	upstreamModel := root.Get("model").String()
	if upstreamModel != job.ExecutionModel {
		return nil, ErrAIBackgroundUnsupported
	}
	p := &aiBackgroundPriceSnapshot{Version: 1, Model: job.ExecutionModel, UpstreamModel: upstreamModel, PricingAt: pricingAt, Multiplier: multiplier, AccountMultiplier: account.BillingRateMultiplier(), AccountType: account.Type, AccountPlatform: account.Platform, AccountQuota: account.SupportsAccountQuota() && account.HasAnyQuotaLimit(), AccountDailyQuota: account.GetQuotaDailyLimit() > 0, AccountWeeklyQuota: account.GetQuotaWeeklyLimit() > 0, KeyQuota: key.Quota > 0, KeyRate: key.HasRateLimits(), SentTier: root.Get("service_tier").String(), Reasoning: root.Get("reasoning.effort").String(), AccountLongContext: openAILongContextBillingGate(account)}
	p.FreeFast = groupBillsOpenAIFastAtStandard(key, account, p.SentTier)
	p.EndpointHash = aiBackgroundEndpointHash(account)
	if s.gateway.resolver != nil {
		resolved := s.gateway.resolver.Resolve(ctx, PricingInput{Model: p.Model, GroupID: key.GroupID, Group: key.Group})
		p.Price = freezeAIBackgroundResolved(resolved)
	} else {
		price, err := s.gateway.billingService.GetModelPricing(p.Model)
		if err != nil {
			return nil, err
		}
		p.Price = &aiBackgroundResolvedPrice{Mode: BillingModeToken, Base: price, Source: PricingSourceLiteLLM, LongContext: p.AccountLongContext == nil || *p.AccountLongContext}
		p.Legacy = true
	}
	if p.Price == nil || p.Price.Mode != BillingModeToken && p.Price.Mode != BillingModePerRequest {
		return nil, ErrAIBackgroundUnsupported
	}
	if cs := s.gateway.channelService; cs != nil {
		channel, err := cs.GetChannelForGroup(ctx, key.Group.ID)
		if err != nil {
			return nil, err
		}
		if channel != nil {
			p.ChannelID = channel.ID
			p.StatsCustomer = channel.ApplyPricingToAccountStats
			platform := cs.GetGroupPlatform(ctx, key.Group.ID)
			for _, rule := range channel.AccountStatsPricingRules {
				if matchAccountStatsRule(&rule, account.ID, key.Group.ID) {
					if price := findPricingForModel(rule.Pricing, platform, strings.ToLower(upstreamModel)); price != nil {
						cloned := price.Clone()
						p.StatsCustom = &cloned
						break
					}
				}
			}
			resolver := NewModelPricingResolver(nil, s.gateway.billingService)
			resolved := resolver.Resolve(ctx, PricingInput{Model: upstreamModel})
			resolved.longContextPricingEnabled = accountStatsLongContextPricingEnabled(p.AccountLongContext)
			p.StatsDefault = freezeAIBackgroundResolved(resolved)
		}
	}
	// JSON round-trip creates an immutable copy of all maps/price cards; no
	// resolver/cache pointer may change a job after admission.
	snapshot, err := json.Marshal(p)
	if err != nil || len(snapshot) > 64<<10 {
		return nil, ErrAIBackgroundUnsupported
	}
	copyJob := *job
	copyJob.PricingSnapshot = snapshot
	copyJob.RequestPayloadHash = HashUsageRequestPayload(finalBody)
	var trace AIRoutingTrace
	trace.SetRequestContext(ctx)
	copyJob.OriginalRequestID = trace.RequestID
	copyJob.ClientRequestID = trace.ClientRequestID
	copyJob.OriginalUsageRequestID = trace.UsageRequestID
	frozen, err := decodeAIBackgroundPrice(&copyJob)
	if err != nil {
		return nil, err
	}
	hold, err := frozen.hold(int(inputReserve), int(outputReserve))
	if err != nil {
		return nil, err
	}
	copyJob.HoldAmount = QuantizeUsageBillingAmount(hold)
	if err = copyJob.Validate(); err != nil {
		return nil, err
	}
	return &copyJob, nil
}

func (p *aiBackgroundPriceSnapshot) calculate(tokens UsageTokens, tier string) (*CostBreakdown, error) {
	calculator := &BillingService{}
	var cost *CostBreakdown
	var err error
	if p.Legacy {
		if p.Price.Base == nil {
			return nil, ErrModelPricingUnavailable
		}
		cost = calculator.computeTokenBreakdown(p.Price.Base, tokens, p.Multiplier, tier, p.AccountLongContext == nil || *p.AccountLongContext)
		cost.BillingMode = string(BillingModeToken)
	} else {
		cost, err = calculator.CalculateCostUnified(CostInput{Ctx: context.Background(), Model: p.Model, Tokens: tokens, RequestCount: 1, RateMultiplier: p.Multiplier, PricingAt: p.PricingAt, ServiceTier: tier, ReasoningEffort: p.Reasoning, Resolver: &ModelPricingResolver{}, Resolved: p.Price.restore(), LongContextBillingEnabled: p.AccountLongContext})
	}
	if err != nil {
		return nil, err
	}
	if cost == nil || math.IsNaN(cost.ActualCost) || math.IsInf(cost.ActualCost, 0) || cost.ActualCost < 0 || cost.ActualCost >= 1e12 {
		return nil, ErrAIBackgroundUnsupported
	}
	return cost, nil
}

func (p *aiBackgroundPriceSnapshot) customerCost(tokens UsageTokens, tier string) (*CostBreakdown, error) {
	cost, err := p.calculate(tokens, tier)
	if err != nil {
		return nil, err
	}
	// Preserve the actual upstream priority cost and service tier separately
	// from the customer's free-fast Standard-tier charge.
	if p.FreeFast && (tier == "priority" || tier == "fast") {
		standard, e := p.calculate(tokens, "")
		if e != nil {
			return nil, e
		}
		cost.ActualCost = standard.ActualCost
	}
	return cost, nil
}

// Non-monotone interval cards can cost more below the maximum context. Evaluate
// both sides of every tier boundary and every input price bucket, reusing the
// same calculator rather than inventing an input/output-only price formula.
func (p *aiBackgroundPriceSnapshot) hold(input, output int) (float64, error) {
	points := map[int]bool{1: true, input: true}
	add := func(n int) {
		if n > 0 && n <= input {
			points[n] = true
		}
		if n > 1 && n-1 <= input {
			points[n-1] = true
		}
		if n < math.MaxInt && n+1 <= input && n >= 0 {
			points[n+1] = true
		}
	}
	for _, iv := range append(append([]PricingInterval{}, p.Price.Intervals...), p.Price.RequestTiers...) {
		add(iv.MinTokens)
		if iv.MaxTokens != nil {
			add(*iv.MaxTokens)
		}
	}
	if p.Price.Base != nil {
		add(p.Price.Base.LongContextInputThreshold)
	}
	hold := 0.0
	for n := range points {
		for _, tokens := range []UsageTokens{{InputTokens: n, OutputTokens: output}, {CacheReadTokens: n, OutputTokens: output}, {CacheCreationTokens: n, OutputTokens: output}, {InputTokens: n, ImageInputTokens: n, OutputTokens: output}} {
			cost, err := p.customerCost(tokens, normalizeBillingServiceTier(p.SentTier))
			if err != nil {
				return 0, err
			}
			hold = math.Max(hold, cost.ActualCost)
		}
	}
	return hold, nil
}

func (p *aiBackgroundPriceSnapshot) prepareBilling(j *AIRoutingBackgroundJob) (*UsageBillingCommand, *UsageLog, error) {
	if j.Terminal == nil || !j.Terminal.UsageKnown {
		return nil, nil, ErrAIBackgroundUnknownUsage
	}
	t := j.Terminal
	tokens := UsageTokens{InputTokens: max(t.InputTokens-t.CacheReadTokens-t.CacheCreationTokens, 0), OutputTokens: t.OutputTokens, CacheCreationTokens: t.CacheCreationTokens, CacheReadTokens: t.CacheReadTokens, ImageInputTokens: max(t.ImageInputTokens-t.ImageCacheReadTokens, 0), ImageCacheReadTokens: t.ImageCacheReadTokens, ImageOutputTokens: t.ImageOutputTokens}
	tier := ResolveBillingServiceTier(p.SentTier, t.ServiceTier).Billing
	cost, err := p.customerCost(tokens, tier)
	if err != nil {
		return nil, nil, err
	}
	key := &APIKey{ID: j.APIKeyID, UserID: j.UserID, GroupID: &j.SourceGroupID}
	if p.KeyQuota {
		key.Quota = 1
	}
	if p.KeyRate {
		key.RateLimit5h = 1
	}
	account := &Account{ID: j.AccountID, Platform: p.AccountPlatform, Type: p.AccountType, Extra: map[string]any{}}
	if p.AccountQuota {
		account.Extra["quota_limit"] = float64(1)
	}
	if p.AccountDailyQuota {
		account.Extra["quota_daily_limit"] = float64(1)
	}
	if p.AccountWeeklyQuota {
		account.Extra["quota_weekly_limit"] = float64(1)
	}
	var sub *UserSubscription
	billingType := BillingTypeBalance
	if j.SubscriptionID > 0 {
		sub = &UserSubscription{ID: j.SubscriptionID}
		billingType = BillingTypeSubscription
	}
	created := j.UpdatedAt
	if j.SettledAt != nil {
		created = *j.SettledAt
	}
	log := &UsageLog{UserID: j.UserID, APIKeyID: j.APIKeyID, AccountID: j.AccountID, RequestID: AIRoutingBackgroundBillingRequestID(j.ID), Model: j.ExecutionModel, RequestedModel: j.PublicModel, UpstreamModel: optionalTrimmedStringPtr(p.UpstreamModel), UpstreamResponseModel: optionalTrimmedStringPtr(t.Model), UpstreamModelMismatch: upstreamModelMismatch(p.UpstreamModel, t.Model), GroupID: &j.SourceGroupID, ServiceTier: optionalTrimmedStringPtr(tier), ReasoningEffort: optionalTrimmedStringPtr(p.Reasoning), InboundEndpoint: optionalTrimmedStringPtr("/v1/responses"), UpstreamEndpoint: optionalTrimmedStringPtr("/v1/responses"), InputTokens: tokens.InputTokens, OutputTokens: tokens.OutputTokens, CacheCreationTokens: tokens.CacheCreationTokens, CacheReadTokens: tokens.CacheReadTokens, ImageInputTokens: t.ImageInputTokens, ImageOutputTokens: t.ImageOutputTokens, InputCost: cost.InputCost, ImageInputCost: cost.ImageInputCost, OutputCost: cost.OutputCost, ImageOutputCost: cost.ImageOutputCost, CacheCreationCost: cost.CacheCreationCost, CacheReadCost: cost.CacheReadCost, TotalCost: cost.TotalCost, ActualCost: cost.ActualCost, RateMultiplier: p.Multiplier, AccountRateMultiplier: &p.AccountMultiplier, LongContextBillingApplied: cost.LongContextBillingApplied, BillingType: billingType, BillingMode: optionalTrimmedStringPtr(cost.BillingMode), ChannelID: optionalInt64Ptr(p.ChannelID), CreatedAt: created}
	if sub != nil {
		log.SubscriptionID = &sub.ID
	}
	if t.ImageCacheReadTokens > 0 {
		log.ImageSizeBreakdown = map[string]int{"image_cache_read_tokens": t.ImageCacheReadTokens}
	}
	if p.StatsCustom != nil {
		log.AccountStatsCost = calculateStatsCost(p.StatsCustom, tokens, 1)
		if log.AccountStatsCost != nil {
			*log.AccountStatsCost *= reasoningEffortBillingMultiplier(p.Reasoning, p.StatsCustom.ReasoningEffortMultipliers)
		}
	}
	if log.AccountStatsCost == nil && p.StatsCustomer && cost.TotalCost > 0 {
		value := cost.TotalCost
		log.AccountStatsCost = &value
	}
	if log.AccountStatsCost == nil && p.StatsDefault != nil && !p.StatsCustomer {
		stats, statsErr := (&BillingService{}).CalculateCostUnified(CostInput{Ctx: context.Background(), Model: p.UpstreamModel, Tokens: tokens, RequestCount: 1, RateMultiplier: 1, PricingAt: p.PricingAt, ServiceTier: tier, ReasoningEffort: p.Reasoning, Resolver: &ModelPricingResolver{}, Resolved: p.StatsDefault.restore()})
		if statsErr == nil && stats != nil && stats.TotalCost > 0 {
			log.AccountStatsCost = &stats.TotalCost
		}
	}
	cmd := buildUsageBillingCommand(log.RequestID, log, &postUsageBillingParams{Cost: cost, User: &User{ID: j.UserID}, APIKey: key, Account: account, Subscription: sub, RequestPayloadHash: j.RequestPayloadHash, IsSubscriptionBill: sub != nil, AccountRateMultiplier: p.AccountMultiplier, APIKeyService: aiBackgroundQuotaMarker{}})
	return cmd, log, nil
}

// buildUsageBillingCommand only tests this dependency's presence; writes are
// performed atomically by the background repository, never by this marker.
type aiBackgroundQuotaMarker struct{}

// A later account URL edit cannot redirect recovery of an existing provider
// resource to another server. Credentials themselves are never persisted.
func aiBackgroundEndpointHash(account *Account) string {
	if account == nil {
		return ""
	}
	base := account.GetOpenAIBaseURL()
	if base == "" {
		base = "https://api.openai.com"
	}
	return HashUsageRequestPayload([]byte(buildOpenAIResponsesURLForPlatform(account.Platform, base)))
}

func (aiBackgroundQuotaMarker) UpdateQuotaUsed(context.Context, int64, float64) error {
	return ErrAIBackgroundConflict
}
func (aiBackgroundQuotaMarker) UpdateRateLimitUsage(context.Context, int64, float64) error {
	return ErrAIBackgroundConflict
}
