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
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type aiControlStore struct {
	service.AIRoutingStore
	aiResponsesMemoryStore
	revokeErr error
}

func (s *aiControlStore) RevokeOriginBinding(_ context.Context, key string, ttl time.Duration) error {
	if s.revokeErr != nil {
		return s.revokeErr
	}
	s.rows[key], s.ttls[key] = "revoked", ttl
	return nil
}

type aiControlValidator struct {
	calls int
	err   error
}

func (v *aiControlValidator) ValidateAIRoutingResponsesOrigin(_ context.Context, _ *service.APIKey, p *service.AIRoutingPolicy, origin *service.AIRoutingOriginBinding) (*service.Account, error) {
	v.calls++
	if v.err != nil {
		return nil, v.err
	}
	if !p.Enabled {
		return nil, errors.New("revoked")
	}
	return &service.Account{ID: origin.Account}, nil
}

type aiControlForwarder struct {
	calls             int
	status            int
	body, contentType string
	beforeSend        func()
}

type aiControlBackgroundSettlement struct {
	pending     bool
	calls       int
	originErr   error
	terminalErr error
	terminal    *service.AIRoutingBackgroundTerminal
}

func (s *aiControlBackgroundSettlement) ValidateResponseOrigin(context.Context, *service.APIKey, int64, string) error {
	return s.originErr
}
func (s *aiControlBackgroundSettlement) RecordResponseTerminal(ctx context.Context, _ *service.APIKey, account int64, id string, terminal service.AIRoutingBackgroundTerminal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.terminalErr != nil {
		return s.terminalErr
	}
	if account != 7 || id != "resp_A" {
		return service.ErrAIBackgroundConflict
	}
	s.terminal = &terminal
	return nil
}

func (s *aiControlBackgroundSettlement) CanDeleteResponse(context.Context, *service.APIKey, int64, string) error {
	s.calls++
	if s.pending {
		return service.ErrAIBackgroundRecoveryPending
	}
	return nil
}

func TestAIBackgroundDeleteRetainsUpstreamEvidenceUntilSettlementIsSynced(t *testing.T) {
	for _, mode := range []string{"unavailable", "pending", "changed_before_send", "settled"} {
		t.Run(mode, func(t *testing.T) {
			key, policy, origin, _, runtime := aiControlFixture(t)
			store := &aiControlStore{}
			origin.Background = true
			require.NoError(t, saveAIResponsesBinding(t.Context(), store, key, service.AIRoutingOriginRef{Kind: "response", ID: origin.ResponseID}, *origin))
			runtime.store = store
			validator := &aiControlValidator{}
			forwarder := &aiControlForwarder{body: `{"id":"resp_A","deleted":true}`}
			settlement := &aiControlBackgroundSettlement{pending: mode == "pending"}
			var check aiResponsesBackgroundDeleteValidator = settlement
			if mode == "unavailable" {
				check = nil
			}
			if mode == "changed_before_send" {
				forwarder.beforeSend = func() { settlement.pending = true }
			}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodDelete, "/v1/responses/resp_A", nil)
			require.True(t, routeAIResponsesControlWith(c, runtime, key, policy, validator, forwarder, check))
			if mode == "settled" {
				require.Equal(t, 200, w.Code, w.Body.String())
				require.Equal(t, 1, forwarder.calls)
				require.Equal(t, 2, settlement.calls)
			} else {
				require.Equal(t, 409, w.Code, w.Body.String())
				require.Zero(t, forwarder.calls)
				_, _, err := loadAIResponsesBinding(t.Context(), store, key, service.AIRoutingRequest{RequiresOrigin: true, OriginRefs: []service.AIRoutingOriginRef{{Kind: "response", ID: origin.ResponseID}}})
				require.NoError(t, err, "denied DELETE cannot revoke the recovery resource")
			}
		})
	}
}

