package service

import "context"

// Optional to preserve compatibility with ordinary billing/test repositories.
// Implementations serialize observations with worker claims and settlement.
type AIRoutingBackgroundTerminalRepository interface {
	RecordBackgroundTerminal(context.Context, string, int64, string, AIRoutingBackgroundTerminal) error
}

// RecordTerminal accepts evidence from the original submitted connection. A
// later account edit must not discard usage already incurred at that endpoint.
func (s *AIRoutingResponsesBackgroundService) RecordTerminal(ctx context.Context, jobID string, accountID int64, responseID string, terminal AIRoutingBackgroundTerminal) error {
	if s == nil || s.repository == nil || jobID == "" || accountID <= 0 || responseID == "" {
		return ErrAIBackgroundRecoveryPending
	}
	j, err := s.repository.Get(ctx, jobID)
	if err != nil || j == nil || j.ID != jobID || j.AccountID != accountID || j.ResponseID != responseID {
		return ErrAIBackgroundRecoveryPending
	}
	prices, err := decodeAIBackgroundPrice(j)
	if err != nil || prices.EndpointHash == "" {
		return ErrAIBackgroundRecoveryPending
	}
	writer, ok := s.repository.(AIRoutingBackgroundTerminalRepository)
	if !ok {
		return ErrAIBackgroundRecoveryPending
	}
	return writer.RecordBackgroundTerminal(ctx, jobID, accountID, responseID, terminal)
}

func (s *AIRoutingResponsesBackgroundService) ownedResponseJob(ctx context.Context, key *APIKey, accountID int64, responseID string) (*AIRoutingBackgroundJob, error) {
	if s == nil || s.repository == nil || key == nil || key.Group == nil || key.Group.ID <= 0 || key.GroupID == nil || *key.GroupID != key.Group.ID || key.ID <= 0 || key.UserID <= 0 || accountID <= 0 || responseID == "" {
		return nil, ErrAIBackgroundRecoveryPending
	}
	reader, ok := s.repository.(AIRoutingBackgroundResponseRepository)
	if !ok {
		return nil, ErrAIBackgroundRecoveryPending
	}
	j, err := reader.GetBackgroundByResponse(ctx, accountID, responseID)
	if err != nil || j == nil || j.SourceGroupID != key.Group.ID || j.UserID != key.UserID || j.APIKeyID != key.ID || j.AccountID != accountID || j.ResponseID != responseID {
		return nil, ErrAIBackgroundRecoveryPending
	}
	return j, nil
}

// ValidateResponseOrigin runs before a control request sends the stored ID.
// Account IDs alone are insufficient if an administrator changed its endpoint.
func (s *AIRoutingResponsesBackgroundService) ValidateResponseOrigin(ctx context.Context, key *APIKey, accountID int64, responseID string) error {
	j, err := s.ownedResponseJob(ctx, key, accountID, responseID)
	if err != nil {
		return err
	}
	if s.gateway == nil || s.gateway.accountRepo == nil {
		return ErrAIBackgroundRecoveryPending
	}
	prices, err := decodeAIBackgroundPrice(j)
	if err != nil || prices.EndpointHash == "" {
		return ErrAIBackgroundRecoveryPending
	}
	account, err := s.gateway.accountRepo.GetByID(ctx, accountID)
	if err != nil || account == nil || account.ID != accountID || prices.EndpointHash != aiBackgroundEndpointHash(account) {
		return ErrAIBackgroundRecoveryPending
	}
	return nil
}

// Control responses have their own strict key and current endpoint check;
// recording their terminal usage never authorizes another generation or charge.
func (s *AIRoutingResponsesBackgroundService) RecordResponseTerminal(ctx context.Context, key *APIKey, accountID int64, responseID string, terminal AIRoutingBackgroundTerminal) error {
	if err := s.ValidateResponseOrigin(ctx, key, accountID, responseID); err != nil {
		return err
	}
	j, err := s.ownedResponseJob(ctx, key, accountID, responseID)
	if err != nil {
		return err
	}
	return s.RecordTerminal(ctx, j.ID, accountID, responseID, terminal)
}
