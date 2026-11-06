//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func backgroundPriceFixture(t *testing.T) (*AIRoutingResponsesBackgroundService, *AIRoutingBackgroundJob, *APIKey, *User, *Account) {
	t.Helper()
	cfg := &config.Config{}
	cfg.Default.RateMultiplier = 1
	gateway := &OpenAIGatewayService{cfg: cfg, billingService: NewBillingService(cfg, nil), billingCacheService: &BillingCacheService{}}
	gateway.resolver = NewModelPricingResolver(nil, gateway.billingService)
	group := &Group{ID: 3, Platform: PlatformOpenAI, RateMultiplier: 1.5, LongContextPricingEnabled: true, FreeOpenAIFast: true, PeakRateEnabled: true, PeakStart: "00:00", PeakEnd: "23:59", PeakRateMultiplier: 2}
	key := &APIKey{ID: 2, UserID: 1, GroupID: &group.ID, Group: group, Quota: 100, RateLimit5h: 100}
	user := &User{ID: 1}
	account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "FAKE_SECRET_DO_NOT_PERSIST"}, Extra: map[string]any{"quota_limit": 100.0, "quota_daily_limit": 10.0, "openai_long_context_billing_enabled": true}}
	j := &AIRoutingBackgroundJob{ID: "background-fixture-uuid", SourceGroupID: 3, UserID: 1, APIKeyID: 2, AccountID: 7, TargetAccountID: 7, CandidateID: "c1", PublicModel: "client-any", ExecutionModel: "gpt-5.1", Platform: PlatformOpenAI, PolicyVersion: 1}
	return NewAIRoutingResponsesBackgroundService(nil, gateway), j, key, user, account
}

func TestAIRoutingBackgroundPricingMatchesExistingRecordUsage(t *testing.T) {
	for _, subscription := range []bool{false, true} {
		t.Run(fmt.Sprint(subscription), func(t *testing.T) {
			s, j, key, user, account := backgroundPriceFixture(t)
			var sub *UserSubscription
			if subscription {
				key.Group.SubscriptionType = SubscriptionTypeSubscription
				sub = &UserSubscription{ID: 8}
				j.SubscriptionID = 8
			}
			at := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
			body := []byte(`{"model":"gpt-5.1","background":true,"store":true,"input":"SENSITIVE_PROMPT_SENTINEL","max_output_tokens":100,"service_tier":"priority","reasoning":{"effort":"high"},"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`)
			saved, err := s.Prepare(t.Context(), j, key, user, account, sub, body, at)
			require.NoError(t, err)
			require.NotContains(t, string(saved.PricingSnapshot), "SENSITIVE_PROMPT_SENTINEL")
			require.NotContains(t, string(saved.PricingSnapshot), "FAKE_SECRET_DO_NOT_PERSIST")
			saved.UpdatedAt = at
			saved.Terminal = &AIRoutingBackgroundTerminal{Status: "completed", UsageKnown: true, Model: "gpt-5.1", ServiceTier: "priority", InputTokens: 90, OutputTokens: 10, CacheReadTokens: 30, CacheCreationTokens: 5, ImageInputTokens: 10, ImageOutputTokens: 2}
			p, err := decodeAIBackgroundPrice(saved)
			require.NoError(t, err)
			command, log, err := p.prepareBilling(saved)
			require.NoError(t, err)
			usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
			billingRepo := &openAIRecordUsageBillingRepoStub{}
			s.gateway.usageLogRepo = usageRepo
			s.gateway.usageBillingRepo = billingRepo
			s.gateway.deferredService = &DeferredService{}
			tier := "priority"
			effort := "high"
			require.NoError(t, s.gateway.RecordUsage(context.Background(), &OpenAIRecordUsageInput{Result: &OpenAIForwardResult{RequestID: "normal-compare", Model: "gpt-5.1", UpstreamModel: "gpt-5.1", Usage: OpenAIUsage{InputTokens: 90, OutputTokens: 10, CacheReadInputTokens: 30, CacheCreationInputTokens: 5, ImageInputTokens: 10, ImageOutputTokens: 2}, ServiceTier: &tier, ReasoningEffort: &effort, UpstreamResponseServiceTier: "priority"}, APIKey: key, User: user, Account: account, Subscription: sub, PricingAt: at, APIKeyService: &openAIRecordUsageAPIKeyQuotaStub{}}))
			require.InDelta(t, usageRepo.lastLog.ActualCost, log.ActualCost, 1e-12)
			require.InDelta(t, usageRepo.lastLog.TotalCost, log.TotalCost, 1e-12)
			require.Equal(t, usageRepo.lastLog.InputTokens, command.InputTokens)
			require.Equal(t, 55, command.InputTokens)
			require.Equal(t, billingRepo.lastCmd.BalanceCost, command.BalanceCost)
			require.Equal(t, billingRepo.lastCmd.SubscriptionCost, command.SubscriptionCost)
			require.Equal(t, billingRepo.lastCmd.APIKeyQuotaCost, command.APIKeyQuotaCost)
			require.Equal(t, billingRepo.lastCmd.APIKeyRateLimitCost, command.APIKeyRateLimitCost)
			require.Equal(t, billingRepo.lastCmd.AccountQuotaCost, command.AccountQuotaCost)
			require.Equal(t, "priority", p.SentTier)
			require.True(t, p.FreeFast)
			require.Greater(t, log.TotalCost*p.Multiplier, log.ActualCost)
			// Changing live configuration after submission cannot change recovery.
			key.Group.RateMultiplier = 99
			key.Group.FreeOpenAIFast = false
			s.gateway.billingService.fallbackPrices["gpt-5.1"] = &ModelPricing{InputPricePerToken: 100, OutputPricePerToken: 100}
			restored, err := decodeAIBackgroundPrice(saved)
			require.NoError(t, err)
			again, _, err := restored.prepareBilling(saved)
			require.NoError(t, err)
			require.Equal(t, command.RequestFingerprint, again.RequestFingerprint)
		})
	}
}

