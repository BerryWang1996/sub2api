package handler

import (
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAIRoutingRevalidatesAfterDecision(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*aiHTTPFixture)
		status int
		code   string
	}{
		{"unchanged publication", func(f *aiHTTPFixture) {
			// A draft-only write preserves the published policy/version.
			copy := *f.policies.policy
			f.policies.policy = &copy
		}, 200, ""},
		{"new publication", func(f *aiHTTPFixture) {
			copy := *f.policies.policy
			copy.Version++
			f.policies.policy = &copy
		}, 409, "ai_policy_changed"},
		{"group disabled", func(f *aiHTTPFixture) {
			f.policies.policy = &service.AIRoutingPolicy{GroupID: f.group.ID}
		}, 403, "ai_routing_disabled"},
		{"publication missing", func(f *aiHTTPFixture) { f.policies.policy = nil }, 403, "ai_routing_disabled"},
		{"read failure", func(f *aiHTTPFixture) { f.policies.err = errors.New("storage unavailable") }, 503, "ai_policy_unavailable"},
		{"candidate no longer eligible", func(f *aiHTTPFixture) { f.pool = f.pool[:1] }, 409, "ai_candidate_revoked"},
		{"candidate id reused for other model", func(f *aiHTTPFixture) { f.pool[1].Model = "other-model" }, 409, "ai_candidate_revoked"},
		{"candidate id reused for other target", func(f *aiHTTPFixture) {
			other := int64(999)
			f.pool[1].TargetGroupID = &other
		}, 409, "ai_candidate_revoked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAIHTTPFixture(t)
			f.requireDecisionSlot = true
			f.chooser.onDecide = func() { tc.change(f) }
			response := f.send("/v1/messages", aiHTTPTask, false)
			require.Equal(t, tc.status, response.Code, response.Body.String())
			require.Equal(t, 1, f.chooser.calls, "must never rerun the paid decision")
			require.Equal(t, 1, f.releases)
			if tc.code == "" {
				require.Equal(t, 1, f.executions)
			} else {
				require.Contains(t, response.Body.String(), tc.code)
				require.Zero(t, f.executions, "must not execute or fall back after revocation")
			}
		})
	}
}

func TestAIRoutingRejectsAmbiguousNestedJSONBeforeDecision(t *testing.T) {
	for _, body := range []string{
		`{"model":"auto","messages":[{"role":"user","content":[{"type":"text","type":"tool_result","text":"hi","tool_use_id":"unknown","content":"x"}]}]}`,
		`{"model":"auto","messages":[{"role":"user","content":[{"type":"text","Type":"tool_result","text":"hi","tool_use_id":"unknown","content":"x"}]}]}`,
		`{"model":"auto","messages":[{"role":"user","role":"assistant","content":"private"}]}`,
	} {
		f := newAIHTTPFixture(t)
		response := f.send("/v1/messages", body, false)
		require.Equal(t, 400, response.Code, response.Body.String())
		require.Zero(t, f.chooser.calls)
		require.Zero(t, f.admissions)
		require.Zero(t, f.executions)
	}
}
