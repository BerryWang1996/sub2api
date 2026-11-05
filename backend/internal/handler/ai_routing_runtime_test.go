package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/testutil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type aiHTTPPolicy struct {
	policy *service.AIRoutingPolicy
	err    error
	reads  int
}

func (p *aiHTTPPolicy) GetPublished(context.Context, int64) (*service.AIRoutingPolicy, error) {
	p.reads++
	return p.policy, p.err
}

type aiHTTPChooser struct {
	calls    int
	seenTask string
	choose   string
	err      error
	onDecide func()
}

func (f *aiHTTPChooser) Decide(_ context.Context, _ *service.AIRoutingPolicy, task string, _ []service.AIRoutingCandidate, _ string) (service.AIRoutingDecisionResult, error) {
	f.calls++
	f.seenTask = task
	if f.onDecide != nil {
		f.onDecide()
	}
	return service.AIRoutingDecisionResult{CandidateID: f.choose}, f.err
}

type aiHTTPFixture struct {
	engine                           *gin.Engine
	group                            *service.Group
	policies                         *aiHTTPPolicy
	chooser                          *aiHTTPChooser
	pool                             []service.AIRoutingCandidate
	admissions, releases, executions int
	requireDecisionSlot              bool
	boundObserved                    int64
	rewritten                        string
	scope                            service.CompositeRouteScope
	hasScope                         bool
	keyID                            int64
	rejectAdmission                  bool
}

func newAIHTTPFixture(t *testing.T) *aiHTTPFixture {
	t.Helper()
	store, _ := testutil.NewRedisAIRoutingStore(t)
	groupID := int64(17)
	targetGroup := int64(61)
	targetAccount := int64(83)
	f := &aiHTTPFixture{
		group:    &service.Group{ID: groupID, Platform: service.PlatformComposite, AIRouting: true, Status: service.StatusActive},
		policies: &aiHTTPPolicy{policy: &service.AIRoutingPolicy{GroupID: groupID, Enabled: true, Version: 3, ContextMode: "task_excerpt"}},
		chooser:  &aiHTTPChooser{choose: "B"}, keyID: 9,
		pool: []service.AIRoutingCandidate{
			{ID: "A", Enabled: true, Platform: service.PlatformAnthropic, Model: "other-executor", TargetAccountID: &targetAccount},
			{ID: "B", Enabled: true, Platform: service.PlatformOpenAI, Model: "picked-executor", TargetGroupID: &targetGroup},
		}}
	runtime := &aiRoutingRuntime{policies: f.policies, decider: f.chooser, store: store}
	runtime.eligible = func(_ context.Context, key *service.APIKey, p *service.AIRoutingPolicy, _ service.AIRoutingRequest, _ string, bound int64) []service.AIRoutingCandidate {
		require.Equal(t, groupID, key.Group.ID)
		require.Equal(t, p.GroupID, key.Group.ID)
		f.boundObserved = bound
		return append([]service.AIRoutingCandidate(nil), f.pool...)
	}
	runtime.admit = func(c *gin.Context, _ *service.APIKey, _ []byte, _ string) (func(), bool) {
		f.admissions++
		if f.rejectAdmission {
			abortAI(c, 403, "denied", "denied")
			return nil, false
		}
		release := func() { f.releases++ }
		return release, true
	}
	h := &GatewayHandler{aiRouting: runtime, gatewayService: &service.GatewayService{}}
	f.engine = gin.New()
	f.engine.Use(func(c *gin.Context) {
		c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{ID: f.keyID, UserID: 42, GroupID: &groupID, Group: f.group})
	})
	f.engine.Use(middleware.GroupModelAllowlist(), h.AIRoute)
	execute := func(c *gin.Context) {
		f.executions++
		raw, err := io.ReadAll(c.Request.Body)
		require.NoError(t, err)
		var req struct {
			Model  string
			Stream bool
			Tools  []json.RawMessage
		}
		require.NoError(t, json.Unmarshal(raw, &req))
		f.rewritten = req.Model
		f.scope, f.hasScope = service.CompositeRouteScopeFromContext(c.Request.Context())
		if f.requireDecisionSlot {
			require.Equal(t, f.admissions, f.releases, "decision user slot must be released before downstream execution")
		}
		c.Set(opsAccountIDKey, int64(75))
		if len(req.Tools) == 0 {
			c.JSON(http.StatusOK, gin.H{"content": []any{}})
			return
		}
		if req.Stream {
			c.Header("Content-Type", "text/event-stream")
			_, _ = c.Writer.WriteString("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"tool_use\",")
			c.Writer.Flush()
			_, _ = c.Writer.Write([]byte("\"id\":\"tool-1\"}}\n\n"))
		} else {
			c.Header("Content-Type", "application/json")
			_, _ = c.Writer.WriteString(`{"type":"message","content":[{"type":"tool_use",`)
			_, _ = c.Writer.Write([]byte(`"id":"tool-1"}]}`))
		}
	}
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens", "/v1/responses", "/antigravity/v1/messages"} {
		f.engine.POST(path, execute)
	}
	return f
}
func (f *aiHTTPFixture) send(path, body string, internal bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if internal {
		r.Header.Set(service.AIRoutingInternalHeader, "1")
	}
	w := httptest.NewRecorder()
	f.engine.ServeHTTP(w, r)
	return w
}