func TestAIRoutingBackgroundPricingFrozenIntervalsAndNonMonotoneHold(t *testing.T) {
	price := 1.0
	maxTokens := 10
	resolved := &ResolvedPricing{Mode: BillingModeToken, Source: PricingSourceChannel, BasePricing: &ModelPricing{InputPricePerToken: .001, OutputPricePerToken: .001, CacheReadPricePerToken: .002, CacheCreationPricePerToken: .003}, Intervals: []PricingInterval{{MinTokens: 0, MaxTokens: &maxTokens, InputPrice: &price}}, channelPricing: &ChannelModelPricing{TimePricing: &ChannelTimePricing{Timezone: "UTC", Periods: []ChannelTimePricingPeriod{{StartTime: "00:00", EndTime: "23:59:59", Multiplier: 2}}}}, longContextPricingEnabled: true}
	p := &aiBackgroundPriceSnapshot{Version: 1, Model: "custom", UpstreamModel: "custom", Price: freezeAIBackgroundResolved(resolved), PricingAt: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC), Multiplier: 3, AccountMultiplier: 1, AccountType: AccountTypeAPIKey, SentTier: "flex", Reasoning: "high"}
	data, err := json.Marshal(p)
	require.NoError(t, err)
	j := &AIRoutingBackgroundJob{ExecutionModel: "custom", PricingSnapshot: data}
	frozen, err := decodeAIBackgroundPrice(j)
	require.NoError(t, err)
	for _, n := range []int{5, 10, 11, 100} {
		tokens := UsageTokens{InputTokens: n, OutputTokens: 4}
		cost, err := frozen.customerCost(tokens, "flex")
		require.NoError(t, err)
		want, err := (&BillingService{}).CalculateCostUnified(CostInput{Ctx: t.Context(), Model: "custom", Tokens: tokens, RateMultiplier: 3, PricingAt: p.PricingAt, ServiceTier: "flex", ReasoningEffort: "high", Resolver: &ModelPricingResolver{}, Resolved: resolved})
		require.NoError(t, err)
		require.Equal(t, want, cost)
	}
	hold, err := frozen.hold(100, 4)
	require.NoError(t, err)
	small, err := frozen.customerCost(UsageTokens{InputTokens: 9, OutputTokens: 4}, "flex")
	require.NoError(t, err)
	require.GreaterOrEqual(t, hold, small.ActualCost)
	resolved.BasePricing.InputPricePerToken = 1000
	price = 1000
	cost, err := frozen.customerCost(UsageTokens{InputTokens: 5, OutputTokens: 4}, "flex")
	require.NoError(t, err)
	require.Less(t, cost.ActualCost, 100.0)
}

