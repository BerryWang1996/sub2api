package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

type aiRoutingResponsesBackgroundRepository struct{ db *sql.DB }

func NewAIRoutingResponsesBackgroundRepository(_ *dbent.Client, db *sql.DB) service.AIRoutingBackgroundRepository {
	return &aiRoutingResponsesBackgroundRepository{db: db}
}

// All financial writers acquire user -> subscription -> key before any ledger
// or dedup row. Missing historical identities are deliberately tolerated here:
// existing billing effects retain their own deleted-row settlement semantics.
func lockUsageBillingIdentities(ctx context.Context, tx *sql.Tx, userID, subscriptionID, keyID int64) error {
	for _, row := range []struct {
		query string
		id    int64
	}{
		{`SELECT id FROM users WHERE id=$1 FOR UPDATE`, userID},
		{`SELECT id FROM user_subscriptions WHERE id=$1 FOR UPDATE`, subscriptionID},
		{`SELECT id FROM api_keys WHERE id=$1 FOR UPDATE`, keyID},
	} {
		if row.id <= 0 {
			continue
		}
		var id int64
		if err := tx.QueryRowContext(ctx, row.query, row.id).Scan(&id); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	return nil
}

const aiBackgroundColumns = `identity,response_id,status,version,cache_synced,lease_owner,lease_until,next_attempt_at,created_at,updated_at,terminal,billing_command,settled_at,failure_reason`

type aiBackgroundScanner interface{ Scan(...any) error }

func scanAIBackground(row aiBackgroundScanner) (*service.AIRoutingBackgroundJob, error) {
	j := new(service.AIRoutingBackgroundJob)
	var identity, terminal, command []byte
	var response, owner sql.NullString
	if err := row.Scan(&identity, &response, &j.Status, &j.Version, &j.CacheSynced, &owner, &j.LeaseUntil, &j.NextAttemptAt, &j.CreatedAt, &j.UpdatedAt, &terminal, &command, &j.SettledAt, &j.FailureReason); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrAIBackgroundNotFound
		}
		return nil, err
	}
	if err := json.Unmarshal(identity, j); err != nil {
		return nil, err
	}
	j.ResponseID = response.String
	j.LeaseOwner = owner.String
	if len(terminal) > 0 {
		if err := json.Unmarshal(terminal, &j.Terminal); err != nil {
			return nil, err
		}
	}
	if len(command) > 0 {
		if err := json.Unmarshal(command, &j.BillingCommand); err != nil {
			return nil, err
		}
	}
	return j, nil
}

func (r *aiRoutingResponsesBackgroundRepository) Get(ctx context.Context, id string) (*service.AIRoutingBackgroundJob, error) {
	return scanAIBackground(r.db.QueryRowContext(ctx, `SELECT `+aiBackgroundColumns+` FROM fork_ai_response_jobs WHERE id=$1`, id))
}

func (r *aiRoutingResponsesBackgroundRepository) GetBackgroundByResponse(ctx context.Context, accountID int64, responseID string) (*service.AIRoutingBackgroundJob, error) {
	if accountID <= 0 || strings.TrimSpace(responseID) != responseID || len(responseID) < 1 || len(responseID) > 256 {
		return nil, service.ErrAIBackgroundConflict
	}
	return scanAIBackground(r.db.QueryRowContext(ctx, `SELECT `+aiBackgroundColumns+` FROM fork_ai_response_jobs WHERE account_id=$1 AND response_id=$2`, accountID, responseID))
}

func (r *aiRoutingResponsesBackgroundRepository) ListBackgroundDiagnostics(ctx context.Context, sourceGroupID int64, status string, limit int) ([]service.AIRoutingBackgroundDiagnostic, error) {
	if sourceGroupID <= 0 || limit < 1 || limit > 50 {
		return nil, service.ErrAIBackgroundConflict
	}
	switch status {
	case "", service.AIBackgroundAdmitted, service.AIBackgroundSubmitting, service.AIBackgroundSubmissionUnknown, service.AIBackgroundActive, service.AIBackgroundUsageUnknown, service.AIBackgroundReady, service.AIBackgroundSettled, service.AIBackgroundReleased:
	default:
		return nil, service.ErrAIBackgroundConflict
	}
	columns := strings.Replace(aiBackgroundColumns, "identity,", `(identity-'pricing_snapshot'),`, 1)
	rows, err := r.db.QueryContext(ctx, `SELECT `+columns+` FROM fork_ai_response_jobs WHERE (identity->>'source_group_id')::bigint=$1 AND ($2='' OR status=$2) ORDER BY created_at DESC,id DESC LIMIT $3`, sourceGroupID, status, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	items := make([]service.AIRoutingBackgroundDiagnostic, 0)
	for rows.Next() {
		j, err := scanAIBackground(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, service.AIRoutingBackgroundDiagnosticFromJob(j))
	}
	return items, rows.Err()
}

type aiBackgroundQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}
type aiBackgroundAdmission struct {
	Reserved                              service.AIRoutingBackgroundReserved
	UserStatus                            string
	Key                                   service.APIKey
	Subscription                          service.UserSubscription
	DailyLimit, WeeklyLimit, MonthlyLimit *float64
	Now                                   time.Time
}

