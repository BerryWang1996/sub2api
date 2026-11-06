package handler

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// This writer belongs to one turn. Shared HTTP admission code can return a
// bounded error without writing to the hijacked connection or another lane.
type aiWSPlanWriter struct {
	header  http.Header
	body    bytes.Buffer
	status  int
	written bool
}

func newAIWSPlanWriter() *aiWSPlanWriter {
	return &aiWSPlanWriter{header: make(http.Header), status: http.StatusOK}
}
func (w *aiWSPlanWriter) Header() http.Header { return w.header }
func (w *aiWSPlanWriter) WriteHeader(status int) {
	if !w.written {
		w.status = status
	}
}
func (w *aiWSPlanWriter) WriteHeaderNow() { w.written = true }
func (w *aiWSPlanWriter) Write(body []byte) (int, error) {
	w.WriteHeaderNow()
	if len(body) > 16<<10-w.body.Len() {
		return 0, errors.New("websocket admission error exceeds limit")
	}
	return w.body.Write(body)
}
func (w *aiWSPlanWriter) WriteString(body string) (int, error) { return w.Write([]byte(body)) }
func (w *aiWSPlanWriter) Status() int                          { return w.status }
func (w *aiWSPlanWriter) Size() int {
	if !w.written {
		return -1
	}
	return w.body.Len()
}
func (w *aiWSPlanWriter) Written() bool { return w.written }
func (w *aiWSPlanWriter) Flush()        { w.WriteHeaderNow() }
func (w *aiWSPlanWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errors.New("turn writer cannot hijack")
}
func (w *aiWSPlanWriter) CloseNotify() <-chan bool { return nil }
func (w *aiWSPlanWriter) Pusher() http.Pusher      { return nil }

type aiWSTurnFailure struct{ code, message string }

func (e *aiWSTurnFailure) Error() string     { return e.message }
func aiWSFailure(code, message string) error { return &aiWSTurnFailure{code: code, message: message} }
func (w *aiWSPlanWriter) failure() error {
	code, message := gjson.GetBytes(w.body.Bytes(), "error.type").Str, gjson.GetBytes(w.body.Bytes(), "error.message").Str
	if code == "" {
		code = "ai_routing_unavailable"
	}
	if message == "" {
		message = "AI websocket admission failed"
	}
	return aiWSFailure(code, message)
}

func aiWSPrivateContext(template *gin.Context, ctx context.Context, key *service.APIKey, sub *service.UserSubscription, body []byte) (*gin.Context, *aiWSPlanWriter) {
	c := template.Copy()
	w := newAIWSPlanWriter()
	c.Writer = w
	id := "ai-ws-" + uuid.NewString()
	ctx = context.WithValue(ctx, ctxkey.RequestID, id)
	ctx = context.WithValue(ctx, ctxkey.ClientRequestID, id)
	ctx = context.WithValue(ctx, ctxkey.Group, key.Group)
	c.Request = template.Request.Clone(ctx)
	c.Request.Header = template.Request.Header.Clone()
	c.Request.Header.Set("X-Request-ID", id)
	requestmodel.ResetRequestBody(c.Request, append([]byte(nil), body...))
	c.Set(string(middleware.ContextKeyAPIKey), key)
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: key.User.ID, Concurrency: key.User.Concurrency})
	c.Set(string(middleware.ContextKeyUserRole), key.User.Role)
	c.Set(string(middleware.ContextKeySubscription), sub)
	c.Set("ai_client_endpoint", service.AIRoutingEndpointResponsesWS)
	setOpenAIClientTransportWS(c)
	return c, w
}

