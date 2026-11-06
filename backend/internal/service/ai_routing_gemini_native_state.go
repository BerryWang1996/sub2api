package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Native state is scoped by the ingress to the currently selected account and
// strict API-key origin. Opaque values are never decision or diagnostic data.
type AIRoutingGeminiNativeStateCallbacks struct {
	LoadTool   func(context.Context, string) (GeminiResponsesToolState, error)
	LoadOpaque func(context.Context, string) (json.RawMessage, error)
}

type aiRoutingGeminiNativeStateKey struct{}

func WithAIRoutingGeminiNativeState(ctx context.Context, callbacks AIRoutingGeminiNativeStateCallbacks) context.Context {
	return context.WithValue(ctx, aiRoutingGeminiNativeStateKey{}, callbacks)
}

// RestoreAIRoutingGeminiNativeRequest restores provider IDs/parts after actual
// account selection. The original caller buffer is not modified. Vertex v1
// never receives a gateway-injected functionCall/functionResponse ID.
func RestoreAIRoutingGeminiNativeRequest(ctx context.Context, body []byte) ([]byte, error) {
	callbacks, active := ctx.Value(aiRoutingGeminiNativeStateKey{}).(AIRoutingGeminiNativeStateCallbacks)
	if !active {
		return body, nil
	}
	if ctx.Err() != nil {
		return nil, geminiResponsesStateError()
	}
	root := gjson.ParseBytes(body)
	base, action := "", "generateContent"
	if nested, ok := aiGeminiField(root, "generateContentRequest", "generate_content_request"); !ok {
		return nil, geminiResponsesStateError()
	} else if nested.Exists() {
		action = "countTokens"
		base = "generateContentRequest."
		if root.Get("generate_content_request").Exists() {
			base = "generate_content_request."
		}
	}
	parsed, err := parseAIRoutingGemini(body, "", action, "metadata")
	if err != nil {
		return nil, geminiResponsesStateError()
	}
	states := map[string]GeminiResponsesToolState{}
	load := func(id string) (GeminiResponsesToolState, error) {
		if state, ok := states[id]; ok {
			return state, nil
		}
		if callbacks.LoadTool == nil || ctx.Err() != nil {
			return GeminiResponsesToolState{}, geminiResponsesStateError()
		}
		state, err := callbacks.LoadTool(ctx, id)
		if err != nil || !validGeminiResponsesToolState(state) {
			return state, geminiResponsesStateError()
		}
		states[id] = state
		return state, nil
	}
	resultIDs := map[[2]int]string{}
	for _, result := range parsed.Results {
		resultIDs[[2]int{result.ContentIndex, result.PartIndex}] = result.CallID
	}
	out := append([]byte(nil), body...)
	seen := map[string]bool{}
	pending := map[string]bool{}
	lastIndex := map[string]int{}
	for contentIndex, content := range gjson.GetBytes(body, base+"contents").Array() {
		hasResult := false
		for partIndex, original := range content.Get("parts").Array() {
			path := fmt.Sprintf("%scontents.%d.parts.%d", base, contentIndex, partIndex)
			fc, _ := aiGeminiField(original, "functionCall", "function_call")
			if fc.Exists() && fc.Get("id").String() != "" {
				id := fc.Get("id").String()
				state, err := load(id)
				if err != nil {
					return nil, err
				}
				args := fc.Get("args").Raw
				if args == "" {
					args = "{}"
				}
				if state.Name != fc.Get("name").String() || !geminiResponsesJSONEqual(state.Arguments, []byte(args)) {
					return nil, geminiResponsesStateError()
				}
				// The gateway changes only the call ID on the visible native
				// response. Reject mutations to signed/opaque surrounding fields.
				field := "functionCall"
				if original.Get("function_call").Exists() {
					field = "function_call"
				}
				nativeCall, _ := aiGeminiField(gjson.ParseBytes(state.Part), "functionCall", "function_call")
				comparison := []byte(original.Raw)
				if nativeCall.Get("id").Exists() {
					comparison, err = sjson.SetBytes(comparison, field+".id", nativeCall.Get("id").Str)
				} else {
					comparison, err = sjson.DeleteBytes(comparison, field+".id")
				}
				if err != nil || !geminiResponsesJSONEqual(comparison, state.Part) {
					return nil, geminiResponsesStateError()
				}
				if previous, ok := lastIndex[state.Batch]; state.Batch != "" && ok && previous >= state.Index {
					return nil, geminiResponsesStateError()
				}
				lastIndex[state.Batch] = state.Index
				out, err = sjson.SetRawBytes(out, path, state.Part)
				if err != nil {
					return nil, err
				}
				seen[id] = true
				pending[id] = true
			}
			if id := resultIDs[[2]int{contentIndex, partIndex}]; id != "" {
				hasResult = true
				state, err := load(id)
				if err != nil {
					return nil, err
				}
				fr, _ := aiGeminiField(original, "functionResponse", "function_response")
				if state.Name != fr.Get("name").String() {
					return nil, geminiResponsesStateError()
				}
				// Native GenerateContent is stateless: retain the complete caller
				// turn, rather than inventing an omitted conversation from a call ID.
				if !seen[id] {
					return nil, geminiResponsesStateError()
				}
				delete(pending, id)
				field := "functionResponse"
				if original.Get("function_response").Exists() {
					field = "function_response"
				}
				nativeCall, _ := aiGeminiField(gjson.ParseBytes(state.Part), "functionCall", "function_call")
				if nativeID := nativeCall.Get("id").String(); nativeID != "" {
					out, err = sjson.SetBytes(out, path+"."+field+".id", nativeID)
				} else {
					out, err = sjson.DeleteBytes(out, path+"."+field+".id")
				}
				if err != nil {
					return nil, err
				}
			}
			part := gjson.GetBytes(out, path)
			if signature, _ := aiGeminiField(part, "thoughtSignature", "thought_signature"); signature.Exists() {
				if callbacks.LoadOpaque == nil {
					return nil, geminiResponsesStateError()
				}
				expected, err := callbacks.LoadOpaque(ctx, AIRoutingOpaqueOriginID(signature.String()))
				if err != nil || !geminiResponsesJSONEqual([]byte(part.Raw), expected) {
					return nil, geminiResponsesStateError()
				}
			}
		}
		if hasResult && len(pending) > 0 {
			return nil, geminiResponsesStateError()
		}
	}
	if len(pending) > 0 {
		return nil, geminiResponsesStateError()
	}
	// Vertex v1 matches ID-less parallel results by name and position. Sort
	// only result slots from the same saved response batch, leaving surrounding
	// visible text/media and every model part in their original positions.
	byContent := map[int][]AIRoutingGeminiToolResult{}
	for _, result := range parsed.Results {
		byContent[result.ContentIndex] = append(byContent[result.ContentIndex], result)
	}
	for contentIndex, results := range byContent {
		if len(results) < 2 {
			continue
		}
		sorted := append([]AIRoutingGeminiToolResult(nil), results...)
		ranks := map[string]int{}
		for _, result := range results {
			batch := states[result.CallID].Batch
			if _, ok := ranks[batch]; !ok {
				ranks[batch] = len(ranks)
			}
		}
		sort.SliceStable(sorted, func(i, j int) bool {
			a, b := states[sorted[i].CallID], states[sorted[j].CallID]
			if a.Batch != b.Batch {
				return ranks[a.Batch] < ranks[b.Batch]
			}
			return a.Index < b.Index
		})
		parts := make([]json.RawMessage, len(sorted))
		for i, result := range sorted {
			parts[i] = json.RawMessage(gjson.GetBytes(out, fmt.Sprintf("%scontents.%d.parts.%d", base, contentIndex, result.PartIndex)).Raw)
		}
		for i, result := range results {
			out, err = sjson.SetRawBytes(out, fmt.Sprintf("%scontents.%d.parts.%d", base, contentIndex, result.PartIndex), parts[i])
			if err != nil {
				return nil, err
			}
		}
	}
	if ctx.Err() != nil {
		return nil, geminiResponsesStateError()
	}
	return out, nil
}

