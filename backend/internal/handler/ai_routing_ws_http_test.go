//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func aiWSHTTPFixture(t *testing.T, protocol string, server *httptest.Server) (*GatewayHandler, *service.Account, service.AIRoutingCandidate, *aiChatLoopbackHTTP) {
	t.Helper()
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	upstream := &aiChatLoopbackHTTP{target: target, client: server.Client()}
	cfg := &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{AllowInsecureHTTP: true, AllowPrivateHosts: true}}, Gateway: config.GatewayConfig{MaxLineSize: 1 << 20}}
	_, candidate := aiGeminiNativeFixture()
	candidate.Platform = service.PlatformOpenAI
	candidate.Model = "gpt-5.1"
	supportsTools := true
	candidate.SupportsTools = &supportsTools
	account := &service.Account{ID: 77, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"api_key": "synthetic", "base_url": server.URL}, Extra: map[string]any{"openai_responses_mode": "force_responses"}}
	h := &GatewayHandler{}
	switch protocol {
	case "responses", "raw":
		if protocol == "raw" {
			account.Extra["openai_responses_mode"] = "force_chat_completions"
		}
		h.openAIGatewayService = service.NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil)
		t.Cleanup(h.openAIGatewayService.CloseOpenAIWSPool)
	case "anthropic":
		candidate.Platform, candidate.Model = service.PlatformAnthropic, "claude-sonnet-4-5"
		account.Platform = candidate.Platform
		h.gatewayService = service.NewGatewayService(nil, nil, nil, nil, nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	case "gemini":
		candidate.Platform, candidate.Model = service.PlatformGemini, "gemini-2.5-pro"
		account.Platform = candidate.Platform
		h.geminiCompatService = service.NewGeminiMessagesCompatService(nil, nil, nil, nil, nil, nil, upstream, nil, cfg)
	case "antigravity":
		candidate.Platform, candidate.Model = service.PlatformAntigravity, "gemini-3.1-pro-high"
		account.Platform = candidate.Platform
		account.Type = service.AccountTypeOAuth
		account.Credentials = map[string]any{"access_token": "synthetic", "project_id": "synthetic", "model_mapping": map[string]any{candidate.Model: candidate.Model}}
		h.antigravityGatewayService = service.NewAntigravityGatewayService(nil, nil, nil, service.NewAntigravityTokenProvider(nil, nil, nil), nil, upstream, service.NewSettingService(aiGeminiBridgeSettings{}, cfg), nil)
	}
	account.SetUpstreamModelMetadataSnapshot(service.UpstreamModelMetadataSnapshot{Source: "synthetic", SyncedAt: time.Now().UTC().Format(time.RFC3339), Models: map[string]service.UpstreamModelMetadata{candidate.Model: {ID: candidate.Model, ContextWindow: 100000, MaxOutputTokens: 8192, InputModalities: []string{"text"}}}})
	return h, account, candidate, upstream
}

func aiWSHTTPTestContext(t *testing.T, body []byte, account *service.Account, candidate service.AIRoutingCandidate) *gin.Context {
	t.Helper()
	req, err := service.NewAIRoutingWSRequest(body, "metadata")
	require.NoError(t, err)
	require.False(t, req.RequiresNativeResponses, "self-contained public subset must remain eligible for the HTTP bridge")
	ctx := service.WithCompositeRouteDecision(t.Context(), service.CompositeRouteDecision{Matched: true, GroupID: 13, PublicModel: "public-alias", UpstreamModel: candidate.Model, Source: service.CompositeRouteSourceAI, AICandidateID: candidate.ID, TargetAccountID: candidate.TargetAccountID, TargetPlatform: candidate.Platform})
	ctx = service.WithAIRoutingExecutionRequirements(ctx, service.AIRoutingRequirementsForCandidate(req, candidate))
	ctx = service.WithAIRoutingOutboundGuard(ctx, func(wire *http.Request, id int64) error {
		require.Equal(t, account.ID, id)
		check := service.CheckAIRoutingFinalPayload(wire.Context(), account, candidate.Model, wire)
		if !check.Allowed {
			return &service.AIRoutingCapacityError{Check: check}
		}
		return nil
	})
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil).WithContext(ctx)
	c.Request.Header.Set("Upgrade", "websocket")
	service.SetOpenAIClientTransport(c, service.OpenAIClientTransportWS)
	setOpsSelectedAccount(c, 77, account.Platform)
	return c
}

