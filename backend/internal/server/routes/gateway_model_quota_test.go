package routes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type modelQuotaRouteKeyRepo struct {
	keyBillingRouteAPIKeyRepo
	touches int
}

func (r *modelQuotaRouteKeyRepo) UpdateLastUsed(context.Context, int64, time.Time) error {
	r.touches++
	return nil
}

func TestModelQuotaGatewayRoutesIsolateCanonicalReadOnlyRepresentation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{RunMode: config.RunModeStandard}
	group := &service.Group{
		ID: 42, Hydrated: true, Status: service.StatusActive, Platform: service.PlatformOpenAI,
		SubscriptionType:          service.SubscriptionTypeStandard,
		CodexModelsManifestConfig: service.GroupCodexModelsManifestConfig{Enabled: true, AccountIDs: []int64{7}},
	}
	user := &service.User{ID: 8, Role: service.RoleUser, Status: service.StatusActive, Balance: 10}
	key := &service.APIKey{ID: 9, UserID: user.ID, User: user, GroupID: &group.ID, Group: group,
		Key: "quota-route-fixture-key", Status: service.StatusAPIKeyQuotaExhausted}
	keys := &modelQuotaRouteKeyRepo{keyBillingRouteAPIKeyRepo: keyBillingRouteAPIKeyRepo{apiKey: key}}
	apiKeys := service.NewAPIKeyService(keys, nil, nil, nil, nil, nil, cfg)
	accounts := &pinnedModelsRoutesRepository{account: service.Account{
		ID: 7, Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true,
		Credentials: map[string]any{"api_key": "upstream-fixture-key", "base_url": "https://models.example/v1"},
	}}
	upstream := &pinnedModelsRoutesUpstream{}
	openAI := service.NewOpenAIGatewayService(accounts, nil, nil, nil, nil, nil, nil, cfg,
		nil, nil, nil, nil, nil, upstream, nil, nil, nil, nil, nil, nil, nil, nil)
	handlers := &handler.Handlers{
		Gateway:       handler.NewGatewayHandler(nil, openAI, nil, nil, nil, nil, nil, nil, apiKeys, nil, nil, nil, nil, cfg, nil),
		OpenAIGateway: handler.NewOpenAIGatewayHandler(openAI, nil, nil, nil, nil, nil, nil, nil, cfg),
		AsyncImage:    handler.NewAsyncImageHandler(nil, nil),
	}
	router := gin.New()
	RegisterGatewayRoutes(router, handlers, middleware.NewAPIKeyAuthMiddleware(apiKeys, nil, cfg),
		apiKeys, nil, nil, nil, nil, cfg)

	call := func(method, path string, authorized bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if authorized {
			req.Header.Set("Authorization", "Bearer "+key.Key)
		}
		req.Header.Set("If-None-Match", `"prior-catalog-etag"`)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	w := call(http.MethodGet, "/v1/models?include=quota", false)
	require.Equal(t, http.StatusUnauthorized, w.Code)
	require.Equal(t, "private, no-store", w.Header().Get("Cache-Control"))

	w = call(http.MethodGet, "/v1/models?include=quota&endpoint=chat_completions", true)
	require.Equal(t, http.StatusNotImplemented, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "quota_catalog_unsupported")
	require.Equal(t, "private, no-store", w.Header().Get("Cache-Control"))
	require.Empty(t, w.Header().Get("ETag"))

	for _, query := range []string{
		"include=quota&client_version=", "include=quota&client_version=v", "include=quota;foo=1&client_version=v",
		"in%63lude=%ZZ&client_version=v", "x=%ZZ&client_version=v", "include=", "include", "Include=quota",
		"include=quota&include=other", "include=other&include=quota", "Include=x&include=quota",
		"include=QuOtA", "include=%20quota", "include=other,%20QuOtA%20", "include=quota&endpoint=",
		"include=quota&endpoint=invalid", "include=quota&endpoint=gemini&endpoint=gemini", "include=quota&group_id=1",
		"include=quota&key_id=1", "include=quota&api_key=not-a-query-credential", "include=quota&model=any",
	} {
		w := call(http.MethodGet, "/v1/models?"+query, true)
		require.Equal(t, http.StatusBadRequest, w.Code, query+" "+w.Body.String())
		require.Contains(t, w.Body.String(), "invalid_model_quota_query")
		require.Equal(t, "private, no-store", w.Header().Get("Cache-Control"))
	}
	for _, path := range []string{
		"/models?include=quota", "/v1/%6dodels?include=quota", "/v1/models/one?include=quota",
		"/backend-api/codex/models?include=quota", "/v1/models", "/v1/models?include=other",
	} {
		w := call(http.MethodGet, path, true)
		require.Equal(t, http.StatusTooManyRequests, w.Code, path+" "+w.Body.String())
		require.Empty(t, w.Header().Get("Cache-Control"))
	}
	require.Equal(t, http.StatusMovedPermanently, call(http.MethodGet, "/v1/models/?include=quota", true).Code)
	require.Equal(t, http.StatusNotFound, call(http.MethodHead, "/v1/models?include=quota", true).Code)
	require.Equal(t, http.StatusNotFound, call(http.MethodOptions, "/v1/models?include=quota", true).Code)
	require.Equal(t, http.StatusNotFound, call(http.MethodPost, "/v1/models?include=quota", true).Code)
	require.Equal(t, http.StatusTooManyRequests, call(http.MethodPost, "/v1/messages?include=quota", true).Code)
	require.Zero(t, keys.touches)
	require.Zero(t, upstream.ordinaryCalls.Load())
	require.Zero(t, upstream.codexCalls.Load())

	key.Status = service.StatusActive
	w = call(http.MethodGet, "/v1/models?include=other", true)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "ordinary-upstream-model")
	require.NotContains(t, w.Body.String(), "sub2api_quota")
	w = call(http.MethodGet, "/v1/models?client_version="+service.CodexCanonicalClientVersion(), true)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), `"slug":"gpt-5.5"`)
	require.EqualValues(t, 1, upstream.ordinaryCalls.Load())
	require.EqualValues(t, 1, upstream.codexCalls.Load())
}

