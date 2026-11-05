package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/tidwall/gjson"
)

const CompositeRouteSourceAI = "ai"

var ErrNoAIRoutingCandidate = errors.New("no eligible AI routing candidate")

type AIRoutingRequest struct {
	PublicModel          string
	UsesTools, HasImages bool
	InputBytes           int
	MaxOutput            int
	ToolResults          []string
	Task                 string
}

var aiSecret = regexp.MustCompile(`(?i)(bearer\s+\S+|(?:sk-|ts-|ghp_|github_pat_|AKIA)[a-z0-9_-]{12,}|(?:api[_-]?key|access[_-]?token|password|secret)\s*[:=]\s*["']?[^\s"']+)`)

func boundedTask(s string) string {
	if strings.Contains(s, "PRIVATE KEY-----") {
		return "[redacted]"
	}
	if len(s) > 2048 {
		s = s[:2048]
		for len(s) > 0 && !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return aiSecret.ReplaceAllString(redactContentModerationSecrets(s), "[redacted]")
}

// Parse only Messages user text for optional egress. Tool payloads, attachments,
// assistant history and system prompts are never included in the decision task.
func NewAIRoutingRequest(body []byte, mode string) (AIRoutingRequest, error) {
	var req AIRoutingRequest
	if !requestmodel.HasUniqueJSONMembers(body) {
		return req, errors.New("invalid JSON")
	}
	root := gjson.ParseBytes(body)
	model := root.Get("model")
	if !aiCanonicalFields(root, "model", "messages", "tools", "max_tokens") || model.Type != gjson.String || strings.TrimSpace(model.Str) == "" || len(model.Str) > 256 {
		return req, errors.New("invalid messages request or model")
	}
	messages := root.Get("messages")
	if !messages.IsArray() || messages.Get("#").Int() == 0 {
		return req, errors.New("messages must be a nonempty array")
	}
	req.PublicModel = model.Str
	req.InputBytes = len(body)
	req.MaxOutput = int(root.Get("max_tokens").Int())
	req.UsesTools = root.Get("tools.#").Int() > 0
	var latestText strings.Builder
	var lastRole string
	bad := false
	turns := 0
	messages.ForEach(func(_, msg gjson.Result) bool {
		if !aiCanonicalFields(msg, "role", "content") {
			bad = true
			return false
		}
		turns++
		previousRole := lastRole
		lastRole = msg.Get("role").Str
		if lastRole != "assistant" && lastRole != "user" {
			bad = true
			return false
		}
		// Adjacent user messages form one turn in Messages. An assistant
		// prefill after tool results must keep the provenance of that turn.
		if lastRole == "user" && previousRole != "user" {
			req.ToolResults = nil
			latestText.Reset()
		}
		content := msg.Get("content")
		if content.Type == gjson.String {
			if lastRole == "user" {
				appendAITaskText(&latestText, content.Str)
			}
			return true
		}
		if !content.IsArray() {
			bad = true
			return false
		}
		content.ForEach(func(_, b gjson.Result) bool {
			if !aiCanonicalFields(b, "type", "text", "tool_use_id", "content") {
				bad = true
				return false
			}
			switch b.Get("type").Str {
			case "text":
				if lastRole == "user" {
					appendAITaskText(&latestText, b.Get("text").Str)
				}
			case "image", "document":
				req.HasImages = true
			case "tool_use":
				req.UsesTools = true
			case "tool_result":
				if lastRole != "user" {
					bad = true
					return false
				}
				req.UsesTools = true
				id := b.Get("tool_use_id").Str
				if id == "" || len(id) > 256 || len(req.ToolResults) >= 64 {
					bad = true
					return false
				}
				req.ToolResults = append(req.ToolResults, id)
				resultContent := b.Get("content")
				if resultContent.Type == gjson.String || !resultContent.Exists() {
					break
				}
				if !resultContent.IsArray() {
					bad = true
					return false
				}
				resultContent.ForEach(func(_, part gjson.Result) bool {
					if !aiCanonicalFields(part, "type") {
						bad = true
						return false
					}
					if t := part.Get("type").Str; t == "image" || t == "document" {
						req.HasImages = true
					}
					return true
				})
			}
			return true
		})
		return !bad
	})
	if bad {
		return req, errors.New("invalid message content")
	}
	state := struct {
		Turns   int    `json:"turns"`
		Bytes   int    `json:"request_bytes"`
		Tools   bool   `json:"tools"`
		Images  bool   `json:"attachments"`
		Output  int    `json:"max_output"`
		Excerpt string `json:"user_task,omitempty"`
	}{Turns: turns, Bytes: len(body), Tools: req.UsesTools, Images: req.HasImages, Output: req.MaxOutput}
	if mode == "task_excerpt" {
		state.Excerpt = boundedTask(latestText.String())
	}
	encoded, _ := json.Marshal(state)
	req.Task = string(encoded)
	return req, nil
}

// Go's JSON struct decoder folds field names; gjson does not. Reject ambiguous
// protocol fields, while preserving arbitrary case-sensitive tool input keys.
func aiCanonicalFields(object gjson.Result, fields ...string) bool {
	if !object.IsObject() {
		return false
	}
	valid := true
	object.ForEach(func(key, _ gjson.Result) bool {
		for _, field := range fields {
			if key.Str != field && strings.EqualFold(key.Str, field) {
				valid = false
				return false
			}
		}
		return true
	})
	return valid
}

func appendAITaskText(task *strings.Builder, text string) {
	// Redact each complete block before bounding it, including private-key
	// markers beyond the excerpt limit. Keep one budget for the whole user turn.
	if task.Len() >= 2048 || text == "" {
		return
	}
	if task.Len() > 0 {
		_ = task.WriteByte('\n')
	}
	text = boundedTask(text)
	if remaining := 2048 - task.Len(); len(text) > remaining {
		text = text[:remaining]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
	}
	_, _ = task.WriteString(text)
}

func (c AIRoutingCandidate) executionDecision(groupID int64, alias, endpoint string, bound int64) CompositeRouteDecision {
	return CompositeRouteDecision{Matched: true, Source: CompositeRouteSourceAI, AICandidateID: c.ID, GroupID: groupID, PublicModel: alias, Endpoint: endpoint, TargetPlatform: c.Platform, UpstreamModel: c.Model, TargetGroupID: c.TargetGroupID, TargetAccountID: c.TargetAccountID, BoundAccountID: bound}
}

func (c AIRoutingCandidate) supportsAIRequest(r AIRoutingRequest) bool {
	// Byte length is a conservative bound when a candidate has a hard input
	// limit and no local tokenizer; do not estimate a multimodal token budget.
	return c.Enabled && (!r.UsesTools || !c.DisableTools) && (!r.HasImages || !c.DisableImages) &&
		(c.MaxInputTokens == 0 || (!r.HasImages && r.InputBytes <= c.MaxInputTokens)) &&
		(c.MaxOutputTokens == 0 || (r.MaxOutput > 0 && r.MaxOutput <= c.MaxOutputTokens))
}

func (s *GatewayService) EligibleAIRoutingCandidates(ctx context.Context, key *APIKey, p *AIRoutingPolicy, r AIRoutingRequest, endpoint string, bound int64) []AIRoutingCandidate {
	if key == nil || key.Group == nil || p == nil || key.Group.ID != p.GroupID {
		return nil
	}
	candidates := make([]AIRoutingCandidate, 0, len(p.Candidates))
	for _, c := range p.Candidates {
		if c.Platform == PlatformOpenAI && !key.Group.AllowMessagesDispatch {
			continue
		}
		if !c.supportsAIRequest(r) || !aiRoutingModelProducesText(c.Model) || !isConcreteRequestPlatform(c.Platform) || c.Platform == PlatformTypeSafe {
			continue
		}
		if c.Platform != PlatformAnthropic && c.Platform != PlatformAntigravity && c.Platform != PlatformGemini && !domain.UsesOpenAIGateway(c.Platform) {
			continue
		}
		if (c.TargetGroupID == nil) == (c.TargetAccountID == nil) {
			continue
		}
		d := c.executionDecision(p.GroupID, r.PublicModel, endpoint, bound)
		scoped := WithCompositeRouteDecision(ctx, d)
		// Token requests use the source group's billing multiplier when the
		// destination pool has an active account profit threshold.
		if endpoint == CompositeRouteEndpointMessages && c.TargetGroupID != nil {
			scoped, _ = WithGatewayTokenRequestPricing(s.withGroupContext(scoped, key.Group))
			scoped = s.withGatewayProfitControlGate(scoped, c.TargetGroupID)
		}
		if mapped, restricted := s.ResolveChannelMappingAndRestrict(scoped, key.GroupID, c.Model); restricted || mapped.MappedModel != "" && mapped.MappedModel != c.Model {
			continue
		}
		if s.IsModelRestricted(scoped, p.GroupID, c.Model) {
			continue
		}
		if ok, _ := s.hasIdentifiedResponseModelPricing(scoped, c.Model, key); !ok {
			continue
		}
		accounts, handled, err := ListCompositeRouteScopedAccounts(scoped, s.groupRepo, s.accountRepo, c.Platform)
		if !handled || err != nil {
			continue
		}
		for i := range accounts {
			a := &accounts[i]
			if !a.IsSchedulable() || !CompositeRouteAccountAllowed(scoped, a) || a.isModelRateLimitedWithContext(scoped, c.Model) || !s.isGatewayAccountProfitEligible(scoped, a) {
				continue
			}
			if domain.UsesOpenAIGateway(c.Platform) && !isOpenAICompatibleAccountEligibleForRequest(scoped, a, c.Platform, c.Model, false, OpenAIEndpointCapabilityChatCompletions) {
				continue
			}
			if key.Group.RequirePrivacySet && !a.IsPrivacySet() {
				continue
			}
			if key.Group.RequireOAuthOnly && a.Type == AccountTypeAPIKey {
				continue
			}
			candidates = append(candidates, c)
			break
		}
	}
	return candidates
}

type AIModelChooser interface {
	Decide(context.Context, *AIRoutingPolicy, string, []AIRoutingCandidate, string) (AIRoutingDecisionResult, error)
}

func ChooseAIRoutingCandidate(ctx context.Context, p *AIRoutingPolicy, r AIRoutingRequest, eligible []AIRoutingCandidate, chooser AIModelChooser, host string) (AIRoutingCandidate, AIRoutingDecisionResult, string, error) {
	var empty AIRoutingCandidate
	var result AIRoutingDecisionResult
	if len(eligible) == 0 {
		return empty, result, "empty", ErrNoAIRoutingCandidate
	}
	if len(eligible) == 1 {
		return eligible[0], result, "single", nil
	}
	var err error
	if chooser != nil {
		result, err = chooser.Decide(ctx, p, r.Task, eligible, host)
	} else {
		err = errors.New("decision service unavailable")
	}
	if ctx.Err() != nil {
		return empty, result, "cancelled", ctx.Err()
	}
	if err == nil {
		for _, candidate := range eligible {
			if candidate.ID == result.CandidateID {
				return candidate, result, "model", nil
			}
		}
	}
	if result.FailureReason == "" {
		switch {
		case chooser == nil:
			result.FailureReason = "unavailable"
		case errors.Is(err, context.DeadlineExceeded):
			result.FailureReason = "timeout"
		case err == nil:
			result.FailureReason = "invalid_choice"
		default:
			result.FailureReason = "decision_error"
		}
	}
	for _, candidate := range eligible {
		if candidate.ID == p.FallbackID {
			return candidate, result, "fallback", nil
		}
	}
	return empty, result, "failed", fmt.Errorf("decision failed without eligible fallback")
}
