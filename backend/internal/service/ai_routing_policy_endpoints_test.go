package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAIRoutingPolicyEndpointSchemasFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		schema    int
		endpoints []string
		valid     bool
		messages  bool
		responses bool
	}{
		{"legacy missing", 0, nil, true, true, false},
		{"legacy v1", 1, nil, true, true, false},
		{"v2 missing", 2, nil, true, true, false},
		{"v2 explicit messages", 2, []string{"messages"}, true, true, false},
		{"v2 responses", 2, []string{"responses"}, true, false, true},
		{"v2 both", 2, []string{"messages", "responses"}, true, true, true},
		{"legacy cannot enable responses", 0, []string{"responses"}, false, false, false},
		{"v1 cannot enable responses", 1, []string{"messages", "responses"}, false, false, false},
		{"explicit empty", 2, []string{}, false, false, false},
		{"legacy empty", 0, []string{}, false, false, false},
		{"future schema", 3, nil, false, false, false},
		{"negative schema", -1, nil, false, false, false},
		{"duplicate", 2, []string{"responses", "responses"}, false, false, false},
		{"websocket not HTTP", 2, []string{"responses_ws"}, true, false, false},
		{"gemini independent", 2, []string{"gemini"}, true, false, false},
		{"legacy websocket disabled", 1, []string{"responses_ws"}, false, false, false},
		{"legacy gemini disabled", 1, []string{"gemini"}, false, false, false},
		{"unknown", 2, []string{"unknown"}, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewAIRoutingDraft(8)
			p.SchemaVersion, p.EnabledEndpoints = tc.schema, tc.endpoints
			require.Equal(t, tc.valid, validateAIRoutingDraft(p) == nil)
			require.Equal(t, tc.messages, p.AllowsEndpoint(CompositeRouteEndpointMessages))
			require.Equal(t, tc.messages, p.AllowsEndpoint(CompositeRouteEndpointCountTokens))
			require.Equal(t, tc.responses, p.AllowsEndpoint(CompositeRouteEndpointResponses))
			require.Equal(t, tc.valid && len(tc.endpoints) == 1 && tc.endpoints[0] == "responses_ws", p.AllowsEndpoint("responses_ws"))
			require.False(t, p.AllowsEndpoint("unknown"))
			if !tc.valid {
				require.Empty(t, p.ClientEndpoints())
			}
		})
	}
	var absent *AIRoutingPolicy
	require.False(t, absent.AllowsEndpoint("messages"))
	var decoded AIRoutingPolicy
	require.NoError(t, json.Unmarshal([]byte(`{"schema_version":2,"enabled_endpoints":[]}`), &decoded))
	require.NotNil(t, decoded.EnabledEndpoints)
	require.False(t, decoded.AllowsEndpoint("messages"))
}

func TestAIRoutingPolicyEndpointSaveUpgradesDraftWithoutChangingPublication(t *testing.T) {
	ctx := context.Background()
	legacy := NewAIRoutingDraft(8)
	legacy.Enabled = true
	store := &aiPolicyStoreStub{st: &AIRoutingPolicyState{Draft: legacy, Published: &legacy, Revision: 1}}
	svc := &AIRoutingPolicyService{repo: store}
	saved, err := svc.SaveDraft(ctx, 8, 1, legacy)
	require.NoError(t, err)
	require.Equal(t, 2, saved.Draft.SchemaVersion)
	require.Equal(t, []string{"messages"}, saved.Draft.EnabledEndpoints)
	require.Zero(t, saved.Published.SchemaVersion)
	require.True(t, saved.Published.Enabled)
	require.False(t, saved.Published.AllowsEndpoint("responses"))
	copy := saved.Draft.ClientEndpoints()
	copy[0] = "responses"
	require.Equal(t, []string{"messages"}, saved.Draft.EnabledEndpoints)
	invalid := saved.Draft
	invalid.SchemaVersion = 99
	_, err = svc.SaveDraft(ctx, 8, saved.Revision, invalid)
	require.Error(t, err)
	require.EqualValues(t, 2, store.st.Revision)
	updated := saved.Draft
	updated.EnabledEndpoints = []string{"responses"}
	saved, err = svc.SaveDraft(ctx, 8, saved.Revision, updated)
	require.NoError(t, err)
	require.True(t, saved.Draft.AllowsEndpoint("responses"))
	require.False(t, saved.Published.AllowsEndpoint("responses"))
}

func TestAIRoutingPolicyV2RejectsLegacyClientFieldLoss(t *testing.T) {
	p := NewAIRoutingDraft(8)
	p.SchemaVersion, p.EnabledEndpoints, p.Enabled, p.Version = 2, []string{"responses"}, true, 5
	store := &aiPolicyStoreStub{st: &AIRoutingPolicyState{Draft: p, Published: &p, Revision: 5}}
	svc := &AIRoutingPolicyService{repo: store}
	legacy := NewAIRoutingDraft(8)
	legacy.Prompt = "old client omitted endpoint fields"
	_, err := svc.SaveDraft(context.Background(), 8, 5, legacy)
	require.ErrorIs(t, err, ErrAISchemaReloadRequired)
	require.EqualValues(t, 5, store.st.Revision)
	require.Equal(t, []string{"responses"}, store.st.Draft.EnabledEndpoints)
	require.Equal(t, []string{"responses"}, store.st.Published.EnabledEndpoints)
	require.True(t, store.st.Published.Enabled)
	require.EqualValues(t, 5, store.st.Published.Version)
	require.Empty(t, store.action)
}