// Actual usage and durable holds MUST be read in one statement. Reading cached
// usage after SUM(holds) permits a settlement/cache-invalidation gap to undercount.
func readAIBackgroundAdmission(ctx context.Context, q aiBackgroundQueryer, userID, subscriptionID, keyID int64) (aiBackgroundAdmission, error) {
	a := aiBackgroundAdmission{}
	var subID, keyFound sql.NullInt64
	var subJSON, keyJSON []byte
	err := q.QueryRowContext(ctx, `
	 SELECT u.id,u.balance,u.status,NOW(),s.id,k.id,
	   COALESCE((SELECT SUM(hold_amount) FROM fork_ai_response_jobs WHERE subscription_id=$2 AND $2>0 AND status NOT IN ('settled','released')),0),
	   COALESCE((SELECT SUM(hold_amount) FROM fork_ai_response_jobs WHERE api_key_id=$3 AND $3>0 AND status NOT IN ('settled','released')),0),
	   (SELECT COUNT(*) FROM fork_ai_response_jobs WHERE user_id=$1 AND status NOT IN ('settled','released')),
	   CASE WHEN s.id IS NULL THEN NULL ELSE json_build_object('ID',s.id,'UserID',s.user_id,'GroupID',s.group_id,'StartsAt',s.starts_at,'ExpiresAt',s.expires_at,'Status',s.status,'DailyWindowStart',s.daily_window_start,'WeeklyWindowStart',s.weekly_window_start,'MonthlyWindowStart',s.monthly_window_start,'DailyUsageUSD',s.daily_usage_usd,'WeeklyUsageUSD',s.weekly_usage_usd,'MonthlyUsageUSD',s.monthly_usage_usd) END,
	   CASE WHEN k.id IS NULL THEN NULL ELSE json_build_object('ID',k.id,'UserID',k.user_id,'GroupID',k.group_id,'Status',k.status,'ExpiresAt',k.expires_at,'Quota',k.quota,'QuotaUsed',k.quota_used,'RateLimit5h',k.rate_limit_5h,'RateLimit1d',k.rate_limit_1d,'RateLimit7d',k.rate_limit_7d,'Usage5h',k.usage_5h,'Usage1d',k.usage_1d,'Usage7d',k.usage_7d,'Window5hStart',k.window_5h_start,'Window1dStart',k.window_1d_start,'Window7dStart',k.window_7d_start) END,
	   g.daily_limit_usd,g.weekly_limit_usd,g.monthly_limit_usd
	 FROM users u
	 LEFT JOIN user_subscriptions s ON s.id=$2 AND $2>0 AND s.user_id=u.id AND s.deleted_at IS NULL
	 LEFT JOIN groups g ON g.id=s.group_id AND g.deleted_at IS NULL
	 LEFT JOIN api_keys k ON k.id=$3 AND $3>0 AND k.user_id=u.id AND k.deleted_at IS NULL
	 WHERE u.id=$1 AND u.deleted_at IS NULL`, userID, subscriptionID, keyID).Scan(
		&a.Reserved.UserID, &a.Reserved.UserBalance, &a.UserStatus, &a.Now, &subID, &keyFound, &a.Reserved.SubscriptionUSD, &a.Reserved.KeyQuotaUSD, &a.Reserved.Pending, &subJSON, &keyJSON, &a.DailyLimit, &a.WeeklyLimit, &a.MonthlyLimit)
	if errors.Is(err, sql.ErrNoRows) {
		return a, service.ErrUserNotFound
	}
	if err != nil {
		return a, err
	}
	if subscriptionID > 0 && !subID.Valid {
		return a, service.ErrSubscriptionNotFound
	}
	if keyID > 0 && !keyFound.Valid {
		return a, service.ErrAPIKeyNotFound
	}
	a.Reserved.SubscriptionID = subID.Int64
	a.Reserved.APIKeyID = keyFound.Int64
	a.Reserved.KeyRateUSD = a.Reserved.KeyQuotaUSD
	if len(subJSON) > 0 {
		if err = json.Unmarshal(subJSON, &a.Subscription); err != nil {
			return a, err
		}
		service.AIRoutingBackgroundAdvanceSubscriptionWindows(&a.Subscription, a.Now)
	}
	if len(keyJSON) > 0 {
		if err = json.Unmarshal(keyJSON, &a.Key); err != nil {
			return a, err
		}
	}
	a.Reserved.SubscriptionDailyUsage = a.Subscription.DailyUsageUSD
	a.Reserved.SubscriptionWeeklyUsage = a.Subscription.WeeklyUsageUSD
	a.Reserved.SubscriptionMonthlyUsage = a.Subscription.MonthlyUsageUSD
	a.Reserved.KeyQuotaUsed = a.Key.QuotaUsed
	effective := func(v float64, start *time.Time, d time.Duration) float64 {
		if start == nil || !a.Now.Before(start.Add(d)) {
			return 0
		}
		return v
	}
	a.Reserved.KeyEffective5hUsage = effective(a.Key.Usage5h, a.Key.Window5hStart, 5*time.Hour)
	a.Reserved.KeyEffective1dUsage = effective(a.Key.Usage1d, a.Key.Window1dStart, 24*time.Hour)
	a.Reserved.KeyEffective7dUsage = effective(a.Key.Usage7d, a.Key.Window7dStart, 7*24*time.Hour)
	return a, nil
}

