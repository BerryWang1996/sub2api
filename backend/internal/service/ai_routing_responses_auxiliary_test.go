//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAIRoutingResponsesAuxiliaryClassificationAndParsing(t *testing.T) {
	for _, base := range []string{"/v1/responses", "/responses", "/backend-api/codex/responses"} {
		require.Equal(t, AIRoutingResponsesInputTokens, AIRoutingResponsesAuxiliaryKind(nil, base+"/input_tokens"))
		require.Equal(t, AIRoutingResponsesCompact, AIRoutingResponsesAuxiliaryKind(nil, base+"/compact"))
		require.Empty(t, AIRoutingResponsesAuxiliaryKind([]byte(`{"context_management":[{"type":"compaction"}]}`), base))
		for _, stream := range []string{"false", "true"} {
			body := []byte(`{"model":"gpt-5.1","store":false,"stream":` + stream + `,"input":[{"role":"user","content":"hi"},{"type":"compaction_trigger"}]}`)
			kind := AIRoutingResponsesCompact
			if stream == "true" {
				kind = AIRoutingResponsesCompactV2
			}
			require.Equal(t, kind, AIRoutingResponsesAuxiliaryKind(body, base))
			r, err := NewAIRoutingResponsesAuxiliaryRequest(body, "bounded_history", kind)
			require.NoError(t, err)
			require.Equal(t, kind, r.Operation)
			require.True(t, r.RequiresNativeResponses)
			require.False(t, r.OutputLimitOmitted)
			require.Zero(t, r.MaxOutput)
		}
	}
	for _, kind := range []string{AIRoutingResponsesInputTokens, AIRoutingResponsesCompact} {
		r, err := NewAIRoutingResponsesAuxiliaryRequest([]byte(`{"model":"gpt-5.1","previous_response_id":"resp_prior","input":"hi"}`), "metadata", kind)
		require.NoError(t, err)
		require.False(t, r.StoreResponse)
		require.True(t, r.RequiresStoredResponses)
		require.True(t, r.OriginRefs[0].RequiresStored)
		_, err = NewAIRoutingResponsesAuxiliaryRequest([]byte(`{"model":"gpt-5.1","store":false,"store":true,"input":"hi"}`), "metadata", kind)
		require.Error(t, err)
		for _, option := range []string{`"store":true`, `"stream":true`, `"max_output_tokens":1`, `"background":true`} {
			_, err = NewAIRoutingResponsesAuxiliaryRequest([]byte(`{"model":"gpt-5.1","input":"hi",`+option+`}`), "metadata", kind)
			require.Error(t, err, option)
		}
	}
}

func TestAIRoutingResponsesAuxiliaryCapacityHasNoGenerationReserve(t *testing.T) {
	account := aiCapabilityAccount()
	body := `{"model":"gpt-5.1","input":"hi"}`
	for _, operation := range []string{AIRoutingResponsesCompact, AIRoutingResponsesInputTokens} {
		r := AIRoutingExecutionRequirements{Operation: operation, RequiresNativeResponses: true, InputBytes: 1, InputModalities: []string{"text"}, ContextWindowTokens: int64(len(body))}
		ctx := WithAIRoutingExecutionRequirements(t.Context(), r)
		check := CheckAIRoutingFinalPayload(ctx, account, "gpt-5.1", aiFinalTestRequest(t, "/v1/responses/"+operation, body))
		require.True(t, check.Allowed, check.Reason)
		require.True(t, check.FinalPayloadChecked)
		require.False(t, check.CapacityVerified)
		require.Zero(t, check.OutputReserveTokens)
		r.ContextWindowTokens = 1
		check = CheckAIRoutingFinalPayload(WithAIRoutingExecutionRequirements(ctx, r), account, "gpt-5.1", aiFinalTestRequest(t, "/v1/responses/"+operation, body))
		require.Equal(t, operation == AIRoutingResponsesInputTokens, check.Allowed)
		check = CheckAIRoutingFinalPayload(ctx, account, "gpt-5.1", aiFinalTestRequest(t, "/v1/responses", body))
		require.Equal(t, "execution_final_protocol_unknown", check.Reason, "auxiliary must not bypass a generation reserve on create")
	}
}

