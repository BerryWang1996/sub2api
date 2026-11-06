//go:build unit

package handler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func aiWSBillingHolds(t *testing.T) (*handlerInflightCache, *service.InflightReservation, []*service.InflightReservation) {
	t.Helper()
	cache := newHandlerInflightCache(10)
	cfg := &config.Config{}
	cfg.Billing.InflightReservation = config.InflightReservationConfig{Enabled: true, TTLSeconds: 60}
	billing := service.NewBillingCacheService(cache, nil, nil, nil, nil, nil, cfg, nil)
	t.Cleanup(billing.Stop)
	user := &service.User{ID: 4, Balance: 10}
	var holds []*service.InflightReservation
	for range 3 {
		r, err := billing.ReserveInflight(t.Context(), user, nil, nil, 0.2)
		require.NoError(t, err)
		require.NotNil(t, r)
		holds = append(holds, r)
		t.Cleanup(r.HandlerDone)
	}
	return cache, holds[0], holds[1:]
}

func TestAIRoutingWSSteeringHoldsSurviveUntilAsyncBillingFinishes(t *testing.T) {
	cache, base, controls := aiWSBillingHolds(t)
	pool := service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{WorkerCount: 1, QueueSize: 1, TaskTimeout: time.Second, OverflowPolicy: "sync"})
	t.Cleanup(pool.Stop)
	h := &GatewayHandler{usageRecordWorkerPool: pool}
	started, unblock, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	h.submitAIWSUsageRecordTask(service.WithInflightReservation(t.Context(), base), controls, func(context.Context) { close(started); <-unblock; close(done) })
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("billing task did not start")
	}
	base.HandlerDone()
	for _, r := range controls {
		r.HandlerDone()
	}
	require.Equal(t, 3, cache.count(), "all input delta holds transfer to the usage task")
	close(unblock)
	<-done
	require.Eventually(t, func() bool { return cache.count() == 0 }, time.Second, time.Millisecond)
}

func TestAIRoutingWSSteeringHoldsReleaseOnDropAndStoppedFallback(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		t.Run(map[bool]string{false: "drop", true: "stopped"}[stopped], func(t *testing.T) {
			cache, base, controls := aiWSBillingHolds(t)
			pool := service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{WorkerCount: 1, QueueSize: 1, TaskTimeout: time.Second, OverflowPolicy: "drop"})
			t.Cleanup(pool.Stop)
			if stopped {
				pool.Stop()
			} else {
				started, unblock := make(chan struct{}), make(chan struct{})
				t.Cleanup(func() { close(unblock) })
				require.Equal(t, service.UsageRecordSubmitModeEnqueued, pool.Submit(func(context.Context) { close(started); <-unblock }))
				<-started
				require.Equal(t, service.UsageRecordSubmitModeEnqueued, pool.Submit(func(context.Context) { <-unblock }))
			}
			var called atomic.Bool
			h := &GatewayHandler{usageRecordWorkerPool: pool}
			h.submitAIWSUsageRecordTask(service.WithInflightReservation(t.Context(), base), controls, func(context.Context) { called.Store(true) })
			require.Equal(t, stopped, called.Load())
			base.HandlerDone()
			for _, r := range controls {
				r.HandlerDone()
			}
			require.Zero(t, cache.count(), "explicit drop or sync stopped fallback leaves no orphan input holds")
		})
	}
}

func TestAIRoutingWSSteeringCannotAddHoldAfterTurnFinishes(t *testing.T) {
	_, _, controls := aiWSBillingHolds(t)
	state := &aiWSExecutionState{}
	require.True(t, state.holdControlReservation(controls[0]))
	require.Len(t, state.finishControlReservations(), 1)
	require.False(t, state.holdControlReservation(controls[1]))
	require.Empty(t, state.finishControlReservations())
}
