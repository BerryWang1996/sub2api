package handler

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/testutil"
	"github.com/stretchr/testify/require"
)

func TestAIGeminiStateRequiresSameKeyTargetAndLiveOrigin(t *testing.T) {
	base, _ := testutil.NewRedisAIRoutingStore(t)
	store, ok := base.(service.AIRoutingOriginStore)
	require.True(t, ok)
	key := &service.APIKey{ID: 9, UserID: 42, Group: &service.Group{ID: 17}}
	account := int64(75)
	candidate := service.AIRoutingCandidate{ID: "gemini", Model: "gemini-test", Platform: service.PlatformGemini, TargetAccountID: &account}
	ctx := context.Background()
	callbacks := newAIGeminiResponseStateCallbacks(ctx, store, key, candidate, func() int64 { return account })
	id := service.GeminiResponsesToolCallPrefix + "synthetic"
	state := service.GeminiResponsesToolState{Name: "scan", Arguments: json.RawMessage(`{"query":"private-tool-input"}`), Part: json.RawMessage(`{"thoughtSignature":"opaque-signature","functionCall":{"name":"scan","args":{"query":"private-tool-input"}}}`)}
	require.NoError(t, callbacks.Save(ctx, id, state))
	_, err := callbacks.Load(ctx, id)
	require.True(t, service.IsAIRoutingCapacityError(err), "state alone is never an origin credential")
	origin := service.AIRoutingOriginBinding{Candidate: candidate.ID, Account: account, Model: candidate.Model, Platform: candidate.Platform, AccountTarget: account, ResponseID: "resp_gemini", PublicModel: "auto"}
	require.NoError(t, saveAIResponsesBinding(ctx, store, key, service.AIRoutingOriginRef{Kind: "response", ID: origin.ResponseID}, origin))
	require.NoError(t, saveAIResponsesBinding(ctx, store, key, service.AIRoutingOriginRef{Kind: "tool_call", ID: id}, origin))
	got, err := callbacks.Load(ctx, id)
	require.NoError(t, err)
	require.Equal(t, state, got)
	otherKey := *key
	otherKey.ID++
	foreign := newAIGeminiResponseStateCallbacks(ctx, store, &otherKey, candidate, func() int64 { return account })
	_, err = foreign.Load(ctx, id)
	require.True(t, service.IsAIRoutingCapacityError(err))
	changedModel := candidate
	changedModel.Model = "other-model"
	changed := newAIGeminiResponseStateCallbacks(ctx, store, key, changedModel, func() int64 { return account })
	_, err = changed.Load(ctx, id)
	require.True(t, service.IsAIRoutingCapacityError(err))
	changed = newAIGeminiResponseStateCallbacks(ctx, store, key, candidate, func() int64 { return account + 1 })
	_, err = changed.Load(ctx, id)
	require.True(t, service.IsAIRoutingCapacityError(err))
	parentKey := service.AIRoutingOriginKey(key.Group.ID, key.UserID, key.ID, "response", origin.ResponseID)
	revoker, ok := base.(service.AIRoutingOriginRevoker)
	require.True(t, ok)
	require.NoError(t, revoker.RevokeOriginBinding(ctx, parentKey, service.AIRoutingResponseOriginTTL))
	_, err = callbacks.Load(ctx, id)
	require.True(t, service.IsAIRoutingCapacityError(err), "revoking the parent also revokes its opaque state")
}

func TestAIGeminiStateRefusesCancellationAndConflicts(t *testing.T) {
	base, _ := testutil.NewRedisAIRoutingStore(t)
	store, ok := base.(service.AIRoutingOriginStore)
	require.True(t, ok)
	key := &service.APIKey{ID: 9, UserID: 42, Group: &service.Group{ID: 17}}
	account := int64(75)
	candidate := service.AIRoutingCandidate{ID: "gemini", Model: "gemini-test", Platform: service.PlatformGemini, TargetAccountID: &account}
	ctx, cancel := context.WithCancel(context.Background())
	callbacks := newAIGeminiResponseStateCallbacks(ctx, store, key, candidate, func() int64 { return account })
	id := service.GeminiResponsesToolCallPrefix + "synthetic"
	state := service.GeminiResponsesToolState{Name: "scan", Arguments: json.RawMessage(`{}`), Part: json.RawMessage(`{"thoughtSignature":"opaque"}`)}
	require.NoError(t, callbacks.Save(ctx, id, state))
	require.NoError(t, callbacks.Save(ctx, id, state), "same immutable state is idempotent")
	state.Part = json.RawMessage(`{"thoughtSignature":"changed"}`)
	require.True(t, service.IsAIRoutingCapacityError(callbacks.Save(ctx, id, state)))
	cancel()
	require.True(t, service.IsAIRoutingCapacityError(callbacks.Save(context.Background(), id+"2", state)), "detached transport cannot bypass inbound cancellation")
	_, err := callbacks.Load(context.Background(), id)
	require.True(t, service.IsAIRoutingCapacityError(err))
}

