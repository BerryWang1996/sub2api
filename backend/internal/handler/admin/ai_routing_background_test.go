//go:build unit

package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type adminBackgroundDiagnostics struct {
	service.AIRoutingBackgroundRepository
	groupID      int64
	status       string
	limit, calls int
}

func (r *adminBackgroundDiagnostics) ListBackgroundDiagnostics(_ context.Context, id int64, status string, limit int) ([]service.AIRoutingBackgroundDiagnostic, error) {
	r.groupID, r.status, r.limit = id, status, limit
	r.calls++
	return []service.AIRoutingBackgroundDiagnostic{{ID: "safe-job", SourceGroupID: id, Status: service.AIBackgroundSubmissionUnknown, UsageRequestID: "responses-background:safe-job"}}, nil
}

func TestAIRoutingBackgroundDiagnosticsAreGroupScopedAndReadOnly(t *testing.T) {
	draft := service.NewAIRoutingDraft(8)
	policies := &adminAIPolicyStore{state: service.AIRoutingPolicyState{Draft: draft}}
	repo := &adminBackgroundDiagnostics{}
	h := &GroupHandler{aiPolicies: service.NewAIRoutingPolicyService(policies, nil, nil, nil, nil), aiBackground: service.NewAIRoutingResponsesBackgroundService(repo, nil)}
	router := gin.New()
	router.GET("/groups/:id/ai-routing/background-jobs", h.AIRoutingBackgroundJobs)
	for _, tc := range []struct {
		path   string
		status int
	}{{"/groups/8/ai-routing/background-jobs?status=submission_unknown", 200}, {"/groups/8/ai-routing/background-jobs?status=anything", 400}, {"/groups/invalid/ai-routing/background-jobs", 400}} {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
		require.Equal(t, tc.status, w.Code, w.Body.String())
		if tc.status == 200 {
			require.Contains(t, w.Body.String(), "responses-background:safe-job")
			require.NotContains(t, w.Body.String(), "pricing_snapshot")
		}
	}
	require.Equal(t, 1, repo.calls)
	require.EqualValues(t, 8, repo.groupID)
	require.Equal(t, "submission_unknown", repo.status)
	require.Equal(t, 50, repo.limit)
	h.aiBackground = nil
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/groups/8/ai-routing/background-jobs", nil))
	require.Equal(t, 500, w.Code)
}
