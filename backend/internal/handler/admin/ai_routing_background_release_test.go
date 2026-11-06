package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAIBackgroundReleaseAdminPreviewAndTrialRefuseWithoutDecision(t *testing.T) {
	for _, endpoint := range []string{service.CompositeRouteEndpointResponses, service.AIRoutingEndpointResponsesWS} {
		for _, operation := range []string{"preview", "test-decision"} {
			t.Run(endpoint+operation, func(t *testing.T) {
				h, p, chooser, _ := aiEndpointAdminHandler()
				p.EnabledEndpoints = []string{service.CompositeRouteEndpointResponses, service.AIRoutingEndpointResponsesWS}
				// No policy, account or pricing lookup can precede this rejection.
				h.aiPolicies = service.NewAIRoutingPolicyService(nil, nil, nil, nil, nil)
				router := gin.New()
				router.POST("/groups/:id/ai-routing/preview", h.PreviewAIRouting)
				router.POST("/groups/:id/ai-routing/test-decision", h.TestAIRouting)
				body, err := json.Marshal(aiPolicyEdit{Policy: &p, SampleEndpoint: endpoint, Sample: `{"input":"synthetic","background":true,"store":true,"max_output_tokens":32}`})
				require.NoError(t, err)
				w := httptest.NewRecorder()
				router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/groups/8/ai-routing/"+operation, strings.NewReader(string(body))))
				require.Equal(t, 400, w.Code, w.Body.String())
				require.Contains(t, w.Body.String(), `"reason":"ai_background_not_released"`)
				require.Zero(t, chooser.calls)
			})
		}
	}
}
