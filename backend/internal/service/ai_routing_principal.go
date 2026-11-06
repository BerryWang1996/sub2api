package service

import (
	"context"
	"crypto/subtle"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
)

var ErrAIRoutingPrincipalRevoked = errors.New("AI routing credentials or source permissions are no longer valid")

// A long-lived connection authenticates each turn from repository state, not
// the connection's cached billing-group projection. Never silently switch its
// owning user, Key or source group after credential rotation or reassignment.
type AIRoutingPrincipal struct {
	APIKey       *APIKey
	Subscription *UserSubscription
}

func (s *APIKeyService) RevalidateAIRoutingKey(ctx context.Context, original *APIKey, securityClientIP string) (*AIRoutingPrincipal, error) {
	deny := ErrAIRoutingPrincipalRevoked
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if s == nil || s.apiKeyRepo == nil || s.userRepo == nil || s.groupRepo == nil || original == nil || original.ID <= 0 || original.UserID <= 0 || original.GroupID == nil || *original.GroupID <= 0 || original.Key == "" {
		return nil, deny
	}
	key, err := s.apiKeyRepo.GetByID(ctx, original.ID)
	if err != nil || key == nil || key.ID != original.ID || key.UserID != original.UserID || key.GroupID == nil || *key.GroupID != *original.GroupID || subtle.ConstantTimeCompare([]byte(key.Key), []byte(original.Key)) != 1 {
		return nil, deny
	}
	simple := s.cfg != nil && s.cfg.RunMode == config.RunModeSimple
	if !key.IsActive() && (!simple || key.Status != StatusAPIKeyExpired && key.Status != StatusAPIKeyQuotaExhausted) || !simple && (key.IsExpired() || key.IsQuotaExhausted()) {
		return nil, deny
	}
	// GetByID's joined User does not load allowed-group edges. Read the complete
	// user repository view instead of treating missing edges as authorization.
	user, err := s.userRepo.GetByID(ctx, key.UserID)
	if err != nil || user == nil || user.ID != original.UserID || !user.IsActive() {
		return nil, deny
	}
	group, err := s.groupRepo.GetByID(ctx, *key.GroupID)
	if err != nil || group == nil || group.ID != *original.GroupID || !group.IsActive() || group.Platform != PlatformComposite || !group.AIRouting || !group.IsSubscriptionType() && !user.CanBindGroup(group.ID, group.IsExclusive) {
		return nil, deny
	}
	// Own the snapshot; no mutable relation pointer from the old connection is
	// reused for this turn's quotas, concurrency, model allowlist or billing.
	current := *key
	current.User, current.Group = user, group
	s.compileAPIKeyIPRules(&current)
	if len(current.IPWhitelist) != 0 || len(current.IPBlacklist) != 0 {
		if allowed, _ := ip.CheckIPRestrictionWithCompiledRules(securityClientIP, current.CompiledIPWhitelist, current.CompiledIPBlacklist); !allowed {
			return nil, deny
		}
	}
	principal := &AIRoutingPrincipal{APIKey: &current}
	if !simple && group.IsSubscriptionType() {
		if s.userSubRepo == nil {
			return nil, deny
		}
		sub, err := s.userSubRepo.GetActiveByUserIDAndGroupID(ctx, user.ID, group.ID)
		if err != nil || sub == nil || sub.UserID != user.ID || sub.GroupID != group.ID || !sub.IsActive() {
			return nil, deny
		}
		principal.Subscription = sub
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return principal, nil
}
