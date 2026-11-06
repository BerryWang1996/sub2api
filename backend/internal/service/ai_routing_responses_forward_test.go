//go:build unit

package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// This fake replaces only the network. Like the repository HTTP transport, it
// executes the request-local final guard immediately before the simulated send.
// The public forwarding entrypoints still build and convert the real wire body.
type aiResponsesGuardedUpstream struct {
	httpUpstreamRecorder
	t       *testing.T
	account *Account
	model   string
	checks  []AIRoutingCapabilityCheck
}

func (u *aiResponsesGuardedUpstream) Do(req *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
	u.t.Helper()
	require.True(u.t, HasAIRoutingOutboundGuard(req.Context()), "conversion must retain the final admission guard")
	require.Equal(u.t, u.model, AIRoutingBillingModel(req.Context()))
	scope, ok := CompositeRouteScopeFromContext(req.Context())
	require.True(u.t, ok)
	require.Equal(u.t, "client-alias-with-no-model-semantics", scope.PublicModel)
	require.Equal(u.t, u.model, scope.ExecutionModel)
	u.checks = append(u.checks, CheckAIRoutingFinalPayload(req.Context(), u.account, u.model, req))
	if err := CheckAIRoutingOutboundRequest(req, id); err != nil {
		return nil, err
	}
	return u.httpUpstreamRecorder.Do(req, proxy, id, concurrency)
}

func (u *aiResponsesGuardedUpstream) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, proxy, id, concurrency)
}

type aiResponsesForwardResult struct {
	model, upstreamModel, requestID string
	input, output                   int
}

func aiResponsesForwardFixture(t *testing.T, kind string, stream bool, window int64) (*aiResponsesGuardedUpstream, *Account, context.Context, []byte) {
	t.Helper()
	account, model := rawChatCompletionsTestAccount(), "gpt-5.1"
	contentType := "application/json"
	response := `{"id":"resp_ai","object":"response","model":"gpt-5.1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}],"status":"completed"}],"usage":{"input_tokens":5,"output_tokens":2}}`
	switch kind {
	case "openai-native":
		account.Extra = map[string]any{"openai_responses_supported": true}
		if stream {
			contentType, response = "text/event-stream", aiExecutionResponsesToolSSE(false)
		}
	case "openai-chat":
		account = forceChatResponsesFallbackAccount()
		response = aiExecutionChatToolResponse(stream)
		if stream {
			contentType = "text/event-stream"
		}
	case "anthropic":
		model = "claude-sonnet-4-5-20250929"
		account = &Account{ID: 102, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Concurrency: 1, Credentials: map[string]any{"api_key": "fake-key", "base_url": "http://upstream.example"}}
		contentType, response = "text/event-stream", namespaceToolAnthropicStream()
	case "antigravity":
		model, account = "gemini-3.1-pro-high", newAntigravityCompatAccount(AccountTypeOAuth)
		contentType = "text/event-stream"
		response = `data: {"response":{"responseId":"resp_ai","candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3}}}` + "\n\n"
	case "gemini":
		model = "gemini-2.5-pro"
		account = &Account{ID: 103, Platform: PlatformGemini, Type: AccountTypeAPIKey, Concurrency: 1, Credentials: map[string]any{"api_key": "fake-key", "base_url": "http://upstream.example"}}
		response = `{"responseId":"resp_ai","modelVersion":"gemini-2.5-pro","candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3}}`
		if stream {
			contentType, response = "text/event-stream", "data: "+response+"\n\n"
		}
	default:
		t.Fatalf("unimplemented real adapter: %s", kind)
	}
	account.Status, account.Schedulable = StatusActive, true
	account.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{Source: "synthetic-fixture", Models: map[string]UpstreamModelMetadata{model: {ID: model, ContextWindow: 500000, MaxOutputTokens: 8192, InputModalities: []string{"text"}}}})
	body := []byte(fmt.Sprintf(`{"model":%q,"instructions":"full-system-evidence","input":[{"role":"user","content":"full-first-turn-evidence"},{"role":"assistant","content":"full-assistant-history-evidence"},{"role":"user","content":"full-current-turn-evidence"}],"max_output_tokens":64,"store":false,"stream":%t}`, model, stream))
	ctx := WithCompositeRouteDecision(t.Context(), CompositeRouteDecision{Matched: true, GroupID: 9, PublicModel: "client-alias-with-no-model-semantics", UpstreamModel: model, Source: CompositeRouteSourceAI, AICandidateID: "selected", TargetAccountID: &account.ID, TargetPlatform: account.Platform})
	ctx = WithAIRoutingExecutionRequirements(ctx, AIRoutingExecutionRequirements{Endpoint: CompositeRouteEndpointResponses, InputModalities: []string{"text"}, InputBytes: int64(len(body)), MaxOutputTokens: 64, ContextWindowTokens: window})
	guardService := &GatewayService{accountRepo: &aiOutboundAccountRepo{account: account}}
	ctx = guardService.WithAIRoutingFinalCapacityGuard(ctx)
	upstream := &aiResponsesGuardedUpstream{t: t, account: account, model: model, httpUpstreamRecorder: httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{contentType}, "X-Request-Id": []string{"ai-responses-wire-id"}}, Body: io.NopCloser(strings.NewReader(response))}}}
	return upstream, account, ctx, body
}

