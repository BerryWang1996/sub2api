package service

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/tidwall/gjson"
)

const aiRoutingFinalPayloadMaxBytes int64 = 32 << 20

// CheckAIRoutingFinalPayload checks the exact replayable request after protocol
// conversion, including retries. Bytes are a budgeting estimate, not a tokenizer
// result or proof of a provider's effective context capacity. It never consumes
// the original request body, fetches referenced content, or calls an upstream.
func CheckAIRoutingFinalPayload(ctx context.Context, account *Account, model string, req *http.Request) AIRoutingCapabilityCheck {
	r, enabled := AIRoutingExecutionRequirementsFromContext(ctx)
	if !enabled {
		return AIRoutingCapabilityCheck{Allowed: true}
	}
	result := AIRoutingCapabilityCheck{
		CapacityMethod: "final_payload_bytes_plus_output",
		Warnings:       []string{"protocol_overhead_unverified", "input_tokens_estimated"},
	}
	deny := func(reason string) AIRoutingCapabilityCheck {
		result.Reason = reason
		return result
	}
	if account == nil || model == "" || !account.IsModelSupported(model) || account.GetMappedModel(model) != model {
		return deny("execution_model_unavailable")
	}
	if ctx.Err() != nil {
		return deny("execution_final_payload_canceled")
	}
	if req == nil || req.URL == nil || req.GetBody == nil {
		return deny("execution_final_payload_unavailable")
	}
	copyBody, err := req.GetBody()
	if err != nil || copyBody == nil {
		return deny("execution_final_payload_unavailable")
	}
	body, readErr := io.ReadAll(io.LimitReader(copyBody, aiRoutingFinalPayloadMaxBytes+1))
	_ = copyBody.Close()
	if readErr != nil {
		return deny("execution_final_payload_unavailable")
	}
	if int64(len(body)) > aiRoutingFinalPayloadMaxBytes {
		return deny("execution_final_payload_too_large")
	}
	if ctx.Err() != nil {
		return deny("execution_final_payload_canceled")
	}
	if !utf8.Valid(body) || !gjson.ValidBytes(body) || !requestmodel.HasUniqueJSONMembers(body) {
		return deny("execution_final_payload_invalid")
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() || !aiFinalCapacityCanonical(root) {
		return deny("execution_final_payload_invalid")
	}
	path := strings.TrimSuffix(req.URL.EscapedPath(), "/")
	switch r.Operation {
	case AIRoutingResponsesInputTokens:
		if !strings.HasSuffix(path, "/responses/input_tokens") {
			return deny("execution_final_protocol_unknown")
		}
	case AIRoutingResponsesCompact:
		if !strings.HasSuffix(path, "/responses/compact") {
			return deny("execution_final_protocol_unknown")
		}
	case AIRoutingResponsesCompactV2:
		if !strings.HasSuffix(path, "/responses") || !HasCompactionTriggerInInput(body) {
			return deny("execution_final_protocol_unknown")
		}
	case AIRoutingGeminiCountTokens:
		if !strings.HasSuffix(path, ":countTokens") {
			return deny("execution_final_protocol_unknown")
		}
	default:
		if strings.HasSuffix(path, "/responses/input_tokens") || strings.HasSuffix(path, "/responses/compact") || strings.HasSuffix(path, ":countTokens") {
			return deny("execution_final_protocol_unknown")
		}
	}
	payload, protocol, urlModel, reason := aiFinalCapacitySchema(req, root)
	if reason != "" {
		return deny(reason)
	}
	if r.RequiresNativeResponses && protocol != "responses" {
		return deny("execution_native_responses_required")
	}
	if r.RequiresNativeChat && protocol != "chat" {
		return deny("execution_native_chat_required")
	}
	if r.RequiresNativeGemini && protocol != "gemini" {
		return deny("execution_native_gemini_required")
	}
	choices := r.OutputChoices
	if choices == 0 {
		choices = 1
	}
	if choices < 1 || choices > 64 {
		return deny("execution_output_choices_invalid")
	}
	if protocol == "chat" {
		actual := int64(1)
		if n := payload.Get("n"); n.Exists() && n.Type != gjson.Null {
			var err error
			actual, err = strconv.ParseInt(n.Raw, 10, 64)
			if n.Type != gjson.Number || err != nil || actual < 1 || actual > 64 {
				return deny("execution_output_choices_invalid")
			}
		}
		if actual != int64(choices) {
			return deny("execution_output_choices_changed")
		}
	} else if protocol == "gemini" {
		generation, ok := aiGeminiField(payload, "generationConfig", "generation_config")
		if !ok || generation.Exists() && !generation.IsObject() {
			return deny("execution_final_output_invalid")
		}
		actual := int64(1)
		if generation.Exists() {
			count, ok := aiGeminiField(generation, "candidateCount", "candidate_count")
			if !ok {
				return deny("execution_output_choices_invalid")
			}
			if count.Exists() {
				var err error
				actual, err = strconv.ParseInt(count.Raw, 10, 64)
				if count.Type != gjson.Number || err != nil || actual < 1 || actual > 64 {
					return deny("execution_output_choices_invalid")
				}
			}
		}
		if actual != int64(choices) {
			return deny("execution_output_choices_changed")
		}
	} else if choices > 1 {
		return deny("execution_native_chat_required")
	}
	if !aiFinalCapacityModelMatches(account, model, root, payload, protocol, urlModel) {
		return deny("execution_final_model_mismatch")
	}
	output, present, valid := aiFinalCapacityOutput(payload, protocol)
	if !valid {
		return deny("execution_final_output_invalid")
	}
	auxiliary := r.Operation == AIRoutingResponsesInputTokens || r.Operation == AIRoutingResponsesCompact || r.Operation == AIRoutingResponsesCompactV2 || r.Operation == AIRoutingGeminiCountTokens
	if r.Warmup && (protocol != APIProtocolResponses || root.Get("generate").Type != gjson.False) {
		return deny("execution_warmup_changed")
	}
	if auxiliary || r.Warmup {
		output, present = 0, true
	}
	result.FinalPayloadChecked = true
	result.InputBytes = int64(len(body))
	warnings := aiFinalCapacityWarnings(root)
	metadata, metadataKnown := aiFinalCapacityMetadata(account, model)
	if metadataKnown {
		// Snapshots may mix upstream, registry and retained older fields. Their
		// timestamp is not per-field provenance or a capacity probe.
		warnings = append(warnings, "metadata_limits_not_request_verified")
		if metadata.MaxContextWindow > 0 && metadata.ContextWindow == metadata.MaxContextWindow {
			warnings = append(warnings, "metadata_context_window_origin_unknown")
		}
	}
	if !present {
		if !metadataKnown || metadata.MaxOutputTokens <= 0 {
			result.Warnings = append(result.Warnings, warnings...)
			result.Warnings = append(result.Warnings, "output_reserve_unknown")
			return deny("execution_output_reserve_unknown")
		}
		// A candidate declaration restricts the account; it does not instruct
		// the upstream. Reserve the complete catalog maximum, not the removed
		// incoming max_tokens or the candidate's desired ceiling.
		output = metadata.MaxOutputTokens
		warnings = append(warnings, "output_reserve_from_metadata_maximum")
	}
	r.InputBytes = int64(len(body))
	r.MaxOutputTokens = output
	result = CheckAIRoutingExecutionAccount(WithAIRoutingExecutionRequirements(ctx, r), account, model)
	result.CapacityMethod = "final_payload_bytes_plus_output"
	if auxiliary {
		result.CapacityMethod = "final_auxiliary_input_bytes"
	}
	if r.Warmup {
		result.CapacityMethod = "final_warmup_input_bytes"
	}
	result.FinalPayloadChecked = true
	result.InputBytes = int64(len(body))
	result.OutputReserveTokens = output
	for _, warning := range warnings {
		if !aiFinalCapacityContains(result.Warnings, warning) {
			result.Warnings = append(result.Warnings, warning)
		}
	}
	return result
}

// Endpoint and schema must agree. In particular, arbitrary JSON carrying a
// max_tokens field must not be mistaken for a supported generation request.
func aiFinalCapacitySchema(req *http.Request, root gjson.Result) (gjson.Result, string, string, string) {
	if req.Method != http.MethodPost {
		return root, "", "", "execution_final_protocol_unknown"
	}
	path := strings.TrimSuffix(req.URL.EscapedPath(), "/")
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		if root.Get("messages").IsArray() && !root.Get("input").Exists() && !root.Get("contents").Exists() {
			return root, "chat", "", ""
		}
	case strings.HasSuffix(path, "/responses"), strings.HasSuffix(path, "/responses/compact"), strings.HasSuffix(path, "/responses/input_tokens"):
		input := root.Get("input")
		warmup := root.Get("type").Str == "response.create" && root.Get("generate").Type == gjson.False && !input.Exists()
		if (input.IsArray() || input.Type == gjson.String || root.Get("previous_response_id").Type == gjson.String || warmup) && !root.Get("messages").Exists() && !root.Get("contents").Exists() {
			return root, "responses", "", ""
		}
	case strings.HasSuffix(path, "/messages"):
		if root.Get("messages").IsArray() && !root.Get("input").Exists() && !root.Get("contents").Exists() {
			return root, "messages", "", ""
		}
	case strings.HasSuffix(path, "/invoke"), strings.HasSuffix(path, "/invoke-with-response-stream"):
		if i := strings.LastIndex(path, "/model/"); i >= 0 && root.Get("messages").IsArray() {
			part := path[i+len("/model/") : strings.LastIndex(path, "/")]
			if value, err := url.PathUnescape(part); err == nil && value != "" {
				return root, "bedrock", value, ""
			}
		}
	case strings.HasSuffix(path, ":rawPredict"), strings.HasSuffix(path, ":streamRawPredict"):
		if strings.Contains(path, "/publishers/anthropic/models/") && root.Get("messages").IsArray() {
			if value := aiFinalCapacityURLModel(path); value != "" {
				return root, "vertex_anthropic", value, ""
			}
		}
	case strings.HasSuffix(path, ":generateContent"), strings.HasSuffix(path, ":streamGenerateContent"), strings.HasSuffix(path, ":countTokens"):
		payload := root
		urlModel := aiFinalCapacityURLModel(path)
		if strings.Contains(path, "/models/") && urlModel == "" {
			return root, "", "", "execution_final_model_mismatch"
		}
		if wrapped := root.Get("request"); wrapped.Exists() {
			if !wrapped.IsObject() || root.Get("contents").Exists() || root.Get("generationConfig").Exists() || !aiFinalCapacityCanonical(wrapped) {
				return root, "", "", "execution_final_payload_invalid"
			}
			payload = wrapped
		} else if urlModel == "" {
			return root, "", "", "execution_final_protocol_unknown"
		}
		if nested, ok := aiGeminiField(payload, "generateContentRequest", "generate_content_request"); !ok {
			return root, "", "", "execution_final_payload_invalid"
		} else if nested.Exists() {
			if !strings.HasSuffix(path, ":countTokens") || !nested.IsObject() || payload.Get("contents").Exists() {
				return root, "", "", "execution_final_payload_invalid"
			}
			payload = nested
		}
		if (payload.Get("contents").IsArray() || payload.Get("cachedContent").Type == gjson.String || payload.Get("cached_content").Type == gjson.String) && !payload.Get("messages").Exists() && !payload.Get("input").Exists() {
			return payload, "gemini", urlModel, ""
		}
	}
	return root, "", "", "execution_final_protocol_unknown"
}

