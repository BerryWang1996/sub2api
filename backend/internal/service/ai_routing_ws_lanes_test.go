//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type aiLaneExecutor struct {
	ctx    context.Context
	cancel context.CancelFunc
	run    func(func([]byte) error) (*OpenAIForwardResult, error)
	steer  func(context.Context, []byte) error
	closed atomic.Int32
}

func (e *aiLaneExecutor) Run(emit func([]byte) error) (*OpenAIForwardResult, error) {
	return e.run(emit)
}
func (e *aiLaneExecutor) Steer(ctx context.Context, raw []byte) error {
	if e.steer != nil {
		return e.steer(ctx, raw)
	}
	return errors.New("no control expected")
}
func (e *aiLaneExecutor) Cancel()        { e.cancel() }
func (e *aiLaneExecutor) Close()         { e.closed.Add(1); e.cancel() }
func (e *aiLaneExecutor) ConnID() string { return "real-lease-test" }

func aiLanePlan(ctx context.Context, executor AIRoutingWSTurnExecutor) *AIRoutingWSPlan {
	return &AIRoutingWSPlan{Context: ctx, PublicModel: "public-alias", CandidateID: "candidate", ExecutionModel: "gpt-5.1", AccountID: 901, PolicyVersion: 2, Executor: executor, Revalidate: func(context.Context) error { return nil }}
}

func aiLaneReceive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for lane event")
		var zero T
		return zero
	}
}

func aiLaneCleanup(t *testing.T, lanes *AIRoutingWSLanes) {
	t.Helper()
	t.Cleanup(func() {
		lanes.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.NoError(t, lanes.Wait(ctx))
	})
}

