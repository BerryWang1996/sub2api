package handler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func newAIResponsesHTTPFixture(t *testing.T) *aiHTTPFixture {
	f := newAIHTTPFixture(t)
	f.policies.policy.SchemaVersion = 2
	f.policies.policy.EnabledEndpoints = []string{"messages", "responses"}
	f.runtime.responseNative = func(context.Context, int64, string) (bool, error) { return true, nil }
	f.responses = func(c *gin.Context, stream, tools bool) {
		id := fmt.Sprintf("resp_ai_%d", f.executions)
		output := fmt.Sprintf(`{"type":"message","id":"msg_ai_%d","role":"assistant","content":[{"type":"output_text","text":"synthetic"}]}`, f.executions)
		if tools {
			output = fmt.Sprintf(`{"type":"function_call","id":"fc_ai_%d","call_id":"call_ai_%d","name":"scan","arguments":"{}"}`, f.executions, f.executions)
		}
		if !stream {
			c.Header("Content-Type", "application/json")
			_, _ = fmt.Fprintf(c.Writer, `{"id":%q,"object":"response","status":"completed","output":[%s]}`, id, output)
			return
		}
		c.Header("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(c.Writer, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":%q,\"output\":[]}}\n\n", id)
		c.Writer.Flush()
		_, _ = fmt.Fprintf(c.Writer, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":%s}\n\n", output)
		_, _ = fmt.Fprintf(c.Writer, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":%q,\"status\":\"completed\",\"output\":[%s]}}\n\n", id, output)
	}
	return f
}

func TestAIRoutingResponsesRuntimeCreateAndStrictContinuation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream_%v", stream), func(t *testing.T) {
			f := newAIResponsesHTTPFixture(t)
			f.requireDecisionSlot = true
			body := fmt.Sprintf(`{"model":"any-client-alias","stream":%v,"max_output_tokens":100,"instructions":"private-instruction","tools":[{"type":"function","name":"scan","parameters":{"type":"object"}}],"input":"check idempotency"}`, stream)
			first := f.send("/v1/responses", body, false)
			require.Equal(t, http.StatusOK, first.Code, first.Body.String())
			require.Contains(t, first.Body.String(), "resp_ai_1")
			require.Contains(t, first.Body.String(), "call_ai_1")
			require.Equal(t, 1, f.chooser.calls)
			require.Equal(t, 1, f.releases)
			require.Equal(t, "picked-executor", gjson.GetBytes(f.forwardedBody, "model").String())
			require.Equal(t, "private-instruction", gjson.GetBytes(f.forwardedBody, "instructions").String())
			require.Equal(t, "check idempotency", gjson.GetBytes(f.forwardedBody, "input").String())
			require.NotContains(t, f.chooser.seenTask, "private-instruction")
			follow := `{"model":"another-allowed-alias","max_output_tokens":100,"previous_response_id":"resp_ai_1","input":[{"type":"function_call_output","call_id":"call_ai_1","output":"private-tool-output"}]}`
			second := f.send("/v1/responses", follow, false)
			require.Equal(t, http.StatusOK, second.Code, second.Body.String())
			require.Equal(t, 1, f.chooser.calls, "continuation does not call a decision model")
			require.Equal(t, int64(75), f.scope.BoundAccountID)
			require.Equal(t, "B", f.scope.AICandidateID)
			require.Contains(t, string(f.forwardedBody), "private-tool-output")
			f.keyID++
			foreign := f.send("/v1/responses", follow, false)
			require.Equal(t, http.StatusConflict, foreign.Code)
			require.Equal(t, 2, f.executions)
			f.keyID--
			f.pool = f.pool[:1]
			revoked := f.send("/v1/responses", follow, false)
			require.Equal(t, http.StatusConflict, revoked.Code)
			require.Equal(t, 2, f.executions)
		})
	}
}

func TestAIRoutingResponsesRuntimeLegacyGateAndStatelessOrigin(t *testing.T) {
	f := newAIResponsesHTTPFixture(t)
	f.policies.policy.SchemaVersion, f.policies.policy.EnabledEndpoints = 0, nil
	blocked := f.send("/v1/responses", `{"model":"auto","input":"hi"}`, false)
	require.Equal(t, http.StatusBadRequest, blocked.Code)
	require.Zero(t, f.executions)
	f.policies.policy.SchemaVersion = 2
	f.policies.policy.EnabledEndpoints = []string{"responses"}
	f.runtime.responseNative = func(context.Context, int64, string) (bool, error) { return false, nil }
	created := f.send("/v1/responses", `{"model":"auto","store":false,"max_output_tokens":100,"input":"hi"}`, false)
	require.Equal(t, http.StatusOK, created.Code, created.Body.String())
	resumed := f.send("/v1/responses", `{"model":"auto","max_output_tokens":100,"previous_response_id":"resp_ai_1","input":"continue"}`, false)
	require.Equal(t, http.StatusConflict, resumed.Code)
	require.Equal(t, 1, f.executions)
}

func TestAIRoutingResponsesRuntimeRechecksDeletionAtFinalSend(t *testing.T) {
	f := newAIResponsesHTTPFixture(t)
	created := f.send("/v1/responses", `{"model":"auto","max_output_tokens":100,"input":"task"}`, false)
	require.Equal(t, http.StatusOK, created.Code, created.Body.String())
	var checked bool
	f.responses = func(c *gin.Context, _, _ bool) {
		// Simulate deletion while this continuation was waiting for an execution
		// slot, after its initial origin lookup and before the actual HTTP send.
		revoker, ok := f.runtime.store.(service.AIRoutingOriginRevoker)
		require.True(t, ok)
		key := service.AIRoutingOriginKey(f.group.ID, 42, f.keyID, "response", "resp_ai_1")
		require.NoError(t, revoker.RevokeOriginBinding(c.Request.Context(), key, service.AIRoutingResponseOriginTTL))
		upstream, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, "https://synthetic.invalid/v1/responses", strings.NewReader(string(f.forwardedBody)))
		require.NoError(t, err)
		err = service.CheckAIRoutingOutboundRequest(upstream, 75)
		var refusal *service.AIRoutingCapacityError
		require.ErrorAs(t, err, &refusal)
		require.Equal(t, "execution_policy_changed", refusal.Check.Reason)
		checked = true
		c.JSON(http.StatusConflict, gin.H{"error": gin.H{"code": "origin_deleted"}})
	}
	follow := f.send("/v1/responses", `{"model":"auto","max_output_tokens":100,"previous_response_id":"resp_ai_1","input":"continue"}`, false)
	require.True(t, checked)
	require.Equal(t, http.StatusConflict, follow.Code)
	require.Equal(t, 1, f.chooser.calls)
}
