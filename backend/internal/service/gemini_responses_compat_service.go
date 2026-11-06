package service

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// GeminiResponsesToolCallPrefix identifies gateway-owned native tool state.
// Even a completed historical call with this prefix must retain its origin.
const GeminiResponsesToolCallPrefix = "call_s2g_"

// GeminiResponsesToolState is short-lived continuation data, never decision
// context or log content. Part preserves the provider's signature and call ID.
type GeminiResponsesToolState struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Part      json.RawMessage `json:"part"`
	Batch     string          `json:"batch,omitempty"`
	Index     int             `json:"index"`
}

// Callbacks are installed by the authenticated ingress with its original
// cancellation context, key, source group, candidate, model and actual account.
// Save must finish before the corresponding call ID becomes client-visible.
type GeminiResponsesToolStateCallbacks struct {
	Save func(context.Context, string, GeminiResponsesToolState) error
	Load func(context.Context, string) (GeminiResponsesToolState, error)
}

type geminiResponsesToolStateKey struct{}

func WithGeminiResponsesToolState(ctx context.Context, callbacks GeminiResponsesToolStateCallbacks) context.Context {
	return context.WithValue(ctx, geminiResponsesToolStateKey{}, callbacks)
}

type geminiResponsesForwardOptions struct {
	prepare func([]byte) ([]byte, error)
	consume func(*http.Response, bool) (*geminiStreamResult, error)
}

// ForwardAsResponses reuses Gemini's ordinary transport and account handling.
// Only the stateless, convertible Responses subset is bridged. Provider-owned
// storage/references/opaque input are rejected before a Gemini request is sent.
func (s *GeminiMessagesCompatService) ForwardAsResponses(ctx context.Context, c *gin.Context, account *Account, body []byte, _ *ParsedRequest) (*ForwardResult, error) {
	start := time.Now()
	invalid := func(err error) (*ForwardResult, error) {
		writeResponsesError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, err
	}
	if account == nil || account.Platform != PlatformGemini {
		return invalid(errors.New("gemini account required for Responses compatibility"))
	}
	request, err := NewAIRoutingResponsesRequest(body, "metadata")
	if err != nil {
		return invalid(err)
	}
	if request.RequiresNativeResponses || request.RequiresStoredResponses {
		return invalid(errors.New("request requires a native Responses account; Gemini bridge requires store:false and inline convertible input"))
	}
	if err := validateGeminiResponsesBridgeFields(body); err != nil {
		return invalid(err)
	}
	adapted, mapping, err := adaptResponsesClientToolsForAnthropic(body)
	if err != nil {
		return invalid(err)
	}
	var responses apicompat.ResponsesRequest
	if err = json.Unmarshal(adapted, &responses); err != nil {
		return invalid(err)
	}
	callbacks, _ := ctx.Value(geminiResponsesToolStateKey{}).(GeminiResponsesToolStateCallbacks)
	replay, err := prepareGeminiResponsesReplay(ctx, &responses, callbacks)
	if err != nil {
		if !handleAIRoutingCapacityError(c, err) {
			writeResponsesError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		}
		return nil, err
	}
	claudeRequest, err := apicompat.ResponsesToAnthropicRequest(&responses)
	if err != nil {
		return invalid(err)
	}
	claudeBody, err := json.Marshal(claudeRequest)
	if err != nil {
		return nil, err
	}
	options := &geminiResponsesForwardOptions{
		prepare: func(wire []byte) ([]byte, error) {
			return replay.apply(wire, responses.ToolChoice)
		},
		consume: func(resp *http.Response, upstreamStream bool) (*geminiStreamResult, error) {
			return s.consumeGeminiResponses(ctx, c, resp, start, responses.Model, responses.Stream, upstreamStream, mapping, callbacks)
		},
	}
	result, err := s.forwardClaudeBodyAsChatCompletions(ctx, c, account, claudeBody, responses.Model, responses.Stream, false, start, options)
	if result != nil {
		result.UpstreamResponseModel = observedUpstreamResponseModel(c)
		result.UpstreamResponseModelConflict = observedUpstreamResponseModelConflict(c)
	}
	return result, err
}

