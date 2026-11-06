package handler

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// httpPrincipal owns one request's source snapshot. Production always supplies
// the repository-backed reader in SetAIRouting; explicitly constructed test
// runtimes may omit it. Never modify the shared authentication-cache projection.
func (r *aiRoutingRuntime) httpPrincipal(c *gin.Context, original *service.APIKey) (*aiRoutingRuntime, *service.APIKey, error) {
	if r.principal == nil {
		return r, original, nil
	}
	local := *r
	local.clientIP = middleware.SecurityClientIP(c)
	// Capture the authenticated identity, not a later mutable relation snapshot.
	anchor := *original
	if original.GroupID != nil {
		id := *original.GroupID
		anchor.GroupID = &id
	}
	local.principal = func(ctx context.Context, _ *service.APIKey, _ string) (*service.AIRoutingPrincipal, error) {
		return r.principal(ctx, &anchor, local.clientIP)
	}
	principal, err := local.readPrincipal(c.Request.Context(), original)
	if err != nil {
		return nil, nil, err
	}
	key := principal.APIKey
	c.Set(string(middleware.ContextKeyAPIKey), key)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: key.User.ID, Concurrency: key.User.Concurrency})
	c.Set(string(middleware.ContextKeyUserRole), key.User.Role)
	c.Set(string(middleware.ContextKeySubscription), principal.Subscription)
	ctx := context.WithValue(c.Request.Context(), ctxkey.Group, key.Group)
	ctx = context.WithValue(ctx, ctxkey.UserID, key.User.ID)
	c.Request = c.Request.WithContext(ctx)
	local.eligible = func(ctx context.Context, source *service.APIKey, p *service.AIRoutingPolicy, req service.AIRoutingRequest, endpoint string, bound int64) []service.AIRoutingCandidate {
		fresh, readErr := local.requestPrincipal(ctx, source, req, endpoint)
		if readErr != nil {
			return nil
		}
		ctx = context.WithValue(ctx, ctxkey.Group, fresh.Group)
		return r.eligible(ctx, fresh, p, req, endpoint, bound)
	}
	return &local, key, nil
}

func (r *aiRoutingRuntime) readPrincipal(ctx context.Context, key *service.APIKey) (*service.AIRoutingPrincipal, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	principal := &service.AIRoutingPrincipal{APIKey: key}
	if r.principal != nil {
		var err error
		principal, err = r.principal(ctx, key, r.clientIP)
		if err != nil {
			return nil, err
		}
		if principal == nil || principal.APIKey == nil || principal.APIKey.Group == nil || principal.APIKey.User == nil {
			return nil, service.ErrAIRoutingPrincipalRevoked
		}
	}
	if r.revalidateSource != nil {
		if err := r.revalidateSource(ctx, principal.APIKey); err != nil {
			return nil, err
		}
	}
	return principal, ctx.Err()
}

func (r *aiRoutingRuntime) requestPrincipal(ctx context.Context, key *service.APIKey, req service.AIRoutingRequest, endpoint string) (*service.APIKey, error) {
	principal, err := r.readPrincipal(ctx, key)
	if err != nil {
		return nil, err
	}
	fresh := principal.APIKey
	if fresh == nil || fresh.Group == nil {
		return nil, service.ErrAIRoutingPrincipalRevoked
	}
	if blockedModelAllowlistCandidate(fresh.Group, []string{req.PublicModel}) != "" {
		return nil, service.ErrAIRoutingPrincipalRevoked
	}
	if fresh.Group.ClaudeCodeOnly && ((endpoint != service.CompositeRouteEndpointMessages && endpoint != service.CompositeRouteEndpointCountTokens) || !service.IsClaudeCodeClient(ctx)) {
		return nil, service.ErrAIRoutingPrincipalRevoked
	}
	return fresh, nil
}