func aiFinalCapacityURLModel(path string) string {
	i := strings.LastIndex(path, "/models/")
	j := strings.LastIndex(path, ":")
	if i < 0 || j <= i+len("/models/") {
		return ""
	}
	value, err := url.PathUnescape(path[i+len("/models/") : j])
	if err != nil || strings.Contains(value, "/") {
		return ""
	}
	return value
}

func aiFinalCapacityModelMatches(account *Account, model string, root, payload gjson.Result, protocol, urlModel string) bool {
	expectedURLModel := model
	expectedBodyModel := model
	switch protocol {
	case "responses", "chat":
		expectedBodyModel = normalizeOpenAIModelForUpstream(account, model)
	case "bedrock":
		resolved, ok := ResolveBedrockModelID(account, model)
		if !ok {
			return false
		}
		expectedURLModel = resolved
	case "vertex_anthropic":
		expectedURLModel = normalizeVertexAnthropicModelID(model)
	}
	if urlModel != "" && urlModel != expectedURLModel {
		return false
	}
	seen := urlModel != ""
	for _, object := range []gjson.Result{root, payload} {
		if value := object.Get("model"); value.Exists() {
			valueModel := value.Str
			if protocol == "gemini" {
				valueModel = strings.TrimPrefix(valueModel, "models/")
			}
			if value.Type != gjson.String || (valueModel != model && valueModel != expectedBodyModel) {
				return false
			}
			seen = true
		}
	}
	return seen
}

