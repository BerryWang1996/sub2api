package service

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/tidwall/gjson"
)

// AIRoutingGeminiToolResult identifies the actual input result, including a
// uniquely provable missing-ID result. IDs authorize nothing until the ingress
// validates their stored origin in the Gemini protocol namespace.
type AIRoutingGeminiToolResult struct {
	ContentIndex int
	PartIndex    int
	CallID       string
	Inferred     bool
}

type aiGeminiParsed struct {
	Request    AIRoutingRequest
	Items      []aiResponsesHistoryItem
	Results    []AIRoutingGeminiToolResult
	References AIRoutingGeminiReferences
}

type AIRoutingGeminiReferences struct {
	CachedContent string
	FileURIs      []string
}

func ParseAIRoutingGeminiReferences(body []byte) (AIRoutingGeminiReferences, error) {
	action := "generateContent"
	root := gjson.ParseBytes(body)
	if root.Get("generateContentRequest").Exists() || root.Get("generate_content_request").Exists() {
		action = "countTokens"
	}
	parsed, err := parseAIRoutingGemini(body, "", action, "metadata")
	return parsed.References, err
}

func NewAIRoutingGeminiRequest(body []byte, publicModel, action, mode string) (AIRoutingRequest, error) {
	if strings.TrimSpace(publicModel) == "" || len(publicModel) > 256 || !IsSafeGeminiModelPathSegment(publicModel) {
		return AIRoutingRequest{}, errors.New("invalid Gemini URL model")
	}
	parsed, err := parseAIRoutingGemini(body, publicModel, action, mode)
	return parsed.Request, err
}

func AIRoutingGeminiToolResults(body []byte) ([]AIRoutingGeminiToolResult, error) {
	parsed, err := parseAIRoutingGemini(body, "", "generateContent", "metadata")
	return parsed.Results, err
}

// aiGeminiField accepts either protobuf JSON spelling, never both. Case-only
// spellings are rejected rather than silently ignored by a later converter.
func aiGeminiField(object gjson.Result, camel, snake string) (gjson.Result, bool) {
	if !object.IsObject() {
		return gjson.Result{}, false
	}
	valid := true
	object.ForEach(func(key, _ gjson.Result) bool {
		if strings.EqualFold(key.Str, camel) && key.Str != camel || snake != "" && strings.EqualFold(key.Str, snake) && key.Str != snake {
			valid = false
			return false
		}
		return true
	})
	a := object.Get(camel)
	if snake == "" || snake == camel {
		return a, valid
	}
	b := object.Get(snake)
	if a.Exists() && b.Exists() {
		return gjson.Result{}, false
	}
	if b.Exists() {
		return b, valid
	}
	return a, valid
}

