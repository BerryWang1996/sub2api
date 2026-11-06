package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *GatewayHandler) forwardAIResponsesWithOrigin(c *gin.Context, runtime *aiRoutingRuntime, store service.AIRoutingOriginStore, key *service.APIKey, policy *service.AIRoutingPolicy, req service.AIRoutingRequest, chosen service.AIRoutingCandidate) {
	var background *aiBackgroundRequestState
	if req.Background {
		var err error
		background, err = h.installAIBackgroundSubmission(c, runtime, key, policy, req, chosen)
		if err != nil {
			abortAI(c, 503, "ai_background_unavailable", "Background response settlement is unavailable")
			return
		}
		defer background.finish(c.Request.Context())
	}
	original := c.Writer
	forwardCtx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	forwardCtx = withAIGeminiResponseState(forwardCtx, store, key, chosen, func() int64 { return c.GetInt64(opsAccountIDKey) })
	c.Request = c.Request.WithContext(forwardCtx)
	var accountID int64
	var native bool
	var writer *aiToolWriter
	newWriter := newAIResponsesWriter
	if req.Operation == service.AIRoutingResponsesCompact {
		newWriter = newAIResponsesCompactWriter
	}
	writer = newWriter(original, func(kind, id, responseID string) error {
		selected := c.GetInt64(opsAccountIDKey)
		if selected <= 0 || accountID != 0 && accountID != selected {
			return errors.New("response execution account unavailable or changed")
		}
		if background != nil && kind == "response" {
			if err := background.attach(forwardCtx, selected, responseID); err != nil {
				return err
			}
			// The stored native API Key account was validated at actual send.
			accountID, native = selected, true
		}
		if accountID == 0 {
			check := runtime.responseNative
			if check == nil {
				check = h.gatewayService.AIRoutingNativeResponsesAccount
			}
			var err error
			native, err = check(forwardCtx, selected, chosen.Model)
			if err != nil {
				return err
			}
			accountID = selected
		}
		target := newAIToolBinding(chosen, accountID)
		origin := service.AIRoutingOriginBinding{
			PublicModel: req.PublicModel,
			Candidate:   target.Candidate, Account: target.Account, Model: target.Model,
			Platform: target.Platform, GroupTarget: target.GroupTarget, AccountTarget: target.AccountTarget,
			ResponseID: responseID, Store: req.StoreResponse, Background: req.Background, Native: native,
		}
		if req.Operation == service.AIRoutingResponsesCompact {
			origin.ParentKind, origin.Store, origin.Background = "compaction", false, false
			if kind != "compaction" {
				return saveAIResponsesControlBinding(forwardCtx, store, key, service.AIRoutingOriginRef{Kind: kind, ID: id}, origin)
			}
		}
		return saveAIResponsesBinding(forwardCtx, store, key, service.AIRoutingOriginRef{Kind: kind, ID: id}, origin)
	}, func(err error) {
		slog.WarnContext(forwardCtx, "ai_routing.response_binding_write_failed", "error", err)
		c.Abort()
		c.Writer = original
		if !original.Written() {
			c.Header("Content-Type", "application/json")
			c.Header("Content-Length", "")
		}
		writeAIResponsesStreamingError(c, http.StatusServiceUnavailable, "ai_response_binding_unavailable", "Response continuation could not be recorded")
		service.MarkResponseCommitted(c)
		c.Writer = writer
		cancel()
	})
	c.Writer = writer
	defer func() { c.Writer = original }()
	c.Next()
	_ = writer.finish()
}
