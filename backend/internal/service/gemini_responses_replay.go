package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestmodel"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type geminiResponsesReplay struct {
	calls   []*GeminiResponsesToolState
	outputs []*GeminiResponsesToolState
}

func prepareGeminiResponsesReplay(ctx context.Context, request *apicompat.ResponsesRequest, callbacks GeminiResponsesToolStateCallbacks) (*geminiResponsesReplay, error) {
	replay := &geminiResponsesReplay{}
	if !gjson.ParseBytes(request.Input).IsArray() {
		return replay, nil
	}
	var input []json.RawMessage
	if err := json.Unmarshal(request.Input, &input); err != nil {
		return nil, err
	}
	states := map[string]*GeminiResponsesToolState{}
	load := func(id string) (*GeminiResponsesToolState, error) {
		if !strings.HasPrefix(id, GeminiResponsesToolCallPrefix) {
			return nil, nil
		}
		if state := states[id]; state != nil {
			return state, nil
		}
		if callbacks.Load == nil || ctx.Err() != nil {
			return nil, geminiResponsesStateError()
		}
		state, err := callbacks.Load(ctx, id)
		if err != nil || !validGeminiResponsesToolState(state) {
			return nil, geminiResponsesStateError()
		}
		states[id] = &state
		return &state, nil
	}
	seen := map[string]bool{}
	lastIndex := map[string]int{}
	var normalized []json.RawMessage
	for index := 0; index < len(input); index++ {
		raw := input[index]
		item := gjson.ParseBytes(raw)
		kind, id := item.Get("type").String(), item.Get("call_id").String()
		switch kind {
		case "function_call":
			arguments := []byte(item.Get("arguments").String())
			if !gjson.ParseBytes(arguments).IsObject() || !requestmodel.HasUniqueJSONMembers(arguments) {
				return nil, errors.New("invalid function call arguments")
			}
			state, err := load(id)
			if err != nil {
				return nil, err
			}
			if state != nil && (state.Name != item.Get("name").String() || !geminiResponsesJSONEqual(state.Arguments, []byte(item.Get("arguments").String()))) {
				return nil, geminiResponsesStateError()
			}
			if state != nil && state.Batch != "" {
				if previous, ok := lastIndex[state.Batch]; ok && previous >= state.Index {
					return nil, geminiResponsesStateError()
				}
				lastIndex[state.Batch] = state.Index
			}
			if state == nil {
				plain := map[string]any{"functionCall": map[string]any{"name": item.Get("name").String(), "args": json.RawMessage(arguments)}}
				if _, ai := AIRoutingExecutionRequirementsFromContext(ctx); !ai {
					plain["thoughtSignature"] = geminiDummyThoughtSignature
				}
				part, err := json.Marshal(plain)
				if err != nil {
					return nil, err
				}
				state = &GeminiResponsesToolState{Name: item.Get("name").String(), Arguments: arguments, Part: part}
			}
			replay.calls = append(replay.calls, state)
			seen[id] = true
		case "function_call_output":
			// Reconstruct all calls before a consecutive result batch, retaining
			// the parallel assistant/user turn structure instead of alternating
			// one synthetic assistant call with each individual result.
			type outputItem struct {
				id    string
				raw   json.RawMessage
				state *GeminiResponsesToolState
				batch int
			}
			var outputs []outputItem
			batches := map[string]int{}
			end := index
			for end < len(input) && gjson.GetBytes(input[end], "type").String() == "function_call_output" {
				outputID := gjson.GetBytes(input[end], "call_id").String()
				state, err := load(outputID)
				if err != nil {
					return nil, err
				}
				batch := outputID
				if state != nil && state.Batch != "" {
					batch = state.Batch
				}
				rank, exists := batches[batch]
				if !exists {
					rank = len(batches)
					batches[batch] = rank
				}
				outputs = append(outputs, outputItem{id: outputID, raw: input[end], state: state, batch: rank})
				end++
			}
			sort.SliceStable(outputs, func(i, j int) bool {
				if outputs[i].batch != outputs[j].batch {
					return outputs[i].batch < outputs[j].batch
				}
				return outputs[i].state != nil && outputs[j].state != nil && outputs[i].state.Index < outputs[j].state.Index
			})
			for _, output := range outputs {
				state, outputID := output.state, output.id
				if !seen[outputID] {
					if state == nil {
						return nil, errors.New("gemini tool result requires its original function call")
					}
					call, err := json.Marshal(map[string]any{"type": "function_call", "call_id": outputID, "name": state.Name, "arguments": string(state.Arguments)})
					if err != nil {
						return nil, err
					}
					normalized = append(normalized, call)
					replay.calls = append(replay.calls, state)
					seen[outputID] = true
				}
				replay.outputs = append(replay.outputs, state)
			}
			for _, output := range outputs {
				normalized = append(normalized, output.raw)
			}
			index = end - 1
			continue
		}
		normalized = append(normalized, raw)
	}
	var err error
	request.Input, err = json.Marshal(normalized)
	return replay, err
}

