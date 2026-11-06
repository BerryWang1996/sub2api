//go:build unit

package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAIBackgroundReleaseGateBeforeDecisionAdmissionAndExecution(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/responses", "/backend-api/codex/responses"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprint(path, stream), func(t *testing.T) {
				f := newAIResponsesHTTPFixture(t)
				bg := &aiBackgroundTestService{ledger: &aiBackgroundTestLedger{}}
				eligibleCalls, sends := 0, 0
				f.runtime.eligible = func(context.Context, *service.APIKey, *service.AIRoutingPolicy, service.AIRoutingRequest, string, int64) []service.AIRoutingCandidate {
					eligibleCalls++
					return f.pool
				}
				h := &GatewayHandler{aiRouting: f.runtime, gatewayService: &service.GatewayService{}, aiBackground: bg}
				engine := gin.New()
				engine.Use(func(c *gin.Context) {
					c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: f.keyID, UserID: 42, GroupID: &f.group.ID, Group: f.group})
				}, h.AIRoute)
				engine.POST(path, func(c *gin.Context) { sends++; c.Status(200) })
				body := fmt.Sprintf(`{"model":"alias","background":true,"store":true,"stream":%v,"max_output_tokens":32,"input":"synthetic"}`, stream)
				w := httptest.NewRecorder()
				engine.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
				require.Equal(t, 400, w.Code, w.Body.String())
				require.Contains(t, w.Body.String(), "ai_background_not_released")
				require.Zero(t, eligibleCalls)
				require.Zero(t, f.chooser.calls)
				require.Zero(t, f.admissions)
				require.Zero(t, sends)
				require.Zero(t, bg.submissions)
				require.Nil(t, bg.ledger.job, "even an injected ready development service cannot bypass the production ingress gate")
			})
		}
	}
}

func TestAIBackgroundReleaseGatePreservesForegroundAndOrdinaryRoutes(t *testing.T) {
	for _, flag := range []string{"", `,"background":false`} {
		f := newAIResponsesHTTPFixture(t)
		w := f.send("/v1/responses", `{"model":"alias","store":false,"max_output_tokens":32,"input":"synthetic"`+flag+`}`, false)
		require.Equal(t, 200, w.Code, w.Body.String())
		require.Equal(t, 1, f.executions)
	}
	for _, platform := range []string{service.PlatformComposite, service.PlatformOpenAI} {
		f := newAIResponsesHTTPFixture(t)
		f.group.AIRouting, f.group.Platform = false, platform
		w := f.send("/v1/responses", `{"model":"alias","background":true,"store":true,"max_output_tokens":32,"input":"synthetic"}`, false)
		require.Equal(t, 200, w.Code, w.Body.String())
		require.Zero(t, f.chooser.calls)
		require.Equal(t, 1, f.executions)
	}
}

func TestAIBackgroundReleaseNilServiceBlocksHistoricalControls(t *testing.T) {
	for _, request := range []struct{ method, suffix string }{{http.MethodGet, ""}, {http.MethodGet, "/input_items"}, {http.MethodPost, "/cancel"}, {http.MethodDelete, ""}} {
		t.Run(request.method+request.suffix, func(t *testing.T) {
			key, policy, origin, _, runtime := aiControlFixture(t)
			origin.Background = true
			store := &aiControlStore{}
			require.NoError(t, saveAIResponsesBinding(t.Context(), store, key, service.AIRoutingOriginRef{Kind: "response", ID: origin.ResponseID}, *origin))
			runtime.store = store
			forwarder := &aiControlForwarder{body: `{"id":"resp_A","status":"completed"}`}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(request.method, "/v1/responses/resp_A"+request.suffix, nil)
			require.True(t, routeAIResponsesControlWith(c, runtime, key, policy, &aiControlValidator{}, forwarder, nil))
			require.GreaterOrEqual(t, w.Code, 400)
			require.Zero(t, forwarder.calls)
		})
	}
}
