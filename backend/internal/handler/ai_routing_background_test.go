//go:build unit

package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type aiBackgroundTestKeys struct {
	service.APIKeyRepository
	key *service.APIKey
}

func (r *aiBackgroundTestKeys) GetByID(context.Context, int64) (*service.APIKey, error) {
	return r.key, nil
}

type aiBackgroundTestUsers struct {
	service.UserRepository
	user *service.User
}

func (r *aiBackgroundTestUsers) GetByID(context.Context, int64) (*service.User, error) {
	return r.user, nil
}

type aiBackgroundTestGroups struct {
	service.GroupRepository
	group *service.Group
}

func (r *aiBackgroundTestGroups) GetByID(context.Context, int64) (*service.Group, error) {
	return r.group, nil
}

type aiBackgroundTestSubs struct {
	service.UserSubscriptionRepository
	sub *service.UserSubscription
}

func (r *aiBackgroundTestSubs) GetActiveByUserIDAndGroupID(context.Context, int64, int64) (*service.UserSubscription, error) {
	return r.sub, nil
}

type aiBackgroundTestLedger struct {
	service.AIRoutingBackgroundRepository
	job *service.AIRoutingBackgroundJob
}

func (r *aiBackgroundTestLedger) Get(context.Context, string) (*service.AIRoutingBackgroundJob, error) {
	if r.job == nil {
		return nil, service.ErrAIBackgroundNotFound
	}
	return r.job, nil
}
func (r *aiBackgroundTestLedger) ReleaseUnsubmitted(_ context.Context, _ string, version int64) error {
	if r.job.Version != version {
		return service.ErrAIBackgroundConflict
	}
	r.job.Status = service.AIBackgroundReleased
	return nil
}
func (r *aiBackgroundTestLedger) MarkSubmissionUnknown(_ context.Context, _ string, version int64) error {
	if r.job.Version != version {
		return service.ErrAIBackgroundConflict
	}
	r.job.Status = service.AIBackgroundSubmissionUnknown
	return nil
}

type aiBackgroundTestService struct {
	ledger                   *aiBackgroundTestLedger
	body                     []byte
	afterReserve             func()
	submissions, attachments int
}

func (s *aiBackgroundTestService) Ready() bool { return true }
func (s *aiBackgroundTestService) CanDeleteResponse(context.Context, *service.APIKey, int64, string) error {
	return nil
}
func (s *aiBackgroundTestService) ValidateResponseOrigin(context.Context, *service.APIKey, int64, string) error {
	return nil
}
func (s *aiBackgroundTestService) RecordResponseTerminal(context.Context, *service.APIKey, int64, string, service.AIRoutingBackgroundTerminal) error {
	return nil
}
func (s *aiBackgroundTestService) Repository() service.AIRoutingBackgroundRepository { return s.ledger }
func (s *aiBackgroundTestService) PrepareForAccount(_ context.Context, j *service.AIRoutingBackgroundJob, _ *service.APIKey, _ *service.User, _ *service.UserSubscription, body []byte, _ time.Time) (*service.AIRoutingBackgroundJob, error) {
	s.body = append([]byte(nil), body...)
	return j, nil
}
func (s *aiBackgroundTestService) ReserveAndSync(_ context.Context, j *service.AIRoutingBackgroundJob, _ *service.APIKey, _ *service.User, _ *service.UserSubscription, _ int) (*service.AIRoutingBackgroundJob, error) {
	j.Status = service.AIBackgroundAdmitted
	j.Version = 2
	s.ledger.job = j
	if s.afterReserve != nil {
		s.afterReserve()
	}
	return j, nil
}
func (s *aiBackgroundTestService) BeginSubmission(_ context.Context, j *service.AIRoutingBackgroundJob) error {
	s.submissions++
	j.Status = service.AIBackgroundSubmitting
	return nil
}
func (s *aiBackgroundTestService) AttachResponse(ctx context.Context, id, response string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.ledger.job.ID != id {
		return service.ErrAIBackgroundConflict
	}
	s.attachments++
	s.ledger.job.ResponseID = response
	s.ledger.job.Status = service.AIBackgroundActive
	return nil
}

func (s *aiBackgroundTestService) RecordTerminal(ctx context.Context, id string, account int64, response string, terminal service.AIRoutingBackgroundTerminal) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.ledger.job.ID != id || s.ledger.job.AccountID != account || s.ledger.job.ResponseID != response {
		return service.ErrAIBackgroundConflict
	}
	s.ledger.job.Terminal = &terminal
	return nil
}

func TestAIBackgroundTerminalRetainedAfterClientDisconnect(t *testing.T) {
	job := &service.AIRoutingBackgroundJob{ID: "job", AccountID: 12, ResponseID: "resp_owned", Status: service.AIBackgroundActive}
	state := &aiBackgroundRequestState{id: job.ID, service: &aiBackgroundTestService{ledger: &aiBackgroundTestLedger{job: job}}}
	state.account.Store(job.AccountID)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	terminal := service.AIRoutingBackgroundTerminal{Status: "completed", Model: "model", UsageKnown: true, InputTokens: 31, OutputTokens: 7}
	require.Error(t, state.recordTerminal(ctx, 13, job.ResponseID, terminal))
	require.Error(t, state.recordTerminal(ctx, 12, "resp_foreign", terminal))
	require.Nil(t, job.Terminal)
	require.NoError(t, state.recordTerminal(ctx, 12, job.ResponseID, terminal))
	require.Equal(t, terminal, *job.Terminal)
}

