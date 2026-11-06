package service

import (
	"context"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

type AIRoutingResponsesBackgroundService struct {
	repository AIRoutingBackgroundRepository
	gateway    *OpenAIGatewayService
	worker     AIRoutingBackgroundWorker
	mu         sync.Mutex
	cancel     context.CancelFunc
	done       chan struct{}
}

func NewAIRoutingResponsesBackgroundService(repository AIRoutingBackgroundRepository, gateway *OpenAIGatewayService) *AIRoutingResponsesBackgroundService {
	s := &AIRoutingResponsesBackgroundService{repository: repository, gateway: gateway}
	s.worker = AIRoutingBackgroundWorker{Repository: repository, Owner: uuid.NewString(), Hooks: AIRoutingBackgroundWorkerHooks{Fetch: s.fetch, BuildCommand: s.buildCommand, Sync: s.sync}}
	return s
}

func (s *AIRoutingResponsesBackgroundService) Ready() bool {
	return s != nil && s.repository != nil && s.gateway != nil && s.gateway.accountRepo != nil && s.gateway.httpUpstream != nil && s.gateway.billingService != nil && s.gateway.billingCacheService != nil && s.gateway.usageLogRepo != nil
}
func (s *AIRoutingResponsesBackgroundService) Repository() AIRoutingBackgroundRepository {
	if s == nil {
		return nil
	}
	return s.repository
}

// The final transport callback sees only the selected account ID. Reload the
// current account before freezing prices; never capture an earlier scheduler
// snapshot or let the callback reach the gateway's private repository.
func (s *AIRoutingResponsesBackgroundService) PrepareForAccount(ctx context.Context, job *AIRoutingBackgroundJob, key *APIKey, user *User, subscription *UserSubscription, finalBody []byte, pricingAt time.Time) (*AIRoutingBackgroundJob, error) {
	if !s.Ready() || job == nil {
		return nil, ErrAIBackgroundUnsupported
	}
	account, err := s.gateway.accountRepo.GetByID(ctx, job.AccountID)
	if err != nil {
		return nil, err
	}
	if account == nil || account.ID != job.AccountID {
		return nil, ErrAIBackgroundUnsupported
	}
	return s.Prepare(ctx, job, key, user, account, subscription, finalBody, pricingAt)
}
func (s *AIRoutingResponsesBackgroundService) RunOnce(ctx context.Context, limit int) (int, error) {
	if !s.Ready() {
		return 0, ErrAIBackgroundUnsupported
	}
	return s.worker.RunOnce(ctx, limit)
}
func (s *AIRoutingResponsesBackgroundService) Start(ctx context.Context) {
	if !s.Ready() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return
	}
	workCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.done = make(chan struct{})
	done := s.done
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-workCtx.Done():
				return
			case <-ticker.C:
				_, _ = s.worker.RunOnce(workCtx, 16)
			}
		}
	}()
}
func (s *AIRoutingResponsesBackgroundService) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel = nil
	s.done = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// A short ordinary in-flight reservation protects admission until the durable
// hold is visible. Release it only after SQL reserve and synchronous cache
// invalidation (or on failure; an already committed SQL hold remains intact).
func (s *AIRoutingResponsesBackgroundService) ReserveAndSync(ctx context.Context, job *AIRoutingBackgroundJob, key *APIKey, user *User, subscription *UserSubscription, maxPending int) (*AIRoutingBackgroundJob, error) {
	if !s.Ready() || job == nil || key == nil || user == nil || key.Group == nil || job.UserID != user.ID || job.APIKeyID != key.ID || job.SourceGroupID != key.Group.ID {
		return nil, ErrAIBackgroundUnsupported
	}
	if _, err := decodeAIBackgroundPrice(job); err != nil {
		return nil, err
	}
	fixed, err := s.gateway.billingCacheService.ReserveFixedCharge(ctx, user, key, key.Group, subscription, job.HoldAmount)
	if err != nil {
		return nil, err
	}
	if fixed != nil {
		defer fixed.Release()
	}
	var platform *InflightReservation
	if job.SubscriptionID == 0 {
		platform, err = s.gateway.billingCacheService.ReserveAIRoutingBackgroundPlatformHold(ctx, user.ID, job.Platform, job.HoldAmount)
		if err != nil {
			return nil, err
		}
		if platform != nil {
			defer platform.Release()
		}
	}
	if err := platform.checkStrictScope(ctx); err != nil {
		return nil, err
	}
	saved, err := s.repository.Reserve(ctx, job, maxPending)
	if err != nil {
		return nil, err
	}
	if saved.Status != AIBackgroundAdmitted {
		return nil, ErrAIBackgroundConflict
	}
	if err := platform.checkStrictScope(ctx); err != nil {
		return nil, err
	}
	if err = s.sync(ctx, saved); err != nil {
		return nil, err
	}
	if err = s.repository.MarkCacheSynced(ctx, saved.ID, saved.Version); err != nil {
		return nil, err
	}
	return s.repository.Get(ctx, saved.ID)
}

