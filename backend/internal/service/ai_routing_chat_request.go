package service

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/tidwall/gjson"
)

type aiChatParsed struct {
	Request AIRoutingRequest
	Items   []aiResponsesHistoryItem // shared text-span representation, not a protocol conversion
}

// NewAIRoutingChatRequest parses the native Chat Completions envelope. Only
// visible user/assistant text can enter decision evidence; HistoryBody remains
// an independent copy of the complete execution request.
func NewAIRoutingChatRequest(body []byte, mode string) (AIRoutingRequest, error) {
	parsed, err := parseAIRoutingChat(body, mode)
	return parsed.Request, err
}

func parseAIRoutingChat(body []byte, mode string) (aiChatParsed, error) {
	var parsed aiChatParsed
	r := &parsed.Request
	r.Endpoint, r.InputBytes, r.OutputLimitOmitted = CompositeRouteEndpointChatCompletions, len(body), true
	r.InputModalities = []string{"text"}
	invalid := func() (aiChatParsed, error) {
		return parsed, errors.New("invalid or unsupported native Chat Completions request")
	}
	if !utf8.Valid(body) || !gjson.ValidBytes(body) || !requestmodel.HasUniqueJSONMembers(body) {
		return invalid()
	}
	root := gjson.ParseBytes(body)
	if !aiCanonicalFields(root, "model", "messages", "tools", "tool_choice", "max_tokens", "max_completion_tokens", "stream", "n", "functions", "function_call", "modalities", "audio", "input", "instructions", "max_output_tokens", "previous_response_id", "conversation", "background", "prompt", "commands") {
		return invalid()
	}
	model := root.Get("model")
	if model.Type != gjson.String || strings.TrimSpace(model.Str) == "" || len(model.Str) > 256 {
		return invalid()
	}
	r.PublicModel = model.Str
	for _, field := range []string{"input", "instructions", "max_output_tokens", "previous_response_id", "conversation", "background", "prompt", "commands"} {
		if root.Get(field).Exists() {
			return invalid()
		}
	}
	// Legacy functions lack a stable call ID and cannot authorize a continuation.
	for _, field := range []string{"functions", "function_call", "audio"} {
		if value := root.Get(field); value.Exists() && value.Type != gjson.Null {
			return invalid()
		}
	}
	if modalities := root.Get("modalities"); modalities.Exists() && modalities.Type != gjson.Null {
		if !modalities.IsArray() || len(modalities.Array()) != 1 || modalities.Array()[0].Str != "text" {
			return invalid()
		}
	}
	if stream := root.Get("stream"); stream.Exists() && stream.Type != gjson.True && stream.Type != gjson.False {
		return invalid()
	}
	if n := root.Get("n"); n.Exists() && n.Type != gjson.Null {
		value, ok := aiChatPositiveInt(n)
		if !ok || value > 64 {
			return invalid()
		}
		r.OutputChoices = value
		r.RequiresNativeChat = value > 1
	}
	// Only the proven text/image/function-call subset may cross protocols.
	// A model's capability metadata cannot prove that an adapter retains a
	// provider-specific option, so preserve such requests on the native wire.
	root.ForEach(func(field, value gjson.Result) bool {
		if value.Type == gjson.Null {
			return true
		}
		switch field.Str {
		case "model", "messages", "tools", "tool_choice", "max_tokens", "max_completion_tokens", "stream", "n", "temperature", "top_p", "modalities":
		case "store":
			r.RequiresNativeChat = r.RequiresNativeChat || value.Type != gjson.False
		case "stream_options":
			if !value.IsObject() {
				r.RequiresNativeChat = true
			} else {
				value.ForEach(func(name, _ gjson.Result) bool {
					r.RequiresNativeChat = r.RequiresNativeChat || name.Str != "include_usage"
					return true
				})
			}
		default:
			r.RequiresNativeChat = true
		}
		return true
	})
	output, legacy := root.Get("max_completion_tokens"), root.Get("max_tokens")
	if output.Exists() && legacy.Exists() {
		return invalid()
	}
	if !output.Exists() {
		output = legacy
	}
	if output.Exists() && output.Type != gjson.Null {
		value, ok := aiChatPositiveInt(output)
		if !ok {
			return invalid()
		}
		r.MaxOutput, r.OutputLimitOmitted = value, false
	}
	if tools := root.Get("tools"); tools.Exists() && tools.Type != gjson.Null {
		if !tools.IsArray() {
			return invalid()
		}
		for _, tool := range tools.Array() {
			if !aiCanonicalFields(tool, "type", "function", "custom") || tool.Get("type").Str != "function" || !aiChatFunction(tool.Get("function"), false) {
				return invalid()
			}
			r.RequiresNativeChat = r.RequiresNativeChat || aiChatHasUnknownFields(tool, "type", "function")
			function := tool.Get("function")
			function.ForEach(func(field, value gjson.Result) bool {
				switch field.Str {
				case "name", "description", "parameters":
				case "strict":
					r.RequiresNativeChat = r.RequiresNativeChat || value.Type != gjson.False && value.Type != gjson.Null
				default:
					r.RequiresNativeChat = true
				}
				return true
			})
			r.UsesTools = true
		}
	}
	if choice := root.Get("tool_choice"); choice.Exists() && choice.Type != gjson.Null {
		if choice.Type == gjson.String {
			if choice.Str != "auto" && choice.Str != "none" && choice.Str != "required" {
				return invalid()
			}
		} else if !aiCanonicalFields(choice, "type", "function", "custom") || choice.Get("type").Str != "function" || !aiChatFunction(choice.Get("function"), false) {
			return invalid()
		}
		if choice.IsObject() {
			r.RequiresNativeChat = r.RequiresNativeChat || aiChatHasUnknownFields(choice, "type", "function") || aiChatHasUnknownFields(choice.Get("function"), "name")
		}
		if choice.Type != gjson.String || choice.Str == "required" {
			r.UsesTools = true
		}
	}
	messages := root.Get("messages")
	if !messages.IsArray() || len(messages.Array()) == 0 {
		return invalid()
	}
	definitions, outputs := make(map[string]string), make(map[string]string)
	pending := make([]string, 0)
	privateCalls := make([]string, 0)
	for index, message := range messages.Array() {
		if !aiCanonicalFields(message, "role", "content", "tool_calls", "tool_call_id", "function_call", "name", "refusal", "audio", "reasoning_content", "reasoning", "reasoning_details", "reasoning_signature", "prefix", "partial", "status") {
			return invalid()
		}
		role := message.Get("role")
		if role.Type != gjson.String || role.Str != "system" && role.Str != "developer" && role.Str != "user" && role.Str != "assistant" && role.Str != "tool" {
			return invalid()
		}
		for _, field := range []string{"function_call", "audio", "reasoning_details", "reasoning_signature"} {
			if value := message.Get(field); value.Exists() && value.Type != gjson.Null {
				return invalid()
			}
		}
		// Vendor prefill/incomplete extensions cannot masquerade as the
		// completed assistant turn that closes a strict tool continuation.
		for _, field := range []string{"prefix", "partial", "status"} {
			if message.Get(field).Exists() {
				return invalid()
			}
		}
		if name := message.Get("name"); name.Exists() && name.Type != gjson.String {
			return invalid()
		}
		entry := aiResponsesHistoryItem{Index: index, Role: role.Str}
		for _, field := range []string{"reasoning_content", "reasoning"} {
			if value := message.Get(field); value.Exists() && value.Type != gjson.Null {
				if role.Str != "assistant" || value.Type != gjson.String {
					return invalid()
				}
				entry.Excluded++
			}
		}
		calls := message.Get("tool_calls")
		hasCalls := calls.Exists() && calls.Type != gjson.Null
		if hasCalls {
			if role.Str != "assistant" || !calls.IsArray() || len(calls.Array()) == 0 || len(calls.Array()) > aiResponsesOriginLimit {
				return invalid()
			}
			for _, call := range calls.Array() {
				id := call.Get("id")
				if !aiCanonicalFields(call, "id", "type", "function", "custom") || id.Type != gjson.String || !aiResponsesValidID(id.Str) || call.Get("type").Str != "function" || !aiChatFunction(call.Get("function"), true) {
					return invalid()
				}
				r.RequiresNativeChat = r.RequiresNativeChat || aiChatHasUnknownFields(call, "id", "type", "function") || aiChatHasUnknownFields(call.Get("function"), "name", "arguments")
				definition := call.Get("function.name").Str + "\x00" + call.Get("function.arguments").Str
				if old, exists := definitions[id.Str]; exists && old != definition {
					return invalid()
				}
				definitions[id.Str] = definition
				if strings.HasPrefix(id.Str, GeminiResponsesToolCallPrefix) && !aiFinalCapacityContains(privateCalls, id.Str) {
					if len(privateCalls) >= aiResponsesOriginLimit {
						return invalid()
					}
					privateCalls = append(privateCalls, id.Str)
				}
				entry.Excluded++
			}
			r.UsesTools = true
		}
		content := message.Get("content")
		if content.Type == gjson.String {
			if role.Str == "user" || role.Str == "assistant" {
				entry.Parts = append(entry.Parts, aiResponsesHistoryPart{Text: content.Str})
			} else {
				entry.Excluded++
			}
		} else if content.IsArray() && len(content.Array()) > 0 {
			for block, part := range content.Array() {
				text, visible, ok := aiChatContentPart(part, role.Str, r)
				if !ok {
					return invalid()
				}
				if visible && (role.Str == "user" || role.Str == "assistant") {
					entry.Parts = append(entry.Parts, aiResponsesHistoryPart{Block: block, Text: text})
				} else {
					entry.Excluded++
				}
			}
		} else if role.Str != "assistant" || content.Exists() && content.Type != gjson.Null {
			return invalid()
		}
		refusal := message.Get("refusal")
		if refusal.Exists() && refusal.Type != gjson.Null {
			if role.Str != "assistant" || refusal.Type != gjson.String {
				return invalid()
			}
			entry.Parts = append(entry.Parts, aiResponsesHistoryPart{Block: len(entry.Parts), Text: refusal.Str})
		}
		if role.Str == "assistant" {
			if !content.Exists() || content.Type == gjson.Null {
				if !hasCalls && refusal.Type != gjson.String && entry.Excluded == 0 {
					return invalid()
				}
			}
			if hasCalls || aiResponsesHasVisibleText(entry.Parts) {
				pending = pending[:0] // a later assistant action consumed the prior tool results
			}
		}
		callID := message.Get("tool_call_id")
		if role.Str == "tool" {
			if callID.Type != gjson.String || !aiResponsesValidID(callID.Str) {
				return invalid()
			}
			if old, exists := outputs[callID.Str]; exists && old != content.Raw {
				return invalid()
			}
			outputs[callID.Str] = content.Raw
			if !aiFinalCapacityContains(pending, callID.Str) {
				if len(pending) >= aiResponsesOriginLimit {
					return invalid()
				}
				pending = append(pending, callID.Str)
			}
			entry.Role, r.UsesTools = "user", true
		} else if callID.Exists() {
			return invalid()
		}
		parsed.Items = append(parsed.Items, entry)
	}
	for _, id := range privateCalls {
		if !aiFinalCapacityContains(pending, id) {
			pending = append(pending, id)
		}
	}
	if len(pending) > aiResponsesOriginLimit {
		return invalid()
	}
	for _, id := range pending {
		r.OriginRefs = append(r.OriginRefs, AIRoutingOriginRef{Kind: "tool_call", ID: id})
	}
	r.ToolResults, r.RequiresOrigin = append([]string(nil), pending...), len(pending) > 0
	if mode == "bounded_history" {
		r.HistoryBody = append([]byte(nil), body...)
	}
	var task strings.Builder
	lastRole, turns := "", 0
	for _, item := range parsed.Items {
		if item.Role != "user" && item.Role != "assistant" {
			lastRole = ""
			continue
		}
		if item.Role != lastRole {
			turns++
			if item.Role == "user" {
				task.Reset()
			}
		}
		lastRole = item.Role
		if item.Role == "user" && mode == "task_excerpt" {
			for _, part := range item.Parts {
				appendAITaskText(&task, part.Text)
			}
		}
	}
	state := struct {
		Turns         int    `json:"turns"`
		Bytes         int    `json:"request_bytes"`
		Tools         bool   `json:"tools"`
		Images        bool   `json:"attachments"`
		Output        int    `json:"max_output"`
		Origin        bool   `json:"origin_required"`
		OutputOmitted bool   `json:"output_limit_omitted"`
		Excerpt       string `json:"user_task,omitempty"`
	}{turns, len(body), r.UsesTools, r.HasImages, r.MaxOutput, r.RequiresOrigin, r.OutputLimitOmitted, task.String()}
	encoded, _ := json.Marshal(state)
	r.Task = string(encoded)
	return parsed, nil
}

