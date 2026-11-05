package repository

import (
	"context"
	"fmt"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestAIRoutingTraceRetentionAndGroupIsolation(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewAIRoutingStore(client)
	ctx := context.Background()
	for i := 0; i < 60; i++ {
		require.NoError(t, service.SaveAIRoutingTrace(ctx, store, 5, service.AIRoutingTrace{RequestID: fmt.Sprint(i), Source: "model"}))
	}
	require.NoError(t, service.SaveAIRoutingTrace(ctx, store, 6, service.AIRoutingTrace{RequestID: "different-group"}))
	require.Equal(t, 30*24*time.Hour, server.TTL(decisionTraceKey(5)))
	rows, err := service.ListAIRoutingDecisions(ctx, store, 5)
	require.NoError(t, err)
	require.Len(t, rows, 50)
	require.Equal(t, "59", rows[0].RequestID)
	require.Equal(t, "10", rows[49].RequestID)
	rows, err = service.ListAIRoutingDecisions(ctx, store, 6)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "different-group", rows[0].RequestID)
	server.FastForward(31 * 24 * time.Hour)
	rows, err = service.ListAIRoutingDecisions(ctx, store, 5)
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestAIRoutingStoreBindingTTLAndConflict(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewAIRoutingStore(client)
	ctx := context.Background()
	key := "airoute:tool:synthetic"
	origin := `{"candidate":"A","account":7,"model":"synthetic-model","platform":"openai"}`
	require.NoError(t, store.SaveToolBinding(ctx, key, origin))
	require.Equal(t, 24*time.Hour, server.TTL(key))
	rows, err := store.LoadToolBindings(ctx, []string{key, "airoute:tool:missing"})
	require.NoError(t, err)
	require.Equal(t, []string{origin, ""}, rows)
	server.FastForward(time.Hour)
	require.NoError(t, store.SaveToolBinding(ctx, key, origin))
	require.Equal(t, 24*time.Hour, server.TTL(key), "same-origin writes refresh the existing lease")
	require.EqualError(t, store.SaveToolBinding(ctx, key, `{"candidate":"B","account":8}`), "tool binding conflicts with another execution")
	value, err := server.Get(key)
	require.NoError(t, err)
	require.Equal(t, "conflict", value)
	server.FastForward(time.Hour)
	require.EqualError(t, store.SaveToolBinding(ctx, key, origin), "tool binding conflicts with another execution")
	require.Equal(t, 24*time.Hour, server.TTL(key), "a collision remains poisoned and renews its expiry")
	server.FastForward(24*time.Hour + time.Second)
	rows, err = store.LoadToolBindings(ctx, []string{key})
	require.NoError(t, err)
	require.Equal(t, []string{""}, rows)
}

func TestAIRoutingStoreBindingConcurrentOriginsCannotOverwrite(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewAIRoutingStore(client)
	const writes = 20
	results := make(chan error, writes)
	for i := range writes {
		go func() {
			results <- store.SaveToolBinding(context.Background(), "airoute:tool:concurrent", fmt.Sprintf(`{"account":%d}`, i+1))
		}()
	}
	successes := 0
	for range writes {
		if err := <-results; err == nil {
			successes++
		} else {
			require.EqualError(t, err, "tool binding conflicts with another execution")
		}
	}
	require.Equal(t, 1, successes)
	rows, err := store.LoadToolBindings(context.Background(), []string{"airoute:tool:concurrent"})
	require.NoError(t, err)
	require.Equal(t, []string{"conflict"}, rows)
}

func TestAIRoutingStoreTraceCorrelationAndMalformedHistory(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewAIRoutingStore(client)
	ctx := context.Background()
	trace := service.AIRoutingTrace{RequestID: "operation-id", ClientRequestID: "client-id", UsageRequestID: "client:client-id", PolicyVersion: 3}
	require.NoError(t, store.SaveTrace(ctx, 5, trace))
	server.FastForward(time.Hour)
	require.NoError(t, store.SaveTrace(ctx, 5, trace))
	require.Equal(t, 30*24*time.Hour, server.TTL(decisionTraceKey(5)))
	rows, err := store.ListTraces(ctx, 5)
	require.NoError(t, err)
	require.Equal(t, []service.AIRoutingTrace{trace, trace}, rows)
	require.NoError(t, client.LPush(ctx, decisionTraceKey(5), "invalid-json").Err())
	rows, err = store.ListTraces(ctx, 5)
	require.Error(t, err)
	require.Nil(t, rows, "malformed history retains the original all-or-error behavior")
}

func TestAIRoutingStoreErrorsAndCancellation(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewAIRoutingStore(client)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, store.SaveToolBinding(ctx, "key", "value"), context.Canceled)
	_, err := store.LoadToolBindings(ctx, []string{"key"})
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, store.SaveTrace(ctx, 5, service.AIRoutingTrace{}), context.Canceled)
	_, err = store.ListTraces(ctx, 5)
	require.ErrorIs(t, err, context.Canceled)
	server.SetError("ERR synthetic unavailable")
	ctx = context.Background()
	require.EqualError(t, store.SaveToolBinding(ctx, "key", "value"), "persist tool binding: ERR synthetic unavailable")
	_, err = store.LoadToolBindings(ctx, []string{"key"})
	require.EqualError(t, err, "ERR synthetic unavailable")
	require.EqualError(t, store.SaveTrace(ctx, 5, service.AIRoutingTrace{}), "ERR synthetic unavailable")
	_, err = store.ListTraces(ctx, 5)
	require.EqualError(t, err, "ERR synthetic unavailable")
	require.Nil(t, NewAIRoutingStore(nil))
}
