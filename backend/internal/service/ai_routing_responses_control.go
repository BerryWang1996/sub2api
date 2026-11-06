package service

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// ValidateAIRoutingResponsesOrigin authorizes an existing resource. It does not
// choose a model, reserve generation budget, or rely on response affinity caches.
func (s *GatewayService) ValidateAIRoutingResponsesOrigin(ctx context.Context, key *APIKey, policy *AIRoutingPolicy, origin *AIRoutingOriginBinding) (*Account, error) {
	deny := errors.New("AI response origin is no longer authorized")
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.groupRepo == nil || s.accountRepo == nil || key == nil || key.ID <= 0 || key.UserID <= 0 || key.Group == nil || policy == nil || origin == nil ||
		!origin.Valid() || !origin.Native || !origin.Store || origin.PublicModel == "" || !policy.Enabled ||
		policy.GroupID != key.Group.ID || !policy.AllowsEndpoint(CompositeRouteEndpointResponses) {
		return nil, deny
	}
	source, err := s.groupRepo.GetByID(ctx, policy.GroupID)
	if err != nil || source == nil || source.ID != policy.GroupID || !source.AIRouting || source.Platform != PlatformComposite || !source.IsActive() || !source.ModelAllowlist.Allows(origin.PublicModel) {
		return nil, deny
	}
	var candidate *AIRoutingCandidate
	for i := range policy.Candidates {
		c := &policy.Candidates[i]
		if !c.Enabled || c.ID != origin.Candidate || c.Model != origin.Model || c.Platform != origin.Platform {
			continue
		}
		groupTarget, accountTarget := int64(0), int64(0)
		if c.TargetGroupID != nil {
			groupTarget = *c.TargetGroupID
		}
		if c.TargetAccountID != nil {
			accountTarget = *c.TargetAccountID
		}
		if groupTarget == origin.GroupTarget && accountTarget == origin.AccountTarget {
			candidate = c
			break
		}
	}
	if candidate == nil {
		return nil, deny
	}
	if origin.GroupTarget != 0 {
		target, err := s.groupRepo.GetByID(ctx, origin.GroupTarget)
		if err != nil || target == nil || target.ID != origin.GroupTarget || !target.IsActive() || target.ID == source.ID || target.Platform != origin.Platform || target.Platform == PlatformComposite {
			return nil, deny
		}
	}
	account, err := s.accountRepo.GetByID(ctx, origin.Account)
	if err != nil || account == nil || account.ID != origin.Account || !account.IsSchedulable() || !AIRoutingAccountSupportsStoredResponses(account, origin.Model) ||
		source.RequireOAuthOnly || source.RequirePrivacySet && !account.IsPrivacySet() {
		return nil, deny
	}
	scoped := WithCompositeRouteDecision(ctx, candidate.executionDecision(source.ID, origin.PublicModel, CompositeRouteEndpointResponses, origin.Account))
	if !CompositeRouteAccountAllowed(scoped, account) {
		return nil, deny
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return account, nil
}

func validAIResponsesControlID(id string) bool {
	if !ValidAIRoutingOriginRef(AIRoutingOriginRef{Kind: "response", ID: id}) || strings.ContainsAny(id, "/\\?#%") || id == "." || id == ".." {
		return false
	}
	for _, char := range id {
		if char <= 0x20 || char == 0x7f {
			return false
		}
	}
	return true
}

// ForwardAIRoutingResponsesControl uses the original account and never retries
// or falls back to another account. Native create support does not prove CRUD
// support on a custom provider; its status and error response remain intact.
func (s *OpenAIGatewayService) ForwardAIRoutingResponsesControl(ctx context.Context, account *Account, model, method, responseID, operation, rawQuery string) (*http.Response, error) {
	if s == nil || s.httpUpstream == nil || !AIRoutingAccountSupportsStoredResponses(account, model) || !validAIResponsesControlID(responseID) {
		return nil, errors.New("unsupported AI response control destination")
	}
	if method == http.MethodGet && (operation == "" || operation == "input_items") || method == http.MethodDelete && operation == "" || method == http.MethodPost && operation == "cancel" {
		// Only these exact method/path pairs are resource operations.
	} else {
		return nil, errors.New("unsupported AI response control operation")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	baseURL := account.GetOpenAIBaseURL()
	if baseURL == "" {
		baseURL = "https://api.openai.com"
	}
	baseURL, err := s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	target, err := url.Parse(buildOpenAIResponsesURLForPlatform(account.Platform, baseURL))
	if err != nil {
		return nil, err
	}
	target.Path = strings.TrimRight(target.Path, "/") + "/" + responseID
	target.RawPath = ""
	if operation != "" {
		target.Path += "/" + operation
	}
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return nil, errors.New("invalid AI response control query")
	}
	target.RawQuery = query.Encode()
	target.Fragment = ""
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	ctx = WithHTTPUpstreamRedirectsDisabled(WithHTTPUpstreamProfile(ctx, HTTPUpstreamProfileOpenAI))
	var body *strings.Reader
	if method == http.MethodPost {
		body = strings.NewReader("{}")
	} else {
		body = strings.NewReader("")
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	headers, err := s.buildOpenAIAuthenticationHeaders(ctx, account, token)
	if err != nil {
		return nil, err
	}
	req.Header = headers
	req.Header.Set("Accept", "application/json, text/event-stream")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	account.ApplyHeaderOverrides(req.Header)
	return s.doOpenAIUpstream(req, resolveAccountProxyURL(account), account)
}
