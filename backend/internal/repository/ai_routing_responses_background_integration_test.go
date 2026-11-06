//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func backgroundFixture(t *testing.T, subscription bool) (service.AIRoutingBackgroundRepository, *service.AIRoutingBackgroundJob) {
	t.Helper()
	client := testEntClient(t)
	// These tests must commit real transactions for worker/race coverage. The
	// shared integration database is not reset between suites, so register exact
	// fixture cleanup before creating anything, including when a later step fails.
	var userID, groupID, keyID, accountID int64
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		tx, err := integrationDB.BeginTx(cleanup, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		for _, step := range []struct {
			query string
			id    int64
		}{
			{"DELETE FROM fork_ai_response_jobs WHERE user_id=$1", userID},
			{"DELETE FROM fork_ai_platform_windows WHERE user_id=$1", userID},
			{"DELETE FROM usage_billing_dedup WHERE api_key_id=$1", keyID},
			{"DELETE FROM usage_billing_dedup_archive WHERE api_key_id=$1", keyID},
			{"DELETE FROM billing_usage_entries WHERE user_id=$1", userID},
			{"DELETE FROM usage_logs WHERE user_id=$1", userID},
			{"DELETE FROM user_subscriptions WHERE user_id=$1", userID},
			{"DELETE FROM user_platform_quotas WHERE user_id=$1", userID},
			{"DELETE FROM api_keys WHERE id=$1", keyID},
			{"DELETE FROM accounts WHERE id=$1", accountID},
			{"DELETE FROM groups WHERE id=$1", groupID},
			{"DELETE FROM users WHERE id=$1", userID},
		} {
			if step.id > 0 {
				_, err = tx.ExecContext(cleanup, step.query, step.id)
				require.NoError(t, err, "cleanup committed background fixture")
			}
		}
		require.NoError(t, tx.Commit())
	})
	u := mustCreateUser(t, client, &service.User{Balance: 10})
	userID = u.ID
	g := mustCreateGroup(t, client, &service.Group{Name: "background-" + uuid.NewString(), RateMultiplier: 1})
	groupID = g.ID
	k := mustCreateApiKey(t, client, &service.APIKey{UserID: u.ID, GroupID: &g.ID, Key: "sk-" + uuid.NewString(), Name: "background", Quota: 10, RateLimit5h: 10, RateLimit1d: 10, RateLimit7d: 10})
	keyID = k.ID
	a := mustCreateAccount(t, client, &service.Account{Name: "background-" + uuid.NewString(), Platform: service.PlatformOpenAI, Type: service.AccountTypeAPIKey})
	accountID = a.ID
	j := &service.AIRoutingBackgroundJob{ID: uuid.NewString(), SourceGroupID: g.ID, UserID: u.ID, APIKeyID: k.ID, AccountID: a.ID, CandidateID: "candidate", PublicModel: "any", ExecutionModel: "fake-model", Platform: service.PlatformOpenAI, TargetAccountID: a.ID, PolicyVersion: 1, RequestPayloadHash: strings.Repeat("a", 64), HoldAmount: 4, PricingSnapshot: json.RawMessage(`{"input_price":0.001,"output_price":0.002}`)}
	if subscription {
		_, err := integrationDB.Exec(`UPDATE groups SET subscription_type='subscription',daily_limit_usd=10,weekly_limit_usd=10,monthly_limit_usd=10 WHERE id=$1`, g.ID)
		require.NoError(t, err)
		s := mustCreateSubscription(t, client, &service.UserSubscription{UserID: u.ID, GroupID: g.ID, ExpiresAt: time.Now().Add(90 * 24 * time.Hour)})
		j.SubscriptionID = s.ID
	}
	return NewAIRoutingResponsesBackgroundRepository(client, integrationDB), j
}

func backgroundGet(t *testing.T, r service.AIRoutingBackgroundRepository, j *service.AIRoutingBackgroundJob) *service.AIRoutingBackgroundJob {
	t.Helper()
	v, e := r.Get(context.Background(), j.ID)
	require.NoError(t, e)
	return v
}
func backgroundActive(t *testing.T, r service.AIRoutingBackgroundRepository, j *service.AIRoutingBackgroundJob) *service.AIRoutingBackgroundJob {
	t.Helper()
	ctx := context.Background()
	require.ErrorIs(t, r.MarkSubmitting(ctx, j.ID, j.Version), service.ErrAIBackgroundConflict, "generation must wait for confirmed cache invalidation")
	require.NoError(t, r.MarkCacheSynced(ctx, j.ID, j.Version))
	j = backgroundGet(t, r, j)
	require.NoError(t, r.MarkSubmitting(ctx, j.ID, j.Version))
	require.NoError(t, r.AttachResponse(ctx, j.ID, "resp_"+j.ID))
	return backgroundGet(t, r, j)
}
func backgroundReady(t *testing.T, r service.AIRoutingBackgroundRepository, j *service.AIRoutingBackgroundJob) *service.AIRoutingBackgroundJob {
	t.Helper()
	require.NoError(t, r.SaveTerminal(context.Background(), j.ID, j.Version, service.AIRoutingBackgroundTerminal{Status: "completed", Model: j.ExecutionModel, UsageKnown: true, InputTokens: 10, OutputTokens: 5, CacheReadTokens: 2}, time.Now()))
	return backgroundGet(t, r, j)
}
func backgroundCommand(j *service.AIRoutingBackgroundJob, cost float64) *service.UsageBillingCommand {
	c := &service.UsageBillingCommand{RequestID: service.AIRoutingBackgroundBillingRequestID(j.ID), UserID: j.UserID, APIKeyID: j.APIKeyID, AccountID: j.AccountID, AccountType: service.AccountTypeAPIKey, Model: j.ExecutionModel, RequestPayloadHash: j.RequestPayloadHash, InputTokens: j.Terminal.InputTokens - j.Terminal.CacheReadTokens - j.Terminal.CacheCreationTokens, OutputTokens: j.Terminal.OutputTokens, CacheReadTokens: j.Terminal.CacheReadTokens, CacheCreationTokens: j.Terminal.CacheCreationTokens, APIKeyQuotaCost: cost, APIKeyRateLimitCost: cost}
	if j.SubscriptionID > 0 {
		c.SubscriptionID = &j.SubscriptionID
		c.SubscriptionCost = cost
	} else {
		c.BalanceCost = cost
	}
	return c
}

