//go:build unit

package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

type aiCapacityPreviewAccounts struct{ *aiDecisionGroupAccounts }

func (r *aiCapacityPreviewAccounts) GetByIDs(_ context.Context, ids []int64) ([]*Account, error) {
	accounts := make([]*Account, 0, len(ids))
	for _, id := range ids {
		if account := r.current[id]; account != nil {
			accounts = append(accounts, account)
		}
	}
	return accounts, nil
}

func TestAIRoutingCapacityPreviewDoesNotPromoteOneAccountToPoolGuarantee(t *testing.T) {
	groupID := int64(2)
	known := Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, AccountGroups: []AccountGroup{{GroupID: groupID}}}
	aiCapabilityMetadata(&known, 1000, 500, "text")
	unknown := known
	unknown.ID, unknown.Extra = 2, nil
	repo := &aiCapacityPreviewAccounts{&aiDecisionGroupAccounts{list: []Account{known, unknown}, current: map[int64]*Account{1: &known, 2: &unknown}}}
	groups := &aiDecisionGroupStore{groups: map[int64]*Group{groupID: {ID: groupID, Platform: PlatformOpenAI, Status: StatusActive}}}
	svc := &AIRoutingPolicyService{groups: groups, accounts: repo}
	policy := NewAIRoutingDraft(9)
	policy.Candidates = []AIRoutingCandidate{{ID: "pool", Platform: PlatformOpenAI, Model: "gpt-5.1", TargetGroupID: &groupID, Enabled: true}}
	request := AIRoutingRequest{PublicModel: "alias", InputBytes: 200, MaxOutput: 100, InputModalities: []string{"text"}}
	rows := svc.previewCapacityChecks(context.Background(), &policy, request)
	require.Len(t, rows, 1)
	require.Len(t, rows[0].Accounts, 2)
	require.True(t, rows[0].Accounts[0].Allowed)
	require.False(t, rows[0].Accounts[0].CapacityVerified)
	require.False(t, rows[0].Accounts[0].FinalPayloadChecked)
	require.Equal(t, "upstream", rows[0].Accounts[0].MetadataSource)
	require.Equal(t, "2026-11-06T00:00:00Z", rows[0].Accounts[0].MetadataSyncedAt)
	require.False(t, rows[0].Accounts[1].Allowed)
	require.Equal(t, "execution_context_window_unknown", rows[0].Accounts[1].Reason)
	require.Empty(t, rows[0].Accounts[1].MetadataSource)
	require.Empty(t, rows[0].Accounts[1].MetadataSyncedAt)
	encoded, err := json.Marshal(rows)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "credentials")
	policy.Candidates[0].Enabled = false
	rows = svc.previewCapacityChecks(context.Background(), &policy, request)
	require.Empty(t, rows[0].Accounts)
	require.Equal(t, "candidate_disabled", rows[0].Reason)
}

func TestAIRoutingResponsesPreviewChecksOriginsInEveryDisclosureMode(t *testing.T) {
	for _, mode := range []string{"metadata", "task_excerpt", "bounded_history"} {
		t.Run(mode, func(t *testing.T) {
			policy := NewAIRoutingDraft(9)
			policy.SchemaVersion, policy.EnabledEndpoints = 2, []string{"responses"}
			policy.ContextMode = mode
			accountID := int64(1)
			policy.Candidates = []AIRoutingCandidate{{ID: "disabled", Platform: PlatformOpenAI, Model: "gpt-5.1", TargetAccountID: &accountID}}
			svc := &AIRoutingPolicyService{
				repo:   &aiPolicyStoreStub{st: &AIRoutingPolicyState{Draft: policy}},
				groups: &aiDecisionGroupStore{groups: map[int64]*Group{9: {ID: 9, Platform: PlatformComposite, AIRouting: true}}},
			}
			request, err := NewAIRoutingSampleRequestForEndpoint(&policy, `{"model":"auto","input":"continue","previous_response_id":"resp_unproven","max_output_tokens":100}`, CompositeRouteEndpointResponses)
			require.NoError(t, err)
			out, err := svc.PreviewRequest(context.Background(), 9, policy, request)
			require.NoError(t, err)
			require.Equal(t, "responses", out.SampleEndpoint)
			require.Len(t, out.CapacityChecks, 1, "Responses checks are independent of decision disclosure")
			require.Equal(t, "candidate_disabled", out.CapacityChecks[0].Reason)
			require.Empty(t, out.Eligible)
			require.NotNil(t, out.Context)
			require.Equal(t, "sample_origin_binding_required", out.Context.FailureReason)
			require.Equal(t, "responses-history-v1", out.Context.ProjectionVersion)
			require.False(t, out.ExternalCall)
		})
	}
}