func parseAIRoutingGemini(body []byte, publicModel, action, mode string) (aiGeminiParsed, error) {
	var parsed aiGeminiParsed
	r := &parsed.Request
	r.Endpoint, r.GeminiAction, r.PublicModel = CompositeRouteEndpointGemini, action, publicModel
	r.InputBytes, r.OutputLimitOmitted = len(body), true
	if action == "countTokens" {
		r.Operation = AIRoutingGeminiCountTokens
	}
	r.InputModalities = []string{"text"}
	invalid := func() (aiGeminiParsed, error) {
		return parsed, errors.New("invalid or unsupported native Gemini request")
	}
	if action != "generateContent" && action != "streamGenerateContent" && action != "countTokens" {
		return invalid()
	}
	if !utf8.Valid(body) || !gjson.ValidBytes(body) || !requestmodel.HasUniqueJSONMembers(body) {
		return invalid()
	}
	root := gjson.ParseBytes(body)
	if !aiCanonicalFields(root, "contents", "model", "input", "messages", "systemInstruction", "system_instruction", "tools", "toolConfig", "tool_config", "generationConfig", "generation_config", "cachedContent", "cached_content", "generateContentRequest", "generate_content_request") {
		return invalid()
	}
	if root.Get("input").Exists() || root.Get("messages").Exists() {
		return invalid()
	}
	if nested, ok := aiGeminiField(root, "generateContentRequest", "generate_content_request"); !ok {
		return invalid()
	} else if nested.Exists() {
		if action != "countTokens" || root.Get("contents").Exists() || !nested.IsObject() {
			return invalid()
		}
		root = nested
	}
	if !aiCanonicalFields(root, "contents", "model", "input", "messages", "systemInstruction", "system_instruction", "tools", "toolConfig", "tool_config", "generationConfig", "generation_config", "cachedContent", "cached_content") || root.Get("input").Exists() || root.Get("messages").Exists() {
		return invalid()
	}
	if model := root.Get("model"); model.Exists() {
		if model.Type != gjson.String || model.Str == "" || publicModel != "" && strings.TrimPrefix(model.Str, "models/") != publicModel {
			return invalid()
		}
	}
	refs := map[AIRoutingOriginRef]bool{}
	addOrigin := func(kind, id string) bool {
		ref := AIRoutingOriginRef{Kind: kind, ID: id}
		if !ValidAIRoutingOriginRef(ref) || !aiResponsesValidID(id) {
			return false
		}
		if refs[ref] {
			return true
		}
		if len(refs) >= aiResponsesOriginLimit {
			return false
		}
		refs[ref] = true
		r.OriginRefs = append(r.OriginRefs, ref)
		r.RequiresOrigin = true
		return true
	}
	if cached, ok := aiGeminiField(root, "cachedContent", "cached_content"); !ok {
		return invalid()
	} else if cached.Exists() {
		if cached.Type != gjson.String || strings.TrimSpace(cached.Str) == "" || len(cached.Str) > 2048 {
			return invalid()
		}
		r.RequiresNativeGemini = true
		parsed.References.CachedContent = cached.Str
		// Existing CachedContent ownership is resolved by the authenticated
		// Gemini ingress. A signature/tool origin must never substitute for it.
	}
	if system, ok := aiGeminiField(root, "systemInstruction", "system_instruction"); !ok {
		return invalid()
	} else if system.Exists() {
		if !aiCanonicalFields(system, "role", "parts") || !system.Get("parts").IsArray() {
			return invalid()
		}
		for _, part := range system.Get("parts").Array() {
			if !aiCanonicalFields(part, "text") || part.Get("text").Type != gjson.String {
				return invalid()
			}
			valid := true
			part.ForEach(func(key, _ gjson.Result) bool {
				if key.Str != "text" {
					valid = false
				}
				return valid
			})
			if !valid {
				return invalid()
			}
		}
	}
	if generation, ok := aiGeminiField(root, "generationConfig", "generation_config"); !ok {
		return invalid()
	} else if generation.Exists() {
		if !generation.IsObject() {
			return invalid()
		}
		for _, pair := range [][2]string{{"topP", "top_p"}, {"temperature", ""}} {
			value, ok := aiGeminiField(generation, pair[0], pair[1])
			if !ok || value.Exists() && value.Type != gjson.Number {
				return invalid()
			}
		}
		if stops, ok := aiGeminiField(generation, "stopSequences", "stop_sequences"); !ok {
			return invalid()
		} else if stops.Exists() {
			if !stops.IsArray() {
				return invalid()
			}
			for _, stop := range stops.Array() {
				if stop.Type != gjson.String {
					return invalid()
				}
			}
			// The reusable Responses execution bridge has no stop-sequence
			// field; native execution must preserve this termination control.
			r.RequiresNativeGemini = r.RequiresNativeGemini || len(stops.Array()) > 0
		}
		if output, ok := aiGeminiField(generation, "maxOutputTokens", "max_output_tokens"); !ok {
			return invalid()
		} else if output.Exists() {
			value, valid := aiChatPositiveInt(output)
			if !valid {
				return invalid()
			}
			r.MaxOutput, r.OutputLimitOmitted = value, false
		}
		if count, ok := aiGeminiField(generation, "candidateCount", "candidate_count"); !ok {
			return invalid()
		} else if count.Exists() {
			value, valid := aiChatPositiveInt(count)
			if !valid {
				return invalid()
			}
			r.RequiresNativeGemini = r.RequiresNativeGemini || value > 1
			r.OutputChoices = value
		}
		// Sampling and token cap have a common bridge. Other generation
		// constraints require an adapter that explicitly preserves them.
		generation.ForEach(func(key, _ gjson.Result) bool {
			switch key.Str {
			case "maxOutputTokens", "max_output_tokens", "candidateCount", "candidate_count", "temperature", "topP", "top_p", "stopSequences", "stop_sequences":
			default:
				r.RequiresNativeGemini = true
			}
			return true
		})
	}
	if tools := root.Get("tools"); tools.Exists() {
		if !tools.IsArray() {
			return invalid()
		}
		for _, tool := range tools.Array() {
			functions, ok := aiGeminiField(tool, "functionDeclarations", "function_declarations")
			if !ok {
				return invalid()
			}
			if functions.Exists() {
				if !functions.IsArray() {
					return invalid()
				}
				for _, function := range functions.Array() {
					if !aiCanonicalFields(function, "name", "description", "parameters", "parametersJsonSchema", "parameters_json_schema") || function.Get("name").Type != gjson.String || function.Get("name").Str == "" {
						return invalid()
					}
					jsonSchema, ok := aiGeminiField(function, "parametersJsonSchema", "parameters_json_schema")
					parameters := function.Get("parameters")
					if !ok || jsonSchema.Exists() && parameters.Exists() || jsonSchema.Exists() && !jsonSchema.IsObject() || parameters.Exists() && !parameters.IsObject() {
						return invalid()
					}
					if parameters.Exists() {
						if _, err := geminiBridgeSchema(parameters); err != nil {
							r.RequiresNativeGemini = true
						}
					}
					function.ForEach(func(key, _ gjson.Result) bool {
						switch key.Str {
						case "name", "description", "parameters", "parametersJsonSchema", "parameters_json_schema":
						default:
							r.RequiresNativeGemini = true
						}
						return true
					})
				}
			}
			tool.ForEach(func(key, _ gjson.Result) bool {
				if key.Str != "functionDeclarations" && key.Str != "function_declarations" {
					r.RequiresNativeGemini = true
				}
				return true
			})
		}
		r.UsesTools = len(tools.Array()) > 0
	}
	if config, ok := aiGeminiField(root, "toolConfig", "tool_config"); !ok {
		return invalid()
	} else if config.Exists() {
		calling, ok := aiGeminiField(config, "functionCallingConfig", "function_calling_config")
		if !ok {
			return invalid()
		}
		if calling.Exists() {
			if !aiCanonicalFields(calling, "mode") || calling.Get("mode").Exists() && calling.Get("mode").Type != gjson.String {
				return invalid()
			}
			names, ok := aiGeminiField(calling, "allowedFunctionNames", "allowed_function_names")
			if !ok || names.Exists() && !names.IsArray() {
				return invalid()
			}
			for _, name := range names.Array() {
				if name.Type != gjson.String || name.Str == "" {
					return invalid()
				}
			}
			mode := calling.Get("mode").String()
			if mode != "" && mode != "AUTO" && mode != "NONE" && mode != "ANY" || len(names.Array()) > 0 && (mode != "ANY" || len(names.Array()) != 1) {
				r.RequiresNativeGemini = true
			}
			calling.ForEach(func(key, _ gjson.Result) bool {
				if key.Str != "mode" && key.Str != "allowedFunctionNames" && key.Str != "allowed_function_names" {
					r.RequiresNativeGemini = true
				}
				return true
			})
		}
		config.ForEach(func(key, _ gjson.Result) bool {
			if key.Str != "functionCallingConfig" && key.Str != "function_calling_config" {
				r.RequiresNativeGemini = true
			}
			return true
		})
	}
	root.ForEach(func(key, _ gjson.Result) bool {
		switch key.Str {
		case "contents", "model", "systemInstruction", "system_instruction", "tools", "toolConfig", "tool_config", "generationConfig", "generation_config", "cachedContent", "cached_content":
		default:
			r.RequiresNativeGemini = true
		}
		return true
	})
	contents := root.Get("contents")
	if !contents.IsArray() || len(contents.Array()) == 0 {
		return invalid()
	}
	type call struct{ id, name, definition string }
	pending := []call{}
	definitions := map[string]string{}
	results := map[string]string{}
	lastRole := ""
	for index, content := range contents.Array() {
		if !aiCanonicalFields(content, "role", "parts") || !content.Get("parts").IsArray() {
			return invalid()
		}
		role := content.Get("role")
		if role.Exists() && role.Type != gjson.String || role.Str != "" && role.Str != "user" && role.Str != "model" {
			return invalid()
		}
		roleName := role.Str
		if roleName == "" {
			roleName = "user"
		}
		if roleName == "model" && lastRole == "user" {
			pending = nil
		}
		lastRole = roleName
		entry := aiResponsesHistoryItem{Index: index, Role: "user"}
		if roleName == "model" {
			entry.Role = "assistant"
		}
		for block, part := range content.Get("parts").Array() {
			if !aiCanonicalFields(part, "text", "thought") {
				return invalid()
			}
			thought := part.Get("thought")
			if thought.Exists() && thought.Type != gjson.True && thought.Type != gjson.False {
				return invalid()
			}
			signature, signatureOK := aiGeminiField(part, "thoughtSignature", "thought_signature")
			fc, callOK := aiGeminiField(part, "functionCall", "function_call")
			fr, resultOK := aiGeminiField(part, "functionResponse", "function_response")
			inline, inlineOK := aiGeminiField(part, "inlineData", "inline_data")
			file, fileOK := aiGeminiField(part, "fileData", "file_data")
			if !signatureOK || !callOK || !resultOK || !inlineOK || !fileOK {
				return invalid()
			}
			part.ForEach(func(key, _ gjson.Result) bool {
				switch key.Str {
				case "text", "thought", "thoughtSignature", "thought_signature", "functionCall", "function_call", "functionResponse", "function_response", "inlineData", "inline_data", "fileData", "file_data":
				default:
					r.RequiresNativeGemini = true
				}
				return true
			})
			if signature.Exists() {
				if signature.Type != gjson.String || signature.Str == "" || !addOrigin("opaque", AIRoutingOpaqueOriginID(signature.Str)) {
					return invalid()
				}
				r.RequiresNativeGemini = true
			}
			payloads := 0
			for _, value := range []gjson.Result{part.Get("text"), fc, fr, inline, file} {
				if value.Exists() {
					payloads++
				}
			}
			if payloads > 1 {
				return invalid()
			}
			if text := part.Get("text"); text.Exists() {
				if text.Type != gjson.String {
					return invalid()
				}
				if thought.Bool() {
					r.RequiresNativeGemini = true
					entry.Excluded++
				} else {
					entry.Parts = append(entry.Parts, aiResponsesHistoryPart{Block: block, Text: text.Str})
				}
			} else if fc.Exists() {
				if roleName != "model" || !aiCanonicalFields(fc, "id", "name", "args") || fc.Get("name").Type != gjson.String || fc.Get("name").Str == "" || fc.Get("args").Exists() && !fc.Get("args").IsObject() {
					return invalid()
				}
				fc.ForEach(func(key, _ gjson.Result) bool {
					if key.Str != "id" && key.Str != "name" && key.Str != "args" {
						r.RequiresNativeGemini = true
					}
					return true
				})
				id := fc.Get("id")
				if id.Exists() && (id.Type != gjson.String || !aiResponsesValidID(id.Str)) {
					return invalid()
				}
				definition := fc.Get("name").Str + "\x00" + fc.Get("args").Raw
				if id.Str != "" {
					if old, exists := definitions[id.Str]; exists && old != definition {
						return invalid()
					}
					definitions[id.Str] = definition
					if !addOrigin("tool_call", id.Str) {
						return invalid()
					}
				} else {
					// A legacy model call with no gateway-issued identity is not
					// proof of ownership, even when its function name is unique.
					r.RequiresOrigin, r.RequiresNativeGemini = true, true
				}
				pending = append(pending, call{id.Str, fc.Get("name").Str, definition})
				entry.Excluded++
				r.UsesTools = true
			} else if fr.Exists() {
				if roleName != "user" || !aiCanonicalFields(fr, "id", "name", "response", "parts") || fr.Get("name").Type != gjson.String || fr.Get("name").Str == "" || !fr.Get("response").IsObject() {
					return invalid()
				}
				fr.ForEach(func(key, _ gjson.Result) bool {
					if key.Str != "id" && key.Str != "name" && key.Str != "response" && key.Str != "parts" {
						r.RequiresNativeGemini = true
					}
					return true
				})
				id := fr.Get("id")
				if id.Exists() && (id.Type != gjson.String || !aiResponsesValidID(id.Str)) {
					return invalid()
				}
				callID, inferred := id.Str, false
				if callID == "" {
					if len(pending) != 1 || !strings.HasPrefix(pending[0].id, GeminiResponsesToolCallPrefix) || pending[0].name != fr.Get("name").Str {
						return invalid()
					}
					callID, inferred = pending[0].id, true
				}
				for i, candidate := range pending {
					if candidate.id == callID {
						if candidate.name != fr.Get("name").Str {
							return invalid()
						}
						pending = append(pending[:i], pending[i+1:]...)
						break
					}
				}
				if old, exists := results[callID]; exists && old != fr.Raw {
					return invalid()
				}
				results[callID] = fr.Raw
				if !addOrigin("tool_call", callID) {
					return invalid()
				}
				if !aiFinalCapacityContains(r.ToolResults, callID) {
					r.ToolResults = append(r.ToolResults, callID)
				}
				parsed.Results = append(parsed.Results, AIRoutingGeminiToolResult{index, block, callID, inferred})
				if fr.Get("parts").Exists() {
					r.RequiresNativeGemini = true
					if !fr.Get("parts").IsArray() {
						return invalid()
					}
					for _, mediaPart := range fr.Get("parts").Array() {
						if !aiGeminiCollectMedia(mediaPart, r, &parsed.References) {
							return invalid()
						}
					}
				}
				entry.Excluded++
				r.UsesTools = true
			} else if inline.Exists() || file.Exists() {
				if !aiGeminiCollectMedia(part, r, &parsed.References) {
					return invalid()
				}
				entry.Excluded++
			} else {
				// Native code/grounding/opaque parts remain in the execution body.
				// They never become plain task text or an equivalent bridge claim.
				r.RequiresNativeGemini = true
				entry.Excluded++
			}
		}
		parsed.Items = append(parsed.Items, entry)
	}
	if mode == "bounded_history" {
		r.HistoryBody = append([]byte(nil), body...)
	}
	var task strings.Builder
	lastRole, turns := "", 0
	for _, item := range parsed.Items {
		if item.Role != lastRole {
			turns++
			if item.Role == "user" {
				task.Reset()
			}
		}
		lastRole = item.Role
		if item.Role == "user" {
			for _, part := range item.Parts {
				if task.Len() > 0 {
					_ = task.WriteByte('\n')
				}
				_, _ = task.WriteString(part.Text)
			}
		}
	}
	r.Task = fmt.Sprintf("turns=%d; tool_results=%d; images=%t", turns, len(r.ToolResults), r.HasImages)
	if mode == "task_excerpt" {
		r.Task += "; task=" + boundedTask(task.String())
	}
	if action == "countTokens" {
		r.MaxOutput, r.OutputLimitOmitted = 0, false
	}
	return parsed, nil
}

