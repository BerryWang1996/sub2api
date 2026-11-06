package admin

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func ProvideAIRoutingGroupHandler(admin service.AdminService, dash *service.DashboardService, cap *service.GroupCapacityService, cfg *config.Config, p *service.AIRoutingPolicyService, d *service.AIRoutingDecider, aiStore service.AIRoutingStore, background *service.AIRoutingResponsesBackgroundService) *GroupHandler {
	h := NewGroupHandlerWithConfig(admin, dash, cap, cfg)
	h.aiPolicies = p
	h.aiDecider = d
	h.aiStore = aiStore
	h.aiBackground = background
	return h
}

func (h *GroupHandler) AIRoutingBackgroundJobs(c *gin.Context) {
	id, ok := h.aiGroupID(c)
	if !ok {
		return
	}
	if _, err := h.aiPolicies.Get(c.Request.Context(), id); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	status := c.Query("status")
	switch status {
	case "", service.AIBackgroundAdmitted, service.AIBackgroundSubmitting, service.AIBackgroundSubmissionUnknown, service.AIBackgroundActive, service.AIBackgroundUsageUnknown, service.AIBackgroundReady, service.AIBackgroundSettled, service.AIBackgroundReleased:
	default:
		response.BadRequest(c, "Invalid background job status")
		return
	}
	rows, err := h.aiBackground.Diagnostics(c.Request.Context(), id, status, 50)
	if err != nil {
		response.InternalError(c, "Background response diagnostics unavailable")
		return
	}
	response.Success(c, gin.H{"items": rows})
}

func (h *GroupHandler) AIRoutingDecisions(c *gin.Context) {
	id, ok := h.aiGroupID(c)
	if !ok {
		return
	}
	if _, err := h.aiPolicies.Get(c.Request.Context(), id); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	rows, err := service.ListAIRoutingDecisions(c.Request.Context(), h.aiStore, id)
	if err != nil {
		response.InternalError(c, "Recent decisions unavailable")
		return
	}
	response.Success(c, gin.H{"items": rows})
}

func (h *GroupHandler) aiGroupID(c *gin.Context) (int64, bool) {
	if h.rejectUnsupportedSimpleModeOperation(c, "advanced") {
		return 0, false
	}
	if h.aiPolicies == nil {
		response.InternalError(c, "AI routing is unavailable")
		return 0, false
	}
	return parsePositiveIDParam(c, "id")
}
func (h *GroupHandler) GetAIRouting(c *gin.Context) {
	id, ok := h.aiGroupID(c)
	if !ok {
		return
	}
	st, e := h.aiPolicies.Get(c.Request.Context(), id)
	if e != nil {
		response.ErrorFrom(c, e)
		return
	}
	response.Success(c, st)
}

type aiPolicyEdit struct {
	Revision       *int64                   `json:"revision"`
	Policy         *service.AIRoutingPolicy `json:"policy"`
	Sample         string                   `json:"sample"`
	SampleEndpoint string                   `json:"sample_endpoint"`
}