func TestAIRoutingWSLanesSerializePerLaneAndRouteParallelLanes(t *testing.T) {
	started := make(chan AIRoutingWSTurn, 8)
	finished := make(chan AIRoutingWSTurn, 8)
	unblock := make(chan struct{})
	var active, maxActive, planned, released atomic.Int32
	lanes, err := NewAIRoutingWSLanes(t.Context(), AIRoutingWSLaneLimits{MaxLanes: 2, MaxParallel: 2}, AIRoutingWSLaneHooks{
		Plan: func(ctx context.Context, turn AIRoutingWSTurn) (*AIRoutingWSPlan, error) {
			planned.Add(1)
			ctx, cancel := context.WithCancel(ctx)
			executor := &aiLaneExecutor{ctx: ctx, cancel: cancel}
			executor.run = func(func([]byte) error) (*OpenAIForwardResult, error) {
				n := active.Add(1)
				for old := maxActive.Load(); n > old && !maxActive.CompareAndSwap(old, n); old = maxActive.Load() {
				}
				defer active.Add(-1)
				started <- turn
				select {
				case <-unblock:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return &OpenAIForwardResult{RequestID: fmt.Sprint(turn.ID)}, nil
			}
			plan := aiLanePlan(ctx, executor)
			plan.AccountID += int64(turn.ID) // same model can use distinct targets
			plan.PublicModel = "alias-" + turn.StreamID
			plan.Release = func() { released.Add(1) }
			return plan, nil
		},
		Emit:   func(context.Context, AIRoutingWSTurn, *AIRoutingWSPlan, []byte, string) error { return nil },
		Finish: func(turn AIRoutingWSTurn, _ *AIRoutingWSPlan, _ *OpenAIForwardResult, _ error) { finished <- turn },
	})
	require.NoError(t, err)
	aiLaneCleanup(t, lanes)
	first := []byte(`{"type":"response.create","stream_id":"a","model":"alias-a","input":"original"}`)
	_, err = lanes.Submit(t.Context(), first)
	require.NoError(t, err)
	turn1 := aiLaneReceive(t, started)
	copy(first, []byte(`{"corrupted":true}`))
	_, err = lanes.Submit(t.Context(), []byte(`{"type":"response.create","stream_id":"a","input":"second"}`))
	require.NoError(t, err)
	_, err = lanes.Submit(t.Context(), []byte(`{"type":"response.create","stream_id":"b","model":"alias-b"}`))
	require.NoError(t, err)
	turnB := aiLaneReceive(t, started)
	require.Equal(t, "a", turn1.StreamID)
	require.Equal(t, "b", turnB.StreamID)
	require.Empty(t, turnB.PreviousPublicModel)
	require.Equal(t, "original", gjson.GetBytes(turn1.Payload, "input").Str)
	require.Equal(t, int32(2), planned.Load(), "second same-lane turn has not planned or consumed a slot")
	close(unblock)
	turn2 := aiLaneReceive(t, started)
	require.Equal(t, "a", turn2.StreamID)
	require.Equal(t, "alias-a", turn2.PreviousPublicModel)
	for range 3 {
		aiLaneReceive(t, finished)
	}
	require.Equal(t, int32(2), maxActive.Load())
	require.Equal(t, int32(3), released.Load())
}

func TestAIRoutingWSLanesQueuedCancelNeverPlansOrBillsExecution(t *testing.T) {
	started := make(chan struct{}, 1)
	finished := make(chan struct {
		plan   *AIRoutingWSPlan
		result *OpenAIForwardResult
		err    error
	}, 4)
	var plans atomic.Int32
	lanes, err := NewAIRoutingWSLanes(t.Context(), AIRoutingWSLaneLimits{MaxLanes: 1, MaxParallel: 1, MaxQueuedPerLane: 1}, AIRoutingWSLaneHooks{
		Plan: func(ctx context.Context, _ AIRoutingWSTurn) (*AIRoutingWSPlan, error) {
			plans.Add(1)
			ctx, cancel := context.WithCancel(ctx)
			e := &aiLaneExecutor{ctx: ctx, cancel: cancel}
			e.run = func(func([]byte) error) (*OpenAIForwardResult, error) {
				started <- struct{}{}
				<-ctx.Done()
				return &OpenAIForwardResult{Usage: OpenAIUsage{InputTokens: 7}}, ctx.Err()
			}
			return aiLanePlan(ctx, e), nil
		},
		Emit: func(context.Context, AIRoutingWSTurn, *AIRoutingWSPlan, []byte, string) error { return nil },
		Finish: func(_ AIRoutingWSTurn, p *AIRoutingWSPlan, r *OpenAIForwardResult, err error) {
			finished <- struct {
				plan   *AIRoutingWSPlan
				result *OpenAIForwardResult
				err    error
			}{p, r, err}
		},
	})
	require.NoError(t, err)
	aiLaneCleanup(t, lanes)
	first, err := lanes.Submit(t.Context(), []byte(`{"type":"response.create"}`))
	require.NoError(t, err)
	aiLaneReceive(t, started)
	queued, err := lanes.Submit(t.Context(), []byte(`{"type":"response.create"}`))
	require.NoError(t, err)
	_, err = lanes.Submit(t.Context(), []byte(`{"type":"response.create"}`))
	require.ErrorContains(t, err, "queue is full")
	require.True(t, lanes.Cancel(queued))
	require.True(t, lanes.Cancel(first))
	a, b := aiLaneReceive(t, finished), aiLaneReceive(t, finished)
	require.Equal(t, int32(1), plans.Load())
	require.NotNil(t, a.plan)
	require.Equal(t, 7, a.result.Usage.InputTokens)
	require.Nil(t, b.plan)
	require.Nil(t, b.result)
	require.ErrorIs(t, b.err, context.Canceled)
	require.False(t, lanes.Cancel(first))
}

func TestAIRoutingWSLanesOriginBindingPrecedesSteerAndFreshAdmission(t *testing.T) {
	for _, bindFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(bindFailure), func(t *testing.T) {
			binding, allowBinding := make(chan struct{}, 1), make(chan struct{})
			controlDone, finished := make(chan error, 1), make(chan error, 1)
			var controls, validations atomic.Int32
			var revoke atomic.Bool
			lanes, err := NewAIRoutingWSLanes(t.Context(), AIRoutingWSLaneLimits{}, AIRoutingWSLaneHooks{
				Plan: func(ctx context.Context, _ AIRoutingWSTurn) (*AIRoutingWSPlan, error) {
					ctx, cancel := context.WithCancel(ctx)
					e := &aiLaneExecutor{ctx: ctx, cancel: cancel}
					e.run = func(emit func([]byte) error) (*OpenAIForwardResult, error) {
						if err := emit([]byte(`{"type":"response.created","response":{"id":"resp_a"},"sequence_number":9007199254740993}`)); err != nil {
							return nil, err
						}
						<-ctx.Done()
						return nil, ctx.Err()
					}
					e.steer = func(context.Context, []byte) error { controls.Add(1); return nil }
					plan := aiLanePlan(ctx, e)
					plan.Revalidate = func(context.Context) error {
						validations.Add(1)
						if revoke.Load() {
							return errors.New("revoked")
						}
						return nil
					}
					return plan, nil
				},
				Emit: func(_ context.Context, turn AIRoutingWSTurn, _ *AIRoutingWSPlan, event []byte, connID string) error {
					if gjson.GetBytes(event, "stream_id").Str != turn.StreamID || connID != "real-lease-test" || gjson.GetBytes(event, "sequence_number").Raw != "9007199254740993" {
						return errors.New("wrong output identity")
					}
					binding <- struct{}{}
					<-allowBinding
					if bindFailure {
						return errors.New("origin store failed")
					}
					return nil
				},
				Finish: func(AIRoutingWSTurn, *AIRoutingWSPlan, *OpenAIForwardResult, error) { finished <- nil },
			})
			require.NoError(t, err)
			aiLaneCleanup(t, lanes)
			id, err := lanes.Submit(t.Context(), []byte(`{"type":"response.create","stream_id":"alpha"}`))
			require.NoError(t, err)
			aiLaneReceive(t, binding)
			go func() {
				controlDone <- lanes.Steer(t.Context(), []byte(`{"type":"response.steer","previous_response_id":"resp_a","input":"update"}`))
			}()
			require.Zero(t, controls.Load(), "origin has not durably persisted")
			close(allowBinding)
			err = aiLaneReceive(t, controlDone)
			if bindFailure {
				require.Error(t, err)
				require.Zero(t, controls.Load())
				aiLaneReceive(t, finished)
				return
			}
			require.NoError(t, err)
			require.Equal(t, int32(1), controls.Load())
			revoke.Store(true)
			require.ErrorContains(t, lanes.Steer(t.Context(), []byte(`{"type":"response.steer","previous_response_id":"resp_a","input":"again"}`)), "revoked")
			require.Equal(t, int32(1), controls.Load())
			require.Equal(t, int32(3), validations.Load())
			require.Error(t, lanes.Steer(t.Context(), []byte(`{"type":"response.steer","previous_response_id":"resp_other","input":"bad"}`)))
			lanes.Cancel(id)
			aiLaneReceive(t, finished)
		})
	}
}

