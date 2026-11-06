//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type aiPrincipalKeys struct {
	APIKeyRepository
	key   *APIKey
	reads int
}

func (r *aiPrincipalKeys) GetByID(context.Context, int64) (*APIKey, error) {
	r.reads++
	return r.key, nil
}

type aiPrincipalUsers struct {
	UserRepository
	user *User
}

func (r *aiPrincipalUsers) GetByID(context.Context, int64) (*User, error) { return r.user, nil }

type aiPrincipalGroups struct {
	GroupRepository
	group *Group
}

func (r *aiPrincipalGroups) GetByID(context.Context, int64) (*Group, error) { return r.group, nil }

type aiPrincipalSubscriptions struct {
	UserSubscriptionRepository
	sub *UserSubscription
}

func (r *aiPrincipalSubscriptions) GetActiveByUserIDAndGroupID(context.Context, int64, int64) (*UserSubscription, error) {
	return r.sub, nil
}

func aiPrincipalFixture() (*APIKeyService, *APIKey, *aiPrincipalKeys, *aiPrincipalUsers, *aiPrincipalGroups, *aiPrincipalSubscriptions) {
	groupID := int64(17)
	original := &APIKey{ID: 9, UserID: 42, Key: "synthetic-local-key", GroupID: &groupID, Status: StatusActive}
	current := *original
	keys := &aiPrincipalKeys{key: &current}
	users := &aiPrincipalUsers{user: &User{ID: 42, Status: StatusActive, Concurrency: 4}}
	groups := &aiPrincipalGroups{group: &Group{ID: groupID, Status: StatusActive, Platform: PlatformComposite, AIRouting: true}}
	subs := &aiPrincipalSubscriptions{sub: &UserSubscription{UserID: 42, GroupID: groupID, Status: SubscriptionStatusActive, ExpiresAt: time.Now().Add(time.Hour)}}
	return &APIKeyService{apiKeyRepo: keys, userRepo: users, groupRepo: groups, userSubRepo: subs}, original, keys, users, groups, subs
}

func TestAIRoutingPrincipalRefreshesEveryTurnAndRefusesRevocation(t *testing.T) {
	for _, change := range []struct {
		name   string
		mutate func(*aiPrincipalKeys, *aiPrincipalUsers, *aiPrincipalGroups)
	}{
		{"missing key", func(k *aiPrincipalKeys, _ *aiPrincipalUsers, _ *aiPrincipalGroups) { k.key = nil }},
		{"rotated credential", func(k *aiPrincipalKeys, _ *aiPrincipalUsers, _ *aiPrincipalGroups) { k.key.Key = "changed-key" }},
		{"disabled key", func(k *aiPrincipalKeys, _ *aiPrincipalUsers, _ *aiPrincipalGroups) {
			k.key.Status = StatusAPIKeyDisabled
		}},
		{"expired key", func(k *aiPrincipalKeys, _ *aiPrincipalUsers, _ *aiPrincipalGroups) {
			now := time.Now().Add(-time.Second)
			k.key.ExpiresAt = &now
		}},
		{"quota exhausted", func(k *aiPrincipalKeys, _ *aiPrincipalUsers, _ *aiPrincipalGroups) {
			k.key.Quota, k.key.QuotaUsed = 1, 1
		}},
		{"new owner", func(k *aiPrincipalKeys, _ *aiPrincipalUsers, _ *aiPrincipalGroups) { k.key.UserID++ }},
		{"new group", func(k *aiPrincipalKeys, _ *aiPrincipalUsers, _ *aiPrincipalGroups) {
			id := int64(18)
			k.key.GroupID = &id
		}},
		{"missing user", func(_ *aiPrincipalKeys, u *aiPrincipalUsers, _ *aiPrincipalGroups) { u.user = nil }},
		{"disabled user", func(_ *aiPrincipalKeys, u *aiPrincipalUsers, _ *aiPrincipalGroups) { u.user.Status = StatusDisabled }},
		{"missing group", func(_ *aiPrincipalKeys, _ *aiPrincipalUsers, g *aiPrincipalGroups) { g.group = nil }},
		{"disabled group", func(_ *aiPrincipalKeys, _ *aiPrincipalUsers, g *aiPrincipalGroups) { g.group.Status = StatusDisabled }},
		{"static group", func(_ *aiPrincipalKeys, _ *aiPrincipalUsers, g *aiPrincipalGroups) { g.group.AIRouting = false }},
		{"exclusive access revoked", func(_ *aiPrincipalKeys, _ *aiPrincipalUsers, g *aiPrincipalGroups) { g.group.IsExclusive = true }},
		{"public access restricted", func(_ *aiPrincipalKeys, u *aiPrincipalUsers, _ *aiPrincipalGroups) {
			u.user.RestrictPublicGroups = true
		}},
		{"IP revoked", func(k *aiPrincipalKeys, _ *aiPrincipalUsers, _ *aiPrincipalGroups) {
			k.key.IPBlacklist = []string{"192.0.2.10"}
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			svc, original, keys, users, groups, _ := aiPrincipalFixture()
			first, err := svc.RevalidateAIRoutingKey(context.Background(), original, "192.0.2.10")
			require.NoError(t, err)
			require.NotSame(t, original, first.APIKey)
			require.NotSame(t, keys.key, first.APIKey)
			require.Equal(t, 4, first.APIKey.User.Concurrency)
			change.mutate(keys, users, groups)
			_, err = svc.RevalidateAIRoutingKey(context.Background(), original, "192.0.2.10")
			require.ErrorIs(t, err, ErrAIRoutingPrincipalRevoked)
			require.Equal(t, 2, keys.reads, "no connection/auth cache fallback")
		})
	}
}

func TestAIRoutingPrincipalUsesCurrentGroupGrantsAndSubscription(t *testing.T) {
	svc, original, _, users, groups, subs := aiPrincipalFixture()
	groups.group.IsExclusive = true
	users.user.AllowedGroups = []int64{groups.group.ID}
	principal, err := svc.RevalidateAIRoutingKey(context.Background(), original, "192.0.2.10")
	require.NoError(t, err)
	require.Nil(t, principal.Subscription)
	groups.group.SubscriptionType = SubscriptionTypeSubscription
	users.user.AllowedGroups = nil
	principal, err = svc.RevalidateAIRoutingKey(context.Background(), original, "192.0.2.10")
	require.NoError(t, err)
	require.Equal(t, subs.sub, principal.Subscription)
	subs.sub.ExpiresAt = time.Now().Add(-time.Second)
	_, err = svc.RevalidateAIRoutingKey(context.Background(), original, "192.0.2.10")
	require.ErrorIs(t, err, ErrAIRoutingPrincipalRevoked)
	svc.cfg = &config.Config{RunMode: config.RunModeSimple}
	principal, err = svc.RevalidateAIRoutingKey(context.Background(), original, "192.0.2.10")
	require.NoError(t, err, "Simple mode keeps existing subscription bypass")
	require.Nil(t, principal.Subscription)
}

func TestAIRoutingPrincipalCancellationNeverUsesStaleSnapshot(t *testing.T) {
	svc, original, keys, _, _, _ := aiPrincipalFixture()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := svc.RevalidateAIRoutingKey(ctx, original, "192.0.2.10")
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, keys.reads)
}