const aiHTTPTask = `{"model":"any-client-alias","messages":[{"role":"user","content":"Check idempotency"}]}`
const aiHTTPContinuation = `{"model":"any-client-alias","messages":[{"role":"user","content":"Check idempotency"},{"role":"assistant","content":[{"type":"tool_use","name":"scan","id":"tool-1","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"private-tool-output"}]}]}`

func TestAIRoutingMessagesDispatchPreservesDecisionOverGroupDefaults(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	key := &service.APIKey{Group: &service.Group{ID: 17, Platform: service.PlatformComposite,
		MessagesDispatchModelConfig: service.OpenAIMessagesDispatchModelConfig{ExactModelMappings: map[string]string{"claude-sonnet-4-5": "gpt-5.4"}},
	}}
	target := int64(61)
	service.SetCompositeRouteDecision(c, service.CompositeRouteDecision{Matched: true, Source: service.CompositeRouteSourceAI, AICandidateID: "B", GroupID: 17, PublicModel: "claude-sonnet-4-5", UpstreamModel: "gpt-5.1", TargetPlatform: service.PlatformOpenAI, TargetGroupID: &target})
	require.Equal(t, "gpt-5.1", resolveOpenAIMessagesDispatchMappedModel(c, key, "claude-sonnet-4-5"))
	// Regular static Composite routes still use their existing dispatch settings.
	service.SetCompositeRouteDecision(c, service.CompositeRouteDecision{Matched: true, Source: service.CompositeRouteSourceExplicit, GroupID: 17, PublicModel: "claude-sonnet-4-5", UpstreamModel: "gpt-5.1", TargetPlatform: service.PlatformOpenAI, TargetGroupID: &target})
	require.Equal(t, "gpt-5.4", resolveOpenAIMessagesDispatchMappedModel(c, key, "claude-sonnet-4-5"))
}

