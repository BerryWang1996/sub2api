package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/tidwall/gjson"
)

// GeminiRequestToResponses converts only the explicitly supported, stateless
// public subset. The caller first restores authenticated native tool state.
// Provider-owned signatures, caches, files and native generation controls never
// disappear through this conversion.
func GeminiRequestToResponses(body []byte, model, action string, stream bool) ([]byte, error) {
	parsed, err := parseAIRoutingGemini(body, "", action, "metadata")
	if err != nil {
		return nil, err
	}
	if action == "countTokens" || parsed.Request.RequiresNativeGemini || len(parsed.References.FileURIs) > 0 || parsed.References.CachedContent != "" {
		return nil, errors.New("request requires native Gemini execution")
	}
	root := gjson.ParseBytes(body)
	out := map[string]any{"model": model, "stream": stream, "store": false}
	var input []any
	for _, content := range root.Get("contents").Array() {
		role := content.Get("role").String()
		if role == "model" {
			role = "assistant"
		}
		if role == "" {
			role = "user"
		}
		var visible []any
		flush := func() {
			if len(visible) > 0 {
				input = append(input, map[string]any{"type": "message", "role": role, "content": visible})
				visible = nil
			}
		}
		for _, part := range content.Get("parts").Array() {
			if text := part.Get("text"); text.Exists() {
				kind := "input_text"
				if role == "assistant" {
					kind = "output_text"
				}
				visible = append(visible, map[string]any{"type": kind, "text": text.Str})
				continue
			}
			if inline, _ := aiGeminiField(part, "inlineData", "inline_data"); inline.Exists() {
				mime, _ := aiGeminiField(inline, "mimeType", "mime_type")
				if role != "user" || !strings.HasPrefix(mime.Str, "image/") {
					return nil, errors.New("media requires native Gemini execution")
				}
				visible = append(visible, map[string]any{"type": "input_image", "image_url": "data:" + mime.Str + ";base64," + inline.Get("data").String()})
				continue
			}
			flush()
			if fc, _ := aiGeminiField(part, "functionCall", "function_call"); fc.Exists() {
				name, args, id, valid := GeminiNativeFunctionCall([]byte(part.Raw))
				if !valid || id == "" {
					return nil, errors.New("tool call requires authenticated ID")
				}
				input = append(input, map[string]any{"type": "function_call", "call_id": id, "name": name, "arguments": string(args)})
				continue
			}
			if result, _ := aiGeminiField(part, "functionResponse", "function_response"); result.Exists() {
				if result.Get("id").String() == "" || result.Get("parts").Exists() {
					return nil, errors.New("tool result requires a supported authenticated ID")
				}
				input = append(input, map[string]any{"type": "function_call_output", "call_id": result.Get("id").String(), "output": result.Get("response").Raw})
				continue
			}
			return nil, errors.New("unsupported Gemini content part")
		}
		flush()
	}
	out["input"] = input
	if system, _ := aiGeminiField(root, "systemInstruction", "system_instruction"); system.Exists() {
		var texts []string
		for _, part := range system.Get("parts").Array() {
			texts = append(texts, part.Get("text").Str)
		}
		out["instructions"] = strings.Join(texts, "\n")
	}
	if generation, _ := aiGeminiField(root, "generationConfig", "generation_config"); generation.Exists() {
		for _, field := range [][3]string{{"maxOutputTokens", "max_output_tokens", "max_output_tokens"}, {"temperature", "", "temperature"}, {"topP", "top_p", "top_p"}} {
			if value, _ := aiGeminiField(generation, field[0], field[1]); value.Exists() {
				out[field[2]] = json.RawMessage(value.Raw)
			}
		}
		// Responses has no stop-sequence field. Refusing this subset is required
		// rather than silently changing the caller's termination semantics.
		if stops, _ := aiGeminiField(generation, "stopSequences", "stop_sequences"); len(stops.Array()) > 0 {
			return nil, errors.New("stop sequences require native Gemini execution")
		}
	}
	var tools []any
	for _, tool := range root.Get("tools").Array() {
		functions, _ := aiGeminiField(tool, "functionDeclarations", "function_declarations")
		for _, fn := range functions.Array() {
			schema, _ := aiGeminiField(fn, "parametersJsonSchema", "parameters_json_schema")
			var converted any
			if schema.Exists() {
				converted = json.RawMessage(schema.Raw)
			} else if schema = fn.Get("parameters"); schema.Exists() {
				converted, err = geminiBridgeSchema(schema)
				if err != nil {
					return nil, err
				}
			} else {
				converted = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, map[string]any{"type": "function", "name": fn.Get("name").Str, "description": fn.Get("description").String(), "parameters": converted})
		}
	}
	if len(tools) > 0 {
		out["tools"] = tools
	}
	if config, _ := aiGeminiField(root, "toolConfig", "tool_config"); config.Exists() {
		calling, _ := aiGeminiField(config, "functionCallingConfig", "function_calling_config")
		names, _ := aiGeminiField(calling, "allowedFunctionNames", "allowed_function_names")
		mode := calling.Get("mode").String()
		switch mode {
		case "", "AUTO":
			out["tool_choice"] = "auto"
		case "NONE":
			out["tool_choice"] = "none"
		case "ANY":
			out["tool_choice"] = "required"
		default:
			return nil, errors.New("tool calling mode requires native Gemini")
		}
		if len(names.Array()) > 0 {
			if mode != "ANY" || len(names.Array()) != 1 {
				return nil, errors.New("allowed-function subset requires native Gemini")
			}
			out["tool_choice"] = map[string]any{"type": "function", "name": names.Array()[0].Str}
		}
	}
	return json.Marshal(out)
}

