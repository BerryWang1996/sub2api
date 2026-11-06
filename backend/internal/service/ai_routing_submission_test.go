//go:build unit

package service

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAIBackgroundSubmissionReportsSafeActionableErrors(t *testing.T) {
	for _, tc := range []struct {
		cause  error
		status int
		code   string
	}{
		{ErrInsufficientBalance, 403, "ai_background_quota_exceeded"},
		{ErrDailyLimitExceeded, 403, "ai_background_quota_exceeded"},
		{ErrAPIKeyRateLimit5hExceeded, 429, "ai_background_capacity_exceeded"},
		{ErrAIBackgroundCapacity, 429, "ai_background_capacity_exceeded"},
		{ErrAIBackgroundUnsupported, 400, "ai_background_unsupported"},
		{ErrAIRoutingPrincipalRevoked, 403, "ai_background_authorization_revoked"},
		{errors.New("private internal database details"), 503, "ai_background_submission_unavailable"},
	} {
		t.Run(tc.code+tc.cause.Error(), func(t *testing.T) {
			ctx := WithAIRoutingBackgroundSubmission(t.Context(), func(*http.Request, int64) error { return tc.cause })
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
			err := BeginAIRoutingBackgroundSubmission(req, 1)
			require.True(t, IsAIRoutingCapacityError(err))
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = req
			require.True(t, handleAIRoutingCapacityError(c, err))
			require.Equal(t, tc.status, w.Code)
			require.Contains(t, w.Body.String(), tc.code)
			require.NotContains(t, w.Body.String(), "private internal database details")
		})
	}
}

func TestAIRoutingSourceRevokedUsesClientProtocol(t *testing.T) {
	for _, endpoint := range []string{CompositeRouteEndpointMessages, CompositeRouteEndpointResponses, CompositeRouteEndpointChatCompletions, CompositeRouteEndpointGemini} {
		t.Run(endpoint, func(t *testing.T) {
			for _, stream := range []bool{false, true} {
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest(http.MethodPost, "/local", nil)
				c.Request = c.Request.WithContext(WithAIRoutingExecutionRequirements(c.Request.Context(), AIRoutingExecutionRequirements{Endpoint: endpoint}))
				if stream {
					c.Header("Content-Type", "text/event-stream")
					c.Writer.WriteHeaderNow()
				}
				require.True(t, handleAIRoutingCapacityError(c, &AIRoutingCapacityError{Check: AIRoutingCapabilityCheck{Reason: "ai_source_revoked"}}))
				if !stream {
					require.Equal(t, http.StatusForbidden, w.Code)
				}
				if endpoint == CompositeRouteEndpointGemini {
					require.Contains(t, w.Body.String(), `"status":"PERMISSION_DENIED"`)
					require.Contains(t, w.Body.String(), `"code":403`)
				} else {
					require.Contains(t, w.Body.String(), `"code":"ai_source_revoked"`)
				}
			}
		})
	}
}
