package service

import (
	"context"
	"errors"
)

// Native Responses is a wire capability, not proof of a compatible provider's
// retention or CRUD implementation. Control operations retain its errors and
// never change accounts. Catalog-only protocol guesses do not grant state access.
func AIRoutingAccountSupportsStoredResponses(account *Account, model string) bool {
	return account != nil && account.IsOpenAIApiKey() &&
		AIRoutingAccountSupportsNativeResponses(account, model)
}

func AIRoutingAccountSupportsNativeResponses(account *Account, model string) bool {
	return account != nil && (account.Platform == PlatformOpenAI || account.IsMultiProtocolAPIKey()) &&
		account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilityResponses) &&
		resolveUpstreamProtocol(account, APIProtocolResponses, model, nil) == APIProtocolResponses
}

func (s *GatewayService) AIRoutingStoredResponsesAccount(ctx context.Context, accountID int64, model string) (bool, error) {
	account, err := s.aiRoutingResponseAccount(ctx, accountID)
	return err == nil && AIRoutingAccountSupportsStoredResponses(account, model), err
}

func (s *GatewayService) AIRoutingNativeResponsesAccount(ctx context.Context, accountID int64, model string) (bool, error) {
	account, err := s.aiRoutingResponseAccount(ctx, accountID)
	return err == nil && AIRoutingAccountSupportsNativeResponses(account, model), err
}

func (s *GatewayService) aiRoutingResponseAccount(ctx context.Context, accountID int64) (*Account, error) {
	if s == nil || s.accountRepo == nil || accountID <= 0 {
		return nil, errors.New("execution account unavailable")
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if account == nil || account.ID != accountID {
		return nil, errors.New("execution account unavailable")
	}
	return account, nil
}
