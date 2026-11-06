package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	pkghttp "github.com/Wei-Shaw/sub2api/internal/pkg/httputil"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/sjson"
)

type aiPolicyReader interface {
	GetPublished(context.Context, int64) (*service.AIRoutingPolicy, error)
}
type aiRoutingRuntime struct {
	policies         aiPolicyReader
	decider          service.AIModelChooser
	store            service.AIRoutingStore
	eligible         func(context.Context, *service.APIKey, *service.AIRoutingPolicy, service.AIRoutingRequest, string, int64) []service.AIRoutingCandidate
	admit            func(*gin.Context, *service.APIKey, []byte, string) (func(), bool)
	responseNative   func(context.Context, int64, string) (bool, error)
	revalidateSource func(context.Context, *service.APIKey) error
	principal        func(context.Context, *service.APIKey, string) (*service.AIRoutingPrincipal, error)
	clientIP         string
}

func (h *GatewayHandler) SetAIRouting(policies *service.AIRoutingPolicyService, decider *service.AIRoutingDecider, store service.AIRoutingStore) {
	h.aiRouting = &aiRoutingRuntime{policies: policies, decider: decider, store: store, eligible: h.gatewayService.EligibleAIRoutingCandidates, admit: h.admitAIDecision, responseNative: h.gatewayService.AIRoutingNativeResponsesAccount, principal: h.apiKeyService.RevalidateAIRoutingKey}
}

func abortAI(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{"type": "error", "error": gin.H{"type": code, "message": message}})
}