func validateGeminiResponsesBridgeFields(body []byte) error {
	root := gjson.ParseBytes(body)
	if !aiCanonicalFields(root, "cachedContent", "cached_content", "cache_control", "text", "prompt_cache_options", "prompt_cache_key", "truncation", "context_management", "service_tier", "include", "parallel_tool_calls") {
		return errors.New("ambiguous Gemini Responses bridge fields")
	}
	// These fields either select provider state or express an output constraint
	// that the existing Messages->Gemini conversion cannot preserve.
	for _, field := range []string{"cachedContent", "cached_content", "cache_control", "text", "prompt_cache_options", "prompt_cache_key", "truncation", "context_management", "service_tier"} {
		if value := root.Get(field); value.Exists() && value.Type != gjson.Null {
			return fmt.Errorf("gemini Responses bridge does not support %s", field)
		}
	}
	if include := root.Get("include"); include.Exists() && (!include.IsArray() || len(include.Array()) > 0) {
		return errors.New("gemini Responses bridge does not support include")
	}
	if parallel := root.Get("parallel_tool_calls"); parallel.Exists() && parallel.Type != gjson.True {
		return errors.New("gemini Responses bridge cannot guarantee parallel_tool_calls:false")
	}
	var items []gjson.Result
	if input := root.Get("input"); input.IsArray() {
		items = input.Array()
	}
	for _, item := range items {
		if !aiCanonicalFields(item, "encrypted_content", "thoughtSignature", "thought_signature", "cache_control", "provider_data") {
			return errors.New("ambiguous Gemini Responses input state")
		}
		for _, field := range []string{"encrypted_content", "thoughtSignature", "thought_signature", "cache_control", "provider_data"} {
			if item.Get(field).Exists() {
				return errors.New("gemini Responses bridge cannot discard native input state")
			}
		}
		parts := item.Get("content")
		if item.Get("type").String() == "function_call_output" || item.Get("type").String() == "custom_tool_call_output" {
			parts = item.Get("output")
		}
		if !parts.IsArray() {
			continue
		}
		for _, part := range parts.Array() {
			if !aiCanonicalFields(part, "encrypted_content", "thoughtSignature", "thought_signature", "file_id", "cache_control", "provider_data") {
				return errors.New("ambiguous Gemini Responses content state")
			}
			for _, field := range []string{"encrypted_content", "thoughtSignature", "thought_signature", "file_id", "cache_control", "provider_data"} {
				if part.Get(field).Exists() {
					return errors.New("gemini Responses bridge cannot discard native content state")
				}
			}
			switch part.Get("type").String() {
			case "input_text", "output_text":
			case "input_image":
				if !strings.HasPrefix(part.Get("image_url").String(), "data:image/") {
					return errors.New("gemini Responses bridge requires inline image data")
				}
			default:
				return errors.New("gemini Responses bridge does not support this content part")
			}
		}
	}
	for _, tool := range root.Get("tools").Array() {
		if !aiCanonicalFields(tool, "strict") || tool.Get("strict").Bool() {
			return errors.New("gemini Responses bridge cannot guarantee strict function schemas")
		}
	}
	return nil
}

func geminiResponsesStateError() error {
	return &AIRoutingCapacityError{Check: AIRoutingCapabilityCheck{Reason: "execution_origin_unavailable"}}
}

// consumeGeminiResponses uses the existing Gemini->Anthropic processor and
// Anthropic->Responses event converter, without routing through Chat events.
// It never fabricates a successful terminal event for an interrupted stream.
func (s *GeminiMessagesCompatService) consumeGeminiResponses(ctx context.Context, c *gin.Context, resp *http.Response, start time.Time, model string, clientStream, upstreamStream bool, mapping apicompat.ResponsesClientToolMapping, callbacks GeminiResponsesToolStateCallbacks) (*geminiStreamResult, error) {
	return consumeGeminiResponses(ctx, c, resp, start, model, clientStream, upstreamStream, mapping, callbacks, s.cfg, s.responseHeaderFilter, nil)
}

type geminiResponsesChatOutput struct{ includeUsage bool }