func TestAIWSHTTPExecutorRealAdaptersPersistWSOriginsAndContinueTools(t *testing.T) {
	for _, protocol := range []string{"responses", "raw", "anthropic", "gemini", "antigravity"} {
		t.Run(protocol, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				payload := aiChatProtocolReply(protocol, true)
				if protocol == "gemini" {
					payload = strings.ReplaceAll(payload, `"functionCall":{"name":"scan","args":{}}`, `"functionCall":{"name":"scan","args":{}},"thoughtSignature":"synthetic-signed-gemini"`)
				}
				_, _ = io.WriteString(w, payload)
			}))
			defer server.Close()
			h, account, candidate, upstream := aiWSHTTPFixture(t, protocol, server)
			key, _ := aiGeminiNativeFixture()
			store := &aiResponsesMemoryStore{}
			run := func(body []byte) []byte {
				c := aiWSHTTPTestContext(t, body, account, candidate)
				ctx := withAIGeminiResponseState(c.Request.Context(), store, key, candidate, func() int64 { return account.ID }, service.AIRoutingEndpointResponsesWS)
				c.Request = c.Request.WithContext(ctx)
				executor, err := h.newAIWSHTTPExecutor(ctx, c, account, body, "public-alias", func(context.Context) error { return nil })
				require.NoError(t, err)
				defer executor.Close()
				require.Empty(t, executor.ConnID())
				require.Error(t, executor.Steer(ctx, []byte(`{}`)))
				var emitted []byte
				inspector := &aiResponsesOriginInspector{emitted: map[string]bool{}, bind: func(kind, id, responseID string) error {
					require.NotContains(t, string(emitted), id, "bind every identifier before WS exposure")
					target := newAIToolBinding(candidate, account.ID)
					origin := service.AIRoutingOriginBinding{Protocol: service.AIRoutingEndpointResponsesWS, Candidate: target.Candidate, Account: target.Account, Model: target.Model, Platform: target.Platform, AccountTarget: target.AccountTarget, ResponseID: responseID, PublicModel: "public-alias", Native: false}
					return saveAIResponsesBinding(ctx, store, key, service.AIRoutingOriginRef{Kind: kind, ID: id}, origin)
				}}
				result, err := executor.Run(func(event []byte) error {
					require.NoError(t, inspector.inspect(event, gjson.GetBytes(event, "type").Str))
					emitted = append(emitted, event...)
					emitted = append(emitted, '\n')
					return nil
				})
				require.NoError(t, err, string(emitted))
				require.NotNil(t, result)
				require.True(t, result.OpenAIWSMode)
				require.Equal(t, "response.completed", result.UpstreamTerminalEvent)
				require.Positive(t, result.Usage.InputTokens)
				require.Positive(t, result.Usage.OutputTokens)
				if protocol != "responses" && protocol != "raw" {
					require.NotNil(t, executor.(*aiWSHTTPExecutor).GatewayResult())
				}
				require.Equal(t, http.MethodGet, c.Request.Method)
				require.Equal(t, service.OpenAIClientTransportWS, service.GetOpenAIClientTransport(c))
				require.Equal(t, "websocket", c.Request.Header.Get("Upgrade"))
				_, err = executor.Run(func([]byte) error { return nil })
				require.Error(t, err)
				return emitted
			}
			body, err := json.Marshal(map[string]any{"type": "response.create", "model": candidate.Model, "stream_id": "lane-a", "store": false, "max_output_tokens": 100, "input": "full-visible-context", "tools": []any{map[string]any{"type": "function", "name": "scan", "parameters": map[string]any{"type": "object"}}}})
			require.NoError(t, err)
			events := run(body)
			require.Len(t, upstream.bodies, 1)
			require.NotContains(t, string(upstream.bodies[0]), "response.create")
			require.NotContains(t, string(upstream.bodies[0]), "stream_id")
			var call gjson.Result
			for _, line := range strings.Split(string(events), "\n") {
				if gjson.Get(line, "type").Str == "response.completed" {
					for _, item := range gjson.Get(line, "response.output").Array() {
						if item.Get("type").Str == "function_call" {
							call = item
						}
					}
				}
			}
			require.NotEmpty(t, call.Raw, string(events))
			id := call.Get("call_id").Str
			follow, err := json.Marshal(map[string]any{"type": "response.create", "model": candidate.Model, "store": false, "max_output_tokens": 100, "input": []any{map[string]any{"role": "user", "content": "full-visible-context"}, json.RawMessage(call.Raw), map[string]any{"type": "function_call_output", "call_id": id, "output": "synthetic result"}}})
			require.NoError(t, err)
			request, err := service.NewAIRoutingWSRequest(follow, "metadata")
			require.NoError(t, err)
			binding, origin, err := loadAIResponsesBinding(t.Context(), store, key, request)
			require.NoError(t, err)
			require.Equal(t, newAIToolBinding(candidate, 77), binding)
			require.NotNil(t, origin)
			require.False(t, origin.Native)
			require.Empty(t, origin.ConnectionID)
			require.Equal(t, service.AIRoutingEndpointResponsesWS, origin.Protocol)
			foreign := request
			foreign.Endpoint = service.CompositeRouteEndpointResponses
			_, _, err = loadAIResponsesBinding(t.Context(), store, key, foreign)
			require.Error(t, err, "HTTP namespace cannot consume WS bridge origin")
			run(follow)
			require.Len(t, upstream.bodies, 2)
			require.Contains(t, string(upstream.bodies[1]), "full-visible-context")
			require.Contains(t, string(upstream.bodies[1]), "synthetic result")
			if protocol == "gemini" {
				require.Contains(t, string(upstream.bodies[1]), "synthetic-signed-gemini")
				require.NotContains(t, string(upstream.bodies[1]), id)
			}
		})
	}
}

