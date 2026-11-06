package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/tidwall/gjson"
)

// AIRoutingWSTurn is one client create, copied before it enters a bounded lane.
// PreviousPublicModel is only a lane-local alias fallback, never conversation
// history or authorization. Every create still needs a fresh Plan.
type AIRoutingWSTurn struct {
	ID                  uint64
	StreamID            string
	Payload             []byte
	PreviousPublicModel string
}

type AIRoutingWSTurnExecutor interface {
	Run(func([]byte) error) (*OpenAIForwardResult, error)
	Steer(context.Context, []byte) error
	Cancel()
	Close()
	ConnID() string
}

type AIRoutingWSPlan struct {
	Context        context.Context
	PublicModel    string
	CandidateID    string
	ExecutionModel string
	AccountID      int64
	PolicyVersion  int64
	Executor       AIRoutingWSTurnExecutor
	Revalidate     func(context.Context) error
	Release        func()
}

type AIRoutingWSLaneLimits struct {
	MaxLanes         int
	MaxParallel      int
	MaxQueuedPerLane int
	MaxBufferedBytes int
}

type aiWSPlanContextKey struct{}

type AIRoutingWSLaneHooks struct {
	Plan func(context.Context, AIRoutingWSTurn) (*AIRoutingWSPlan, error)
	// BeforeSteer validates the current durable response origin and audits the
	// new user input without mutating the running turn's request context.
	BeforeSteer func(context.Context, AIRoutingWSTurn, *AIRoutingWSPlan, []byte) error
	// Emit must persist origin bindings before exposing IDs to the client.
	// ConnID is the actual upstream lease, not a client connection identifier.
	Emit   func(context.Context, AIRoutingWSTurn, *AIRoutingWSPlan, []byte, string) error
	Finish func(AIRoutingWSTurn, *AIRoutingWSPlan, *OpenAIForwardResult, error)
}

type aiWSLaneTask struct {
	turn   AIRoutingWSTurn
	ctx    context.Context
	cancel context.CancelFunc
	stop   func() bool
	plan   *AIRoutingWSPlan // protected by the dispatcher mutex
	ended  bool
}

type aiWSLane struct {
	queue         chan *aiWSLaneTask
	previousModel string // used only by this lane's worker
}

type aiWSOrigin struct {
	task  *aiWSLaneTask
	ready chan struct{}
	err   error // immutable after ready is closed
}

// AIRoutingWSLanes multiplexes independently admitted creates. It does not
// select accounts, cache authorization, retry failed turns, or infer tool IDs.
type AIRoutingWSLanes struct {
	ctx      context.Context
	cancel   context.CancelFunc
	limits   AIRoutingWSLaneLimits
	hooks    AIRoutingWSLaneHooks
	parallel chan struct{}
	mu       sync.Mutex
	writeMu  sync.Mutex
	lanes    map[string]*aiWSLane
	tasks    map[uint64]*aiWSLaneTask
	origins  map[string]*aiWSOrigin
	nextID   uint64
	buffered int
	closed   bool
	workers  sync.WaitGroup
	done     chan struct{}
}

func NewAIRoutingWSLanes(ctx context.Context, limits AIRoutingWSLaneLimits, hooks AIRoutingWSLaneHooks) (*AIRoutingWSLanes, error) {
	if ctx == nil || hooks.Plan == nil || hooks.Emit == nil || hooks.Finish == nil {
		return nil, errors.New("websocket lanes require a context and all lifecycle hooks")
	}
	if limits.MaxLanes == 0 {
		limits.MaxLanes = 8
	}
	if limits.MaxParallel == 0 {
		limits.MaxParallel = 4
	}
	if limits.MaxQueuedPerLane == 0 {
		limits.MaxQueuedPerLane = 8
	}
	if limits.MaxBufferedBytes == 0 {
		limits.MaxBufferedBytes = 32 << 20
	}
	if limits.MaxLanes < 1 || limits.MaxLanes > 64 || limits.MaxParallel < 1 || limits.MaxParallel > limits.MaxLanes || limits.MaxQueuedPerLane < 1 || limits.MaxQueuedPerLane > 64 || limits.MaxBufferedBytes < 1 || limits.MaxBufferedBytes > 64<<20 {
		return nil, errors.New("invalid websocket lane limits")
	}
	ctx, cancel := context.WithCancel(ctx)
	d := &AIRoutingWSLanes{ctx: ctx, cancel: cancel, limits: limits, hooks: hooks, parallel: make(chan struct{}, limits.MaxParallel), lanes: make(map[string]*aiWSLane), tasks: make(map[uint64]*aiWSLaneTask), origins: make(map[string]*aiWSOrigin), done: make(chan struct{})}
	go func() { <-ctx.Done(); d.Close() }()
	return d, nil
}

