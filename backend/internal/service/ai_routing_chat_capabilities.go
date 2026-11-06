package service

import "github.com/Wei-Shaw/sub2api/internal/domain"

// Multiple choices and inline audio/files must retain the native Chat wire.
// Metadata saying a model supports audio does not attest a bridge's conversion.
func AIRoutingAccountSupportsNativeChat(account *Account, model string) bool {
	return account != nil && account.Type == AccountTypeAPIKey && domain.UsesOpenAIGateway(account.Platform) &&
		account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityChatCompletions) &&
		resolveUpstreamProtocol(account, APIProtocolChatCompletions, model, nil) == APIProtocolChatCompletions
}
