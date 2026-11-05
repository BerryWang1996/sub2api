//go:build unit

package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func aiExecutionResponsesToolSSE(interrupted bool) string {
	s := "data: " + `{"type":"response.created","response":{"id":"resp_ai","model":"gpt-5.1","status":"in_progress"}}` + "\n\n" +
		"data: " + `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_ai","call_id":"call_selected","name":"lookup","arguments":"","status":"in_progress"}}` + "\n\n"
	if interrupted {
		return s + "data: " + `{"type":"error","error":{"code":"invalid_encrypted_content","message":"interrupted after tool output"}}` + "\n\n"
	}
	return s + "data: " + `{"type":"response.function_call_arguments.delta","item_id":"fc_ai","output_index":0,"delta":"{}"}` + "\n\n" +
		"data: " + `{"type":"response.completed","response":{"id":"resp_ai","model":"gpt-5.1","status":"completed","output":[{"type":"function_call","id":"fc_ai","call_id":"call_selected","name":"lookup","arguments":"{}","status":"completed"}],"usage":{"input_tokens":5,"output_tokens":2}}}` + "\n\ndata: [DONE]\n\n"
}

func aiExecutionChatToolResponse(stream bool) string {
	if !stream {
		return `{"id":"chatcmpl_ai","object":"chat.completion","model":"gpt-5.1","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_selected","type":"function","function":{"name":"lookup","arguments":"{}"}}]}}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`
	}
	return "data: " + `{"id":"chatcmpl_ai","object":"chat.completion.chunk","model":"gpt-5.1","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_selected","type":"function","function":{"name":"lookup","arguments":"{}"}}]}}]}` + "\n\n" +
		"data: " + `{"id":"chatcmpl_ai","object":"chat.completion.chunk","model":"gpt-5.1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}` + "\n\ndata: [DONE]\n\n"
}

// The transport is a local TLS server. This exercises request construction and
// the real Messages converters; no real provider or model is contacted.
func TestAIRoutingForwardSelectedModelAndConvertedToolID(t *testing.T) {
	for _, protocol := range []string{"responses", "chat"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", protocol, stream), func(t *testing.T) {
				var requests atomic.Int32
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					raw, err := io.ReadAll(r.Body)
					assert.NoError(t, err)
					assert.Equal(t, "gpt-5.1", gjson.GetBytes(raw, "model").Str)
					assert.NotContains(t, string(raw), "arbitrary-client-alias")
					if protocol == "responses" {
						assert.Equal(t, "/v1/responses", r.URL.Path)
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, aiExecutionResponsesToolSSE(false))
					} else {
						assert.Equal(t, "/v1/chat/completions", r.URL.Path)
						w.Header().Set("Content-Type", "application/json")
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
						}
						_, _ = io.WriteString(w, aiExecutionChatToolResponse(stream))
					}
				}))
				defer server.Close()
				account := rawChatCompletionsTestAccount()
				account.Credentials["base_url"] = server.URL
				if protocol == "chat" {
					account.Extra = map[string]any{"openai_responses_supported": false}
				}
				svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: &reasoningTLSUpstream{client: server.Client()}}
				body := []byte(fmt.Sprintf(`{"model":"gpt-5.1","stream":%v,"max_tokens":32,"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"look up this key"}]}`, stream))
				c, rec := reasoningReplayContext(body)
				target := account.ID
				ctx := WithCompositeRouteDecision(context.Background(), CompositeRouteDecision{Matched: true, GroupID: 10, PublicModel: "arbitrary-client-alias", UpstreamModel: "gpt-5.1", Source: CompositeRouteSourceAI, AICandidateID: "selected", TargetAccountID: &target, TargetPlatform: PlatformOpenAI})
				c.Request = c.Request.WithContext(ctx)
				result, err := svc.ForwardAsAnthropic(ctx, c, account, body, "ai-route", "gpt-5.1")
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, "gpt-5.1", result.UpstreamModel)
				require.Equal(t, int32(1), requests.Load())
				if stream {
					var toolID string
					for _, line := range strings.Split(rec.Body.String(), "\n") {
						if strings.HasPrefix(line, "data: ") {
							event := gjson.Parse(strings.TrimPrefix(line, "data: "))
							if event.Get("type").Str == "content_block_start" && event.Get("content_block.type").Str == "tool_use" {
								toolID = event.Get("content_block.id").Str
							}
						}
					}
					require.Equal(t, "call_selected", toolID, rec.Body.String())
					require.Contains(t, rec.Body.String(), "event: message_stop")
				} else {
					require.Equal(t, "call_selected", gjson.GetBytes(rec.Body.Bytes(), "content.0.id").Str)
					require.Equal(t, "tool_use", gjson.GetBytes(rec.Body.Bytes(), "content.0.type").Str)
				}
			})
		}
	}
}

func TestAIRoutingForwardNeverReplaysAfterConvertedToolOutput(t *testing.T) {
	body := reasoningReplayMessagesBody(true, "gAAAA-stale")
	c, rec := reasoningReplayContext(body)
	upstream := &httpUpstreamRecorder{resp: reasoningReplayHTTPResponse(200, aiExecutionResponsesToolSSE(true))}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
	account := rawChatCompletionsTestAccount()
	target := account.ID
	ctx := WithCompositeRouteDecision(context.Background(), CompositeRouteDecision{Matched: true, GroupID: 10, PublicModel: "public", UpstreamModel: "gpt-5.1", Source: CompositeRouteSourceAI, AICandidateID: "selected", TargetAccountID: &target, TargetPlatform: PlatformOpenAI})
	c.Request = c.Request.WithContext(ctx)
	_, err := svc.ForwardAsAnthropic(ctx, c, account, body, "ai-route", "gpt-5.1")
	require.Error(t, err)
	require.Len(t, upstream.bodies, 1, "even the reasoning recovery path must not replay an emitted tool turn")
	require.Contains(t, rec.Body.String(), "call_selected")
	require.Contains(t, rec.Body.String(), "event: error")
	require.Equal(t, 1, strings.Count(rec.Body.String(), "event: message_start"))
}
