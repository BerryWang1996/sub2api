package repository

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestAIRoutingOriginStoreRetentionAndConflict(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store, ok := NewAIRoutingStore(client).(service.AIRoutingOriginStore)
	require.True(t, ok)
	ctx := t.Context()
	key := service.AIRoutingOriginKey(1, 2, 3, "response", "resp_test")
	require.NoError(t, store.SaveOriginBinding(ctx, key, `{"account":1}`, service.AIRoutingResponseOriginTTL))
	require.Equal(t, service.AIRoutingResponseOriginTTL, server.TTL(key))
	rows, err := store.LoadOriginBindings(ctx, []string{key, service.AIRoutingOriginKey(1, 2, 4, "response", "resp_test")})
	require.NoError(t, err)
	require.Equal(t, []string{`{"account":1}`, ""}, rows)
	server.FastForward(time.Hour)
	require.NoError(t, store.SaveOriginBinding(ctx, key, `{"account":1}`, service.AIRoutingResponseOriginTTL))
	require.Equal(t, service.AIRoutingResponseOriginTTL, server.TTL(key))
	require.Error(t, store.SaveOriginBinding(ctx, key, `{"account":2}`, service.AIRoutingToolOriginTTL))
	require.Equal(t, service.AIRoutingResponseOriginTTL, server.TTL(key), "a shorter conflicting lease cannot erase the collision marker early")
	value, err := server.Get(key)
	require.NoError(t, err)
	require.Equal(t, "conflict", value)
	require.Error(t, store.SaveOriginBinding(ctx, key, `{"account":1}`, service.AIRoutingToolOriginTTL))
	toolKey := service.AIRoutingOriginKey(1, 2, 3, "tool_call", "call_test")
	require.NoError(t, store.SaveOriginBinding(ctx, toolKey, `{"account":1}`, service.AIRoutingToolOriginTTL))
	server.FastForward(service.AIRoutingToolOriginTTL + time.Second)
	rows, err = store.LoadOriginBindings(ctx, []string{key, toolKey})
	require.NoError(t, err)
	require.Equal(t, []string{"conflict", ""}, rows)
	server.FastForward(service.AIRoutingResponseOriginTTL)
	rows, err = store.LoadOriginBindings(ctx, []string{key})
	require.NoError(t, err)
	require.Equal(t, []string{""}, rows)
}

func TestAIRoutingOriginStoreConcurrentConflictAndErrors(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store, ok := NewAIRoutingStore(client).(service.AIRoutingOriginStore)
	require.True(t, ok)
	key := service.AIRoutingOriginKey(1, 2, 3, "item", "item_test")
	results := make(chan error, 12)
	for i := range 12 {
		go func() {
			results <- store.SaveOriginBinding(t.Context(), key, fmt.Sprint(i), service.AIRoutingToolOriginTTL)
		}()
	}
	success := 0
	for range 12 {
		if <-results == nil {
			success++
		}
	}
	require.Equal(t, 1, success)
	rows, err := store.LoadOriginBindings(t.Context(), []string{key})
	require.NoError(t, err)
	require.Equal(t, []string{"conflict"}, rows)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, store.SaveOriginBinding(ctx, key, "value", time.Hour), context.Canceled)
	_, err = store.LoadOriginBindings(ctx, []string{key})
	require.ErrorIs(t, err, context.Canceled)
	for _, ttl := range []time.Duration{0, time.Millisecond, 31 * 24 * time.Hour} {
		require.Error(t, store.SaveOriginBinding(t.Context(), key, "value", ttl))
	}
	server.SetError("ERR synthetic unavailable")
	require.Error(t, store.SaveOriginBinding(t.Context(), key, "value", time.Hour))
	_, err = store.LoadOriginBindings(t.Context(), []string{key})
	require.Error(t, err)
}

func TestAIRoutingOriginStoreSharedTombstoneCannotBeResurrected(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewAIRoutingStore(client)
	origins, ok := store.(service.AIRoutingOriginStore)
	require.True(t, ok)
	revoker, ok := store.(service.AIRoutingOriginRevoker)
	require.True(t, ok)
	key := service.AIRoutingOriginKey(1, 2, 3, "response", "resp_A")
	require.NoError(t, origins.SaveOriginBinding(t.Context(), key, "original", service.AIRoutingResponseOriginTTL))
	require.NoError(t, revoker.RevokeOriginBinding(t.Context(), key, service.AIRoutingResponseOriginTTL))
	// A second store instance sees the shared tombstone, not a process-local flag.
	reader, ok := NewAIRoutingStore(client).(service.AIRoutingOriginStore)
	require.True(t, ok)
	rows, err := reader.LoadOriginBindings(t.Context(), []string{key})
	require.NoError(t, err)
	require.Equal(t, []string{"revoked"}, rows)
	require.Error(t, reader.SaveOriginBinding(t.Context(), key, "original", service.AIRoutingToolOriginTTL))
	require.Equal(t, service.AIRoutingResponseOriginTTL, server.TTL(key))
	rows, err = reader.LoadOriginBindings(t.Context(), []string{key})
	require.NoError(t, err)
	require.NotEqual(t, "original", rows[0])
}