func TestAIRoutingWSLanesRejectStalePlanAndReleaseOnce(t *testing.T) {
	for _, scenario := range []string{"revoked", "cancel_during_check", "unrelated_context"} {
		t.Run(scenario, func(t *testing.T) {
			finished := make(chan error, 1)
			var executions, releases atomic.Int32
			var executor *aiLaneExecutor
			lanes, err := NewAIRoutingWSLanes(t.Context(), AIRoutingWSLaneLimits{}, AIRoutingWSLaneHooks{
				Plan: func(ctx context.Context, _ AIRoutingWSTurn) (*AIRoutingWSPlan, error) {
					ctx, cancel := context.WithCancel(ctx)
					executor = &aiLaneExecutor{ctx: ctx, cancel: cancel, run: func(func([]byte) error) (*OpenAIForwardResult, error) { executions.Add(1); return nil, nil }}
					plan := aiLanePlan(ctx, executor)
					plan.Release = func() { releases.Add(1) }
					plan.Revalidate = func(context.Context) error {
						if scenario == "revoked" {
							return errors.New("revoked")
						}
						if scenario == "cancel_during_check" {
							cancel()
						}
						return nil
					}
					if scenario == "unrelated_context" {
						plan.Context = context.Background()
					}
					return plan, nil
				},
				Emit:   func(context.Context, AIRoutingWSTurn, *AIRoutingWSPlan, []byte, string) error { return nil },
				Finish: func(_ AIRoutingWSTurn, _ *AIRoutingWSPlan, _ *OpenAIForwardResult, err error) { finished <- err },
			})
			require.NoError(t, err)
			aiLaneCleanup(t, lanes)
			_, err = lanes.Submit(t.Context(), []byte(`{"type":"response.create"}`))
			require.NoError(t, err)
			require.Error(t, aiLaneReceive(t, finished))
			require.Zero(t, executions.Load())
			require.Equal(t, int32(1), releases.Load())
			require.Equal(t, int32(1), executor.closed.Load())
		})
	}
}