func consumeGeminiResponses(ctx context.Context, c *gin.Context, resp *http.Response, start time.Time, model string, clientStream, upstreamStream bool, mapping apicompat.ResponsesClientToolMapping, callbacks GeminiResponsesToolStateCallbacks, cfg *config.Config, filter *responseheaders.CompiledHeaderFilter, chat *geminiResponsesChatOutput) (*geminiStreamResult, error) {
	result := &geminiStreamResult{usage: &ClaudeUsage{}}
	observer := beginUpstreamResponseModelObservation(c)
	state := apicompat.NewAnthropicEventToResponsesState()
	state.Model = model
	restorer := apicompat.NewResponsesClientToolStreamRestorer(mapping)
	processor := antigravity.NewStreamingProcessor(model)
	sequence := &geminiResponsesToolSequence{batch: randomHex(16), nativeIDs: map[string]bool{}}
	toolArguments := map[string]string{}
	activeTool := ""
	var completed *apicompat.ResponsesResponse
	var chatState *apicompat.ResponsesEventToChatState
	if chat != nil {
		chatState = apicompat.NewResponsesEventToChatState()
		chatState.Model, chatState.IncludeUsage = model, chat.includeUsage
	}
	writeEvents := func(events []apicompat.ResponsesStreamEvent) error {
		for _, event := range events {
			for _, restored := range restorer.Restore(event) {
				if restored.Type == "response.completed" || restored.Type == "response.incomplete" {
					completed = restored.Response
				}
				if !clientStream || result.clientDisconnect {
					continue
				}
				wire, err := apicompat.ResponsesEventToSSE(restored)
				if chatState != nil {
					var frames strings.Builder
					for _, chunk := range apicompat.ResponsesEventToChatChunks(&restored, chatState) {
						frame, convertErr := apicompat.ChatChunkToSSE(chunk)
						if convertErr != nil {
							return convertErr
						}
						_, _ = frames.WriteString(frame)
					}
					wire = frames.String()
				}
				if err != nil {
					return err
				}
				responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, filter)
				c.Header("Content-Type", "text/event-stream")
				c.Header("Cache-Control", "no-cache")
				c.Header("X-Accel-Buffering", "no")
				if _, err := c.Writer.WriteString(wire); err != nil {
					result.clientDisconnect = true
					return err
				}
				c.Writer.Flush()
			}
		}
		return nil
	}
	consume := func(raw []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		observer.ObserveGemini(raw)
		wire, usage, terminal, err := prepareGeminiResponsesChunk(ctx, raw, callbacks, sequence)
		if usage != nil {
			result.usage = usage
		}
		if err != nil {
			return err
		}
		if result.firstTokenMs == nil {
			ms := int(time.Since(start).Milliseconds())
			result.firstTokenMs = &ms
		}
		for _, part := range gjson.GetBytes(wire, "candidates.0.content.parts").Array() {
			if fc := part.Get("functionCall"); fc.Exists() {
				toolArguments[fc.Get("id").String()] = fc.Get("args").Raw
			}
		}
		// StreamingProcessor expects the v1internal envelope. Always construct
		// it, including for direct AI Studio/Vertex responses.
		claudeEvents := processor.ProcessLine("data: {\"response\":" + string(wire) + "}")
		for _, line := range strings.Split(string(claudeEvents), "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var event apicompat.AnthropicStreamEvent
			if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event); err != nil {
				return errors.New("invalid converted Gemini stream event")
			}
			if event.Type == "message_delta" && event.Delta != nil && gjson.GetBytes(wire, "candidates.0.finishReason").String() == "MAX_TOKENS" {
				event.Delta.StopReason = "max_tokens"
			}
			if event.ContentBlock != nil && event.ContentBlock.Type == "tool_use" {
				activeTool = event.ContentBlock.ID
			}
			if event.Delta != nil && event.Delta.Type == "input_json_delta" && activeTool != "" {
				// The shared processor decodes args into interface{} (float64).
				// Retain the exact complete native JSON so large integers are not
				// rounded in the client call or signature replay comparison.
				event.Delta.PartialJSON = toolArguments[activeTool]
			}
			if event.Type == "content_block_stop" {
				delete(toolArguments, activeTool)
				activeTool = ""
			}
			if err := writeEvents(apicompat.AnthropicEventToResponsesEvents(&event, state)); err != nil {
				return err
			}
		}
		result.finished = terminal
		return nil
	}
	var consumeErr error
	if upstreamStream {
		scanner := bufio.NewScanner(resp.Body)
		maxLine := defaultMaxLineSize
		if cfg != nil && cfg.Gateway.MaxLineSize > 0 {
			maxLine = cfg.Gateway.MaxLineSize
		}
		scanner.Buffer(make([]byte, min(64*1024, maxLine)), maxLine)
		var data strings.Builder
		dispatch := func() error {
			if data.Len() == 0 {
				return nil
			}
			payload := data.String()
			data.Reset()
			if payload == "[DONE]" {
				return errors.New("gemini stream ended without a finishReason")
			}
			return consume([]byte(payload))
		}
		for scanner.Scan() {
			line := strings.TrimSuffix(scanner.Text(), "\r")
			if line == "" {
				consumeErr = dispatch()
			} else if value, ok := strings.CutPrefix(line, "data:"); ok {
				if data.Len() > 0 {
					_ = data.WriteByte('\n')
				}
				value = strings.TrimPrefix(value, " ")
				if data.Len()+len(value) > maxLine {
					consumeErr = ErrUpstreamResponseBodyTooLarge
				} else {
					_, _ = data.WriteString(value)
				}
			}
			if consumeErr != nil || result.finished {
				break
			}
		}
		if consumeErr == nil && !result.finished {
			consumeErr = scanner.Err()
			if consumeErr == nil {
				consumeErr = dispatch()
			}
		}
	} else {
		var raw []byte
		raw, consumeErr = readUpstreamResponseBodyLimited(resp.Body, resolveUpstreamResponseReadLimit(cfg))
		if consumeErr == nil {
			consumeErr = consume(raw)
		}
	}
	if consumeErr == nil && (!result.finished || completed == nil) {
		consumeErr = errors.New("gemini response ended without a finishReason")
	}
	if consumeErr != nil {
		if !result.clientDisconnect {
			if !c.Writer.Written() {
				if !handleAIRoutingCapacityError(c, consumeErr) {
					writeResponsesError(c, http.StatusBadGateway, "upstream_error", "Incomplete or unsupported Gemini response")
				}
			} else if clientStream {
				_, _ = c.Writer.WriteString("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"upstream_error\",\"message\":\"Incomplete or unsupported Gemini response\"}}\n\n")
				c.Writer.Flush()
			}
		}
		return result, consumeErr
	}
	if !clientStream {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, filter)
		c.Header("Content-Type", "application/json; charset=utf-8")
		if chat != nil {
			c.JSON(http.StatusOK, apicompat.ResponsesToChatCompletions(completed, model))
		} else {
			c.JSON(http.StatusOK, completed)
		}
	} else if chat != nil {
		if _, err := c.Writer.WriteString("data: [DONE]\n\n"); err != nil {
			result.clientDisconnect = true
			return result, err
		}
		c.Writer.Flush()
	}
	return result, nil
}