func bindAIPolicyEdit(c *gin.Context, needRevision, needPolicy bool) (aiPolicyEdit, bool) {
	var req aiPolicyEdit
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 256<<10)
	if err := c.ShouldBindJSON(&req); err != nil || needRevision && (req.Revision == nil || *req.Revision < 0) || needPolicy && req.Policy == nil || len(req.Sample) > 128<<10 {
		response.BadRequest(c, "Invalid AI routing request or sample length")
		return req, false
	}
	if req.SampleEndpoint == "" {
		req.SampleEndpoint = service.CompositeRouteEndpointMessages
	}
	if req.SampleEndpoint != service.CompositeRouteEndpointMessages && req.SampleEndpoint != service.CompositeRouteEndpointResponses && req.SampleEndpoint != service.CompositeRouteEndpointChatCompletions && req.SampleEndpoint != service.AIRoutingEndpointResponsesWS && req.SampleEndpoint != service.CompositeRouteEndpointGemini {
		response.BadRequest(c, "Unsupported AI routing sample endpoint")
		return req, false
	}
	return req, true
}
func (h *GroupHandler) SaveAIRoutingDraft(c *gin.Context) {
	id, ok := h.aiGroupID(c)
	if !ok {
		return
	}
	r, ok := bindAIPolicyEdit(c, true, true)
	if !ok {
		return
	}
	st, e := h.aiPolicies.SaveDraft(c.Request.Context(), id, *r.Revision, *r.Policy)
	if e != nil {
		response.ErrorFrom(c, e)
		return
	}
	response.Success(c, st)
}
func (h *GroupHandler) setAIPublication(c *gin.Context, enable bool) {
	id, ok := h.aiGroupID(c)
	if !ok {
		return
	}
	r, ok := bindAIPolicyEdit(c, true, false)
	if !ok {
		return
	}
	write := h.aiPolicies.Disable
	if enable {
		write = h.aiPolicies.Publish
	}
	st, e := write(c.Request.Context(), id, *r.Revision)
	if e != nil {
		response.ErrorFrom(c, e)
		return
	}
	response.Success(c, st)
}
func (h *GroupHandler) PublishAIRouting(c *gin.Context) { h.setAIPublication(c, true) }
func (h *GroupHandler) DisableAIRouting(c *gin.Context) { h.setAIPublication(c, false) }
func (h *GroupHandler) PreviewAIRouting(c *gin.Context) {
	id, ok := h.aiGroupID(c)
	if !ok {
		return
	}
	r, ok := bindAIPolicyEdit(c, false, true)
	if !ok {
		return
	}
	if !r.Policy.AllowsEndpoint(r.SampleEndpoint) {
		response.BadRequest(c, "Sample endpoint is not enabled by this policy")
		return
	}
	req, e := service.NewAIRoutingSampleRequestForEndpoint(r.Policy, r.Sample, r.SampleEndpoint)
	if e != nil {
		if errors.Is(e, service.ErrAIRoutingBackgroundNotReleased) {
			response.ErrorFrom(c, e)
			return
		}
		response.BadRequest(c, "Invalid AI routing sample for "+r.SampleEndpoint)
		return
	}
	v, e := h.aiPolicies.PreviewRequest(c.Request.Context(), id, *r.Policy, req)
	if e != nil {
		response.ErrorFrom(c, e)
		return
	}
	response.Success(c, v)
}
func (h *GroupHandler) TestAIRouting(c *gin.Context) {
	id, ok := h.aiGroupID(c)
	if !ok {
		return
	}
	r, ok := bindAIPolicyEdit(c, false, true)
	if !ok {
		return
	}
	p := r.Policy
	p.GroupID = id
	ctx := c.Request.Context()
	if !p.AllowsEndpoint(r.SampleEndpoint) {
		response.BadRequest(c, "Sample endpoint is not enabled by this policy")
		return
	}
	req, e := service.NewAIRoutingSampleRequestForEndpoint(p, r.Sample, r.SampleEndpoint)
	if e != nil {
		if errors.Is(e, service.ErrAIRoutingBackgroundNotReleased) {
			response.ErrorFrom(c, e)
			return
		}
		response.BadRequest(c, "Invalid AI routing sample for "+r.SampleEndpoint)
		return
	}
	v, e := h.aiPolicies.PreviewRequest(ctx, id, *p, req)
	if e != nil {
		response.ErrorFrom(c, e)
		return
	}
	if len(v.Eligible) == 0 {
		response.BadRequest(c, "No eligible execution candidate")
		return
	}
	if h.aiDecider == nil {
		response.InternalError(c, "Decision engine unavailable")
		return
	}
	if e := h.aiPolicies.ValidateDecision(ctx, p); e != nil {
		response.ErrorFrom(c, e)
		return
	}
	allowed := map[string]bool{}
	for _, candidateID := range v.Eligible {
		allowed[candidateID] = true
	}
	candidates := make([]service.AIRoutingCandidate, 0, len(allowed))
	for _, candidate := range p.Candidates {
		if allowed[candidate.ID] {
			candidates = append(candidates, candidate)
		}
	}
	// Single-choice fast path mirrors runtime and discloses that no decision fee was incurred.
	if len(candidates) == 1 {
		response.Success(c, gin.H{"sample_endpoint": r.SampleEndpoint, "candidate_id": candidates[0].ID, "model": "", "protocol": "", "input_tokens": 0, "output_tokens": 0, "elapsed_ms": 0, "external_call": false, "reason": "single"})
		return
	}
	start := time.Now()
	chosen, result, reason, e := service.ChooseAIRoutingCandidate(service.WithAIRoutingAdminSample(ctx), p, req, candidates, h.aiDecider, c.Request.Host)
	if e != nil {
		chosen = service.AIRoutingCandidate{}
		if result.FailureReason == "" {
			result.FailureReason = "decision_error"
		}
	}
	response.Success(c, gin.H{"sample_endpoint": r.SampleEndpoint, "candidate_id": chosen.ID, "model": result.Model, "protocol": result.Protocol, "input_tokens": result.InputTokens, "output_tokens": result.OutputTokens, "elapsed_ms": time.Since(start).Milliseconds(), "external_call": result.DecisionCalls > 0 || (p.ContextMode != "bounded_history" && result.Model != ""), "reason": reason, "failure_reason": result.FailureReason, "context": result.Context, "decision_calls": result.DecisionCalls, "stages": result.Calls})
}
func (h *GroupHandler) AIRoutingModels(c *gin.Context) {
	id, ok := h.aiGroupID(c)
	if !ok {
		return
	}
	var decisionGroupID, decisionAccountID *int64
	for key, out := range map[string]**int64{"target_group_id": &decisionGroupID, "target_account_id": &decisionAccountID} {
		if raw := c.Query(key); raw != "" {
			v, e := strconv.ParseInt(raw, 10, 64)
			if e != nil || v <= 0 {
				response.BadRequest(c, "Invalid destination ID")
				return
			}
			*out = &v
		}
	}
	models, e := h.aiPolicies.LocalModels(c.Request.Context(), id, c.Query("platform"), decisionGroupID, decisionAccountID)
	if e != nil {
		response.ErrorFrom(c, e)
		return
	}
	response.Success(c, gin.H{"models": models})
}