func (s *AIRoutingResponsesBackgroundService) BeginSubmission(ctx context.Context, job *AIRoutingBackgroundJob) error {
	if !s.Ready() || job == nil {
		return ErrAIBackgroundUnsupported
	}
	return s.repository.MarkSubmitting(ctx, job.ID, job.Version)
}
func (s *AIRoutingResponsesBackgroundService) AttachResponse(ctx context.Context, jobID, responseID string) error {
	if !s.Ready() {
		return ErrAIBackgroundUnsupported
	}
	return s.repository.AttachResponse(ctx, jobID, responseID)
}

func (s *AIRoutingResponsesBackgroundService) buildCommand(_ context.Context, j *AIRoutingBackgroundJob) (*UsageBillingCommand, error) {
	p, err := decodeAIBackgroundPrice(j)
	if err != nil {
		return nil, err
	}
	cmd, _, err := p.prepareBilling(j)
	return cmd, err
}

func (s *AIRoutingResponsesBackgroundService) sync(ctx context.Context, j *AIRoutingBackgroundJob) error {
	if s.gateway == nil || s.gateway.billingCacheService == nil {
		return ErrAIBackgroundUnsupported
	}
	cache := s.gateway.billingCacheService
	if err := cache.InvalidateUserBalance(ctx, j.UserID); err != nil {
		return err
	}
	if j.SubscriptionID > 0 {
		if err := cache.InvalidateSubscription(ctx, j.UserID, j.SourceGroupID); err != nil {
			return err
		}
	}
	if err := cache.InvalidateAPIKeyRateLimit(ctx, j.APIKeyID); err != nil {
		return err
	}
	// Platform usage is intentionally NOT incremented or invalidated: ordinary
	// Redis counters may hold dirty usage. The authoritative ledger contributes
	// separately to every read/admission, independent of cache synchronization.
	if j.Status == AIBackgroundSettled {
		if s.gateway.usageLogRepo == nil {
			return ErrAIBackgroundUnsupported
		}
		p, err := decodeAIBackgroundPrice(j)
		if err != nil {
			return err
		}
		cmd, log, err := p.prepareBilling(j)
		if err != nil {
			return err
		}
		if j.BillingCommand == nil || cmd.RequestFingerprint != j.BillingCommand.RequestFingerprint {
			return ErrUsageBillingRequestConflict
		}
		// Create is synchronous and returns failures. Best-effort helpers swallow
		// errors and cannot clear a durable cache/log repair obligation.
		_, err = s.gateway.usageLogRepo.Create(ctx, log)
		return err
	}
	return nil
}

func (s *AIRoutingResponsesBackgroundService) fetch(ctx context.Context, j *AIRoutingBackgroundJob) (AIRoutingBackgroundTerminal, bool, error) {
	if j == nil || j.ResponseID == "" || !s.Ready() {
		return AIRoutingBackgroundTerminal{}, false, ErrAIBackgroundUnsupported
	}
	account, err := s.gateway.accountRepo.GetByID(ctx, j.AccountID)
	if err != nil {
		return AIRoutingBackgroundTerminal{}, false, err
	}
	prices, err := decodeAIBackgroundPrice(j)
	if err != nil || prices.EndpointHash == "" || prices.EndpointHash != aiBackgroundEndpointHash(account) {
		return AIRoutingBackgroundTerminal{}, false, ErrAIBackgroundRecoveryPending
	}
	// No current user/key/policy permission is required to settle costs already
	// incurred. The original account and native resource endpoint remain fixed.
	response, err := s.gateway.ForwardAIRoutingResponsesControl(ctx, account, j.ExecutionModel, http.MethodGet, j.ResponseID, "", "")
	if err != nil {
		return AIRoutingBackgroundTerminal{}, false, err
	}
	if response == nil || response.Body == nil {
		return AIRoutingBackgroundTerminal{}, false, ErrAIBackgroundRecoveryPending
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return AIRoutingBackgroundTerminal{}, false, ErrAIBackgroundRecoveryPending
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (4<<20)+1))
	if err != nil || len(raw) > 4<<20 {
		return AIRoutingBackgroundTerminal{}, false, ErrAIBackgroundRecoveryPending
	}
	return parseAIBackgroundTerminal(raw, j.ResponseID)
}

