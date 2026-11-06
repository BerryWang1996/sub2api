//go:build unit

package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func aiIntegrationReviewTrial(t *testing.T, h *GroupHandler, p service.AIRoutingPolicy, sample string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(aiPolicyEdit{Policy: &p, Sample: sample})
	require.NoError(t, err)
	router := gin.New()
	router.POST("/groups/:id/ai-routing/test-decision", h.TestAIRouting)
	w := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/groups/8/ai-routing/test-decision", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, request)
	return w
}

func TestAIRoutingIntegrationReviewUnboundToolSampleCannotChooseNewOrigin(t *testing.T) {
	h, p, chooser := aiAdminTestHandler()
	p.ContextMode = "bounded_history"
	tools := true
	for i := range p.Candidates {
		p.Candidates[i].SupportsTools = &tools
	}
	sample := `{"model":"auto","max_tokens":100,"messages":[{"role":"user","content":"Review the payment incident"},{"role":"assistant","content":[{"type":"tool_use","id":"foreign-tool","name":"read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"foreign-tool","content":"synthetic output"},{"type":"text","text":"Continue the review"}]}]}`
	body, err := json.Marshal(aiPolicyEdit{Policy: &p, Sample: sample})
	require.NoError(t, err)
	router := gin.New()
	router.POST("/groups/:id/ai-routing/preview", h.PreviewAIRouting)
	preview := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/groups/8/ai-routing/preview", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(preview, request)
	require.Equal(t, http.StatusOK, preview.Code, preview.Body.String())
	require.Empty(t, gjson.Get(preview.Body.String(), "data.eligible").Array())
	require.False(t, gjson.Get(preview.Body.String(), "data.needs_decision").Bool())
	require.Equal(t, "sample_origin_binding_required", gjson.Get(preview.Body.String(), "data.context.failure_reason").String())
	w := aiIntegrationReviewTrial(t, h, p, sample)
	require.Zero(t, chooser.calls, "the sample has no strict Key/origin proof and must not be semantically rerouted")
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "No eligible execution candidate")

	closed := `{"model":"auto","max_tokens":100,"messages":[{"role":"user","content":"Review the payment incident"},{"role":"assistant","content":[{"type":"tool_use","id":"old-tool","name":"read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"old-tool","content":"synthetic output"}]},{"role":"assistant","content":"The review is complete."},{"role":"user","content":"Now translate the release notes."}]}`
	w = aiIntegrationReviewTrial(t, h, p, closed)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, 1, chooser.calls, "completed historical tools must not pin an independent new task")
	require.Equal(t, "model", gjson.Get(w.Body.String(), "data.reason").String())
}

func TestAIRoutingIntegrationReviewPublishRejectsImpossibleDecisionEnvelope(t *testing.T) {
	h, p, _ := aiAdminTestHandler()
	p.ContextMode = "bounded_history"
	p.ContextBudget.MaxInputBytes = 1024
	p.Prompt = strings.Repeat("route-rule ", 200)
	state, err := h.aiPolicies.SaveDraft(context.Background(), p.GroupID, 0, p)
	require.NoError(t, err, "the draft may retain an incomplete configuration")
	_, err = h.aiPolicies.Publish(context.Background(), p.GroupID, state.Revision)
	require.Error(t, err, "even an empty history cannot fit the administrator rules into the declared per-call limit")

	h, p, _ = aiAdminTestHandler()
	p.ContextBudget.MaxInputBytes, p.Prompt = 1024, strings.Repeat("route-rule ", 200)
	p.Candidates, p.FallbackID = p.Candidates[:1], ""
	state, err = h.aiPolicies.SaveDraft(context.Background(), p.GroupID, 0, p)
	require.NoError(t, err)
	_, err = h.aiPolicies.Publish(context.Background(), p.GroupID, state.Revision)
	require.NoError(t, err, "single-candidate execution requires no decision envelope")
}

type aiIntegrationReviewFailedChooser struct{}

func (*aiIntegrationReviewFailedChooser) Decide(context.Context, *service.AIRoutingPolicy, string, []service.AIRoutingCandidate, string) (service.AIRoutingDecisionResult, error) {
	return service.AIRoutingDecisionResult{
		Model: "synthetic-decision-model", Protocol: service.APIProtocolAnthropic, AccountID: 3,
		Attempted: true, DecisionCalls: 1, UsageKnown: false, FailureReason: "upstream_rejected",
	}, errors.New("synthetic response interrupted")
}

func TestAIRoutingIntegrationReviewFailedTrialPreservesUnknownUsage(t *testing.T) {
	h, p, _ := aiAdminTestHandler()
	p.ContextMode, p.FallbackID = "bounded_history", ""
	h.aiDecider = &aiIntegrationReviewFailedChooser{}
	w := aiIntegrationReviewTrial(t, h, p, "Translate the synthetic release note")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "failed", gjson.Get(w.Body.String(), "data.reason").String())
	require.Empty(t, gjson.Get(w.Body.String(), "data.candidate_id").String())
	require.Contains(t, w.Body.String(), `"external_call":true`, "a failed paid attempt must not disappear from the trial response")
	require.Contains(t, w.Body.String(), `"decision_calls":1`)
	require.Contains(t, w.Body.String(), `"usage_known":false`, "unknown metering must stay explicitly unknown")
	require.NotContains(t, w.Body.String(), "Translate the synthetic release note")
}

func TestAIRoutingIntegrationReviewOfflineSampleProjectionIsRedacted(t *testing.T) {
	h, p, chooser := aiAdminTestHandler()
	p.ContextMode = "bounded_history"
	sample := `{"model":"auto","max_tokens":100,"system":"private-system-content","messages":[{"role":"user","content":"Translate notes password=sample-secret-value"},{"role":"assistant","content":[{"type":"text","text":"Use the second option"},{"type":"thinking","thinking":"private-reasoning-content"}]},{"role":"user","content":"Continue"}]}`
	body, err := json.Marshal(aiPolicyEdit{Policy: &p, Sample: sample})
	require.NoError(t, err)
	router := gin.New()
	router.POST("/groups/:id/ai-routing/preview", h.PreviewAIRouting)
	w := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/groups/8/ai-routing/preview", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(w, request)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Zero(t, chooser.calls)
	require.False(t, gjson.Get(w.Body.String(), "data.external_call").Bool())
	spans := gjson.Get(w.Body.String(), "data.sample_spans")
	require.Len(t, spans.Array(), 3)
	require.Contains(t, spans.Raw, "Translate notes")
	require.Contains(t, spans.Raw, "Use the second option")
	for _, secret := range []string{"sample-secret-value", "private-system-content", "private-reasoning-content"} {
		require.NotContains(t, w.Body.String(), secret)
	}
}
