package service

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
)

var (
	ErrAIBackgroundConflict     = errors.New("background response state conflict")
	ErrAIBackgroundCapacity     = errors.New("background response pending or reserved quota exceeded")
	ErrAIBackgroundNotFound     = errors.New("background response job not found")
	ErrAIBackgroundUnknownUsage = errors.New("background response terminal usage is unknown")
)

const (
	AIBackgroundAdmitted          = "admitted"
	AIBackgroundSubmitting        = "submitting"
	AIBackgroundSubmissionUnknown = "submission_unknown"
	AIBackgroundActive            = "active"
	AIBackgroundUsageUnknown      = "usage_unknown"
	AIBackgroundReady             = "ready"
	AIBackgroundSettled           = "settled"
	AIBackgroundReleased          = "released"
)

// AIRoutingBackgroundJob persists only execution/billing identity and a
// whitelisted price snapshot. It never contains credentials or input/output text.
type AIRoutingBackgroundJob struct {
	ID                     string                       `json:"id"`
	OriginalRequestID      string                       `json:"original_request_id,omitempty"`
	ClientRequestID        string                       `json:"client_request_id,omitempty"`
	OriginalUsageRequestID string                       `json:"original_usage_request_id,omitempty"`
	SourceGroupID          int64                        `json:"source_group_id"`
	UserID                 int64                        `json:"user_id"`
	APIKeyID               int64                        `json:"api_key_id"`
	AccountID              int64                        `json:"account_id"`
	SubscriptionID         int64                        `json:"subscription_id,omitempty"`
	CandidateID            string                       `json:"candidate_id"`
	PublicModel            string                       `json:"public_model"`
	ExecutionModel         string                       `json:"execution_model"`
	Platform               string                       `json:"platform"`
	TargetGroupID          int64                        `json:"target_group_id,omitempty"`
	TargetAccountID        int64                        `json:"target_account_id,omitempty"`
	PolicyVersion          int64                        `json:"policy_version"`
	RequestPayloadHash     string                       `json:"request_payload_hash"`
	HoldAmount             float64                      `json:"hold_amount"`
	PricingSnapshot        json.RawMessage              `json:"pricing_snapshot"`
	ResponseID             string                       `json:"-"`
	Status                 string                       `json:"-"`
	FailureReason          string                       `json:"-"`
	Version                int64                        `json:"-"`
	CacheSynced            bool                         `json:"-"`
	LeaseOwner             string                       `json:"-"`
	LeaseUntil             *time.Time                   `json:"-"`
	NextAttemptAt          time.Time                    `json:"-"`
	CreatedAt              time.Time                    `json:"-"`
	UpdatedAt              time.Time                    `json:"-"`
	SettledAt              *time.Time                   `json:"-"`
	Terminal               *AIRoutingBackgroundTerminal `json:"-"`
	BillingCommand         *UsageBillingCommand         `json:"-"`
}

type AIRoutingBackgroundTerminal struct {
	Status               string `json:"status"`
	Model                string `json:"model"`
	UsageKnown           bool   `json:"usage_known"`
	InputTokens          int    `json:"input_tokens"`
	OutputTokens         int    `json:"output_tokens"`
	CacheReadTokens      int    `json:"cache_read_tokens"`
	CacheCreationTokens  int    `json:"cache_creation_tokens,omitempty"`
	ImageInputTokens     int    `json:"image_input_tokens,omitempty"`
	ImageCacheReadTokens int    `json:"image_cache_read_tokens,omitempty"`
	ImageOutputTokens    int    `json:"image_output_tokens,omitempty"`
	ServiceTier          string `json:"service_tier,omitempty"`
}

// Pending amounts remain reserved across quota-window resets. They are removed
// only when released before submission or when authoritative usage is settled.
type AIRoutingBackgroundReserved struct {
	UserID                   int64
	SubscriptionID           int64
	APIKeyID                 int64
	UserBalance              float64
	SubscriptionDailyUsage   float64
	SubscriptionWeeklyUsage  float64
	SubscriptionMonthlyUsage float64
	KeyQuotaUsed             float64
	KeyEffective5hUsage      float64
	KeyEffective1dUsage      float64
	KeyEffective7dUsage      float64
	SubscriptionUSD          float64
	KeyQuotaUSD              float64
	KeyRateUSD               float64
	Pending                  int
}

type AIRoutingBackgroundPlatformWindows struct {
	DailyStart   time.Time
	WeeklyStart  time.Time
	MonthlyStart time.Time
}