func TestAIRoutingHTTPToolContinuations(t *testing.T) {
	for _, mode := range []string{"true", "false"} {
		t.Run("stream="+mode, func(t *testing.T) {
			f := newAIHTTPFixture(t)
			f.requireDecisionSlot = true
			body := `{"model":"any-client-alias","stream":` + mode + `,"tools":[{"name":"scan","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"Check idempotency"}]}`
			first := f.send("/v1/messages", body, false)
			require.Equal(t, 200, first.Code, first.Body.String())
			require.Contains(t, first.Body.String(), "tool-1")
			require.Equal(t, 1, f.chooser.calls)
			require.Equal(t, 1, f.admissions)
			require.Contains(t, f.chooser.seenTask, "idempotency")
			require.Equal(t, "picked-executor", f.rewritten)
			require.True(t, f.hasScope)
			require.Equal(t, "any-client-alias", f.scope.PublicModel)
			require.Equal(t, "B", f.scope.AICandidateID)
			require.Equal(t, int64(61), f.scope.TargetGroupID)
			follow := f.send("/v1/messages", aiHTTPContinuation, false)
			require.Equal(t, 200, follow.Code, follow.Body.String())
			require.Equal(t, 1, f.chooser.calls)
			require.Equal(t, 1, f.admissions)
			require.Equal(t, int64(75), f.boundObserved)
			require.Equal(t, int64(75), f.scope.BoundAccountID)
			require.Equal(t, "picked-executor", f.rewritten)
			f.keyID++
			foreign := f.send("/v1/messages", aiHTTPContinuation, false)
			require.Equal(t, 409, foreign.Code)
			require.Contains(t, foreign.Body.String(), "ai_tool_binding_unavailable")
			f.keyID--
			f.pool = f.pool[:1]
			revoked := f.send("/v1/messages", aiHTTPContinuation, false)
			require.Equal(t, 409, revoked.Code)
			require.Contains(t, revoked.Body.String(), "ai_tool_binding_revoked")
			require.Equal(t, 2, f.executions)
			require.Equal(t, 1, f.chooser.calls)
		})
	}
}

func TestAIRoutingHTTPGatesAndStaticCompatibility(t *testing.T) {
	cases := []struct {
		name, path string
		configure  func(*aiHTTPFixture)
		code       int
		errorCode  string
		internal   bool
	}{
		{"disabled", "/v1/messages", func(f *aiHTTPFixture) { f.policies.policy.Enabled = false }, 403, "ai_routing_disabled", false},
		{"missing-policy", "/v1/messages", func(f *aiHTTPFixture) { f.policies.policy = nil }, 503, "ai_policy_unavailable", false},
		{"lookup-failure", "/v1/messages", func(f *aiHTTPFixture) { f.policies.err = errors.New("failed") }, 503, "ai_policy_unavailable", false},
		{"unsupported", "/v1/responses", nil, 400, "ai_routing_endpoint_unsupported", false},
		{"forced-provider", "/antigravity/v1/messages", nil, 400, "ai_routing_endpoint_unsupported", false},
		{"nested", "/v1/messages", nil, 400, "ai_routing_loop", true},
		{"claude-code-only", "/v1/messages", func(f *aiHTTPFixture) { f.group.ClaudeCodeOnly = true }, 403, "permission_error", false},
		{"ambiguous-count", "/v1/messages/count_tokens", nil, 400, "ai_count_target_ambiguous", false},
		{"external-admission-denied", "/v1/messages", func(f *aiHTTPFixture) { f.rejectAdmission = true }, 403, "denied", false},
		{"static", "/v1/messages", func(f *aiHTTPFixture) { f.group.AIRouting = false; f.policies.policy = nil }, 200, "", false},
		{"static-policy-storage-failure", "/v1/messages", func(f *aiHTTPFixture) {
			f.group.AIRouting = false
			f.policies.err = errors.New("AI policy table unavailable")
		}, 200, "", false},
		{"static-unsupported-ai-endpoint", "/v1/responses", func(f *aiHTTPFixture) {
			f.group.AIRouting = false
			f.policies.err = errors.New("AI policy table unavailable")
		}, 200, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAIHTTPFixture(t)
			if tc.configure != nil {
				tc.configure(f)
			}
			w := f.send(tc.path, aiHTTPTask, tc.internal)
			require.Equal(t, tc.code, w.Code, w.Body.String())
			if !f.group.AIRouting {
				require.Zero(t, f.policies.reads, "static Composite routing must not depend on AI policy storage")
			}
			require.Equal(t, 0, f.chooser.calls)
			if tc.errorCode != "" {
				require.Contains(t, w.Body.String(), tc.errorCode)
				require.Zero(t, f.executions)
			} else {
				require.Equal(t, 1, f.executions)
				require.False(t, f.hasScope)
				require.Equal(t, "any-client-alias", f.rewritten)
			}
		})
	}
}