// AIRoute runs after authentication and public-model allowlisting, before
// platform dispatch. Non-generative and forced-platform aliases fail closed.
func (h *GatewayHandler) AIRoute(c *gin.Context) {
	key, ok := middleware.GetAPIKeyFromContext(c)
	if !ok || key == nil || key.Group == nil || key.Group.Platform != service.PlatformComposite {
		return
	}
	// Routing mode is immutable after group creation. Legacy auth snapshots
	// omit this bit and identify static groups; keep their existing dispatch
	// independent of AI policy storage and endpoint restrictions.
	if !key.Group.AIRouting {
		return
	}
	if h == nil || h.aiRouting == nil {
		abortAI(c, 503, "ai_router_unavailable", "AI router unavailable")
		return
	}
	runtime, key, err := h.aiRouting.httpPrincipal(c, key)
	if err != nil {
		abortAI(c, 403, "ai_source_revoked", "Routing credentials or source permissions changed")
		return
	}
	p, err := runtime.policies.GetPublished(c.Request.Context(), key.Group.ID)
	if err != nil || p == nil {
		abortAI(c, 503, "ai_policy_unavailable", "Unable to load routing policy")
		return
	}
	if !p.Enabled || !key.Group.IsActive() {
		abortAI(c, 403, "ai_routing_disabled", "AI routing group is disabled")
		return
	}
	if c.GetHeader(service.AIRoutingInternalHeader) != "" {
		abortAI(c, 400, "ai_routing_loop", "Recursive decision routing is disabled")
		return
	}
	if h.routeAIResponsesControl(c, runtime, key, p) {
		return
	}
	if h.routeAIResponsesWebSocket(c, runtime, key, p) {
		return
	}
	path := c.FullPath()
	if c.Request.Method == http.MethodGet {
		switch path {
		case "/v1/usage":
			return
		case "/v1/models", "/models":
			if c.Query("client_version") != "" {
				break
			}
			aliases := key.Group.ModelAllowlist.FilterForListing([]string{"auto", "*"})
			models := []gin.H{}
			for _, alias := range aliases {
				if !strings.Contains(alias, "*") {
					models = append(models, gin.H{"id": alias, "object": "model", "owned_by": "sub2api"})
				}
			}
			c.AbortWithStatusJSON(200, gin.H{"object": "list", "data": models})
			return
		}
	}
	endpoint := service.CompositeRouteEndpointMessages
	switch path {
	case "/v1/messages":
	case "/messages/count_tokens", "/v1/messages/count_tokens":
		endpoint = service.CompositeRouteEndpointCountTokens
	case "/v1/responses", "/responses", "/backend-api/codex/responses":
		endpoint = service.CompositeRouteEndpointResponses
	case "/v1/chat/completions", "/chat/completions":
		endpoint = service.CompositeRouteEndpointChatCompletions
	case "/v1beta/models/*modelAction":
		endpoint = service.CompositeRouteEndpointGemini
	default:
		if service.AIRoutingResponsesAuxiliaryKind(nil, c.Request.URL.Path) == "" {
			abortAI(c, 400, "ai_routing_endpoint_unsupported", "This API is not supported by AI routing")
			return
		}
		endpoint = service.CompositeRouteEndpointResponses
	}
	if c.Request.Method != http.MethodPost {
		abortAI(c, 400, "ai_routing_endpoint_unsupported", "Unsupported AI routing method")
		return
	}
	if !p.AllowsEndpoint(endpoint) {
		abortAI(c, 400, "ai_routing_endpoint_disabled", "This API is not enabled in the published routing policy")
		return
	}
	c.Set("ai_client_endpoint", endpoint)
	body, err := pkghttp.ReadRequestBodyWithPrealloc(c.Request)
	if err != nil {
		status := 400
		var sizeErr *http.MaxBytesError
		if errors.As(err, &sizeErr) {
			status = 413
		}
		abortAI(c, status, "invalid_request_error", "Unable to read request body")
		return
	}
	var req service.AIRoutingRequest
	switch endpoint {
	case service.CompositeRouteEndpointResponses:
		if operation := service.AIRoutingResponsesAuxiliaryKind(body, c.Request.URL.Path); operation != "" {
			req, err = service.NewAIRoutingResponsesAuxiliaryRequest(body, p.ContextMode, operation)
		} else {
			req, err = service.NewAIRoutingResponsesRequest(body, p.ContextMode)
		}
	case service.CompositeRouteEndpointChatCompletions:
		req, err = service.NewAIRoutingChatRequest(body, p.ContextMode)
	case service.CompositeRouteEndpointGemini:
		var model, action string
		model, action, err = parseGeminiModelAction(strings.TrimPrefix(c.Param("modelAction"), "/"))
		if err == nil {
			req, err = service.NewAIRoutingGeminiRequest(body, model, action, p.ContextMode)
		}
	default:
		req, err = service.NewAIRoutingRequest(body, p.ContextMode)
	}
	if err != nil {
		abortAI(c, 400, "invalid_request_error", err.Error())
		return
	}
	if req.Background && !service.AIRoutingBackgroundReleased() {
		abortAI(c, http.StatusBadRequest, "ai_background_not_released", "Background AI routing has not been released; use foreground Responses")
		return
	}
	if key.Group.ClaudeCodeOnly {
		if endpoint != service.CompositeRouteEndpointMessages && endpoint != service.CompositeRouteEndpointCountTokens {
			abortAI(c, 403, "permission_error", "This group requires the Claude Code Messages API")
			return
		}
		SetClaudeCodeClientContext(c, body, nil)
		if !service.IsClaudeCodeClient(c.Request.Context()) {
			abortAI(c, 403, "permission_error", "This group requires a Claude Code client")
			return
		}
	}
	if req.UsesTools && runtime.store == nil {
		abortAI(c, 503, "ai_binding_unavailable", "Tool continuation storage unavailable")
		return
	}
	plan := h.prepareAIRoutingExecution(c, runtime, key, p, req, body, endpoint)
	if plan == nil {
		return
	}
	chosen, originStore := plan.Candidate, plan.OriginStore
	ctx := c.Request.Context()
	requestID, _ := ctx.Value(ctxkey.RequestID).(string)
	if req.Operation == service.AIRoutingResponsesInputTokens {
		return // The existing count handler writes a count, not a Response ID.
	}
	if endpoint == service.CompositeRouteEndpointResponses {
		h.forwardAIResponsesWithOrigin(c, runtime, originStore, key, p, req, chosen)
		return
	}
	if endpoint == service.CompositeRouteEndpointChatCompletions {
		h.forwardAIChatWithOrigin(c, runtime, key, chosen)
		return
	}
	if endpoint == service.CompositeRouteEndpointGemini {
		if req.Operation != service.AIRoutingGeminiCountTokens {
			h.forwardAIGeminiWithOrigin(c, runtime, originStore, key, req, chosen)
		}
		return
	}
	if req.UsesTools && endpoint == service.CompositeRouteEndpointMessages {
		original := c.Writer
		forwardCtx, cancelForward := context.WithCancel(c.Request.Context())
		defer cancelForward()
		c.Request = c.Request.WithContext(forwardCtx)
		writer := &aiToolWriter{ResponseWriter: original, bind: func(id string) error {
			account := c.GetInt64(opsAccountIDKey)
			return saveAIToolBinding(forwardCtx, runtime.store, key, req.PublicModel, id, newAIToolBinding(chosen, account))
		}}
		writer.onFailure = func(err error) {
			slog.WarnContext(ctx, "ai_routing.binding_write_failed", "request_id", requestID, "error", err)
			c.Abort()
			c.Writer = original
			if !original.Written() {
				c.Header("Content-Type", "application/json")
				c.Header("Content-Length", "")
			}
			h.handleStreamingAwareErrorWithCode(c, http.StatusServiceUnavailable, "api_error", "ai_tool_binding_unavailable", "Tool continuation could not be recorded", original.Written())
			service.MarkResponseCommitted(c)
			c.Writer = writer
			cancelForward()
		}
		c.Writer = writer
		defer func() { c.Writer = original }()
		c.Next()
		_ = writer.finish()
	}
}

