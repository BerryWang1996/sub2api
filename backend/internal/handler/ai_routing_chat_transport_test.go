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
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Always sends to the test's loopback server, including adapters whose native
// URL is fixed. No external DNS/HTTP, proxy, credential refresh or model runs.
type aiChatLoopbackHTTP struct {
	target *url.URL
	client *http.Client
	bodies [][]byte
	paths  []string
}

func (h *aiChatLoopbackHTTP) Do(req *http.Request, _ string, accountID int64, _ int) (*http.Response, error) {
	if err := service.CheckAIRoutingOutboundRequest(req, accountID); err != nil {
		return nil, err
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		return nil, err
	}
	h.bodies = append(h.bodies, raw)
	h.paths = append(h.paths, req.URL.Path)
	clone := req.Clone(req.Context())
	u := *req.URL
	u.Scheme, u.Host = h.target.Scheme, h.target.Host
	clone.URL = &u
	clone.Host = h.target.Host
	return h.client.Do(clone)
}
func (h *aiChatLoopbackHTTP) DoWithTLS(req *http.Request, proxy string, id int64, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return h.Do(req, proxy, id, n)
}

func aiChatProtocolReply(protocol string, stream bool) string {
	call := `{"id":"call_wire","type":"function","function":{"name":"scan","arguments":"{}"}}`
	switch protocol {
	case "raw":
		if !stream {
			return `{"id":"chat_wire","object":"chat.completion","model":"gpt-5.1","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[` + call + `]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":9,"completion_tokens":3}}`
		}
		return "data: {\"id\":\"chat_wire\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_wire\",\"type\":\"function\",\"function\":{\"name\":\"scan\",\"arguments\":\"{}\"}}]},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n"
	case "responses":
		return "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_wire\",\"model\":\"gpt-5.1\",\"output\":[]}}\n\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"item_wire\",\"type\":\"function_call\",\"call_id\":\"call_wire\",\"name\":\"scan\",\"arguments\":\"\"}}\n\ndata: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"delta\":\"{}\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_wire\",\"model\":\"gpt-5.1\",\"status\":\"completed\",\"output\":[{\"id\":\"item_wire\",\"type\":\"function_call\",\"call_id\":\"call_wire\",\"name\":\"scan\",\"arguments\":\"{}\"}],\"usage\":{\"input_tokens\":9,\"output_tokens\":3}}}\n\n"
	case "anthropic":
		return "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_wire\",\"role\":\"assistant\",\"type\":\"message\",\"content\":[],\"usage\":{\"input_tokens\":9}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_wire\",\"name\":\"scan\",\"input\":{}}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":3}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	default:
		payload := `{"responseId":"resp_wire","candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"scan","args":{}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":3}}`
		if protocol == "antigravity" {
			payload = `{"response":` + payload + `}`
			stream = true
		}
		if stream {
			return "data: " + payload + "\n\n"
		}
		return payload
	}
}

func TestAIChatWriterRealHTTPAdapters(t *testing.T) {
	for _, protocol := range []string{"raw", "responses", "anthropic", "gemini", "antigravity"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", protocol, stream), func(t *testing.T) {
				cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true}}, Gateway: config.GatewayConfig{MaxLineSize: 1 << 20}}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, err := io.Copy(io.Discard, r.Body)
					require.NoError(t, err)
					isSSE := stream || protocol == "responses" || protocol == "anthropic" || protocol == "antigravity"
					if isSSE {
						w.Header().Set("Content-Type", "text/event-stream")
					} else {
						w.Header().Set("Content-Type", "application/json")
					}
					_, err = io.WriteString(w, aiChatProtocolReply(protocol, stream))
					require.NoError(t, err)
				}))
				defer server.Close()
				target, err := url.Parse(server.URL)
				require.NoError(t, err)
				httpUpstream := &aiChatLoopbackHTTP{target: target, client: server.Client()}
				account := &service.Account{ID: 75, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"api_key": "fake-local", "base_url": server.URL}}
				model := "gpt-5.1"
				switch protocol {
				case "raw":
					account.Extra = map[string]any{"openai_responses_mode": "force_chat_completions"}
				case "responses":
					account.Extra = map[string]any{"openai_responses_mode": "force_responses"}
				case "anthropic":
					account.Platform = service.PlatformAnthropic
					model = "claude-sonnet-4-5"
				case "gemini":
					account.Platform = service.PlatformGemini
					model = "gemini-2.5-flash"
				case "antigravity":
					account.Platform = service.PlatformAntigravity
					account.Type = service.AccountTypeOAuth
					account.Credentials = map[string]any{"access_token": "fake-oauth", "project_id": "fake-project", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)}
					model = "gemini-3.1-pro-high"
				}
				body := []byte(fmt.Sprintf(`{"model":%q,"max_completion_tokens":100,"stream":%v,"messages":[{"role":"system","content":"keep system"},{"role":"user","content":"scan"}],"tools":[{"type":"function","function":{"name":"scan","parameters":{"type":"object"}}}]}`, model, stream))
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
				bound := []string{}
				writer := newAIChatWriter(c.Writer, func(id string) error {
					require.NotContains(t, rec.Body.String(), id)
					bound = append(bound, id)
					return nil
				}, nil)
				c.Writer = writer
				switch protocol {
				case "raw", "responses":
					svc := service.NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, httpUpstream, nil, nil, nil, nil, nil, nil, nil, nil)
					defer svc.CloseOpenAIWSPool()
					_, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "")
				case "anthropic":
					svc := service.NewGatewayService(nil, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, nil, httpUpstream, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
					_, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, nil)
				case "gemini":
					svc := service.NewGeminiMessagesCompatService(nil, nil, nil, nil, nil, nil, httpUpstream, nil, cfg)
					_, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body)
				case "antigravity":
					svc := service.NewAntigravityGatewayService(nil, nil, nil, service.NewAntigravityTokenProvider(nil, nil, nil), nil, httpUpstream, service.NewSettingService(nil, cfg), nil)
					_, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, nil)
				}
				require.NoError(t, err, rec.Body.String())
				require.NoError(t, writer.finish(), rec.Body.String())
				require.Len(t, httpUpstream.bodies, 1)
				require.Len(t, bound, 1)
				require.Contains(t, rec.Body.String(), bound[0])
				require.Contains(t, string(httpUpstream.bodies[0]), "scan")
				if protocol == "raw" {
					require.Contains(t, httpUpstream.paths[0], "/chat/completions")
					require.Equal(t, "keep system", gjson.GetBytes(httpUpstream.bodies[0], "messages.0.content").Str)
				}
			})
		}
	}
}