func (r *aiRoutingResponsesBackgroundRepository) Reserved(ctx context.Context, userID, subscriptionID, keyID int64) (service.AIRoutingBackgroundReserved, error) {
	if userID <= 0 || subscriptionID < 0 || keyID < 0 {
		return service.AIRoutingBackgroundReserved{}, service.ErrAIBackgroundConflict
	}
	a, err := readAIBackgroundAdmission(ctx, r.db, userID, subscriptionID, keyID)
	return a.Reserved, err
}

func (r *aiRoutingResponsesBackgroundRepository) ReservedPlatform(ctx context.Context, userID int64, platform string, windows service.AIRoutingBackgroundPlatformWindows) (service.AIRoutingBackgroundPlatformReserved, error) {
	return readAIBackgroundPlatform(ctx, r.db, userID, platform, windows)
}

func readAIBackgroundPlatform(ctx context.Context, q aiBackgroundQueryer, userID int64, platform string, windows service.AIRoutingBackgroundPlatformWindows) (service.AIRoutingBackgroundPlatformReserved, error) {
	result := service.AIRoutingBackgroundPlatformReserved{}
	if userID <= 0 || platform == "" || windows.DailyStart.IsZero() || windows.WeeklyStart.IsZero() {
		return result, service.ErrAIBackgroundConflict
	}
	// MonthlyStart from the caller describes the ordinary cache only. It may be
	// lazily replaced by any ordinary request and cannot revoke background cost.
	// Read the independent anchors and all sums in one consistent DB snapshot.
	err := q.QueryRowContext(ctx, `WITH w AS (
	 SELECT GREATEST($3::timestamptz,m.daily_reset_at) AS ds,
	 GREATEST($4::timestamptz,m.weekly_reset_at) AS ws,
	 CASE WHEN m.monthly_window_start IS NOT NULL AND clock_timestamp()<m.monthly_window_start+INTERVAL '30 days' THEN m.monthly_window_start END AS ms,
	 m.monthly_reset_at AS mr FROM users u LEFT JOIN fork_ai_platform_windows m ON m.user_id=u.id AND m.platform=$2
	 WHERE u.id=$1 AND u.deleted_at IS NULL
	) SELECT
	 COALESCE(SUM(CASE WHEN j.status NOT IN ('settled','released') THEN j.hold_amount ELSE 0 END),0),
	 COALESCE(SUM(CASE WHEN j.status NOT IN ('settled','released') THEN j.hold_amount WHEN j.status='settled' AND j.settled_at >= w.ds THEN j.settled_actual_cost ELSE 0 END),0),
	 COALESCE(SUM(CASE WHEN j.status NOT IN ('settled','released') THEN j.hold_amount WHEN j.status='settled' AND j.settled_at >= w.ws THEN j.settled_actual_cost ELSE 0 END),0),
	 COALESCE(SUM(CASE WHEN j.status NOT IN ('settled','released') THEN j.hold_amount WHEN j.status='settled' AND w.ms IS NOT NULL AND j.settled_at >= GREATEST(w.ms,w.mr) THEN j.settled_actual_cost ELSE 0 END),0),
	 w.ds,w.ws,w.ms
	 FROM w LEFT JOIN fork_ai_response_jobs j ON j.user_id=$1 AND j.platform=$2 AND j.subscription_id IS NULL AND j.status<>'released'
	 GROUP BY w.ds,w.ws,w.ms,w.mr`, userID, platform, windows.DailyStart, windows.WeeklyStart).Scan(&result.PendingUSD, &result.DailyUSD, &result.WeeklyUSD, &result.MonthlyUSD, &result.DailyWindowStart, &result.WeeklyWindowStart, &result.MonthlyWindowStart)
	if errors.Is(err, sql.ErrNoRows) {
		err = service.ErrUserNotFound
	}
	result.MonthlyWindowIndependent = result.MonthlyWindowStart != nil && (windows.MonthlyStart.IsZero() || !result.MonthlyWindowStart.Equal(windows.MonthlyStart))
	return result, err
}

