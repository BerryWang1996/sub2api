package service

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	errs "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

type AIRoutingPolicy struct {
	GroupID           int64                `json:"group_id"`
	Enabled           bool                 `json:"enabled"`
	Version           int64                `json:"version"`
	Engine            string               `json:"engine"`
	DecisionGroupID   *int64               `json:"decision_group_id"`
	DecisionAccountID *int64               `json:"decision_account_id"`
	DecisionPlatform  string               `json:"decision_platform"`
	DecisionModel     string               `json:"decision_model"`
	Prompt            string               `json:"prompt"`
	ContextMode       string               `json:"context_mode"`
	TimeoutMS         int                  `json:"timeout_ms"`
	FallbackID        string               `json:"fallback_id"`
	Candidates        []AIRoutingCandidate `json:"candidates"`
}
type AIRoutingCandidate struct {
	ID              string `json:"id"`
	Platform        string `json:"platform"`
	Model           string `json:"model"`
	Description     string `json:"description"`
	CostTier        string `json:"cost_tier,omitempty"`
	MaxInputTokens  int    `json:"max_input_tokens,omitempty"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
	DisableTools    bool   `json:"disable_tools,omitempty"`
	DisableImages   bool   `json:"disable_images,omitempty"`
	TargetGroupID   *int64 `json:"target_group_id"`
	TargetAccountID *int64 `json:"target_account_id"`
	Enabled         bool   `json:"enabled"`
}
type AIRoutingPolicyState struct {
	Draft     AIRoutingPolicy  `json:"draft"`
	Published *AIRoutingPolicy `json:"published"`
	Revision  int64            `json:"revision"`
}

// A nil state identifies a static group. A new AI group without a policy row
// has revision zero and is explicitly disabled until its first publication.
type AIRoutingPolicyRepository interface {
	Load(context.Context, int64) (*AIRoutingPolicyState, error)
	Published(context.Context, int64) (*AIRoutingPolicy, error)
	Write(context.Context, int64, int64, AIRoutingPolicy, *AIRoutingPolicy, string) (*AIRoutingPolicyState, error)
}

var ErrAIRevision = errs.Conflict("AI_ROUTING_REVISION_CONFLICT", "Policy was edited by another admin; reload and retry")
var ErrNotAIGroup = errs.BadRequest("NOT_AI_ROUTING_GROUP", "Group does not use AI routing")

func NewAIRoutingDraft(id int64) AIRoutingPolicy {
	return AIRoutingPolicy{GroupID: id, Engine: "generic", ContextMode: "metadata", TimeoutMS: 3000, Candidates: []AIRoutingCandidate{}}
}

type AIRoutingPolicyService struct {
	repo     AIRoutingPolicyRepository
	groups   GroupRepository
	accounts AccountRepository
	gateway  *GatewayService
	cache    APIKeyAuthCacheInvalidator
}

func NewAIRoutingPolicyService(repo AIRoutingPolicyRepository, groups GroupRepository, accounts AccountRepository, gateway *GatewayService, cache APIKeyAuthCacheInvalidator) *AIRoutingPolicyService {
	return &AIRoutingPolicyService{repo, groups, accounts, gateway, cache}
}
func (s *AIRoutingPolicyService) writePublication(ctx context.Context, id, rev int64, draft AIRoutingPolicy, published *AIRoutingPolicy, status string) (*AIRoutingPolicyState, error) {
	st, e := s.repo.Write(ctx, id, rev, draft, published, status)
	if e == nil && s.cache != nil {
		s.cache.InvalidateAuthCacheByGroupID(ctx, id)
	}
	return st, e
}
func (s *AIRoutingPolicyService) GetPublished(ctx context.Context, id int64) (*AIRoutingPolicy, error) {
	return s.repo.Published(ctx, id)
}
func (s *AIRoutingPolicyService) Get(ctx context.Context, id int64) (*AIRoutingPolicyState, error) {
	st, e := s.repo.Load(ctx, id)
	if e == nil && st == nil {
		return nil, ErrNotAIGroup
	}
	return st, e
}

var aiChoiceID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

func aiPolicyError(msg string) error { return errs.BadRequest("INVALID_AI_ROUTING_POLICY", msg) }

// AIRoutingSampleTask applies exactly the disclosure level chosen in the
// admin form to a manually submitted sample. Metadata has no content excerpt.
func AIRoutingSampleTask(mode, sample string) string {
	meta := fmt.Sprintf("sample_characters=%d", utf8.RuneCountInString(sample))
	if mode == "task_excerpt" {
		clean := []rune(boundedTask(redactContentModerationSecrets(sample)))
		if len(clean) > 1024 {
			clean = clean[:1024]
		}
		return meta + "\nsample_excerpt=" + string(clean)
	}
	return meta
}
func (s *AIRoutingPolicyService) LocalModels(ctx context.Context, id int64, platform string, groupID, accountID *int64) ([]string, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	if (groupID == nil) == (accountID == nil) || !isConcreteRequestPlatform(platform) {
		return nil, aiPolicyError("Select one concrete account or group")
	}
	if err := validateCompositeRouteDestination(ctx, s.groups, s.accounts, CompositeModelRoute{GroupID: id, TargetPlatform: platform, TargetGroupID: groupID, TargetAccountID: accountID}); err != nil {
		return nil, err
	}
	var accounts []Account
	if accountID != nil {
		a, e := s.accounts.GetByID(ctx, *accountID)
		if e != nil {
			return nil, e
		}
		if a == nil {
			return nil, aiPolicyError("Account unavailable")
		}
		accounts = []Account{*a}
	} else {
		var e error
		accounts, e = s.accounts.ListSchedulableByGroupIDAndPlatform(ctx, *groupID, platform)
		if e != nil {
			return nil, e
		}
	}
	seen := map[string]bool{}
	for i := range accounts {
		for model := range accounts[i].GetModelMapping() {
			if model != "" && !strings.Contains(model, "*") {
				seen[model] = true
			}
		}
	}
	models := make([]string, 0, len(seen))
	for m := range seen {
		models = append(models, m)
	}
	sort.Strings(models)
	return models, nil
}
func validateAIRoutingDraft(p AIRoutingPolicy) error {
	if p.Engine != "generic" && p.Engine != "jev" {
		return aiPolicyError("Unknown decision engine")
	}
	if p.ContextMode != "metadata" && p.ContextMode != "task_excerpt" {
		return aiPolicyError("Unknown context mode")
	}
	if p.TimeoutMS < 100 || p.TimeoutMS > 60000 {
		return aiPolicyError("Decision timeout must be 100–60000 ms")
	}
	if !utf8.ValidString(p.Prompt) || len(p.Prompt) > 16384 || len(p.Candidates) > 64 || len(p.DecisionModel) > 256 || len(p.FallbackID) > 32 || len(p.DecisionPlatform) > 64 {
		return aiPolicyError("Policy exceeds size limit")
	}
	if err := validateCompositeRouteDestinationIDs(p.GroupID, p.DecisionGroupID, p.DecisionAccountID); err != nil {
		return err
	}
	seen := map[string]bool{}
	destinations := map[string]bool{}
	for _, c := range p.Candidates {
		if c.MaxInputTokens < 0 || c.MaxOutputTokens < 0 {
			return aiPolicyError("Candidate token limits must be nonnegative")
		}
		if !aiChoiceID.MatchString(c.ID) || seen[c.ID] {
			return aiPolicyError("Choice IDs must be unique short identifiers")
		}
		seen[c.ID] = true
		if len(c.Description) > 2048 || len(c.Model) > 256 || len(c.CostTier) > 64 || len(c.Platform) > 64 {
			return aiPolicyError("Candidate fields exceed size limit")
		}
		if err := validateCompositeRouteDestinationIDs(p.GroupID, c.TargetGroupID, c.TargetAccountID); err != nil {
			return err
		}
		var dst string
		if c.TargetGroupID != nil {
			dst = fmt.Sprintf("g%d", *c.TargetGroupID)
		} else if c.TargetAccountID != nil {
			dst = fmt.Sprintf("a%d", *c.TargetAccountID)
		}
		if dst != "" && c.Model != "" {
			k := c.Platform + "\x00" + c.Model + "\x00" + dst
			if destinations[k] {
				return aiPolicyError("Duplicate candidate destination and model")
			}
			destinations[k] = true
		}
	}
	return nil
}
func (s *AIRoutingPolicyService) SaveDraft(ctx context.Context, id, rev int64, p AIRoutingPolicy) (*AIRoutingPolicyState, error) {
	st, e := s.Get(ctx, id)
	if e != nil {
		return nil, e
	}
	if rev != st.Revision {
		return nil, ErrAIRevision
	}
	p.GroupID = id
	p.Enabled = false
	p.Version = rev + 1
	if e := validateAIRoutingDraft(p); e != nil {
		return nil, e
	}
	return s.repo.Write(ctx, id, rev, p, st.Published, "")
}

type AIRoutingExclusion struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}
type AIRoutingPreview struct {
	Eligible     []string             `json:"eligible"`
	Excluded     []AIRoutingExclusion `json:"excluded"`
	ExternalCall bool                 `json:"external_call"`
}

func (s *AIRoutingPolicyService) Preview(ctx context.Context, id int64, p AIRoutingPolicy) (*AIRoutingPreview, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	p.GroupID = id
	if err := validateAIRoutingDraft(p); err != nil {
		return nil, err
	}
	group, err := s.groups.GetByIDLite(ctx, id)
	if err != nil {
		return nil, err
	}
	out := &AIRoutingPreview{Eligible: []string{}, Excluded: []AIRoutingExclusion{}}
	for _, c := range p.Candidates {
		reason := s.candidateIssue(ctx, group, c)
		if reason != "" {
			out.Excluded = append(out.Excluded, AIRoutingExclusion{c.ID, reason})
		} else {
			out.Eligible = append(out.Eligible, c.ID)
		}
	}
	return out, nil
}
func (s *AIRoutingPolicyService) candidateIssue(ctx context.Context, group *Group, c AIRoutingCandidate) string {
	if !c.Enabled {
		return "disabled"
	}
	if c.Platform == PlatformOpenAI && !group.AllowMessagesDispatch {
		return "enable Messages dispatch on the source group to execute OpenAI models"
	}
	if strings.TrimSpace(c.Model) == "" || !aiRoutingModelProducesText(c.Model) || !isConcreteRequestPlatform(c.Platform) || c.Platform == PlatformTypeSafe || (c.TargetGroupID == nil) == (c.TargetAccountID == nil) {
		return "requires a concrete text model and one destination"
	}
	if err := validateCompositeRouteDestination(ctx, s.groups, s.accounts, CompositeModelRoute{GroupID: group.ID, TargetPlatform: c.Platform, TargetAccountID: c.TargetAccountID, TargetGroupID: c.TargetGroupID}); err != nil {
		return "destination unavailable or platform mismatch"
	}
	key := &APIKey{Group: group, GroupID: &group.ID}
	if s.gateway == nil {
		return "execution gateway unavailable"
	}
	if priced, _ := s.gateway.hasIdentifiedResponseModelPricing(ctx, c.Model, key); !priced {
		return "pricing unavailable"
	}
	// No real client request or key is sent during a configuration preview.
	// Use the runtime endpoint constraints and a minimal text-only request.
	if len(s.gateway.EligibleAIRoutingCandidates(ctx, key, &AIRoutingPolicy{GroupID: group.ID, Candidates: []AIRoutingCandidate{c}}, AIRoutingRequest{PublicModel: c.Model, MaxOutput: 1}, CompositeRouteEndpointMessages, 0)) == 0 {
		return "no eligible account for this model and endpoint"
	}
	return ""
}
func (s *AIRoutingPolicyService) ValidateDecision(ctx context.Context, p *AIRoutingPolicy) error {
	if strings.TrimSpace(p.DecisionModel) == "" || (p.DecisionGroupID == nil) == (p.DecisionAccountID == nil) {
		return aiPolicyError("Choose one decision account/group and a decision model")
	}
	if !isConcreteRequestPlatform(p.DecisionPlatform) || (p.Engine == "jev") != (p.DecisionPlatform == PlatformTypeSafe) {
		return aiPolicyError("Decision platform does not match engine")
	}
	if p.DecisionAccountID != nil {
		a, e := s.accounts.GetByID(ctx, *p.DecisionAccountID)
		if e != nil || a == nil || a.Platform != p.DecisionPlatform || !a.IsSchedulable() {
			return aiPolicyError("Decision account is unavailable")
		}
		if _, e := AIRoutingDecisionProtocol(p.Engine, a, p.DecisionModel); e != nil {
			return aiPolicyError("Unsupported decision account/model combination")
		}
		return nil
	}
	g, e := s.groups.GetByIDLite(ctx, *p.DecisionGroupID)
	if e != nil || g == nil || !g.IsActive() || g.Platform != p.DecisionPlatform || g.ID == p.GroupID {
		return aiPolicyError("Decision group must be a live concrete group")
	}
	accounts, e := s.accounts.ListSchedulableByGroupIDAndPlatform(ctx, g.ID, g.Platform)
	if e != nil {
		return e
	}
	for i := range accounts {
		if _, e := AIRoutingDecisionProtocol(p.Engine, &accounts[i], p.DecisionModel); e == nil {
			return nil
		}
	}
	return aiPolicyError("Decision group has no eligible account for this model")
}
func (s *AIRoutingPolicyService) Publish(ctx context.Context, id, rev int64) (*AIRoutingPolicyState, error) {
	st, e := s.Get(ctx, id)
	if e != nil {
		return nil, e
	}
	if rev != st.Revision {
		return nil, ErrAIRevision
	}
	p := st.Draft
	if e = s.ValidateDecision(ctx, &p); e != nil {
		return nil, e
	}
	v, e := s.Preview(ctx, id, p)
	if e != nil {
		return nil, e
	}
	if len(v.Eligible) == 0 {
		return nil, aiPolicyError("No eligible execution candidates")
	}
	for _, ex := range v.Excluded {
		if ex.Reason != "disabled" {
			return nil, aiPolicyError(ex.ID + ": " + ex.Reason)
		}
	}
	if p.FallbackID != "" {
		found := false
		for _, id := range v.Eligible {
			if id == p.FallbackID {
				found = true
			}
		}
		if !found {
			return nil, aiPolicyError("Fallback is not eligible")
		}
	}
	p.Enabled = true
	p.Version = rev + 1
	return s.writePublication(ctx, id, rev, st.Draft, &p, StatusActive)
}
func (s *AIRoutingPolicyService) Disable(ctx context.Context, id, rev int64) (*AIRoutingPolicyState, error) {
	st, e := s.Get(ctx, id)
	if e != nil {
		return nil, e
	}
	if rev != st.Revision {
		return nil, ErrAIRevision
	}
	if st.Published != nil {
		st.Published.Enabled = false
		st.Published.Version = rev + 1
	}
	return s.writePublication(ctx, id, rev, st.Draft, st.Published, "inactive")
}
