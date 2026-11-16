package routes

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// The forwarder writes and the model-list handler reads the same account state.
type modelQuotaFlowAccounts struct {
	service.AccountRepository
	group   *service.Group
	account *service.Account
}

func (r *modelQuotaFlowAccounts) ReadModelQuotaCatalog(_ context.Context, groupID int64, _ bool) (service.ModelQuotaCatalogInventory, error) {
	if groupID != r.group.ID {
		return service.ModelQuotaCatalogInventory{}, nil
	}
	return service.ModelQuotaCatalogInventory{Accounts: []service.Account{*r.account}}, nil
}

func (r *modelQuotaFlowAccounts) ReadModelQuotaTargets(_ context.Context, groupIDs, _ []int64, _ int) (service.ModelQuotaTargetInventory, error) {
	for _, id := range groupIDs {
		if id == r.group.ID {
			return service.ModelQuotaTargetInventory{
				Groups:   map[int64]service.Group{id: *r.group},
				Accounts: []service.ModelQuotaAccountLink{{ID: r.account.ID, Groups: []int64{id}}},
				Complete: true,
			}, nil
		}
	}
	return service.ModelQuotaTargetInventory{Complete: true}, nil
}

func (r *modelQuotaFlowAccounts) GetByIDs(_ context.Context, ids []int64) ([]*service.Account, error) {
	for _, id := range ids {
		if id == r.account.ID {
			return []*service.Account{r.account}, nil
		}
	}
	return nil, nil
}

func (r *modelQuotaFlowAccounts) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	if id == r.account.ID {
		for key, value := range updates {
			r.account.Extra[key] = value
		}
	}
	return nil
}

type modelQuotaFlowUpstream struct {
	service.HTTPUpstream
	reset time.Time
	calls int
}

func (u *modelQuotaFlowUpstream) DoWithTLS(_ *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	u.calls++
	const response = `event: message_start
data: {"type":"message_start","message":{"id":"msg_fixture","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"usage":{"input_tokens":5,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"fixture answer"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}

event: message_stop
data: {"type":"message_stop"}

`
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": {"text/event-stream"},
			http.CanonicalHeaderKey("anthropic-ratelimit-unified-7d-utilization"): {"0.3"},
			http.CanonicalHeaderKey("anthropic-ratelimit-unified-7d-reset"):       {strconv.FormatInt(u.reset.Unix(), 10)},
		},
		Body: io.NopCloser(strings.NewReader(response)),
	}, nil
}

func TestModelQuotaNaturalReplyThenExhaustionAndRevocation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{RunMode: config.RunModeStandard}
	const model = "claude-sonnet-4-6"
	group := &service.Group{ID: 42, Hydrated: true, Platform: service.PlatformAnthropic,
		Status: service.StatusActive, SubscriptionType: service.SubscriptionTypeStandard}
	user := &service.User{ID: 8, Role: service.RoleUser, Status: service.StatusActive, Balance: 10}
	key := &service.APIKey{ID: 9, UserID: user.ID, User: user, GroupID: &group.ID, Group: group,
		Key: "quota-flow-fixture-key", Status: service.StatusActive, Quota: 1}
	keys := &modelQuotaRouteKeyRepo{keyBillingRouteAPIKeyRepo: keyBillingRouteAPIKeyRepo{apiKey: key}}
	apiKeys := service.NewAPIKeyService(keys, nil, nil, nil, nil, nil, cfg)
	account := &service.Account{ID: 7, Name: "private-fixture-account", Platform: service.PlatformAnthropic,
		Type: service.AccountTypeAPIKey, Status: service.StatusActive, Schedulable: true,
		Credentials:   map[string]any{"api_key": "private-fixture-upstream-key", "model_mapping": map[string]any{model: model}},
		Extra:         map[string]any{},
		AccountGroups: []service.AccountGroup{{AccountID: 7, GroupID: group.ID}}}
	accounts := &modelQuotaFlowAccounts{account: account, group: group}
	upstream := &modelQuotaFlowUpstream{reset: time.Now().Add(6 * time.Hour).Truncate(time.Second)}
	limits := service.NewRateLimitService(accounts, nil, cfg, nil, nil)
	billing := service.NewBillingCacheService(nil, nil, nil, keys, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	gateway := service.NewGatewayService(accounts, nil, nil, nil, nil, nil, nil, nil, cfg,
		nil, nil, nil, limits, billing, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	// Exercise the actual compatibility forwarder against an isolated upstream reply.
	forwarded := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(forwarded)
	body := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"fixture"}],"max_tokens":8}`)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	c.Request.Header.Set("Authorization", "Bearer "+key.Key)
	result, err := gateway.ForwardAsChatCompletions(c.Request.Context(), c, account, body, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Contains(t, forwarded.Body.String(), "fixture answer")

	handlers := &handler.Handlers{
		Gateway:       handler.NewGatewayHandler(gateway, nil, nil, nil, nil, nil, nil, nil, apiKeys, nil, nil, nil, nil, cfg, nil),
		OpenAIGateway: &handler.OpenAIGatewayHandler{},
		AsyncImage:    handler.NewAsyncImageHandler(nil, nil),
	}
	router := gin.New()
	RegisterGatewayRoutes(router, handlers, middleware.NewAPIKeyAuthMiddleware(apiKeys, nil, cfg), apiKeys, nil, nil, nil, nil, cfg)
	call := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+key.Key)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	const quotaPath = "/v1/models?include=quota&endpoint=chat_completions"
	w := call(http.MethodGet, quotaPath)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	q := gjson.Get(w.Body.String(), `data.#(id=="claude-sonnet-4-6").sub2api_quota`)
	require.Equal(t, "available", q.Get("status").String())
	window := q.Get(`windows.#(window=="7d")`)
	require.EqualValues(t, 30, window.Get("used_percent").Float())
	observed, err := time.Parse(time.RFC3339, window.Get("observed_at").String())
	require.NoError(t, err)
	require.WithinDuration(t, time.Now(), observed, time.Minute)
	require.NotContains(t, w.Body.String(), "private-fixture")

	key.Status, key.QuotaUsed = service.StatusAPIKeyQuotaExhausted, 1
	w = call(http.MethodGet, quotaPath)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "blocked", gjson.Get(w.Body.String(), "sub2api_quota.key_admission.status").String())
	require.Equal(t, http.StatusTooManyRequests, call(http.MethodPost, "/v1/chat/completions").Code)
	group.Status = "disabled"
	require.Equal(t, http.StatusForbidden, call(http.MethodGet, quotaPath).Code)
	require.Equal(t, 1, upstream.calls, "only the fixture inference may call the Anthropic upstream")
}