// Call only while holding the user billing lock. The first background hold may
// share an existing ordinary month; after that, only its own expiry or an
// explicit administrator reset can change this anchor. Settlement starts a new
// period if a long-running job outlived its reservation period.
func activateAIBackgroundPlatformMonth(ctx context.Context, tx *sql.Tx, userID int64, platform string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO fork_ai_platform_windows(user_id,platform,monthly_window_start)
	 SELECT $1,$2,COALESCE((SELECT monthly_window_start FROM user_platform_quotas
	 WHERE user_id=$1 AND platform=$2 AND deleted_at IS NULL AND monthly_window_start<=clock_timestamp() AND monthly_window_start+INTERVAL '30 days'>clock_timestamp()),clock_timestamp())
	 ON CONFLICT(user_id,platform) DO UPDATE SET monthly_window_start=CASE
	 WHEN fork_ai_platform_windows.monthly_window_start IS NULL THEN EXCLUDED.monthly_window_start
	 WHEN fork_ai_platform_windows.monthly_window_start+INTERVAL '30 days'<=clock_timestamp() THEN clock_timestamp()
	 ELSE fork_ai_platform_windows.monthly_window_start END`, userID, platform)
	return err
}

func checkAIBackgroundPlatformAdmission(ctx context.Context, tx *sql.Tx, j *service.AIRoutingBackgroundJob, now time.Time) error {
	if err := activateAIBackgroundPlatformMonth(ctx, tx, j.UserID, j.Platform); err != nil {
		return err
	}
	var daily, weekly, monthly *float64
	var ds, ws, ms *time.Time
	err := tx.QueryRowContext(ctx, `SELECT daily_limit_usd,weekly_limit_usd,monthly_limit_usd,daily_window_start,weekly_window_start,monthly_window_start FROM user_platform_quotas WHERE user_id=$1 AND platform=$2 AND deleted_at IS NULL`, j.UserID, j.Platform).Scan(&daily, &weekly, &monthly, &ds, &ws, &ms)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	windows := service.AIRoutingBackgroundPlatformWindows{DailyStart: timezone.StartOfDay(now), WeeklyStart: timezone.StartOfWeek(now)}
	if ms != nil && now.Before(ms.Add(30*24*time.Hour)) {
		windows.MonthlyStart = *ms
	}
	// Never initialize/invalidate ordinary counters: Redis may have dirty usage.
	reserved, err := readAIBackgroundPlatform(ctx, tx, j.UserID, j.Platform, windows)
	if err != nil {
		return err
	}
	for _, v := range []struct {
		usage float64
		limit *float64
	}{{reserved.DailyUSD, daily}, {reserved.WeeklyUSD, weekly}, {reserved.MonthlyUSD, monthly}} {
		if v.limit != nil && v.usage+j.HoldAmount > *v.limit {
			return service.ErrAIBackgroundCapacity
		}
	}
	return nil
}

func saveAIBackgroundSubscriptionWindows(ctx context.Context, tx *sql.Tx, s *service.UserSubscription) error {
	if s.ID <= 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE user_subscriptions SET daily_window_start=$2,weekly_window_start=$3,monthly_window_start=$4,daily_usage_usd=$5,weekly_usage_usd=$6,monthly_usage_usd=$7,updated_at=NOW() WHERE id=$1`, s.ID, s.DailyWindowStart, s.WeeklyWindowStart, s.MonthlyWindowStart, s.DailyUsageUSD, s.WeeklyUsageUSD, s.MonthlyUsageUSD)
	return err
}