// Platform actuals are a separate durable contribution, never copied into the
// ordinary Redis usage counters or their absolute-value database snapshots.
type AIRoutingBackgroundPlatformReserved struct {
	PendingUSD               float64
	DailyUSD                 float64
	WeeklyUSD                float64
	MonthlyUSD               float64
	DailyWindowStart         *time.Time
	WeeklyWindowStart        *time.Time
	MonthlyWindowStart       *time.Time
	MonthlyWindowIndependent bool
}

type AIRoutingBackgroundRepository interface {
	Reserve(context.Context, *AIRoutingBackgroundJob, int) (*AIRoutingBackgroundJob, error)
	Get(context.Context, string) (*AIRoutingBackgroundJob, error)
	Reserved(ctx context.Context, userID, subscriptionID, keyID int64) (AIRoutingBackgroundReserved, error)
	ReservedPlatform(context.Context, int64, string, AIRoutingBackgroundPlatformWindows) (AIRoutingBackgroundPlatformReserved, error)
	MarkCacheSynced(context.Context, string, int64) error
	MarkSubmitting(context.Context, string, int64) error
	AttachResponse(context.Context, string, string) error
	MarkSubmissionUnknown(context.Context, string, int64) error
	ClaimDue(context.Context, string, time.Duration, int) ([]*AIRoutingBackgroundJob, error)
	Reschedule(context.Context, string, int64, time.Time) error
	SaveTerminal(context.Context, string, int64, AIRoutingBackgroundTerminal, time.Time) error
	Settle(context.Context, string, int64, *UsageBillingCommand) (*UsageBillingApplyResult, error)
	ReleaseUnsubmitted(context.Context, string, int64) error
}

func AIRoutingBackgroundBillingRequestID(id string) string {
	return "responses-background:" + strings.TrimSpace(id)
}

// Uses the same calendar/period anchors as ordinary subscription maintenance;
// repository callers apply this snapshot while holding that subscription row.
func AIRoutingBackgroundAdvanceSubscriptionWindows(sub *UserSubscription, now time.Time) {
	if sub == nil {
		return
	}
	if sub.DailyWindowStart == nil {
		start := timezone.StartOfDay(now)
		sub.DailyWindowStart = &start
	} else if start, ok := sub.automaticDailyWindowStartAt(now); ok {
		sub.DailyWindowStart = &start
		sub.DailyUsageUSD = 0
	}
	if sub.WeeklyWindowStart == nil {
		start := now
		sub.WeeklyWindowStart = &start
	} else if start, ok := sub.automaticWindowStartAt(sub.WeeklyWindowStart, 7*24*time.Hour, now); ok {
		sub.WeeklyWindowStart = &start
		sub.WeeklyUsageUSD = 0
	}
	if sub.MonthlyWindowStart == nil {
		start := now
		sub.MonthlyWindowStart = &start
	} else if start, ok := sub.automaticWindowStartAt(sub.MonthlyWindowStart, 30*24*time.Hour, now); ok {
		sub.MonthlyWindowStart = &start
		sub.MonthlyUsageUSD = 0
	}
}

func (j *AIRoutingBackgroundJob) Validate() error {
	if j == nil || len(j.ID) < 16 || len(j.ID) > 128 || strings.TrimSpace(j.ID) != j.ID || j.SourceGroupID <= 0 || j.UserID <= 0 || j.APIKeyID <= 0 || j.AccountID <= 0 || j.SubscriptionID < 0 || j.PolicyVersion <= 0 || len(j.RequestPayloadHash) != 64 || j.CandidateID == "" || j.PublicModel == "" || j.ExecutionModel == "" || j.Platform == "" || (j.TargetGroupID > 0) == (j.TargetAccountID > 0) || j.TargetGroupID < 0 || j.TargetAccountID < 0 || j.TargetAccountID > 0 && j.TargetAccountID != j.AccountID || math.IsNaN(j.HoldAmount) || math.IsInf(j.HoldAmount, 0) || j.HoldAmount < 0 {
		return errors.New("invalid background response billing identity")
	}
	if len(j.PricingSnapshot) == 0 || len(j.PricingSnapshot) > 64<<10 || !requestmodel.HasUniqueJSONMembers(j.PricingSnapshot) {
		return errors.New("invalid background response price snapshot")
	}
	var prices map[string]json.RawMessage
	if json.Unmarshal(j.PricingSnapshot, &prices) != nil || prices == nil {
		return errors.New("invalid background response price snapshot")
	}
	return nil
}