func aiFinalCapacityCanonical(object gjson.Result) bool {
	return aiCanonicalFields(object, "model", "messages", "input", "contents", "request", "generationConfig", "generation_config", "generateContentRequest", "generate_content_request", "max_tokens", "max_output_tokens", "max_completion_tokens", "n")
}

func aiFinalCapacityOutput(payload gjson.Result, protocol string) (int64, bool, bool) {
	field := "max_tokens"
	switch protocol {
	case "responses":
		field = "max_output_tokens"
	case "chat":
		field = "max_completion_tokens"
		if !payload.Get(field).Exists() {
			field = "max_tokens"
		}
	case "gemini":
		for _, foreign := range []string{"max_tokens", "max_output_tokens", "max_completion_tokens"} {
			if payload.Get(foreign).Exists() {
				return 0, false, false
			}
		}
		config, ok := aiGeminiField(payload, "generationConfig", "generation_config")
		if !ok || config.Exists() && (!config.IsObject() || !aiCanonicalFields(config, "maxOutputTokens", "max_output_tokens", "candidateCount", "candidate_count")) {
			return 0, false, false
		}
		if !config.Exists() {
			return 0, false, true
		}
		value, ok := aiGeminiField(config, "maxOutputTokens", "max_output_tokens")
		if !ok {
			return 0, false, false
		}
		if !value.Exists() {
			return 0, false, true
		}
		n, err := strconv.ParseInt(value.Raw, 10, 64)
		return n, true, value.Type == gjson.Number && err == nil && n > 0
	}
	for _, other := range []string{"max_tokens", "max_output_tokens", "max_completion_tokens"} {
		if other != field && payload.Get(other).Exists() {
			return 0, false, false
		}
	}
	value := payload.Get(field)
	if !value.Exists() {
		return 0, false, true
	}
	if value.Type != gjson.Number {
		return 0, true, false
	}
	n, err := strconv.ParseInt(value.Raw, 10, 64)
	return n, true, err == nil && n > 0
}

