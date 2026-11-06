package service

const AIRoutingCurrentSchemaVersion = 2

// ClientEndpoints returns a detached allowlist. Missing endpoint configuration
// retains the original Messages contract; unsupported schemas fail closed.
func (p *AIRoutingPolicy) ClientEndpoints() []string {
	if p == nil || p.validateClientEndpoints() != nil {
		return nil
	}
	if p.EnabledEndpoints == nil {
		return []string{CompositeRouteEndpointMessages}
	}
	return append([]string(nil), p.EnabledEndpoints...)
}

// AllowsEndpoint treats Messages token counting as part of the Messages client
// contract. Responses HTTP never implicitly enables WebSocket or other APIs.
func (p *AIRoutingPolicy) AllowsEndpoint(endpoint string) bool {
	if endpoint == CompositeRouteEndpointCountTokens {
		endpoint = CompositeRouteEndpointMessages
	}
	for _, enabled := range p.ClientEndpoints() {
		if enabled == endpoint {
			return true
		}
	}
	return false
}

func (p *AIRoutingPolicy) validateClientEndpoints() error {
	if p.SchemaVersion < 0 || p.SchemaVersion > AIRoutingCurrentSchemaVersion {
		return aiPolicyError("Unsupported AI routing schema version")
	}
	if p.EnabledEndpoints == nil {
		return nil
	}
	if len(p.EnabledEndpoints) == 0 {
		return aiPolicyError("Enable at least one client endpoint")
	}
	seen := make(map[string]bool, len(p.EnabledEndpoints))
	for _, endpoint := range p.EnabledEndpoints {
		if endpoint != CompositeRouteEndpointMessages && endpoint != CompositeRouteEndpointResponses && endpoint != CompositeRouteEndpointChatCompletions && endpoint != AIRoutingEndpointResponsesWS && endpoint != CompositeRouteEndpointGemini {
			return aiPolicyError("Unsupported AI routing client endpoint")
		}
		if p.SchemaVersion < AIRoutingCurrentSchemaVersion && endpoint != CompositeRouteEndpointMessages {
			return aiPolicyError("Additional client endpoints require schema version 2")
		}
		if seen[endpoint] {
			return aiPolicyError("Client endpoints must be unique")
		}
		seen[endpoint] = true
	}
	return nil
}
