package service

import (
	"encoding/json"
	"time"
)

const (
	modelQuotaSchemaVersion = 1
	modelQuotaOutputField   = "sub2api_quota"

	modelQuotaAvailable  = "available"
	modelQuotaExhausted  = "exhausted"
	modelQuotaBlocked    = "blocked"
	modelQuotaUnknown    = "unknown"
	modelQuotaNotBlocked = "not_blocked_by_observed_limits"
)

type ModelQuotaResponse struct {
	Object string                       `json:"object"`
	Data   []map[string]json.RawMessage `json:"data"`
	Quota  ModelQuotaEnvelope           `json:"sub2api_quota"`
}

type ModelQuotaEnvelope struct {
	SchemaVersion int                    `json:"schema_version"`
	Endpoint      string                 `json:"endpoint"`
	QueriedAt     time.Time              `json:"queried_at"`
	KeyAdmission  ModelQuotaKeyAdmission `json:"key_admission"`
}

type ModelQuotaModel struct {
	Status            string             `json:"status"`
	RecoversAt        *time.Time         `json:"recovers_at"`
	AccountsAvailable int                `json:"accounts_available"`
	AccountsTotal     int                `json:"accounts_total"`
	Windows           []ModelQuotaWindow `json:"windows"`
	Reasons           []string           `json:"reasons,omitempty"`
}

type ModelQuotaWindow struct {
	Window      string     `json:"window"`
	UsedPercent float64    `json:"used_percent"`
	Pool        string     `json:"pool"`
	ResetsAt    *time.Time `json:"resets_at"`
	ObservedAt  *time.Time `json:"observed_at"`
}

type ModelQuotaKeyAdmission struct {
	Status        string              `json:"status"`
	Reasons       []string            `json:"reasons"`
	Unevaluated   []string            `json:"unevaluated"`
	NotApplicable []string            `json:"not_applicable,omitempty"`
	Recovery      *ModelQuotaRecovery `json:"recovery"`
}

type ModelQuotaRecovery struct {
	At          time.Time `json:"at"`
	InMinutes   int       `json:"in_minutes"`
	Conditional bool      `json:"conditional"`
}

type modelQuotaConstraint struct {
	identity    string
	window      string
	usedPercent *float64
	observedAt  *time.Time
	resetsAt    *time.Time
	exhausted   bool
	reason      string
}

type modelQuotaConstraintSet struct {
	windows  []modelQuotaConstraint
	reasons  []string
	complete bool
}

func (s *modelQuotaConstraintSet) merge(other modelQuotaConstraintSet) {
	s.windows = append(s.windows, other.windows...)
	s.reasons = append(s.reasons, other.reasons...)
	s.complete = s.complete && other.complete
}

type modelQuotaCandidate struct {
	account       *Account
	upstreamModel string
	constraints   modelQuotaConstraintSet
	cooldownUntil *time.Time
	blockedReason string
	blockedUntil  *time.Time
}

var modelQuotaDisplayZone = time.FixedZone("UTC+08:00", 8*60*60)

func modelQuotaDisplayTime(t time.Time) time.Time {
	return t.In(modelQuotaDisplayZone)
}

func modelQuotaDisplayTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	formatted := modelQuotaDisplayTime(*t)
	return &formatted
}

func timePtrIfSet(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func newModelQuotaRecovery(now, at time.Time) *ModelQuotaRecovery {
	if !at.After(now) {
		return nil
	}
	diff := at.Sub(now)
	minutes := int(diff / time.Minute)
	if diff%time.Minute != 0 {
		minutes++
	}
	return &ModelQuotaRecovery{At: modelQuotaDisplayTime(at), InMinutes: minutes, Conditional: true}
}