// AI-generated IDs must never be cleaned into dummy signatures or sent as
// provider IDs by a path that omitted native state restoration.
func HasAIRoutingGeminiNativeState(ctx context.Context) bool {
	_, ok := ctx.Value(aiRoutingGeminiNativeStateKey{}).(AIRoutingGeminiNativeStateCallbacks)
	return ok
}

// Native state retains either valid protobuf spelling; comparison uses the
// original part, so callers must not rename fields around a thought signature.
func GeminiNativeFunctionCall(part []byte) (name string, args json.RawMessage, id string, ok bool) {
	fc, valid := aiGeminiField(gjson.ParseBytes(part), "functionCall", "function_call")
	if !valid || !fc.IsObject() || !aiCanonicalFields(fc, "name", "args", "id") || fc.Get("name").Type != gjson.String || strings.TrimSpace(fc.Get("name").String()) == "" {
		return "", nil, "", false
	}
	fc.ForEach(func(key, _ gjson.Result) bool {
		if key.Str != "name" && key.Str != "args" && key.Str != "id" {
			valid = false
		}
		return valid
	})
	if !valid {
		return "", nil, "", false
	}
	value := fc.Get("args")
	if id := fc.Get("id"); id.Exists() && (id.Type != gjson.String || !aiResponsesValidID(id.Str)) {
		return "", nil, "", false
	}
	if value.Exists() && !value.IsObject() {
		return "", nil, "", false
	}
	args = json.RawMessage(value.Raw)
	if !value.Exists() {
		args = json.RawMessage(`{}`)
	}
	return fc.Get("name").String(), args, fc.Get("id").String(), true
}