func TestAIRoutingBackgroundPrepareRejectsUnpricedToolsAndUnknownHistory(t *testing.T) {
	s, j, key, user, account := backgroundPriceFixture(t)
	for _, extra := range []string{`,"tools":[{"type":"web_search"}]`, `,"tools":[{"type":"image_generation"}]`, `,"tools":[{"type":"code_interpreter"}]`, `,"previous_response_id":"resp_old"`, `,"input":[{"type":"reasoning","encrypted_content":"opaque"}]`} {
		input := `,"input":"fake"`
		if strings.Contains(extra, `"input"`) {
			input = ""
		}
		_, err := s.Prepare(t.Context(), j, key, user, account, nil, []byte(`{"model":"gpt-5.1","background":true,"max_output_tokens":100`+input+extra+`}`), time.Now())
		require.Error(t, err)
	}
	ctx := WithAIRoutingExecutionRequirements(t.Context(), AIRoutingExecutionRequirements{Endpoint: CompositeRouteEndpointResponses, RequiresStoredResponses: true, ContextWindowTokens: 10000, InputBytes: 1, MaxOutputTokens: 100, CandidateMaxOutputTokens: 100, InputModalities: []string{"text"}})
	saved, err := s.Prepare(ctx, j, key, user, account, nil, []byte(`{"model":"gpt-5.1","background":true,"max_output_tokens":100,"previous_response_id":"resp_old"}`), time.Now())
	require.NoError(t, err)
	require.Positive(t, saved.HoldAmount)
}

func TestAIRoutingBackgroundTerminalUsageIsAuthoritativeOrUnknown(t *testing.T) {
	for _, tc := range []struct {
		name, body  string
		known, fail bool
	}{
		{"zero", `{"usage":{"input_tokens":0,"output_tokens":0}}`, true, false},
		{"zero aliases agree", `{"usage":{"input_tokens":0,"prompt_tokens":0,"output_tokens":0,"completion_tokens":0}}`, true, false},
		{"zero output alias conflict", `{"usage":{"input_tokens":10,"output_tokens":0,"completion_tokens":500}}`, false, false},
		{"zero input alias conflict", `{"usage":{"input_tokens":0,"prompt_tokens":500,"output_tokens":0}}`, false, false},
		{"nonzero output alias conflict", `{"usage":{"input_tokens":10,"output_tokens":3,"completion_tokens":500}}`, false, false},
		{"cache aliases agree", `{"usage":{"input_tokens":10,"prompt_tokens":10,"output_tokens":3,"completion_tokens":3,"input_tokens_details":{"cached_tokens":4,"cache_write_tokens":2},"cached_tokens":4,"cache_creation_input_tokens":2}}`, true, false},
		{"cache zero alias conflict", `{"usage":{"input_tokens":10,"output_tokens":3,"input_tokens_details":{"cached_tokens":0},"cached_tokens":4}}`, false, false},
		{"cache nonzero alias conflict", `{"usage":{"input_tokens":10,"output_tokens":3,"input_tokens_details":{"cached_tokens":2},"cache_read_input_tokens":4}}`, false, false},
		{"cache write alias conflict", `{"usage":{"input_tokens":10,"output_tokens":3,"input_tokens_details":{"cache_write_tokens":2},"cache_creation_tokens":4}}`, false, false},
		{"image aliases conflict", `{"usage":{"input_tokens":10,"output_tokens":3,"input_tokens_details":{"image_tokens":0},"prompt_tokens_details":{"image_tokens":4}}}`, false, false},
		{"independent reasoning unknown", `{"usage":{"input_tokens":10,"output_tokens":0,"total_tokens":15,"output_tokens_details":{"reasoning_tokens":5}}}`, false, false},
		{"absent", `{}`, false, false},
		{"partial", `{"usage":{"input_tokens":2}}`, false, false},
		{"bad integer", `{"usage":{"input_tokens":1.2,"output_tokens":2}}`, false, false},
		{"cache", `{"usage":{"input_tokens":10,"output_tokens":3,"input_tokens_details":{"cached_tokens":4,"cache_write_tokens":2}}}`, true, false},
		{"too much cache", `{"usage":{"input_tokens":2,"output_tokens":3,"input_tokens_details":{"cached_tokens":4}}}`, false, false},
		{"hosted fees", `{"usage":{"input_tokens":2,"output_tokens":3},"output":[{"type":"web_search_call"}]}`, false, false},
		{"protocol case", `{"usage":{"input_tokens":2,"Input_tokens":0,"output_tokens":3}}`, false, true},
		{"duplicate", `{"usage":{"input_tokens":2,"input_tokens":0,"output_tokens":3}}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"id":"resp_A","status":"failed","model":"gpt-5.1",` + strings.TrimPrefix(tc.body, "{")
			if tc.body == `{}` {
				body = `{"id":"resp_A","status":"failed","model":"gpt-5.1"}`
			}
			terminal, done, err := parseAIBackgroundTerminal([]byte(body), "resp_A")
			if tc.fail {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.True(t, done)
			require.Equal(t, tc.known, terminal.UsageKnown)
			if tc.name == "zero" || tc.name == "zero aliases agree" {
				require.Zero(t, terminal.InputTokens)
				require.Zero(t, terminal.OutputTokens)
			}
		})
	}
}

