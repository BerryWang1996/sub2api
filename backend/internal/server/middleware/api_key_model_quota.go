package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type modelQuotaRequest struct {
	endpoint string
}

// NewModelQuotaAuth is registered only on the canonical model collection.
func NewModelQuotaAuth(
	ordinary APIKeyAuthMiddleware,
	apiKeys *service.APIKeyService,
	subscriptions *service.SubscriptionService,
	cfg *config.Config,
	read func(*gin.Context, string),
) gin.HandlerFunc {
	authenticate := apiKeyAuthForRequest(apiKeys, subscriptions, cfg, read)
	return func(c *gin.Context) {
		quota, err := parseModelQuotaRequest(c)
		if quota == nil && err == nil {
			ordinary(c)
			return
		}
		c.Header("Cache-Control", "private, no-store")
		if err != nil {
			AbortWithError(c, http.StatusBadRequest, "invalid_model_quota_query", err.Error())
			return
		}
		authenticate(c, quota)
	}
}

func parseModelQuotaRequest(c *gin.Context) (*modelQuotaRequest, error) {
	const modelsPath = "/v1/models"
	r := c.Request
	if r.Method != http.MethodGet || c.FullPath() != modelsPath || r.URL.EscapedPath() != modelsPath {
		return nil, nil
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, errors.New("malformed model query string")
	}

	var include string
	for name, values := range query {
		if !strings.EqualFold(name, "include") {
			continue
		}
		if name != "include" || len(values) != 1 || strings.TrimSpace(values[0]) == "" {
			return nil, errors.New("the include parameter must use its standard name and a single non-empty value")
		}
		include = values[0]
	}
	if include != "quota" {
		for _, value := range strings.Split(include, ",") {
			if strings.EqualFold(strings.TrimSpace(value), "quota") {
				return nil, errors.New("request quota with exactly include=quota")
			}
		}
		return nil, nil
	}
	if query.Has("client_version") {
		return nil, errors.New("include=quota cannot be combined with client_version")
	}
	for name := range query {
		if name != "include" && name != "endpoint" {
			return nil, errors.New("unsupported parameter for include=quota")
		}
	}
	quota := &modelQuotaRequest{}
	if endpoints, ok := query["endpoint"]; ok {
		if len(endpoints) == 1 {
			switch endpoints[0] {
			case service.CompositeRouteEndpointMessages, service.CompositeRouteEndpointChatCompletions,
				service.CompositeRouteEndpointResponses, service.CompositeRouteEndpointGemini:
				quota.endpoint = endpoints[0]
				return quota, nil
			}
		}
		return nil, errors.New("endpoint must be one of messages, chat_completions, responses or gemini")
	}
	return quota, nil
}

func serveAuthenticatedModelQuota(
	c *gin.Context,
	key *service.APIKey,
	subscriptions *service.SubscriptionService,
	cfg *config.Config,
	quota *modelQuotaRequest,
	read func(*gin.Context, string),
) {
	if key.UserID <= 0 || key.User.ID != key.UserID {
		AbortWithError(c, http.StatusUnauthorized, "USER_NOT_FOUND", "User associated with API key not found")
		return
	}
	if key.Status == service.StatusAPIKeyExpired || (key.ExpiresAt != nil && !time.Now().Before(*key.ExpiresAt)) {
		AbortWithError(c, http.StatusForbidden, "API_KEY_EXPIRED", "API key 已过期")
		return
	}
	if key.GroupID == nil || key.Group == nil || *key.GroupID <= 0 || key.Group.ID != *key.GroupID {
		AbortWithError(c, http.StatusForbidden, "GROUP_REQUIRED", "An active source group is required")
		return
	}
	c.Set(string(ContextKeySubscription), nil)
	if cfg.RunMode != config.RunModeSimple && key.Group.IsSubscriptionType() {
		sub, err := subscriptions.GetActiveSubscriptionForModelQuota(c.Request.Context(), key.User.ID, key.Group.ID)
		if c.Request.Context().Err() != nil {
			c.AbortWithStatus(statusClientClosedRequest)
			return
		}
		if errors.Is(err, service.ErrSubscriptionNotFound) {
			AbortWithError(c, http.StatusForbidden, "SUBSCRIPTION_NOT_FOUND", "No active subscription found for this group")
			return
		}
		if err != nil {
			AbortWithError(c, http.StatusServiceUnavailable, "quota_subscription_unavailable", "Unable to verify the active subscription")
			return
		}
		c.Set(string(ContextKeySubscription), sub)
	}
	if read == nil {
		AbortWithError(c, http.StatusServiceUnavailable, "quota_unavailable", "Model quota is unavailable")
		return
	}
	c.Set(string(ContextKeyAPIKey), key)
	c.Set(string(ContextKeyUser), AuthSubject{UserID: key.User.ID, Concurrency: key.User.Concurrency})
	c.Set(string(ContextKeyUserRole), key.User.Role)
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ctxkey.Group, key.Group))
	// A Key admitted for quota inspection may still be disallowed on the regular chain.
	c.Abort()
	read(c, quota.endpoint)
}
