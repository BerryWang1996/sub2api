package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAIBackgroundReleasePreviewRejectsBeforeAnyDependencies(t *testing.T) {
	p := NewAIRoutingDraft(8)
	p.SchemaVersion = 2
	p.EnabledEndpoints = []string{CompositeRouteEndpointResponses, AIRoutingEndpointResponsesWS}
	for _, mode := range []string{"metadata", "task_excerpt", "bounded_history"} {
		p.ContextMode = mode
		for _, endpoint := range p.EnabledEndpoints {
			t.Run(mode+endpoint, func(t *testing.T) {
				_, err := NewAIRoutingSampleRequestForEndpoint(&p, `{"input":"synthetic","background":true,"store":true}`, endpoint)
				require.ErrorIs(t, err, ErrAIRoutingBackgroundNotReleased)
			})
		}
	}
	var svc *AIRoutingPolicyService
	out, err := svc.PreviewRequest(t.Context(), 8, p, AIRoutingRequest{Endpoint: CompositeRouteEndpointResponses, Background: true})
	require.Nil(t, out)
	require.ErrorIs(t, err, ErrAIRoutingBackgroundNotReleased)
	// Parsing remains available to the isolated development/ledger tests.
	req, err := NewAIRoutingResponsesRequest([]byte(`{"model":"alias","input":"synthetic","background":true,"store":true,"max_output_tokens":32}`), "metadata")
	require.NoError(t, err)
	require.True(t, req.Background)
}