func TestAIBackgroundControlRecordsTerminalAndRevalidatesProvider(t *testing.T) {
	for _, mode := range []string{"retrieve", "stream", "cancel", "usage_unknown", "queued", "input_items", "provider_error", "origin_changed", "origin_changed_before_send", "ledger_failure"} {
		t.Run(mode, func(t *testing.T) {
			key, policy, origin, _, runtime := aiControlFixture(t)
			store := &aiControlStore{}
			origin.Background = true
			require.NoError(t, saveAIResponsesBinding(t.Context(), store, key, service.AIRoutingOriginRef{Kind: "response", ID: origin.ResponseID}, *origin))
			runtime.store = store
			settlement := &aiControlBackgroundSettlement{}
			terminal := `{"id":"resp_A","object":"response","status":"completed","model":"gpt-4o","output":[],"usage":{"input_tokens":31,"output_tokens":7}}`
			forwarder := &aiControlForwarder{body: terminal}
			method, path := http.MethodGet, "/v1/responses/resp_A"
			switch mode {
			case "stream":
				forwarder.contentType = "text/event-stream"
				forwarder.body = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + terminal + "}\n\n"
			case "cancel":
				method, path = http.MethodPost, path+"/cancel"
				forwarder.body = strings.Replace(terminal, "completed", "cancelled", 1)
			case "usage_unknown":
				forwarder.body = `{"id":"resp_A","status":"completed","model":"gpt-4o","output":[],"usage":null}`
			case "queued":
				forwarder.body = `{"id":"resp_A","status":"queued","model":"gpt-4o","output":[],"usage":null}`
			case "input_items":
				path += "/input_items"
				forwarder.body = `{"object":"list","data":[]}`
			case "provider_error":
				forwarder.status, forwarder.body = 503, `{"error":{"message":"synthetic failure"}}`
			case "origin_changed":
				settlement.originErr = service.ErrAIBackgroundConflict
			case "origin_changed_before_send":
				forwarder.beforeSend = func() { settlement.originErr = service.ErrAIBackgroundConflict }
			case "ledger_failure":
				settlement.terminalErr = errors.New("private database details")
			}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(method, path, nil)
			require.True(t, routeAIResponsesControlWith(c, runtime, key, policy, &aiControlValidator{}, forwarder, settlement))
			switch mode {
			case "origin_changed", "origin_changed_before_send":
				require.Equal(t, 403, w.Code, w.Body.String())
				require.Zero(t, forwarder.calls)
				require.Nil(t, settlement.terminal)
			case "ledger_failure":
				require.Equal(t, 502, w.Code, w.Body.String())
				require.NotContains(t, w.Body.String(), "private database details")
				require.NotContains(t, w.Body.String(), "resp_A")
			case "queued", "input_items", "provider_error":
				require.Nil(t, settlement.terminal)
				require.Equal(t, forwarder.body, w.Body.String())
			default:
				require.Equal(t, 200, w.Code, w.Body.String())
				require.Equal(t, forwarder.body, w.Body.String())
				require.NotNil(t, settlement.terminal)
				require.Equal(t, mode != "usage_unknown", settlement.terminal.UsageKnown)
				if mode != "usage_unknown" {
					require.Equal(t, 31, settlement.terminal.InputTokens)
					require.Equal(t, 7, settlement.terminal.OutputTokens)
				}
			}
		})
	}
}

func (f *aiControlForwarder) ForwardAIRoutingResponsesControl(ctx context.Context, account *service.Account, _, method, responseID, operation, query string) (*http.Response, error) {
	if f.beforeSend != nil {
		f.beforeSend()
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://synthetic.invalid/responses/"+responseID, nil)
	if err != nil {
		return nil, err
	}
	if err := service.CheckAIRoutingOutboundRequest(req, account.ID); err != nil {
		return nil, err
	}
	f.calls++
	status := f.status
	if status == 0 {
		status = 200
	}
	contentType := f.contentType
	if contentType == "" {
		contentType = "application/json"
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(f.body))}, nil
}

func aiControlFixture(t *testing.T) (*service.APIKey, *service.AIRoutingPolicy, *service.AIRoutingOriginBinding, *aiControlStore, *aiRoutingRuntime) {
	t.Helper()
	key := &service.APIKey{ID: 3, UserID: 2, Group: &service.Group{ID: 1, Platform: service.PlatformComposite, Status: service.StatusActive, AIRouting: true}}
	policy := &service.AIRoutingPolicy{GroupID: 1, Enabled: true}
	origin := &service.AIRoutingOriginBinding{Candidate: "c1", Account: 7, AccountTarget: 7, Model: "gpt-4o", Platform: service.PlatformOpenAI, ResponseID: "resp_A", PublicModel: "alias", Native: true, Store: true}
	store := &aiControlStore{}
	require.NoError(t, saveAIResponsesBinding(t.Context(), store, key, service.AIRoutingOriginRef{Kind: "response", ID: "resp_A"}, *origin))
	runtime := &aiRoutingRuntime{policies: &aiHTTPPolicy{policy: policy}, store: store, decider: &aiHTTPChooser{}}
	return key, policy, origin, store, runtime
}

func runAIControl(t *testing.T, method, path string, key *service.APIKey, policy *service.AIRoutingPolicy, runtime *aiRoutingRuntime, validator *aiControlValidator, forwarder *aiControlForwarder) *httptest.ResponseRecorder {
	t.Helper()
	output := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(output)
	c.Request = httptest.NewRequest(method, path, nil)
	require.True(t, routeAIResponsesControlWith(c, runtime, key, policy, validator, forwarder, &aiControlBackgroundSettlement{}))
	require.True(t, c.IsAborted())
	chooser, ok := runtime.decider.(*aiHTTPChooser)
	require.True(t, ok)
	require.Zero(t, chooser.calls)
	return output
}