func TestAIGeminiStateChatUsesChatBindingsAndNeverBorrowsResponsesState(t *testing.T) {
	base, _ := testutil.NewRedisAIRoutingStore(t)
	store, ok := base.(service.AIRoutingOriginStore)
	require.True(t, ok)
	key := &service.APIKey{ID: 9, UserID: 42, Group: &service.Group{ID: 17}}
	account := int64(75)
	candidate := service.AIRoutingCandidate{ID: "ag-gemini", Model: "gemini-3.1-pro-high", Platform: service.PlatformAntigravity, TargetAccountID: &account}
	id := service.GeminiResponsesToolCallPrefix + "chat-state"
	state := service.GeminiResponsesToolState{Name: "scan", Arguments: json.RawMessage(`{}`), Part: json.RawMessage(`{"thoughtSignature":"opaque"}`)}
	chatState := newAIGeminiResponseStateCallbacks(t.Context(), store, key, candidate, func() int64 { return account }, service.CompositeRouteEndpointChatCompletions)
	require.NoError(t, chatState.Save(t.Context(), id, state))
	_, err := chatState.Load(t.Context(), id)
	require.Error(t, err, "private state alone does not authorize a tool continuation")
	require.NoError(t, saveAIChatBinding(t.Context(), base, key, id, newAIToolBinding(candidate, account)))
	got, err := chatState.Load(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, state, got)
	httpState := newAIGeminiResponseStateCallbacks(t.Context(), store, key, candidate, func() int64 { return account })
	_, err = httpState.Load(t.Context(), id)
	require.Error(t, err)
	other := *key
	other.ID++
	foreign := newAIGeminiResponseStateCallbacks(t.Context(), store, &other, candidate, func() int64 { return account }, service.CompositeRouteEndpointChatCompletions)
	_, err = foreign.Load(t.Context(), id)
	require.Error(t, err)
}

func TestAIGeminiStateSeparatesHTTPAndWebSocketBridgeOrigins(t *testing.T) {
	store := &aiResponsesMemoryStore{}
	key := &service.APIKey{ID: 9, UserID: 42, Group: &service.Group{ID: 17}}
	account := int64(75)
	candidate := service.AIRoutingCandidate{ID: "gemini", Model: "gemini-test", Platform: service.PlatformGemini, TargetAccountID: &account}
	id := service.GeminiResponsesToolCallPrefix + "same-id"
	state := service.GeminiResponsesToolState{Name: "scan", Arguments: json.RawMessage(`{}`), Part: json.RawMessage(`{"thoughtSignature":"opaque"}`)}
	httpState := newAIGeminiResponseStateCallbacks(t.Context(), store, key, candidate, func() int64 { return account })
	wsState := newAIGeminiResponseStateCallbacks(t.Context(), store, key, candidate, func() int64 { return account }, service.AIRoutingEndpointResponsesWS)
	require.NoError(t, httpState.Save(t.Context(), id, state))
	origin := service.AIRoutingOriginBinding{Protocol: service.AIRoutingEndpointResponsesWS, Candidate: candidate.ID, Account: account, Model: candidate.Model, Platform: candidate.Platform, AccountTarget: account, ResponseID: "resp_ws_bridge"}
	require.NoError(t, saveAIResponsesBinding(t.Context(), store, key, service.AIRoutingOriginRef{Kind: "response", ID: origin.ResponseID}, origin))
	require.NoError(t, saveAIResponsesBinding(t.Context(), store, key, service.AIRoutingOriginRef{Kind: "tool_call", ID: id}, origin))
	_, err := wsState.Load(t.Context(), id)
	require.Error(t, err, "HTTP opaque state cannot be borrowed by a WS origin")
	require.NoError(t, wsState.Save(t.Context(), id, state))
	got, err := wsState.Load(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, state, got)
	_, err = httpState.Load(t.Context(), id)
	require.Error(t, err, "WS origin cannot authorize HTTP restoration")
	_, _, err = loadAIResponsesBinding(t.Context(), store, key, service.AIRoutingRequest{Endpoint: service.AIRoutingEndpointResponsesWS, RequiresOrigin: true, PreviousResponseID: origin.ResponseID, OriginRefs: []service.AIRoutingOriginRef{{Kind: "response", ID: origin.ResponseID, RequiresStored: true}}})
	require.Error(t, err, "HTTP bridge owns tool state, never server-side previous_response_id state")
}
