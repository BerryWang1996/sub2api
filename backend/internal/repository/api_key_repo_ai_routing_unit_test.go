package repository

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyRepositoryAuthProjectionAndCachePreserveAIRoutingMode(t *testing.T) {
	for _, tc := range []struct {
		name, platform string
		ai             bool
	}{
		{"ai", service.PlatformComposite, true},
		{"static", service.PlatformComposite, false},
		{"ordinary", service.PlatformOpenAI, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, client := newAPIKeyRepoSQLite(t)
			ctx := context.Background()
			user := mustCreateAPIKeyRepoUser(t, ctx, client, "auth-mode-"+tc.name+"@example.test")
			group, err := client.Group.Create().
				SetName("auth-mode-" + tc.name).
				SetPlatform(tc.platform).
				SetForkAiRouting(tc.ai).
				SetStatus(service.StatusActive).
				SetSubscriptionType(service.SubscriptionTypeStandard).
				SetRateMultiplier(1).
				Save(ctx)
			require.NoError(t, err)
			key := &service.APIKey{UserID: user.ID, GroupID: &group.ID, Key: "synthetic-auth-mode-" + tc.name, Name: tc.name, Status: service.StatusActive}
			require.NoError(t, repo.Create(ctx, key))
			full, err := repo.GetByKey(ctx, key.Key)
			require.NoError(t, err)
			projected, err := repo.GetByKeyForAuth(ctx, key.Key)
			require.NoError(t, err)
			require.NotNil(t, projected.Group)
			require.Equal(t, tc.ai, full.Group.AIRouting)
			require.Equal(t, full.Group.AIRouting, projected.Group.AIRouting, "the auth SQL projection must retain the persisted routing mode")

			server := miniredis.RunT(t)
			redisClient := redis.NewClient(&redis.Options{Addr: server.Addr()})
			t.Cleanup(func() { _ = redisClient.Close() })
			cache := NewAPIKeyCache(redisClient)
			cfg := &config.Config{APIKeyAuth: config.APIKeyAuthCacheConfig{L2TTLSeconds: 60}}
			auth := service.NewAPIKeyService(repo, nil, nil, nil, nil, cache, cfg)
			cold, err := auth.GetByKey(ctx, key.Key)
			require.NoError(t, err)
			require.Equal(t, tc.ai, cold.Group.AIRouting)
			// A separate instance with no repository proves that the serialized L2
			// snapshot, rather than a second SQL lookup, carries the mode.
			cacheOnly := service.NewAPIKeyService(nil, nil, nil, nil, nil, cache, cfg)
			hot, err := cacheOnly.GetByKey(ctx, key.Key)
			require.NoError(t, err)
			require.Equal(t, tc.ai, hot.Group.AIRouting)

			digest := sha256.Sum256([]byte(key.Key))
			cacheKey := fmt.Sprintf("%x", digest)
			legacy, err := cache.GetAuthCache(ctx, cacheKey)
			require.NoError(t, err)
			legacy.Snapshot.Version = 25
			legacy.Snapshot.Group.AIRouting = false
			require.NoError(t, cache.SetAuthCache(ctx, cacheKey, legacy, time.Minute))
			refreshed, err := auth.GetByKey(ctx, key.Key)
			require.NoError(t, err)
			require.Equal(t, tc.ai, refreshed.Group.AIRouting, "v25 snapshots must be reloaded instead of classifying an AI group as static")
			current, err := cache.GetAuthCache(ctx, cacheKey)
			require.NoError(t, err)
			require.Greater(t, current.Snapshot.Version, 25)
			require.Equal(t, tc.ai, current.Snapshot.Group.AIRouting)
		})
	}
}