type geminiResponsesToolSequence struct {
	batch     string
	index     int
	nativeIDs map[string]bool
}

func prepareGeminiResponsesChunk(ctx context.Context, raw []byte, callbacks GeminiResponsesToolStateCallbacks, sequence *geminiResponsesToolSequence) ([]byte, *ClaudeUsage, bool, error) {
	if !gjson.ValidBytes(raw) || !requestmodel.HasUniqueJSONMembers(raw) {
		return nil, nil, false, errors.New("invalid Gemini response JSON")
	}
	envelope := gjson.ParseBytes(raw)
	if !envelope.IsObject() || !aiCanonicalFields(envelope, "response", "candidates", "usageMetadata", "error") || envelope.Get("error").Exists() {
		return nil, nil, false, errors.New("invalid Gemini response envelope")
	}
	if wrapped := envelope.Get("response"); wrapped.Exists() {
		if !wrapped.IsObject() || envelope.Get("candidates").Exists() {
			return nil, nil, false, errors.New("ambiguous Gemini response envelope")
		}
		raw = []byte(wrapped.Raw)
	}
	usage := extractGeminiUsage(raw)
	root := gjson.ParseBytes(raw)
	if root.Get("error").Exists() || !aiCanonicalFields(root, "candidates", "usageMetadata", "error") {
		return nil, usage, false, errors.New("gemini response contains an error")
	}
	if list := root.Get("candidates"); list.Exists() && !list.IsArray() {
		return nil, usage, false, errors.New("invalid Gemini candidates")
	}
	candidates := root.Get("candidates").Array()
	if len(candidates) > 1 {
		return nil, usage, false, errors.New("multiple Gemini candidates cannot be collapsed into one Responses result")
	}
	if len(candidates) == 0 {
		if usage == nil {
			return nil, usage, false, errors.New("empty Gemini response")
		}
		return raw, usage, false, nil
	}
	if !candidates[0].IsObject() || !aiCanonicalFields(candidates[0], "finishReason", "index", "content") {
		return nil, usage, false, errors.New("invalid Gemini candidate")
	}
	if index := candidates[0].Get("index"); index.Exists() && (index.Type != gjson.Number || index.Raw != "0") {
		return nil, usage, false, errors.New("unsupported Gemini candidate index")
	}
	finish := candidates[0].Get("finishReason").String()
	if value := candidates[0].Get("finishReason"); value.Exists() && (value.Type != gjson.String || value.Str == "") {
		return nil, usage, false, errors.New("invalid Gemini finishReason")
	}
	if finish != "" && finish != "STOP" && finish != "MAX_TOKENS" {
		return nil, usage, false, errors.New("gemini response did not finish successfully")
	}
	if content := candidates[0].Get("content"); content.Exists() && (!content.IsObject() || !aiCanonicalFields(content, "role", "parts") || content.Get("parts").Exists() && !content.Get("parts").IsArray()) {
		return nil, usage, false, errors.New("invalid Gemini content")
	}
	for index, part := range candidates[0].Get("content.parts").Array() {
		if !aiCanonicalFields(part, "functionCall", "thoughtSignature", "text", "thought", "inlineData", "fileData", "executableCode", "codeExecutionResult") {
			return nil, usage, false, errors.New("ambiguous Gemini response part")
		}
		if part.Get("inlineData").Exists() || part.Get("fileData").Exists() || part.Get("executableCode").Exists() || part.Get("codeExecutionResult").Exists() {
			return nil, usage, false, errors.New("gemini response contains unsupported native output")
		}
		fc := part.Get("functionCall")
		if !fc.Exists() {
			continue
		}
		args := fc.Get("args")
		if _, _, _, valid := GeminiNativeFunctionCall([]byte(part.Raw)); !valid || !args.IsObject() {
			return nil, usage, false, errors.New("invalid Gemini function call")
		}
		if nativeID := fc.Get("id").String(); nativeID != "" {
			if sequence.nativeIDs[nativeID] {
				return nil, usage, false, errors.New("duplicate Gemini function call ID")
			}
			sequence.nativeIDs[nativeID] = true
		}
		callID := GeminiResponsesToolCallPrefix + randomHex(16)
		if callbacks.Save != nil {
			state := GeminiResponsesToolState{Name: fc.Get("name").String(), Arguments: json.RawMessage(args.Raw), Part: json.RawMessage(part.Raw), Batch: sequence.batch, Index: sequence.index}
			if err := callbacks.Save(ctx, callID, state); err != nil {
				return nil, usage, false, geminiResponsesStateError()
			}
			if ctx.Err() != nil {
				return nil, usage, false, geminiResponsesStateError()
			}
		} else {
			if part.Get("thoughtSignature").String() != "" {
				return nil, usage, false, geminiResponsesStateError()
			}
			callID = "call_" + randomHex(16)
		}
		sequence.index++
		var err error
		raw, err = sjson.SetBytes(raw, fmt.Sprintf("candidates.0.content.parts.%d.functionCall.id", index), callID)
		if err != nil {
			return nil, usage, false, err
		}
	}
	return raw, usage, finish != "", nil
}
