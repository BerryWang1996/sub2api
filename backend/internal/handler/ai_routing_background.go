package handler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type aiRoutingBackgroundService interface {
	Ready() bool
	Repository() service.AIRoutingBackgroundRepository
	PrepareForAccount(context.Context, *service.AIRoutingBackgroundJob, *service.APIKey, *service.User, *service.UserSubscription, []byte, time.Time) (*service.AIRoutingBackgroundJob, error)
	ReserveAndSync(context.Context, *service.AIRoutingBackgroundJob, *service.APIKey, *service.User, *service.UserSubscription, int) (*service.AIRoutingBackgroundJob, error)
	BeginSubmission(context.Context, *service.AIRoutingBackgroundJob) error
	AttachResponse(context.Context, string, string) error
	RecordTerminal(context.Context, string, int64, string, service.AIRoutingBackgroundTerminal) error
	RecordResponseTerminal(context.Context, *service.APIKey, int64, string, service.AIRoutingBackgroundTerminal) error
	ValidateResponseOrigin(context.Context, *service.APIKey, int64, string) error
	CanDeleteResponse(context.Context, *service.APIKey, int64, string) error
}

type aiBackgroundRequestState struct {
	id      string
	account atomic.Int64
	service aiRoutingBackgroundService
}

func (h *GatewayHandler) installAIBackgroundSubmission(c *gin.Context, runtime *aiRoutingRuntime, key *service.APIKey, p *service.AIRoutingPolicy, req service.AIRoutingRequest, chosen service.AIRoutingCandidate) (*aiBackgroundRequestState, error) {
	if h.aiBackground == nil || !h.aiBackground.Ready() || h.apiKeyService == nil || req.Operation != "" || !req.StoreResponse {
		return nil, errors.New("background Responses require durable settlement and provider storage")
	}
	state := &aiBackgroundRequestState{id: uuid.NewString(), service: h.aiBackground}
	c.Header("X-Sub2API-Background-Job-ID", state.id)
	clientIP := middleware.SecurityClientIP(c)
	ctx := service.WithAIRoutingBackgroundSubmission(c.Request.Context(), func(wire *http.Request, accountID int64) error {
		if wire.Method != http.MethodPost || wire.GetBody == nil || accountID <= 0 {
			return service.ErrAIBackgroundConflict
		}
		fresh, err := h.apiKeyService.RevalidateAIRoutingKey(wire.Context(), key, clientIP)
		if err != nil {
			return err
		}
		if blockedModelAllowlistCandidate(fresh.APIKey.Group, []string{req.PublicModel}) != "" {
			return service.ErrAIRoutingPrincipalRevoked
		}
		if status, _ := runtime.revalidateChoice(wire.Context(), fresh.APIKey, p, req, service.CompositeRouteEndpointResponses, accountID, chosen); status != 0 {
			return service.ErrAIRoutingDecisionRevoked
		}
		copy, err := wire.GetBody()
		if err != nil {
			return err
		}
		body, readErr := io.ReadAll(io.LimitReader(copy, (32<<20)+1))
		_ = copy.Close()
		if readErr != nil || len(body) > 32<<20 {
			return service.ErrAIBackgroundUnsupported
		}
		job := &service.AIRoutingBackgroundJob{ID: state.id, SourceGroupID: key.Group.ID, UserID: key.UserID, APIKeyID: key.ID, AccountID: accountID, CandidateID: chosen.ID, PublicModel: req.PublicModel, ExecutionModel: chosen.Model, Platform: service.QuotaPlatform(wire.Context(), fresh.APIKey), PolicyVersion: p.Version, RequestPayloadHash: service.HashUsageRequestPayload(body)}
		if fresh.Subscription != nil && fresh.APIKey.Group.IsSubscriptionType() {
			job.SubscriptionID = fresh.Subscription.ID
		}
		if chosen.TargetGroupID != nil {
			job.TargetGroupID = *chosen.TargetGroupID
		}
		if chosen.TargetAccountID != nil {
			job.TargetAccountID = *chosen.TargetAccountID
		}
		job, err = h.aiBackground.PrepareForAccount(wire.Context(), job, fresh.APIKey, fresh.APIKey.User, fresh.Subscription, body, service.GatewayTokenRequestPricingAtFromContext(wire.Context()))
		if err != nil {
			return err
		}
		job, err = h.aiBackground.ReserveAndSync(wire.Context(), job, fresh.APIKey, fresh.APIKey.User, fresh.Subscription, 16)
		if err != nil {
			return err
		}
		// Reservation/cache work may block. Recheck credentials, policy and the
		// exact final payload after it, before the durable submitting transition.
		latest, err := h.apiKeyService.RevalidateAIRoutingKey(wire.Context(), key, clientIP)
		if err != nil {
			return err
		}
		if blockedModelAllowlistCandidate(latest.APIKey.Group, []string{req.PublicModel}) != "" {
			return service.ErrAIRoutingPrincipalRevoked
		}
		if latest.APIKey.Group.IsSubscriptionType() != (job.SubscriptionID > 0) || job.SubscriptionID > 0 && (latest.Subscription == nil || latest.Subscription.ID != job.SubscriptionID) {
			return service.ErrAIRoutingPrincipalRevoked
		}
		if status, _ := runtime.revalidateChoice(wire.Context(), latest.APIKey, p, req, service.CompositeRouteEndpointResponses, accountID, chosen); status != 0 {
			return service.ErrAIRoutingDecisionRevoked
		}
		if err = service.CheckAIRoutingOutboundRequest(wire, accountID); err != nil {
			return err
		}
		if err = h.aiBackground.BeginSubmission(wire.Context(), job); err != nil {
			return err
		}
		state.account.Store(accountID)
		return nil
	})
	c.Request = c.Request.WithContext(ctx)
	c.Request = c.Request.WithContext(service.WithAIRoutingBackgroundResponseObserver(c.Request.Context(), state.attach))
	c.Request = c.Request.WithContext(service.WithAIRoutingBackgroundTerminalObserver(c.Request.Context(), state.recordTerminal))
	return state, nil
}

func (s *aiBackgroundRequestState) attach(ctx context.Context, accountID int64, responseID string) error {
	if accountID <= 0 || s.account.Load() != accountID {
		return service.ErrAIBackgroundConflict
	}
	// The provider may accept work before the client disconnects. Recording
	// that fact for accounting is independent of exposing an ID to the client.
	recording, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	return s.service.AttachResponse(recording, s.id, responseID)
}

func (s *aiBackgroundRequestState) recordTerminal(ctx context.Context, accountID int64, responseID string, terminal service.AIRoutingBackgroundTerminal) error {
	if accountID <= 0 || s.account.Load() != accountID {
		return service.ErrAIBackgroundConflict
	}
	recording, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	return s.service.RecordTerminal(recording, s.id, accountID, responseID, terminal)
}

func (s *aiBackgroundRequestState) finish(ctx context.Context) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	repo := s.service.Repository()
	job, err := repo.Get(cleanup, s.id)
	if errors.Is(err, service.ErrAIBackgroundNotFound) {
		return
	}
	if err == nil {
		switch job.Status {
		case service.AIBackgroundAdmitted:
			err = repo.ReleaseUnsubmitted(cleanup, job.ID, job.Version)
		case service.AIBackgroundSubmitting:
			err = repo.MarkSubmissionUnknown(cleanup, job.ID, job.Version)
		}
	}
	if err != nil {
		slog.WarnContext(cleanup, "ai_routing.background_recovery_pending", "job_id", s.id)
	}
}