type aiRoutingExecutionPlan struct {
	Candidate   service.AIRoutingCandidate
	Binding     aiToolBinding
	OriginStore service.AIRoutingOriginStore
	Body        []byte
	Origin      *service.AIRoutingOriginBinding
}

// prepareAIRoutingExecution is shared by HTTP requests and individual WS turns.
// It resolves one immutable candidate, records the decision, revalidates it and
// installs final-send restrictions. Response I/O remains owned by each ingress.
func (h *GatewayHandler) prepareAIRoutingExecution(c *gin.Context, runtime *aiRoutingRuntime, key *service.APIKey, p *service.AIRoutingPolicy, req service.AIRoutingRequest, body []byte, endpoint string) *aiRoutingExecutionPlan {
	var ok bool
	var err error
	ctx := c.Request.Context()
	if _, err := runtime.requestPrincipal(ctx, key, req, endpoint); err != nil {
		abortAI(c, 403, "ai_source_revoked", "Routing credentials or source permissions changed")
		return nil
	}
	var binding aiToolBinding
	var origin *service.AIRoutingOriginBinding
	var originStore service.AIRoutingOriginStore
	var geminiCache *service.GeminiCachedContent
	switch endpoint {
	case service.CompositeRouteEndpointResponses, service.AIRoutingEndpointResponsesWS:
		originStore, ok = runtime.store.(service.AIRoutingOriginStore)
		if !ok || originStore == nil {
			abortAI(c, 503, "ai_binding_unavailable", "Response origin storage unavailable")
			return nil
		}
		if req.Background && (h.aiBackground == nil || !h.aiBackground.Ready()) {
			abortAI(c, 503, "ai_background_unavailable", "Background response settlement is unavailable")
			return nil
		}
		if req.Background && !req.StoreResponse {
			abortAI(c, 400, "invalid_request_error", "Background Responses require provider storage")
			return nil
		}
		binding, origin, err = loadAIResponsesBinding(ctx, originStore, key, req)
	case service.CompositeRouteEndpointChatCompletions:
		binding, err = loadAIChatBinding(ctx, runtime.store, key, req)
	case service.CompositeRouteEndpointGemini:
		originStore, ok = runtime.store.(service.AIRoutingOriginStore)
		if !ok || originStore == nil {
			abortAI(c, 503, "ai_binding_unavailable", "Gemini origin storage unavailable")
			return nil
		}
		binding, err = loadAIGeminiBinding(ctx, originStore, key, req)
		if err == nil {
			geminiCache, err = h.prepareAIGeminiProviderReferences(c, key, req, body)
			if err == nil && geminiCache != nil && binding.Account != 0 && (binding.Account != geminiCache.AccountID || binding.Model != geminiCache.UpstreamModelID()) {
				err = errors.New("gemini cache and tool origins differ")
			}
		}
	default:
		binding, err = loadAIToolBinding(ctx, runtime.store, key, req)
	}
	if err != nil {
		if endpoint == service.CompositeRouteEndpointResponses {
			abortAI(c, 409, "ai_response_origin_unavailable", "Cannot resume the originating response")
			return nil
		}
		abortAI(c, 409, "ai_tool_binding_unavailable", "Cannot resume the originating tool request")
		return nil
	}
	boundAccount := binding.Account
	if geminiCache != nil {
		boundAccount = geminiCache.AccountID
	}
	eligible := runtime.eligible(ctx, key, p, req, endpoint, boundAccount)
	if geminiCache != nil {
		matches := eligible[:0]
		for _, candidate := range eligible {
			if candidate.Model == geminiCache.UpstreamModelID() {
				matches = append(matches, candidate)
			}
		}
		eligible = matches
	}
	if binding.Account != 0 {
		matches := eligible[:0]
		for _, candidate := range eligible {
			if newAIToolBinding(candidate, binding.Account) == binding {
				matches = append(matches, candidate)
			}
		}
		eligible = matches
		if len(eligible) == 0 {
			if endpoint == service.CompositeRouteEndpointResponses {
				abortAI(c, 409, "ai_response_origin_revoked", "Original response destination is no longer eligible")
				return nil
			}
			abortAI(c, 409, "ai_tool_binding_revoked", "Original tool destination is no longer eligible")
			return nil
		}
	}
	if endpoint == service.CompositeRouteEndpointCountTokens && len(eligible) > 1 {
		abortAI(c, 400, "ai_count_target_ambiguous", "Token counting requires a single eligible or bound target")
		return nil
	}
	if req.Operation != "" && len(eligible) > 1 {
		abortAI(c, 400, "ai_auxiliary_target_ambiguous", "This operation requires a single eligible or bound target")
		return nil
	}
	var release func()
	if len(eligible) > 1 {
		var admitted bool
		release, admitted = runtime.admit(c, key, body, req.PublicModel)
		if !admitted {
			return nil
		}
		release = wrapReleaseOnDone(c.Request.Context(), release)
		if release != nil {
			defer release()
		}
	}
	ctx = c.Request.Context()
	{
		ctx = service.WithAIRoutingDecisionGuard(ctx, func(checkCtx context.Context) error {
			if _, checkErr := runtime.requestPrincipal(checkCtx, key, req, endpoint); checkErr != nil {
				return service.ErrAIRoutingDecisionRevoked
			}
			current, checkErr := runtime.policies.GetPublished(checkCtx, p.GroupID)
			if checkErr != nil || current == nil || !current.Enabled || current.Version != p.Version || current.GroupID != p.GroupID {
				return service.ErrAIRoutingDecisionRevoked
			}
			if (endpoint == service.CompositeRouteEndpointResponses || endpoint == service.AIRoutingEndpointResponsesWS) && binding.Account != 0 {
				latestBinding, _, bindingErr := loadAIResponsesBinding(checkCtx, originStore, key, req)
				if bindingErr != nil || latestBinding != binding {
					return service.ErrAIRoutingDecisionRevoked
				}
			}
			if endpoint == service.CompositeRouteEndpointChatCompletions && binding.Account != 0 {
				latestBinding, bindingErr := loadAIChatBinding(checkCtx, runtime.store, key, req)
				if bindingErr != nil || latestBinding != binding {
					return service.ErrAIRoutingDecisionRevoked
				}
			}
			if endpoint == service.CompositeRouteEndpointGemini {
				latestBinding, bindingErr := loadAIGeminiBinding(checkCtx, originStore, key, req)
				if bindingErr != nil || latestBinding != binding {
					return service.ErrAIRoutingDecisionRevoked
				}
				if geminiCache != nil {
					latest, lookupErr := h.geminiCachedContentService.Resolve(checkCtx, key.ID, key.GroupID, geminiCache.PublicName())
					if lookupErr != nil || latest == nil || latest.AccountID != geminiCache.AccountID || latest.UpstreamModelID() != geminiCache.UpstreamModelID() || latest.UpstreamName != geminiCache.UpstreamName {
						return service.ErrAIRoutingDecisionRevoked
					}
				}
			}
			return nil
		})
		c.Request = c.Request.WithContext(ctx)
	}
	started := time.Now()
	chosen, decision, source, err := service.ChooseAIRoutingCandidate(ctx, p, req, eligible, runtime.decider, c.Request.Host)
	if release != nil {
		release()
	} // Execution acquires its own user and account slots.
	requestID, _ := ctx.Value(ctxkey.RequestID).(string)
	if boundAccount != 0 && err == nil {
		source = "continuation"
	}
	// A paid decision can take seconds. Publication and account grants may
	// have changed while it was running; never execute a stale choice or reroute
	// silently. Draft-only edits do not change the published version.
	revalidationStatus, revalidationCode := 0, ""
	if err == nil {
		revalidationStatus, revalidationCode = runtime.revalidateChoice(ctx, key, p, req, endpoint, boundAccount, chosen)
		if revalidationStatus != 0 {
			source = "rejected"
			decision.FailureReason = revalidationCode
		}
	}
	trace := service.AIRoutingTrace{
		Endpoint: endpoint,
		At:       started.UTC(), RequestID: requestID, PublicModel: req.PublicModel,
		CandidateID: chosen.ID, ExecutionModel: chosen.Model,
		DecisionModel: decision.Model, DecisionAccountID: decision.AccountID,
		InputTokens: decision.InputTokens, OutputTokens: decision.OutputTokens,
		DecisionCostUSD: h.gatewayService.EstimateAIDecisionUSD(decision),
		LatencyMS:       time.Since(started).Milliseconds(), Source: source,
		FailureReason: decision.FailureReason, PolicyVersion: p.Version,
		Context: decision.Context, DecisionCalls: decision.DecisionCalls, Stages: decision.Calls,
	}
	trace.SetRequestContext(ctx)
	saveTrace := func() {
		traceCtx, finishTrace := context.WithTimeout(context.WithoutCancel(ctx), 200*time.Millisecond)
		traceErr := service.SaveAIRoutingTrace(traceCtx, runtime.store, p.GroupID, trace)
		finishTrace()
		if traceErr != nil {
			slog.Warn("ai_routing.trace_unavailable", "request_id", requestID)
		}
	}
	saveTrace()
	slog.InfoContext(ctx, "ai_routing.decision", "request_id", requestID, "client_request_id", trace.ClientRequestID, "usage_request_id", trace.UsageRequestID, "key_id", key.ID, "group_id", p.GroupID, "policy_version", p.Version, "source", source, "candidate_id", chosen.ID, "execution_model", chosen.Model, "decision_model", decision.Model, "decision_account_id", decision.AccountID, "decision_input_tokens", decision.InputTokens, "decision_output_tokens", decision.OutputTokens)
	if err != nil {
		abortAI(c, 503, "ai_routing_unavailable", "No eligible route or decision available")
		return nil
	}
	if revalidationStatus != 0 {
		abortAI(c, revalidationStatus, revalidationCode, "Routing policy or destination changed; retry the request")
		return nil
	}
	// Trace persistence deliberately survives request cancellation and may block.
	// Revalidate after it and logging, including single/count/continuation paths.
	// Successful admission must be followed only by local request preparation.
	if status, code := runtime.revalidateChoice(ctx, key, p, req, endpoint, boundAccount, chosen); status != 0 {
		abortAI(c, status, code, "Routing policy or destination changed; retry the request")
		// The bounded history is append-only: retain the latest refusal under the
		// same request ID rather than leaving the earlier selection as its outcome.
		trace.Source, trace.FailureReason = "rejected", code
		saveTrace()
		return nil
	}
	d := service.CompositeRouteDecision{Matched: true, Source: service.CompositeRouteSourceAI, AICandidateID: chosen.ID, GroupID: p.GroupID, PublicModel: req.PublicModel, Endpoint: endpoint, TargetPlatform: chosen.Platform, UpstreamModel: chosen.Model, TargetGroupID: chosen.TargetGroupID, TargetAccountID: chosen.TargetAccountID, BoundAccountID: boundAccount}
	rewritten := body
	if endpoint != service.CompositeRouteEndpointGemini {
		rewritten, err = sjson.SetBytes(body, "model", chosen.Model)
	}
	if err != nil {
		abortAI(c, 400, "invalid_request_error", "Could not rewrite the request model")
		return nil
	}
	service.SetCompositeRouteDecision(c, d)
	if (p.ContextMode == "bounded_history" && endpoint == service.CompositeRouteEndpointMessages) || endpoint == service.CompositeRouteEndpointResponses || endpoint == service.CompositeRouteEndpointChatCompletions || endpoint == service.AIRoutingEndpointResponsesWS || endpoint == service.CompositeRouteEndpointGemini {
		executionCtx := service.WithAIRoutingExecutionRequirements(c.Request.Context(), service.AIRoutingRequirementsForCandidate(req, chosen))
		c.Request = c.Request.WithContext(h.gatewayService.WithAIRoutingFinalCapacityGuard(executionCtx))
	}
	if req.Operation != "" {
		// A unique pool candidate can contain several accounts. Once its first
		// account is about to run this auxiliary operation, retries cannot switch.
		finalCtx := c.Request.Context()
		var locked atomic.Int64
		locked.Store(boundAccount)
		c.Request = c.Request.WithContext(service.WithAIRoutingOutboundGuard(finalCtx, func(outbound *http.Request, accountID int64) error {
			if accountID <= 0 || !locked.CompareAndSwap(0, accountID) && locked.Load() != accountID {
				return &service.AIRoutingCapacityError{Check: service.AIRoutingCapabilityCheck{Reason: "execution_origin_unavailable"}}
			}
			return service.CheckAIRoutingOutboundRequest(outbound.WithContext(finalCtx), accountID)
		}))
	}
	// Every AI HTTP/WS execution, including legacy Messages context modes and
	// counts, rechecks the fresh principal and actual destination immediately
	// before transport. Keep the capacity/origin guard already installed above.
	guardCtx := c.Request.Context()
	c.Request = c.Request.WithContext(service.WithAIRoutingOutboundGuard(guardCtx, func(outbound *http.Request, accountID int64) error {
		if _, err := runtime.requestPrincipal(guardCtx, key, req, endpoint); err != nil {
			return &service.AIRoutingCapacityError{Check: service.AIRoutingCapabilityCheck{Reason: "ai_source_revoked"}}
		}
		if err := service.CheckAIRoutingOutboundRequest(outbound.WithContext(guardCtx), accountID); err != nil {
			return err
		}
		if status, code := runtime.revalidateChoice(guardCtx, key, p, req, endpoint, accountID, chosen); status != 0 {
			return &service.AIRoutingCapacityError{Check: service.AIRoutingCapabilityCheck{Reason: code}}
		}
		return nil
	}))
	requestmodel.ResetRequestBody(c.Request, rewritten)
	return &aiRoutingExecutionPlan{Candidate: chosen, Binding: binding, OriginStore: originStore, Body: rewritten, Origin: origin}
}