func TestAIRoutingResponsesAuxiliaryCountSourcesAndHiddenHistory(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		hidden      bool
	}{
		{"visible", `"input":"hello"`, false},
		{"previous", `"previous_response_id":"resp_a","input":"hello"`, true},
		{"item", `"input":[{"type":"item_reference","id":"item_a"}]`, true},
		{"opaque", `"input":[{"type":"compaction","encrypted_content":"fake"}]`, true},
		{"media", `"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://synthetic.invalid/img"}]}]`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"gpt-5.1",` + tc.input + `}`)
			ctx := WithAIRoutingExecutionRequirements(t.Context(), AIRoutingExecutionRequirements{Operation: AIRoutingResponsesInputTokens})
			calls := 0
			ctx = WithAIRoutingOutboundGuard(ctx, func(*http.Request, int64) error { calls++; return nil })
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/input_tokens", strings.NewReader(string(body))).WithContext(ctx)
			upstream := &httpUpstreamRecorder{}
			svc := &OpenAIGatewayService{httpUpstream: upstream}
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": "https://synthetic.invalid", "api_key": "fake"}}
			require.NoError(t, svc.ForwardResponsesInputTokens(ctx, c, account, body))
			require.Equal(t, 1, calls)
			require.Nil(t, upstream.lastReq)
			if tc.hidden {
				require.Equal(t, 422, rec.Code)
				require.Equal(t, "unsupported", gjson.Get(rec.Body.String(), "count_source").Str)
				require.False(t, gjson.Get(rec.Body.String(), "input_tokens").Exists())
			} else {
				require.Equal(t, 200, rec.Code)
				require.Equal(t, "local_estimated", gjson.Get(rec.Body.String(), "count_source").Str)
				require.True(t, gjson.Get(rec.Body.String(), "estimated").Bool())
			}
		})
	}
	for _, upstreamBody := range []string{`{"object":"response.input_tokens","input_tokens":42,"id":"unbound_response"}`, `{"input_tokens":-1}`, `{"input_tokens":1,"input_tokens":2}`} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		ctx := WithAIRoutingExecutionRequirements(t.Context(), AIRoutingExecutionRequirements{Operation: AIRoutingResponsesInputTokens})
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/input_tokens", nil).WithContext(ctx)
		upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(upstreamBody))}}
		svc := &OpenAIGatewayService{httpUpstream: upstream}
		account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fake"}}
		err := svc.ForwardResponsesInputTokens(ctx, c, account, []byte(`{"model":"gpt-5.1","input":"hi"}`))
		if strings.Contains(upstreamBody, `"object"`) {
			require.NoError(t, err)
			require.Equal(t, "upstream", gjson.Get(rec.Body.String(), "count_source").Str)
			require.False(t, gjson.Get(rec.Body.String(), "estimated").Bool())
			require.NotContains(t, rec.Body.String(), "unbound_response")
		} else {
			require.Error(t, err)
			require.Equal(t, 502, rec.Code)
		}
	}
}

func TestAIRoutingResponsesAuxiliaryLocalCountRevokedAndLegacyFallback(t *testing.T) {
	body := []byte(`{"model":"gpt-5.1","input":"hello"}`)
	for _, guarded := range []bool{false, true} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		ctx := context.Background()
		if guarded {
			ctx = WithAIRoutingExecutionRequirements(ctx, AIRoutingExecutionRequirements{Operation: AIRoutingResponsesInputTokens})
			ctx = WithAIRoutingOutboundGuard(ctx, func(*http.Request, int64) error {
				return &AIRoutingCapacityError{Check: AIRoutingCapabilityCheck{Reason: "execution_policy_changed"}}
			})
		}
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/input_tokens", nil).WithContext(ctx)
		account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{"base_url": "https://synthetic.invalid"}}
		err := (&OpenAIGatewayService{}).ForwardResponsesInputTokens(ctx, c, account, body)
		if guarded {
			require.Error(t, err)
			require.True(t, IsAIRoutingCapacityError(err))
			require.NotContains(t, rec.Body.String(), `"input_tokens"`)
		} else {
			require.NoError(t, err)
			require.Equal(t, 200, rec.Code)
			require.False(t, gjson.Get(rec.Body.String(), "count_source").Exists())
		}
	}
	for _, operation := range []string{"", AIRoutingResponsesCompact, AIRoutingResponsesCompactV2} {
		c := newOpenAICompactFallbackTestContext(t, "/v1/responses/compact")
		if operation != "" {
			c.Request = c.Request.WithContext(WithAIRoutingExecutionRequirements(c.Request.Context(), AIRoutingExecutionRequirements{Operation: operation}))
		}
		svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{OpenAICompactModel: "gpt-5.4"}}}
		_, _, retry := svc.prepareOpenAICompactFallbackRetry(c, nil, "gpt-5.1", body, 400, "context window exceeded", []byte(`{"error":{"code":"context_length_exceeded"}}`), false)
		require.Equal(t, operation == "", retry)
	}
}

func TestAIRoutingResponsesAuxiliaryCompactKeepsChosenModelOnFirstSend(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		body := []byte(`{"model":"gpt-5.1","instructions":"compact-test","input":[]}`)
		c := newOpenAICompactFallbackTestContext(t, "/v1/responses/compact")
		ctx := WithAIRoutingExecutionRequirements(t.Context(), AIRoutingExecutionRequirements{Operation: AIRoutingResponsesCompact})
		c.Request = c.Request.WithContext(ctx)
		upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"cmp_a","object":"response.compaction","model":"gpt-5.1","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`))}}
		svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{OpenAICompactModel: "global-compact"}}, httpUpstream: upstream}
		account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1, Status: StatusActive, Schedulable: true, Credentials: map[string]any{"api_key": "fake", "compact_model_mapping": map[string]any{"gpt-5.1": "account-compact"}}}
		if passthrough {
			account.Extra = map[string]any{"openai_passthrough": true}
		}
		_, err := svc.Forward(ctx, c, account, body)
		require.NoError(t, err)
		require.Len(t, upstream.bodies, 1)
		require.Equal(t, "gpt-5.1", gjson.GetBytes(upstream.bodies[0], "model").Str)
	}
}
