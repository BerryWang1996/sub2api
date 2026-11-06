//go:build unit

package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAIBackgroundAccountingOwnershipSurvivesActualAsyncUsagePool(t *testing.T) {
	parent, cancel := context.WithCancel(service.WithAIRoutingBackgroundSubmission(t.Context(), func(*http.Request, int64) error {
		t.Error("submission callback must not reach an accounting worker")
		return nil
	}))
	cancel()
	pool := newUsageRecordTestPool(t)
	h := &OpenAIGatewayHandler{usageRecordWorkerPool: pool}
	type observation struct {
		owned, callback, cancelled bool
		err                        error
	}
	done := make(chan observation, 1)
	h.submitOpenAIUsageRecordTask(parent, &service.OpenAIForwardResult{}, func(ctx context.Context) {
		gateway := &service.OpenAIGatewayService{}
		// A non-background call with nil input returns an error. Here the ledger
		// owns all usage, even if the initial response already carried terminal usage.
		done <- observation{service.AIRoutingBackgroundOwnsBilling(ctx), service.HasAIRoutingBackgroundSubmission(ctx), ctx.Err() != nil, gateway.RecordUsage(ctx, nil)}
	})
	select {
	case got := <-done:
		require.True(t, got.owned)
		require.False(t, got.callback)
		require.False(t, got.cancelled)
		require.NoError(t, got.err)
	case <-time.After(3 * time.Second):
		t.Fatal("asynchronous usage task was not executed")
	}
	ordinary := usageRecordContext(t.Context(), context.Background())
	require.False(t, service.AIRoutingBackgroundOwnsBilling(ordinary))
	require.Error(t, (&service.OpenAIGatewayService{}).RecordUsage(ordinary, nil))
}