func TestAIRoutingBackgroundPostgresLifecycle(t *testing.T) {
	for _, subscription := range []bool{false, true} {
		t.Run(fmt.Sprintf("subscription=%v", subscription), func(t *testing.T) {
			ctx := context.Background()
			r, j := backgroundFixture(t, subscription)
			saved, err := r.Reserve(ctx, j, 4)
			require.NoError(t, err)
			duplicate, err := r.Reserve(ctx, j, 4)
			require.NoError(t, err)
			require.Equal(t, saved.Version, duplicate.Version)
			changed := *j
			changed.ExecutionModel = "different"
			_, err = r.Reserve(ctx, &changed, 4)
			require.ErrorIs(t, err, service.ErrAIBackgroundConflict)
			snapshot, err := r.Reserved(ctx, j.UserID, j.SubscriptionID, j.APIKeyID)
			require.NoError(t, err)
			require.Equal(t, 1, snapshot.Pending)
			require.Equal(t, 4.0, snapshot.KeyQuotaUSD)
			wantBalance := 6.0
			if subscription {
				wantBalance = 10
				require.Equal(t, 4.0, snapshot.SubscriptionUSD)
			}
			require.Equal(t, wantBalance, snapshot.UserBalance)
			// A new repository process resumes from SQL; no transient job state is required.
			r = NewAIRoutingResponsesBackgroundRepository(nil, integrationDB)
			saved = backgroundActive(t, r, saved)
			require.ErrorIs(t, r.ReleaseUnsubmitted(ctx, saved.ID, saved.Version), service.ErrAIBackgroundConflict)
			require.NoError(t, r.SaveTerminal(ctx, saved.ID, saved.Version, service.AIRoutingBackgroundTerminal{Status: "failed", Model: saved.ExecutionModel}, time.Now()))
			unknown := backgroundGet(t, r, saved)
			require.Equal(t, service.AIBackgroundUsageUnknown, unknown.Status)
			_, err = r.Settle(ctx, unknown.ID, unknown.Version, backgroundCommand(unknown, 0))
			require.ErrorIs(t, err, service.ErrAIBackgroundUnknownUsage)
			saved = backgroundReady(t, r, unknown)
			// Disabling a Key prevents new execution but cannot waive historical cost.
			_, err = integrationDB.Exec(`UPDATE api_keys SET status='disabled' WHERE id=$1`, j.APIKeyID)
			require.NoError(t, err)
			cmd := backgroundCommand(saved, 2)
			result, err := r.Settle(ctx, saved.ID, saved.Version, cmd)
			require.NoError(t, err)
			require.True(t, result.Applied)
			replay, err := r.Settle(ctx, saved.ID, saved.Version, cmd)
			require.NoError(t, err)
			require.False(t, replay.Applied)
			_, err = r.Settle(ctx, saved.ID, saved.Version, backgroundCommand(saved, 3))
			require.ErrorIs(t, err, service.ErrUsageBillingRequestConflict)
			snapshot, err = r.Reserved(ctx, j.UserID, j.SubscriptionID, j.APIKeyID)
			require.NoError(t, err)
			require.Zero(t, snapshot.Pending)
			require.Zero(t, snapshot.KeyQuotaUSD)
			require.Equal(t, 2.0, snapshot.KeyQuotaUsed)
			require.Equal(t, 2.0, snapshot.KeyEffective5hUsage)
			if subscription {
				require.Equal(t, 10.0, snapshot.UserBalance)
				require.Equal(t, 2.0, snapshot.SubscriptionDailyUsage)
				require.Equal(t, 2.0, snapshot.SubscriptionWeeklyUsage)
				require.Equal(t, 2.0, snapshot.SubscriptionMonthlyUsage)
			} else {
				require.Equal(t, 8.0, snapshot.UserBalance)
			}
			var frozen float64
			require.NoError(t, integrationDB.QueryRow(`SELECT frozen_balance FROM users WHERE id=$1`, j.UserID).Scan(&frozen))
			require.Zero(t, frozen)
			final := backgroundGet(t, r, saved)
			require.Equal(t, service.AIBackgroundSettled, final.Status)
			require.False(t, final.CacheSynced)
		})
	}
}

func TestAIRoutingBackgroundPostgresConcurrentReserveAndOrdinaryBilling(t *testing.T) {
	for _, subscription := range []bool{false, true} {
		t.Run(fmt.Sprintf("subscription=%v", subscription), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			r, j := backgroundFixture(t, subscription)
			j.HoldAmount = 6
			start := make(chan struct{})
			result := make(chan error, 2)
			for i := 0; i < 2; i++ {
				copyJob := *j
				copyJob.ID = uuid.NewString()
				go func(job service.AIRoutingBackgroundJob) { <-start; _, e := r.Reserve(ctx, &job, 4); result <- e }(copyJob)
			}
			close(start)
			first, second := <-result, <-result
			require.True(t, (first == nil) != (second == nil), "only one reserve: %v / %v", first, second)
			s, err := r.Reserved(ctx, j.UserID, j.SubscriptionID, j.APIKeyID)
			require.NoError(t, err)
			require.Equal(t, 1, s.Pending)
			require.Equal(t, 6.0, s.KeyQuotaUSD)
			// Ordinary Apply shares user -> subscription -> key locking with reserve.
			// It represents already incurred cost and may create debt; it must not be
			// dropped or deadlock when admitted before the background hold.
			billing := NewUsageBillingRepository(nil, integrationDB)
			var wg sync.WaitGroup
			errs := make(chan error, 8)
			for n := 0; n < 4; n++ {
				wg.Add(2)
				go func() {
					defer wg.Done()
					cmd := &service.UsageBillingCommand{RequestID: uuid.NewString(), UserID: j.UserID, APIKeyID: j.APIKeyID, BalanceCost: .1, APIKeyQuotaCost: .1}
					if subscription {
						cmd.SubscriptionID = &j.SubscriptionID
						cmd.SubscriptionCost = .1
					}
					_, e := billing.Apply(ctx, cmd)
					errs <- e
				}()
				go func() {
					defer wg.Done()
					copyJob := *j
					copyJob.ID = uuid.NewString()
					copyJob.HoldAmount = .1
					_, e := r.Reserve(ctx, &copyJob, 8)
					errs <- e
				}()
			}
			wg.Wait()
			close(errs)
			for e := range errs {
				require.NoError(t, e)
			}
		})
	}
}