func aiChatPositiveInt(value gjson.Result) (int, bool) {
	n, err := strconv.ParseInt(value.Raw, 10, 64)
	return int(n), value.Type == gjson.Number && err == nil && n > 0 && uint64(n) <= uint64(^uint(0)>>1)
}

func aiChatFunction(function gjson.Result, call bool) bool {
	if !aiCanonicalFields(function, "name", "arguments", "description", "parameters", "strict") || function.Get("name").Type != gjson.String || strings.TrimSpace(function.Get("name").Str) == "" {
		return false
	}
	return !call || function.Get("arguments").Type == gjson.String
}

func aiChatHasUnknownFields(value gjson.Result, allowed ...string) bool {
	unknown := false
	value.ForEach(func(key, _ gjson.Result) bool {
		unknown = !aiFinalCapacityContains(allowed, key.Str)
		return !unknown
	})
	return unknown
}

func aiChatContentPart(part gjson.Result, role string, r *AIRoutingRequest) (string, bool, bool) {
	if !aiCanonicalFields(part, "type", "text", "refusal", "image_url", "input_audio", "file") || part.Get("type").Type != gjson.String {
		return "", false, false
	}
	switch part.Get("type").Str {
	case "text":
		return part.Get("text").Str, true, part.Get("text").Type == gjson.String
	case "refusal":
		return part.Get("refusal").Str, true, role == "assistant" && part.Get("refusal").Type == gjson.String
	case "image_url":
		image := part.Get("image_url")
		if role != "user" || !aiCanonicalFields(image, "url", "detail") || image.Get("url").Type != gjson.String || image.Get("url").Str == "" {
			return "", false, false
		}
		r.addInputModality("image")
	case "input_audio":
		audio := part.Get("input_audio")
		if role != "user" || !aiCanonicalFields(audio, "data", "format") || audio.Get("data").Type != gjson.String || audio.Get("data").Str == "" || audio.Get("format").Str != "wav" && audio.Get("format").Str != "mp3" {
			return "", false, false
		}
		r.addInputModality("audio")
		r.RequiresNativeChat = true
	case "file":
		file := part.Get("file")
		// Account-owned uploaded file IDs cannot inherit a tool-call grant.
		if role != "user" || !aiCanonicalFields(file, "file_id", "file_data", "filename") || file.Get("file_id").Exists() || file.Get("file_data").Type != gjson.String || file.Get("file_data").Str == "" {
			return "", false, false
		}
		r.addInputModality("document")
		r.RequiresNativeChat = true
	default:
		return "", false, false
	}
	r.HasImages = true
	return "", false, true
}