func aiResponsesForward(t *testing.T, kind string, upstream *aiResponsesGuardedUpstream, account *Account, ctx context.Context, body []byte) (*aiResponsesForwardResult, string, error) {
	t.Helper()
	c, recorder := newAntigravityCompatContext(http.MethodPost, "/v1/responses", body)
	c.Request = c.Request.WithContext(ctx)
	if kind == "openai-native" || kind == "openai-chat" {
		svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
		result, err := svc.Forward(ctx, c, account, body)
		if result == nil {
			return nil, recorder.Body.String(), err
		}
		return &aiResponsesForwardResult{result.Model, result.UpstreamModel, result.RequestID, result.Usage.InputTokens, result.Usage.OutputTokens}, recorder.Body.String(), err
	}
	var result *ForwardResult
	var err error
	switch kind {
	case "antigravity":
		svc := newAntigravityCompatService(config.GatewayConfig{MaxLineSize: defaultMaxLineSize}, upstream)
		result, err = svc.ForwardAsResponses(ctx, c, account, body, nil)
	case "gemini":
		svc := &GeminiMessagesCompatService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
		result, err = svc.ForwardAsResponses(ctx, c, account, body, nil)
	default:
		svc := &GatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
		result, err = svc.ForwardAsResponses(ctx, c, account, body, nil)
	}
	if result == nil {
		return nil, recorder.Body.String(), err
	}
	return &aiResponsesForwardResult{result.Model, result.UpstreamModel, result.RequestID, result.Usage.InputTokens, result.Usage.OutputTokens}, recorder.Body.String(), err
}