func TestAIRoutingBackgroundPostgresWindowResetAndStrictIdentity(t *testing.T) {
	ctx := context.Background()
	r, j := backgroundFixture(t, true)
	saved, err := r.Reserve(ctx, j, 4)
	require.NoError(t, err)
	_, err = integrationDB.Exec(`UPDATE user_subscriptions SET starts_at=NOW()-INTERVAL '45 days',daily_window_start=NOW()-INTERVAL '2 days',weekly_window_start=NOW()-INTERVAL '15 days',monthly_window_start=NOW()-INTERVAL '35 days',daily_usage_usd=9,weekly_usage_usd=9,monthly_usage_usd=9 WHERE id=$1`, j.SubscriptionID)
	require.NoError(t, err)
	_, err = integrationDB.Exec(`UPDATE api_keys SET window_5h_start=NOW()-INTERVAL '8 days',window_1d_start=NOW()-INTERVAL '8 days',window_7d_start=NOW()-INTERVAL '8 days',usage_5h=9,usage_1d=9,usage_7d=9 WHERE id=$1`, j.APIKeyID)
	require.NoError(t, err)
	s, err := r.Reserved(ctx, j.UserID, j.SubscriptionID, j.APIKeyID)
	require.NoError(t, err)
	require.Zero(t, s.SubscriptionDailyUsage)
	require.Zero(t, s.SubscriptionWeeklyUsage)
	require.Zero(t, s.SubscriptionMonthlyUsage)
	require.Zero(t, s.KeyEffective5hUsage)
	require.Zero(t, s.KeyEffective1dUsage)
	require.Zero(t, s.KeyEffective7dUsage)
	require.Equal(t, 4.0, s.SubscriptionUSD)
	other := mustCreateUser(t, testEntClient(t), &service.User{})
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := integrationDB.ExecContext(cleanup, "DELETE FROM users WHERE id=$1", other.ID)
		require.NoError(t, err, "cleanup unrelated identity fixture")
	})
	_, err = r.Reserved(ctx, other.ID, j.SubscriptionID, 0)
	require.ErrorIs(t, err, service.ErrSubscriptionNotFound)
	_, err = r.Reserved(ctx, other.ID, 0, j.APIKeyID)
	require.ErrorIs(t, err, service.ErrAPIKeyNotFound)
	s, err = r.Reserved(ctx, j.UserID, 0, 0)
	require.NoError(t, err)
	require.Zero(t, s.SubscriptionUSD)
	require.Zero(t, s.KeyQuotaUSD)
	require.Zero(t, s.APIKeyID)
	copyJob := *j
	copyJob.ID = uuid.NewString()
	copyJob.HoldAmount = 7
	_, err = r.Reserve(ctx, &copyJob, 4)
	require.ErrorIs(t, err, service.ErrAIBackgroundCapacity, "reset does not erase unresolved reservations")
	require.NoError(t, r.ReleaseUnsubmitted(ctx, saved.ID, saved.Version))
	require.NoError(t, r.ReleaseUnsubmitted(ctx, saved.ID, saved.Version))
}

func TestAIRoutingBackgroundPostgresLeaseAndUnknownNeverResubmitted(t *testing.T) {
	ctx := context.Background()
	r, j := backgroundFixture(t, false)
	j, err := r.Reserve(ctx, j, 4)
	require.NoError(t, err)
	require.NoError(t, r.MarkCacheSynced(ctx, j.ID, j.Version))
	j = backgroundGet(t, r, j)
	require.NoError(t, r.MarkSubmitting(ctx, j.ID, j.Version))
	_, err = integrationDB.Exec(`UPDATE fork_ai_response_jobs SET next_attempt_at=NOW()-INTERVAL '1 second' WHERE id=$1`, j.ID)
	require.NoError(t, err)
	claims, err := r.ClaimDue(ctx, "first", time.Minute, 64)
	require.NoError(t, err)
	var mine *service.AIRoutingBackgroundJob
	for _, claimed := range claims {
		if claimed.ID == j.ID {
			mine = claimed
		}
	}
	require.NotNil(t, mine)
	_, err = integrationDB.Exec(`UPDATE fork_ai_response_jobs SET lease_until=NOW()-INTERVAL '1 second' WHERE id=$1`, j.ID)
	require.NoError(t, err)
	claims, err = r.ClaimDue(ctx, "restarted", time.Minute, 64)
	require.NoError(t, err)
	var restarted *service.AIRoutingBackgroundJob
	for _, claimed := range claims {
		if claimed.ID == j.ID {
			restarted = claimed
		}
	}
	require.NotNil(t, restarted)
	require.ErrorIs(t, r.MarkSubmissionUnknown(ctx, j.ID, mine.Version), service.ErrAIBackgroundConflict)
	require.NoError(t, r.MarkSubmissionUnknown(ctx, j.ID, restarted.Version))
	j = backgroundGet(t, r, j)
	require.ErrorIs(t, r.MarkSubmitting(ctx, j.ID, j.Version), service.ErrAIBackgroundConflict)
	require.ErrorIs(t, r.ReleaseUnsubmitted(ctx, j.ID, j.Version), service.ErrAIBackgroundConflict)
	// A delayed initial response can reconcile its ID; this is not a new POST.
	require.NoError(t, r.AttachResponse(ctx, j.ID, "resp_late"))
	require.NoError(t, r.AttachResponse(ctx, j.ID, "resp_late"))
	require.ErrorIs(t, r.AttachResponse(ctx, j.ID, "resp_conflict"), service.ErrAIBackgroundConflict)
}

