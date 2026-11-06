//go:build unit

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/testutil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAIGeminiCompatSignedStateAcrossProtocols(t *testing.T) {
	for _, platform := range []string{"gemini", "antigravity"} {
		for _, endpoint := range []string{service.CompositeRouteEndpointChatCompletions, service.CompositeRouteEndpointResponses} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%v", platform, endpoint, stream), func(t *testing.T) {
					native := `{"responseId":"native_response","candidates":[{"content":{"parts":[{"thoughtSignature":"private-native-signature","functionCall":{"id":"native_1","name":"same_name","args":{"n":1}}},{"functionCall":{"id":"native_2","name":"same_name","args":{"n":2}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":9,"candidatesTokenCount":3}}`
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						_, _ = io.Copy(io.Discard, r.Body)
						payload := native
						if platform == "antigravity" {
							payload = `{"response":` + native + `}`
						}
						if stream || platform == "antigravity" {
							w.Header().Set("Content-Type", "text/event-stream")
							payload = "data: " + payload + "\n\n"
						} else {
							w.Header().Set("Content-Type", "application/json")
						}
						_, _ = io.WriteString(w, payload)
					}))
					defer server.Close()
					h, account, candidate, upstream := aiWSHTTPFixture(t, platform, server)
					store, _ := testutil.NewRedisAIRoutingStore(t)
					origins, ok := store.(service.AIRoutingOriginStore)
					require.True(t, ok)
					key, _ := aiGeminiNativeFixture()
					run := func(body []byte, caller *service.APIKey) (string, error) {
						var request service.AIRoutingRequest
						var err error
						if endpoint == service.CompositeRouteEndpointChatCompletions {
							request, err = service.NewAIRoutingChatRequest(body, "metadata")
						} else {
							request, err = service.NewAIRoutingResponsesRequest(body, "metadata")
						}
						require.NoError(t, err)
						ctx := service.WithCompositeRouteDecision(t.Context(), service.CompositeRouteDecision{Matched: true, GroupID: key.Group.ID, PublicModel: candidate.Model, UpstreamModel: candidate.Model, Source: service.CompositeRouteSourceAI, AICandidateID: candidate.ID, TargetAccountID: candidate.TargetAccountID, TargetPlatform: candidate.Platform})
						ctx = service.WithAIRoutingExecutionRequirements(ctx, service.AIRoutingRequirementsForCandidate(request, candidate))
						ctx = service.WithAIRoutingOutboundGuard(ctx, func(wire *http.Request, id int64) error {
							check := service.CheckAIRoutingFinalPayload(wire.Context(), account, candidate.Model, wire)
							if !check.Allowed {
								return &service.AIRoutingCapacityError{Check: check}
							}
							return nil
						})
						ctx = withAIGeminiResponseState(ctx, origins, caller, candidate, func() int64 { return account.ID }, endpoint)
						recorder := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(recorder)
						c.Request = httptest.NewRequest(http.MethodPost, "/synthetic", strings.NewReader(string(body))).WithContext(ctx)
						target := newAIToolBinding(candidate, account.ID)
						if endpoint == service.CompositeRouteEndpointChatCompletions {
							writer := newAIChatWriter(c.Writer, func(id string) error {
								require.NotContains(t, recorder.Body.String(), id)
								return saveAIChatBinding(ctx, store, caller, id, target)
							}, nil)
							c.Writer = writer
							if platform == "gemini" {
								_, err = h.geminiCompatService.ForwardAsChatCompletions(ctx, c, account, body)
							} else {
								_, err = h.antigravityGatewayService.ForwardAsChatCompletions(ctx, c, account, body, nil)
							}
							if finishErr := writer.finish(); err == nil {
								err = finishErr
							}
						} else {
							writer := newAIResponsesWriter(c.Writer, func(kind, id, responseID string) error {
								return saveAIResponsesBinding(ctx, origins, caller, service.AIRoutingOriginRef{Kind: kind, ID: id}, service.AIRoutingOriginBinding{Protocol: endpoint, Candidate: target.Candidate, Account: target.Account, Model: target.Model, Platform: target.Platform, AccountTarget: target.AccountTarget, ResponseID: responseID})
							}, nil)
							c.Writer = writer
							if platform == "gemini" {
								_, err = h.geminiCompatService.ForwardAsResponses(ctx, c, account, body, nil)
							} else {
								_, err = h.antigravityGatewayService.ForwardAsResponses(ctx, c, account, body, nil)
							}
							if finishErr := writer.finish(); err == nil {
								err = finishErr
							}
						}
						return recorder.Body.String(), err
					}
					initial := map[string]any{"model": candidate.Model, "stream": stream, "store": false}
					if endpoint == service.CompositeRouteEndpointChatCompletions {
						initial["messages"], initial["max_completion_tokens"] = []any{map[string]any{"role": "user", "content": "task"}}, 100
					} else {
						initial["input"], initial["max_output_tokens"] = "task", 100
					}
					body, err := json.Marshal(initial)
					require.NoError(t, err)
					output, err := run(body, key)
					require.NoError(t, err, output)
					require.NotContains(t, output, "private-native-signature")
					calls := aiGeminiCompatTestCalls(output, endpoint, stream)
					require.Len(t, calls, 2, output)
					for _, call := range calls {
						require.True(t, strings.HasPrefix(call["id"].(string), service.GeminiResponsesToolCallPrefix))
					}
					if endpoint == service.CompositeRouteEndpointChatCompletions {
						initial["messages"] = []any{map[string]any{"role": "user", "content": "task"}, map[string]any{"role": "assistant", "content": nil, "tool_calls": calls}, map[string]any{"role": "tool", "tool_call_id": calls[1]["id"], "content": "second"}, map[string]any{"role": "tool", "tool_call_id": calls[0]["id"], "content": "first"}, map[string]any{"role": "assistant", "content": "done"}, map[string]any{"role": "user", "content": "continue"}}
					} else {
						initial["input"] = []any{map[string]any{"role": "user", "content": "task"}, map[string]any{"type": "function_call", "call_id": calls[0]["id"], "name": "same_name", "arguments": `{"n":1}`}, map[string]any{"type": "function_call", "call_id": calls[1]["id"], "name": "same_name", "arguments": `{"n":2}`}, map[string]any{"type": "function_call_output", "call_id": calls[1]["id"], "output": "second"}, map[string]any{"type": "function_call_output", "call_id": calls[0]["id"], "output": "first"}}
					}
					body, err = json.Marshal(initial)
					require.NoError(t, err)
					if endpoint == service.CompositeRouteEndpointChatCompletions {
						parsed, parseErr := service.NewAIRoutingChatRequest(body, "metadata")
						require.NoError(t, parseErr)
						require.True(t, parsed.RequiresOrigin, "closed signed history must not route to another candidate")
						bound, loadErr := loadAIChatBinding(context.Background(), store, key, parsed)
						require.NoError(t, loadErr)
						require.Equal(t, newAIToolBinding(candidate, account.ID), bound)
					}
					foreign := *key
					foreign.ID++
					_, err = run(body, &foreign)
					require.Error(t, err)
					require.Len(t, upstream.bodies, 1, "foreign Key fails before sending any model request")
					output, err = run(body, key)
					require.NoError(t, err, output)
					require.Len(t, upstream.bodies, 2)
					wire := upstream.bodies[1]
					if platform == "antigravity" {
						wire = []byte(gjson.GetBytes(wire, "request").Raw)
					}
					require.NotContains(t, string(wire), "skip_thought_signature_validator")
					require.NotContains(t, string(wire), service.GeminiResponsesToolCallPrefix)
					var parts, results []gjson.Result
					for _, content := range gjson.GetBytes(wire, "contents").Array() {
						for _, part := range content.Get("parts").Array() {
							if part.Get("functionCall").Exists() {
								parts = append(parts, part)
							}
							if part.Get("functionResponse").Exists() {
								results = append(results, part)
							}
						}
					}
					require.Len(t, parts, 2)
					require.Len(t, results, 2)
					require.Equal(t, "private-native-signature", parts[0].Get("thoughtSignature").Str)
					require.False(t, parts[1].Get("thoughtSignature").Exists())
					require.Equal(t, "native_1", results[0].Get("functionResponse.id").Str)
					require.Contains(t, results[0].Raw, "first")
					require.Equal(t, "native_2", results[1].Get("functionResponse.id").Str)
				})
			}
		}
	}
}

