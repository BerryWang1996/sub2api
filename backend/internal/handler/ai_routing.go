package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
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
	policies aiPolicyReader
	decider  service.AIModelChooser
	store    service.AIRoutingStore
	eligible func(context.Context, *service.APIKey, *service.AIRoutingPolicy, service.AIRoutingRequest, string, int64) []service.AIRoutingCandidate
	admit    func(*gin.Context, *service.APIKey, []byte, string) (func(), bool)
}

func (h *GatewayHandler) SetAIRouting(policies *service.AIRoutingPolicyService, decider *service.AIRoutingDecider, store service.AIRoutingStore) {
	h.aiRouting = &aiRoutingRuntime{policies: policies, decider: decider, store: store, eligible: h.gatewayService.EligibleAIRoutingCandidates, admit: h.admitAIDecision}
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
	runtime := h.aiRouting
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
	default:
		abortAI(c, 400, "ai_routing_endpoint_unsupported", "This AI group supports the Messages API")
		return
	}
	if c.Request.Method != http.MethodPost {
		abortAI(c, 400, "ai_routing_endpoint_unsupported", "Unsupported AI routing method")
		return
	}
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
	req, err := service.NewAIRoutingRequest(body, p.ContextMode)
	if err != nil {
		abortAI(c, 400, "invalid_request_error", err.Error())
		return
	}
	if key.Group.ClaudeCodeOnly {
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
	ctx := c.Request.Context()
	binding, err := loadAIToolBinding(ctx, runtime.store, key, req)
	if err != nil {
		abortAI(c, 409, "ai_tool_binding_unavailable", "Cannot resume the originating tool request")
		return
	}
	eligible := runtime.eligible(ctx, key, p, req, endpoint, binding.Account)
	if binding.Account != 0 {
		matches := eligible[:0]
		for _, candidate := range eligible {
			if newAIToolBinding(candidate, binding.Account) == binding {
				matches = append(matches, candidate)
			}
		}
		eligible = matches
		if len(eligible) == 0 {
			abortAI(c, 409, "ai_tool_binding_revoked", "Original tool destination is no longer eligible")
			return
		}
	}
	if endpoint == service.CompositeRouteEndpointCountTokens && len(eligible) > 1 {
		abortAI(c, 400, "ai_count_target_ambiguous", "Token counting requires a single eligible or bound target")
		return
	}
	var release func()
	if len(eligible) > 1 {
		var admitted bool
		release, admitted = runtime.admit(c, key, body, req.PublicModel)
		if !admitted {
			return
		}
		release = wrapReleaseOnDone(c.Request.Context(), release)
		if release != nil {
			defer release()
		}
	}
	ctx = c.Request.Context()
	started := time.Now()
	chosen, decision, source, err := service.ChooseAIRoutingCandidate(ctx, p, req, eligible, runtime.decider, c.Request.Host)
	if release != nil {
		release()
	} // Execution acquires its own user and account slots.
	requestID, _ := ctx.Value(ctxkey.RequestID).(string)
	if binding.Account != 0 && err == nil {
		source = "continuation"
	}
	// A paid decision can take seconds. Publication and account grants may
	// have changed while it was running; never execute a stale choice or reroute
	// silently. Draft-only edits do not change the published version.
	revalidationStatus, revalidationCode := 0, ""
	if err == nil {
		revalidationStatus, revalidationCode = runtime.revalidateChoice(ctx, key, p, req, endpoint, binding.Account, chosen)
		if revalidationStatus != 0 {
			source = "rejected"
			decision.FailureReason = revalidationCode
		}
	}
	trace := service.AIRoutingTrace{
		At: started.UTC(), RequestID: requestID, PublicModel: req.PublicModel,
		CandidateID: chosen.ID, ExecutionModel: chosen.Model,
		DecisionModel: decision.Model, DecisionAccountID: decision.AccountID,
		InputTokens: decision.InputTokens, OutputTokens: decision.OutputTokens,
		DecisionCostUSD: h.gatewayService.EstimateAIDecisionUSD(decision),
		LatencyMS:       time.Since(started).Milliseconds(), Source: source,
		FailureReason: decision.FailureReason, PolicyVersion: p.Version,
	}
	trace.SetRequestContext(ctx)
	traceCtx, finishTrace := context.WithTimeout(context.WithoutCancel(ctx), 200*time.Millisecond)
	traceErr := service.SaveAIRoutingTrace(traceCtx, runtime.store, p.GroupID, trace)
	finishTrace()
	if traceErr != nil {
		slog.Warn("ai_routing.trace_unavailable", "request_id", requestID)
	}
	slog.InfoContext(ctx, "ai_routing.decision", "request_id", requestID, "client_request_id", trace.ClientRequestID, "usage_request_id", trace.UsageRequestID, "key_id", key.ID, "group_id", p.GroupID, "policy_version", p.Version, "source", source, "candidate_id", chosen.ID, "execution_model", chosen.Model, "decision_model", decision.Model, "decision_account_id", decision.AccountID, "decision_input_tokens", decision.InputTokens, "decision_output_tokens", decision.OutputTokens)
	if err != nil {
		abortAI(c, 503, "ai_routing_unavailable", "No eligible route or decision available")
		return
	}
	if revalidationStatus != 0 {
		abortAI(c, revalidationStatus, revalidationCode, "Routing policy or destination changed; retry the request")
		return
	}
	d := service.CompositeRouteDecision{Matched: true, Source: service.CompositeRouteSourceAI, AICandidateID: chosen.ID, GroupID: p.GroupID, PublicModel: req.PublicModel, Endpoint: endpoint, TargetPlatform: chosen.Platform, UpstreamModel: chosen.Model, TargetGroupID: chosen.TargetGroupID, TargetAccountID: chosen.TargetAccountID, BoundAccountID: binding.Account}
	rewritten, err := sjson.SetBytes(body, "model", chosen.Model)
	if err != nil {
		abortAI(c, 400, "invalid_request_error", "Could not rewrite the request model")
		return
	}
	service.SetCompositeRouteDecision(c, d)
	requestmodel.ResetRequestBody(c.Request, rewritten)
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

func (r *aiRoutingRuntime) revalidateChoice(ctx context.Context, key *service.APIKey, original *service.AIRoutingPolicy, req service.AIRoutingRequest, endpoint string, bound int64, chosen service.AIRoutingCandidate) (int, string) {
	if ctx.Err() != nil {
		return http.StatusServiceUnavailable, "ai_routing_cancelled"
	}
	current, err := r.policies.GetPublished(ctx, original.GroupID)
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
	for _, candidate := range r.eligible(ctx, key, current, req, endpoint, bound) {
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
	if d := runSecurityAudit(c, nil, h.securityAuditCoordinator, h.contentModerationService, key, subject, service.ContentModerationProtocolAnthropicMessages, model, body, "ai_decision"); d != nil && !d.AllowNextStage {
		h.anthropicSecurityAuditError(c, d)
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
