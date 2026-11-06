//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type backgroundTerminalRepo struct {
	AIRoutingBackgroundRepository
	job   *AIRoutingBackgroundJob
	calls int
}

func (r *backgroundTerminalRepo) Get(context.Context, string) (*AIRoutingBackgroundJob, error) {
	return r.job, nil
}
func (r *backgroundTerminalRepo) GetBackgroundByResponse(context.Context, int64, string) (*AIRoutingBackgroundJob, error) {
	return r.job, nil
}
func (r *backgroundTerminalRepo) RecordBackgroundTerminal(_ context.Context, id string, accountID int64, responseID string, terminal AIRoutingBackgroundTerminal) error {
	if id != r.job.ID || accountID != r.job.AccountID || responseID != r.job.ResponseID {
		return ErrAIBackgroundConflict
	}
	r.calls++
	r.job.Terminal = &terminal
	return nil
}

func TestAIBackgroundTerminalStrictOriginAndSubmittedEndpoint(t *testing.T) {
	s, j, key, user, account := backgroundPriceFixture(t)
	prepared, err := s.Prepare(t.Context(), j, key, user, account, nil, []byte(`{"model":"gpt-5.1","background":true,"store":true,"input":"fake","max_output_tokens":20}`), time.Now())
	require.NoError(t, err)
	prepared.ResponseID = "resp_original"
	prepared.Status = AIBackgroundActive
	r := &backgroundTerminalRepo{job: prepared}
	s.repository = r
	s.gateway.accountRepo = &openAIRecordUsageAccountRepoStub{account: account}
	terminal := AIRoutingBackgroundTerminal{Status: "completed", Model: "gpt-5.1", UsageKnown: true, InputTokens: 10, OutputTokens: 2}
	require.NoError(t, s.ValidateResponseOrigin(t.Context(), key, account.ID, prepared.ResponseID))
	require.NoError(t, s.RecordResponseTerminal(t.Context(), key, account.ID, prepared.ResponseID, terminal))
	require.Equal(t, 1, r.calls)
	for _, mutate := range []func(*APIKey){
		func(k *APIKey) { k.ID++ }, func(k *APIKey) { k.UserID++ },
		func(k *APIKey) { id := *k.GroupID + 1; k.GroupID = &id; k.Group = &Group{ID: id} },
		func(k *APIKey) { k.GroupID = nil },
	} {
		other := *key
		mutate(&other)
		require.ErrorIs(t, s.RecordResponseTerminal(t.Context(), &other, account.ID, prepared.ResponseID, terminal), ErrAIBackgroundRecoveryPending)
	}
	require.ErrorIs(t, s.RecordResponseTerminal(t.Context(), key, account.ID+1, prepared.ResponseID, terminal), ErrAIBackgroundRecoveryPending)
	require.ErrorIs(t, s.RecordResponseTerminal(t.Context(), key, account.ID, "resp_other", terminal), ErrAIBackgroundRecoveryPending)
	account.Credentials["base_url"] = "https://different.example.invalid"
	require.ErrorIs(t, s.ValidateResponseOrigin(t.Context(), key, account.ID, prepared.ResponseID), ErrAIBackgroundRecoveryPending)
	require.ErrorIs(t, s.RecordResponseTerminal(t.Context(), key, account.ID, prepared.ResponseID, terminal), ErrAIBackgroundRecoveryPending)
	require.Equal(t, 1, r.calls, "no control evidence from an edited endpoint")
	// Evidence from the original, already submitted connection is still valid.
	require.NoError(t, s.RecordTerminal(t.Context(), prepared.ID, account.ID, prepared.ResponseID, terminal))
	require.Equal(t, 2, r.calls)
	require.ErrorIs(t, s.RecordTerminal(t.Context(), prepared.ID, account.ID+1, prepared.ResponseID, terminal), ErrAIBackgroundRecoveryPending)
}