func TestAIRoutingResponsesRealForwardingPreservesExecutionIdentityAndFullContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, kind := range []string{"openai-native", "openai-chat", "anthropic", "antigravity", "gemini"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", kind, stream), func(t *testing.T) {
				upstream, account, ctx, body := aiResponsesForwardFixture(t, kind, stream, 500000)
				result, clientBody, err := aiResponsesForward(t, kind, upstream, account, ctx, body)
				require.NoError(t, err, clientBody)
				require.NotNil(t, result)
				require.Len(t, upstream.requests, 1, "one real adapter request, with no replay")
				require.Len(t, upstream.checks, 1)
				check := upstream.checks[0]
				require.True(t, check.Allowed, check.Reason)
				require.True(t, check.FinalPayloadChecked)
				require.False(t, check.CapacityVerified, "byte budgeting is not provider token verification")
				require.EqualValues(t, len(upstream.lastBody), check.InputBytes)
				paths := map[string]string{"openai-native": "/v1/responses", "openai-chat": "/v1/chat/completions", "anthropic": "/v1/messages", "antigravity": "/v1internal:streamGenerateContent"}
				paths["gemini"] = "/v1beta/models/gemini-2.5-pro:generateContent"
				if stream {
					paths["gemini"] = "/v1beta/models/gemini-2.5-pro:streamGenerateContent"
				}
				require.Equal(t, paths[kind], upstream.lastReq.URL.Path)
				switch kind {
				case "openai-native":
					require.True(t, gjson.GetBytes(upstream.lastBody, "input").IsArray())
				case "openai-chat", "anthropic":
					require.True(t, gjson.GetBytes(upstream.lastBody, "messages").IsArray())
					require.False(t, gjson.GetBytes(upstream.lastBody, "input").Exists())
				case "antigravity":
					require.True(t, gjson.GetBytes(upstream.lastBody, "request.contents").IsArray())
				case "gemini":
					require.True(t, gjson.GetBytes(upstream.lastBody, "contents").IsArray())
				}
				for _, marker := range []string{"full-system-evidence", "full-first-turn-evidence", "full-assistant-history-evidence", "full-current-turn-evidence"} {
					require.Contains(t, string(upstream.lastBody), marker, "decision projection cannot replace execution history")
				}
				require.NotContains(t, string(upstream.lastBody), "client-alias-with-no-model-semantics")
				if kind != "gemini" {
					require.Equal(t, upstream.model, gjson.GetBytes(upstream.lastBody, "model").String())
				}
				require.Equal(t, upstream.model, result.upstreamModel)
				require.Equal(t, upstream.model, result.model)
				require.NotEmpty(t, result.requestID)
				require.Positive(t, result.input)
				require.Positive(t, result.output)
				if stream {
					require.Contains(t, clientBody, "response.completed")
				} else {
					require.Equal(t, "response", gjson.Get(clientBody, "object").String())
				}
				billingCtx := CopyAIRoutingBillingModel(context.Background(), ctx)
				require.Equal(t, upstream.model, AIRoutingBillingModel(billingCtx))
				_, grant := CompositeRouteScopeFromContext(billingCtx)
				require.False(t, grant, "async billing inherits the selected model, not a routing authorization")
			})
		}
	}
}

func TestAIRoutingResponsesRealForwardingFinalGuardRejectsConvertedCapacity(t *testing.T) {
	for _, kind := range []string{"openai-native", "openai-chat", "anthropic", "antigravity", "gemini"} {
		t.Run(kind, func(t *testing.T) {
			upstream, account, ctx, body := aiResponsesForwardFixture(t, kind, false, 65)
			// Deliberately supply a smaller pre-conversion estimate. The guard
			// must independently budget the actual converted bytes before send.
			r, _ := AIRoutingExecutionRequirementsFromContext(ctx)
			r.InputBytes = 1
			ctx = WithAIRoutingExecutionRequirements(ctx, r)
			ctx = (&GatewayService{accountRepo: &aiOutboundAccountRepo{account: account}}).WithAIRoutingFinalCapacityGuard(ctx)
			result, _, err := aiResponsesForward(t, kind, upstream, account, ctx, body)
			var refused *AIRoutingCapacityError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, "execution_context_window_exceeded", refused.Check.Reason)
			require.Nil(t, result)
			require.Empty(t, upstream.requests, "capacity refusal happens before network send")
		})
	}
}

func TestAIRoutingResponsesRealForwardingRejectsRemappedExecutionModel(t *testing.T) {
	for _, kind := range []string{"openai-native", "openai-chat", "anthropic", "antigravity", "gemini"} {
		t.Run(kind, func(t *testing.T) {
			upstream, account, ctx, body := aiResponsesForwardFixture(t, kind, false, 500000)
			account.Credentials["model_mapping"] = map[string]any{upstream.model: "gpt-5.4"}
			_, _, err := aiResponsesForward(t, kind, upstream, account, ctx, body)
			require.Error(t, err)
			require.Empty(t, upstream.requests, "a late account mapping cannot silently replace the chosen model")
		})
	}
}