func (h *GatewayHandler) routeAIResponsesWebSocket(c *gin.Context, runtime *aiRoutingRuntime, key *service.APIKey, p *service.AIRoutingPolicy) bool {
	if c.Request.Method != http.MethodGet || !isOpenAIWSUpgradeRequest(c.Request) {
		return false
	}
	switch c.Request.URL.Path {
	case "/v1/responses", "/responses", "/backend-api/codex/responses":
	default:
		return false
	}
	if !p.AllowsEndpoint(service.AIRoutingEndpointResponsesWS) {
		abortAI(c, 400, "ai_routing_endpoint_disabled", "Responses WebSocket is not enabled in the published routing policy")
		return true
	}
	if h.apiKeyService == nil || h.gatewayService == nil || h.openAIGatewayService == nil || h.billingCacheService == nil || h.concurrencyHelper == nil || runtime.store == nil {
		abortAI(c, 503, "ai_routing_unavailable", "AI websocket dependencies unavailable")
		return true
	}
	if key.Group.ClaudeCodeOnly {
		abortAI(c, 403, "permission_error", "This group requires the Claude Code Messages API")
		return true
	}
	clientIP := middleware.SecurityClientIP(c)
	if _, err := h.apiKeyService.RevalidateAIRoutingKey(c.Request.Context(), key, clientIP); err != nil {
		abortAI(c, 403, "ai_source_revoked", "Routing credentials or source permissions changed")
		return true
	}
	maxIngress := 0
	if h.cfg != nil {
		maxIngress = h.cfg.Gateway.OpenAIWS.MaxIngressConnectionsPerAPIKey
	}
	lease, acquired, err := h.concurrencyHelper.AcquireOpenAIWSIngressLease(c.Request.Context(), key.ID, maxIngress)
	if err != nil || !acquired {
		abortAI(c, 429, "rate_limit_error", "WebSocket ingress capacity unavailable")
		return true
	}
	ctx := c.Request.Context()
	if lease != nil {
		defer lease.Release()
		ctx = lease.Context()
	}
	ctx, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()
	template := c.Copy()
	template.Request = c.Request.Clone(ctx)
	ws, err := coderws.Accept(c.Writer, c.Request, &coderws.AcceptOptions{CompressionMode: coderws.CompressionContextTakeover})
	c.Abort()
	if err != nil {
		return true
	}
	defer func() { _ = ws.CloseNow() }()
	ws.SetReadLimit(service.ResolveOpenAIWSClientReadLimitBytes(h.cfg))
	var writeMu sync.Mutex
	write := func(writeCtx context.Context, event []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		writeCtx, finish := context.WithTimeout(writeCtx, 10*time.Second)
		defer finish()
		return ws.Write(writeCtx, coderws.MessageText, event)
	}
	hooks := h.aiWSProductionHooks(template, runtime, key, clientIP, write)
	lanes, err := service.NewAIRoutingWSLanes(ctx, service.AIRoutingWSLaneLimits{}, hooks)
	if err != nil {
		return true
	}
	defer func() {
		lanes.Close()
		waitCtx, finish := context.WithTimeout(context.WithoutCancel(ctx), 8*time.Second)
		defer finish()
		if err := lanes.Wait(waitCtx); err != nil {
			slog.Warn("ai_routing.ws_close_pending", "key_id", key.ID)
		}
	}()
	first := true
	for {
		var typ coderws.MessageType
		var raw []byte
		if first {
			typ, raw, err = service.ReadOpenAIWSClientMessage(ctx, ws, service.ResolveOpenAIWSClientFirstMessageTimeout(h.cfg), coderws.StatusPolicyViolation, "missing first response.create message")
			first = false
		} else {
			typ, raw, err = ws.Read(ctx)
		}
		if err != nil {
			return true
		}
		if typ != coderws.MessageText && typ != coderws.MessageBinary {
			_ = aiWSWriteError(ctx, write, "", "invalid_request_error", "Unsupported websocket frame")
			continue
		}
		eventType := gjson.GetBytes(raw, "type").Str
		switch eventType {
		case "response.create":
			_, err = lanes.Submit(ctx, raw)
		case "response.steer":
			controlCtx, finish := context.WithTimeout(ctx, 10*time.Second)
			err = lanes.Steer(controlCtx, raw)
			finish()
		default:
			err = aiWSFailure("unsupported_event", "Only response.create and response.steer are supported")
		}
		if err != nil {
			code, message := "invalid_request_error", "WebSocket event was not admitted"
			var failure *aiWSTurnFailure
			if errors.As(err, &failure) {
				code, message = failure.code, failure.message
			}
			if writeErr := aiWSWriteError(ctx, write, gjson.GetBytes(raw, "stream_id").Str, code, message); writeErr != nil {
				return true
			}
		}
	}
}

func aiWSWriteError(ctx context.Context, write func(context.Context, []byte) error, lane, code, message string) error {
	event := map[string]any{"type": "error", "error": map[string]string{"type": code, "code": code, "message": message}}
	if lane != "" {
		event["stream_id"] = lane
	}
	raw, _ := json.Marshal(event)
	return write(ctx, raw)
}