func (r *aiRoutingResponsesBackgroundRepository) Reserve(ctx context.Context, input *service.AIRoutingBackgroundJob, maxPending int) (*service.AIRoutingBackgroundJob, error) {
	if err := input.Validate(); err != nil {
		return nil, err
	}
	if maxPending < 1 || maxPending > 64 {
		return nil, service.ErrAIBackgroundCapacity
	}
	j := *input
	j.HoldAmount = service.QuantizeUsageBillingAmount(j.HoldAmount)
	identity, err := json.Marshal(&j)
	if err != nil {
		return nil, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = lockUsageBillingIdentities(ctx, tx, j.UserID, j.SubscriptionID, j.APIKeyID); err != nil {
		return nil, err
	}
	old, err := scanAIBackground(tx.QueryRowContext(ctx, `SELECT `+aiBackgroundColumns+` FROM fork_ai_response_jobs WHERE id=$1 FOR UPDATE`, j.ID))
	if err == nil {
		var same bool
		if err = tx.QueryRowContext(ctx, `SELECT identity=$2::jsonb FROM fork_ai_response_jobs WHERE id=$1`, j.ID, string(identity)).Scan(&same); err != nil {
			return nil, err
		}
		if !same {
			return nil, service.ErrAIBackgroundConflict
		}
		return old, nil
	}
	if !errors.Is(err, service.ErrAIBackgroundNotFound) {
		return nil, err
	}
	a, err := readAIBackgroundAdmission(ctx, tx, j.UserID, j.SubscriptionID, j.APIKeyID)
	if err != nil {
		return nil, err
	}
	if a.UserStatus != service.StatusActive || a.Key.Status != service.StatusAPIKeyActive || a.Key.GroupID == nil || *a.Key.GroupID != j.SourceGroupID || a.Key.ExpiresAt != nil && !a.Now.Before(*a.Key.ExpiresAt) {
		return nil, service.ErrAIBackgroundConflict
	}
	if a.Reserved.Pending >= maxPending {
		return nil, service.ErrAIBackgroundCapacity
	}
	over := func(used, held, limit float64) bool { return limit > 0 && used+held+j.HoldAmount > limit }
	if over(a.Key.QuotaUsed, a.Reserved.KeyQuotaUSD, a.Key.Quota) || over(a.Reserved.KeyEffective5hUsage, a.Reserved.KeyRateUSD, a.Key.RateLimit5h) || over(a.Reserved.KeyEffective1dUsage, a.Reserved.KeyRateUSD, a.Key.RateLimit1d) || over(a.Reserved.KeyEffective7dUsage, a.Reserved.KeyRateUSD, a.Key.RateLimit7d) {
		return nil, service.ErrAIBackgroundCapacity
	}
	if j.SubscriptionID > 0 {
		if a.Subscription.GroupID != j.SourceGroupID || a.Subscription.Status != service.SubscriptionStatusActive || a.Now.Before(a.Subscription.StartsAt) || !a.Now.Before(a.Subscription.ExpiresAt) {
			return nil, service.ErrAIBackgroundConflict
		}
		for _, v := range []struct {
			usage float64
			limit *float64
		}{{a.Reserved.SubscriptionDailyUsage, a.DailyLimit}, {a.Reserved.SubscriptionWeeklyUsage, a.WeeklyLimit}, {a.Reserved.SubscriptionMonthlyUsage, a.MonthlyLimit}} {
			if v.limit != nil && v.usage+a.Reserved.SubscriptionUSD+j.HoldAmount > *v.limit {
				return nil, service.ErrAIBackgroundCapacity
			}
		}
		if err = saveAIBackgroundSubscriptionWindows(ctx, tx, &a.Subscription); err != nil {
			return nil, err
		}
	} else {
		if err = checkAIBackgroundPlatformAdmission(ctx, tx, &j, a.Now); err != nil {
			return nil, err
		}
		if _, err = reserveUsageBillingBatchImageBalance(ctx, tx, &service.BatchImageBalanceHoldCommand{UserID: j.UserID, HoldAmount: j.HoldAmount}); err != nil {
			return nil, err
		}
	}
	row := tx.QueryRowContext(ctx, `INSERT INTO fork_ai_response_jobs(id,user_id,api_key_id,subscription_id,hold_amount,identity,platform,account_id,next_attempt_at) VALUES($1,$2,$3,NULLIF($4,0),$5,$6::jsonb,$7,$8,NOW()+INTERVAL '2 minutes') RETURNING `+aiBackgroundColumns, j.ID, j.UserID, j.APIKeyID, j.SubscriptionID, j.HoldAmount, string(identity), j.Platform, j.AccountID)
	job, err := scanAIBackground(row)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return job, nil
}

func aiBackgroundCAS(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return service.ErrAIBackgroundConflict
	}
	return nil
}

func (r *aiRoutingResponsesBackgroundRepository) MarkCacheSynced(ctx context.Context, id string, version int64) error {
	return aiBackgroundCAS(r.db.ExecContext(ctx, `UPDATE fork_ai_response_jobs SET cache_synced=TRUE,version=version+1,updated_at=NOW() WHERE id=$1 AND version=$2`, id, version))
}
func (r *aiRoutingResponsesBackgroundRepository) MarkSubmitting(ctx context.Context, id string, version int64) error {
	return aiBackgroundCAS(r.db.ExecContext(ctx, `UPDATE fork_ai_response_jobs SET status='submitting',version=version+1,updated_at=NOW(),next_attempt_at=NOW()+INTERVAL '2 minutes' WHERE id=$1 AND version=$2 AND status='admitted' AND cache_synced`, id, version))
}

