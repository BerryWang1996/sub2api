package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type aiResponsesControlValidator interface {
	ValidateAIRoutingResponsesOrigin(context.Context, *service.APIKey, *service.AIRoutingPolicy, *service.AIRoutingOriginBinding) (*service.Account, error)
}

type aiResponsesControlForwarder interface {
	ForwardAIRoutingResponsesControl(context.Context, *service.Account, string, string, string, string, string) (*http.Response, error)
}

// Match exact resource shapes across the already-supported Responses aliases.
// POST compact/input_tokens remain handled by their own endpoint contracts.
func aiResponsesControlPath(c *gin.Context) (responseID, operation string, matched bool) {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return "", "", false
	}
	path := c.Request.URL.Path
	var suffix string
	for _, prefix := range []string{"/v1/responses/", "/responses/", "/backend-api/codex/responses/"} {
		if strings.HasPrefix(path, prefix) {
			suffix = strings.TrimPrefix(path, prefix)
			break
		}
	}
	if suffix == "" {
		return "", "", false
	}
	parts := strings.Split(suffix, "/")
	if len(parts) == 1 && (c.Request.Method == http.MethodGet || c.Request.Method == http.MethodDelete) {
		return parts[0], "", true
	}
	if len(parts) == 2 && (c.Request.Method == http.MethodGet && parts[1] == "input_items" || c.Request.Method == http.MethodPost && parts[1] == "cancel") {
		return parts[0], parts[1], true
	}
	return "", "", false
}

func aiResponsesControlError(c *gin.Context, status int, code string) {
	c.Abort()
	if c.Request.Context().Err() != nil || service.IsResponseCommitted(c) {
		return
	}
	writeAIResponsesStreamingError(c, status, code, "The requested AI response resource is unavailable for this operation.")
	service.MarkResponseCommitted(c)
}

// Shared by create/control binding failures. The caller owns Abort and the
// committed marker; an already-started Responses SSE uses the native flat shape.
func writeAIResponsesStreamingError(c *gin.Context, status int, code, message string) {
	if c == nil || c.Writer == nil {
		return
	}
	if c.Writer.Written() {
		if !strings.Contains(c.Writer.Header().Get("Content-Type"), "text/event-stream") {
			return
		}
		payload := gin.H{"type": "error", "code": code, "message": message, "param": nil}
		encoded, _ := json.Marshal(payload)
		_, _ = c.Writer.Write(append(append([]byte("event: error\ndata: "), encoded...), []byte("\n\n")...))
		c.Writer.Flush()
		return
	}
	c.JSON(status, gin.H{"error": gin.H{"type": "invalid_request_error", "code": code, "message": message}})
}

func (h *GatewayHandler) routeAIResponsesControl(c *gin.Context, runtime *aiRoutingRuntime, key *service.APIKey, policy *service.AIRoutingPolicy) bool {
	return routeAIResponsesControlWith(c, runtime, key, policy, h.gatewayService, h.openAIGatewayService, h.aiBackground)
}

type aiResponsesBackgroundDeleteValidator interface {
	CanDeleteResponse(context.Context, *service.APIKey, int64, string) error
	ValidateResponseOrigin(context.Context, *service.APIKey, int64, string) error
	RecordResponseTerminal(context.Context, *service.APIKey, int64, string, service.AIRoutingBackgroundTerminal) error
}