func TestAIWSHTTPExecutorCancellationStopsDetachedAnthropicRead(t *testing.T) {
	var requests atomic.Int32
	upstreamCancelled := make(chan struct{})
	cleanup := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":9}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			once.Do(func() { close(upstreamCancelled) })
		case <-cleanup:
		}
	}))
	defer server.Close()
	defer close(cleanup)
	h, account, candidate, _ := aiWSHTTPFixture(t, "anthropic", server)
	body, err := json.Marshal(map[string]any{"type": "response.create", "model": candidate.Model, "store": false, "max_output_tokens": 100, "input": "request"})
	require.NoError(t, err)
	c := aiWSHTTPTestContext(t, body, account, candidate)
	executor, err := h.newAIWSHTTPExecutor(c.Request.Context(), c, account, body, "alias", func(context.Context) error { return nil })
	require.NoError(t, err)
	defer executor.Close()
	type completion struct {
		result *service.OpenAIForwardResult
		err    error
	}
	done := make(chan completion, 1)
	go func() {
		result, err := executor.Run(func(event []byte) error {
			if gjson.GetBytes(event, "type").Str == "response.output_text.delta" {
				executor.Cancel()
			}
			return nil
		})
		done <- completion{result, err}
	}()
	select {
	case result := <-done:
		require.Error(t, result.err)
		require.NotNil(t, result.result)
		require.Equal(t, 9, result.result.Usage.InputTokens)
		require.Empty(t, result.result.UpstreamTerminalEvent)
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled bridge retained its execution slot")
	}
	select {
	case <-upstreamCancelled:
	case <-time.After(time.Second):
		t.Fatal("HTTP upstream did not observe lane cancellation")
	}
	require.Equal(t, int32(1), requests.Load())
}