func TestAIChatNativeChoicesAndMediaPreservedOverRealHTTP(t *testing.T) {
	for _, native := range []bool{true, false} {
		t.Run(fmt.Sprintf("native=%v", native), func(t *testing.T) {
			cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true}}}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, err := io.WriteString(w, `{"id":"chat_native","object":"chat.completion","model":"gpt-5.1","choices":[{"index":0,"message":{"role":"assistant","content":"one"},"finish_reason":"stop"},{"index":1,"message":{"role":"assistant","content":"two"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":6}}`)
				require.NoError(t, err)
			}))
			defer server.Close()
			target, err := url.Parse(server.URL)
			require.NoError(t, err)
			upstream := &aiChatLoopbackHTTP{target: target, client: server.Client()}
			account := &service.Account{ID: 75, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Credentials: map[string]any{"api_key": "fake-local", "base_url": server.URL}, Extra: map[string]any{"openai_responses_mode": "force_chat_completions"}}
			if !native {
				account.Extra["openai_responses_mode"] = "force_responses"
			}
			account.SetUpstreamModelMetadataSnapshot(service.UpstreamModelMetadataSnapshot{Source: "local-test", SyncedAt: time.Now().UTC().Format(time.RFC3339), Models: map[string]service.UpstreamModelMetadata{"gpt-5.1": {ID: "gpt-5.1", ContextWindow: 100000, MaxOutputTokens: 200, InputModalities: []string{"text", "audio", "document"}}}})
			body := []byte(`{"model":"gpt-5.1","n":2,"max_completion_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"preserve"},{"type":"input_audio","input_audio":{"data":"ZmFrZQ==","format":"wav"}},{"type":"file","file":{"filename":"fake.txt","file_data":"data:text/plain;base64,ZmFrZQ=="}}]}]}`)
			routingReq, err := service.NewAIRoutingChatRequest(body, "metadata")
			require.NoError(t, err)
			require.True(t, routingReq.RequiresNativeChat)
			require.Equal(t, 2, routingReq.OutputChoices)
			ctx := service.WithAIRoutingExecutionRequirements(t.Context(), service.AIRoutingRequirementsForCandidate(routingReq, service.AIRoutingCandidate{}))
			ctx = service.WithAIRoutingOutboundGuard(ctx, func(req *http.Request, _ int64) error {
				check := service.CheckAIRoutingFinalPayload(ctx, account, "gpt-5.1", req)
				if !check.Allowed {
					return &service.AIRoutingCapacityError{Check: check}
				}
				return nil
			})
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body))).WithContext(ctx)
			writer := newAIChatWriter(c.Writer, func(string) error { return nil }, nil)
			c.Writer = writer
			svc := service.NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil)
			defer svc.CloseOpenAIWSPool()
			_, err = svc.ForwardAsChatCompletions(ctx, c, account, body, "", "")
			if !native {
				require.Error(t, err)
				require.Empty(t, upstream.bodies)
				return
			}
			require.NoError(t, err)
			require.NoError(t, writer.finish())
			require.Len(t, upstream.bodies, 1)
			require.EqualValues(t, 2, gjson.GetBytes(upstream.bodies[0], "n").Int())
			require.JSONEq(t, gjson.GetBytes(body, "messages").Raw, gjson.GetBytes(upstream.bodies[0], "messages").Raw)
			require.EqualValues(t, 2, gjson.Get(rec.Body.String(), "choices.#").Int())
		})
	}
}