func validGeminiResponsesToolState(state GeminiResponsesToolState) bool {
	if state.Name == "" || !gjson.ValidBytes(state.Part) || !gjson.ParseBytes(state.Arguments).IsObject() || state.Index < 0 {
		return false
	}
	name, arguments, _, ok := GeminiNativeFunctionCall(state.Part)
	return ok && name == state.Name && geminiResponsesJSONEqual(state.Arguments, arguments)
}

func geminiResponsesJSONEqual(a, b []byte) bool {
	var left, right any
	// UseNumber avoids conflating distinct large integers when validating a
	// client replay against a signature-bearing provider argument object.
	decode := func(raw []byte, out *any) error {
		if !gjson.ValidBytes(raw) || !requestmodel.HasUniqueJSONMembers(raw) {
			return errors.New("invalid JSON")
		}
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.UseNumber()
		return decoder.Decode(out)
	}
	return decode(a, &left) == nil && decode(b, &right) == nil && reflect.DeepEqual(left, right)
}

func (r *geminiResponsesReplay) apply(wire []byte, toolChoice json.RawMessage) ([]byte, error) {
	call, output := 0, 0
	for i, content := range gjson.GetBytes(wire, "contents").Array() {
		for j, part := range content.Get("parts").Array() {
			path := fmt.Sprintf("contents.%d.parts.%d", i, j)
			if part.Get("functionCall").Exists() {
				if call >= len(r.calls) {
					return nil, errors.New("gemini function call conversion mismatch")
				}
				if state := r.calls[call]; state != nil {
					var err error
					wire, err = sjson.SetRawBytes(wire, path, state.Part)
					if err != nil {
						return nil, err
					}
				}
				call++
			}
			if part.Get("functionResponse").Exists() {
				if output >= len(r.outputs) {
					return nil, errors.New("gemini function result conversion mismatch")
				}
				if state := r.outputs[output]; state != nil {
					if id := gjson.GetBytes(state.Part, "functionCall.id").String(); id != "" {
						var err error
						wire, err = sjson.SetBytes(wire, path+".functionResponse.id", id)
						if err != nil {
							return nil, err
						}
					}
				}
				output++
			}
		}
	}
	if call != len(r.calls) || output != len(r.outputs) {
		return nil, errors.New("gemini tool conversion lost history")
	}
	if len(toolChoice) == 0 || string(toolChoice) == "null" {
		return wire, nil
	}
	choice := gjson.ParseBytes(toolChoice)
	mode, name := "", ""
	if choice.Type == gjson.String {
		switch choice.String() {
		case "auto":
			mode = "AUTO"
		case "none":
			mode = "NONE"
		case "required":
			mode = "ANY"
		}
	} else if choice.Get("type").String() == "function" {
		mode, name = "ANY", choice.Get("name").String()
		if name == "" {
			return nil, errors.New("invalid forced function name")
		}
	}
	if mode == "" {
		return nil, errors.New("unsupported Gemini Responses tool_choice")
	}
	config := map[string]any{"mode": mode}
	if name != "" {
		config["allowedFunctionNames"] = []string{name}
	}
	return sjson.SetBytes(wire, "toolConfig.functionCallingConfig", config)
}
