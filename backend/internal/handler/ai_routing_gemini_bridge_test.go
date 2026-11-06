//go:build unit

package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type aiGeminiBridgeSettings struct{ service.SettingRepository }

func (aiGeminiBridgeSettings) GetValue(context.Context, string) (string, error) { return "", nil }
func (aiGeminiBridgeSettings) GetMultiple(context.Context, []string) (map[string]string, error) {
	return map[string]string{}, nil
}

func TestAIGeminiBridgeRealHTTPAndToolContinuation(t *testing.T) {
	for _, protocol := range []string{"responses", "raw", "anthropic", "antigravity"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", protocol, stream), func(t *testing.T) {
				cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true}}, Gateway: config.GatewayConfig{MaxLineSize: 1 << 20}}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					if stream || protocol == "anthropic" || protocol == "antigravity" {
						w.Header().Set("Content-Type", "text/event-stream")
					} else {
						w.Header().Set("Content-Type", "application/json")
					}
					if protocol == "responses" && !stream {
						_, _ = io.WriteString(w, `{"object":"response","id":"resp_wire","status":"completed","model":"gpt-5.1","output":[{"id":"item_wire","type":"function_call","call_id":"call_wire","name":"scan","arguments":"{}"}],"usage":{"input_tokens":9,"output_tokens":3}}`)
					} else {
						_, _ = io.WriteString(w, aiChatProtocolReply(protocol, stream))
					}
				}))
				defer server.Close()
				target, err := url.Parse(server.URL)
				require.NoError(t, err)
				upstream := &aiChatLoopbackHTTP{target: target, client: server.Client()}
				key, chosen := aiGeminiNativeFixture()
				chosen.Platform = service.PlatformOpenAI
				tools := true
				chosen.SupportsTools = &tools
				chosen.Model = "gpt-5.1"
				account := &service.Account{ID: 77, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"api_key": "synthetic", "base_url": server.URL}, Extra: map[string]any{"openai_responses_mode": "force_responses"}}
				h := &GatewayHandler{}
				switch protocol {
				case "raw", "responses":
					if protocol == "raw" {
						account.Extra["openai_responses_mode"] = "force_chat_completions"
					}
					h.openAIGatewayService = service.NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil)
					defer h.openAIGatewayService.CloseOpenAIWSPool()
				case "anthropic":
					chosen.Model, chosen.Platform = "claude-sonnet-4-5", service.PlatformAnthropic
					account.Platform = chosen.Platform
					h.gatewayService = service.NewGatewayService(nil, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
				case "antigravity":
					chosen.Model, chosen.Platform = "claude-sonnet-4-5", service.PlatformAntigravity
					account.Platform = chosen.Platform
					account.Type = service.AccountTypeOAuth
					account.Credentials = map[string]any{"access_token": "synthetic", "project_id": "synthetic", "model_mapping": map[string]any{chosen.Model: chosen.Model}}
					h.antigravityGatewayService = service.NewAntigravityGatewayService(nil, nil, nil, service.NewAntigravityTokenProvider(nil, nil, nil), nil, upstream, service.NewSettingService(aiGeminiBridgeSettings{}, cfg), nil)
				}
				account.SetUpstreamModelMetadataSnapshot(service.UpstreamModelMetadataSnapshot{Source: "synthetic", SyncedAt: time.Now().UTC().Format(time.RFC3339), Models: map[string]service.UpstreamModelMetadata{chosen.Model: {ID: chosen.Model, ContextWindow: 100000, MaxOutputTokens: 8192, InputModalities: []string{"text"}}}})
				store := &aiResponsesMemoryStore{}
				run := func(body []byte) *httptest.ResponseRecorder {
					r := gin.New()
					r.POST("/", func(c *gin.Context) {
						req, err := service.NewAIRoutingGeminiRequest(body, "public-alias", "generateContent", "metadata")
						require.NoError(t, err)
						ctx := service.WithCompositeRouteDecision(c.Request.Context(), service.CompositeRouteDecision{Matched: true, GroupID: key.Group.ID, PublicModel: "public-alias", UpstreamModel: chosen.Model, Source: service.CompositeRouteSourceAI, AICandidateID: chosen.ID, TargetAccountID: chosen.TargetAccountID, TargetPlatform: chosen.Platform})
						ctx = service.WithAIRoutingExecutionRequirements(ctx, service.AIRoutingRequirementsForCandidate(req, chosen))
						ctx = service.WithAIRoutingOutboundGuard(ctx, func(wire *http.Request, id int64) error {
							require.Equal(t, account.ID, id)
							check := service.CheckAIRoutingFinalPayload(wire.Context(), account, chosen.Model, wire)
							require.True(t, check.Allowed, check)
							return nil
						})
						c.Request = c.Request.WithContext(ctx)
						h.forwardAIGeminiWithOrigin(c, nil, store, key, req, chosen)
					}, func(c *gin.Context) {
						setOpsSelectedAccount(c, 77, chosen.Platform)
						restored, err := service.RestoreAIRoutingGeminiNativeRequest(c.Request.Context(), body)
						require.NoError(t, err)
						result, err := h.forwardAIGeminiCompatible(c.Request.Context(), c, account, chosen.Model, map[bool]string{true: "streamGenerateContent", false: "generateContent"}[stream], stream, restored)
						require.NoError(t, err)
						require.NotNil(t, result)
						require.Positive(t, result.Usage.InputTokens)
						require.Positive(t, result.Usage.OutputTokens)
					})
					recorder := httptest.NewRecorder()
					r.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body))))
					return recorder
				}
				initial := []byte(`{"systemInstruction":{"parts":[{"text":"system-preserved"}]},"contents":[{"role":"user","parts":[{"text":"original-visible-context"}]}],"generationConfig":{"maxOutputTokens":100},"tools":[{"functionDeclarations":[{"name":"scan","parameters":{"type":"OBJECT"}}]}]}`)
				first := run(initial)
				require.Equal(t, 200, first.Code, first.Body.String())
				require.Len(t, upstream.bodies, 1)
				var part gjson.Result
				if stream {
					for _, frame := range strings.Split(first.Body.String(), "\n\n") {
						payload := strings.TrimPrefix(frame, "data: ")
						for _, p := range gjson.Get(payload, "candidates.0.content.parts").Array() {
							if p.Get("functionCall").Exists() {
								part = p
							}
						}
					}
				} else {
					part = gjson.Get(first.Body.String(), "candidates.0.content.parts.0")
				}
				require.True(t, part.Get("functionCall").Exists(), first.Body.String())
				id := part.Get("functionCall.id").Str
				require.True(t, strings.HasPrefix(id, service.GeminiResponsesToolCallPrefix))
				follow := []byte(`{"contents":[{"role":"user","parts":[{"text":"original-visible-context"}]},{"role":"model","parts":[` + part.Raw + `]},{"role":"user","parts":[{"functionResponse":{"name":"scan","id":"` + id + `","response":{"ok":true}}}]}],"generationConfig":{"maxOutputTokens":100}}`)
				parsed, err := service.NewAIRoutingGeminiRequest(follow, "public-alias", "generateContent", "metadata")
				require.NoError(t, err)
				binding, err := loadAIGeminiBinding(t.Context(), store, key, parsed)
				require.NoError(t, err)
				require.Equal(t, newAIToolBinding(chosen, 77), binding)
				second := run(follow)
				require.Equal(t, 200, second.Code, second.Body.String())
				require.Len(t, upstream.bodies, 2)
				require.NotContains(t, string(upstream.bodies[1]), id, "gateway IDs restore before the foreign protocol conversion")
				require.Contains(t, string(upstream.bodies[1]), "original-visible-context")
				require.Contains(t, string(upstream.bodies[1]), "ok")
				if protocol != "antigravity" {
					require.Contains(t, string(upstream.bodies[1]), "call_wire")
				}
			})
		}
	}
}

