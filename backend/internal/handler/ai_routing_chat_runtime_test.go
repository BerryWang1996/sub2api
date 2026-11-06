package handler

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAIRoutingChatRuntimeCreateAndContinuation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream_%v", stream), func(t *testing.T) {
			f := newAIHTTPFixture(t)
			f.policies.policy.SchemaVersion = 2
			f.policies.policy.EnabledEndpoints = []string{"messages", "responses", "chat_completions"}
			f.chat = func(c *gin.Context, stream, _ bool) {
				if !stream {
					c.Data(200, "application/json", []byte(`{"id":"chatcmpl_1","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_chat_1","type":"function","function":{"name":"scan","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`))
					return
				}
				c.Header("Content-Type", "text/event-stream")
				_, _ = c.Writer.WriteString("data: {\"id\":\"chatcmpl_1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_chat_1\",\"type\":\"function\",\"function\":{\"name\":\"scan\",\"arguments\":\"{}\"}}]},\"finish_reason\":null}]}\n\n")
				c.Writer.Flush()
				_, _ = c.Writer.WriteString("data: {\"id\":\"chatcmpl_1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			}
			body := fmt.Sprintf(`{"model":"arbitrary","max_completion_tokens":100,"stream":%v,"messages":[{"role":"system","content":"private system"},{"role":"user","content":"review idempotency"}],"tools":[{"type":"function","function":{"name":"scan","parameters":{"type":"object"}}}]}`, stream)
			created := f.send("/v1/chat/completions", body, false)
			require.Equal(t, http.StatusOK, created.Code, created.Body.String())
			require.Contains(t, created.Body.String(), "call_chat_1")
			require.Equal(t, 1, f.chooser.calls)
			require.NotContains(t, f.chooser.seenTask, "private system")
			require.Equal(t, "private system", gjson.GetBytes(f.forwardedBody, "messages.0.content").String())
			require.Equal(t, "picked-executor", gjson.GetBytes(f.forwardedBody, "model").String())
			follow := `{"model":"another-alias","max_completion_tokens":100,"messages":[{"role":"assistant","tool_calls":[{"id":"call_chat_1","type":"function","function":{"name":"scan","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_chat_1","content":"private tool output"}]}`
			resumed := f.send("/v1/chat/completions", follow, false)
			require.Equal(t, http.StatusOK, resumed.Code, resumed.Body.String())
			require.Equal(t, 1, f.chooser.calls)
			require.EqualValues(t, 75, f.scope.BoundAccountID)
			require.Equal(t, "another-alias", f.scope.PublicModel)
			require.Contains(t, string(f.forwardedBody), "private tool output")
			f.keyID++
			foreign := f.send("/v1/chat/completions", follow, false)
			require.Equal(t, http.StatusConflict, foreign.Code)
			f.keyID--
			f.pool = f.pool[:1]
			revoked := f.send("/v1/chat/completions", follow, false)
			require.Equal(t, http.StatusConflict, revoked.Code)
			require.Equal(t, 2, f.executions)
		})
	}
}

func TestAIRoutingChatRuntimeLegacyPolicyIsClosed(t *testing.T) {
	f := newAIHTTPFixture(t)
	response := f.send("/v1/chat/completions", `{"model":"auto","messages":[{"role":"user","content":"hello"}]}`, false)
	require.Equal(t, http.StatusBadRequest, response.Code)
	require.Contains(t, response.Body.String(), "ai_routing_endpoint_disabled")
	require.Zero(t, f.executions)
	require.Zero(t, f.chooser.calls)
}
