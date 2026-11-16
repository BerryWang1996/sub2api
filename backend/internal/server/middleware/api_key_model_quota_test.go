//go:build unit

package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func testModelQuotaKey(subscriptionType string) *service.APIKey {
	group := &service.Group{ID: 42, Status: service.StatusActive, Hydrated: true, SubscriptionType: subscriptionType}
	user := &service.User{ID: 7, Status: service.StatusActive, Role: service.RoleUser}
	return &service.APIKey{ID: 9, UserID: user.ID, User: user, GroupID: &group.ID, Group: group,
		Key: "quota-view-fixture-key", Status: service.StatusAPIKeyQuotaExhausted, Quota: 1, QuotaUsed: 1}
}

func TestModelQuotaAuthPreservesEntitlementAndDoesNotTouchUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cases := []struct {
		name         string
		runMode      string
		change       func(*service.APIKey)
		subStatus    string
		subExpired   bool
		subErr       error
		want         int
		wantSubReads int
	}{
		{name: "active subscription despite exhausted windows", subStatus: service.SubscriptionStatusActive, want: 200, wantSubReads: 1},
		{name: "simple neither reads nor requires subscription", runMode: config.RunModeSimple, subErr: service.ErrSubscriptionNotFound, want: 200},
		{name: "missing subscription", subErr: service.ErrSubscriptionNotFound, want: 403, wantSubReads: 1},
		{name: "subscription dependency unavailable", subErr: errors.New("private connection detail"), want: 503, wantSubReads: 1},
		{name: "expired subscription", subStatus: service.SubscriptionStatusActive, subExpired: true, want: 403, wantSubReads: 1},
		{name: "disabled subscription", subStatus: service.SubscriptionStatusSuspended, want: 403, wantSubReads: 1},
		{name: "expired key status", change: func(k *service.APIKey) { k.Status = service.StatusAPIKeyExpired }, want: 403},
		{name: "simple rejects elapsed key expiry", runMode: config.RunModeSimple, change: func(k *service.APIKey) {
			deadline := time.Now()
			k.ExpiresAt = &deadline
		}, want: 403},
		{name: "disabled key", change: func(k *service.APIKey) { k.Status = service.StatusDisabled }, want: 401},
		{name: "wrong IP", change: func(k *service.APIKey) { k.IPWhitelist = []string{"203.0.113.7"} }, want: 403},
		{name: "inactive source group", change: func(k *service.APIKey) { k.Group.Status = service.StatusDisabled }, want: 403},
		{name: "missing source group", change: func(k *service.APIKey) { k.Group, k.GroupID = nil, nil }, want: 403},
		{name: "inactive user", change: func(k *service.APIKey) { k.User.Status = service.StatusDisabled }, want: 401},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := testModelQuotaKey(service.SubscriptionTypeSubscription)
			if tc.change != nil {
				tc.change(key)
			}
			cfg := &config.Config{RunMode: config.RunModeStandard}
			if tc.runMode != "" {
				cfg.RunMode = tc.runMode
			}
			touchCalls, subReads, quotaCalls := 0, 0, 0
			apiKeys := service.NewAPIKeyService(&stubApiKeyRepo{
				getByKey: func(_ context.Context, credential string) (*service.APIKey, error) {
					require.Equal(t, key.Key, credential)
					return key, nil
				},
				updateLastUsed: func(context.Context, int64, time.Time) error {
					touchCalls++
					return nil
				},
			}, nil, nil, nil, nil, nil, cfg)
			subscriptions := service.NewSubscriptionService(nil, &stubUserSubscriptionRepo{
				getActive: func(_ context.Context, userID, groupID int64) (*service.UserSubscription, error) {
					subReads++
					if tc.subErr != nil {
						return nil, tc.subErr
					}
					expires := time.Now().Add(time.Hour)
					if tc.subExpired {
						expires = time.Now().Add(-time.Hour)
					}
					return &service.UserSubscription{UserID: userID, GroupID: groupID, Status: tc.subStatus,
						ExpiresAt: expires, DailyUsageUSD: 90, WeeklyUsageUSD: 100, MonthlyUsageUSD: 120}, nil
				},
			}, nil, nil, cfg)
			t.Cleanup(subscriptions.Stop)
			engine := gin.New()
			engine.Use(func(c *gin.Context) {
				// Earlier context values cannot override the authoritative group projection.
				c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.Group,
					&service.Group{ID: 42, Hydrated: true, Status: service.StatusActive, Name: "old"}))
			})
			engine.GET("/v1/models", NewModelQuotaAuth(NewAPIKeyAuthMiddleware(apiKeys, subscriptions, cfg), apiKeys, subscriptions, cfg,
				func(c *gin.Context, endpoint string) {
					quotaCalls++
					require.Equal(t, service.CompositeRouteEndpointGemini, endpoint)
					got, ok := GetAPIKeyFromContext(c)
					require.True(t, ok)
					require.Same(t, key.Group, got.Group)
					require.Same(t, key.Group, c.Request.Context().Value(ctxkey.Group))
					_, hasSub := GetSubscriptionFromContext(c)
					require.Equal(t, cfg.RunMode != config.RunModeSimple, hasSub)
					c.JSON(http.StatusOK, gin.H{"quota": true})
				}),
				func(c *gin.Context) { t.Fatal("quota request reached the ordinary chain") })

			req := httptest.NewRequest(http.MethodGet, "/v1/models?include=quota&endpoint=gemini", nil)
			req.Header.Set("x-api-key", key.Key)
			response := httptest.NewRecorder()
			engine.ServeHTTP(response, req)
			require.Equal(t, tc.want, response.Code, response.Body.String())
			require.Equal(t, "private, no-store", response.Header().Get("Cache-Control"))
			require.NotContains(t, response.Body.String(), "private connection detail")
			require.Equal(t, tc.wantSubReads, subReads)
			require.Zero(t, touchCalls)
			if tc.want == http.StatusOK {
				require.Equal(t, 1, quotaCalls)
			} else {
				require.Zero(t, quotaCalls)
			}
		})
	}
}

