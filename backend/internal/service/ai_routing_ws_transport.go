package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// AIRoutingWSNativeTurnOptions binds one immutable execution plan to one native
// upstream lease. The handler supplies fresh authorization and durable origin
// checks; it must bind client-visible IDs inside emit before writing the event.
type AIRoutingWSNativeTurnOptions struct {
	PublicModel        string
	PreferredConnID    string
	ForcePreferredConn bool
	Revalidate         func(context.Context) error
	DrainTimeout       time.Duration
	CheckSteer         func(context.Context, AIRoutingWSSteeringBudget) error
	ReserveSteer       func(context.Context, AIRoutingWSSteeringBudget) (func(), error)
}

// AIRoutingWSNativeTurn reuses the ordinary OpenAI WS pool and wire helpers.
// It deliberately does not invoke an HTTP handler or the connection-scoped
// ingress loop, whose fixed account and first-terminal exit cannot route lanes.
type AIRoutingWSNativeTurn struct {
	service          *OpenAIGatewayService
	account          *Account
	gin              *gin.Context
	lease            *openAIWSConnLease
	ctx              context.Context
	cancel           context.CancelFunc
	options          AIRoutingWSNativeTurnOptions
	payload          []byte
	model            string
	request          map[string]any
	started          atomic.Bool
	closed           atomic.Bool
	close            sync.Once
	leaseMu          sync.Mutex
	controlMu        sync.Mutex
	mu               sync.Mutex
	ids              map[string]bool
	steers           int
	totalSteers      int
	steerInputBytes  int64
	outputEventBytes int64
}

func aiRoutingWSValidLane(lane string) bool {
	if len(lane) == 0 || len(lane) > 256 {
		return false
	}
	for _, char := range lane {
		if char != '_' && char != '-' && char != '.' && (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') {
			return false
		}
	}
	return true
}