// Convert the documented Gemini Schema subset without changing property names,
// descriptions or arbitrary JSON examples. Unsupported constraints stay native.
func geminiBridgeSchema(schema gjson.Result) (any, error) {
	if !schema.IsObject() {
		return nil, errors.New("invalid Gemini tool schema")
	}
	out := map[string]any{}
	var failure error
	schema.ForEach(func(key, value gjson.Result) bool {
		switch key.Str {
		case "type":
			typ := strings.ToLower(value.String())
			switch typ {
			case "object", "array", "string", "number", "integer", "boolean", "null":
				out["type"] = typ
			default:
				failure = errors.New("unsupported Gemini schema type")
			}
		case "properties":
			if !value.IsObject() {
				failure = errors.New("invalid Gemini schema properties")
				break
			}
			properties := map[string]any{}
			value.ForEach(func(name, child gjson.Result) bool {
				properties[name.Str], failure = geminiBridgeSchema(child)
				return failure == nil
			})
			out[key.Str] = properties
		case "items":
			out[key.Str], failure = geminiBridgeSchema(value)
		case "anyOf", "any_of":
			if !value.IsArray() {
				failure = errors.New("invalid Gemini union schema")
				break
			}
			var items []any
			for _, child := range value.Array() {
				var item any
				item, failure = geminiBridgeSchema(child)
				if failure != nil {
					break
				}
				items = append(items, item)
			}
			out["anyOf"] = items
		case "nullable":
			if value.Type != gjson.False {
				failure = errors.New("nullable Gemini schema requires native execution")
			}
		case "description", "title", "format", "enum", "required", "minimum", "maximum", "minItems", "maxItems", "minLength", "maxLength", "pattern", "default", "example":
			out[key.Str] = json.RawMessage(value.Raw)
		case "propertyOrdering": // JSON objects are unordered; not a validation constraint.
		default:
			failure = errors.New("unsupported Gemini schema constraint")
		}
		return failure == nil
	})
	return out, failure
}

// GeminiResponsesOutput converts the Responses public output subset. It emits
// deltas as they arrive and complete function calls only after their arguments
// finish; no completion is fabricated after EOF or an upstream error.
type GeminiResponsesOutput struct {
	Model    string
	Complete bool
	text     map[string]string
	items    map[int]json.RawMessage
	bytes    int
}

