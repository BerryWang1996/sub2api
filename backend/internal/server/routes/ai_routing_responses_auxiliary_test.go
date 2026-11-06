package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAIResponsesAuxiliaryCountCannotFallThroughToGeneration(t *testing.T) {
	for _, base := range []string{"/v1/responses", "/responses", "/backend-api/codex/responses"} {
		for _, tc := range []struct {
			platform string
			ai, deny bool
		}{{service.PlatformAnthropic, true, true}, {service.PlatformGemini, true, true}, {service.PlatformOpenAI, true, false}, {service.PlatformGrok, true, false}, {service.PlatformAnthropic, false, false}} {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, base+"/input_tokens", nil)
			c.Request = c.Request.WithContext(service.WithResolvedTargetPlatform(c.Request.Context(), tc.platform))
			c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{Group: &service.Group{Platform: service.PlatformComposite, AIRouting: tc.ai}})
			require.Equal(t, tc.deny, rejectUnsupportedAIResponsesInputTokens(c))
			require.Equal(t, tc.deny, c.IsAborted())
			if tc.deny {
				require.Equal(t, 422, rec.Code)
				require.NotContains(t, rec.Body.String(), `"input_tokens":`)
				require.Contains(t, rec.Body.String(), `"count_source":"unsupported"`)
			}
		}
	}
}
