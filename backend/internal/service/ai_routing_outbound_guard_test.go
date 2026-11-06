//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type aiOutboundAccountRepo struct {
	AccountRepository
	account *Account
	reads   int
}

func (r *aiOutboundAccountRepo) GetByID(context.Context, int64) (*Account, error) {
	r.reads++
	return r.account, nil
}

func aiOutboundGuardFixture() (context.Context, *GatewayService, *aiOutboundAccountRepo) {
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true}
	repo := &aiOutboundAccountRepo{account: account}
	ctx := WithCompositeRouteDecision(context.Background(), CompositeRouteDecision{Matched: true, Source: CompositeRouteSourceAI, AICandidateID: "chosen", GroupID: 9, TargetAccountID: &account.ID, TargetPlatform: PlatformOpenAI, UpstreamModel: "gpt-5.1"})
	ctx = WithAIRoutingExecutionRequirements(ctx, AIRoutingExecutionRequirements{InputModalities: []string{"text"}, InputBytes: 32, MaxOutputTokens: 10, ContextWindowTokens: 512})
	return ctx, &GatewayService{accountRepo: repo}, repo
}

func TestAIRoutingOutboundGuardUsesFinalBodyAndFreshAccount(t *testing.T) {
	ctx, service, repo := aiOutboundGuardFixture()
	ctx = service.WithAIRoutingFinalCapacityGuard(ctx)
	body := `{"model":"gpt-5.1","messages":[{"role":"user","content":"hello"}],"max_tokens":10}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.invalid/v1/messages", strings.NewReader(body))
	require.NoError(t, err)
	require.NoError(t, CheckAIRoutingOutboundRequest(req, 1))
	original, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	require.Equal(t, body, string(original), "capacity check must not consume or rewrite signed request data")
	aiCapabilityMetadata(repo.account, 70, 10, "text")
	err = CheckAIRoutingOutboundRequest(req, 1)
	var refused *AIRoutingCapacityError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, "execution_context_window_exceeded", refused.Check.Reason)
	require.True(t, refused.Check.FinalPayloadChecked)
	require.False(t, refused.Check.CapacityVerified)
	require.Equal(t, 3, repo.reads, "successful inspection rechecks the account; a failed body check stops earlier")
	require.NotContains(t, err.Error(), "hello")
}

func TestAIRoutingOutboundGuardRejectsPolicyAndCancellationBeforeDetachedSend(t *testing.T) {
	for _, mode := range []string{"cancelled", "revoked", "wrong_account"} {
		t.Run(mode, func(t *testing.T) {
			ctx, service, repo := aiOutboundGuardFixture()
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			ctx = WithAIRoutingDecisionGuard(ctx, func(context.Context) error {
				if mode == "revoked" {
					return errors.New("policy changed")
				}
				return nil
			})
			guarded := service.WithAIRoutingFinalCapacityGuard(ctx)
			req, err := http.NewRequestWithContext(context.WithoutCancel(guarded), http.MethodPost, "https://upstream.invalid/v1/messages", strings.NewReader(`{"model":"gpt-5.1","messages":[],"max_tokens":10}`))
			require.NoError(t, err)
			if mode == "cancelled" {
				cancel()
			}
			id := int64(1)
			if mode == "wrong_account" {
				id = 2
			}
			err = CheckAIRoutingOutboundRequest(req, id)
			require.True(t, IsAIRoutingCapacityError(err))
			if mode != "wrong_account" {
				require.Zero(t, repo.reads)
			}
		})
	}
	var service *GatewayService
	ctx := service.WithAIRoutingFinalCapacityGuard(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.invalid/arbitrary", nil)
	require.NoError(t, err)
	require.NoError(t, CheckAIRoutingOutboundRequest(req, 0), "ordinary routes do not acquire a capacity guard")
}

func TestAIRoutingCapacityErrorIsLocalAndStreamingAware(t *testing.T) {
	for _, stream := range []bool{false, true} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		if stream {
			c.Header("Content-Type", "text/event-stream")
			_, _ = c.Writer.WriteString(": keepalive\n\n")
			c.Writer.Flush()
		}
		err := &AIRoutingCapacityError{Check: AIRoutingCapabilityCheck{Reason: "execution_context_window_exceeded"}}
		require.True(t, handleAIRoutingCapacityError(c, err))
		require.True(t, c.IsAborted())
		require.True(t, IsResponseCommitted(c))
		require.Contains(t, recorder.Body.String(), "ai_execution_capacity_rejected")
		if stream {
			require.Contains(t, recorder.Body.String(), "event: error")
		} else {
			require.Equal(t, http.StatusBadRequest, recorder.Code)
		}
		before := recorder.Body.String()
		require.True(t, handleAIRoutingCapacityError(c, err))
		require.Equal(t, before, recorder.Body.String(), "transport wrappers must not duplicate the error")
	}
}

func TestAIRoutingCapacityErrorPreservesGeminiProtocol(t *testing.T) {
	for _, stream := range []bool{false, true} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1beta/models/auto:generateContent", nil)
		c.Request = c.Request.WithContext(WithAIRoutingExecutionRequirements(c.Request.Context(), AIRoutingExecutionRequirements{Endpoint: CompositeRouteEndpointGemini}))
		if stream {
			c.Header("Content-Type", "text/event-stream")
			_, _ = c.Writer.WriteString(": keepalive\n\n")
			c.Writer.Flush()
		}
		err := &AIRoutingCapacityError{Check: AIRoutingCapabilityCheck{Reason: "execution_context_window_exceeded"}}
		require.True(t, handleAIRoutingCapacityError(c, err))
		require.Contains(t, recorder.Body.String(), `"status":"INVALID_ARGUMENT"`)
		require.Contains(t, recorder.Body.String(), `"code":400`)
		require.NotContains(t, recorder.Body.String(), "event: error")
		if stream {
			require.Contains(t, recorder.Body.String(), "data: ")
		} else {
			require.Equal(t, http.StatusBadRequest, recorder.Code)
		}
	}
}

func TestAIRoutingOutboundGuardRechecksPolicyAfterAccountLookup(t *testing.T) {
	ctx, service, _ := aiOutboundGuardFixture()
	checks := 0
	ctx = WithAIRoutingDecisionGuard(ctx, func(context.Context) error {
		checks++
		if checks > 1 {
			return errors.New("publication changed during final inspection")
		}
		return nil
	})
	ctx = service.WithAIRoutingFinalCapacityGuard(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.invalid/v1/messages", strings.NewReader(`{"model":"gpt-5.1","messages":[],"max_tokens":10}`))
	require.NoError(t, err)
	err = CheckAIRoutingOutboundRequest(req, 1)
	var refused *AIRoutingCapacityError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, "execution_policy_changed", refused.Check.Reason)
	require.Equal(t, 2, checks)
}

func TestAIRoutingOutboundGuardRechecksDestinationAfterBodyInspection(t *testing.T) {
	for _, mode := range []string{"unchanged", "account_disabled", "membership_removed", "group_disabled", "window_reduced", "output_reduced", "implicit_output_increased", "implicit_output_unknown"} {
		t.Run(mode, func(t *testing.T) {
			ctx, service, repo := aiOutboundGuardFixture()
			groupID := int64(2)
			group := &Group{ID: groupID, Platform: PlatformOpenAI, Status: StatusActive}
			groups := &aiDecisionGroupStore{groups: map[int64]*Group{groupID: group}}
			service.groupRepo = groups
			repo.account.AccountGroups = []AccountGroup{{GroupID: groupID}}
			aiCapabilityMetadata(repo.account, 512, 120, "text")
			ctx = WithCompositeRouteDecision(ctx, CompositeRouteDecision{Matched: true, Source: CompositeRouteSourceAI, AICandidateID: "chosen", GroupID: 9, TargetGroupID: &groupID, TargetPlatform: PlatformOpenAI, UpstreamModel: "gpt-5.1"})
			policyChecks := 0
			ctx = WithAIRoutingDecisionGuard(ctx, func(context.Context) error { policyChecks++; return nil })
			ctx = service.WithAIRoutingFinalCapacityGuard(ctx)
			body := `{"model":"gpt-5.1","input":[{"role":"user","content":"` + strings.Repeat("x", 80) + `"}],"max_output_tokens":100}`
			if strings.HasPrefix(mode, "implicit_output_") {
				body = `{"model":"gpt-5.1","input":[{"role":"user","content":"` + strings.Repeat("x", 80) + `"}]}`
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://upstream.invalid/v1/responses", strings.NewReader(body))
			require.NoError(t, err)
			copyBody := req.GetBody
			copies := 0
			req.GetBody = func() (io.ReadCloser, error) {
				copies++
				// Replace the repository snapshot rather than mutating the account
				// already loaded by the guard; model a committed admin change.
				fresh := *repo.account
				fresh.Extra = nil
				aiCapabilityMetadata(&fresh, 512, 120, "text")
				switch mode {
				case "account_disabled":
					fresh.Status = StatusDisabled
				case "membership_removed":
					fresh.AccountGroups = nil
				case "group_disabled":
					changedGroup := *group
					changedGroup.Status = StatusDisabled
					groups.groups[groupID] = &changedGroup
				case "window_reduced":
					aiCapabilityMetadata(&fresh, 200, 120, "text")
				case "output_reduced":
					aiCapabilityMetadata(&fresh, 512, 50, "text")
				case "implicit_output_increased":
					aiCapabilityMetadata(&fresh, 512, 500, "text")
				case "implicit_output_unknown":
					fresh.Extra = nil
				}
				repo.account = &fresh
				return copyBody()
			}
			err = CheckAIRoutingOutboundRequest(req, 1)
			require.Equal(t, 1, copies, "the final authorization check must reuse the measured body facts")
			original, readErr := io.ReadAll(req.Body)
			require.NoError(t, readErr)
			require.Equal(t, body, string(original))
			if mode == "unchanged" {
				require.NoError(t, err)
				require.Equal(t, 2, policyChecks)
				return
			}
			var refused *AIRoutingCapacityError
			require.ErrorAs(t, err, &refused)
			switch mode {
			case "window_reduced", "implicit_output_increased":
				require.Equal(t, "execution_context_window_exceeded", refused.Check.Reason)
				require.EqualValues(t, len(body), refused.Check.InputBytes)
				require.True(t, refused.Check.FinalPayloadChecked)
				if mode == "implicit_output_increased" {
					require.EqualValues(t, 500, refused.Check.OutputReserveTokens)
				}
			case "output_reduced":
				require.Equal(t, "execution_output_limit_exceeded", refused.Check.Reason)
				require.EqualValues(t, 100, refused.Check.OutputReserveTokens)
			case "implicit_output_unknown":
				require.Equal(t, "execution_output_reserve_unknown", refused.Check.Reason)
			default:
				require.Equal(t, "execution_account_unavailable", refused.Check.Reason)
			}
			require.False(t, refused.Check.CapacityVerified)
			require.Equal(t, 1, policyChecks, "source publication is unchanged; destination revalidation itself must refuse")
		})
	}
}