func TestAIRoutingBackgroundPostgresWorkerRecoveryWithFakeHTTP(t *testing.T) {
	for _, subscription := range []bool{false, true} {
		t.Run(fmt.Sprintf("subscription=%v", subscription), func(t *testing.T) {
			ctx := context.Background()
			r, j := backgroundFixture(t, subscription)
			j, err := r.Reserve(ctx, j, 4)
			require.NoError(t, err)
			j = backgroundActive(t, r, j)
			var gets atomic.Int64
			var unknown atomic.Bool
			unknown.Store(true)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				require.Equal(t, http.MethodGet, req.Method)
				gets.Add(1)
				w.Header().Set("Content-Type", "application/json")
				terminal := service.AIRoutingBackgroundTerminal{Status: "completed", Model: j.ExecutionModel, UsageKnown: !unknown.Load(), InputTokens: 10, OutputTokens: 5}
				require.NoError(t, json.NewEncoder(w).Encode(terminal))
			}))
			defer server.Close()
			var failSync atomic.Bool
			failSync.Store(true)
			var logs sync.Map
			hooks := service.AIRoutingBackgroundWorkerHooks{
				Fetch: func(ctx context.Context, job *service.AIRoutingBackgroundJob) (service.AIRoutingBackgroundTerminal, bool, error) {
					req, e := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/responses/"+job.ResponseID, nil)
					if e != nil {
						return service.AIRoutingBackgroundTerminal{}, false, e
					}
					resp, e := server.Client().Do(req)
					if e != nil {
						return service.AIRoutingBackgroundTerminal{}, false, e
					}
					defer func() { _ = resp.Body.Close() }()
					var terminal service.AIRoutingBackgroundTerminal
					e = json.NewDecoder(resp.Body).Decode(&terminal)
					return terminal, true, e
				},
				BuildCommand: func(_ context.Context, job *service.AIRoutingBackgroundJob) (*service.UsageBillingCommand, error) {
					return backgroundCommand(job, 2), nil
				},
				Sync: func(_ context.Context, job *service.AIRoutingBackgroundJob) error {
					if job.Status == service.AIBackgroundSettled {
						if failSync.Load() {
							return errors.New("synthetic cache outage")
						}
						logs.LoadOrStore(service.AIRoutingBackgroundBillingRequestID(job.ID), true)
					}
					return nil
				},
			}
			// Isolate due selection from other jobs owned by this fixture without
			// changing another test's data in the shared integration database.
			due := func() {
				_, e := integrationDB.Exec(`UPDATE fork_ai_response_jobs SET next_attempt_at=NOW()+INTERVAL '1 hour' WHERE user_id=$2 AND id<>$1`, j.ID, j.UserID)
				require.NoError(t, e)
				_, e = integrationDB.Exec(`UPDATE fork_ai_response_jobs SET next_attempt_at=NOW()-INTERVAL '1 second',lease_until=NULL WHERE id=$1`, j.ID)
				require.NoError(t, e)
			}
			w := service.AIRoutingBackgroundWorker{Repository: r, Hooks: hooks, Owner: "before-restart"}
			due()
			_, err = w.RunOnce(ctx, 1)
			require.NoError(t, err)
			require.Equal(t, service.AIBackgroundUsageUnknown, backgroundGet(t, r, j).Status)
			unknown.Store(false)
			due()
			_, err = w.RunOnce(ctx, 1)
			require.ErrorIs(t, err, service.ErrAIBackgroundRecoveryPending)
			settled := backgroundGet(t, r, j)
			require.Equal(t, service.AIBackgroundSettled, settled.Status)
			require.False(t, settled.CacheSynced)
			// The SQL billing commit survived; a new worker only repairs the log/cache.
			failSync.Store(false)
			w = service.AIRoutingBackgroundWorker{Repository: NewAIRoutingResponsesBackgroundRepository(nil, integrationDB), Hooks: hooks, Owner: "after-restart"}
			due()
			_, err = w.RunOnce(ctx, 1)
			require.NoError(t, err)
			require.True(t, backgroundGet(t, r, j).CacheSynced)
			require.EqualValues(t, 2, gets.Load())
			_, ok := logs.Load(service.AIRoutingBackgroundBillingRequestID(j.ID))
			require.True(t, ok)
			var dedup int
			require.NoError(t, integrationDB.QueryRow(`SELECT COUNT(*) FROM usage_billing_dedup WHERE request_id=$1 AND api_key_id=$2`, service.AIRoutingBackgroundBillingRequestID(j.ID), j.APIKeyID).Scan(&dedup))
			require.Equal(t, 1, dedup)
		})
	}
}

func TestAIRoutingBackgroundPostgresPlatformContribution(t *testing.T) {
	ctx := context.Background()
	r, j := backgroundFixture(t, false)
	_, err := integrationDB.Exec(`INSERT INTO user_platform_quotas(user_id,platform,daily_limit_usd,weekly_limit_usd,monthly_limit_usd) VALUES($1,$2,5,8,9)`, j.UserID, j.Platform)
	require.NoError(t, err)
	j, err = r.Reserve(ctx, j, 4)
	require.NoError(t, err)
	windows := service.AIRoutingBackgroundPlatformWindows{DailyStart: time.Now().Add(-24 * time.Hour), WeeklyStart: time.Now().Add(-7 * 24 * time.Hour), MonthlyStart: time.Now().Add(-30 * 24 * time.Hour)}
	p, err := r.ReservedPlatform(ctx, j.UserID, j.Platform, windows)
	require.NoError(t, err)
	require.Equal(t, 4.0, p.PendingUSD)
	require.Equal(t, 4.0, p.DailyUSD)
	require.Equal(t, 4.0, p.WeeklyUSD)
	require.Equal(t, 4.0, p.MonthlyUSD)
	next := *j
	next.ID = uuid.NewString()
	next.HoldAmount = 2
	_, err = r.Reserve(ctx, &next, 4)
	require.ErrorIs(t, err, service.ErrAIBackgroundCapacity)
	j = backgroundReady(t, r, backgroundActive(t, r, j))
	_, err = r.Settle(ctx, j.ID, j.Version, backgroundCommand(j, 2))
	require.NoError(t, err)
	p, err = r.ReservedPlatform(ctx, j.UserID, j.Platform, windows)
	require.NoError(t, err)
	require.Zero(t, p.PendingUSD)
	require.Equal(t, 2.0, p.DailyUSD)
	require.Equal(t, 2.0, p.WeeklyUSD)
	require.Equal(t, 2.0, p.MonthlyUSD)
	// Normal platform counters stay untouched, so their absolute-value flusher
	// cannot erase or double-count the separate durable background contribution.
	var normal float64
	require.NoError(t, integrationDB.QueryRow(`SELECT daily_usage_usd FROM user_platform_quotas WHERE user_id=$1 AND platform=$2`, j.UserID, j.Platform).Scan(&normal))
	require.Zero(t, normal)
	quotaRepo := NewUserPlatformQuotaRepository(testEntClient(t))
	require.NoError(t, quotaRepo.ResetExpiredWindow(ctx, j.UserID, j.Platform, "daily", time.Now()))
	p, err = r.ReservedPlatform(ctx, j.UserID, j.Platform, windows)
	require.NoError(t, err)
	require.Zero(t, p.DailyUSD)
	require.Equal(t, 2.0, p.WeeklyUSD)
	_, err = integrationDB.Exec(`UPDATE fork_ai_response_jobs SET settled_at=NOW()-INTERVAL '10 days' WHERE id=$1`, j.ID)
	require.NoError(t, err)
	_, err = integrationDB.Exec(`UPDATE fork_ai_platform_windows SET monthly_window_start=NOW()-INTERVAL '20 days' WHERE user_id=$1 AND platform=$2`, j.UserID, j.Platform)
	require.NoError(t, err)
	p, err = r.ReservedPlatform(ctx, j.UserID, j.Platform, windows)
	require.NoError(t, err)
	require.Zero(t, p.DailyUSD)
	require.Zero(t, p.WeeklyUSD)
	require.Equal(t, 2.0, p.MonthlyUSD)
	p, err = r.ReservedPlatform(ctx, j.UserID, service.PlatformAnthropic, windows)
	require.NoError(t, err)
	require.Zero(t, p.MonthlyUSD)
}

