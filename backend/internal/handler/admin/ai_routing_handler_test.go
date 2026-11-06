package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type adminAIPolicyStore struct{ state service.AIRoutingPolicyState }

func (s *adminAIPolicyStore) Load(context.Context, int64) (*service.AIRoutingPolicyState, error) {
	return &s.state, nil
}
func (s *adminAIPolicyStore) Published(context.Context, int64) (*service.AIRoutingPolicy, error) {
	return s.state.Published, nil
}
func (s *adminAIPolicyStore) Write(_ context.Context, _ int64, version int64, d service.AIRoutingPolicy, p *service.AIRoutingPolicy, status string) (*service.AIRoutingPolicyState, error) {
	if version != s.state.Revision {
		return nil, service.ErrAIRevision
	}
	s.state = service.AIRoutingPolicyState{Draft: d, Published: p, Revision: version + 1}
	return &s.state, nil
}
func TestAIRoutingAdminDraftRequiresRevisionAndPreservesPublishedPolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	live := service.NewAIRoutingDraft(8)
	live.Enabled = true
	live.Version = 2
	live.Prompt = "live"
	store := &adminAIPolicyStore{state: service.AIRoutingPolicyState{Draft: live, Published: &live, Revision: 2}}
	h := &GroupHandler{aiPolicies: service.NewAIRoutingPolicyService(store, nil, nil, nil, nil)}
	r := gin.New()
	r.PUT("/groups/:id/ai-routing", h.SaveAIRoutingDraft)
	draft := live
	draft.Prompt = "edited"
	payload := func(rev *int64) string {
		body, err := json.Marshal(map[string]any{"revision": rev, "policy": draft})
		require.NoError(t, err)
		return string(body)
	}
	version := int64(2)
	for _, tc := range []struct {
		body   string
		status int
	}{{payload(nil), 400}, {payload(&version), 200}, {payload(&version), 409}, {strings.Repeat("x", 256<<10+1), 400}} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/groups/8/ai-routing", strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		require.Equal(t, tc.status, w.Code, w.Body.String())
	}
	require.False(t, store.state.Draft.Enabled)
	require.Equal(t, "live", store.state.Published.Prompt)
}
func TestAIRoutingAdminPolicyUnavailableAndSimpleModeFailClosed(t *testing.T) {
	for _, h := range []*GroupHandler{{}, {cfg: &config.Config{RunMode: config.RunModeSimple}}} {
		r := gin.New()
		r.GET("/groups/:id/ai-routing", h.GetAIRouting)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/groups/8/ai-routing", nil))
		require.GreaterOrEqual(t, w.Code, 400)
	}
}

type aiAdminGroupRepo struct {
	service.GroupRepository
	group *service.Group
}

func (r *aiAdminGroupRepo) GetByIDLite(context.Context, int64) (*service.Group, error) {
	return r.group, nil
}

type aiAdminAccountRepo struct{ service.AccountRepository }

func (*aiAdminAccountRepo) GetByID(_ context.Context, id int64) (*service.Account, error) {
	return &service.Account{ID: id, Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true}, nil
}

type aiAdminChooser struct {
	task  string
	calls int
	id    string
}

func (d *aiAdminChooser) Decide(_ context.Context, _ *service.AIRoutingPolicy, task string, _ []service.AIRoutingCandidate, _ string) (service.AIRoutingDecisionResult, error) {
	d.task = task
	d.calls++
	return service.AIRoutingDecisionResult{CandidateID: d.id, Model: "decision-model", Protocol: service.APIProtocolAnthropic}, nil
}

func aiAdminTestHandler() (*GroupHandler, service.AIRoutingPolicy, *aiAdminChooser) {
	groups := &aiAdminGroupRepo{group: &service.Group{ID: 8, Platform: service.PlatformComposite, AIRouting: true, Status: "inactive"}}
	accounts := &aiAdminAccountRepo{}
	billing := service.NewBillingService(nil, nil)
	gateway := service.NewGatewayService(accounts, groups, nil, nil, nil, nil, nil, nil, nil, nil, nil, billing, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	draft := service.NewAIRoutingDraft(8)
	decisionID, otherID := int64(3), int64(4)
	draft.DecisionAccountID = &decisionID
	draft.DecisionPlatform = service.PlatformAnthropic
	draft.DecisionModel = "claude-sonnet-4"
	draft.FallbackID = "b"
	draft.Candidates = []service.AIRoutingCandidate{
		{ID: "a", Platform: service.PlatformAnthropic, Model: "claude-sonnet-4", TargetAccountID: &decisionID, Enabled: true, ContextWindowTokens: 128000},
		{ID: "b", Platform: service.PlatformAnthropic, Model: "claude-opus-4.5", TargetAccountID: &otherID, Enabled: true, ContextWindowTokens: 128000},
	}
	store := &adminAIPolicyStore{state: service.AIRoutingPolicyState{Draft: draft}}
	chooser := &aiAdminChooser{id: "a"}
	return &GroupHandler{aiPolicies: service.NewAIRoutingPolicyService(store, groups, accounts, gateway, nil), aiDecider: chooser}, draft, chooser
}

func TestAIRoutingAdminSampleHonorsDisclosureAndFiniteChoice(t *testing.T) {
	for _, mode := range []string{"metadata", "task_excerpt"} {
		t.Run(mode, func(t *testing.T) {
			h, draft, chooser := aiAdminTestHandler()
			draft.ContextMode = mode
			for _, requestedID := range []string{"a", "outside"} {
				chooser.id = requestedID
				body, err := json.Marshal(aiPolicyEdit{Policy: &draft, Sample: "private task with password=secretvalue"})
				require.NoError(t, err)
				router := gin.New()
				router.POST("/groups/:id/ai-routing/test-decision", h.TestAIRouting)
				w := httptest.NewRecorder()
				router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/groups/8/ai-routing/test-decision", strings.NewReader(string(body))))
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				if requestedID == "outside" {
					require.Contains(t, w.Body.String(), `"reason":"fallback"`)
					require.Contains(t, w.Body.String(), `"candidate_id":"b"`)
				} else {
					require.Contains(t, w.Body.String(), `"reason":"model"`)
				}
				require.NotContains(t, chooser.task, "secretvalue")
				if mode == "metadata" {
					require.NotContains(t, chooser.task, "private task")
				} else {
					require.Contains(t, chooser.task, "private task")
				}
			}
		})
	}
}

func TestAIRoutingAdminSingleChoiceAndMalformedDraftDoNotCallModel(t *testing.T) {
	h, draft, chooser := aiAdminTestHandler()
	draft.Candidates = draft.Candidates[:1]
	for _, timeout := range []int{3000, -1} {
		draft.TimeoutMS = timeout
		body, err := json.Marshal(aiPolicyEdit{Policy: &draft})
		require.NoError(t, err)
		router := gin.New()
		router.POST("/groups/:id/ai-routing/test-decision", h.TestAIRouting)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/groups/8/ai-routing/test-decision", strings.NewReader(string(body))))
		if timeout > 0 {
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), `"reason":"single"`)
			require.Contains(t, w.Body.String(), `"external_call":false`)
		} else {
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		}
	}
	require.Zero(t, chooser.calls)
}