func (s *OpenAIGatewayService) NewAIRoutingWSNativeTurn(ctx context.Context, c *gin.Context, account *Account, body []byte, options AIRoutingWSNativeTurnOptions) (*AIRoutingWSNativeTurn, error) {
	if s == nil || c == nil || account == nil || options.Revalidate == nil || !HasAIRoutingOutboundGuard(ctx) {
		return nil, errors.New("AI websocket turn requires an execution plan and final guard")
	}
	if len(body) > int(aiRoutingFinalPayloadMaxBytes) || !gjson.ValidBytes(body) || !requestmodel.HasUniqueJSONMembers(body) {
		return nil, errors.New("invalid websocket create payload")
	}
	root := gjson.ParseBytes(body)
	if !aiCanonicalFields(root, "type", "model", "stream_id", "generate", "input", "previous_response_id", "store", "background") || root.Get("type").Str != "response.create" || root.Get("model").Type != gjson.String || strings.TrimSpace(root.Get("model").Str) == "" {
		return nil, errors.New("invalid websocket create payload")
	}
	if lane := root.Get("stream_id"); lane.Exists() && (lane.Type != gjson.String || !aiRoutingWSValidLane(lane.Str)) {
		return nil, errors.New("invalid websocket stream_id")
	}
	for _, field := range []string{"generate", "store"} {
		if value := root.Get(field); value.Exists() && value.Type != gjson.True && value.Type != gjson.False {
			return nil, errors.New("invalid websocket create flag")
		}
	}
	if root.Get("background").Exists() {
		return nil, errors.New("background is not supported by websocket create")
	}
	if options.ForcePreferredConn && options.PreferredConnID == "" {
		return nil, errors.New("required websocket continuation lease is unavailable")
	}
	if err := options.Revalidate(ctx); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	decision := s.getOpenAIWSProtocolResolver().Resolve(account)
	if decision.Transport != OpenAIUpstreamTransportResponsesWebsocketV2 {
		return nil, errors.New("selected account does not support native Responses websocket transport")
	}
	if err := s.prepareAIWSNativePayloadIdentity(ctx, c, account); err != nil {
		return nil, err
	}
	setCodexToolNameReverse(c, nil)
	if normalized, _, normalizeErr := normalizeOpenAIResponsesWebSocketCompatibilityBodyWithConfig(s.cfg, body, account, isOpenAIResponsesLiteWebSocketPayload(body)); normalizeErr != nil {
		return nil, normalizeErr
	} else {
		body = normalized
	}
	if account.IsOpenAIOAuthLike() {
		aliased, reverse, changed, aliasErr := aliasOpenAIOAuthReservedToolNamesBody(body)
		if aliasErr != nil {
			return nil, aliasErr
		}
		updateCodexToolNameReverseForWSFrame(c, body, reverse)
		if changed {
			body = aliased
		}
	}
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	wsURL, err := s.buildOpenAIResponsesWSURL(account)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var request map[string]any
	if err = decoder.Decode(&request); err != nil {
		return nil, errors.New("invalid websocket create payload")
	}
	payload := s.buildOpenAIWSCreatePayload(request, account)
	wire, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if (!root.Get("store").Exists() || root.Get("store").Bool()) && gjson.GetBytes(wire, "store").Exists() && !gjson.GetBytes(wire, "store").Bool() {
		return nil, errors.New("selected websocket transport cannot preserve response storage")
	}
	if err = aiRoutingWSCheckFinalRequest(ctx, account.ID, wire); err != nil {
		return nil, err
	}
	headers, _, err := s.buildOpenAIWSHeaders(ctx, c, account, token, decision, false, c.GetHeader(openAIWSTurnStateHeader), c.GetHeader(openAIWSTurnMetadataHeader), root.Get("prompt_cache_key").Str, root.Get("model").Str, root.Get("service_tier").Str)
	if err != nil {
		return nil, err
	}
	proxy := ""
	if account.Proxy != nil && account.ProxyID != nil {
		proxy = account.Proxy.URL()
	}
	acquireCtx, releaseAcquire := context.WithTimeout(ctx, s.openAIWSAcquireTimeout())
	lease, err := s.getOpenAIWSConnPool().Acquire(acquireCtx, openAIWSAcquireRequest{
		Account: account, WSURL: wsURL, Headers: headers, ProxyURL: proxy,
		PreferredConnID: options.PreferredConnID, ForcePreferredConn: options.ForcePreferredConn,
		HeadersFactory: func(factoryCtx context.Context, values http.Header) (http.Header, error) {
			return s.refreshOpenAIAgentIdentityHeaders(factoryCtx, account, values)
		},
	})
	releaseAcquire()
	if err != nil {
		return nil, err
	}
	if options.DrainTimeout <= 0 || options.DrainTimeout > 5*time.Second {
		options.DrainTimeout = 2 * time.Second
	}
	if options.PublicModel == "" {
		options.PublicModel = root.Get("model").Str
	}
	turnCtx, cancel := context.WithCancel(ctx)
	return &AIRoutingWSNativeTurn{service: s, account: account, gin: c, lease: lease, ctx: turnCtx, cancel: cancel, options: options, payload: wire, model: root.Get("model").Str, request: request, ids: make(map[string]bool)}, nil
}

func (s *OpenAIGatewayService) prepareAIWSNativePayloadIdentity(ctx context.Context, c *gin.Context, account *Account) error {
	_, err := s.prepareCodexAccountIdentitySource(ctx, c, account)
	return err
}

func aiRoutingWSCheckFinalRequest(ctx context.Context, accountID int64, payload []byte) error {
	// The shared final guard validates Responses' JSON schema and the chosen
	// model, without sending an HTTP request or consuming the websocket bytes.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://ai-routing.invalid/v1/responses", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer func() { _ = req.Body.Close() }()
	return CheckAIRoutingOutboundRequest(req, accountID)
}

func (turn *AIRoutingWSNativeTurn) ConnID() string { return turn.lease.ConnID() }

// Cancel stops local execution and enables a bounded read drain for usage.
// It does not invent an undocumented response.cancel frame or assert that the
// provider acknowledged cancellation of generation or already-running tools.
func (turn *AIRoutingWSNativeTurn) Cancel() { turn.cancel() }