func aiGeminiCompatTestCalls(body, endpoint string, stream bool) []map[string]any {
	if endpoint == service.CompositeRouteEndpointResponses {
		var response gjson.Result
		if !stream {
			response = gjson.Parse(body)
		} else {
			for _, line := range strings.Split(body, "\n") {
				if strings.HasPrefix(line, "data:") && gjson.Get(strings.TrimSpace(strings.TrimPrefix(line, "data:")), "type").Str == "response.completed" {
					response = gjson.Get(strings.TrimSpace(strings.TrimPrefix(line, "data:")), "response")
				}
			}
		}
		var calls []map[string]any
		for _, item := range response.Get("output").Array() {
			if item.Get("type").Str == "function_call" {
				calls = append(calls, map[string]any{"id": item.Get("call_id").Str})
			}
		}
		return calls
	}
	if !stream {
		var calls []map[string]any
		_ = json.Unmarshal([]byte(gjson.Get(body, "choices.0.message.tool_calls").Raw), &calls)
		return calls
	}
	calls := map[int]map[string]any{}
	for _, line := range strings.Split(body, "\n") {
		for _, call := range gjson.Get(strings.TrimSpace(strings.TrimPrefix(line, "data:")), "choices.0.delta.tool_calls").Array() {
			i := int(call.Get("index").Int())
			if calls[i] == nil {
				calls[i] = map[string]any{"id": "", "type": "function", "function": map[string]any{"name": "", "arguments": ""}}
			}
			calls[i]["id"] = calls[i]["id"].(string) + call.Get("id").Str
			function := calls[i]["function"].(map[string]any)
			function["name"] = function["name"].(string) + call.Get("function.name").Str
			function["arguments"] = function["arguments"].(string) + call.Get("function.arguments").Str
		}
	}
	ordered := make([]map[string]any, len(calls))
	for index := range ordered {
		ordered[index] = calls[index]
	}
	return ordered
}