type aiWSExecutionState struct {
	c            *gin.Context
	key          *service.APIKey
	sub          *service.UserSubscription
	account      *service.Account
	request      service.AIRoutingRequest
	prepared     *aiRoutingExecutionPlan
	pricingAt    time.Time
	payloadHash  string
	inspector    *aiResponsesOriginInspector
	inflightDone func()
	controlMu    sync.Mutex
	controlHolds []*service.InflightReservation
	finished     bool
	native       bool
	executor     service.AIRoutingWSTurnExecutor
}

func (h *GatewayHandler) aiWSProductionHooks(template *gin.Context, runtime *aiRoutingRuntime, original *service.APIKey, clientIP string, write func(context.Context, []byte) error) service.AIRoutingWSLaneHooks {
	var states sync.Map
	return service.AIRoutingWSLaneHooks{
		Plan: func(ctx context.Context, turn service.AIRoutingWSTurn) (plan *service.AIRoutingWSPlan, err error) {
			principal, err := h.apiKeyService.RevalidateAIRoutingKey(ctx, original, clientIP)
			if err != nil {
				return nil, aiWSFailure("ai_source_revoked", "Routing credentials or source permissions changed")
			}
			key := principal.APIKey
			p, err := runtime.policies.GetPublished(ctx, key.Group.ID)
			if err != nil || p == nil || !p.Enabled || !p.AllowsEndpoint(service.AIRoutingEndpointResponsesWS) {
				return nil, aiWSFailure("ai_routing_endpoint_disabled", "Responses WebSocket is no longer enabled")
			}
			body := append([]byte(nil), turn.Payload...)
			if !gjson.GetBytes(body, "model").Exists() && turn.PreviousPublicModel != "" {
				body, err = sjson.SetBytes(body, "model", turn.PreviousPublicModel)
				if err != nil {
					return nil, err
				}
			}
			req, err := service.NewAIRoutingWSRequest(body, p.ContextMode)
			if err != nil {
				return nil, aiWSFailure("invalid_request_error", err.Error())
			}
			if blockedModelAllowlistCandidate(key.Group, requestmodel.FromBodyCandidates("", "application/json", body)) != "" {
				return nil, aiWSFailure("model_not_allowed", "Requested model alias is not permitted")
			}
			if key.Group.ClaudeCodeOnly {
				return nil, aiWSFailure("permission_error", "This group requires the Claude Code Messages API")
			}
			c, writer := aiWSPrivateContext(template, ctx, key, principal.Subscription, body)
			turnRuntime := *runtime
			turnRuntime.eligible = func(checkCtx context.Context, _ *service.APIKey, current *service.AIRoutingPolicy, request service.AIRoutingRequest, endpoint string, bound int64) []service.AIRoutingCandidate {
				fresh, checkErr := h.apiKeyService.RevalidateAIRoutingKey(checkCtx, original, clientIP)
				if checkErr != nil {
					return nil
				}
				return runtime.eligible(checkCtx, fresh.APIKey, current, request, endpoint, bound)
			}
			turnRuntime.revalidateSource = func(checkCtx context.Context, _ *service.APIKey) error {
				fresh, checkErr := h.apiKeyService.RevalidateAIRoutingKey(checkCtx, original, clientIP)
				if checkErr != nil {
					return checkErr
				}
				if fresh.APIKey.Group.ClaudeCodeOnly || blockedModelAllowlistCandidate(fresh.APIKey.Group, []string{req.PublicModel}) != "" {
					return service.ErrAIRoutingPrincipalRevoked
				}
				return nil
			}
			prepared := h.prepareAIRoutingExecution(c, &turnRuntime, key, p, req, body, service.AIRoutingEndpointResponsesWS)
			if prepared == nil {
				return nil, writer.failure()
			}
			var requestCtx context.Context
			var pricingAt time.Time
			if req.RequiresNativeResponses {
				requestCtx, pricingAt = h.openAIGatewayService.WithOpenAIRequestPricingContext(c.Request.Context(), key.GroupID)
			} else {
				requestCtx, pricingAt = service.WithGatewayTokenRequestPricing(c.Request.Context())
			}
			c.Request = c.Request.WithContext(requestCtx)
			subject, _ := middleware.GetAuthSubjectFromContext(c)
			if audit := h.checkSecurityAudit(c, nil, key, subject, service.ContentModerationProtocolOpenAIResponses, req.PublicModel, body); audit != nil && !audit.AllowNextStage {
				return nil, aiWSFailure("permission_error", "Request did not pass security policy")
			}
			userRelease, acquired, err := h.concurrencyHelper.TryAcquireUserSlotForAPIKey(requestCtx, key.UserID, key.User.Concurrency, key.ID)
			if err != nil || !acquired {
				return nil, aiWSFailure("rate_limit_error", "User concurrency capacity unavailable")
			}
			var accountRelease func()
			inflightDone := func() {}
			owned := true
			defer func() {
				if owned {
					if accountRelease != nil {
						accountRelease()
					}
					if userRelease != nil {
						userRelease()
					}
					inflightDone()
				}
			}()
			if err = h.billingCacheService.CheckBillingEligibility(requestCtx, key.User, key, key.Group, principal.Subscription, service.QuotaPlatform(requestCtx, key)); err != nil {
				return nil, aiWSFailure("billing_unavailable", "Billing eligibility check failed")
			}
			requestCtx, inflightDone, err = reserveInflightBalanceCtx(requestCtx, h.billingCacheService, h.openAIGatewayService, key, principal.Subscription, tokenInflightEstimate(prepared.Candidate.Model, prepared.Body))
			if err != nil {
				return nil, aiWSFailure("billing_unavailable", "Inflight balance reservation failed")
			}
			c.Request = c.Request.WithContext(requestCtx)
			var selection *service.AccountSelectionResult
			if req.RequiresNativeResponses {
				selection, _, err = h.openAIGatewayService.SelectAccountWithSchedulerForCapability(requestCtx, key.GroupID, req.PreviousResponseID, "", prepared.Candidate.Model, nil, service.OpenAIUpstreamTransportResponsesWebsocketV2Ingress, service.OpenAIEndpointCapabilityResponses, false, false, true, prepared.Candidate.Platform)
			} else {
				selection, err = h.gatewayService.SelectAccountWithLoadAwareness(requestCtx, key.GroupID, "", prepared.Candidate.Model, nil, "", 0)
			}
			if err != nil || selection == nil || selection.Account == nil {
				return nil, aiWSFailure("ai_destination_unavailable", "No available websocket execution account")
			}
			account := selection.Account
			accountRelease = selection.ReleaseFunc
			if !selection.Acquired {
				if selection.WaitPlan == nil {
					return nil, aiWSFailure("rate_limit_error", "Execution account is busy")
				}
				accountRelease, acquired, err = h.concurrencyHelper.TryAcquireAccountSlot(requestCtx, account.ID, selection.WaitPlan.MaxConcurrency)
				if err != nil || !acquired {
					return nil, aiWSFailure("rate_limit_error", "Execution account concurrency unavailable")
				}
			}
			requestCtx = service.ContextWithSelectionProfitGate(requestCtx, selection)
			checkProfit := h.gatewayService.GatewayProfitControlVetoLatest
			if req.RequiresNativeResponses {
				checkProfit = h.openAIGatewayService.ProfitControlVetoLatest
			}
			account, vetoed, _ := checkProfit(requestCtx, account)
			if vetoed || account == nil {
				return nil, aiWSFailure("ai_destination_unavailable", "Execution account failed final pricing admission")
			}
			c.Request = c.Request.WithContext(requestCtx)
			setOpsSelectedAccount(c, account.ID, account.Platform)
			revalidate := func(checkCtx context.Context) error {
				if status, code := turnRuntime.revalidateChoice(checkCtx, key, p, req, service.AIRoutingEndpointResponsesWS, account.ID, prepared.Candidate); status != 0 {
					return aiWSFailure(code, "Routing policy or execution destination changed")
				}
				if _, vetoed, _ := checkProfit(checkCtx, account); vetoed {
					return aiWSFailure("ai_destination_unavailable", "Execution pricing admission changed")
				}
				return checkCtx.Err()
			}
			state := &aiWSExecutionState{c: c, key: key, sub: principal.Subscription, account: account, request: req, prepared: prepared, pricingAt: pricingAt, payloadHash: service.HashUsageRequestPayload(body), inflightDone: inflightDone}
			options := service.AIRoutingWSNativeTurnOptions{PublicModel: req.PublicModel, Revalidate: revalidate,
				CheckSteer: func(checkCtx context.Context, budget service.AIRoutingWSSteeringBudget) error {
					return h.gatewayService.CheckAIRoutingWSSteeringCapacity(checkCtx, account.ID, prepared.Candidate.Model, budget)
				},
				ReserveSteer: func(checkCtx context.Context, budget service.AIRoutingWSSteeringBudget) (func(), error) {
					if !h.billingCacheService.InflightReservationEnabled() {
						return func() {}, nil
					}
					amount, priced := h.openAIGatewayService.EstimateAIRoutingWSSteeringReservation(checkCtx, key, prepared.Candidate.Model, budget, req.MaxOutput, req.OutputChoices)
					if !priced && h.billingCacheService.InflightReservationFailClosedOnUnpriced() {
						return nil, aiWSFailure("billing_unavailable", "Steering input reservation cannot be priced")
					}
					if amount <= 0 {
						return func() {}, nil
					}
					fresh, lookupErr := h.apiKeyService.RevalidateAIRoutingKey(checkCtx, original, clientIP)
					if lookupErr != nil {
						return nil, lookupErr
					}
					reservation, reserveErr := h.billingCacheService.ReserveInflight(checkCtx, fresh.APIKey.User, fresh.APIKey.Group, fresh.Subscription, amount)
					if reserveErr != nil {
						return nil, aiWSFailure("billing_unavailable", "Steering input balance reservation failed")
					}
					if !state.holdControlReservation(reservation) {
						reservation.HandlerDone()
						return nil, context.Canceled
					}
					return reservation.HandlerDone, nil
				},
			}
			if prepared.Origin != nil {
				options.PreferredConnID = prepared.Origin.ConnectionID
				options.ForcePreferredConn = !prepared.Origin.Store
			}
			native := service.NewOpenAIWSProtocolResolver(h.cfg).Resolve(account).Transport == service.OpenAIUpstreamTransportResponsesWebsocketV2 && service.AIRoutingAccountSupportsNativeResponses(account, prepared.Candidate.Model)
			var executor service.AIRoutingWSTurnExecutor
			if native {
				executor, err = h.openAIGatewayService.NewAIRoutingWSNativeTurn(requestCtx, c, account, prepared.Body, options)
			} else {
				if req.RequiresNativeResponses {
					return nil, aiWSFailure("ai_transport_unavailable", "Request requires native websocket execution")
				}
				requestCtx = withAIGeminiResponseState(requestCtx, prepared.OriginStore, key, prepared.Candidate, func() int64 { return account.ID }, service.AIRoutingEndpointResponsesWS)
				c.Request = c.Request.WithContext(requestCtx)
				executor, err = h.newAIWSHTTPExecutor(requestCtx, c, account, prepared.Body, req.PublicModel, revalidate)
			}
			if err != nil {
				return nil, aiWSFailure("ai_transport_unavailable", "Websocket execution transport or required continuation state unavailable")
			}
			state.native, state.executor = native, executor
			states.Store(turn.ID, state)
			var once sync.Once
			plan = &service.AIRoutingWSPlan{Context: requestCtx, PublicModel: req.PublicModel, CandidateID: prepared.Candidate.ID, ExecutionModel: prepared.Candidate.Model, AccountID: account.ID, PolicyVersion: p.Version, Executor: executor, Revalidate: revalidate, Release: func() {
				once.Do(func() {
					if accountRelease != nil {
						accountRelease()
					}
					if userRelease != nil {
						userRelease()
					}
				})
			}}
			owned = false
			return plan, nil
		},
		Emit: func(ctx context.Context, turn service.AIRoutingWSTurn, _ *service.AIRoutingWSPlan, event []byte, connID string) error {
			value, ok := states.Load(turn.ID)
			if !ok {
				return errors.New("websocket execution state unavailable")
			}
			state, valid := value.(*aiWSExecutionState)
			if !valid || state == nil {
				return errors.New("websocket execution state unavailable")
			}
			return state.emit(ctx, event, connID, write)
		},
		BeforeSteer: func(ctx context.Context, turn service.AIRoutingWSTurn, plan *service.AIRoutingWSPlan, raw []byte) error {
			value, ok := states.Load(turn.ID)
			if !ok {
				return errors.New("websocket execution state unavailable")
			}
			state, valid := value.(*aiWSExecutionState)
			if !valid || state == nil {
				return errors.New("websocket execution state unavailable")
			}
			if !state.native {
				return aiWSFailure("unsupported_event", "Selected HTTP execution transport does not support mid-turn steering")
			}
			id := gjson.GetBytes(raw, "previous_response_id").Str
			request := service.AIRoutingRequest{Endpoint: service.AIRoutingEndpointResponsesWS, RequiresOrigin: true, PreviousResponseID: id, OriginRefs: []service.AIRoutingOriginRef{{Kind: "response", ID: id, RequiresStored: true}}}
			binding, origin, err := loadAIResponsesBinding(ctx, state.prepared.OriginStore, state.key, request)
			if err != nil || origin == nil || binding != newAIToolBinding(state.prepared.Candidate, state.account.ID) || origin.ConnectionID != plan.Executor.ConnID() {
				return aiWSFailure("ai_origin_unavailable", "Steering response origin unavailable or revoked")
			}
			fresh, err := h.apiKeyService.RevalidateAIRoutingKey(ctx, original, clientIP)
			if err != nil {
				return err
			}
			body, err := json.Marshal(map[string]any{"model": state.request.PublicModel, "input": json.RawMessage(gjson.GetBytes(raw, "input").Raw), "store": false})
			if err != nil {
				return err
			}
			control, _ := aiWSPrivateContext(template, ctx, fresh.APIKey, fresh.Subscription, body)
			subject, _ := middleware.GetAuthSubjectFromContext(control)
			if audit := h.checkSecurityAudit(control, nil, fresh.APIKey, subject, service.ContentModerationProtocolOpenAIResponses, state.request.PublicModel, body); audit != nil && !audit.AllowNextStage {
				return aiWSFailure("permission_error", "Steering input did not pass security policy")
			}
			return ctx.Err()
		},
		Finish: func(turn service.AIRoutingWSTurn, plan *service.AIRoutingWSPlan, result *service.OpenAIForwardResult, turnErr error) {
			value, ok := states.LoadAndDelete(turn.ID)
			if ok {
				state, valid := value.(*aiWSExecutionState)
				if !valid || state == nil {
					return
				}
				defer state.inflightDone()
				controls := state.finishControlReservations()
				defer func() {
					for _, reservation := range controls {
						reservation.HandlerDone()
					}
				}()
				var gatewayResult *service.ForwardResult
				if bridge, ok := state.executor.(interface{ GatewayResult() *service.ForwardResult }); ok {
					gatewayResult = bridge.GatewayResult()
				}
				if result != nil && gatewayResult == nil {
					h.openAIGatewayService.ReportOpenAIAccountScheduleResult(state.account, state.prepared.Candidate.Model, turnErr == nil && openAIForwardSucceededForScheduling(result), result.FirstTokenMs)
				}
				if result != nil && (turnErr == nil || aiWSHasUsage(result)) && (!state.request.Warmup || aiWSHasUsage(result)) {
					result.BillingModel = state.prepared.Candidate.Model
					usageFields := clientRequestedUsageFields(state.c, service.ChannelMappingResult{}, state.request.PublicModel, state.prepared.Candidate.Model)
					if gatewayResult != nil {
						input := &service.RecordUsageInput{Result: gatewayResult, APIKey: state.key, User: state.key.User, Account: state.account, Subscription: state.sub, InboundEndpoint: EndpointResponses, UpstreamEndpoint: GetUpstreamEndpoint(state.c, state.account.Platform), UserAgent: template.GetHeader("User-Agent"), IPAddress: clientIP, RequestPayloadHash: state.payloadHash, APIKeyService: h.apiKeyService, QuotaPlatform: service.QuotaPlatform(plan.Context, state.key), ChannelUsageFields: usageFields, PricingAt: state.pricingAt, SessionID: service.ExtractClientSessionID(state.c)}
						h.submitAIWSUsageRecordTask(plan.Context, controls, func(recordCtx context.Context) {
							if err := h.gatewayService.RecordUsage(recordCtx, input); err != nil {
								slog.ErrorContext(recordCtx, "ai_routing.ws_usage_failed", "account_id", state.account.ID, "error", err)
							}
						})
					} else {
						input := &service.OpenAIRecordUsageInput{Result: result, APIKey: state.key, User: state.key.User, Account: state.account, Subscription: state.sub, InboundEndpoint: EndpointResponses, UpstreamEndpoint: EndpointResponses, UserAgent: template.GetHeader("User-Agent"), IPAddress: clientIP, RequestPayloadHash: state.payloadHash, APIKeyService: h.apiKeyService, QuotaPlatform: service.QuotaPlatform(plan.Context, state.key), ChannelUsageFields: usageFields, PricingAt: state.pricingAt, SessionID: service.ExtractClientSessionID(state.c)}
						h.submitAIWSUsageRecordTask(plan.Context, controls, func(recordCtx context.Context) {
							if err := h.openAIGatewayService.RecordUsage(recordCtx, input); err != nil {
								slog.ErrorContext(recordCtx, "ai_routing.ws_usage_failed", "account_id", state.account.ID, "error", err)
							}
						})
					}
				}
			}
			if turnErr != nil && !errors.Is(turnErr, context.Canceled) && (result == nil || result.UpstreamTerminalEvent != "error") {
				code, message := "ai_execution_failed", "WebSocket turn could not complete"
				var failure *aiWSTurnFailure
				if errors.As(turnErr, &failure) {
					code, message = failure.code, failure.message
				}
				_ = aiWSWriteError(template.Request.Context(), write, turn.StreamID, code, message)
			}
		},
	}
}

