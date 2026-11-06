package service

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Capacity facts are per account. A source may combine provider and catalog
// metadata; this read-only view never probes an upstream or attests its limits.
type AIRoutingAccountCapacity struct {
	AccountID int64 `json:"account_id"`
	AIRoutingCapabilityCheck
}

type AIRoutingCandidateCapacity struct {
	CandidateID string                     `json:"candidate_id"`
	Model       string                     `json:"model"`
	Reason      string                     `json:"reason,omitempty"`
	Accounts    []AIRoutingAccountCapacity `json:"accounts"`
}

// NewAIRoutingSampleRequest builds the same request projection as runtime. A
// synthetic sample is never forwarded to an execution provider by preview/trial.
func NewAIRoutingSampleRequest(p *AIRoutingPolicy, sample string) (AIRoutingRequest, error) {
	return NewAIRoutingSampleRequestForEndpoint(p, sample, CompositeRouteEndpointMessages)
}

func NewAIRoutingSampleRequestForEndpoint(p *AIRoutingPolicy, sample, endpoint string) (AIRoutingRequest, error) {
	if endpoint == "" {
		endpoint = CompositeRouteEndpointMessages
	}
	if p == nil || !p.AllowsEndpoint(endpoint) {
		return AIRoutingRequest{}, aiPolicyError("Sample endpoint is not enabled")
	}
	if endpoint == CompositeRouteEndpointChatCompletions {
		var body []byte
		var err error
		if strings.HasPrefix(strings.TrimSpace(sample), "{") {
			body = []byte(sample)
			if !gjson.GetBytes(body, "model").Exists() {
				body, err = sjson.SetBytes(body, "model", "auto")
			}
		} else {
			body, err = json.Marshal(map[string]any{"model": "auto", "messages": []map[string]string{{"role": "user", "content": sample}}, "max_completion_tokens": 1024})
		}
		if err != nil {
			return AIRoutingRequest{}, aiPolicyError("Invalid Chat sample")
		}
		return NewAIRoutingChatRequest(body, p.ContextMode)
	}
	if endpoint == CompositeRouteEndpointGemini {
		body := []byte(sample)
		model := "auto"
		if !strings.HasPrefix(strings.TrimSpace(sample), "{") {
			body, _ = json.Marshal(map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": sample}}}}, "generationConfig": map[string]int{"maxOutputTokens": 1024}})
		} else if value := gjson.GetBytes(body, "model"); value.Exists() {
			model = strings.TrimPrefix(value.Str, "models/")
		}
		return NewAIRoutingGeminiRequest(body, model, "generateContent", p.ContextMode)
	}
	if endpoint == CompositeRouteEndpointResponses || endpoint == AIRoutingEndpointResponsesWS {
		var body []byte
		var err error
		if strings.HasPrefix(strings.TrimSpace(sample), "{") {
			body = []byte(sample)
			if !gjson.GetBytes(body, "model").Exists() {
				body, err = sjson.SetBytes(body, "model", "auto")
			}
		} else {
			body, err = json.Marshal(map[string]any{"model": "auto", "input": sample, "max_output_tokens": 1024, "store": false})
		}
		if err != nil {
			return AIRoutingRequest{}, aiPolicyError("Invalid Responses sample")
		}
		if gjson.GetBytes(body, "background").Type == gjson.True && !AIRoutingBackgroundReleased() {
			return AIRoutingRequest{}, ErrAIRoutingBackgroundNotReleased
		}
		if endpoint == AIRoutingEndpointResponsesWS {
			if !gjson.GetBytes(body, "type").Exists() {
				body, err = sjson.SetBytes(body, "type", "response.create")
			}
			if err != nil {
				return AIRoutingRequest{}, aiPolicyError("Invalid WebSocket sample")
			}
			return NewAIRoutingWSRequest(body, p.ContextMode)
		}
		return NewAIRoutingResponsesRequest(body, p.ContextMode)
	}
	var body []byte
	var err error
	if p.ContextMode == "bounded_history" && strings.HasPrefix(strings.TrimSpace(sample), "{") {
		body = []byte(sample)
		if !gjson.GetBytes(body, "model").Exists() {
			body, err = sjson.SetBytes(body, "model", "auto")
		}
		if err == nil && !gjson.GetBytes(body, "max_tokens").Exists() {
			body, err = sjson.SetBytes(body, "max_tokens", 1024)
		}
	} else {
		body, err = json.Marshal(map[string]any{"model": "auto", "max_tokens": 1024, "messages": []map[string]string{{"role": "user", "content": sample}}})
	}
	if err != nil {
		return AIRoutingRequest{}, aiPolicyError("Invalid Messages sample")
	}
	r, err := NewAIRoutingRequest(body, p.ContextMode)
	if err == nil && p.ContextMode != "bounded_history" {
		r.Task = AIRoutingSampleTask(p.ContextMode, sample)
	}
	return r, err
}

// PreviewRequest adds actual sample constraints and disclosure planning without
// ever invoking a decision engine. Publication uses Preview's configuration
// checks; it must not infer a real task from an empty example.
func (s *AIRoutingPolicyService) PreviewRequest(ctx context.Context, id int64, p AIRoutingPolicy, request AIRoutingRequest) (*AIRoutingPreview, error) {
	if request.Background && !AIRoutingBackgroundReleased() {
		return nil, ErrAIRoutingBackgroundNotReleased
	}
	endpoint := request.ClientEndpoint()
	out, err := s.PreviewForEndpoint(ctx, id, p, endpoint)
	if err != nil || (p.ContextMode != "bounded_history" && endpoint != CompositeRouteEndpointResponses && endpoint != CompositeRouteEndpointChatCompletions && endpoint != AIRoutingEndpointResponsesWS && endpoint != CompositeRouteEndpointGemini) {
		return out, err
	}
	p.GroupID = id
	out.CapacityChecks = s.previewCapacityChecks(ctx, &p, request)
	if len(request.ToolResults) > 0 || request.RequiresOrigin || len(request.OriginRefs) > 0 {
		for _, candidateID := range out.Eligible {
			out.Excluded = append(out.Excluded, AIRoutingExclusion{ID: candidateID, Reason: "sample_origin_binding_required"})
		}
		out.Eligible = []string{}
		out.Context = &AIRoutingHistoryDiagnostics{ProjectionVersion: aiRoutingRequestHistoryVersion(request), Source: "request", Coverage: "none", FailureReason: "sample_origin_binding_required"}
		return out, nil
	}
	group, err := s.groups.GetByIDLite(ctx, id)
	if err != nil {
		return nil, err
	}
	key := &APIKey{Group: group, GroupID: &group.ID}
	eligible := s.gateway.EligibleAIRoutingCandidates(ctx, key, &p, request, endpoint, 0)
	ids := make(map[string]bool, len(eligible))
	for _, candidate := range eligible {
		ids[candidate.ID] = true
	}
	kept := make([]string, 0, len(out.Eligible))
	for _, id := range out.Eligible {
		if ids[id] {
			kept = append(kept, id)
		} else {
			reason := "sample capability, context window or account constraints are not satisfied"
			for _, candidate := range p.Candidates {
				if candidate.ID == id {
					if issue := s.candidateCapacityIssue(ctx, p.GroupID, candidate, request); issue != "" {
						reason = issue
					}
					break
				}
			}
			out.Excluded = append(out.Excluded, AIRoutingExclusion{ID: id, Reason: reason})
		}
	}
	out.Eligible = kept
	out.NeedsDecision = len(kept) > 1
	if !out.NeedsDecision || p.ContextMode != "bounded_history" {
		return out, nil
	}
	limits, reserved, err := aiHistoryPlanningLimits(&p, eligible)
	if err != nil {
		out.Context = previewHistoryFailure(err)
		out.Context.ProjectionVersion = aiRoutingRequestHistoryVersion(request)
		return out, nil
	}
	history, err := ParseAIRoutingRequestHistory(request, p.ContextBudget)
	if err != nil {
		out.Context = previewHistoryFailure(err)
		out.Context.ProjectionVersion = aiRoutingRequestHistoryVersion(request)
		return out, nil
	}
	out.SampleSpans = history.Spans
	prepared, err := BuildAIRoutingHistoryTask(ctx, history, limits, reserved, nil, aiHistoryWireSizer(&p, eligible))
	out.Context = &prepared.Diagnostics
	if err != nil && prepared.Diagnostics.FailureReason == "history_classifier_unavailable" {
		// Expected in an offline preview; semantic relevance needs an explicit
		// trial. Never supply an invented selection to make this look complete.
		out.Context.FailureReason = ""
	}
	return out, nil
}

func previewHistoryFailure(err error) *AIRoutingHistoryDiagnostics {
	return &AIRoutingHistoryDiagnostics{ProjectionVersion: AIRoutingHistoryProjectionVersion, Source: "request", Coverage: "none", FailureReason: aiHistoryErrorReason(err)}
}

func (s *AIRoutingPolicyService) candidateCapacityIssue(ctx context.Context, groupID int64, candidate AIRoutingCandidate, request AIRoutingRequest) string {
	ctx = WithCompositeRouteDecision(ctx, candidate.executionDecision(groupID, request.PublicModel, request.ClientEndpoint(), 0))
	accounts, handled, err := ListCompositeRouteScopedAccounts(ctx, s.groups, s.accounts, candidate.Platform)
	if !handled || err != nil {
		return "destination unavailable"
	}
	ctx = WithAIRoutingExecutionRequirements(ctx, AIRoutingRequirementsForCandidate(request, candidate))
	reason := "no eligible account for this model and endpoint"
	for i := range accounts {
		check := CheckAIRoutingExecutionAccount(ctx, &accounts[i], candidate.Model)
		if check.Allowed {
			return ""
		}
		if check.Reason != "" {
			reason = check.Reason
		}
	}
	return reason
}

func (s *AIRoutingPolicyService) previewCapacityChecks(ctx context.Context, policy *AIRoutingPolicy, request AIRoutingRequest) []AIRoutingCandidateCapacity {
	rows := make([]AIRoutingCandidateCapacity, 0, len(policy.Candidates))
	for _, candidate := range policy.Candidates {
		row := AIRoutingCandidateCapacity{CandidateID: candidate.ID, Model: candidate.Model, Accounts: []AIRoutingAccountCapacity{}}
		if !candidate.Enabled {
			row.Reason = "candidate_disabled"
			rows = append(rows, row)
			continue
		}
		scoped := WithCompositeRouteDecision(ctx, candidate.executionDecision(policy.GroupID, request.PublicModel, request.ClientEndpoint(), 0))
		accounts, handled, err := ListCompositeRouteScopedAccounts(scoped, s.groups, s.accounts, candidate.Platform)
		if !handled || err != nil || len(accounts) == 0 {
			row.Reason = "destination_unavailable"
		} else {
			scoped = WithAIRoutingExecutionRequirements(scoped, AIRoutingRequirementsForCandidate(request, candidate))
			for i := range accounts {
				row.Accounts = append(row.Accounts, AIRoutingAccountCapacity{AccountID: accounts[i].ID, AIRoutingCapabilityCheck: CheckAIRoutingExecutionAccount(scoped, &accounts[i], candidate.Model)})
			}
		}
		rows = append(rows, row)
	}
	return rows
}