func TestModelQuotaAuthRejectsRevokedGroupWithWarmKeyCache(t *testing.T) {
	gin.SetMode(gin.TestMode)
	key := testModelQuotaKey(service.SubscriptionTypeStandard)
	key.Status = service.StatusActive
	key.Group.IsExclusive = true
	key.User.AllowedGroups = []int64{key.Group.ID}
	revoked := false
	lookups := 0
	cfg := &config.Config{
		RunMode:    config.RunModeStandard,
		APIKeyAuth: config.APIKeyAuthCacheConfig{L1Size: 100, L1TTLSeconds: 60},
	}
	apiKeys := service.NewAPIKeyService(&stubApiKeyRepo{
		getByKey: func(context.Context, string) (*service.APIKey, error) {
			lookups++
			keyCopy, userCopy := *key, *key.User
			if revoked {
				userCopy.AllowedGroups = nil
			}
			keyCopy.User = &userCopy
			return &keyCopy, nil
		},
	}, nil, nil, nil, nil, nil, cfg)
	require.Eventually(t, func() bool {
		before := lookups
		_, err := apiKeys.GetByKey(context.Background(), key.Key)
		return err == nil && before == lookups
	}, time.Second, time.Millisecond)
	revoked = true
	cached, err := apiKeys.GetByKey(context.Background(), key.Key)
	require.NoError(t, err)
	require.True(t, cached.User.CanBindGroup(key.Group.ID, true), "ordinary auth must still hold the old snapshot in this fixture")

	engine := gin.New()
	engine.GET("/v1/models", NewModelQuotaAuth(NewAPIKeyAuthMiddleware(apiKeys, nil, cfg), apiKeys, nil, cfg,
		func(c *gin.Context, endpoint string) { t.Fatal("revoked group reached quota service") }))
	req := httptest.NewRequest(http.MethodGet, "/v1/models?include=quota", nil)
	req.Header.Set("Authorization", "Bearer "+key.Key)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, req)
	require.Equal(t, http.StatusForbidden, response.Code)
	require.Contains(t, response.Body.String(), "GROUP_NOT_ALLOWED")
}

func TestModelQuotaAuthRejectsRevokedSubscriptionWithWarmCache(t *testing.T) {
	gin.SetMode(gin.TestMode)
	key := testModelQuotaKey(service.SubscriptionTypeSubscription)
	cfg := &config.Config{
		RunMode:           config.RunModeStandard,
		SubscriptionCache: config.SubscriptionCacheConfig{L1Size: 100, L1TTLSeconds: 60},
	}
	apiKeys := service.NewAPIKeyService(&stubApiKeyRepo{
		getByKey: func(context.Context, string) (*service.APIKey, error) { return key, nil },
	}, nil, nil, nil, nil, nil, cfg)
	revoked := false
	lookups := 0
	subscriptions := service.NewSubscriptionService(nil, &stubUserSubscriptionRepo{
		getActive: func(context.Context, int64, int64) (*service.UserSubscription, error) {
			lookups++
			if revoked {
				return nil, service.ErrSubscriptionNotFound
			}
			return &service.UserSubscription{
				UserID: key.User.ID, GroupID: key.Group.ID,
				Status: service.SubscriptionStatusActive, ExpiresAt: time.Now().Add(time.Hour),
			}, nil
		},
	}, nil, nil, cfg)
	t.Cleanup(subscriptions.Stop)
	require.Eventually(t, func() bool {
		before := lookups
		_, err := subscriptions.GetActiveSubscription(context.Background(), key.User.ID, key.Group.ID)
		return err == nil && before == lookups
	}, time.Second, time.Millisecond)
	revoked = true
	cached, err := subscriptions.GetActiveSubscription(context.Background(), key.User.ID, key.Group.ID)
	require.NoError(t, err)
	require.True(t, cached.IsActive(), "ordinary auth must still hold the old subscription in this fixture")

	engine := gin.New()
	engine.GET("/v1/models", NewModelQuotaAuth(NewAPIKeyAuthMiddleware(apiKeys, subscriptions, cfg), apiKeys, subscriptions, cfg,
		func(c *gin.Context, endpoint string) { t.Fatal("revoked subscription reached quota service") }))
	req := httptest.NewRequest(http.MethodGet, "/v1/models?include=quota", nil)
	req.Header.Set("Authorization", "Bearer "+key.Key)
	response := httptest.NewRecorder()
	engine.ServeHTTP(response, req)
	require.Equal(t, http.StatusForbidden, response.Code)
	require.Contains(t, response.Body.String(), "SUBSCRIPTION_NOT_FOUND")
}
