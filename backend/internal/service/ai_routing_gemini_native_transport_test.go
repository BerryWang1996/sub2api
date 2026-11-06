package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestAIGeminiNativeAntigravityNeverFabricatesOrCleansSignature(t *testing.T) {
	for _, signature := range []bool{false, true} {
		body := `{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{}}` + map[bool]string{true: `,"thoughtSignature":"exact-original-signature"`, false: ""}[signature] + `}]}]}`
		upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{{StatusCode: 400, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"response":{"error":{"code":400,"message":"Corrupted thought signature.","status":"INVALID_ARGUMENT"}}}`))}}}
		svc := &AntigravityGatewayService{settingService: NewSettingService(&antigravitySettingRepoStub{}, &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}), tokenProvider: &AntigravityTokenProvider{}, httpUpstream: upstream}
		account := &Account{ID: 7, Platform: PlatformAntigravity, Type: AccountTypeOAuth, Status: StatusActive, Concurrency: 1, Credentials: map[string]any{"access_token": "synthetic", "project_id": "synthetic", "model_mapping": map[string]any{"gemini-3.1-pro-high": "gemini-3.1-pro-high"}}}
		ctx := WithAIRoutingGeminiNativeState(context.Background(), AIRoutingGeminiNativeStateCallbacks{})
		c, _ := newAntigravityCompatContext(http.MethodPost, "/v1beta/models/gemini-3.1-pro-high:generateContent", []byte(body))
		c.Request = c.Request.WithContext(ctx)
		_, err := svc.ForwardGemini(ctx, c, account, "gemini-3.1-pro-high", "generateContent", false, []byte(body), false)
		require.Error(t, err)
		require.Len(t, upstream.requestBodies, 1, "AI origins forbid a cleaning retry")
		require.NotContains(t, string(upstream.requestBodies[0]), "skip_thought_signature_validator")
		if signature {
			require.Contains(t, string(upstream.requestBodies[0]), "exact-original-signature")
		} else {
			require.NotContains(t, string(upstream.requestBodies[0]), "thoughtSignature")
		}
	}
}
