package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// One bounded HTTP generation is an executor of a WS lane, not a new HTTP
// ingress. Admission, slots, origins and billing remain owned by the lane plan.
type aiWSHTTPExecutor struct {
	h             *GatewayHandler
	c             *gin.Context
	ctx           context.Context
	cancel        context.CancelFunc
	account       *service.Account
	body          []byte
	publicModel   string
	revalidate    func(context.Context) error
	started       atomic.Bool
	gatewayResult *service.ForwardResult // assigned by Run before Finish reads it
}

func (h *GatewayHandler) newAIWSHTTPExecutor(ctx context.Context, c *gin.Context, account *service.Account, body []byte, publicModel string, revalidate func(context.Context) error) (service.AIRoutingWSTurnExecutor, error) {
	if h == nil || c == nil || c.Request == nil || account == nil || revalidate == nil || !service.HasAIRoutingOutboundGuard(ctx) {
		return nil, errors.New("ai websocket HTTP executor requires a complete admission plan")
	}
	if len(body) > 32<<20 {
		return nil, errors.New("websocket HTTP create exceeds limit")
	}
	wsRequest, err := service.NewAIRoutingWSRequest(body, "metadata")
	if err != nil {
		return nil, err
	}
	if wsRequest.Warmup || wsRequest.StoreResponse || wsRequest.PreviousResponseID != "" || wsRequest.Background {
		return nil, errors.New("websocket HTTP bridge requires a stateless generation")
	}
	converted := append([]byte(nil), body...)
	for _, field := range []string{"type", "stream_id", "generate"} {
		converted, err = sjson.DeleteBytes(converted, field)
		if err != nil {
			return nil, err
		}
	}
	converted, err = sjson.SetBytes(converted, "stream", true)
	if err != nil {
		return nil, err
	}
	request, err := service.NewAIRoutingResponsesRequest(converted, "metadata")
	if err != nil {
		return nil, err
	}
	if request.RequiresNativeResponses || request.RequiresStoredResponses {
		return nil, errors.New("request requires native websocket provider state")
	}
	for _, ref := range request.OriginRefs {
		if ref.Kind != "tool_call" {
			return nil, errors.New("provider reference cannot cross HTTP websocket bridge")
		}
	}
	// HTTP bridge execution has no implicit provider session. Every tool result
	// must include its preceding call in explicit client history; provenance is
	// independently checked by the ingress and the final outbound guard.
	calls := make(map[string]string)
	input := gjson.GetBytes(converted, "input")
	if input.IsArray() {
		for _, item := range input.Array() {
			switch kind := item.Get("type").Str; kind {
			case "function_call", "custom_tool_call":
				id := item.Get("call_id").Str
				if _, exists := calls[id]; exists {
					return nil, errors.New("duplicate websocket bridge call")
				}
				calls[id] = kind
			case "function_call_output", "custom_tool_call_output":
				id := item.Get("call_id").Str
				if calls[id] != strings.TrimSuffix(kind, "_output") {
					return nil, errors.New("websocket HTTP bridge needs the original tool call in explicit history")
				}
				delete(calls, id)
			}
		}
	}
	if len(calls) > 0 {
		return nil, errors.New("websocket HTTP bridge has unanswered tool calls")
	}
	turnCtx, cancel := context.WithCancel(ctx)
	private := c.Copy()
	private.Writer = newAIWSPlanWriter()
	private.Request = c.Request.Clone(turnCtx)
	private.Request.Header = c.Request.Header.Clone()
	private.Request.Method = http.MethodPost
	u := *private.Request.URL
	u.Path = "/v1/responses"
	u.RawPath = ""
	u.RawQuery = ""
	private.Request.URL = &u
	for _, header := range []string{"Upgrade", "Connection", "Sec-WebSocket-Key", "Sec-WebSocket-Version", "Sec-WebSocket-Protocol", "Sec-WebSocket-Extensions"} {
		private.Request.Header.Del(header)
	}
	private.Request.Header.Set("Content-Type", "application/json")
	requestmodel.ResetRequestBody(private.Request, converted)
	// The private transport copy must select HTTP even when the account also
	// supports a WS pool. Billing/metrics retain the original lane context.
	service.SetOpenAIClientTransport(private, service.OpenAIClientTransportHTTP)
	return &aiWSHTTPExecutor{h: h, c: private, ctx: turnCtx, cancel: cancel, account: account, body: converted, publicModel: publicModel, revalidate: revalidate}, nil
}

func (e *aiWSHTTPExecutor) ConnID() string { return "" }
func (e *aiWSHTTPExecutor) Cancel()        { e.cancel() }
func (e *aiWSHTTPExecutor) Close()         { e.cancel() }
func (e *aiWSHTTPExecutor) Steer(context.Context, []byte) error {
	return errors.New("http-backed websocket turns do not support provider steering")
}
func (e *aiWSHTTPExecutor) GatewayResult() *service.ForwardResult { return e.gatewayResult }