func aiGeminiCollectMedia(part gjson.Result, r *AIRoutingRequest, refs *AIRoutingGeminiReferences) bool {
	inline, inlineOK := aiGeminiField(part, "inlineData", "inline_data")
	file, fileOK := aiGeminiField(part, "fileData", "file_data")
	if !inlineOK || !fileOK || inline.Exists() == file.Exists() {
		return false
	}
	media := inline
	if file.Exists() {
		media = file
		r.RequiresNativeGemini = true
	}
	mime, ok := aiGeminiField(media, "mimeType", "mime_type")
	if !ok || mime.Type != gjson.String || mime.Str == "" {
		return false
	}
	if inline.Exists() && (inline.Get("data").Type != gjson.String || inline.Get("data").Str == "") {
		return false
	}
	if file.Exists() {
		uri, ok := aiGeminiField(file, "fileUri", "file_uri")
		if !ok || uri.Type != gjson.String || uri.Str == "" || len(uri.Str) > 4096 {
			return false
		}
		if !aiFinalCapacityContains(refs.FileURIs, uri.Str) {
			if len(refs.FileURIs) >= aiResponsesOriginLimit {
				return false
			}
			refs.FileURIs = append(refs.FileURIs, uri.Str)
		}
	}
	modality := "document"
	for _, prefix := range []string{"image", "audio", "video"} {
		if strings.HasPrefix(mime.Str, prefix+"/") {
			modality = prefix
			break
		}
	}
	if !aiFinalCapacityContains(r.InputModalities, modality) {
		r.InputModalities = append(r.InputModalities, modality)
	}
	r.HasImages = true
	if modality != "image" {
		r.RequiresNativeGemini = true
	}
	return true
}
