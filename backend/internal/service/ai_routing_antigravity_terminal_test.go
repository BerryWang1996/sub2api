//go:build unit

package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAIAntigravityCompatRequiresRealTerminal(t *testing.T) {
	partial := "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]}}],\"usageMetadata\":{\"promptTokenCount\":9,\"candidatesTokenCount\":2}}}\n\n"
	for _, test := range []struct {
		name, tail string
		legacy     bool
		wantError  bool
	}{
		{name: "missing finish", wantError: true},
		{name: "early done", tail: "data: [DONE]\n\n", wantError: true},
		{name: "malformed JSON", tail: "data: {broken\n\n", wantError: true},
		{name: "multiple candidates", tail: "data: {\"response\":{\"candidates\":[{\"finishReason\":\"STOP\"},{\"finishReason\":\"STOP\"}]}}\n\n", wantError: true},
		{name: "nonzero candidate", tail: "data: {\"response\":{\"candidates\":[{\"index\":1,\"finishReason\":\"STOP\"}]}}\n\n", wantError: true},
		{name: "null finish", tail: "data: {\"response\":{\"candidates\":[{\"finishReason\":null}]}}\n\n", wantError: true},
		{name: "ambiguous finish", tail: "data: {\"response\":{\"candidates\":[{\"finishReason\":\"STOP\",\"FinishReason\":\"MAX_TOKENS\"}]}}\n\n", wantError: true},
		{name: "duplicate finish", tail: "data: {\"response\":{\"candidates\":[{\"finishReason\":\"STOP\",\"finishReason\":\"MAX_TOKENS\"}]}}\n\n", wantError: true},
		{name: "actual stop and EOF", tail: "data: {\"response\":{\"candidates\":[{\"finishReason\":\"STOP\"}]}}\n\n"},
		{name: "actual maximum and EOF", tail: "data: {\"response\":{\"candidates\":[{\"finishReason\":\"MAX_TOKENS\"}]}}\n\n"},
		{name: "legacy unchanged", legacy: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			ctx := t.Context()
			if !test.legacy {
				ctx = WithAIRoutingExecutionRequirements(ctx, AIRoutingExecutionRequirements{Endpoint: AIRoutingEndpointResponsesWS})
			}
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
			response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(partial + test.tail))}
			svc := &AntigravityGatewayService{}
			result, err := svc.handleResponsesStreamingFromAntigravity(c, response, time.Now(), "gemini-3.1-pro-high")
			require.NotNil(t, result)
			require.Equal(t, 9, result.usage.InputTokens)
			require.Equal(t, 2, result.usage.OutputTokens)
			if test.wantError {
				require.Error(t, err)
				require.NotContains(t, recorder.Body.String(), `"type":"response.completed"`)
				require.NotContains(t, recorder.Body.String(), `"type":"response.incomplete"`)
			} else {
				require.NoError(t, err)
				require.Contains(t, recorder.Body.String(), `"type":"response.`)
				if test.name == "actual maximum and EOF" {
					require.Contains(t, recorder.Body.String(), `"type":"response.incomplete"`)
				} else {
					require.Contains(t, recorder.Body.String(), `"type":"response.completed"`)
				}
			}
		})
	}
}