func (e *aiWSHTTPExecutor) Run(emit func([]byte) error) (*service.OpenAIForwardResult, error) {
	if !e.started.CompareAndSwap(false, true) || emit == nil {
		return nil, errors.New("websocket HTTP turn can run only once")
	}
	defer e.cancel()
	if err := e.ctx.Err(); err != nil {
		return nil, err
	}
	if err := e.revalidate(e.ctx); err != nil {
		return nil, err
	}
	base := e.c.Writer
	var terminal, responseID string
	var outputBytes int
	writer := &aiToolWriter{ResponseWriter: base}
	writer.inspect = func(raw []byte, _ string) error {
		writer.pending = nil // no HTTP frame is retained or written to a WS socket
		if err := e.ctx.Err(); err != nil {
			return err
		}
		if writer.Status() >= 300 {
			return errors.New("upstream HTTP bridge rejected the request")
		}
		if len(raw) > aiToolEventLimit || !gjson.ValidBytes(raw) || !requestmodel.HasUniqueJSONMembers(raw) {
			return errors.New("invalid websocket bridge response event")
		}
		if len(raw) > (16<<20)-outputBytes {
			return errors.New("websocket bridge output exceeds limit")
		}
		outputBytes += len(raw)
		root := gjson.ParseBytes(raw)
		typ := root.Get("type").Str
		if terminal != "" {
			return errors.New("websocket bridge output after terminal")
		}
		if typ == "" || !strings.HasPrefix(typ, "response.") && typ != "error" {
			return errors.New("unsupported websocket bridge event")
		}
		if item := root.Get("item"); item.Get("encrypted_content").Exists() {
			return errors.New("opaque output requires native websocket transport")
		}
		for _, item := range root.Get("response.output").Array() {
			if item.Get("encrypted_content").Exists() {
				return errors.New("opaque output requires native websocket transport")
			}
		}
		if id := root.Get("response.id").Str; id != "" {
			if responseID != "" && responseID != id {
				return errors.New("websocket HTTP response identity changed")
			}
			responseID = id
		}
		switch typ {
		case "response.completed", "response.incomplete", "response.failed", "error":
			terminal = typ
		}
		out := append([]byte(nil), raw...)
		if e.publicModel != "" && root.Get("response.model").Exists() {
			var err error
			out, err = sjson.SetBytes(out, "response.model", e.publicModel)
			if err != nil {
				return err
			}
		}
		if err := emit(out); err != nil {
			return err
		}
		return nil
	}
	writer.inspectDone = func() error {
		writer.pending = nil
		if terminal == "" {
			return errors.New("websocket HTTP stream ended without terminal")
		}
		return nil
	}
	writer.onFailure = func(error) { e.cancel(); service.MarkResponseCommitted(e.c) }
	e.c.Writer = writer
	defer func() { e.c.Writer = base }()
	var result *service.OpenAIForwardResult
	var err error
	switch e.account.Platform {
	case service.PlatformOpenAI:
		if e.h.openAIGatewayService == nil {
			return nil, errors.New("openai HTTP executor unavailable")
		}
		result, err = e.h.openAIGatewayService.Forward(e.ctx, e.c, e.account, e.body)
	case service.PlatformAnthropic:
		if e.h.gatewayService == nil {
			return nil, errors.New("anthropic HTTP executor unavailable")
		}
		e.gatewayResult, err = e.h.gatewayService.ForwardAsResponses(e.ctx, e.c, e.account, e.body, nil)
	case service.PlatformGemini:
		if e.h.geminiCompatService == nil {
			return nil, errors.New("gemini HTTP executor unavailable")
		}
		e.gatewayResult, err = e.h.geminiCompatService.ForwardAsResponses(e.ctx, e.c, e.account, e.body, nil)
	case service.PlatformAntigravity:
		if e.h.antigravityGatewayService == nil {
			return nil, errors.New("antigravity HTTP executor unavailable")
		}
		e.gatewayResult, err = e.h.antigravityGatewayService.ForwardAsResponses(e.ctx, e.c, e.account, e.body, nil)
	default:
		return nil, errors.New("unsupported websocket HTTP execution platform")
	}
	if e.gatewayResult != nil {
		result = aiWSHTTPGatewayResult(e.gatewayResult)
	}
	if finishErr := writer.finish(); err == nil {
		err = finishErr
	}
	if err == nil && terminal == "" {
		err = errors.New("websocket HTTP response did not complete")
	}
	if err == nil && (terminal == "error" || terminal == "response.failed") {
		err = errors.New("websocket HTTP upstream failed")
	}
	if result != nil {
		result.OpenAIWSMode = true
		result.UpstreamTerminalEvent = terminal
		result.ResponseID = responseID
	}
	return result, err
}

func aiWSHTTPGatewayResult(result *service.ForwardResult) *service.OpenAIForwardResult {
	return &service.OpenAIForwardResult{
		RequestID: result.RequestID, UpstreamHeaders: result.UpstreamHeaders, Model: result.Model, UpstreamModel: result.UpstreamModel, BillingModel: result.Model,
		Usage:                 service.OpenAIUsage{InputTokens: result.Usage.InputTokens + result.Usage.CacheReadInputTokens + result.Usage.CacheCreationInputTokens, OutputTokens: result.Usage.OutputTokens, CacheReadInputTokens: result.Usage.CacheReadInputTokens, CacheCreationInputTokens: result.Usage.CacheCreationInputTokens, ImageOutputTokens: result.Usage.ImageOutputTokens},
		UpstreamResponseModel: result.UpstreamResponseModel, UpstreamResponseModelConflict: result.UpstreamResponseModelConflict, UpstreamResponseServiceTier: result.UpstreamResponseServiceTier,
		Stream: result.Stream, Duration: result.Duration, FirstTokenMs: result.FirstTokenMs, ClientDisconnect: result.ClientDisconnect, ReasoningEffort: result.ReasoningEffort, RequestedReasoningEffort: result.RequestedReasoningEffort, ServiceTier: result.ServiceTier,
		ImageCount: result.ImageCount, ImageEndpoint: result.ImageEndpoint, ImageSize: result.ImageSize, ImageInputSize: result.ImageInputSize, ImageOutputSizes: result.ImageOutputSizes, ImageSizeSource: result.ImageSizeSource, ImageSizeBreakdown: result.ImageSizeBreakdown, SearchCount: result.SearchCount, AudioUsage: result.AudioUsage,
	}
}

var _ service.AIRoutingWSTurnExecutor = (*aiWSHTTPExecutor)(nil)