type modelQuotaRouteCatalogRepo struct {
	service.AccountRepository
	t     *testing.T
	group *service.Group
}

func (r *modelQuotaRouteCatalogRepo) ReadModelQuotaCatalog(_ context.Context, groupID int64, _ bool) (service.ModelQuotaCatalogInventory, error) {
	require.Equal(r.t, r.group.ID, groupID)
	// The configured alias survives the absence of any currently eligible execution path.
	return service.ModelQuotaCatalogInventory{Accounts: []service.Account{
		{ID: 7, Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
			Credentials: map[string]any{"model_mapping": map[string]any{"quota-fixture-model": "claude-upstream-fixture"}}},
	}}, nil
}

func (r *modelQuotaRouteCatalogRepo) ReadModelQuotaTargets(context.Context, []int64, []int64, int) (service.ModelQuotaTargetInventory, error) {
	return service.ModelQuotaTargetInventory{Groups: map[int64]service.Group{r.group.ID: *r.group}, Complete: true}, nil
}

func (r *modelQuotaRouteCatalogRepo) GetByIDs(_ context.Context, ids []int64) ([]*service.Account, error) {
	require.Empty(r.t, ids)
	return nil, nil
}

func TestModelQuotaGatewayReturnsSchemaWhenInferenceKeyIsExhausted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{RunMode: config.RunModeStandard}
	group := &service.Group{ID: 42, Hydrated: true, Status: service.StatusActive, Platform: service.PlatformAnthropic,
		SubscriptionType: service.SubscriptionTypeStandard}
	user := &service.User{ID: 8, Role: service.RoleUser, Status: service.StatusActive, Balance: 10}
	key := &service.APIKey{ID: 9, UserID: user.ID, User: user, GroupID: &group.ID, Group: group,
		Key: "quota-schema-fixture-key", Status: service.StatusAPIKeyQuotaExhausted, Quota: 1, QuotaUsed: 1}
	keys := &modelQuotaRouteKeyRepo{keyBillingRouteAPIKeyRepo: keyBillingRouteAPIKeyRepo{apiKey: key}}
	apiKeys := service.NewAPIKeyService(keys, nil, nil, nil, nil, nil, cfg)
	gateway := service.NewGatewayService(
		&modelQuotaRouteCatalogRepo{t: t, group: group}, nil, nil, nil, nil, nil, nil, nil, cfg,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	handlers := &handler.Handlers{
		Gateway:       handler.NewGatewayHandler(gateway, nil, nil, nil, nil, nil, nil, nil, apiKeys, nil, nil, nil, nil, cfg, nil),
		OpenAIGateway: &handler.OpenAIGatewayHandler{},
		AsyncImage:    handler.NewAsyncImageHandler(nil, nil),
	}
	router := gin.New()
	RegisterGatewayRoutes(router, handlers, middleware.NewAPIKeyAuthMiddleware(apiKeys, nil, cfg),
		apiKeys, nil, nil, nil, nil, cfg)

	req := httptest.NewRequest(http.MethodGet, "/v1/models?include=%71uota&endpoint=messages", nil)
	req.Header.Set("Authorization", "Bearer "+key.Key)
	req.Header.Set("If-None-Match", "*")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "private, no-store", w.Header().Get("Cache-Control"))
	require.Empty(t, w.Header().Get("ETag"))
	require.NotContains(t, w.Body.String(), key.Key)
	require.NotContains(t, w.Body.String(), "claude-upstream-fixture")
	var response service.ModelQuotaResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.Equal(t, "list", response.Object)
	require.Equal(t, 1, response.Quota.SchemaVersion)
	require.Equal(t, service.CompositeRouteEndpointMessages, response.Quota.Endpoint)
	require.Len(t, response.Data, 1)
	row := response.Data[0]
	require.JSONEq(t, `"quota-fixture-model"`, string(row["id"]))
	require.JSONEq(t, `"model"`, string(row["type"]))
	var model service.ModelQuotaModel
	require.NoError(t, json.Unmarshal(row["sub2api_quota"], &model))
	require.Equal(t, "blocked", model.Status)
	require.Zero(t, model.AccountsTotal)
	defaultReq := httptest.NewRequest(http.MethodGet, "/v1/models?include=quota", nil)
	defaultReq.Header.Set("Authorization", "Bearer "+key.Key)
	defaultResponse := httptest.NewRecorder()
	router.ServeHTTP(defaultResponse, defaultReq)
	require.Equal(t, http.StatusOK, defaultResponse.Code, defaultResponse.Body.String())
	var implicit service.ModelQuotaResponse
	require.NoError(t, json.Unmarshal(defaultResponse.Body.Bytes(), &implicit))
	require.Equal(t, response.Quota.Endpoint, implicit.Quota.Endpoint)
	require.Equal(t, response.Data, implicit.Data)
	require.Zero(t, keys.touches)
}