func TestAIRoutingBackgroundPostgresIndependentMonthAndExplicitReset(t *testing.T) {
	ctx := t.Context()
	r, j := backgroundFixture(t, false)
	_, err := integrationDB.Exec(`INSERT INTO user_platform_quotas(user_id,platform,monthly_limit_usd) VALUES($1,$2,10)`, j.UserID, j.Platform)
	require.NoError(t, err)
	j, err = r.Reserve(ctx, j, 4)
	require.NoError(t, err)
	j = backgroundReady(t, r, backgroundActive(t, r, j))
	_, err = r.Settle(ctx, j.ID, j.Version, backgroundCommand(j, 2))
	require.NoError(t, err)
	windows := service.AIRoutingBackgroundPlatformWindows{DailyStart: time.Now().Add(-24 * time.Hour), WeeklyStart: time.Now().Add(-7 * 24 * time.Hour)}
	before, err := r.ReservedPlatform(ctx, j.UserID, j.Platform, windows)
	require.NoError(t, err)
	require.NotNil(t, before.MonthlyWindowStart)
	require.Equal(t, 2.0, before.MonthlyUSD)
	// An ordinary first usage / lazy cache reset chooses a newer anchor. It
	// cannot remove settled background cost from its independent 30-day period.
	newOrdinary := time.Now()
	quotaRepo := NewUserPlatformQuotaRepository(testEntClient(t))
	require.NoError(t, quotaRepo.IncrementUsageWithReset(ctx, j.UserID, j.Platform, 1, newOrdinary))
	windows.MonthlyStart = newOrdinary
	after, err := r.ReservedPlatform(ctx, j.UserID, j.Platform, windows)
	require.NoError(t, err)
	require.Equal(t, 2.0, after.MonthlyUSD)
	require.True(t, after.MonthlyWindowIndependent)
	require.Equal(t, before.MonthlyWindowStart, after.MonthlyWindowStart)
	// Absolute ordinary dirty snapshots cannot reset the separate ledger.
	_, err = integrationDB.Exec(`UPDATE user_platform_quotas SET monthly_usage_usd=0,monthly_window_start=clock_timestamp() WHERE user_id=$1 AND platform=$2`, j.UserID, j.Platform)
	require.NoError(t, err)
	after, err = r.ReservedPlatform(ctx, j.UserID, j.Platform, windows)
	require.NoError(t, err)
	require.Equal(t, 2.0, after.MonthlyUSD)
	next := *j
	next.ID = uuid.NewString()
	next.HoldAmount = 3
	next.ResponseID = ""
	pending, err := r.Reserve(ctx, &next, 4)
	require.NoError(t, err)
	require.NoError(t, quotaRepo.ResetExpiredWindow(ctx, j.UserID, j.Platform, "monthly", time.Now()))
	reset, err := r.ReservedPlatform(ctx, j.UserID, j.Platform, windows)
	require.NoError(t, err)
	require.Equal(t, 3.0, reset.PendingUSD)
	require.Equal(t, 3.0, reset.MonthlyUSD, "manual reset removes settled cost but never releases an unresolved hold")
	pending = backgroundReady(t, r, backgroundActive(t, r, pending))
	_, err = r.Settle(ctx, pending.ID, pending.Version, backgroundCommand(pending, 1))
	require.NoError(t, err)
	reset, err = r.ReservedPlatform(ctx, j.UserID, j.Platform, windows)
	require.NoError(t, err)
	require.Equal(t, 1.0, reset.MonthlyUSD)
	// Ordinary and background portions may expire on different days. Background
	// expires at its own 30-day boundary, not 30 days after the later ordinary
	// first usage; a new background charge opens a fresh independent period.
	_, err = integrationDB.Exec(`UPDATE fork_ai_platform_windows SET monthly_window_start=NOW()-INTERVAL '31 days' WHERE user_id=$1 AND platform=$2`, j.UserID, j.Platform)
	require.NoError(t, err)
	expired, err := r.ReservedPlatform(ctx, j.UserID, j.Platform, windows)
	require.NoError(t, err)
	require.Nil(t, expired.MonthlyWindowStart)
	require.Zero(t, expired.MonthlyUSD)
}

