package service

import "strings"

const AIRoutingGeminiCountTokens = "gemini_count_tokens"

// Native capability describes the wire adapter, not a provider/model probe.
// Antigravity's Claude models cannot preserve Gemini-only signatures or config.
func AIRoutingAccountSupportsNativeGemini(account *Account, model string) bool {
	if account == nil || !account.IsModelSupported(model) || account.GetMappedModel(model) != model {
		return false
	}
	return account.Platform == PlatformGemini || account.Platform == PlatformAntigravity && strings.HasPrefix(strings.ToLower(model), "gemini-")
}