func TestAIWSHTTPExecutorRejectsImplicitStateBeforeHTTP(t *testing.T) {
	h := &GatewayHandler{}
	account := &service.Account{ID: 1, Platform: service.PlatformOpenAI}
	for _, body := range []string{
		`{"type":"response.create","model":"m","input":"x"}`,
		`{"type":"response.create","model":"m","store":false,"generate":false,"input":[]}`,
		`{"type":"response.create","model":"m","store":false,"previous_response_id":"resp","input":"x"}`,
		`{"type":"response.create","model":"m","store":false,"input":[{"type":"reasoning","encrypted_content":"opaque"}]}`,
		`{"type":"response.create","model":"m","store":false,"input":[{"type":"function_call_output","call_id":"call","output":"x"}]}`,
	} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx := service.WithAIRoutingOutboundGuard(t.Context(), func(*http.Request, int64) error { return nil })
		c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil).WithContext(ctx)
		_, err := h.newAIWSHTTPExecutor(ctx, c, account, []byte(body), "alias", func(context.Context) error { return nil })
		require.Error(t, err, body)
	}
}

func TestAIWSHTTPExecutorTruncatedUpstreamNeverCompletesOrReplays(t *testing.T) {
	for _, protocol := range []string{"responses", "raw", "anthropic", "gemini", "antigravity"} {
		t.Run(protocol, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				payload := aiChatProtocolReply(protocol, true)
				switch protocol {
				case "responses":
					payload = strings.Split(payload, `data: {"type":"response.completed"`)[0]
				case "raw":
					payload = "data: {\"id\":\"chat_wire\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":2}}\n\n"
				case "anthropic":
					payload = strings.Split(payload, "event: message_delta")[0]
				default:
					payload = strings.ReplaceAll(payload, `,"finishReason":"STOP"`, "")
				}
				_, _ = io.WriteString(w, payload)
			}))
			defer server.Close()
			h, account, candidate, _ := aiWSHTTPFixture(t, protocol, server)
			body, err := json.Marshal(map[string]any{"type": "response.create", "model": candidate.Model, "store": false, "max_output_tokens": 100, "input": "request"})
			require.NoError(t, err)
			c := aiWSHTTPTestContext(t, body, account, candidate)
			executor, err := h.newAIWSHTTPExecutor(c.Request.Context(), c, account, body, "alias", func(context.Context) error { return nil })
			require.NoError(t, err)
			defer executor.Close()
			var events []string
			result, err := executor.Run(func(event []byte) error {
				events = append(events, gjson.GetBytes(event, "type").Str)
				return nil
			})
			require.Error(t, err, "EOF without a real provider terminal cannot finish a WS turn")
			require.NotContains(t, events, "response.completed")
			require.NotContains(t, events, "response.incomplete")
			if protocol != "responses" {
				require.NotNil(t, result, "preserve any reported partial billing evidence")
				require.Equal(t, 9, result.Usage.InputTokens)
			}
			require.Equal(t, int32(1), requests.Load(), "never replay after partial output")
		})
	}
}

func TestAIWSHTTPExecutorLocalRefusalAndOutputFailureDoNotReplay(t *testing.T) {
	for _, stage := range []string{"revalidate", "capacity", "emit"} {
		t.Run(stage, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, aiChatProtocolReply("responses", true))
			}))
			defer server.Close()
			h, account, candidate, _ := aiWSHTTPFixture(t, "responses", server)
			body, err := json.Marshal(map[string]any{"type": "response.create", "model": candidate.Model, "store": false, "max_output_tokens": 100, "input": "request"})
			require.NoError(t, err)
			c := aiWSHTTPTestContext(t, body, account, candidate)
			failure := errors.New("synthetic local refusal")
			if stage == "capacity" {
				ctx := service.WithAIRoutingOutboundGuard(c.Request.Context(), func(*http.Request, int64) error {
					return &service.AIRoutingCapacityError{Check: service.AIRoutingCapabilityCheck{Reason: "synthetic_refusal"}}
				})
				c.Request = c.Request.WithContext(ctx)
			}
			executor, err := h.newAIWSHTTPExecutor(c.Request.Context(), c, account, body, "alias", func(context.Context) error {
				if stage == "revalidate" {
					return failure
				}
				return nil
			})
			require.NoError(t, err)
			defer executor.Close()
			var events int
			_, err = executor.Run(func([]byte) error {
				events++
				return failure
			})
			require.Error(t, err)
			if stage == "emit" {
				require.Equal(t, int32(1), requests.Load())
				require.Equal(t, 1, events)
			} else {
				require.Zero(t, requests.Load())
				require.Zero(t, events)
			}
		})
	}
}