func TestAIRoutingBackgroundPostgresDiagnosticsExcludeSnapshots(t *testing.T) {
	r, j := backgroundFixture(t, false)
	j.OriginalRequestID = "operational-request"
	j.ClientRequestID = "client-request"
	j.OriginalUsageRequestID = "client:client-request"
	j.PricingSnapshot = []byte(`{"private_price_marker":"fake-secret-no-output"}`)
	j, err := r.Reserve(t.Context(), j, 4)
	require.NoError(t, err)
	diagnostics := r.(service.AIRoutingBackgroundDiagnosticsRepository)
	rows, err := diagnostics.ListBackgroundDiagnostics(t.Context(), j.SourceGroupID, "", 50)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "operational-request", rows[0].RequestID)
	require.Equal(t, "client:client-request", rows[0].OriginalUsageRequestID)
	require.Equal(t, service.AIRoutingBackgroundBillingRequestID(j.ID), rows[0].UsageRequestID)
	require.Nil(t, rows[0].ActualCost)
	data, err := json.Marshal(rows)
	require.NoError(t, err)
	require.NotContains(t, string(data), "private_price_marker")
	require.NotContains(t, string(data), "fake-secret-no-output")
	other, err := diagnostics.ListBackgroundDiagnostics(t.Context(), j.SourceGroupID+999999, "", 50)
	require.NoError(t, err)
	require.Empty(t, other)
}

func TestAIRoutingBackgroundPostgresNilSubscriptionAnchorDoesNotEraseCost(t *testing.T) {
	ctx := context.Background()
	r, j := backgroundFixture(t, true)
	_, err := integrationDB.Exec(`UPDATE user_subscriptions SET daily_usage_usd=8,weekly_usage_usd=8,monthly_usage_usd=8 WHERE id=$1`, j.SubscriptionID)
	require.NoError(t, err)
	s, err := r.Reserved(ctx, j.UserID, j.SubscriptionID, j.APIKeyID)
	require.NoError(t, err)
	require.Equal(t, 8.0, s.SubscriptionDailyUsage)
	require.Equal(t, 8.0, s.SubscriptionWeeklyUsage)
	require.Equal(t, 8.0, s.SubscriptionMonthlyUsage)
	_, err = r.Reserve(ctx, j, 4)
	require.ErrorIs(t, err, service.ErrAIBackgroundCapacity)
}

func TestAIRoutingBackgroundPostgresOverageAndDeletedKey(t *testing.T) {
	ctx := context.Background()
	r, j := backgroundFixture(t, false)
	j, err := r.Reserve(ctx, j, 4)
	require.NoError(t, err)
	j = backgroundReady(t, r, backgroundActive(t, r, j))
	_, err = integrationDB.Exec(`UPDATE api_keys SET deleted_at=NOW() WHERE id=$1`, j.APIKeyID)
	require.NoError(t, err)
	result, err := r.Settle(ctx, j.ID, j.Version, backgroundCommand(j, 12))
	require.NoError(t, err)
	require.True(t, result.BalanceOverdrafted)
	require.Equal(t, -2.0, *result.NewBalance)
	var frozen float64
	require.NoError(t, integrationDB.QueryRow(`SELECT frozen_balance FROM users WHERE id=$1`, j.UserID).Scan(&frozen))
	require.Zero(t, frozen)
}

type backgroundIntegrationHTTP struct {
	service.HTTPUpstream
	client *http.Client
}

func (h *backgroundIntegrationHTTP) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return h.client.Do(req)
}