func TestAIBackgroundSubmissionRevalidatesAfterReserveAndPreservesUnknown(t *testing.T) {
	for _, subscription := range []bool{false, true} {
		for _, mode := range []string{"success", "unknown", "key_revoked", "policy_revoked", "subscription_replaced", "final_guard"} {
			t.Run(mode+map[bool]string{false: "_balance", true: "_subscription"}[subscription], func(t *testing.T) {
				group := &service.Group{ID: 17, Status: service.StatusActive, Platform: service.PlatformComposite, AIRouting: true}
				if subscription {
					group.SubscriptionType = service.SubscriptionTypeSubscription
				}
				user := &service.User{ID: 42, Status: service.StatusActive, Balance: 100}
				key := &service.APIKey{ID: 9, UserID: user.ID, Key: "synthetic-key", GroupID: &group.ID, Group: group, User: user, Status: service.StatusActive}
				current := *key
				subs := &aiBackgroundTestSubs{sub: &service.UserSubscription{ID: 8, UserID: user.ID, GroupID: group.ID, Status: service.SubscriptionStatusActive, ExpiresAt: time.Now().Add(time.Hour)}}
				keys := service.NewAPIKeyService(&aiBackgroundTestKeys{key: &current}, &aiBackgroundTestUsers{user: user}, &aiBackgroundTestGroups{group: group}, subs, nil, nil, nil)
				bg := &aiBackgroundTestService{ledger: &aiBackgroundTestLedger{}}
				h := &GatewayHandler{apiKeyService: keys, aiBackground: bg}
				p := &service.AIRoutingPolicy{GroupID: group.ID, Enabled: true, Version: 3}
				live := *p
				account := int64(75)
				chosen := service.AIRoutingCandidate{ID: "candidate", Platform: service.PlatformOpenAI, Model: "gpt-5.1", TargetAccountID: &account}
				runtime := &aiRoutingRuntime{policies: &aiHTTPPolicy{policy: &live}, eligible: func(context.Context, *service.APIKey, *service.AIRoutingPolicy, service.AIRoutingRequest, string, int64) []service.AIRoutingCandidate {
					return []service.AIRoutingCandidate{chosen}
				}}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				req := service.AIRoutingRequest{PublicModel: "arbitrary-alias", Background: true, StoreResponse: true}
				state, err := h.installAIBackgroundSubmission(c, runtime, key, p, req, chosen)
				require.NoError(t, err)
				bg.afterReserve = func() {
					switch mode {
					case "key_revoked":
						current.Status = service.StatusAPIKeyDisabled
					case "policy_revoked":
						live.Version++
					case "subscription_replaced":
						if subscription {
							subs.sub.ID++
						}
					}
				}
				wireCtx := c.Request.Context()
				if mode == "final_guard" {
					wireCtx = service.WithAIRoutingOutboundGuard(wireCtx, func(*http.Request, int64) error { return errors.New("changed during reservation") })
				}
				body := `{"model":"gpt-5.1","input":"final transformed input","background":true,"store":true,"max_output_tokens":100}`
				wire, err := http.NewRequestWithContext(wireCtx, http.MethodPost, "https://synthetic.invalid/v1/responses", strings.NewReader(body))
				require.NoError(t, err)
				err = service.BeginAIRoutingBackgroundSubmission(wire, account)
				rejected := mode == "key_revoked" || mode == "policy_revoked" || mode == "subscription_replaced" && subscription || mode == "final_guard"
				if rejected {
					require.Error(t, err)
					require.Zero(t, bg.submissions)
				} else {
					require.NoError(t, err)
					require.Equal(t, 1, bg.submissions)
				}
				require.Equal(t, body, string(bg.body))
				require.Equal(t, service.HashUsageRequestPayload([]byte(body)), bg.ledger.job.RequestPayloadHash)
				require.Equal(t, subscription, bg.ledger.job.SubscriptionID > 0)
				if !rejected && mode != "unknown" {
					require.Error(t, state.attach(t.Context(), account+1, "resp_wrong"))
					disconnected, disconnect := context.WithCancel(t.Context())
					disconnect()
					require.NoError(t, state.attach(disconnected, account, "resp_owned"))
				}
				cancelled, cancel := context.WithCancel(c.Request.Context())
				cancel()
				state.finish(cancelled)
				expected := service.AIBackgroundActive
				if rejected {
					expected = service.AIBackgroundReleased
				} else if mode == "unknown" {
					expected = service.AIBackgroundSubmissionUnknown
				}
				require.Equal(t, expected, bg.ledger.job.Status)
				require.Error(t, service.BeginAIRoutingBackgroundSubmission(wire, account), "no application retry may resubmit this operation")
			})
		}
	}
}

func TestAIBackgroundWithoutLedgerNotReleasedBeforeDecision(t *testing.T) {
	f := newAIResponsesHTTPFixture(t)
	response := f.send("/v1/responses", `{"model":"auto","input":"task","max_output_tokens":100,"background":true}`, false)
	require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "ai_background_not_released")
	require.Zero(t, f.chooser.calls)
	require.Zero(t, f.executions)
}
