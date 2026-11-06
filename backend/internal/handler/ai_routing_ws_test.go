//go:build unit

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

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func aiWSHandlerKey() *service.APIKey {
	groupID := int64(8)
	return &service.APIKey{ID: 3, UserID: 2, GroupID: &groupID, Group: &service.Group{ID: groupID, Platform: service.PlatformComposite, AIRouting: true, Status: service.StatusActive}, User: &service.User{ID: 2, Concurrency: 4}}
}

func TestAIRoutingWSPrivateTurnsOwnWriterBodyIdentityAndFreshSubject(t *testing.T) {
	recorder := httptest.NewRecorder()
	template, _ := gin.CreateTestContext(recorder)
	template.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	template.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 999, Concurrency: 999})
	key := aiWSHandlerKey()
	first, writer := aiWSPrivateContext(template, t.Context(), key, nil, []byte(`{"input":"first"}`))
	second, other := aiWSPrivateContext(template, t.Context(), key, nil, []byte(`{"input":"second"}`))
	require.NotSame(t, writer, other)
	require.NotSame(t, first.Request, second.Request)
	requestID := first.Request.Context().Value(ctxkey.RequestID)
	require.NotEmpty(t, requestID)
	require.NotEqual(t, requestID, second.Request.Context().Value(ctxkey.RequestID))
	require.Equal(t, requestID, first.Request.Context().Value(ctxkey.ClientRequestID))
	first.Request.Header.Set("private", "one")
	require.Empty(t, template.GetHeader("private"))
	require.Empty(t, second.GetHeader("private"))
	first.Set("turn-only", true)
	_, exists := second.Get("turn-only")
	require.False(t, exists)
	subject, ok := middleware.GetAuthSubjectFromContext(first)
	require.True(t, ok)
	require.Equal(t, int64(2), subject.UserID)
	require.Equal(t, 4, subject.Concurrency)
	body, err := io.ReadAll(second.Request.Body)
	require.NoError(t, err)
	require.Equal(t, `{"input":"second"}`, string(body))
	abortAI(first, 403, "ai_source_revoked", "Synthetic admission refusal")
	require.Equal(t, 403, writer.Status())
	require.True(t, writer.Written())
	require.ErrorContains(t, writer.failure(), "Synthetic admission refusal")
	require.False(t, other.Written())
	require.Empty(t, recorder.Body.String(), "turn errors never leak into hijacked shared writer")
	require.Equal(t, key.Group, first.Request.Context().Value(ctxkey.Group))
}

func TestAIRoutingWSAdmissionWriterHasBoundedErrors(t *testing.T) {
	w := newAIWSPlanWriter()
	require.Equal(t, -1, w.Size())
	_, err := w.Write([]byte(strings.Repeat("x", 16<<10)))
	require.NoError(t, err)
	_, err = w.Write([]byte("x"))
	require.Error(t, err)
	require.Equal(t, 16<<10, w.Size())
	_, _, err = w.Hijack()
	require.Error(t, err)
}

func aiWSStateFixture(accountID int64, store *aiResponsesMemoryStore) *aiWSExecutionState {
	key := aiWSHandlerKey()
	return &aiWSExecutionState{native: true, key: key, account: &service.Account{ID: accountID, Platform: service.PlatformOpenAI}, request: service.AIRoutingRequest{PublicModel: "arbitrary-client-alias", StoreResponse: false}, prepared: &aiRoutingExecutionPlan{Candidate: service.AIRoutingCandidate{ID: "candidate", Platform: service.PlatformOpenAI, Model: "gpt-5.1", TargetAccountID: &accountID}, OriginStore: store}}
}

func TestAIRoutingWSEmittedOriginsCarryActualTargetAndLeaseAcrossSteeredResponses(t *testing.T) {
	store := &aiResponsesMemoryStore{}
	state := aiWSStateFixture(99, store)
	events := []string{
		`{"type":"response.created","response":{"id":"resp_one"}}`,
		`{"type":"response.output_item.added","item":{"type":"function_call","id":"item_one","call_id":"call_one","name":"lookup"}}`,
		`{"type":"response.incomplete","response":{"id":"resp_one","incomplete_details":{"reason":"steered"}}}`,
		`{"type":"response.created","response":{"id":"resp_two"}}`,
		`{"type":"response.output_item.added","item":{"type":"function_call","id":"item_two","call_id":"call_two","name":"lookup"}}`,
		`{"type":"response.completed","response":{"id":"resp_two"}}`,
	}
	written := 0
	for _, event := range events {
		require.NoError(t, state.emit(t.Context(), []byte(event), "upstream-lease-17", func(context.Context, []byte) error { written++; return nil }))
	}
	require.Equal(t, len(events), written)
	for _, ref := range []service.AIRoutingOriginRef{{Kind: "response", ID: "resp_one"}, {Kind: "tool_call", ID: "call_one"}, {Kind: "response", ID: "resp_two"}, {Kind: "tool_call", ID: "call_two"}} {
		key, err := aiResponsesProtocolOriginKey(state.key, ref, service.AIRoutingEndpointResponsesWS)
		require.NoError(t, err)
		var origin service.AIRoutingOriginBinding
		require.NoError(t, json.Unmarshal([]byte(store.rows[key]), &origin))
		require.True(t, origin.Valid())
		require.Equal(t, int64(99), origin.Account)
		require.Equal(t, "gpt-5.1", origin.Model)
		require.Equal(t, "upstream-lease-17", origin.ConnectionID)
		require.False(t, origin.Store)
		require.Equal(t, "arbitrary-client-alias", origin.PublicModel)
		if strings.HasSuffix(ref.ID, "one") {
			require.Equal(t, "resp_one", origin.ResponseID)
		} else {
			require.Equal(t, "resp_two", origin.ResponseID)
		}
	}
	other := aiWSStateFixture(100, store)
	require.NoError(t, other.bindEvent(t.Context(), []byte(`{"type":"response.created","response":{"id":"resp_other"}}`), "another-lease"))
	req := service.AIRoutingRequest{Endpoint: service.AIRoutingEndpointResponsesWS, RequiresOrigin: true, OriginRefs: []service.AIRoutingOriginRef{{Kind: "response", ID: "resp_one", RequiresStored: true}}}
	binding, origin, err := loadAIResponsesBinding(t.Context(), store, state.key, req)
	require.NoError(t, err)
	require.Equal(t, int64(99), binding.Account)
	require.Equal(t, "upstream-lease-17", origin.ConnectionID)
	req.OriginRefs = append(req.OriginRefs, service.AIRoutingOriginRef{Kind: "response", ID: "resp_other", RequiresStored: true})
	_, _, err = loadAIResponsesBinding(t.Context(), store, state.key, req)
	require.Error(t, err, "same model cannot merge different accounts")
}