func TestAIRoutingBackgroundPostgresActualServiceFactory(t *testing.T) {
	for _, subscription := range []bool{false, true} {
		for _, observation := range []string{"fetch", "create", "control"} {
			t.Run(fmt.Sprint(subscription)+"/"+observation, func(t *testing.T) {
				ctx := context.Background()
				repo, j := backgroundFixture(t, subscription)
				j.ExecutionModel = "gpt-5.1"
				var gets atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					require.Equal(t, http.MethodGet, req.Method)
					require.Equal(t, "/v1/responses/resp_factory_"+j.ID, req.URL.Path)
					require.Equal(t, "Bearer fake-background-key", req.Header.Get("Authorization"))
					gets.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"id":"resp_factory_%s","status":"completed","model":"gpt-5.1","usage":{"input_tokens":11,"output_tokens":7,"input_tokens_details":{"cached_tokens":1}},"output":[]}`, j.ID)
				}))
				defer server.Close()
				credentials, err := json.Marshal(map[string]string{"base_url": server.URL, "api_key": "fake-background-key"})
				require.NoError(t, err)
				_, err = integrationDB.Exec(`UPDATE accounts SET credentials=$2::jsonb WHERE id=$1`, j.AccountID, string(credentials))
				require.NoError(t, err)
				_, err = integrationDB.Exec(`UPDATE groups SET rate_multiplier=1.2345 WHERE id=$1`, j.SourceGroupID)
				require.NoError(t, err)
				client := testEntClient(t)
				users := NewUserRepository(client, integrationDB)
				keys := NewAPIKeyRepository(client, integrationDB)
				subs := NewUserSubscriptionRepository(client)
				accounts := NewAccountRepository(client, integrationDB, nil)
				logs := NewUsageLogRepository(client, integrationDB)
				user, err := users.GetByID(ctx, j.UserID)
				require.NoError(t, err)
				key, err := keys.GetByID(ctx, j.APIKeyID)
				require.NoError(t, err)
				var sub *service.UserSubscription
				if subscription {
					sub, err = subs.GetByID(ctx, j.SubscriptionID)
					require.NoError(t, err)
				}
				cfg := &config.Config{}
				cfg.Default.RateMultiplier = 1
				cfg.Security.URLAllowlist.AllowInsecureHTTP = true
				quotas := NewUserPlatformQuotaServiceAdapter(NewUserPlatformQuotaRepository(client))
				cache := service.NewBillingCacheService(nil, users, subs, keys, nil, nil, cfg, quotas)
				cache.SetBackgroundReservations(repo)
				defer cache.Stop()
				billing := service.NewBillingService(cfg, nil)
				gateway := service.NewOpenAIGatewayService(accounts, logs, NewUsageBillingRepository(client, integrationDB), users, subs, nil, nil, cfg, nil, nil, billing, nil, cache, &backgroundIntegrationHTTP{client: server.Client()}, nil, nil, nil, service.NewModelPricingResolver(nil, billing), nil, nil, nil, quotas)
				defer gateway.CloseOpenAIWSPool()
				factory := service.NewAIRoutingResponsesBackgroundService(repo, gateway)
				body := []byte(`{"model":"gpt-5.1","background":true,"store":true,"input":"fake local input","max_output_tokens":20}`)
				prepared, err := factory.PrepareForAccount(ctx, j, key, user, sub, body, time.Now())
				require.NoError(t, err)
				saved, err := factory.ReserveAndSync(ctx, prepared, key, user, sub, 4)
				require.NoError(t, err)
				require.True(t, saved.CacheSynced)
				require.NoError(t, factory.BeginSubmission(ctx, saved))
				require.NoError(t, factory.AttachResponse(ctx, saved.ID, "resp_factory_"+saved.ID))
				require.ErrorIs(t, factory.CanDeleteResponse(ctx, key, j.AccountID, "resp_factory_"+saved.ID), service.ErrAIBackgroundRecoveryPending)
				linked, err := repo.(service.AIRoutingBackgroundResponseRepository).GetBackgroundByResponse(ctx, j.AccountID, "resp_factory_"+saved.ID)
				require.NoError(t, err)
				require.Equal(t, saved.ID, linked.ID)
				if observation != "fetch" {
					observed := ctx
					if observation == "create" {
						observed = service.WithAIRoutingBackgroundTerminalObserver(observed, func(ctx context.Context, accountID int64, responseID string, terminal service.AIRoutingBackgroundTerminal) error {
							return factory.RecordTerminal(ctx, saved.ID, accountID, responseID, terminal)
						})
					} else {
						observed = service.WithAIRoutingBackgroundTerminalObserver(observed, func(ctx context.Context, accountID int64, responseID string, terminal service.AIRoutingBackgroundTerminal) error {
							return factory.RecordResponseTerminal(ctx, key, accountID, responseID, terminal)
						})
					}
					payload := []byte(fmt.Sprintf(`{"id":"resp_factory_%s","status":"completed","model":"gpt-5.1","usage":{"input_tokens":11,"output_tokens":7,"input_tokens_details":{"cached_tokens":1}},"output":[]}`, j.ID))
					require.NoError(t, service.ObserveAIRoutingBackgroundResponsePayload(observed, j.AccountID, payload, ""))
					require.Equal(t, service.AIBackgroundReady, backgroundGet(t, repo, j).Status)
					require.NoError(t, service.ObserveAIRoutingBackgroundResponsePayload(observed, j.AccountID, payload, ""), "duplicate observer evidence is idempotent")
				}
				// Prices changed after submission; the fresh worker must use only the
				// recorded price snapshot and the original immutable execution identity.
				_, err = integrationDB.Exec(`UPDATE groups SET rate_multiplier=999 WHERE id=$1`, j.SourceGroupID)
				require.NoError(t, err)
				_, err = integrationDB.Exec(`UPDATE fork_ai_response_jobs SET next_attempt_at=NOW()+INTERVAL '1 hour' WHERE user_id=$2 AND id<>$1`, j.ID, j.UserID)
				require.NoError(t, err)
				factory = service.NewAIRoutingResponsesBackgroundService(NewAIRoutingResponsesBackgroundRepository(nil, integrationDB), gateway)
				processed, err := factory.RunOnce(ctx, 1)
				require.NoError(t, err)
				require.Equal(t, 1, processed)
				settled := backgroundGet(t, repo, j)
				require.Equal(t, service.AIBackgroundSettled, settled.Status)
				require.True(t, settled.CacheSynced)
				require.NotNil(t, settled.SettledAt)
				require.NoError(t, factory.CanDeleteResponse(ctx, key, j.AccountID, "resp_factory_"+saved.ID))
				var count int
				var actual, rate float64
				var input, output, cached int
				err = integrationDB.QueryRow(`SELECT COUNT(*),MAX(actual_cost),MAX(rate_multiplier),MAX(input_tokens),MAX(output_tokens),MAX(cache_read_tokens) FROM usage_logs WHERE request_id=$1 AND api_key_id=$2`, service.AIRoutingBackgroundBillingRequestID(j.ID), j.APIKeyID).Scan(&count, &actual, &rate, &input, &output, &cached)
				require.NoError(t, err)
				require.Equal(t, 1, count)
				require.Equal(t, 1.2345, rate)
				require.Equal(t, 10, input)
				require.Equal(t, 7, output)
				require.Equal(t, 1, cached)
				billed := settled.BillingCommand.BalanceCost
				if subscription {
					billed = settled.BillingCommand.SubscriptionCost
				}
				require.InDelta(t, actual, billed, 1e-8)
				require.Positive(t, billed)
				_, err = factory.RunOnce(ctx, 1)
				require.NoError(t, err)
				if observation == "fetch" {
					require.EqualValues(t, 1, gets.Load())
				} else {
					require.Zero(t, gets.Load(), "received durable terminal evidence settles after restart without another provider GET")
				}
			})
		}
	}
}

func TestAIRoutingBackgroundPostgresTerminalObservationsFenceWorkers(t *testing.T) {
	for _, subscription := range []bool{false, true} {
		t.Run(fmt.Sprint(subscription), func(t *testing.T) {
			ctx := t.Context()
			r, j := backgroundFixture(t, subscription)
			j, err := r.Reserve(ctx, j, 4)
			require.NoError(t, err)
			j = backgroundActive(t, r, j)
			writer := r.(service.AIRoutingBackgroundTerminalRepository)
			unknown := service.AIRoutingBackgroundTerminal{Status: "completed", Model: j.ExecutionModel}
			require.NoError(t, writer.RecordBackgroundTerminal(ctx, j.ID, j.AccountID, j.ResponseID, unknown))
			j = backgroundGet(t, r, j)
			require.Equal(t, service.AIBackgroundUsageUnknown, j.Status)
			require.NoError(t, writer.RecordBackgroundTerminal(ctx, j.ID, j.AccountID, j.ResponseID, unknown))
			require.Equal(t, j.Version, backgroundGet(t, r, j).Version)
			_, err = integrationDB.Exec(`UPDATE fork_ai_response_jobs SET next_attempt_at=NOW()+INTERVAL '1 hour' WHERE user_id=$2 AND id<>$1`, j.ID, j.UserID)
			require.NoError(t, err)
			claimed, err := r.ClaimDue(ctx, "stale-fetch", time.Minute, 1)
			require.NoError(t, err)
			require.Len(t, claimed, 1)
			stale := claimed[0]
			terminal := unknown
			terminal.UsageKnown = true
			terminal.InputTokens = 10
			terminal.OutputTokens = 5
			terminal.CacheReadTokens = 2
			var wg sync.WaitGroup
			errors := make(chan error, 12)
			for i := 0; i < 12; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					errors <- writer.RecordBackgroundTerminal(ctx, j.ID, j.AccountID, j.ResponseID, terminal)
				}()
			}
			wg.Wait()
			close(errors)
			for err := range errors {
				require.NoError(t, err)
			}
			ready := backgroundGet(t, r, j)
			require.Equal(t, service.AIBackgroundReady, ready.Status)
			require.Equal(t, stale.Version+1, ready.Version, "only the first observation changes state")
			require.Empty(t, ready.LeaseOwner)
			require.ErrorIs(t, r.SaveTerminal(ctx, j.ID, stale.Version, unknown, time.Now()), service.ErrAIBackgroundConflict, "an older GET cannot erase newer terminal evidence")
			claimed, err = r.ClaimDue(ctx, "settling", time.Minute, 1)
			require.NoError(t, err)
			require.Len(t, claimed, 1)
			ready = claimed[0]
			require.NoError(t, writer.RecordBackgroundTerminal(ctx, j.ID, j.AccountID, j.ResponseID, terminal))
			require.NoError(t, writer.RecordBackgroundTerminal(ctx, j.ID, j.AccountID, j.ResponseID, unknown))
			duplicate := backgroundGet(t, r, j)
			require.Equal(t, ready.Version, duplicate.Version)
			require.Equal(t, "settling", duplicate.LeaseOwner, "duplicate callbacks cannot steal the settlement lease")
			cmd := backgroundCommand(ready, 2)
			var applied atomic.Int32
			errors = make(chan error, 8)
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					result, err := r.Settle(ctx, j.ID, ready.Version, cmd)
					if err == nil && result.Applied {
						applied.Add(1)
					}
					errors <- err
				}()
			}
			wg.Wait()
			close(errors)
			for err := range errors {
				require.NoError(t, err)
			}
			require.EqualValues(t, 1, applied.Load())
			settled := backgroundGet(t, r, j)
			require.NoError(t, writer.RecordBackgroundTerminal(ctx, j.ID, j.AccountID, j.ResponseID, terminal))
			require.NoError(t, writer.RecordBackgroundTerminal(ctx, j.ID, j.AccountID, j.ResponseID, unknown))
			require.Equal(t, settled.Version, backgroundGet(t, r, j).Version)
			changed := terminal
			changed.OutputTokens++
			require.ErrorIs(t, writer.RecordBackgroundTerminal(ctx, j.ID, j.AccountID, j.ResponseID, changed), service.ErrAIBackgroundConflict)
			require.ErrorIs(t, writer.RecordBackgroundTerminal(ctx, j.ID, j.AccountID+1, j.ResponseID, terminal), service.ErrAIBackgroundConflict)
			require.ErrorIs(t, writer.RecordBackgroundTerminal(ctx, j.ID, j.AccountID, "resp_wrong", terminal), service.ErrAIBackgroundConflict)
			var used float64
			require.NoError(t, integrationDB.QueryRow(`SELECT quota_used FROM api_keys WHERE id=$1`, j.APIKeyID).Scan(&used))
			require.Equal(t, 2.0, used)
		})
	}
}

func TestAIRoutingBackgroundPostgresPreservesPreQuantizationFingerprint(t *testing.T) {
	ctx := context.Background()
	repo, j := backgroundFixture(t, false)
	j, err := repo.Reserve(ctx, j, 4)
	require.NoError(t, err)
	j = backgroundReady(t, repo, backgroundActive(t, repo, j))
	cmd := backgroundCommand(j, 1.234567895)
	cmd.Normalize()
	expected := cmd.RequestFingerprint
	_, err = repo.Settle(ctx, j.ID, j.Version, cmd)
	require.NoError(t, err)
	settled := backgroundGet(t, repo, j)
	require.Equal(t, expected, settled.BillingCommand.RequestFingerprint)
	replay, err := repo.Settle(ctx, j.ID, j.Version, cmd)
	require.NoError(t, err)
	require.False(t, replay.Applied)
}

func TestAIRoutingBackgroundPostgresSameProviderResponseCannotBillTwice(t *testing.T) {
	ctx := context.Background()
	repo, first := backgroundFixture(t, false)
	first.HoldAmount = 2
	second := *first
	second.ID = uuid.NewString()
	first, err := repo.Reserve(ctx, first, 4)
	require.NoError(t, err)
	first = backgroundActive(t, repo, first)
	other, err := repo.Reserve(ctx, &second, 4)
	require.NoError(t, err)
	require.NoError(t, repo.MarkCacheSynced(ctx, other.ID, other.Version))
	other = backgroundGet(t, repo, other)
	require.NoError(t, repo.MarkSubmitting(ctx, other.ID, other.Version))
	require.ErrorIs(t, repo.AttachResponse(ctx, other.ID, first.ResponseID), service.ErrAIBackgroundConflict)
	other = backgroundGet(t, repo, other)
	require.Equal(t, service.AIBackgroundSubmissionUnknown, other.Status)
	require.Equal(t, "response_id_conflict", other.FailureReason)
	require.Empty(t, other.ResponseID)
	require.ErrorIs(t, repo.MarkSubmitting(ctx, other.ID, other.Version), service.ErrAIBackgroundConflict)
	first = backgroundReady(t, repo, first)
	_, err = repo.Settle(ctx, first.ID, first.Version, backgroundCommand(first, 1))
	require.NoError(t, err)
	var frozen float64
	require.NoError(t, integrationDB.QueryRow(`SELECT frozen_balance FROM users WHERE id=$1`, first.UserID).Scan(&frozen))
	require.Equal(t, 2.0, frozen, "ambiguous second job stays reserved, never independently billed")
}