func TestAIRoutingWSLanesSlowFinishDoesNotHoldWriteLockAndWaitIsBounded(t *testing.T) {
	blocked, unblock, emitted := make(chan struct{}, 1), make(chan struct{}), make(chan struct{}, 1)
	lanes, err := NewAIRoutingWSLanes(t.Context(), AIRoutingWSLaneLimits{MaxLanes: 2, MaxParallel: 2}, AIRoutingWSLaneHooks{
		Plan: func(ctx context.Context, turn AIRoutingWSTurn) (*AIRoutingWSPlan, error) {
			ctx, cancel := context.WithCancel(ctx)
			e := &aiLaneExecutor{ctx: ctx, cancel: cancel}
			e.run = func(emit func([]byte) error) (*OpenAIForwardResult, error) {
				if turn.StreamID == "b" {
					return nil, emit([]byte(`{"type":"response.completed","response":{"id":"resp_b"}}`))
				}
				return nil, nil
			}
			return aiLanePlan(ctx, e), nil
		},
		Emit: func(context.Context, AIRoutingWSTurn, *AIRoutingWSPlan, []byte, string) error {
			emitted <- struct{}{}
			return nil
		},
		Finish: func(turn AIRoutingWSTurn, _ *AIRoutingWSPlan, _ *OpenAIForwardResult, _ error) {
			if turn.StreamID == "a" {
				blocked <- struct{}{}
				<-unblock
			}
		},
	})
	require.NoError(t, err)
	aiLaneCleanup(t, lanes)
	_, err = lanes.Submit(t.Context(), []byte(`{"type":"response.create","stream_id":"a"}`))
	require.NoError(t, err)
	aiLaneReceive(t, blocked)
	_, err = lanes.Submit(t.Context(), []byte(`{"type":"response.create","stream_id":"b"}`))
	require.NoError(t, err)
	aiLaneReceive(t, emitted)
	lanes.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, lanes.Wait(ctx), context.DeadlineExceeded)
	close(unblock)
}

func TestAIRoutingWSLanesBoundQueuedBytesAndRejectMalformedEvents(t *testing.T) {
	lanes, err := NewAIRoutingWSLanes(t.Context(), AIRoutingWSLaneLimits{MaxBufferedBytes: 8}, AIRoutingWSLaneHooks{
		Plan: func(context.Context, AIRoutingWSTurn) (*AIRoutingWSPlan, error) {
			return nil, errors.New("must not plan")
		},
		Emit:   func(context.Context, AIRoutingWSTurn, *AIRoutingWSPlan, []byte, string) error { return nil },
		Finish: func(AIRoutingWSTurn, *AIRoutingWSPlan, *OpenAIForwardResult, error) {},
	})
	require.NoError(t, err)
	aiLaneCleanup(t, lanes)
	_, err = lanes.Submit(t.Context(), []byte(`{"type":"response.create"}`))
	require.ErrorContains(t, err, "buffered payload limit")
	for _, raw := range []string{`{"type":"response.create","stream_id":"bad lane"}`, `{"type":"response.create","stream_id":""}`, `{"type":"response.create","type":"response.create"}`, `{"type":"response.steer"}`} {
		_, err = lanes.Submit(t.Context(), []byte(raw))
		require.Error(t, err)
	}
}

func TestAIRoutingWSLanesRunsActualNativeAdapter(t *testing.T) {
	native, conn, _ := aiNativeWSTransportFixture(t, nil, aiNativeWSCreate)
	completed := make(chan *OpenAIForwardResult, 1)
	conn.events <- []byte(`{"type":"response.created","response":{"id":"resp_native"}}`)
	conn.events <- []byte(`{"type":"response.completed","response":{"id":"resp_native","usage":{"input_tokens":9,"output_tokens":2}}}`)
	lanes, err := NewAIRoutingWSLanes(native.ctx, AIRoutingWSLaneLimits{}, AIRoutingWSLaneHooks{
		Plan: func(ctx context.Context, _ AIRoutingWSTurn) (*AIRoutingWSPlan, error) {
			// The test fixture pre-acquired a real pool lease. Production Plan
			// constructs this adapter with its freshly authorized turn context.
			native.ctx, native.cancel = context.WithCancel(ctx)
			return aiLanePlan(ctx, native), nil
		},
		Emit: func(_ context.Context, _ AIRoutingWSTurn, _ *AIRoutingWSPlan, event []byte, connID string) error {
			if connID != native.ConnID() || gjson.GetBytes(event, "stream_id").Str != "planner" {
				return errors.New("wrong native lane/lease")
			}
			return nil
		},
		Finish: func(_ AIRoutingWSTurn, _ *AIRoutingWSPlan, result *OpenAIForwardResult, err error) {
			if err != nil {
				completed <- nil
			} else {
				completed <- result
			}
		},
	})
	require.NoError(t, err)
	aiLaneCleanup(t, lanes)
	_, err = lanes.Submit(t.Context(), []byte(aiNativeWSCreate))
	require.NoError(t, err)
	result := aiLaneReceive(t, completed)
	require.NotNil(t, result)
	require.Equal(t, 9, result.Usage.InputTokens)
	require.Len(t, conn.writes, 1)
}