func (r *aiRoutingResponsesBackgroundRepository) Reschedule(ctx context.Context, id string, version int64, next time.Time) error {
	return aiBackgroundCAS(r.db.ExecContext(ctx, `UPDATE fork_ai_response_jobs SET lease_owner=NULL,lease_until=NULL,next_attempt_at=$3,version=version+1,updated_at=NOW() WHERE id=$1 AND version=$2`, id, version, next))
}
func (r *aiRoutingResponsesBackgroundRepository) MarkSubmissionUnknown(ctx context.Context, id string, version int64) error {
	return aiBackgroundCAS(r.db.ExecContext(ctx, `UPDATE fork_ai_response_jobs SET status='submission_unknown',failure_reason='submission_result_unknown',version=version+1,lease_owner=NULL,lease_until=NULL,next_attempt_at=NOW()+INTERVAL '5 minutes',updated_at=NOW() WHERE id=$1 AND version=$2 AND status='submitting' AND response_id IS NULL`, id, version))
}
func (r *aiRoutingResponsesBackgroundRepository) AttachResponse(ctx context.Context, id, responseID string) error {
	if strings.TrimSpace(responseID) != responseID || len(responseID) < 1 || len(responseID) > 512 {
		return service.ErrAIBackgroundConflict
	}
	result, err := r.db.ExecContext(ctx, `UPDATE fork_ai_response_jobs SET response_id=$2,status='active',failure_reason='',version=version+1,next_attempt_at=NOW(),updated_at=NOW() WHERE id=$1 AND status IN ('submitting','submission_unknown') AND response_id IS NULL`, id, responseID)
	var pg *pq.Error
	if errors.As(err, &pg) && pg.Code == "23505" && pg.Constraint == "fork_ai_response_jobs_account_response" {
		_, recordErr := r.db.ExecContext(ctx, `UPDATE fork_ai_response_jobs SET status='submission_unknown',failure_reason='response_id_conflict',version=version+1,lease_owner=NULL,lease_until=NULL,next_attempt_at=NOW()+INTERVAL '5 minutes',updated_at=NOW() WHERE id=$1 AND response_id IS NULL AND status IN ('submitting','submission_unknown')`, id)
		if recordErr != nil {
			return recordErr
		}
		return service.ErrAIBackgroundConflict
	}
	casErr := aiBackgroundCAS(result, err)
	if errors.Is(casErr, service.ErrAIBackgroundConflict) {
		job, loadErr := r.Get(ctx, id)
		if loadErr != nil {
			return loadErr
		}
		if job.ResponseID == responseID && (job.Status == service.AIBackgroundActive || job.Status == service.AIBackgroundUsageUnknown || job.Status == service.AIBackgroundReady || job.Status == service.AIBackgroundSettled) {
			return nil
		}
	}
	return casErr
}