func (s *GeminiResponsesOutput) Convert(raw []byte, stream bool) ([]byte, error) {
	if !gjson.ValidBytes(raw) || !requestmodel.HasUniqueJSONMembers(raw) {
		return nil, errors.New("invalid Responses output")
	}
	root := gjson.ParseBytes(raw)
	if !aiCanonicalFields(root, "type", "response", "error", "object", "output", "status", "usage", "item", "delta", "output_index", "content_index") {
		return nil, errors.New("ambiguous Responses output fields")
	}
	if root.Get("error").Exists() || root.Get("type").String() == "error" || root.Get("type").String() == "response.failed" {
		return nil, errors.New("upstream Responses execution failed")
	}
	if s.text == nil {
		s.text = map[string]string{}
		s.items = map[int]json.RawMessage{}
	}
	if len(raw) > 4<<20 || s.Complete {
		return nil, errors.New("gemini bridge output exceeds limit or follows completion")
	}
	var parts []any
	finish := ""
	response := root
	if stream {
		kind := root.Get("type").String()
		switch kind {
		case "response.output_text.delta":
			if s.Complete {
				return nil, errors.New("output after completion")
			}
			id := fmt.Sprintf("%d:%d", root.Get("output_index").Int(), root.Get("content_index").Int())
			if root.Get("delta").Type != gjson.String {
				return nil, errors.New("invalid text delta")
			}
			if err := s.reserve(len(root.Get("delta").Str)); err != nil {
				return nil, err
			}
			s.text[id] += root.Get("delta").Str
			parts = append(parts, map[string]any{"text": root.Get("delta").Str})
		case "response.output_item.done":
			item := root.Get("item")
			converted, err := s.consumeItem(item, int(root.Get("output_index").Int()))
			if err != nil {
				return nil, err
			}
			parts = append(parts, converted...)
		case "response.completed", "response.incomplete":
			if s.Complete {
				return nil, errors.New("duplicate Responses completion")
			}
			response = root.Get("response")
			if !response.IsObject() {
				return nil, errors.New("missing terminal response")
			}
			if !response.Get("output").IsArray() {
				return nil, errors.New("missing terminal output")
			}
			for index, item := range response.Get("output").Array() {
				converted, err := s.consumeItem(item, index)
				if err != nil {
					return nil, err
				}
				parts = append(parts, converted...)
			}
			for index := range s.items {
				if index >= len(response.Get("output").Array()) {
					return nil, errors.New("terminal output omitted an emitted item")
				}
			}
			for key, prior := range s.text {
				var index, content int
				if _, err := fmt.Sscanf(key, "%d:%d", &index, &content); err != nil {
					return nil, err
				}
				value := response.Get(fmt.Sprintf("output.%d.content.%d.text", index, content))
				if !value.Exists() || value.String() != prior {
					return nil, errors.New("terminal output differs from streamed text")
				}
			}
			finish = "STOP"
			if kind == "response.incomplete" {
				if response.Get("incomplete_details.reason").String() != "max_output_tokens" {
					return nil, errors.New("unsupported incomplete response")
				}
				finish = "MAX_TOKENS"
			}
			s.Complete = true
		case "response.created", "response.in_progress", "response.output_item.added", "response.content_part.added", "response.content_part.done", "response.output_text.done", "response.function_call_arguments.delta", "response.function_call_arguments.done", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done", "response.reasoning_summary_text.delta", "response.reasoning_summary_text.done":
			return nil, nil
		default:
			return nil, errors.New("unsupported Responses stream event")
		}
	} else {
		if root.Get("object").String() != "response" {
			return nil, errors.New("invalid Responses result")
		}
		if !root.Get("output").IsArray() {
			return nil, errors.New("missing Responses output")
		}
		for index, item := range root.Get("output").Array() {
			converted, err := s.consumeItem(item, index)
			if err != nil {
				return nil, err
			}
			parts = append(parts, converted...)
		}
		switch root.Get("status").String() {
		case "completed":
			finish = "STOP"
		case "incomplete":
			if root.Get("incomplete_details.reason").String() != "max_output_tokens" {
				return nil, errors.New("unsupported incomplete response")
			}
			finish = "MAX_TOKENS"
		default:
			return nil, errors.New("responses result is not complete")
		}
		s.Complete = true
	}
	if len(parts) == 0 && finish == "" {
		return nil, nil
	}
	candidate := map[string]any{"index": 0}
	if len(parts) > 0 {
		candidate["content"] = map[string]any{"role": "model", "parts": parts}
	}
	if finish != "" {
		candidate["finishReason"] = finish
	}
	out := map[string]any{"candidates": []any{candidate}, "modelVersion": s.Model}
	if id := response.Get("id").String(); id != "" {
		out["responseId"] = id
	}
	if usage := response.Get("usage"); usage.Exists() {
		input, output, cached := usage.Get("input_tokens").Int(), usage.Get("output_tokens").Int(), usage.Get("input_tokens_details.cached_tokens").Int()
		if input < 0 || output < 0 || cached < 0 || cached > input {
			return nil, errors.New("invalid Responses usage")
		}
		out["usageMetadata"] = map[string]any{"promptTokenCount": input, "candidatesTokenCount": output, "cachedContentTokenCount": cached, "totalTokenCount": input + output}
	}
	return json.Marshal(out)
}