func TestAIRoutingWSBindingFailureNeverExposesIDAndCrossKeyCannotResume(t *testing.T) {
	store := &aiResponsesMemoryStore{err: errors.New("synthetic unavailable")}
	state := aiWSStateFixture(99, store)
	writes := 0
	event := []byte(`{"type":"response.created","response":{"id":"resp_private"}}`)
	err := state.emit(t.Context(), event, "lease", func(context.Context, []byte) error { writes++; return nil })
	require.Error(t, err)
	require.Zero(t, writes)
	store.err = nil
	state.inspector = nil
	require.NoError(t, state.bindEvent(t.Context(), event, "lease"))
	otherKey := *state.key
	otherKey.ID++
	req := service.AIRoutingRequest{Endpoint: service.AIRoutingEndpointResponsesWS, RequiresOrigin: true, OriginRefs: []service.AIRoutingOriginRef{{Kind: "response", ID: "resp_private", RequiresStored: true}}}
	_, _, err = loadAIResponsesBinding(t.Context(), store, &otherKey, req)
	require.Error(t, err)
	req.Endpoint = service.CompositeRouteEndpointResponses
	_, _, err = loadAIResponsesBinding(t.Context(), store, state.key, req)
	require.Error(t, err, "WS origins never authorize HTTP state access")
}

func TestAIRoutingWSHTTPOriginsAllowExplicitToolHistoryButNeverImplicitProviderState(t *testing.T) {
	store := &aiResponsesMemoryStore{}
	state := aiWSStateFixture(99, store)
	state.native = false
	require.NoError(t, state.emit(t.Context(), []byte(`{"type":"response.created","response":{"id":"resp_bridge"}}`), "", func(context.Context, []byte) error { return nil }))
	require.NoError(t, state.emit(t.Context(), []byte(`{"type":"response.output_item.added","item":{"type":"function_call","id":"item_bridge","call_id":"call_bridge","name":"lookup"}}`), "", func(context.Context, []byte) error { return nil }))
	req := service.AIRoutingRequest{Endpoint: service.AIRoutingEndpointResponsesWS, RequiresOrigin: true, OriginRefs: []service.AIRoutingOriginRef{{Kind: "tool_call", ID: "call_bridge"}}}
	binding, origin, err := loadAIResponsesBinding(t.Context(), store, state.key, req)
	require.NoError(t, err)
	require.Equal(t, int64(99), binding.Account)
	require.False(t, origin.Native)
	require.Empty(t, origin.ConnectionID)
	req.PreviousResponseID = "resp_bridge"
	req.OriginRefs = []service.AIRoutingOriginRef{{Kind: "response", ID: "resp_bridge", RequiresStored: true}}
	_, _, err = loadAIResponsesBinding(t.Context(), store, state.key, req)
	require.Error(t, err, "a bridge response ID must not promise provider-retained context")
}

func TestAIRoutingWSRouteRequiresExplicitPolicyAndDependenciesBeforeUpgrade(t *testing.T) {
	key := aiWSHandlerKey()
	h := &GatewayHandler{}
	for _, enabled := range []bool{false, true} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
		c.Request.Header.Set("Connection", "Upgrade")
		c.Request.Header.Set("Upgrade", "websocket")
		p := &service.AIRoutingPolicy{SchemaVersion: 2, Enabled: true, EnabledEndpoints: []string{"messages"}}
		if enabled {
			p.EnabledEndpoints = append(p.EnabledEndpoints, service.AIRoutingEndpointResponsesWS)
		}
		require.True(t, h.routeAIResponsesWebSocket(c, &aiRoutingRuntime{}, key, p))
		require.NotEqual(t, http.StatusSwitchingProtocols, recorder.Code)
		if enabled {
			require.Equal(t, 503, recorder.Code)
		} else {
			require.Equal(t, 400, recorder.Code)
		}
	}
}

func TestAIRoutingWSPartialUsageIsRecognizedForSettlement(t *testing.T) {
	require.False(t, aiWSHasUsage(&service.OpenAIForwardResult{}))
	for _, usage := range []service.OpenAIUsage{{InputTokens: 1}, {OutputTokens: 1}, {CacheReadInputTokens: 1}, {CacheCreationInputTokens: 1}, {ImageInputTokens: 1}, {ImageOutputTokens: 1}} {
		require.True(t, aiWSHasUsage(&service.OpenAIForwardResult{Usage: usage}))
	}
}