func TestAIResponsesControlRetrieveAndInputOrigins(t *testing.T) {
	key, policy, origin, store, runtime := aiControlFixture(t)
	prior := *origin
	prior.ResponseID = "resp_prior"
	require.NoError(t, saveAIResponsesBinding(t.Context(), store, key, service.AIRoutingOriginRef{Kind: "response", ID: prior.ResponseID}, prior))
	require.NoError(t, saveAIResponsesBinding(t.Context(), store, key, service.AIRoutingOriginRef{Kind: "item", ID: "item_prior"}, prior))
	for _, tc := range []struct{ path, body string }{
		{"/v1/responses/resp_A", `{"id":"resp_A","output":[{"id":"item_new","type":"message"}]}`},
		{"/responses/resp_A/input_items", `{"object":"list","data":[{"id":"item_prior","type":"message"},{"id":"item_input_new","type":"message"}],"first_id":"item_prior","last_id":"item_input_new"}`},
	} {
		forwarder := &aiControlForwarder{body: tc.body}
		validator := &aiControlValidator{}
		output := runAIControl(t, "GET", tc.path, key, policy, runtime, validator, forwarder)
		require.Equal(t, 200, output.Code)
		require.JSONEq(t, tc.body, output.Body.String())
		require.Equal(t, 1, forwarder.calls)
		require.Equal(t, 2, validator.calls)
	}
	_, old, err := loadAIResponsesBinding(t.Context(), store, key, service.AIRoutingRequest{OriginRefs: []service.AIRoutingOriginRef{{Kind: "item", ID: "item_prior"}}})
	require.NoError(t, err)
	require.Equal(t, "resp_prior", old.ResponseID)
	store.rows[service.AIRoutingOriginKey(1, 2, 3, "response", "resp_prior")] = "revoked"
	forwarder := &aiControlForwarder{body: `{"data":[{"id":"item_prior","type":"message"}]}`}
	output := runAIControl(t, "GET", "/responses/resp_A/input_items", key, policy, runtime, &aiControlValidator{}, forwarder)
	require.NotContains(t, output.Body.String(), "item_prior")
	require.Equal(t, 502, output.Code)
}

func TestAIResponsesControlDeleteRevokesChildrenAndReplay(t *testing.T) {
	key, policy, origin, store, runtime := aiControlFixture(t)
	tool := service.AIRoutingOriginRef{Kind: "tool_call", ID: "call_A"}
	require.NoError(t, saveAIResponsesBinding(t.Context(), store, key, tool, *origin))
	forwarder := &aiControlForwarder{body: `{"id":"resp_A","deleted":true}`}
	output := runAIControl(t, "DELETE", "/backend-api/codex/responses/resp_A", key, policy, runtime, &aiControlValidator{}, forwarder)
	require.Equal(t, 200, output.Code)
	responseKey := service.AIRoutingOriginKey(1, 2, 3, "response", "resp_A")
	require.Equal(t, "revoked", store.rows[responseKey])
	require.Equal(t, service.AIRoutingResponseOriginTTL, store.ttls[responseKey])
	_, _, err := loadAIResponsesBinding(t.Context(), store, key, service.AIRoutingRequest{OriginRefs: []service.AIRoutingOriginRef{tool}, RequiresOrigin: true})
	require.Error(t, err)
	require.Error(t, saveAIResponsesBinding(t.Context(), store, key, tool, *origin))
	output = runAIControl(t, "GET", "/v1/responses/resp_A", key, policy, runtime, &aiControlValidator{}, forwarder)
	require.Equal(t, 404, output.Code)
	require.Equal(t, 1, forwarder.calls)
}

func TestAIResponsesControlStreamsAndAllowsOnlyBoundBackgroundCancel(t *testing.T) {
	key, policy, origin, store, runtime := aiControlFixture(t)
	forwarder := &aiControlForwarder{contentType: "text/event-stream", body: "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_A\"}}\n\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"item_A\",\"type\":\"function_call\",\"call_id\":\"call_A\"}}\n\n"}
	output := runAIControl(t, "GET", "/v1/responses/resp_A?stream=true", key, policy, runtime, &aiControlValidator{}, forwarder)
	require.Equal(t, forwarder.body, output.Body.String())
	require.True(t, output.Flushed)
	_, _, err := loadAIResponsesBinding(t.Context(), store, key, service.AIRoutingRequest{OriginRefs: []service.AIRoutingOriginRef{{Kind: "tool_call", ID: "call_A"}}})
	require.NoError(t, err)
	origin.Background = true
	raw, err := json.Marshal(origin)
	require.NoError(t, err)
	store.rows[service.AIRoutingOriginKey(1, 2, 3, "response", "resp_A")] = string(raw)
	forwarder = &aiControlForwarder{body: `{"id":"resp_A","status":"cancelled","output":[]}`}
	output = runAIControl(t, "POST", "/v1/responses/resp_A/cancel", key, policy, runtime, &aiControlValidator{}, forwarder)
	require.Equal(t, 200, output.Code)
	require.Equal(t, 1, forwarder.calls)
	require.JSONEq(t, forwarder.body, output.Body.String())
}