func parseAIBackgroundTerminal(raw []byte, responseID string) (AIRoutingBackgroundTerminal, bool, error) {
	t := AIRoutingBackgroundTerminal{}
	if len(raw) > 4<<20 || !requestmodel.HasUniqueJSONMembers(raw) {
		return t, false, ErrAIBackgroundRecoveryPending
	}
	root := gjson.ParseBytes(raw)
	if !root.IsObject() || !aiCanonicalFields(root, "id", "object", "status", "model", "service_tier", "usage", "output", "tool_usage") || root.Get("id").Type != gjson.String || root.Get("id").String() != responseID {
		return t, false, ErrAIBackgroundRecoveryPending
	}
	if object := root.Get("object"); object.Exists() && (object.Type != gjson.String || object.Str != "response") {
		return t, false, ErrAIBackgroundRecoveryPending
	}
	if root.Get("status").Type != gjson.String {
		return t, false, ErrAIBackgroundRecoveryPending
	}
	for _, name := range []string{"model", "service_tier"} {
		if value := root.Get(name); value.Exists() && value.Type != gjson.Null && value.Type != gjson.String {
			return t, false, ErrAIBackgroundRecoveryPending
		}
	}
	t.Status = root.Get("status").String()
	t.Model = root.Get("model").String()
	t.ServiceTier = root.Get("service_tier").String()
	switch t.Status {
	case "queued", "in_progress":
		return t, false, nil
	case "completed", "failed", "cancelled", "incomplete":
	default:
		return t, false, ErrAIBackgroundRecoveryPending
	}
	if len(t.Model) > 256 || len(t.ServiceTier) > 64 {
		return t, false, ErrAIBackgroundRecoveryPending
	}
	u := root.Get("usage")
	if !u.IsObject() {
		return t, true, nil
	}
	// Do not treat an incomplete/ambiguous usage object as authoritative zero.
	if !aiCanonicalFields(u, "input_tokens", "output_tokens", "total_tokens", "input_tokens_details", "output_tokens_details", "cache_creation_input_tokens", "cache_read_input_tokens", "cache_write_tokens", "cache_creation_tokens", "cache_write_input_tokens", "cache_read_tokens", "cached_tokens", "prompt_cache_hit_tokens", "prompt_tokens", "completion_tokens", "prompt_tokens_details", "completion_tokens_details") {
		return t, false, ErrAIBackgroundRecoveryPending
	}
	for _, field := range []string{"input_tokens", "output_tokens"} {
		v := u.Get(field)
		if !aiBackgroundUsageInteger(v) {
			return t, true, nil
		}
	}
	for _, field := range []string{"total_tokens", "cache_creation_input_tokens", "cache_read_input_tokens", "cache_write_tokens", "cache_creation_tokens", "cache_write_input_tokens", "cache_read_tokens", "cached_tokens", "prompt_cache_hit_tokens", "prompt_tokens", "completion_tokens"} {
		v := u.Get(field)
		if v.Exists() && !aiBackgroundUsageInteger(v) {
			return t, true, nil
		}
	}
	for _, name := range []string{"input_tokens_details", "output_tokens_details", "prompt_tokens_details", "completion_tokens_details"} {
		details := u.Get(name)
		if !details.Exists() || details.Type == gjson.Null {
			continue
		}
		if !details.IsObject() || !aiCanonicalFields(details, "cached_tokens", "cache_write_tokens", "cache_creation_tokens", "image_tokens", "text_tokens", "reasoning_tokens", "audio_tokens", "cached_tokens_details") {
			return t, false, ErrAIBackgroundRecoveryPending
		}
		valid := true
		details.ForEach(func(key, value gjson.Result) bool {
			if key.String() == "cached_tokens_details" {
				return true
			}
			if !aiBackgroundUsageInteger(value) {
				valid = false
			}
			return true
		})
		if !valid || details.Get("audio_tokens").Int() > 0 {
			return t, true, nil
		}
		if cached := details.Get("cached_tokens_details"); cached.Exists() && cached.Type != gjson.Null {
			if !cached.IsObject() || !aiCanonicalFields(cached, "image_tokens", "text_tokens", "audio_tokens") {
				return t, false, ErrAIBackgroundRecoveryPending
			}
			cached.ForEach(func(_, value gjson.Result) bool {
				if !aiBackgroundUsageInteger(value) {
					valid = false
				}
				return true
			})
			if !valid || cached.Get("audio_tokens").Int() > 0 {
				return t, true, nil
			}
		}
	}
	if root.Get("tool_usage").Exists() {
		return t, true, nil
	}
	if output := root.Get("output"); output.Exists() && output.Type != gjson.Null && !output.IsArray() {
		return t, true, nil
	}
	for _, item := range root.Get("output").Array() {
		if !item.IsObject() || !aiCanonicalFields(item, "type") {
			return t, false, ErrAIBackgroundRecoveryPending
		}
		switch item.Get("type").String() {
		case "message":
			for _, part := range item.Get("content").Array() {
				if !part.IsObject() || !aiCanonicalFields(part, "type") {
					return t, false, ErrAIBackgroundRecoveryPending
				}
				if part.Get("type").String() != "output_text" && part.Get("type").String() != "refusal" {
					return t, true, nil
				}
			}
		case "function_call", "custom_tool_call", "reasoning", "compaction":
		default:
			return t, true, nil
		}
	}
	// Native Responses canonical counters are authoritative, including zero.
	// Ordinary compatibility parsing intentionally falls back across protocols;
	// durable accounting must not reinterpret a contradictory alias as cost.
	groups := [][]string{
		{"input_tokens", "prompt_tokens"},
		{"output_tokens", "completion_tokens"},
		{"input_tokens_details.cached_tokens", "prompt_tokens_details.cached_tokens", "cache_read_input_tokens", "cache_read_tokens", "cached_tokens", "prompt_cache_hit_tokens"},
		{"input_tokens_details.cache_write_tokens", "prompt_tokens_details.cache_write_tokens", "input_tokens_details.cache_creation_tokens", "prompt_tokens_details.cache_creation_tokens", "cache_write_tokens", "cache_creation_input_tokens", "cache_write_input_tokens", "cache_creation_tokens"},
		{"input_tokens_details.image_tokens", "prompt_tokens_details.image_tokens"},
		{"output_tokens_details.image_tokens", "completion_tokens_details.image_tokens"},
		{"input_tokens_details.cached_tokens_details.image_tokens", "prompt_tokens_details.cached_tokens_details.image_tokens"},
		{"output_tokens_details.reasoning_tokens", "completion_tokens_details.reasoning_tokens"},
		{"input_tokens_details.text_tokens", "prompt_tokens_details.text_tokens"},
		{"output_tokens_details.text_tokens", "completion_tokens_details.text_tokens"},
		{"input_tokens_details.cached_tokens_details.text_tokens", "prompt_tokens_details.cached_tokens_details.text_tokens"},
	}
	values := make([]int, len(groups))
	for i, paths := range groups {
		value, valid := aiBackgroundUsageSynonyms(u, paths)
		if !valid {
			return t, true, nil
		}
		values[i] = value
	}
	input, output, read, write, imageIn, imageOut, imageCached, reasoning := values[0], values[1], values[2], values[3], values[4], values[5], values[6], values[7]
	if read > input || write > input-read || imageIn > input || imageOut > output || imageCached > read || imageCached > imageIn || reasoning > output {
		return t, true, nil
	}
	if total := u.Get("total_tokens"); total.Exists() && total.Int() != int64(input)+int64(output) {
		return t, true, nil
	}
	t.InputTokens, t.OutputTokens = input, output
	t.CacheReadTokens, t.CacheCreationTokens = read, write
	t.ImageInputTokens, t.ImageOutputTokens, t.ImageCacheReadTokens = imageIn, imageOut, imageCached
	t.UsageKnown = true
	return t, true, nil
}

func aiBackgroundUsageSynonyms(usage gjson.Result, paths []string) (int, bool) {
	var result int64
	found := false
	for _, path := range paths {
		value := usage.Get(path)
		if !value.Exists() {
			continue
		}
		if !aiBackgroundUsageInteger(value) || found && result != value.Int() {
			return 0, false
		}
		result, found = value.Int(), true
	}
	return int(result), true
}

func aiBackgroundUsageInteger(value gjson.Result) bool {
	return value.Type == gjson.Number && value.Float() >= 0 && value.Float() <= math.MaxInt32 && value.Float() == float64(value.Int()) && !strings.ContainsAny(value.Raw, ".eE")
}