func TestAIRoutingResponsesRealForwardingBillingKeepsAliasAndSelectedModel(t *testing.T) {
	for _, kind := range []string{"openai-native", "openai-chat", "anthropic", "antigravity", "gemini"} {
		t.Run(kind, func(t *testing.T) {
			upstream, account, ctx, body := aiResponsesForwardFixture(t, kind, false, 500000)
			result, _, err := aiResponsesForward(t, kind, upstream, account, ctx, body)
			require.NoError(t, err)
			scope, ok := CompositeRouteScopeFromContext(ctx)
			require.True(t, ok)
			billingCtx := CopyAIRoutingBillingModel(context.Background(), ctx)
			usageRepo := &openAIRecordUsageLogRepoStub{inserted: true}
			userRepo := &openAIRecordUsageUserRepoStub{}
			// Simulate the independently captured handler fields at the async
			// accounting boundary, including an unrelated reported/mapped model.
			fields := ChannelUsageFields{OriginalModel: scope.PublicModel, ChannelMappedModel: "gpt-5.4", BillingModelSource: BillingModelSourceResponse}
			key, user := &APIKey{ID: 201}, &User{ID: 202}
			var expected *CostBreakdown
			if kind == "openai-native" || kind == "openai-chat" {
				svc := newOpenAIRecordUsageServiceForTest(usageRepo, userRepo, &openAIRecordUsageSubRepoStub{}, nil)
				usage := OpenAIUsage{InputTokens: result.input, OutputTokens: result.output}
				expected = expectedOpenAICost(t, svc, upstream.model, usage, 1.1)
				err = svc.RecordUsage(billingCtx, &OpenAIRecordUsageInput{Result: &OpenAIForwardResult{RequestID: result.requestID, Model: result.model, UpstreamModel: result.upstreamModel, BillingModel: "gpt-5.4", UpstreamResponseModel: "gpt-5.4", Usage: usage, Duration: time.Second}, APIKey: key, User: user, Account: account, ChannelUsageFields: fields})
			} else {
				svc := newGatewayRecordUsageServiceForTest(usageRepo, userRepo, &openAIRecordUsageSubRepoStub{})
				if kind == "gemini" {
					// The offline fallback catalog need not contain this historical
					// model. Use an explicit synthetic local price, never discovery.
					svc.billingService = NewBillingService(svc.cfg, &PricingService{pricingData: map[string]*LiteLLMModelPricing{
						upstream.model: {InputCostPerToken: 2e-6, OutputCostPerToken: 8e-6},
					}})
					svc.resolver = NewModelPricingResolver(nil, svc.billingService)
				}
				expected, err = svc.billingService.CalculateCost(upstream.model, UsageTokens{InputTokens: result.input, OutputTokens: result.output}, 1.1)
				require.NoError(t, err)
				err = svc.RecordUsage(billingCtx, &RecordUsageInput{Result: &ForwardResult{RequestID: result.requestID, Model: result.model, UpstreamModel: result.upstreamModel, UpstreamResponseModel: "gpt-5.4", Usage: ClaudeUsage{InputTokens: result.input, OutputTokens: result.output}, Duration: time.Second}, APIKey: key, User: user, Account: account, ChannelUsageFields: fields})
			}
			require.NoError(t, err)
			require.NotNil(t, usageRepo.lastLog)
			require.Equal(t, scope.PublicModel, usageRepo.lastLog.RequestedModel)
			require.Equal(t, upstream.model, usageRepo.lastLog.Model)
			require.NotNil(t, usageRepo.lastLog.UpstreamModel)
			require.Equal(t, upstream.model, *usageRepo.lastLog.UpstreamModel)
			require.Equal(t, result.requestID, usageRepo.lastLog.RequestID)
			require.Positive(t, expected.ActualCost)
			require.InDelta(t, expected.ActualCost, usageRepo.lastLog.ActualCost, 1e-12)
			require.InDelta(t, expected.ActualCost, userRepo.lastAmount, 1e-12)
		})
	}
}
