package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/tidwall/gjson"
)

const aiResponsesOriginLimit = 64

type aiResponsesHistoryPart struct {
	Block int
	Text  string
}

type aiResponsesHistoryItem struct {
	Index    int
	Role     string
	Parts    []aiResponsesHistoryPart
	Excluded int
}

type aiResponsesParsed struct {
	Request AIRoutingRequest
	Items   []aiResponsesHistoryItem
}

// AIRoutingOpaqueOriginID lets response writers persist provenance without
// storing encrypted reasoning itself. The input is the decoded JSON string.
func AIRoutingOpaqueOriginID(encrypted string) string {
	sum := sha256.Sum256([]byte(encrypted))
	return hex.EncodeToString(sum[:])
}

// NewAIRoutingResponsesRequest reads native Responses input without converting
// it to another protocol. HistoryBody is an execution-body copy, never a
// projection to forward. IDs and excluded content never enter Task.
func NewAIRoutingResponsesRequest(body []byte, mode string) (AIRoutingRequest, error) {
	parsed, err := parseAIRoutingResponses(body, mode)
	return parsed.Request, err
}

func parseAIRoutingResponses(body []byte, mode string) (aiResponsesParsed, error) {
	var parsed aiResponsesParsed
	r := &parsed.Request
	r.Endpoint = CompositeRouteEndpointResponses
	r.InputBytes, r.StoreResponse, r.OutputLimitOmitted = len(body), true, true
	r.InputModalities = []string{"text"}
	invalid := func() (aiResponsesParsed, error) { return parsed, errors.New("invalid native Responses request") }
	if !utf8.Valid(body) || !gjson.ValidBytes(body) || !requestmodel.HasUniqueJSONMembers(body) {
		return invalid()
	}
	root := gjson.ParseBytes(body)
	if !aiCanonicalFields(root, "model", "input", "instructions", "tools", "tool_choice", "max_output_tokens", "previous_response_id", "conversation", "background", "store", "stream", "messages", "prompt", "commands", "max_tokens", "max_completion_tokens", "text", "truncation", "parallel_tool_calls", "reasoning", "temperature", "top_p", "include", "service_tier", "metadata", "context_management", "prompt_cache_key", "prompt_cache_options") {
		return invalid()
	}
	model := root.Get("model")
	if model.Type != gjson.String || strings.TrimSpace(model.Str) == "" || len(model.Str) > 256 {
		return invalid()
	}
	r.PublicModel = model.Str
	// Do not silently choose one of two histories, or resolve an account-owned
	// prompt template using an arbitrary newly selected execution account.
	for _, alias := range []string{"messages", "prompt", "commands", "max_tokens", "max_completion_tokens"} {
		if root.Get(alias).Exists() {
			return invalid()
		}
	}
	if instructions := root.Get("instructions"); instructions.Exists() && instructions.Type != gjson.Null && instructions.Type != gjson.String {
		return invalid()
	}
	for _, flag := range []struct {
		name  string
		value *bool
	}{{"store", &r.StoreResponse}, {"background", &r.Background}} {
		if value := root.Get(flag.name); value.Exists() {
			if value.Type != gjson.True && value.Type != gjson.False {
				return invalid()
			}
			*flag.value = value.Bool()
		}
	}
	if stream := root.Get("stream"); stream.Exists() && stream.Type != gjson.True && stream.Type != gjson.False {
		return invalid()
	}
	r.RequiresNativeResponses = r.StoreResponse || r.Background || aiResponsesNativeSemantics(root)
	r.RequiresStoredResponses = r.StoreResponse || r.Background
	if output := root.Get("max_output_tokens"); output.Exists() {
		value, err := strconv.ParseInt(output.Raw, 10, 64)
		if output.Type != gjson.Number || err != nil || value <= 0 || uint64(value) > uint64(^uint(0)>>1) {
			return invalid()
		}
		r.MaxOutput, r.OutputLimitOmitted = int(value), false
	}
	refs := make(map[AIRoutingOriginRef]int)
	addOrigin := func(kind, id string, stored bool) bool {
		ref := AIRoutingOriginRef{Kind: kind, ID: id}
		if !aiResponsesValidID(id) || !ValidAIRoutingOriginRef(ref) {
			return false
		}
		if index, exists := refs[ref]; exists {
			r.OriginRefs[index].RequiresStored = r.OriginRefs[index].RequiresStored || stored
			return true
		}
		if len(refs) >= aiResponsesOriginLimit {
			return false
		}
		refs[ref] = len(r.OriginRefs)
		ref.RequiresStored = stored
		r.OriginRefs = append(r.OriginRefs, ref)
		r.RequiresOrigin = true
		return true
	}
	if previous := root.Get("previous_response_id"); previous.Exists() && previous.Type != gjson.Null {
		if previous.Type != gjson.String || !addOrigin("response", previous.Str, true) {
			return invalid()
		}
		r.PreviousResponseID = previous.Str
		r.RequiresNativeResponses = true
		r.RequiresStoredResponses = true
	}
	if conversation := root.Get("conversation"); conversation.Exists() && conversation.Type != gjson.Null {
		if r.PreviousResponseID != "" {
			return invalid()
		}
		id := conversation
		if conversation.IsObject() && aiCanonicalFields(conversation, "id") {
			id = conversation.Get("id")
		}
		if id.Type != gjson.String || !addOrigin("conversation", id.Str, true) {
			return invalid()
		}
		r.RequiresNativeResponses = true
		r.RequiresStoredResponses = true
	}
	if tools := root.Get("tools"); tools.Exists() {
		if !tools.IsArray() {
			return invalid()
		}
		for _, tool := range tools.Array() {
			if !aiCanonicalFields(tool, "type", "name", "description", "parameters", "tools", "children", "strict") || tool.Get("type").Type != gjson.String || tool.Get("type").Str == "" {
				return invalid()
			}
			r.UsesTools = true
			if kind := tool.Get("type").Str; kind != "function" {
				r.RequiresNativeResponses = true
			}
			tool.ForEach(func(field, value gjson.Result) bool {
				if value.Type == gjson.Null {
					return true
				}
				switch field.Str {
				case "type", "name", "description", "parameters":
				case "strict":
					r.RequiresNativeResponses = r.RequiresNativeResponses || value.Type != gjson.False
				default:
					r.RequiresNativeResponses = true
				}
				return true
			})
		}
	}
	input := root.Get("input")
	var items []gjson.Result
	switch {
	case input.Type == gjson.String:
		parsed.Items = append(parsed.Items, aiResponsesHistoryItem{Index: 0, Role: "user", Parts: []aiResponsesHistoryPart{{Text: input.Str}}})
	case input.IsArray():
		items = input.Array()
	case !input.Exists() && r.RequiresOrigin:
		// A state-only continuation still needs its recorded origin, and must
		// never classify an invented current user request.
	default:
		return invalid()
	}
	itemIDs := make(map[string]string)
	callDefinitions := make(map[string]string)
	callOutputs := make(map[string]string)
	pending := make([]string, 0)
	for index, item := range items {
		if !aiCanonicalFields(item, "type", "id", "role", "content", "call_id", "name", "arguments", "input", "output", "summary", "encrypted_content", "status", "approval_request_id") {
			return invalid()
		}
		kind := item.Get("type")
		if kind.Exists() && (kind.Type != gjson.String || kind.Str == "") {
			return invalid()
		}
		if status := item.Get("status"); status.Exists() && status.Type != gjson.String {
			return invalid()
		}
		id := item.Get("id")
		if id.Exists() && (id.Type != gjson.String || !aiResponsesValidID(id.Str)) {
			return invalid()
		}
		if id.Exists() && kind.Str != "item_reference" {
			if old, exists := itemIDs[id.Str]; exists && old != item.Raw {
				return invalid()
			}
			itemIDs[id.Str] = item.Raw
		}
		entry := aiResponsesHistoryItem{Index: index}
		switch kind.Str {
		case "", "message":
			role := item.Get("role")
			if role.Type != gjson.String || role.Str != "user" && role.Str != "assistant" && role.Str != "system" && role.Str != "developer" {
				return invalid()
			}
			entry.Role = role.Str
			content := item.Get("content")
			if content.Type == gjson.String {
				if role.Str == "user" || role.Str == "assistant" {
					entry.Parts = []aiResponsesHistoryPart{{Text: content.Str}}
				} else {
					entry.Excluded++
				}
			} else if content.IsArray() {
				for block, part := range content.Array() {
					text, visible, ok := aiResponsesContentPart(part, r)
					if !ok {
						return invalid()
					}
					if visible && (role.Str == "user" || role.Str == "assistant") {
						entry.Parts = append(entry.Parts, aiResponsesHistoryPart{Block: block, Text: text})
					} else {
						entry.Excluded++
					}
				}
			} else {
				return invalid()
			}
			if role.Str == "assistant" && aiResponsesHasVisibleText(entry.Parts) && item.Get("status").Str != "in_progress" && item.Get("status").Str != "incomplete" {
				pending = pending[:0]
			}
		case "function_call", "custom_tool_call":
			callID, name := item.Get("call_id"), item.Get("name")
			if callID.Type != gjson.String || !aiResponsesValidID(callID.Str) || name.Type != gjson.String || name.Str == "" {
				return invalid()
			}
			argument := item.Get("arguments")
			if kind.Str == "custom_tool_call" {
				argument = item.Get("input")
				r.RequiresNativeResponses = true
			}
			if argument.Type != gjson.String {
				return invalid()
			}
			definition := kind.Str + "\x00" + name.Str + "\x00" + argument.Str
			if old, exists := callDefinitions[callID.Str]; exists && old != definition {
				return invalid()
			}
			if output, exists := callOutputs[callID.Str]; exists && !strings.HasPrefix(output, kind.Str+"_output\x00") {
				return invalid()
			}
			callDefinitions[callID.Str] = definition
			if strings.HasPrefix(callID.Str, GeminiResponsesToolCallPrefix) && !addOrigin("tool_call", callID.Str, false) {
				return invalid()
			}
			entry.Role, entry.Excluded, r.UsesTools = "assistant", 1, true
			pending = pending[:0] // a subsequent assistant action consumed the old results
		case "function_call_output", "custom_tool_call_output":
			if kind.Str == "custom_tool_call_output" {
				r.RequiresNativeResponses = true
			}
			callID, output := item.Get("call_id"), item.Get("output")
			if callID.Type != gjson.String || !aiResponsesValidID(callID.Str) || !aiResponsesToolOutput(output, r) {
				return invalid()
			}
			outputIdentity := kind.Str + "\x00" + output.Raw
			if old, exists := callOutputs[callID.Str]; exists && old != outputIdentity {
				return invalid()
			}
			if definition, exists := callDefinitions[callID.Str]; exists && !strings.HasPrefix(definition, strings.TrimSuffix(kind.Str, "_output")+"\x00") {
				return invalid()
			}
			callOutputs[callID.Str] = outputIdentity
			if !aiFinalCapacityContains(pending, callID.Str) {
				if len(pending) >= aiResponsesOriginLimit {
					return invalid()
				}
				pending = append(pending, callID.Str)
			}
			entry.Role, entry.Excluded, r.UsesTools = "user", 1, true
		case "item_reference":
			if id.Type != gjson.String || !addOrigin("item", id.Str, true) {
				return invalid()
			}
			entry.Excluded, r.RequiresNativeResponses = 1, true
			r.RequiresStoredResponses = true
		case "compaction_trigger":
			entry.Excluded, r.RequiresNativeResponses = 1, true
		case "reasoning", "compaction":
			entry.Role, entry.Excluded, r.RequiresNativeResponses = "assistant", 1, true
			if kind.Str == "compaction" && !item.Get("encrypted_content").Exists() {
				return invalid()
			}
			if encrypted := item.Get("encrypted_content"); encrypted.Exists() {
				if encrypted.Type != gjson.String || encrypted.Str == "" {
					return invalid()
				}
				refKind, refID := "item", id.Str
				if refID == "" {
					refKind, refID = "opaque", AIRoutingOpaqueOriginID(encrypted.Str)
				}
				if !addOrigin(refKind, refID, false) {
					return invalid()
				}
			}
		default:
			// These native server-tool items cannot be treated as text or
			// translated by guessing an account. Unknown item types fail closed.
			switch kind.Str {
			case "web_search_call", "file_search_call", "code_interpreter_call", "image_generation_call", "computer_call", "computer_call_output", "local_shell_call", "local_shell_call_output", "shell_call", "shell_call_output", "apply_patch_call", "apply_patch_call_output", "tool_search_call", "tool_search_output", "mcp_call", "mcp_list_tools", "mcp_approval_request", "mcp_approval_response":
				entry.Role, entry.Excluded = "assistant", 1
				r.UsesTools, r.RequiresNativeResponses, r.RequiresOrigin = true, true, true
				ref := id
				if kind.Str == "mcp_approval_response" {
					ref = item.Get("approval_request_id")
				}
				if ref.Type != gjson.String || !addOrigin("item", ref.Str, false) {
					return invalid()
				}
			default:
				return invalid()
			}
		}
		parsed.Items = append(parsed.Items, entry)
	}
	for _, id := range pending {
		if !addOrigin("tool_call", id, false) {
			return invalid()
		}
	}
	r.ToolResults = append([]string(nil), pending...)
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

// This is an adapter contract, not a model capability inference. Fields absent
// from a conversion must stay on the native wire even when the model could
// theoretically honor them. Unknown extensions are retained only natively.
func aiResponsesNativeSemantics(root gjson.Result) bool {
	native := false
	root.ForEach(func(field, value gjson.Result) bool {
		if value.Type == gjson.Null {
			return true
		}
		switch field.Str {
		case "model", "input", "instructions", "max_output_tokens", "stream", "store", "background", "tools", "temperature", "top_p":
		case "parallel_tool_calls":
			native = native || value.Type != gjson.True
		case "include":
			native = native || !value.IsArray() || len(value.Array()) != 0
		case "tool_choice":
			if value.Type == gjson.String {
				native = native || value.Str != "auto" && value.Str != "none" && value.Str != "required"
			} else if value.IsObject() && value.Get("type").Str == "function" && value.Get("name").Type == gjson.String {
				value.ForEach(func(key, _ gjson.Result) bool { native = native || key.Str != "type" && key.Str != "name"; return true })
			} else {
				native = true
			}
		default:
			native = true
		}
		return true
	})
	return native
}

func aiResponsesValidID(id string) bool {
	return ValidAIRoutingOriginRef(AIRoutingOriginRef{Kind: "item", ID: id}) && !strings.ContainsAny(id, "\x00\r\n")
}

func aiResponsesHasVisibleText(parts []aiResponsesHistoryPart) bool {
	for _, part := range parts {
		if strings.TrimSpace(part.Text) != "" {
			return true
		}
	}
	return false
}

func aiResponsesContentPart(part gjson.Result, r *AIRoutingRequest) (string, bool, bool) {
	if !aiCanonicalFields(part, "type", "text", "refusal", "image_url", "file_id", "file_data", "filename", "annotations") || part.Get("type").Type != gjson.String {
		return "", false, false
	}
	switch part.Get("type").Str {
	case "input_text", "output_text":
		text := part.Get("text")
		return text.Str, true, text.Type == gjson.String
	case "refusal":
		text := part.Get("refusal")
		return text.Str, true, text.Type == gjson.String
	case "input_image", "input_file", "input_audio", "input_video":
		kind := map[string]string{"input_image": "image", "input_file": "document", "input_audio": "audio", "input_video": "video"}[part.Get("type").Str]
		r.addInputModality(kind)
		r.HasImages = true
		if kind != "image" {
			r.RequiresNativeResponses = true
		}
		if fileID := part.Get("file_id"); fileID.Exists() {
			// Uploaded files are account-owned server state. No Files API
			// origin is implemented in this slice. An unrelated previous-response
			// binding cannot authorize a file reference, so fail closed here.
			return "", false, false
		}
		return "", false, true
	default:
		return "", false, false
	}
}

func aiResponsesToolOutput(output gjson.Result, r *AIRoutingRequest) bool {
	if output.Type == gjson.String {
		return true
	}
	if !output.IsArray() {
		return false
	}
	for _, part := range output.Array() {
		if _, _, ok := aiResponsesContentPart(part, r); !ok {
			return false
		}
	}
	return true
}
