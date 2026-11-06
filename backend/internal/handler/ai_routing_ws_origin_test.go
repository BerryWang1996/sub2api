package handler

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAIWSOriginSeparatesHTTPAndLeaseContinuation(t *testing.T) {
	ctx := context.Background()
	key := &service.APIKey{ID: 2, UserID: 1, Group: &service.Group{ID: 3}}
	store := &aiResponsesMemoryStore{}
	origin := service.AIRoutingOriginBinding{Protocol: service.AIRoutingEndpointResponsesWS, ConnectionID: "native-lease-1", Candidate: "candidate", Account: 4, AccountTarget: 4, Model: "chosen", Platform: service.PlatformOpenAI, ResponseID: "resp_ws", Native: true}
	ref := service.AIRoutingOriginRef{Kind: "response", ID: origin.ResponseID, RequiresStored: true}
	require.NoError(t, saveAIResponsesBinding(ctx, store, key, ref, origin))
	storageKey, err := aiResponsesProtocolOriginKey(key, ref, service.AIRoutingEndpointResponsesWS)
	require.NoError(t, err)
	require.Equal(t, service.AIRoutingToolOriginTTL, store.ttls[storageKey])
	req := service.AIRoutingRequest{Endpoint: service.AIRoutingEndpointResponsesWS, RequiresOrigin: true, OriginRefs: []service.AIRoutingOriginRef{ref}, PreviousResponseID: ref.ID}
	binding, loaded, err := loadAIResponsesBinding(ctx, store, key, req)
	require.NoError(t, err)
	require.EqualValues(t, 4, binding.Account)
	require.Equal(t, "native-lease-1", loaded.ConnectionID)
	req.Endpoint = service.CompositeRouteEndpointResponses
	_, _, err = loadAIResponsesBinding(ctx, store, key, req)
	require.Error(t, err, "WS state must not authorize HTTP retrieval or continuation")
	httpOrigin := origin
	httpOrigin.Protocol, httpOrigin.ConnectionID, httpOrigin.Store = "", "", true
	require.NoError(t, saveAIResponsesBinding(ctx, store, key, ref, httpOrigin))
	_, loaded, err = loadAIResponsesBinding(ctx, store, key, req)
	require.NoError(t, err)
	require.Empty(t, loaded.ConnectionID)
	req.Endpoint = service.AIRoutingEndpointResponsesWS
	foreign := *key
	foreign.ID++
	_, _, err = loadAIResponsesBinding(ctx, store, &foreign, req)
	require.Error(t, err)
	origin.ConnectionID = ""
	require.Error(t, saveAIResponsesBinding(ctx, store, key, ref, origin))
}

func TestAIWSStoredItemCannotBorrowEphemeralLeaseState(t *testing.T) {
	ctx := context.Background()
	key := &service.APIKey{ID: 2, UserID: 1, Group: &service.Group{ID: 3}}
	store := &aiResponsesMemoryStore{}
	origin := service.AIRoutingOriginBinding{Protocol: service.AIRoutingEndpointResponsesWS, ConnectionID: "lease", Candidate: "candidate", Account: 4, AccountTarget: 4, Model: "chosen", Platform: service.PlatformOpenAI, ResponseID: "resp_ephemeral", Native: true}
	require.NoError(t, saveAIResponsesBinding(ctx, store, key, service.AIRoutingOriginRef{Kind: "response", ID: origin.ResponseID}, origin))
	item := service.AIRoutingOriginRef{Kind: "item", ID: "item_private", RequiresStored: true}
	require.NoError(t, saveAIResponsesBinding(ctx, store, key, item, origin))
	_, _, err := loadAIResponsesBinding(ctx, store, key, service.AIRoutingRequest{Endpoint: service.AIRoutingEndpointResponsesWS, RequiresOrigin: true, OriginRefs: []service.AIRoutingOriginRef{item}})
	require.Error(t, err)
}