func aiFinalCapacityMetadata(account *Account, model string) (UpstreamModelMetadata, bool) {
	snapshot := account.GetUpstreamModelMetadataSnapshot()
	if snapshot == nil || strings.TrimSpace(snapshot.Source) == "" {
		return UpstreamModelMetadata{}, false
	}
	if _, err := time.Parse(time.RFC3339, snapshot.SyncedAt); err != nil {
		return UpstreamModelMetadata{}, false
	}
	metadata, ok := snapshot.Models[model]
	return metadata, ok && (metadata.ID == "" || metadata.ID == model)
}

// Warnings describe content whose input-token contribution is not established by
// the serialized byte count. They never cause a verified-capacity claim. Inline
// cache_control/prompt_cache_key and ordinary tool call IDs are not remote state.
func aiFinalCapacityWarnings(root gjson.Result) []string {
	warnings := make([]string, 0, 3)
	add := func(warning string) {
		if !aiFinalCapacityContains(warnings, warning) {
			warnings = append(warnings, warning)
		}
	}
	var visit func(gjson.Result, int)
	visit = func(value gjson.Result, depth int) {
		if depth > 64 {
			add("nested_content_token_usage_unverified")
			return
		}
		if value.IsObject() {
			switch value.Get("type").Str {
			case "image", "input_image", "image_url", "document", "input_file", "file", "input_audio", "audio", "video":
				add("attachment_token_usage_unverified")
			case "redacted_thinking", "reasoning":
				add("opaque_state_token_usage_unverified")
			case "item_reference":
				add("referenced_context_token_usage_unverified")
			}
			value.ForEach(func(key, child gjson.Result) bool {
				switch key.Str {
				case "parameters", "input_schema", "arguments":
					return true // user tool schemas/arguments are ordinary inline data
				case "previous_response_id", "cachedContent", "conversation":
					if child.Type != gjson.Null && child.Raw != `""` {
						add("referenced_context_token_usage_unverified")
					}
				case "encrypted_content", "thoughtSignature", "signature":
					if child.Type != gjson.Null && child.Raw != `""` {
						add("opaque_state_token_usage_unverified")
					}
				case "inlineData", "fileData", "file_id", "file_data", "image_url":
					add("attachment_token_usage_unverified")
				}
				visit(child, depth+1)
				return true
			})
		} else if value.IsArray() {
			value.ForEach(func(_, child gjson.Result) bool { visit(child, depth+1); return true })
		}
	}
	visit(root, 0)
	return warnings
}

func aiFinalCapacityContains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