func routeAIResponsesControlWith(c *gin.Context, runtime *aiRoutingRuntime, key *service.APIKey, policy *service.AIRoutingPolicy, validator aiResponsesControlValidator, forwarder aiResponsesControlForwarder, background ...aiResponsesBackgroundDeleteValidator) bool {
	responseID, operation, matched := aiResponsesControlPath(c)
	if !matched {
		return false
	}
	c.Abort()
	if runtime == nil || runtime.store == nil || runtime.policies == nil || validator == nil || forwarder == nil {
		aiResponsesControlError(c, 503, "ai_response_control_unavailable")
		return true
	}
	store, ok := runtime.store.(service.AIRoutingOriginStore)
	if !ok {
		aiResponsesControlError(c, 503, "ai_binding_unavailable")
		return true
	}
	ref := service.AIRoutingOriginRef{Kind: "response", ID: responseID}
	request := service.AIRoutingRequest{RequiresOrigin: true, OriginRefs: []service.AIRoutingOriginRef{ref}, PreviousResponseID: responseID}
	_, origin, err := loadAIResponsesBinding(c.Request.Context(), store, key, request)
	if err != nil || origin == nil || origin.PublicModel == "" {
		aiResponsesControlError(c, 404, "ai_response_origin_unavailable")
		return true
	}
	request.PublicModel = origin.PublicModel
	key, err = runtime.requestPrincipal(c.Request.Context(), key, request, service.CompositeRouteEndpointResponses)
	if err != nil {
		aiResponsesControlError(c, 403, "ai_source_revoked")
		return true
	}
	if operation == "cancel" && !origin.Background {
		aiResponsesControlError(c, 409, "ai_response_cancel_unsupported")
		return true
	}
	var revoker service.AIRoutingOriginRevoker
	checkSettlement := func(ctx context.Context) error {
		if c.Request.Method != http.MethodDelete || !origin.Background {
			return nil
		}
		if len(background) == 0 || background[0] == nil {
			return service.ErrAIBackgroundRecoveryPending
		}
		return background[0].CanDeleteResponse(ctx, key, origin.Account, responseID)
	}
	if c.Request.Method == http.MethodDelete {
		if checkSettlement(c.Request.Context()) != nil {
			aiResponsesControlError(c, 409, "ai_background_settlement_pending")
			return true
		}
		revoker, ok = runtime.store.(service.AIRoutingOriginRevoker)
		if !ok {
			aiResponsesControlError(c, 503, "ai_response_revocation_unavailable")
			return true
		}
	}
	checkBackgroundOrigin := func(ctx context.Context, fresh *service.APIKey) error {
		if !origin.Background {
			return nil
		}
		if len(background) == 0 || background[0] == nil {
			return service.ErrAIBackgroundConflict
		}
		return background[0].ValidateResponseOrigin(ctx, fresh, origin.Account, responseID)
	}
	if checkBackgroundOrigin(c.Request.Context(), key) != nil {
		aiResponsesControlError(c, 403, "ai_background_origin_unavailable")
		return true
	}
	account, err := validator.ValidateAIRoutingResponsesOrigin(c.Request.Context(), key, policy, origin)
	if err != nil || account == nil {
		aiResponsesControlError(c, 403, "ai_response_origin_revoked")
		return true
	}
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	// Token retrieval can block. Recheck the shared tombstone, latest policy,
	// membership and account at the common final HTTP send boundary as well.
	guarded := service.WithAIRoutingOutboundGuard(ctx, func(_ *http.Request, accountID int64) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		fresh, err := runtime.requestPrincipal(ctx, key, request, service.CompositeRouteEndpointResponses)
		if err != nil {
			return service.ErrAIRoutingPrincipalRevoked
		}
		_, latestOrigin, err := loadAIResponsesBinding(ctx, store, fresh, request)
		if err != nil || latestOrigin == nil || *latestOrigin != *origin {
			return errors.New("AI response origin revoked")
		}
		latestPolicy, err := runtime.policies.GetPublished(ctx, fresh.Group.ID)
		if err != nil || latestPolicy == nil {
			return errors.New("AI response policy unavailable")
		}
		latestAccount, err := validator.ValidateAIRoutingResponsesOrigin(ctx, fresh, latestPolicy, latestOrigin)
		if err != nil || latestAccount == nil || latestAccount.ID != accountID || accountID != origin.Account {
			return errors.New("AI response origin revoked")
		}
		if err := checkBackgroundOrigin(ctx, fresh); err != nil {
			return service.ErrAIBackgroundConflict
		}
		return checkSettlement(ctx)
	})
	resp, err := forwarder.ForwardAIRoutingResponsesControl(guarded, account, origin.Model, c.Request.Method, responseID, operation, c.Request.URL.RawQuery)
	if err != nil || resp == nil || resp.Body == nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if errors.Is(err, service.ErrAIRoutingPrincipalRevoked) {
			aiResponsesControlError(c, 403, "ai_source_revoked")
		} else if errors.Is(err, service.ErrAIBackgroundConflict) {
			aiResponsesControlError(c, 403, "ai_background_origin_unavailable")
		} else if errors.Is(err, service.ErrAIBackgroundRecoveryPending) {
			aiResponsesControlError(c, 409, "ai_background_settlement_pending")
		} else {
			aiResponsesControlError(c, 502, "ai_response_control_failed")
		}
		return true
	}
	defer func() { _ = resp.Body.Close() }()
	for _, name := range []string{"Content-Type", "X-Request-ID", "OpenAI-Processing-Ms"} {
		if value := resp.Header.Get(name); value != "" {
			c.Header(name, value)
		}
	}
	if resp.Header.Get("Content-Type") == "" {
		c.Header("Content-Type", "application/json")
	}
	if c.Request.Method == http.MethodDelete || resp.StatusCode >= 300 {
		body, err := io.ReadAll(io.LimitReader(resp.Body, aiToolJSONLimit+1))
		if err != nil || len(body) > aiToolJSONLimit {
			aiResponsesControlError(c, 502, "ai_response_control_invalid_response")
			return true
		}
		if c.Request.Method == http.MethodDelete && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			storageKey, keyErr := aiResponsesOriginKey(key, ref)
			if keyErr != nil {
				aiResponsesControlError(c, 503, "ai_response_revocation_failed")
				return true
			}
			// The upstream deletion has already happened. Persist revocation even
			// if the downstream disconnects immediately after that success.
			revokeCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			err = revoker.RevokeOriginBinding(revokeCtx, storageKey, service.AIRoutingResponseOriginTTL)
			stop()
			if err != nil {
				aiResponsesControlError(c, 503, "ai_response_revocation_failed")
				return true
			}
			// A deletion response identifies the resource already authorized above;
			// it must not introduce another response's identifiers after revocation.
			if len(body) > 0 {
				inspector := &aiResponsesOriginInspector{responseID: responseID, emitted: map[string]bool{}, bind: func(kind, id, _ string) error {
					if kind != "response" || id != responseID {
						return errors.New("unexpected identifier in AI response deletion")
					}
					return nil
				}}
				if err := inspector.inspect(body, ""); err != nil {
					aiResponsesControlError(c, 502, "ai_response_control_invalid_response")
					return true
				}
			}
		} else if json.Valid(body) {
			inspector := &aiResponsesOriginInspector{responseID: responseID, emitted: map[string]bool{}, bind: func(kind, id, parent string) error {
				binding := *origin
				binding.ResponseID = parent
				return saveAIResponsesControlBinding(ctx, store, key, service.AIRoutingOriginRef{Kind: kind, ID: id}, binding)
			}}
			if err := inspector.inspect(body, ""); err != nil {
				aiResponsesControlError(c, 502, "ai_response_binding_failed")
				return true
			}
		}
		c.Data(resp.StatusCode, resp.Header.Get("Content-Type"), body)
		return true
	}
	base := c.Writer
	writer := newAIResponsesControlWriter(base, responseID, func(kind, id, parent string) error {
		binding := *origin
		binding.ResponseID = parent
		return saveAIResponsesControlBinding(ctx, store, key, service.AIRoutingOriginRef{Kind: kind, ID: id}, binding)
	}, func(error) { cancel() })
	if origin.Background && operation != "input_items" && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		observed := service.WithAIRoutingBackgroundTerminalObserver(ctx, func(recordCtx context.Context, accountID int64, id string, terminal service.AIRoutingBackgroundTerminal) error {
			if accountID != origin.Account || id != responseID {
				return service.ErrAIBackgroundConflict
			}
			recording, stop := context.WithTimeout(context.WithoutCancel(recordCtx), 2*time.Second)
			defer stop()
			return background[0].RecordResponseTerminal(recording, key, accountID, id, terminal)
		})
		inspect := writer.inspect
		writer.inspect = func(payload []byte, event string) error {
			if err := inspect(payload, event); err != nil {
				return err
			}
			return service.ObserveAIRoutingBackgroundResponsePayload(observed, origin.Account, payload, event)
		}
	}
	writer.WriteHeader(resp.StatusCode)
	_, err = io.Copy(aiResponsesControlFlusher{writer}, resp.Body)
	if err == nil {
		err = writer.finish()
	}
	if err != nil {
		// Use the original writer for the failure; the guarded writer is closed
		// permanently after a binding error and must not accept recovery bytes.
		aiResponsesControlError(c, 502, "ai_response_binding_failed")
		return true
	}
	return true
}