func TestAIGeminiBridgeInterruptionAndCapacityRefusalNeverReplay(t *testing.T) {
	for _, mode := range []string{"responses-truncated", "chat-truncated", "capacity"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				if mode == "chat-truncated" {
					_, _ = io.WriteString(w, "data: {\"id\":\"chat\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial-only\"},\"finish_reason\":null}]}\n\n")
				} else {
					_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp\",\"output\":[]}}\n\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg\",\"output_index\":0,\"content_index\":0,\"delta\":\"partial-only\"}\n\n")
				}
			}))
			defer server.Close()
			u, err := url.Parse(server.URL)
			require.NoError(t, err)
			upstream := &aiChatLoopbackHTTP{target: u, client: server.Client()}
			cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true}}, Gateway: config.GatewayConfig{MaxLineSize: 1 << 20}}
			account := &service.Account{ID: 77, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"api_key": "synthetic", "base_url": server.URL}, Extra: map[string]any{"openai_responses_mode": "force_responses"}}
			if mode == "chat-truncated" {
				account.Extra["openai_responses_mode"] = "force_chat_completions"
			}
			account.SetUpstreamModelMetadataSnapshot(service.UpstreamModelMetadataSnapshot{Source: "synthetic", SyncedAt: time.Now().UTC().Format(time.RFC3339), Models: map[string]service.UpstreamModelMetadata{"gpt-5.1": {ID: "gpt-5.1", ContextWindow: 100000, MaxOutputTokens: 8192, InputModalities: []string{"text"}}}})
			h := &GatewayHandler{openAIGatewayService: service.NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil)}
			defer h.openAIGatewayService.CloseOpenAIWSPool()
			body := []byte(`{"contents":[{"parts":[{"text":"preserved original"}]}],"generationConfig":{"maxOutputTokens":100}}`)
			req, err := service.NewAIRoutingGeminiRequest(body, "alias", "streamGenerateContent", "metadata")
			require.NoError(t, err)
			requirements := service.AIRoutingRequirementsForCandidate(req, service.AIRoutingCandidate{})
			if mode == "capacity" {
				requirements.ContextWindowTokens = 64
			}
			ctx := service.WithAIRoutingExecutionRequirements(t.Context(), requirements)
			ctx = service.WithAIRoutingOutboundGuard(ctx, func(wire *http.Request, _ int64) error {
				check := service.CheckAIRoutingFinalPayload(wire.Context(), account, "gpt-5.1", wire)
				if !check.Allowed {
					return &service.AIRoutingCapacityError{Check: check}
				}
				return nil
			})
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/alias:streamGenerateContent", strings.NewReader(string(body))).WithContext(ctx)
			_, err = h.forwardAIGeminiCompatible(ctx, c, account, "gpt-5.1", "streamGenerateContent", true, body)
			require.Error(t, err, recorder.Body.String())
			require.NotContains(t, recorder.Body.String(), `"finishReason":"STOP"`)
			if mode == "capacity" {
				require.Empty(t, upstream.bodies)
				require.True(t, service.IsAIRoutingCapacityError(err))
			} else {
				require.Len(t, upstream.bodies, 1)
				require.Contains(t, recorder.Body.String(), "partial-only")
			}
		})
	}
}

func TestGeminiBridgeOpenAIUsageKeepsCachedInputSeparate(t *testing.T) {
	result := geminiBridgeOpenAIResult(&service.OpenAIForwardResult{RequestID: "synthetic-request", Model: "selected", UpstreamModel: "selected", UpstreamResponseModel: "reported", Usage: service.OpenAIUsage{InputTokens: 1000, CacheReadInputTokens: 200, CacheCreationInputTokens: 100, OutputTokens: 100}})
	require.Equal(t, 700, result.Usage.InputTokens)
	require.Equal(t, 100, result.Usage.CacheCreationInputTokens)
	require.Equal(t, 200, result.Usage.CacheReadInputTokens)
	require.Equal(t, 100, result.Usage.OutputTokens)
	require.Equal(t, "selected", result.Model)
	require.Equal(t, "synthetic-request", result.RequestID)
}