func (state *aiWSExecutionState) holdControlReservation(reservation *service.InflightReservation) bool {
	state.controlMu.Lock()
	defer state.controlMu.Unlock()
	if state.finished {
		return false
	}
	if reservation != nil {
		state.controlHolds = append(state.controlHolds, reservation)
	}
	return true
}

func (state *aiWSExecutionState) finishControlReservations() []*service.InflightReservation {
	state.controlMu.Lock()
	defer state.controlMu.Unlock()
	state.finished = true
	reservations := state.controlHolds
	state.controlHolds = nil
	return reservations
}

// Extend the existing usage task's reservation handoff to steering deltas.
// Both normal completion and an explicit worker-pool drop release every hold.
func (h *GatewayHandler) submitAIWSUsageRecordTask(parent context.Context, controls []*service.InflightReservation, task service.UsageRecordTask) {
	wrapped, abandon := wrapUsageRecordTaskContext(parent, task)
	var releases []func()
	for _, reservation := range controls {
		releases = append(releases, reservation.Acquire())
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			for _, done := range releases {
				done()
			}
		})
	}
	run := func(ctx context.Context) { defer release(); wrapped(ctx) }
	if h.usageRecordWorkerPool != nil {
		mode := h.usageRecordWorkerPool.Submit(run)
		if mode != service.UsageRecordSubmitModeDroppedStopped {
			if mode.Dropped() {
				abandon()
				release()
			}
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("ai_routing.ws_usage_panic")
		}
	}()
	run(ctx)
}

