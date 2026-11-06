//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestAIRoutingTransportCapacityIsLocal(t *testing.T) {
	for _, kind := range []string{"anthropic", "openai", "gemini", "antigravity"} {
		t.Run(kind, func(t *testing.T) {
			c := newTransportErrorTestGin(t)
			repo := &transportTempUnschedRepoStub{}
			// Network-like wording must never turn a typed local refusal into an
			// upstream health update or an account-switch signal.
			denied := &AIRoutingCapacityError{Check: AIRoutingCapabilityCheck{Reason: "connection refused"}}
			account := &Account{ID: 41, Platform: PlatformAnthropic}
			var err error
			switch kind {
			case "anthropic":
				err = (&GatewayService{accountRepo: repo}).handleUpstreamTransportError(c.Request.Context(), c, account, denied, OpsUpstreamErrorEvent{})
			case "openai":
				err = (&OpenAIGatewayService{accountRepo: repo}).handleOpenAIUpstreamTransportError(c.Request.Context(), c, account, denied, false)
			case "gemini":
				err = (&GeminiMessagesCompatService{accountRepo: repo}).handleUpstreamTransportError(c.Request.Context(), c, account, denied)
			case "antigravity":
				err = (&AntigravityGatewayService{}).handleAntigravityCompatTransportError(c, denied)
			}
			require.Same(t, denied, err)
			var failover *UpstreamFailoverError
			require.False(t, errors.As(err, &failover))
			require.Zero(t, repo.calls)
			require.Equal(t, http.StatusBadRequest, c.Writer.Status())
			require.True(t, IsResponseCommitted(c))
			require.True(t, c.IsAborted())
		})
	}
}

func TestAIRoutingTransportCapacityBeforePlugin(t *testing.T) {
	manager := &PluginManager{}
	manager.route.Store(&pluginRoute{pluginID: 1, rolloutPercent: 100, unavailable: "must not enter plugin"})
	upstream := &pluginRoutingHTTPUpstream{}
	svc := &OpenAIGatewayService{pluginManager: manager, httpUpstream: upstream}
	denied := &AIRoutingCapacityError{Check: AIRoutingCapabilityCheck{Reason: "execution_context_exceeded"}}
	checks := 0
	ctx := WithAIRoutingOutboundGuard(t.Context(), func(_ *http.Request, accountID int64) error {
		checks++
		require.Equal(t, int64(41), accountID)
		return denied
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://example.invalid/v1/responses", strings.NewReader(`{"model":"final"}`))
	require.NoError(t, err)
	resp, err := svc.doOpenAIUpstream(req, "", &Account{ID: 41, Platform: PlatformOpenAI, Type: AccountTypeOAuth})
	require.Same(t, denied, err)
	require.Nil(t, resp)
	require.Equal(t, 1, checks)
	require.Zero(t, upstream.doCalls)
	require.NoError(t, req.Body.Close())
}

func TestAIRoutingTransportCapacityWebSearchCannotBypass(t *testing.T) {
	c := newTransportErrorTestGin(t)
	ctx := WithAIRoutingExecutionRequirements(c.Request.Context(), AIRoutingExecutionRequirements{})
	// A nil account proves no search provider or account credential is touched.
	result, err := writeEmulatedWebSearch(ctx, c, nil, []byte(`{}`), "public", false)
	require.Nil(t, result)
	var denied *AIRoutingCapacityError
	require.ErrorAs(t, err, &denied)
	require.Equal(t, "execution_capacity_unknown", denied.Check.Reason)
	require.Equal(t, http.StatusBadRequest, c.Writer.Status())
	legacy := newTransportErrorTestGin(t)
	_, err = writeEmulatedWebSearch(legacy.Request.Context(), legacy, nil, []byte(`{}`), "public", false)
	require.ErrorContains(t, err, "no query found")
	require.False(t, IsAIRoutingCapacityError(err))
	require.False(t, legacy.Writer.Written())
}

func TestAIRoutingTransportCapacityStopsAnthropicBudgetRetry(t *testing.T) {
	c := newTransportErrorTestGin(t)
	denied := &AIRoutingCapacityError{Check: AIRoutingCapabilityCheck{Reason: "execution_output_limit_exceeded"}}
	upstream := &queuedHTTPUpstreamStub{
		responses: []*http.Response{{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"thinking.budget_tokens must be greater than or equal to 1024"}}`))}, nil},
		errors:    []error{nil, denied},
	}
	cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
	svc := &GatewayService{cfg: cfg, httpUpstream: upstream, settingService: NewSettingService(newMockSettingRepo(), cfg)}
	account := &Account{ID: 41, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fake"}, Concurrency: 1}
	body := []byte(`{"model":"claude-sonnet-4-20250514","max_tokens":1024,"thinking":{"type":"enabled","budget_tokens":512},"messages":[{"role":"user","content":"hello"}]}`)
	parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
	require.NoError(t, err)
	result, err := svc.Forward(context.Background(), c, account, parsed)
	require.Nil(t, result)
	require.Same(t, denied, err)
	require.Equal(t, 2, upstream.callCount)
	require.NotEqual(t, string(upstream.requestBodies[0]), string(upstream.requestBodies[1]))
	require.Equal(t, http.StatusBadRequest, c.Writer.Status())
	require.True(t, IsResponseCommitted(c))
}

func TestAIRoutingTransportCapacityStopsAntigravityRetries(t *testing.T) {
	for _, mode := range []string{"normal", "credits", "smart", "smart credits"} {
		t.Run(mode, func(t *testing.T) {
			c := newTransportErrorTestGin(t)
			denied := &AIRoutingCapacityError{Check: AIRoutingCapabilityCheck{Reason: "execution_context_exceeded"}}
			upstream := &queuedHTTPUpstreamStub{errors: []error{denied}}
			params := antigravityRetryLoopParams{ctx: c.Request.Context(), c: c, account: &Account{ID: 41, Platform: PlatformAntigravity, Type: AccountTypeOAuth}, action: "streamGenerateContent", body: []byte(`{"request":{},"model":"final"}`), httpUpstream: upstream}
			svc := &AntigravityGatewayService{}
			var err error
			if mode == "normal" {
				_, err = svc.antigravityRetryLoop(params)
			} else if mode == "credits" {
				result := svc.attemptCreditsOveragesRetry(params, "https://example.invalid", "final", 0, http.StatusTooManyRequests, nil)
				require.True(t, result.handled)
				err = result.err
			} else {
				body := []byte(`{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","metadata":{"model":"claude-sonnet-4-5"},"reason":"RATE_LIMIT_EXCEEDED"},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"0.001s"}]}}`)
				if mode == "smart credits" {
					params.account.Extra = map[string]any{"allow_overages": true}
					body = []byte(`{"error":{"status":"RESOURCE_EXHAUSTED","message":"QUOTA_EXHAUSTED"}}`)
				}
				response := &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}
				defer func() { require.NoError(t, response.Body.Close()) }()
				result := svc.handleSmartRetry(params, response, body, "https://example.invalid", 0, []string{"https://example.invalid"})
				err = result.err
				require.Equal(t, smartRetryActionBreakWithResp, result.action)
				require.Nil(t, result.switchError)
			}
			require.Same(t, denied, err)
			require.Equal(t, 1, upstream.callCount)
			require.Equal(t, http.StatusBadRequest, c.Writer.Status())
			require.True(t, c.IsAborted())
		})
	}
}
