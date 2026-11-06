package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

// The hook models cancellation/revocation while the synchronous trace write is
// in flight. The underlying store remains the fixture's isolated Redis adapter.
type aiTraceHookStore struct {
	service.AIRoutingStore
	onSave func()
	traces []service.AIRoutingTrace
}

func (s *aiTraceHookStore) SaveTrace(ctx context.Context, groupID int64, trace service.AIRoutingTrace) error {
	s.traces = append(s.traces, trace)
	if s.onSave != nil {
		hook := s.onSave
		s.onSave = nil
		hook()
	}
	return s.AIRoutingStore.SaveTrace(ctx, groupID, trace)
}

func aiTraceRequest(t *testing.T, path string) (*aiHTTPFixture, string, string) {
	t.Helper()
	f := newAIHTTPFixture(t)
	switch path {
	case "single", "count":
		f.pool = f.pool[1:]
	case "continuation", "bound-count":
		seed := `{"model":"any-client-alias","tools":[{"name":"scan","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"Check idempotency"}]}`
		response := f.send("/v1/messages", seed, false)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		require.Contains(t, response.Body.String(), "tool-1")
	}
	endpoint, body := "/v1/messages", aiHTTPTask
	if path == "count" || path == "bound-count" {
		endpoint = "/v1/messages/count_tokens"
	}
	if path == "continuation" || path == "bound-count" {
		body = aiHTTPContinuation
	}
	return f, endpoint, body
}

func TestAIRoutingRevalidatesAfterTraceWrite(t *testing.T) {
	for _, requestPath := range []string{"decision", "single", "count", "continuation", "bound-count"} {
		for _, tc := range []struct {
			name   string
			change func(*aiHTTPFixture, context.CancelFunc)
			status int
			code   string
		}{
			{"unchanged", func(*aiHTTPFixture, context.CancelFunc) {}, http.StatusOK, ""},
			{"cancelled", func(_ *aiHTTPFixture, cancel context.CancelFunc) { cancel() }, http.StatusServiceUnavailable, "ai_routing_cancelled"},
			{"disabled", func(f *aiHTTPFixture, _ context.CancelFunc) {
				copy := *f.policies.policy
				copy.Enabled = false
				f.policies.policy = &copy
			}, http.StatusForbidden, "ai_routing_disabled"},
			{"republished", func(f *aiHTTPFixture, _ context.CancelFunc) {
				copy := *f.policies.policy
				copy.Version++
				f.policies.policy = &copy
			}, http.StatusConflict, "ai_policy_changed"},
			{"same model different target", func(f *aiHTTPFixture, _ context.CancelFunc) {
				other := int64(999)
				for i := range f.pool {
					if f.pool[i].ID == "B" {
						f.pool[i].TargetGroupID = &other
					}
				}
			}, http.StatusConflict, "ai_candidate_revoked"},
		} {
			t.Run(requestPath+"/"+tc.name, func(t *testing.T) {
				f, endpoint, body := aiTraceRequest(t, requestPath)
				ctx, cancel := context.WithCancel(context.WithValue(context.Background(), ctxkey.RequestID, "trace-order-request"))
				defer cancel()
				store := &aiTraceHookStore{AIRoutingStore: f.runtime.store, onSave: func() { tc.change(f, cancel) }}
				f.runtime.store = store
				beforeExecutions, beforeChoices := f.executions, f.chooser.calls
				req := httptest.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				f.engine.ServeHTTP(response, req)
				require.Equal(t, tc.status, response.Code, response.Body.String())
				choices := 0
				if requestPath == "decision" {
					choices = 1
				}
				require.Equal(t, beforeChoices+choices, f.chooser.calls, "must never retry a decision after trace I/O")
				require.Equal(t, f.admissions, f.releases)
				if tc.code == "" {
					require.Equal(t, beforeExecutions+1, f.executions)
					require.Len(t, store.traces, 1)
				} else {
					require.Contains(t, response.Body.String(), tc.code)
					require.Equal(t, beforeExecutions, f.executions, "trace I/O cannot authorize stale/cancelled execution")
					require.Len(t, store.traces, 2)
					last := store.traces[len(store.traces)-1]
					require.Equal(t, "rejected", last.Source)
					require.Equal(t, tc.code, last.FailureReason)
					require.Equal(t, store.traces[0].RequestID, last.RequestID)
					require.Equal(t, "trace-order-request", last.RequestID)
					rows, err := store.ListTraces(context.Background(), f.group.ID)
					require.NoError(t, err)
					require.Equal(t, "rejected", rows[0].Source, "latest persisted outcome must be the rejection")
				}
			})
		}
	}
}

func TestAIRoutingRevalidatesCancellationAfterFinalEligibility(t *testing.T) {
	for _, requestPath := range []string{"decision", "single", "count", "continuation", "bound-count"} {
		t.Run(requestPath, func(t *testing.T) {
			f, endpoint, body := aiTraceRequest(t, requestPath)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			traceWritten := false
			store := &aiTraceHookStore{AIRoutingStore: f.runtime.store, onSave: func() { traceWritten = true }}
			f.runtime.store = store
			eligible := f.runtime.eligible
			f.runtime.eligible = func(ctx context.Context, key *service.APIKey, policy *service.AIRoutingPolicy, request service.AIRoutingRequest, endpoint string, bound int64) []service.AIRoutingCandidate {
				candidates := eligible(ctx, key, policy, request, endpoint, bound)
				if traceWritten {
					cancel() // A stale but otherwise valid list is returned after cancellation.
				}
				return candidates
			}
			beforeExecutions := f.executions
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			f.engine.ServeHTTP(response, req)
			require.Equal(t, http.StatusServiceUnavailable, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), "ai_routing_cancelled")
			require.Equal(t, beforeExecutions, f.executions)
			require.Equal(t, "rejected", store.traces[len(store.traces)-1].Source)
		})
	}
}

func TestAIRoutingDecisionProjectionPreservesExecutionBody(t *testing.T) {
	const body = `{"model":"any-client-alias","max_tokens":128,"system":"SYSTEM_EXECUTION_ONLY","messages":[{"role":"user","content":"EARLIER_USER_REQUIREMENT"},{"role":"assistant","content":"VISIBLE_ASSISTANT_PLAN"},{"role":"user","content":"CURRENT_USER_TASK"}]}`
	for _, mode := range []string{"metadata", "task_excerpt", "bounded_history"} {
		t.Run(mode, func(t *testing.T) {
			f := newAIHTTPFixture(t)
			f.policies.policy.ContextMode = mode
			f.policies.policy.TimeoutMS = 2000
			response := f.send("/v1/messages", body, false)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			require.Equal(t, 1, f.executions)
			expected, err := sjson.Set(body, "model", "picked-executor")
			require.NoError(t, err)
			require.JSONEq(t, expected, string(f.forwardedBody), "decision projection must never replace the full execution request")
			require.NotContains(t, f.chooser.seenTask, "SYSTEM_EXECUTION_ONLY")
			if mode == "bounded_history" {
				require.Contains(t, f.chooser.seenTask, "EARLIER_USER_REQUIREMENT")
				require.Contains(t, f.chooser.seenTask, "VISIBLE_ASSISTANT_PLAN")
			} else {
				require.NotContains(t, f.chooser.seenTask, "EARLIER_USER_REQUIREMENT")
			}
		})
	}
}