func aiWSHasUsage(result *service.OpenAIForwardResult) bool {
	u := result.Usage
	return u.InputTokens > 0 || u.OutputTokens > 0 || u.CacheCreationInputTokens > 0 || u.CacheReadInputTokens > 0 || u.ImageInputTokens > 0 || u.ImageOutputTokens > 0 || u.ImageCacheReadTokens > 0 || result.ImageCount > 0
}

func (state *aiWSExecutionState) emit(ctx context.Context, event []byte, connID string, write func(context.Context, []byte) error) error {
	if err := state.bindEvent(ctx, event, connID); err != nil {
		return err
	}
	return write(ctx, event)
}

func (state *aiWSExecutionState) bindEvent(ctx context.Context, event []byte, connID string) error {
	typ := gjson.GetBytes(event, "type").Str
	if strings.HasPrefix(typ, "response.steer.") {
		return nil
	}
	if typ == "response.created" {
		state.inspector = nil
	}
	if state.inspector == nil {
		state.inspector = &aiResponsesOriginInspector{emitted: make(map[string]bool), bind: func(kind, id, responseID string) error {
			target := newAIToolBinding(state.prepared.Candidate, state.account.ID)
			origin := service.AIRoutingOriginBinding{Protocol: service.AIRoutingEndpointResponsesWS, ConnectionID: connID, PublicModel: state.request.PublicModel, Candidate: target.Candidate, Account: target.Account, Model: target.Model, Platform: target.Platform, GroupTarget: target.GroupTarget, AccountTarget: target.AccountTarget, ResponseID: responseID, Store: state.request.StoreResponse, Native: state.native}
			return saveAIResponsesBinding(ctx, state.prepared.OriginStore, state.key, service.AIRoutingOriginRef{Kind: kind, ID: id}, origin)
		}}
	}
	return state.inspector.inspect(event, typ)
}

var _ gin.ResponseWriter = (*aiWSPlanWriter)(nil)
