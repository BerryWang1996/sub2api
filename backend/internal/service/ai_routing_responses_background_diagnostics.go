package service

import (
	"context"
	"time"
)

// Administrator-only projection: no request/response body, pricing snapshot,
// credentials or provider error text is exposed by the diagnostics boundary.
type AIRoutingBackgroundDiagnostic struct {
	ID                     string     `json:"id"`
	SourceGroupID          int64      `json:"source_group_id"`
	UserID                 int64      `json:"user_id"`
	APIKeyID               int64      `json:"api_key_id"`
	AccountID              int64      `json:"account_id"`
	SubscriptionID         int64      `json:"subscription_id,omitempty"`
	RequestID              string     `json:"request_id,omitempty"`
	ClientRequestID        string     `json:"client_request_id,omitempty"`
	OriginalUsageRequestID string     `json:"original_usage_request_id,omitempty"`
	UsageRequestID         string     `json:"usage_request_id"`
	CandidateID            string     `json:"candidate_id"`
	PublicModel            string     `json:"public_model"`
	ExecutionModel         string     `json:"execution_model"`
	Platform               string     `json:"platform"`
	PolicyVersion          int64      `json:"policy_version"`
	ResponseID             string     `json:"response_id,omitempty"`
	Status                 string     `json:"status"`
	FailureReason          string     `json:"failure_reason,omitempty"`
	HoldAmount             float64    `json:"hold_amount"`
	ActualCost             *float64   `json:"actual_cost"`
	UsageKnown             bool       `json:"usage_known"`
	InputTokens            int        `json:"input_tokens"`
	OutputTokens           int        `json:"output_tokens"`
	CacheSyncPending       bool       `json:"cache_sync_pending"`
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
	NextAttemptAt          time.Time  `json:"next_attempt_at"`
	SettledAt              *time.Time `json:"settled_at,omitempty"`
	LeaseUntil             *time.Time `json:"lease_until,omitempty"`
}

type AIRoutingBackgroundDiagnosticsRepository interface {
	ListBackgroundDiagnostics(context.Context, int64, string, int) ([]AIRoutingBackgroundDiagnostic, error)
}

type AIRoutingBackgroundResponseRepository interface {
	GetBackgroundByResponse(context.Context, int64, string) (*AIRoutingBackgroundJob, error)
}

// Deleting a provider resource before durable settlement can destroy the only
// authoritative usage source. This is a strict original-key authorization check,
// independent of the ordinary response-owner cache.
func (s *AIRoutingResponsesBackgroundService) CanDeleteResponse(ctx context.Context, key *APIKey, accountID int64, responseID string) error {
	if s == nil || s.repository == nil || key == nil || key.Group == nil || key.GroupID == nil || *key.GroupID != key.Group.ID || key.ID <= 0 || key.UserID <= 0 || accountID <= 0 || responseID == "" {
		return ErrAIBackgroundRecoveryPending
	}
	reader, ok := s.repository.(AIRoutingBackgroundResponseRepository)
	if !ok {
		return ErrAIBackgroundRecoveryPending
	}
	j, err := reader.GetBackgroundByResponse(ctx, accountID, responseID)
	if err != nil || j == nil || j.SourceGroupID != key.Group.ID || j.UserID != key.UserID || j.APIKeyID != key.ID || j.AccountID != accountID || j.ResponseID != responseID || j.Status != AIBackgroundSettled || !j.CacheSynced {
		return ErrAIBackgroundRecoveryPending
	}
	return nil
}

func AIRoutingBackgroundDiagnosticFromJob(j *AIRoutingBackgroundJob) AIRoutingBackgroundDiagnostic {
	if j == nil {
		return AIRoutingBackgroundDiagnostic{}
	}
	d := AIRoutingBackgroundDiagnostic{ID: j.ID, SourceGroupID: j.SourceGroupID, UserID: j.UserID, APIKeyID: j.APIKeyID, AccountID: j.AccountID, SubscriptionID: j.SubscriptionID, RequestID: j.OriginalRequestID, ClientRequestID: j.ClientRequestID, OriginalUsageRequestID: j.OriginalUsageRequestID, UsageRequestID: AIRoutingBackgroundBillingRequestID(j.ID), CandidateID: j.CandidateID, PublicModel: j.PublicModel, ExecutionModel: j.ExecutionModel, Platform: j.Platform, PolicyVersion: j.PolicyVersion, ResponseID: j.ResponseID, Status: j.Status, FailureReason: j.FailureReason, HoldAmount: j.HoldAmount, CacheSyncPending: !j.CacheSynced, CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt, NextAttemptAt: j.NextAttemptAt, SettledAt: j.SettledAt, LeaseUntil: j.LeaseUntil}
	if j.Terminal != nil {
		d.UsageKnown = j.Terminal.UsageKnown
		if d.UsageKnown {
			d.InputTokens = j.Terminal.InputTokens
			d.OutputTokens = j.Terminal.OutputTokens
		}
	}
	if j.Status == AIBackgroundSettled && j.BillingCommand != nil {
		actual := j.BillingCommand.BalanceCost
		if j.SubscriptionID > 0 {
			actual = j.BillingCommand.SubscriptionCost
		}
		d.ActualCost = &actual
	}
	return d
}

func (s *AIRoutingResponsesBackgroundService) Diagnostics(ctx context.Context, sourceGroupID int64, status string, limit int) ([]AIRoutingBackgroundDiagnostic, error) {
	if s == nil || s.repository == nil {
		return nil, ErrAIBackgroundUnsupported
	}
	r, ok := s.repository.(AIRoutingBackgroundDiagnosticsRepository)
	if !ok {
		return nil, ErrAIBackgroundUnsupported
	}
	return r.ListBackgroundDiagnostics(ctx, sourceGroupID, status, limit)
}