func TestAIResponsesControlStartedSSEFailureUsesNativeError(t *testing.T) {
	key, policy, _, _, runtime := aiControlFixture(t)
	created := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_A\"}}\n\n"
	forwarder := &aiControlForwarder{contentType: "text/event-stream", body: created + "data: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"unbound_item\",\"Call_ID\":\"invalid_case\"}}\n\n"}
	output := runAIControl(t, "GET", "/v1/responses/resp_A?stream=true", key, policy, runtime, &aiControlValidator{}, forwarder)
	require.Equal(t, http.StatusOK, output.Code)
	require.True(t, strings.HasPrefix(output.Body.String(), created))
	require.NotContains(t, output.Body.String(), "unbound_item")
	parts := strings.Split(output.Body.String(), "event: error\ndata: ")
	require.Len(t, parts, 2)
	require.JSONEq(t, `{"type":"error","code":"ai_response_binding_failed","message":"The requested AI response resource is unavailable for this operation.","param":null}`, strings.TrimSpace(parts[1]))
	var event map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(parts[1])), &event))
	require.NotContains(t, event, "error")
}

func TestAIResponsesControlFailClosedAndPreservesProviderError(t *testing.T) {
	for _, mode := range []string{"other-key", "revoked-policy", "revoked-before-send", "cancel", "legacy-alias", "upstream", "revoke-failure"} {
		t.Run(mode, func(t *testing.T) {
			key, policy, origin, store, runtime := aiControlFixture(t)
			validator := &aiControlValidator{}
			forwarder := &aiControlForwarder{body: `{"id":"resp_A","deleted":true}`}
			method, path, status, calls := "GET", "/v1/responses/resp_A", 403, 0
			switch mode {
			case "other-key":
				key.ID++
				status = 404
			case "revoked-policy":
				validator.err = errors.New("revoked")
			case "revoked-before-send":
				forwarder.beforeSend = func() { policy.Enabled = false }
				status = 502
			case "cancel":
				method, path, status = "POST", "/v1/responses/resp_A/cancel", 409
			case "legacy-alias":
				origin.PublicModel = ""
				raw, err := json.Marshal(origin)
				require.NoError(t, err)
				store.rows[service.AIRoutingOriginKey(1, 2, 3, "response", "resp_A")] = string(raw)
				status = 404
			case "upstream":
				forwarder.status, forwarder.body = 501, `{"error":{"message":"synthetic unsupported"}}`
				status, calls = 501, 1
			case "revoke-failure":
				method = "DELETE"
				store.revokeErr = errors.New("unavailable")
				status, calls = 503, 1
			}
			output := runAIControl(t, method, path, key, policy, runtime, validator, forwarder)
			require.Equal(t, status, output.Code, output.Body.String())
			require.Equal(t, calls, forwarder.calls)
			if mode == "upstream" {
				require.JSONEq(t, forwarder.body, output.Body.String())
			}
		})
	}
}

func TestAIResponsesOriginStoredReferenceAndParentPresence(t *testing.T) {
	key, _, origin, store, _ := aiControlFixture(t)
	origin.Store = false
	raw, err := json.Marshal(origin)
	require.NoError(t, err)
	store.rows[service.AIRoutingOriginKey(1, 2, 3, "response", "resp_A")] = string(raw)
	ref := service.AIRoutingOriginRef{Kind: "item", ID: "reason_A"}
	require.NoError(t, saveAIResponsesBinding(t.Context(), store, key, ref, *origin))
	_, _, err = loadAIResponsesBinding(t.Context(), store, key, service.AIRoutingRequest{StoreResponse: true, OriginRefs: []service.AIRoutingOriginRef{ref}})
	require.NoError(t, err, "inline encrypted state does not require provider storage, even if the new request asks to store")
	ref.RequiresStored = true
	_, _, err = loadAIResponsesBinding(t.Context(), store, key, service.AIRoutingRequest{OriginRefs: []service.AIRoutingOriginRef{ref}})
	require.Error(t, err)
	delete(store.rows, service.AIRoutingOriginKey(1, 2, 3, "response", "resp_A"))
	ref.RequiresStored = false
	_, _, err = loadAIResponsesBinding(t.Context(), store, key, service.AIRoutingRequest{OriginRefs: []service.AIRoutingOriginRef{ref}})
	require.Error(t, err, "missing parent never authorizes a child")
}