func (r *aiRoutingRuntime) revalidateChoice(ctx context.Context, key *service.APIKey, original *service.AIRoutingPolicy, req service.AIRoutingRequest, endpoint string, bound int64, chosen service.AIRoutingCandidate) (int, string) {
	if ctx.Err() != nil {
		return http.StatusServiceUnavailable, "ai_routing_cancelled"
	}
	fresh, sourceErr := r.requestPrincipal(ctx, key, req, endpoint)
	if ctx.Err() != nil {
		return http.StatusServiceUnavailable, "ai_routing_cancelled"
	}
	if sourceErr != nil {
		return http.StatusForbidden, "ai_source_revoked"
	}
	current, err := r.policies.GetPublished(ctx, original.GroupID)
	if ctx.Err() != nil {
		return http.StatusServiceUnavailable, "ai_routing_cancelled"
	}
	if err != nil {
		return http.StatusServiceUnavailable, "ai_policy_unavailable"
	}
	if current == nil || !current.Enabled {
		return http.StatusForbidden, "ai_routing_disabled"
	}
	if current.GroupID != original.GroupID || current.Version != original.Version {
		return http.StatusConflict, "ai_policy_changed"
	}
	expected := newAIToolBinding(chosen, bound)
	candidates := r.eligible(ctx, fresh, current, req, endpoint, bound)
	if ctx.Err() != nil {
		return http.StatusServiceUnavailable, "ai_routing_cancelled"
	}
	for _, candidate := range candidates {
		if newAIToolBinding(candidate, bound) == expected {
			return 0, ""
		}
	}
	return http.StatusConflict, "ai_candidate_revoked"
}