// Close is the final resource owner cleanup. Never return an unconsumed or
// failed stream to the pool; a running Run normally owns this cleanup itself.
func (turn *AIRoutingWSNativeTurn) Close() {
	turn.cancel()
	turn.leaseMu.Lock()
	defer turn.leaseMu.Unlock()
	turn.close.Do(func() {
		turn.closed.Store(true)
		turn.cancel()
		turn.lease.MarkBroken()
		turn.lease.Release()
	})
}

// Steer sends the exact supported control event on the running lease after
// fresh authorization. A response ID from another lane/turn is never accepted.
func (turn *AIRoutingWSNativeTurn) Steer(ctx context.Context, body []byte) error {
	if ctx == nil {
		return errors.New("websocket steering context unavailable")
	}
	controlCtx, cancel := context.WithCancel(turn.ctx)
	stop := context.AfterFunc(ctx, cancel)
	defer func() { stop(); cancel() }()
	ctx = controlCtx // Preserve this turn's identity, capacity and pricing context.
	if turn.closed.Load() || !turn.started.Load() || turn.ctx.Err() != nil || len(body) > 128<<10 || !gjson.ValidBytes(body) || !requestmodel.HasUniqueJSONMembers(body) {
		return errors.New("websocket steering is unavailable")
	}
	root := gjson.ParseBytes(body)
	valid := root.IsObject()
	root.ForEach(func(key, _ gjson.Result) bool {
		valid = valid && (key.Str == "type" || key.Str == "previous_response_id" || key.Str == "input")
		return valid
	})
	id := root.Get("previous_response_id")
	input := root.Get("input")
	if !valid || root.Get("type").Str != "response.steer" || id.Type != gjson.String || !aiResponsesValidID(id.Str) || input.Type != gjson.String && !input.IsArray() {
		return errors.New("invalid websocket steering payload")
	}
	if input.IsArray() {
		if len(input.Array()) == 0 {
			return errors.New("empty websocket steering input")
		}
		for _, item := range input.Array() {
			if item.Get("role").Str != "user" || item.Get("type").Exists() && item.Get("type").Str != "message" {
				return errors.New("websocket steering requires user messages")
			}
		}
	}
	projection, marshalErr := json.Marshal(map[string]any{"model": turn.model, "input": json.RawMessage(input.Raw), "store": false})
	if marshalErr != nil {
		return errors.New("invalid websocket steering input")
	}
	if _, parseErr := NewAIRoutingResponsesRequest(projection, "metadata"); parseErr != nil {
		return errors.New("unsupported websocket steering input")
	}
	turn.mu.Lock()
	owned := turn.ids[id.Str]
	turn.mu.Unlock()
	if !owned {
		return errors.New("websocket steering origin is not owned by this turn")
	}
	turn.controlMu.Lock()
	defer turn.controlMu.Unlock()
	if turn.options.CheckSteer == nil || turn.options.ReserveSteer == nil || gjson.GetBytes(turn.payload, "generate").Type == gjson.False {
		return errors.New("websocket steering admission unavailable")
	}
	budget := func() AIRoutingWSSteeringBudget {
		turn.mu.Lock()
		defer turn.mu.Unlock()
		return AIRoutingWSSteeringBudget{PriorInputBytes: int64(len(turn.payload)) + turn.steerInputBytes, AdditionalInputBytes: int64(len(body)), OutputEventBytes: turn.outputEventBytes}
	}
	turn.mu.Lock()
	exhausted := turn.totalSteers >= 128
	turn.mu.Unlock()
	if exhausted {
		return errors.New("websocket steering count limit reached")
	}
	if err := turn.options.CheckSteer(ctx, budget()); err != nil {
		return err
	}
	abort, err := turn.options.ReserveSteer(ctx, budget())
	if err != nil {
		return err
	}
	sent := false
	defer func() {
		if !sent && abort != nil {
			abort()
		}
	}()
	if err := turn.options.Revalidate(ctx); err != nil {
		return err
	}
	if err := turn.options.CheckSteer(ctx, budget()); err != nil {
		return err
	}
	if err := turn.ctx.Err(); err != nil {
		return err
	}
	turn.leaseMu.Lock()
	defer turn.leaseMu.Unlock()
	if turn.closed.Load() || turn.ctx.Err() != nil {
		return errors.New("websocket steering is unavailable")
	}
	turn.mu.Lock()
	turn.steers++
	turn.totalSteers++
	turn.steerInputBytes += int64(len(body))
	turn.mu.Unlock()
	sent = true // A write failure can still have delivered the frame; hold until settlement.
	if err := turn.lease.WriteJSONWithContextTimeout(ctx, json.RawMessage(append([]byte(nil), body...)), turn.service.openAIWSWriteTimeout()); err != nil {
		turn.mu.Lock()
		turn.steers--
		turn.mu.Unlock()
		return err
	}
	return nil
}