func (s *GeminiResponsesOutput) reserve(n int) error {
	if n < 0 || n > (4<<20)-s.bytes {
		return errors.New("gemini bridge cumulative output exceeds limit")
	}
	s.bytes += n
	return nil
}

func (s *GeminiResponsesOutput) consumeItem(item gjson.Result, index int) ([]any, error) {
	if index < 0 || index > 4096 {
		return nil, errors.New("invalid output index")
	}
	converted, err := geminiBridgeOutputItem(item)
	if err != nil {
		return nil, err
	}
	if item.Get("type").String() == "message" {
		var remaining []any
		for partIndex, part := range item.Get("content").Array() {
			key := fmt.Sprintf("%d:%d", index, partIndex)
			prior := s.text[key]
			full := part.Get("text").Str
			if !strings.HasPrefix(full, prior) {
				return nil, errors.New("completed text differs from emitted deltas")
			}
			if tail := strings.TrimPrefix(full, prior); tail != "" {
				if err := s.reserve(len(tail)); err != nil {
					return nil, err
				}
				remaining = append(remaining, map[string]any{"text": tail})
			}
			s.text[key] = full
		}
		return remaining, nil
	}
	// Item envelope IDs may be regenerated by the established Chat bridge.
	// Compare the preserved native payload: real call ID, name and exact args.
	encoded, err := json.Marshal(converted)
	if err != nil {
		return nil, err
	}
	if previous, ok := s.items[index]; ok {
		if !geminiResponsesJSONEqual(previous, encoded) {
			return nil, errors.New("completed output item changed")
		}
		return nil, nil
	}
	if len(s.items) >= 64 {
		return nil, errors.New("too many Gemini bridge output items")
	}
	if err := s.reserve(len(encoded)); err != nil {
		return nil, err
	}
	s.items[index] = encoded
	return converted, nil
}

func geminiBridgeOutputItem(item gjson.Result) ([]any, error) {
	if !aiCanonicalFields(item, "type", "name", "call_id", "arguments", "content", "role", "encrypted_content", "namespace") {
		return nil, errors.New("ambiguous Responses item fields")
	}
	switch item.Get("type").String() {
	case "message":
		if item.Get("role").String() != "assistant" || !item.Get("content").IsArray() {
			return nil, errors.New("invalid Responses message")
		}
		var parts []any
		for _, part := range item.Get("content").Array() {
			if part.Get("type").String() != "output_text" || part.Get("text").Type != gjson.String || len(part.Get("annotations").Array()) > 0 {
				return nil, errors.New("unsupported Responses message part")
			}
			parts = append(parts, map[string]any{"text": part.Get("text").Str})
		}
		return parts, nil
	case "function_call":
		if item.Get("namespace").String() != "" {
			return nil, errors.New("namespaced tool output requires native Responses")
		}
		id, name, args := item.Get("call_id").String(), item.Get("name").String(), item.Get("arguments")
		if !aiResponsesValidID(id) || name == "" || args.Type != gjson.String || !gjson.Parse(args.Str).IsObject() || !requestmodel.HasUniqueJSONMembers([]byte(args.Str)) {
			return nil, errors.New("invalid completed Responses function call")
		}
		return []any{map[string]any{"functionCall": map[string]any{"id": id, "name": name, "args": json.RawMessage(args.Str)}}}, nil
	case "reasoning":
		if item.Get("encrypted_content").String() != "" {
			return nil, errors.New("opaque Responses reasoning cannot cross Gemini bridge")
		}
		return nil, nil
	default:
		return nil, errors.New("unsupported Responses output item")
	}
}
