package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type aiEndpointAdminAccounts struct{ service.AccountRepository }

func (*aiEndpointAdminAccounts) GetByID(_ context.Context, id int64) (*service.Account, error) {
	platform := service.PlatformOpenAI
	if id == 3 {
		platform = service.PlatformAnthropic
	}
	return &service.Account{ID: id, Platform: platform, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true}, nil
}

func aiEndpointAdminHandler() (*GroupHandler, service.AIRoutingPolicy, *aiAdminChooser, *adminAIPolicyStore) {
	groups := &aiAdminGroupRepo{group: &service.Group{ID: 8, Platform: service.PlatformComposite, AIRouting: true, Status: "inactive", AllowMessagesDispatch: false}}
	accounts := &aiEndpointAdminAccounts{}
	billing := service.NewBillingService(nil, nil)
	gateway := service.NewGatewayService(accounts, groups, nil, nil, nil, nil, nil, nil, nil, nil, nil, billing, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	policy := service.NewAIRoutingDraft(8)
	decision, execution := int64(3), int64(4)
	policy.SchemaVersion, policy.EnabledEndpoints = 2, []string{"responses"}
	policy.ContextMode = "metadata"
	policy.DecisionAccountID, policy.DecisionPlatform, policy.DecisionModel = &decision, service.PlatformAnthropic, "claude-sonnet-4"
	policy.Candidates = []service.AIRoutingCandidate{{ID: "a", Platform: service.PlatformOpenAI, Model: "gpt-5.4", TargetAccountID: &execution, Enabled: true, ContextWindowTokens: 128000}}
	policy.FallbackID = "a"
	store := &adminAIPolicyStore{state: service.AIRoutingPolicyState{Draft: policy}}
	chooser := &aiAdminChooser{id: "a"}
	return &GroupHandler{aiPolicies: service.NewAIRoutingPolicyService(store, groups, accounts, gateway, nil), aiDecider: chooser}, policy, chooser, store
}

func TestAIRoutingAdminResponsesSampleEndpointIsExplicitAndEchoed(t *testing.T) {
	for _, operation := range []string{"preview", "test-decision"} {
		t.Run(operation, func(t *testing.T) {
			h, p, chooser, _ := aiEndpointAdminHandler()
			router := gin.New()
			router.POST("/groups/:id/ai-routing/preview", h.PreviewAIRouting)
			router.POST("/groups/:id/ai-routing/test-decision", h.TestAIRouting)
			for _, endpoint := range []string{"responses", "", "messages", "responses_ws", "count_tokens"} {
				body, err := json.Marshal(aiPolicyEdit{Policy: &p, Sample: `{"input":"synthetic request","max_output_tokens":100}`, SampleEndpoint: endpoint})
				require.NoError(t, err)
				w := httptest.NewRecorder()
				router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/groups/8/ai-routing/"+operation, strings.NewReader(string(body))))
				if endpoint == "responses" {
					require.Equal(t, 200, w.Code, w.Body.String())
					require.Contains(t, w.Body.String(), `"sample_endpoint":"responses"`)
					require.Contains(t, w.Body.String(), `"external_call":false`)
					if operation == "preview" {
						require.Contains(t, w.Body.String(), `"eligible":["a"]`, w.Body.String())
					}
				} else {
					require.Equal(t, 400, w.Code, w.Body.String())
				}
			}
			require.Zero(t, chooser.calls)
		})
	}
}

func TestAIRoutingAdminLegacySampleDefaultsToMessages(t *testing.T) {
	h, p, chooser := aiAdminTestHandler()
	p.Candidates, p.FallbackID = p.Candidates[:1], "a"
	router := gin.New()
	router.POST("/groups/:id/ai-routing/preview", h.PreviewAIRouting)
	router.POST("/groups/:id/ai-routing/test-decision", h.TestAIRouting)
	for _, operation := range []string{"preview", "test-decision"} {
		body, err := json.Marshal(aiPolicyEdit{Policy: &p, Sample: "continue"})
		require.NoError(t, err)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/groups/8/ai-routing/"+operation, strings.NewReader(string(body))))
		require.Equal(t, 200, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), `"sample_endpoint":"messages"`)
	}
	require.Zero(t, chooser.calls)
}

func TestAIRoutingAdminBindsAllFiveSampleEndpoints(t *testing.T) {
	for _, endpoint := range []string{"messages", "responses", "chat_completions", "responses_ws", "gemini"} {
		t.Run(endpoint, func(t *testing.T) {
			router := gin.New()
			router.POST("/preview", func(c *gin.Context) {
				req, ok := bindAIPolicyEdit(c, false, true)
				if ok {
					c.JSON(200, gin.H{"endpoint": req.SampleEndpoint})
				}
			})
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/preview", strings.NewReader(`{"policy":{},"sample_endpoint":"`+endpoint+`"}`)))
			require.Equal(t, 200, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), endpoint)
		})
	}
}

func TestAIRoutingAdminResponsesTrialKeepsClientAndDecisionProtocolsSeparate(t *testing.T) {
	h, p, chooser, _ := aiEndpointAdminHandler()
	other := int64(5)
	candidate := p.Candidates[0]
	candidate.ID, candidate.TargetAccountID = "b", &other
	p.Candidates = append(p.Candidates, candidate)
	p.ContextMode = "task_excerpt"
	body, err := json.Marshal(aiPolicyEdit{Policy: &p, SampleEndpoint: "responses", Sample: `{"input":[{"role":"user","content":"synthetic user task"}],"instructions":"system secret","max_output_tokens":100}`})
	require.NoError(t, err)
	router := gin.New()
	router.POST("/groups/:id/ai-routing/test-decision", h.TestAIRouting)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/groups/8/ai-routing/test-decision", strings.NewReader(string(body))))
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"sample_endpoint":"responses"`)
	require.Contains(t, w.Body.String(), `"protocol":"`+service.APIProtocolAnthropic+`"`)
	require.Equal(t, 1, chooser.calls)
	require.Contains(t, chooser.task, "synthetic user task")
	require.NotContains(t, chooser.task, "system secret")
}

func TestAIRoutingAdminPublishChecksEveryEnabledEndpointAndFallback(t *testing.T) {
	h, p, _, store := aiEndpointAdminHandler()
	ctx := context.Background()
	state, err := h.aiPolicies.SaveDraft(ctx, 8, 0, p)
	require.NoError(t, err)
	state, err = h.aiPolicies.Publish(ctx, 8, state.Revision)
	require.NoError(t, err, "Responses-only publication must not require Messages dispatch")
	require.True(t, state.Published.AllowsEndpoint("responses"))
	require.False(t, state.Published.AllowsEndpoint("messages"))
	liveVersion := state.Published.Version
	p.EnabledEndpoints = []string{"responses", "messages"}
	state, err = h.aiPolicies.SaveDraft(ctx, 8, state.Revision, p)
	require.NoError(t, err)
	_, err = h.aiPolicies.Publish(ctx, 8, state.Revision)
	require.ErrorContains(t, err, "messages:")
	require.Equal(t, liveVersion, store.state.Published.Version)
	p.EnabledEndpoints, p.FallbackID = []string{"responses"}, "missing"
	state, err = h.aiPolicies.SaveDraft(ctx, 8, state.Revision, p)
	require.NoError(t, err)
	_, err = h.aiPolicies.Publish(ctx, 8, state.Revision)
	require.ErrorContains(t, err, "responses:")
	require.ErrorContains(t, err, "Fallback")
	require.Equal(t, liveVersion, store.state.Published.Version)
}