func (d *AIRoutingWSLanes) Submit(ctx context.Context, payload []byte) (uint64, error) {
	if ctx == nil || ctx.Err() != nil {
		return 0, errors.New("websocket create context is unavailable")
	}
	if len(payload) > int(aiRoutingFinalPayloadMaxBytes) || !gjson.ValidBytes(payload) || !requestmodel.HasUniqueJSONMembers(payload) {
		return 0, errors.New("invalid websocket create payload")
	}
	root := gjson.ParseBytes(payload)
	if !root.IsObject() || !aiCanonicalFields(root, "type", "stream_id", "model", "input", "previous_response_id") || root.Get("type").Str != "response.create" {
		return 0, errors.New("websocket lane accepts response.create only")
	}
	laneID := root.Get("stream_id")
	if laneID.Exists() && (laneID.Type != gjson.String || !aiRoutingWSValidLane(laneID.Str)) {
		return 0, errors.New("invalid websocket stream_id")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.ctx.Err() != nil {
		return 0, errors.New("websocket lanes are closed")
	}
	if len(payload) > d.limits.MaxBufferedBytes-d.buffered {
		return 0, errors.New("websocket buffered payload limit reached")
	}
	lane := d.lanes[laneID.Str]
	if lane == nil {
		if len(d.lanes) >= d.limits.MaxLanes {
			return 0, errors.New("websocket lane limit reached")
		}
		lane = &aiWSLane{queue: make(chan *aiWSLaneTask, d.limits.MaxQueuedPerLane)}
		d.lanes[laneID.Str] = lane
		d.workers.Add(1)
		go d.runLane(lane)
	}
	if len(lane.queue) == cap(lane.queue) {
		return 0, errors.New("websocket lane queue is full")
	}
	d.nextID++
	turnCtx, cancel := context.WithCancel(d.ctx)
	task := &aiWSLaneTask{turn: AIRoutingWSTurn{ID: d.nextID, StreamID: laneID.Str, Payload: append([]byte(nil), payload...)}, ctx: turnCtx, cancel: cancel}
	task.ctx = context.WithValue(turnCtx, aiWSPlanContextKey{}, task)
	task.stop = context.AfterFunc(ctx, cancel)
	d.tasks[task.turn.ID] = task
	d.buffered += len(payload)
	lane.queue <- task
	return task.turn.ID, nil
}

func (d *AIRoutingWSLanes) runLane(lane *aiWSLane) {
	defer d.workers.Done()
	for {
		select {
		case <-d.ctx.Done():
			for {
				select {
				case task := <-lane.queue:
					d.finish(task, nil, nil, task.ctx.Err())
				default:
					return
				}
			}
		case task := <-lane.queue:
			task.turn.PreviousPublicModel = lane.previousModel
			d.runTask(task, lane)
		}
	}
}

func (d *AIRoutingWSLanes) runTask(task *aiWSLaneTask, lane *aiWSLane) {
	if err := task.ctx.Err(); err != nil {
		d.finish(task, nil, nil, err)
		return
	}
	select {
	case <-task.ctx.Done():
		d.finish(task, nil, nil, task.ctx.Err())
		return
	case d.parallel <- struct{}{}:
	}
	var result *OpenAIForwardResult
	plan, err := d.hooks.Plan(task.ctx, task.turn)
	if err == nil && (plan == nil || plan.Context == nil || plan.Context.Value(aiWSPlanContextKey{}) != task || plan.Executor == nil || plan.Revalidate == nil || plan.PublicModel == "" || plan.CandidateID == "" || plan.ExecutionModel == "" || plan.AccountID <= 0 || plan.PolicyVersion <= 0) {
		err = errors.New("invalid websocket execution plan")
	}
	if err == nil {
		err = task.ctx.Err()
	}
	if err == nil {
		err = plan.Context.Err()
	}
	if err == nil {
		err = plan.Revalidate(plan.Context)
	}
	if err == nil {
		err = task.ctx.Err()
	}
	if err == nil {
		err = plan.Context.Err()
	}
	if err == nil {
		d.mu.Lock()
		task.plan = plan
		d.mu.Unlock()
		lane.previousModel = plan.PublicModel
		stop := context.AfterFunc(task.ctx, plan.Executor.Cancel)
		result, err = plan.Executor.Run(func(event []byte) error { return d.emit(task, plan, event) })
		stop()
	}
	<-d.parallel
	d.finish(task, plan, result, err)
}

func (d *AIRoutingWSLanes) emit(task *aiWSLaneTask, plan *AIRoutingWSPlan, event []byte) error {
	if err := task.ctx.Err(); err != nil {
		return err
	}
	if err := plan.Context.Err(); err != nil {
		return err
	}
	if !gjson.ValidBytes(event) || !requestmodel.HasUniqueJSONMembers(event) {
		return errors.New("invalid websocket output event")
	}
	root := gjson.ParseBytes(event)
	lane := root.Get("stream_id")
	if lane.Exists() && (lane.Type != gjson.String || lane.Str != task.turn.StreamID) {
		return errors.New("websocket event lane mismatch")
	}
	// Each executor owns a separate native lease. Some compatible providers omit
	// stream_id; attach the exact client lane, never infer a different owner.
	if !lane.Exists() && task.turn.StreamID != "" {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(event, &fields); err != nil {
			return err
		}
		fields["stream_id"], _ = json.Marshal(task.turn.StreamID)
		var err error
		event, err = json.Marshal(fields)
		if err != nil {
			return err
		}
	}
	var origin *aiWSOrigin
	if root.Get("type").Str == "response.created" {
		id := root.Get("response.id").Str
		if !aiResponsesValidID(id) {
			return errors.New("websocket created event has invalid response identity")
		}
		d.mu.Lock()
		owned := 0
		for _, prior := range d.origins {
			if prior.task == task {
				owned++
			}
		}
		if owned >= 128 {
			d.mu.Unlock()
			return errors.New("websocket turn response limit reached")
		}
		if existing := d.origins[id]; existing != nil {
			d.mu.Unlock()
			return errors.New("websocket response identity already exposed")
		}
		origin = &aiWSOrigin{task: task, ready: make(chan struct{})}
		d.origins[id] = origin
		d.mu.Unlock()
	}
	d.writeMu.Lock()
	err := task.ctx.Err()
	if err == nil {
		err = plan.Context.Err()
	}
	if err == nil {
		err = d.hooks.Emit(plan.Context, task.turn, plan, append([]byte(nil), event...), plan.Executor.ConnID())
	}
	d.writeMu.Unlock()
	if origin != nil {
		origin.err = err
		close(origin.ready)
	}
	return err
}

func (d *AIRoutingWSLanes) finish(task *aiWSLaneTask, plan *AIRoutingWSPlan, result *OpenAIForwardResult, err error) {
	d.mu.Lock()
	if task.ended {
		d.mu.Unlock()
		return
	}
	task.ended = true
	d.buffered -= len(task.turn.Payload)
	delete(d.tasks, task.turn.ID)
	for id, origin := range d.origins {
		if origin.task == task {
			delete(d.origins, id)
		}
	}
	d.mu.Unlock()
	task.stop()
	task.cancel()
	if plan != nil {
		if plan.Executor != nil {
			plan.Executor.Close()
		}
		if plan.Release != nil {
			plan.Release()
		}
	}
	// Billing may block. No dispatcher, client-write, or upstream-write lock is
	// held here, so another lane and bounded Wait remain independent.
	d.hooks.Finish(task.turn, plan, result, err)
}

// Steer targets a response already durably emitted by this live turn. It does
// not perform a new selection and cannot mutate a queued or completed turn.
func (d *AIRoutingWSLanes) Steer(ctx context.Context, payload []byte) error {
	if ctx == nil || len(payload) > 128<<10 || !gjson.ValidBytes(payload) || !requestmodel.HasUniqueJSONMembers(payload) {
		return errors.New("invalid websocket steering payload")
	}
	root := gjson.ParseBytes(payload)
	if root.Get("type").Str != "response.steer" || !aiCanonicalFields(root, "type", "previous_response_id", "input") || !aiResponsesValidID(root.Get("previous_response_id").Str) {
		return errors.New("invalid websocket steering payload")
	}
	d.mu.Lock()
	origin := d.origins[root.Get("previous_response_id").Str]
	d.mu.Unlock()
	if origin == nil {
		return errors.New("websocket steering origin is not active")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-origin.task.ctx.Done():
		return origin.task.ctx.Err()
	case <-origin.ready:
	}
	if origin.err != nil {
		return origin.err
	}
	d.mu.Lock()
	plan, ended := origin.task.plan, origin.task.ended
	d.mu.Unlock()
	if ended || plan == nil {
		return errors.New("websocket steering origin is not active")
	}
	controlCtx, cancel := context.WithCancel(plan.Context)
	stop := context.AfterFunc(ctx, cancel)
	defer func() { stop(); cancel() }()
	if err := plan.Revalidate(controlCtx); err != nil {
		return err
	}
	if err := controlCtx.Err(); err != nil {
		return err
	}
	if err := origin.task.ctx.Err(); err != nil {
		return err
	}
	if d.hooks.BeforeSteer != nil {
		if err := d.hooks.BeforeSteer(controlCtx, origin.task.turn, plan, append([]byte(nil), payload...)); err != nil {
			return err
		}
	}
	if err := controlCtx.Err(); err != nil {
		return err
	}
	return plan.Executor.Steer(controlCtx, append([]byte(nil), payload...))
}

func (d *AIRoutingWSLanes) Cancel(turnID uint64) bool {
	d.mu.Lock()
	task := d.tasks[turnID]
	var executor AIRoutingWSTurnExecutor
	if task != nil && task.plan != nil {
		executor = task.plan.Executor
	}
	d.mu.Unlock()
	if task == nil {
		return false
	}
	task.cancel()
	if executor != nil {
		executor.Cancel()
	}
	return true
}

func (d *AIRoutingWSLanes) Close() {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.closed = true
	d.cancel()
	d.mu.Unlock()
	go func() { d.workers.Wait(); close(d.done) }()
}

// Wait is bounded by the caller even if a supplied Finish hook is slow.
func (d *AIRoutingWSLanes) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-d.done:
		return nil
	}
}