func (h *GatewayHandler) admitAIDecision(c *gin.Context, key *service.APIKey, body []byte, model string) (func(), bool) {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok || key.User == nil || h.billingCacheService == nil || h.concurrencyHelper == nil {
		abortAI(c, 503, "ai_admission_unavailable", "Decision admission unavailable")
		return nil, false
	}
	protocol := service.ContentModerationProtocolAnthropicMessages
	if c.GetString("ai_client_endpoint") == service.CompositeRouteEndpointResponses || c.GetString("ai_client_endpoint") == service.AIRoutingEndpointResponsesWS {
		protocol = service.ContentModerationProtocolOpenAIResponses
	} else if c.GetString("ai_client_endpoint") == service.CompositeRouteEndpointChatCompletions {
		protocol = service.ContentModerationProtocolOpenAIChat
	} else if c.GetString("ai_client_endpoint") == service.CompositeRouteEndpointGemini {
		protocol = service.ContentModerationProtocolGemini
	}
	if d := runSecurityAudit(c, nil, h.securityAuditCoordinator, h.contentModerationService, key, subject, protocol, model, body, "ai_decision"); d != nil && !d.AllowNextStage {
		switch protocol {
		case service.ContentModerationProtocolOpenAIResponses, service.ContentModerationProtocolOpenAIChat:
			h.responsesSecurityAuditError(c, d)
		case service.ContentModerationProtocolGemini:
			googleSecurityAuditError(c, d)
		default:
			h.anthropicSecurityAuditError(c, d)
		}
		c.Abort()
		return nil, false
	}
	release, acquired, err := h.concurrencyHelper.TryAcquireUserSlotForAPIKey(c.Request.Context(), subject.UserID, subject.Concurrency, key.ID)
	if err != nil {
		abortAI(c, 503, "ai_admission_unavailable", "Decision concurrency check unavailable")
		return nil, false
	}
	if !acquired {
		abortAI(c, 429, "rate_limit_error", "User concurrency limit reached")
		return nil, false
	}
	subscription, _ := middleware.GetSubscriptionFromContext(c)
	admitted, err := h.billingCacheService.AdmitAIRoutingDecision(c.Request.Context(), key.User, key, key.Group, subscription)
	if err != nil {
		if release != nil {
			release()
		}
		status, code, message, _ := billingErrorDetails(err)
		abortAI(c, status, code, message)
		return nil, false
	}
	c.Request = c.Request.WithContext(admitted)
	return release, true
}