// Claims are lease/version fenced; claimed jobs never represent permission to
// send a new POST. A submitting job with no ID must become submission_unknown.
func (r *aiRoutingResponsesBackgroundRepository) ClaimDue(ctx context.Context, owner string, lease time.Duration, limit int) ([]*service.AIRoutingBackgroundJob, error) {
	if owner == "" || len(owner) > 128 || lease < time.Second || lease > 10*time.Minute || limit < 1 || limit > 64 {
		return nil, service.ErrAIBackgroundConflict
	}
	rows, err := r.db.QueryContext(ctx, `WITH due AS (SELECT id FROM fork_ai_response_jobs WHERE (status NOT IN ('settled','released') OR NOT cache_synced) AND next_attempt_at<=NOW() AND (lease_until IS NULL OR lease_until<=NOW()) ORDER BY next_attempt_at,id FOR UPDATE SKIP LOCKED LIMIT $3) UPDATE fork_ai_response_jobs j SET lease_owner=$1,lease_until=NOW()+($2*INTERVAL '1 second'),version=j.version+1,updated_at=NOW() FROM due WHERE j.id=due.id RETURNING j.`+strings.ReplaceAll(aiBackgroundColumns, ",", ",j."), owner, lease.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var jobs []*service.AIRoutingBackgroundJob
	for rows.Next() {
		j, err := scanAIBackground(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func (r *aiRoutingResponsesBackgroundRepository) SaveTerminal(ctx context.Context, id string, version int64, terminal service.AIRoutingBackgroundTerminal, next time.Time) error {
	data, status, err := encodeAIBackgroundTerminal(terminal)
	if err != nil {
		return err
	}
	return aiBackgroundCAS(r.db.ExecContext(ctx, `UPDATE fork_ai_response_jobs SET terminal=$3::jsonb,status=$4,failure_reason=CASE WHEN $4='usage_unknown' THEN 'usage_missing_or_unsupported' ELSE '' END,version=version+1,lease_owner=NULL,lease_until=NULL,next_attempt_at=$5,updated_at=NOW() WHERE id=$1 AND version=$2 AND status IN ('active','usage_unknown') AND response_id IS NOT NULL`, id, version, string(data), status, next))
}

func encodeAIBackgroundTerminal(terminal service.AIRoutingBackgroundTerminal) ([]byte, string, error) {
	if terminal.Status != "completed" && terminal.Status != "failed" && terminal.Status != "cancelled" && terminal.Status != "incomplete" {
		return nil, "", service.ErrAIBackgroundConflict
	}
	if terminal.InputTokens < 0 || terminal.OutputTokens < 0 || terminal.CacheReadTokens < 0 || terminal.CacheReadTokens > terminal.InputTokens || terminal.CacheCreationTokens < 0 || terminal.CacheCreationTokens > terminal.InputTokens-terminal.CacheReadTokens || terminal.ImageInputTokens < 0 || terminal.ImageInputTokens > terminal.InputTokens || terminal.ImageCacheReadTokens < 0 || terminal.ImageCacheReadTokens > terminal.CacheReadTokens || terminal.ImageCacheReadTokens > terminal.ImageInputTokens || terminal.ImageOutputTokens < 0 || terminal.ImageOutputTokens > terminal.OutputTokens || len(terminal.Model) > 256 || len(terminal.ServiceTier) > 64 {
		return nil, "", service.ErrAIBackgroundConflict
	}
	data, err := json.Marshal(terminal)
	if err != nil {
		return nil, "", err
	}
	status := service.AIBackgroundUsageUnknown
	if terminal.UsageKnown {
		status = service.AIBackgroundReady
	}
	return data, status, nil
}

// Observations hold only the job row and never acquire a billing identity lock.
// Settlement's user -> subscription -> key -> job order cannot form a cycle.
// Known evidence is immutable; duplicates do not bump versions or steal leases.
func (r *aiRoutingResponsesBackgroundRepository) RecordBackgroundTerminal(ctx context.Context, id string, accountID int64, responseID string, terminal service.AIRoutingBackgroundTerminal) error {
	data, status, err := encodeAIBackgroundTerminal(terminal)
	if err != nil {
		return err
	}
	if id == "" || accountID <= 0 || responseID == "" {
		return service.ErrAIBackgroundConflict
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	j, err := scanAIBackground(tx.QueryRowContext(ctx, `SELECT `+aiBackgroundColumns+` FROM fork_ai_response_jobs WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return err
	}
	if j.AccountID != accountID || j.ResponseID != responseID {
		return service.ErrAIBackgroundConflict
	}
	if j.Terminal != nil {
		if j.Terminal.UsageKnown {
			if (j.Status == service.AIBackgroundReady || j.Status == service.AIBackgroundSettled) && (!terminal.UsageKnown || *j.Terminal == terminal) {
				return nil
			}
			return service.ErrAIBackgroundConflict
		}
		if j.Status == service.AIBackgroundUsageUnknown && *j.Terminal == terminal {
			return nil
		}
	}
	if j.Status != service.AIBackgroundActive && j.Status != service.AIBackgroundUsageUnknown {
		return service.ErrAIBackgroundConflict
	}
	_, err = tx.ExecContext(ctx, `UPDATE fork_ai_response_jobs SET terminal=$2::jsonb,status=$3,failure_reason=CASE WHEN $3='usage_unknown' THEN 'usage_missing_or_unsupported' ELSE '' END,version=version+1,lease_owner=NULL,lease_until=NULL,next_attempt_at=clock_timestamp(),updated_at=clock_timestamp() WHERE id=$1`, id, string(data), status)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func validateAIBackgroundBilling(j *service.AIRoutingBackgroundJob, cmd *service.UsageBillingCommand) error {
	if cmd == nil || j.Terminal == nil || !j.Terminal.UsageKnown {
		return service.ErrAIBackgroundUnknownUsage
	}
	if cmd.RequestID != service.AIRoutingBackgroundBillingRequestID(j.ID) || cmd.UserID != j.UserID || cmd.APIKeyID != j.APIKeyID || cmd.AccountID != j.AccountID || cmd.Model != j.ExecutionModel || cmd.RequestPayloadHash != j.RequestPayloadHash || cmd.InputTokens != j.Terminal.InputTokens-j.Terminal.CacheReadTokens-j.Terminal.CacheCreationTokens || cmd.OutputTokens != j.Terminal.OutputTokens || cmd.CacheReadTokens != j.Terminal.CacheReadTokens || cmd.CacheCreationTokens != j.Terminal.CacheCreationTokens {
		return service.ErrAIBackgroundConflict
	}
	subID := int64(0)
	if cmd.SubscriptionID != nil {
		subID = *cmd.SubscriptionID
	}
	if subID != j.SubscriptionID || j.SubscriptionID > 0 && cmd.BalanceCost != 0 || j.SubscriptionID == 0 && cmd.SubscriptionCost != 0 {
		return service.ErrAIBackgroundConflict
	}
	for _, amount := range []float64{cmd.BalanceCost, cmd.SubscriptionCost, cmd.APIKeyQuotaCost, cmd.APIKeyRateLimitCost, cmd.AccountQuotaCost} {
		if math.IsNaN(amount) || math.IsInf(amount, 0) || amount < 0 {
			return service.ErrAIBackgroundConflict
		}
	}
	return nil
}

// Finish frozen funds and apply the ordinary usage effects in the SAME SQL
// transaction as the stable billing dedup and ledger transition. An overage is
// historical cost, not permission to discard usage, and can create balance debt.
func (r *aiRoutingResponsesBackgroundRepository) Settle(ctx context.Context, id string, version int64, input *service.UsageBillingCommand) (*service.UsageBillingApplyResult, error) {
	j, err := r.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if err = validateAIBackgroundBilling(j, input); err != nil {
		return nil, err
	}
	cmd := *input
	cmd.Normalize()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = lockUsageBillingIdentities(ctx, tx, j.UserID, j.SubscriptionID, j.APIKeyID); err != nil {
		return nil, err
	}
	j, err = scanAIBackground(tx.QueryRowContext(ctx, `SELECT `+aiBackgroundColumns+` FROM fork_ai_response_jobs WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return nil, err
	}
	if err = validateAIBackgroundBilling(j, &cmd); err != nil {
		return nil, err
	}
	if j.Status == service.AIBackgroundSettled {
		if j.BillingCommand == nil || j.BillingCommand.RequestFingerprint != cmd.RequestFingerprint {
			return nil, service.ErrUsageBillingRequestConflict
		}
		return &service.UsageBillingApplyResult{Applied: false}, nil
	}
	if j.Version != version || j.Status != service.AIBackgroundReady {
		return nil, service.ErrAIBackgroundConflict
	}
	billing := usageBillingRepository{db: r.db}
	applied, err := billing.claimUsageBillingKey(ctx, tx, &cmd)
	if err != nil {
		return nil, err
	}
	if !applied {
		return nil, service.ErrAIBackgroundConflict
	}
	result := &service.UsageBillingApplyResult{Applied: true}
	if j.SubscriptionID > 0 {
		// Deleted/disabled keys never prevent settling already incurred costs.
		a, readErr := readAIBackgroundAdmission(ctx, tx, j.UserID, j.SubscriptionID, 0)
		if readErr != nil {
			return nil, readErr
		}
		if err = saveAIBackgroundSubscriptionWindows(ctx, tx, &a.Subscription); err != nil {
			return nil, err
		}
	} else {
		if err = activateAIBackgroundPlatformMonth(ctx, tx, j.UserID, j.Platform); err != nil {
			return nil, err
		}
		var balance float64
		err = tx.QueryRowContext(ctx, `UPDATE users SET balance=balance+$2-$3,frozen_balance=frozen_balance-$2,updated_at=NOW() WHERE id=$1 AND deleted_at IS NULL AND frozen_balance>=$2 RETURNING balance`, j.UserID, j.HoldAmount, cmd.BalanceCost).Scan(&balance)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrAIBackgroundConflict
		}
		if err != nil {
			return nil, err
		}
		result.NewBalance = &balance
		result.BalanceOverdrafted = balance < 0
	}
	effects := cmd
	effects.BalanceCost = 0
	if err = billing.applyUsageBillingEffects(ctx, tx, &effects, result); err != nil {
		return nil, err
	}
	data, err := json.Marshal(&cmd)
	if err != nil {
		return nil, err
	}
	actualCost := cmd.BalanceCost
	if j.SubscriptionID > 0 {
		actualCost = cmd.SubscriptionCost
	}
	_, err = tx.ExecContext(ctx, `UPDATE fork_ai_response_jobs SET status='settled',billing_command=$2::jsonb,settled_actual_cost=$3,cache_synced=FALSE,version=version+1,lease_owner=NULL,lease_until=NULL,next_attempt_at=NOW(),settled_at=clock_timestamp(),updated_at=NOW() WHERE id=$1`, id, string(data), actualCost)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func (r *aiRoutingResponsesBackgroundRepository) ReleaseUnsubmitted(ctx context.Context, id string, version int64) error {
	j, err := r.Get(ctx, id)
	if err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = lockUsageBillingIdentities(ctx, tx, j.UserID, j.SubscriptionID, j.APIKeyID); err != nil {
		return err
	}
	j, err = scanAIBackground(tx.QueryRowContext(ctx, `SELECT `+aiBackgroundColumns+` FROM fork_ai_response_jobs WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return err
	}
	if j.Status == service.AIBackgroundReleased {
		return nil
	}
	if j.Version != version || j.Status != service.AIBackgroundAdmitted || j.ResponseID != "" {
		return service.ErrAIBackgroundConflict
	}
	if j.SubscriptionID == 0 {
		result, err := tx.ExecContext(ctx, `UPDATE users SET balance=balance+$2,frozen_balance=frozen_balance-$2,updated_at=NOW() WHERE id=$1 AND deleted_at IS NULL AND frozen_balance>=$2`, j.UserID, j.HoldAmount)
		if err = aiBackgroundCAS(result, err); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE fork_ai_response_jobs SET status='released',cache_synced=FALSE,version=version+1,lease_owner=NULL,lease_until=NULL,next_attempt_at=NOW(),updated_at=NOW() WHERE id=$1`, id)
	if err != nil {
		return err
	}
	return tx.Commit()
}
