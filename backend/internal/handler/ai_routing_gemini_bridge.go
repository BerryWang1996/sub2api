package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// The existing Responses services retain auth, proxy, capacity guard, account
// health, cancellation and usage accounting. Only the wire representations are
// adapted here; the native origin writer remains outside this bounded writer.
func (h *GatewayHandler) forwardAIGeminiCompatible(ctx context.Context, c *gin.Context, account *service.Account, model, action string, stream bool, body []byte) (*service.ForwardResult, error) {
	converted, err := service.GeminiRequestToResponses(body, model, action, stream)
	if err != nil {
		googleError(c, http.StatusBadRequest, "Gemini request requires a native-compatible route")
		return nil, err
	}
	base := c.Writer
	output := &service.GeminiResponsesOutput{Model: model}
	if scope, ok := service.CompositeRouteScopeFromContext(ctx); ok && scope.PublicModel != "" {
		output.Model = scope.PublicModel
	}
	writer := &aiToolWriter{ResponseWriter: base}
	writer.inspect = func(raw []byte, _ string) error {
		if writer.Status() >= 300 {
			encoded, _ := json.Marshal(gin.H{"error": gin.H{"code": writer.Status(), "status": "FAILED_PRECONDITION", "message": "The selected route could not execute this Gemini request"}})
			writer.Header().Del("Content-Length")
			if writer.isStream() {
				writer.pending = append(append([]byte("data: "), encoded...), []byte("\n\n")...)
			} else {
				writer.pending = encoded
			}
			return nil
		}
		encoded, err := output.Convert(raw, stream)
		if err != nil {
			return err
		}
		writer.Header().Del("Content-Length")
		if stream {
			if len(encoded) == 0 {
				writer.pending = nil
			} else {
				writer.pending = append(append([]byte("data: "), encoded...), []byte("\n\n")...)
			}
		} else {
			writer.pending = encoded
		}
		return nil
	}
	writer.inspectDone = func() error {
		writer.pending = nil
		if !output.Complete {
			return errors.New("gemini bridge missing terminal event")
		}
		return nil
	}
	writer.onFailure = func(error) {
		c.Abort()
		if ctx.Err() == nil {
			c.Writer = base
			if !base.Written() {
				googleError(c, http.StatusBadGateway, "Upstream response cannot be represented as Gemini")
			} else if stream {
				_, _ = base.Write([]byte("data: {\"error\":{\"code\":502,\"status\":\"INTERNAL\",\"message\":\"Upstream Gemini bridge interrupted\"}}\n\n"))
				base.Flush()
			}
			c.Writer = writer
		}
		service.MarkResponseCommitted(c)
	}
	c.Writer = writer
	defer func() { c.Writer = base }()
	var result *service.ForwardResult
	unavailable := func() (*service.ForwardResult, error) {
		c.Writer = base
		googleError(c, http.StatusServiceUnavailable, "Selected Gemini compatibility service is unavailable")
		c.Writer = writer
		return nil, errors.New("gemini bridge service unavailable")
	}
	switch account.Platform {
	case service.PlatformOpenAI:
		if h.openAIGatewayService == nil {
			return unavailable()
		}
		var forwarded *service.OpenAIForwardResult
		forwarded, err = h.openAIGatewayService.Forward(ctx, c, account, converted)
		if forwarded != nil {
			result = geminiBridgeOpenAIResult(forwarded)
		}
	case service.PlatformAnthropic:
		if h.gatewayService == nil {
			return unavailable()
		}
		result, err = h.gatewayService.ForwardAsResponses(ctx, c, account, converted, nil)
	case service.PlatformAntigravity:
		if h.antigravityGatewayService == nil {
			return unavailable()
		}
		result, err = h.antigravityGatewayService.ForwardAsResponses(ctx, c, account, converted, nil)
	default:
		return unavailable()
	}
	if finishErr := writer.finish(); err == nil {
		err = finishErr
	}
	if err == nil && writer.Status() < 300 && !output.Complete {
		err = writer.fail(errors.New("gemini bridge missing completion"))
	}
	return result, err
}

func geminiBridgeOpenAIResult(result *service.OpenAIForwardResult) *service.ForwardResult {
	// OpenAI input includes cached tokens; the shared Gateway billing path uses
	// Anthropic's uncached-input convention and a separate cache-read field.
	cached := min(result.Usage.CacheReadInputTokens, result.Usage.InputTokens)
	created := min(result.Usage.CacheCreationInputTokens, result.Usage.InputTokens-cached)
	return &service.ForwardResult{
		RequestID: result.RequestID, UpstreamHeaders: result.UpstreamHeaders,
		Usage: service.ClaudeUsage{InputTokens: result.Usage.InputTokens - cached - created, OutputTokens: result.Usage.OutputTokens, CacheReadInputTokens: cached, CacheCreationInputTokens: created, ImageOutputTokens: result.Usage.ImageOutputTokens},
		Model: result.Model, UpstreamModel: result.UpstreamModel, UpstreamResponseModel: result.UpstreamResponseModel, UpstreamResponseModelConflict: result.UpstreamResponseModelConflict,
		UpstreamResponseServiceTier: result.UpstreamResponseServiceTier, Stream: result.Stream, Duration: result.Duration, FirstTokenMs: result.FirstTokenMs, ClientDisconnect: result.ClientDisconnect,
		ReasoningEffort: result.ReasoningEffort, RequestedReasoningEffort: result.RequestedReasoningEffort, ServiceTier: result.ServiceTier,
	}
}
