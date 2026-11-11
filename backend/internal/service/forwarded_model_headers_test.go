//go:build unit

package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newForwardedModelTestContext(path, mode string, enabled bool) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, path, nil)
	if mode != "" {
		SetCompositeRouteDecision(c, CompositeRouteDecision{
			Matched: true, GroupID: 23, TargetPlatform: PlatformOpenAI,
			PublicModel: "public-alias", Source: mode, UpstreamModel: "route-candidate",
		})
	}
	c.Set(forwardedModelExposureKey, enabled)
	return c, rec
}

func TestForwardedModelHeadersScopeAndCommit(t *testing.T) {
	for _, tc := range []struct {
		name, mode, model, wantRoute string
		enabled, heartbeat, clear    bool
	}{
		{name: "composite", mode: "explicit", model: "final", enabled: true, wantRoute: "composite/openai"},
		{name: "AI", mode: CompositeRouteSourceAI, model: "final", enabled: true, wantRoute: "ai/openai"},
		{name: "default off", mode: "explicit", model: "final"},
		{name: "unmatched", model: "final", enabled: true},
		{name: "early heartbeat", mode: "explicit", model: "final", enabled: true, heartbeat: true},
		{name: "rejected buffered output", mode: "explicit", model: "final", enabled: true, clear: true},
		{name: "invalid bytes", mode: "explicit", model: "a\r\nX-Injected: y", enabled: true},
		{name: "overlong", mode: "explicit", model: strings.Repeat("x", 257), enabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := newForwardedModelTestContext("/v1/responses", tc.mode, tc.enabled)
			if tc.heartbeat {
				_, _ = c.Writer.WriteString(":\n\n")
				c.Writer.Flush()
			}
			writeForwardedModelHeaders(c, tc.model)
			if tc.clear {
				ClearUncommittedForwardedModelHeaders(c.Writer)
			}
			_, _ = c.Writer.WriteString("output")
			require.Equal(t, tc.wantRoute, rec.Result().Header.Get(responseheaders.ForwardedRouteHeader))
			wantModel := ""
			if tc.wantRoute != "" {
				wantModel = tc.model
			}
			require.Equal(t, wantModel, rec.Result().Header.Get(responseheaders.ForwardedModelHeader))
		})
	}
}

func TestForwardedModelResponsesMetadataThenFailover(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		name := "normal"
		if passthrough {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			c, rec := newForwardedModelTestContext("/v1/responses", CompositeRouteSourceAI, true)
			svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
			stream := func(body, actualModel string) error {
				resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
				if passthrough {
					_, err := svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "public-alias", actualModel)
					return err
				}
				_, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "public-alias", actualModel)
				return err
			}
			failed := "data: {\"type\":\"codex.rate_limits\",\"rate_limits\":{}}\n\n" +
				"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_is_overloaded\",\"message\":\"Our servers are currently overloaded. Please try again later.\"}}}\n\n"
			var failover *UpstreamFailoverError
			require.ErrorAs(t, stream(failed, "attempt-a"), &failover)
			require.False(t, c.Writer.Written())
			require.Empty(t, c.Writer.Header().Get(responseheaders.ForwardedModelHeader))
			account.ID = 2
			success := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"synthetic\"}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_b\",\"model\":\"attempt-b\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n"
			require.NoError(t, stream(success, "attempt-b"))
			require.Equal(t, "attempt-b", rec.Result().Header.Get(responseheaders.ForwardedModelHeader))
			require.Equal(t, "ai/openai", rec.Result().Header.Get(responseheaders.ForwardedRouteHeader))
			require.Contains(t, rec.Body.String(), "synthetic")
			require.NotContains(t, rec.Body.String(), "codex.rate_limits")
		})
	}
}

func TestForwardedModelMessagesKeepsPublicAlias(t *testing.T) {
	c, rec := newForwardedModelTestContext("/v1/messages", "explicit", true)
	svc := &GatewayService{}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"id":"msg_synthetic","type":"message","model":"wire-model","content":[],"usage":{"input_tokens":4,"output_tokens":2}}`)),
	}
	_, err := svc.handleNonStreamingResponse(c.Request.Context(), resp, c, &Account{ID: 1, Platform: PlatformAnthropic}, "public-alias", "wire-model")
	require.NoError(t, err)
	require.Equal(t, "wire-model", rec.Result().Header.Get(responseheaders.ForwardedModelHeader))
	require.Equal(t, "composite/openai", rec.Result().Header.Get(responseheaders.ForwardedRouteHeader))
	require.Contains(t, rec.Body.String(), `"model":"public-alias"`)
}