// Run forwards actual upstream JSON events. A steered terminal does not end the
// lease before its successor; per-response usage snapshots are summed once.
func (turn *AIRoutingWSNativeTurn) Run(emit func([]byte) error) (result *OpenAIForwardResult, err error) {
	if emit == nil || turn.closed.Load() || !turn.started.CompareAndSwap(false, true) {
		return nil, errors.New("websocket turn already used or emitter unavailable")
	}
	start := time.Now()
	result = &OpenAIForwardResult{Model: turn.options.PublicModel, UpstreamModel: turn.model, BillingModel: turn.model, Stream: true, OpenAIWSMode: true, ResponseHeaders: turn.lease.HandshakeHeaders()}
	observer := &upstreamResponseModelObserver{}
	imageCounter := newOpenAIImageOutputCounter()
	usages := make(map[string]OpenAIUsage)
	clean := false
	defer func() {
		result.Duration = time.Since(start)
		for _, usage := range usages {
			addOpenAIUsage(&result.Usage, usage)
			result.Usage.ImageCacheReadTokens += usage.ImageCacheReadTokens
		}
		result.UpstreamResponseModel, result.UpstreamResponseModelConflict = observer.Model(), observer.Conflict()
		result.UpstreamResponseServiceTier = observer.ServiceTier()
		result.ServiceTier = resolvedOpenAIUpstreamServiceTierFromObserver(observer, extractOpenAIServiceTier(turn.request))
		result.ReasoningEffort = extractOpenAIReasoningEffort(turn.request, turn.model, turn.options.PublicModel)
		result.ImageCount, result.ImageOutputSizes = imageCounter.Count(), imageCounter.Sizes()
		turn.leaseMu.Lock()
		defer turn.leaseMu.Unlock()
		turn.close.Do(func() {
			turn.closed.Store(true)
			turn.cancel()
			if !clean {
				turn.lease.MarkBroken()
			}
			turn.lease.Release()
		})
	}()
	if err = turn.options.Revalidate(turn.ctx); err != nil {
		return result, err
	}
	if err = aiRoutingWSCheckFinalRequest(turn.ctx, turn.account.ID, turn.payload); err != nil {
		return result, err
	}
	if err = turn.ctx.Err(); err != nil {
		return result, err
	}
	turn.leaseMu.Lock()
	if turn.closed.Load() {
		err = errors.New("websocket turn is closed")
	} else {
		err = turn.lease.WriteJSONWithContextTimeout(turn.ctx, json.RawMessage(turn.payload), turn.service.openAIWSWriteTimeout())
	}
	turn.leaseMu.Unlock()
	if err != nil {
		return result, err
	}
	readCtx := turn.ctx
	var drainCancel context.CancelFunc
	var pending [][]byte
	var deliveryErr error
	lastTerminal, lastResponseID := "", ""
	for {
		if turn.ctx.Err() != nil && drainCancel == nil {
			result.ClientDisconnect = true
			readCtx, drainCancel = context.WithTimeout(context.WithoutCancel(turn.ctx), turn.options.DrainTimeout)
			defer drainCancel()
		}
		var message []byte
		if len(pending) > 0 {
			message, pending = pending[0], pending[1:]
		} else {
			message, err = turn.lease.ReadMessageWithContextTimeout(readCtx, turn.service.openAIWSReadTimeout())
			if err != nil {
				if turn.ctx.Err() != nil && drainCancel == nil {
					continue
				}
				if deliveryErr != nil {
					return result, deliveryErr
				}
				return result, err
			}
			if documents, split := splitOpenAIConcatenatedJSONDocuments(message); split {
				message, pending = documents[0], documents[1:]
			}
		}
		if !gjson.ValidBytes(message) || !requestmodel.HasUniqueJSONMembers(message) {
			return result, errors.New("invalid upstream websocket event")
		}
		turn.mu.Lock()
		if turn.outputEventBytes <= int64(^uint64(0)>>1)-int64(len(message)) {
			turn.outputEventBytes += int64(len(message))
		} else {
			turn.outputEventBytes = int64(^uint64(0) >> 1)
		}
		turn.mu.Unlock()
		if lane := gjson.GetBytes(message, "stream_id"); lane.Exists() && (lane.Type != gjson.String || lane.Str != gjson.GetBytes(turn.payload, "stream_id").Str) {
			return result, errors.New("upstream websocket event belongs to another lane")
		}
		eventType, responseID, _ := parseOpenAIWSEventEnvelope(message)
		observer.ObserveOpenAI(message, eventType)
		imageCounter.AddSSEData(message)
		if eventType == "response.created" && responseID != "" {
			turn.mu.Lock()
			if len(turn.ids) >= 128 && !turn.ids[responseID] {
				turn.mu.Unlock()
				return result, errors.New("websocket turn response limit reached")
			}
			turn.ids[responseID] = true
			if lastResponseID != "" && lastResponseID != responseID {
				turn.steers = 0
				lastTerminal = ""
			}
			turn.mu.Unlock()
			lastResponseID = responseID
		}
		if responseID != "" {
			result.RequestID, result.ResponseID = responseID, responseID
		}
		if isOpenAIWSTokenEvent(eventType) && result.FirstTokenMs == nil {
			ms := int(time.Since(start).Milliseconds())
			result.FirstTokenMs = &ms
		}
		if openAIWSMessageShouldParseUsage(eventType, message) {
			id := responseID
			if id == "" {
				id = lastResponseID
			}
			if id == "" {
				return result, errors.New("upstream websocket usage has no response identity")
			}
			usage := usages[id]
			parseOpenAIWSResponseUsageFromCompletedEvent(message, &usage)
			usages[id] = usage
		}
		if gjson.GetBytes(turn.payload, "generate").Type == gjson.False && (isOpenAIWSTokenEvent(eventType) || strings.HasPrefix(eventType, "response.output_item.") || gjson.GetBytes(message, "response.output.#").Int() > 0) {
			deliveryErr = errors.New("upstream returned generated output for websocket warmup")
			turn.cancel()
		}
		if turn.ctx.Err() == nil {
			message = restoreCodexToolNamesFromContext(turn.gin, message)
			if err = emit(append([]byte(nil), message...)); err != nil {
				deliveryErr = err
				result.ClientDisconnect = true
				turn.cancel()
			}
		}
		if eventType == "error" {
			result.UpstreamTerminalEvent = "error"
			return result, errors.New("upstream websocket error event")
		}
		if eventType == "response.steer.failed" {
			turn.mu.Lock()
			if turn.steers > 0 {
				turn.steers--
			}
			turn.mu.Unlock()
		}
		if isOpenAIWSTerminalEvent(eventType) {
			lastTerminal = eventType
			result.UpstreamTerminalEvent = eventType
			if eventType == "response.incomplete" && gjson.GetBytes(message, "response.incomplete_details.reason").Str == "steered" && turn.ctx.Err() == nil {
				continue
			}
		}
		turn.mu.Lock()
		waiting := turn.steers > 0
		turn.mu.Unlock()
		if lastTerminal != "" && (!waiting || eventType == "response.steer.pending" || turn.ctx.Err() != nil) {
			clean = len(pending) == 0
			if turn.ctx.Err() != nil {
				if deliveryErr != nil {
					return result, deliveryErr
				}
				return result, turn.ctx.Err()
			}
			return result, nil
		}
		if eventType == "" {
			return result, errors.New("upstream websocket event type is missing")
		}
	}
}