type aiResponsesControlFlusher struct{ writer *aiToolWriter }

func (f aiResponsesControlFlusher) Write(data []byte) (int, error) {
	n, err := f.writer.Write(data)
	if err == nil && f.writer.isStream() {
		f.writer.Flush()
	}
	return n, err
}

// input_items can include an earlier response's tool or opaque state. Retain
// that immutable parent instead of moving the identifier to the queried response.
func saveAIResponsesControlBinding(ctx context.Context, store service.AIRoutingOriginStore, key *service.APIKey, ref service.AIRoutingOriginRef, origin service.AIRoutingOriginBinding) error {
	storageKey, err := aiResponsesOriginKey(key, ref)
	if err != nil {
		return err
	}
	lookup, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	rows, err := store.LoadOriginBindings(lookup, []string{storageKey})
	if err != nil || len(rows) != 1 {
		return errors.New("AI response origin lookup failed")
	}
	if rows[0] == "" {
		return saveAIResponsesBinding(lookup, store, key, ref, origin)
	}
	var existing service.AIRoutingOriginBinding
	if !requestmodel.HasUniqueJSONMembers([]byte(rows[0])) || json.Unmarshal([]byte(rows[0]), &existing) != nil || !existing.Valid() || aiResponsesToolBinding(existing) != aiResponsesToolBinding(origin) {
		return errors.New("AI response origin conflicts with another execution")
	}
	if ref.Kind == "response" || ref.Kind == "compaction" {
		if existing != origin {
			return errors.New("AI response origin changed")
		}
		return nil
	}
	return validateAIResponsesParents(lookup, store, key, []service.AIRoutingOriginBinding{existing})
}