type backgroundSyncCache struct {
	BillingCache
	fail  bool
	calls int
}

func (c *backgroundSyncCache) InvalidateUserBalance(context.Context, int64) error {
	c.calls++
	if c.fail {
		return errors.New("synthetic cache failure")
	}
	return nil
}
func (c *backgroundSyncCache) InvalidateSubscriptionCache(context.Context, int64, int64) error {
	return nil
}
func (c *backgroundSyncCache) InvalidateAPIKeyRateLimit(context.Context, int64) error { return nil }

func TestAIRoutingBackgroundSyncReturnsCacheAndUsageFailures(t *testing.T) {
	s, j, key, user, account := backgroundPriceFixture(t)
	saved, err := s.Prepare(t.Context(), j, key, user, account, nil, []byte(`{"model":"gpt-5.1","background":true,"input":"fake","max_output_tokens":100}`), time.Now())
	require.NoError(t, err)
	cache := &backgroundSyncCache{fail: true}
	s.gateway.billingCacheService = &BillingCacheService{cache: cache}
	require.Error(t, s.sync(t.Context(), saved))
	require.False(t, saved.CacheSynced)
	cache.fail = false
	require.NoError(t, s.sync(t.Context(), saved))
	saved.Status = AIBackgroundSettled
	saved.Terminal = &AIRoutingBackgroundTerminal{Status: "completed", UsageKnown: true, Model: "gpt-5.1", InputTokens: 5, OutputTokens: 3}
	cmd, err := s.buildCommand(t.Context(), saved)
	require.NoError(t, err)
	saved.BillingCommand = cmd
	logs := &openAIRecordUsageLogRepoStub{err: errors.New("synthetic database failure")}
	s.gateway.usageLogRepo = logs
	require.Error(t, s.sync(t.Context(), saved))
	logs.err = nil
	require.NoError(t, s.sync(t.Context(), saved))
	require.Equal(t, AIRoutingBackgroundBillingRequestID(saved.ID), logs.lastLog.RequestID)
}

func TestAIRoutingBackgroundFetchUsesRealLocalHTTPAndOriginalAccount(t *testing.T) {
	s, j, key, user, account := backgroundPriceFixture(t)
	var gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/v1/responses/resp_stored", r.URL.Path)
		require.Equal(t, "Bearer FAKE_SECRET_DO_NOT_PERSIST", r.Header.Get("Authorization"))
		gets.Add(1)
		_, _ = w.Write([]byte(`{"id":"resp_stored","status":"cancelled","model":"gpt-5.1","usage":{"input_tokens":5,"output_tokens":3},"output":[]}`))
	}))
	defer server.Close()
	account.Credentials["base_url"] = server.URL
	j, err := s.Prepare(t.Context(), j, key, user, account, nil, []byte(`{"model":"gpt-5.1","background":true,"input":"fake","max_output_tokens":100}`), time.Now())
	require.NoError(t, err)
	j.ResponseID = "resp_stored"
	account.Status = "disabled"
	s.gateway.cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	s.gateway.accountRepo = &openAIRecordUsageAccountRepoStub{account: account}
	s.gateway.httpUpstream = &aiControlHTTP{do: func(r *http.Request, _ string, id int64, _ int) (*http.Response, error) {
		require.Equal(t, account.ID, id)
		require.True(t, HTTPUpstreamRedirectsDisabled(r.Context()))
		return server.Client().Do(r)
	}}
	s.gateway.billingCacheService = &BillingCacheService{}
	s.gateway.usageLogRepo = &openAIRecordUsageLogRepoStub{}
	s.repository = &backgroundPriceRepoMarker{}
	terminal, done, err := s.fetch(t.Context(), j)
	require.NoError(t, err)
	require.True(t, done)
	require.True(t, terminal.UsageKnown)
	require.Equal(t, 5, terminal.InputTokens)
	account.Credentials["base_url"] = server.URL + "/other"
	_, _, err = s.fetch(t.Context(), j)
	require.ErrorIs(t, err, ErrAIBackgroundRecoveryPending)
	require.EqualValues(t, 1, gets.Load(), "changed endpoint must not receive the old response identifier")
}

type backgroundPriceRepoMarker struct{ AIRoutingBackgroundRepository }
