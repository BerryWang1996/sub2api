//go:build unit

package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func aiChatTestBody(t *testing.T, messages []any, extra map[string]any) []byte {
	t.Helper()
	value := map[string]any{"model": "public-alias", "messages": messages, "max_completion_tokens": 512}
	for key, v := range extra {
		value[key] = v
	}
	body, err := json.Marshal(value)
	require.NoError(t, err)
	return body
}

func aiChatTestCall(id, arguments string) map[string]any {
	return map[string]any{"id": id, "type": "function", "function": map[string]any{"name": "same_function", "arguments": arguments}}
}

func TestAIRoutingChatRequestDisclosureAndNativeBody(t *testing.T) {
	body := aiChatTestBody(t, []any{
		map[string]any{"role": "system", "content": "private-system"},
		map[string]any{"role": "developer", "content": "private-developer"},
		map[string]any{"role": "user", "content": "earlier-task"},
		map[string]any{"role": "assistant", "content": "earlier-answer", "reasoning_content": "private-thinking"},
		map[string]any{"role": "user", "content": "current-task password=synthetic-password"},
	}, map[string]any{"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "description": "private-schema", "parameters": map[string]any{"type": "object", "properties": map[string]any{"Type": map[string]any{"type": "string"}}}}}}})
	for _, mode := range []string{"metadata", "task_excerpt", "bounded_history"} {
		t.Run(mode, func(t *testing.T) {
			r, err := NewAIRoutingChatRequest(body, mode)
			require.NoError(t, err)
			require.Equal(t, CompositeRouteEndpointChatCompletions, r.ClientEndpoint())
			require.Equal(t, "public-alias", r.PublicModel)
			require.Equal(t, len(body), r.InputBytes)
			require.Equal(t, 512, r.MaxOutput)
			require.False(t, r.OutputLimitOmitted)
			require.True(t, r.UsesTools)
			require.Equal(t, []string{"text"}, r.InputModalities)
			require.False(t, r.RequiresNativeResponses)
			require.False(t, r.RequiresNativeChat)
			require.False(t, r.RequiresStoredResponses)
			require.False(t, r.RequiresOrigin)
			for _, secret := range []string{"private-system", "private-developer", "private-thinking", "private-schema", "synthetic-password", "earlier-task", "earlier-answer"} {
				require.NotContains(t, r.Task, secret)
			}
			if mode == "task_excerpt" {
				require.Contains(t, r.Task, "current-task")
				require.Contains(t, r.Task, "[redacted]")
			} else {
				require.NotContains(t, r.Task, "current-task")
			}
			if mode == "bounded_history" {
				require.Equal(t, body, r.HistoryBody)
				r.HistoryBody[0] = '!'
				require.Equal(t, byte('{'), body[0])
			} else {
				require.Empty(t, r.HistoryBody)
			}
		})
	}
}

func TestAIRoutingChatRequestOutputLimits(t *testing.T) {
	for _, tc := range []struct {
		field string
		want  int
	}{{``, 0}, {`,"max_tokens":17`, 17}, {`,"max_completion_tokens":19`, 19}, {`,"max_completion_tokens":null`, 0}, {`,"max_tokens":null`, 0}} {
		r, err := NewAIRoutingChatRequest([]byte(`{"model":"x","messages":[{"role":"user","content":"task"}]`+tc.field+`}`), "metadata")
		require.NoError(t, err)
		require.Equal(t, tc.want, r.MaxOutput)
		require.Equal(t, tc.want == 0, r.OutputLimitOmitted)
	}
	for _, n := range []int{1, 2, 3} {
		r, err := NewAIRoutingChatRequest(aiChatTestBody(t, []any{map[string]any{"role": "user", "content": "task"}}, map[string]any{"n": n}), "metadata")
		require.NoError(t, err)
		require.Equal(t, n > 1, r.RequiresNativeChat)
	}
}

func TestAIRoutingChatRequestStrictToolOriginsAndClosedHistory(t *testing.T) {
	calls := map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{aiChatTestCall("call_A", `{"a":1}`), aiChatTestCall("call_B", `{"a":2}`)}}
	outputs := []any{map[string]any{"role": "tool", "tool_call_id": "call_B", "content": "private-result-B"}, map[string]any{"role": "tool", "tool_call_id": "call_A", "content": []any{map[string]any{"type": "text", "text": "private-result-A"}}}}
	for _, tc := range []struct {
		name   string
		suffix []any
		bound  bool
	}{
		{"pending parallel same-name tools", nil, true},
		{"pending plus user instruction", []any{map[string]any{"role": "user", "content": "continue"}}, true},
		{"hidden thinking does not close tools", []any{map[string]any{"role": "assistant", "content": nil, "reasoning_content": "private-thinking"}}, true},
		{"closed history", []any{map[string]any{"role": "assistant", "content": "completed"}, map[string]any{"role": "user", "content": "new task"}}, false},
		{"new assistant action consumes prior results", []any{map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{aiChatTestCall("call_C", "{}")}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			messages := append([]any{map[string]any{"role": "user", "content": "earlier task"}, calls}, outputs...)
			messages = append(messages, tc.suffix...)
			r, err := NewAIRoutingChatRequest(aiChatTestBody(t, messages, nil), "task_excerpt")
			require.NoError(t, err)
			require.True(t, r.UsesTools)
			require.Equal(t, tc.bound, r.RequiresOrigin)
			if tc.bound {
				require.Equal(t, []string{"call_B", "call_A"}, r.ToolResults)
				require.Equal(t, []AIRoutingOriginRef{{Kind: "tool_call", ID: "call_B"}, {Kind: "tool_call", ID: "call_A"}}, r.OriginRefs)
			} else {
				require.Empty(t, r.ToolResults)
				require.Empty(t, r.OriginRefs)
			}
			for _, secret := range []string{"call_A", "call_B", "same_function", "private-result", "private-thinking"} {
				require.NotContains(t, r.Task, secret)
			}
		})
	}
	r, err := NewAIRoutingChatRequest(aiChatTestBody(t, []any{outputs[0]}, nil), "metadata")
	require.NoError(t, err, "a truncated history still requires the durable strict-Key origin; local definitions never grant trust")
	require.True(t, r.RequiresOrigin)
	require.Equal(t, []string{"call_B"}, r.ToolResults)
}

func TestAIRoutingChatRequestAttachmentsAreCapabilitiesNotEvidence(t *testing.T) {
	body := aiChatTestBody(t, []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "visible task"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://synthetic.invalid/private-image"}},
		map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": "private-audio", "format": "wav"}},
		map[string]any{"type": "file", "file": map[string]any{"file_data": "private-file", "filename": "private-filename"}},
	}}}, nil)
	r, err := NewAIRoutingChatRequest(body, "task_excerpt")
	require.NoError(t, err)
	require.True(t, r.HasImages)
	require.True(t, r.RequiresNativeChat)
	require.Equal(t, []string{"text", "image", "audio", "document"}, r.InputModalities)
	require.Contains(t, r.Task, "visible task")
	require.NotContains(t, r.Task, "private-")
}

func TestAIRoutingChatRequestRejectsAmbiguousAndUnsupportedInputs(t *testing.T) {
	for _, raw := range []string{
		`{"model":"x","messages":[],"Messages":[]}`,
		`{"model":"x","messages":[{"role":"user","content":"a","content":"b"}]}`,
		`{"model":"x","messages":[{"role":"user","Content":"ambiguous"}]}`,
		`{"model":"x","messages":[]}`,
		`{"model":"x","messages":[{"role":"function","name":"f","content":"result"}]}`,
		`{"model":"x","messages":[{"role":"assistant","function_call":{"name":"f","arguments":"{}"}}]}`,
		`{"model":"x","messages":[{"role":"tool","name":"f","content":"result"}]}`,
		`{"model":"x","messages":[{"role":"tool","tool_call_id":" call_x","content":"result"}]}`,
		`{"model":"x","messages":[{"role":"tool","tool_call_id":"call_x","content":"a"},{"role":"tool","tool_call_id":"call_x","content":"b"}]}`,
		`{"model":"x","messages":[{"role":"user","tool_call_id":"call_x","content":"task"}]}`,
		`{"model":"x","messages":[{"role":"assistant","content":null}]}`,
		`{"model":"x","messages":[{"role":"assistant","content":"incomplete","prefix":true}]}`,
		`{"model":"x","messages":[{"role":"assistant","content":"incomplete","status":"in_progress"}]}`,
		`{"model":"x","messages":[{"role":"assistant","content":"ok","audio":{"id":"audio_owned"}}]}`,
		`{"model":"x","messages":[{"role":"assistant","content":"ok","reasoning_details":[{"type":"encrypted","data":"secret"}]}]}`,
		`{"model":"x","messages":[{"role":"user","content":[{"type":"input_text","text":"wrong protocol"}]}]}`,
		`{"model":"x","messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"file_owned"}}]}]}`,
		`{"model":"x","messages":[{"role":"user","content":[{"type":"new_type","text":"not guessed"}]}]}`,
		`{"model":"x","messages":[{"role":"user","content":[{"type":"image_url","image_url":"not canonical"}]}]}`,
	} {
		t.Run(fmt.Sprintf("input-%d", len(raw))+raw[:min(len(raw), 36)], func(t *testing.T) {
			_, err := NewAIRoutingChatRequest([]byte(raw), "metadata")
			require.Error(t, err)
			require.NotContains(t, err.Error(), "secret")
		})
	}
	for _, extra := range []string{
		`,"input":"other history"`, `,"max_output_tokens":1`, `,"previous_response_id":"resp_x"`, `,"conversation":"conv_x"`,
		`,"max_tokens":1,"max_completion_tokens":2`, `,"max_tokens":null,"max_completion_tokens":2`,
		`,"max_tokens":0`, `,"max_tokens":-1`, `,"max_tokens":1.5`, `,"max_tokens":1e3`, `,"max_tokens":9223372036854775808`, `,"max_tokens":"1"`,
		`,"stream":"true"`, `,"n":0`, `,"n":1.5`, `,"modalities":["audio"]`,
		`,"functions":[{"name":"legacy"}]`, `,"tools":[{"type":"custom","custom":{"name":"unsafe"}}]`,
		`,"tool_choice":{"type":"custom","custom":{"name":"unsafe"}}`, `,"tool_choice":"unknown"`,
	} {
		_, err := NewAIRoutingChatRequest([]byte(`{"model":"x","messages":[{"role":"user","content":"task"}]`+extra+`}`), "metadata")
		require.Error(t, err, extra)
	}
	_, err := NewAIRoutingChatRequest(append([]byte(`{"model":"x","messages":[{"role":"user","content":"`), []byte{0xff, '"', '}', ']', '}'}...), "metadata")
	require.Error(t, err)
	_, err = NewAIRoutingChatRequest([]byte(`{"model":"x","messages":[{"role":"tool","tool_call_id":"`+strings.Repeat("x", 257)+`","content":"task"}]}`), "metadata")
	require.Error(t, err)
}

func TestAIRoutingChatRequestConflictingCallsAndOriginLimit(t *testing.T) {
	_, err := NewAIRoutingChatRequest(aiChatTestBody(t, []any{map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{aiChatTestCall("same_id", "one"), aiChatTestCall("same_id", "two")}}}, nil), "metadata")
	require.Error(t, err)
	var messages []any
	for i := range 65 {
		messages = append(messages, map[string]any{"role": "tool", "tool_call_id": fmt.Sprintf("call_%d", i), "content": "private-result"})
	}
	_, err = NewAIRoutingChatRequest(aiChatTestBody(t, messages, nil), "metadata")
	require.Error(t, err)
	r, err := NewAIRoutingChatRequest(aiChatTestBody(t, messages[:64], nil), "metadata")
	require.NoError(t, err)
	require.Len(t, r.OriginRefs, 64)
}

func TestAIRoutingChatRequestPreservesNonPublicOptionsOnNativeWire(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options map[string]any
		native  bool
	}{
		{"plain", nil, false},
		{"temperature", map[string]any{"temperature": 0.2, "top_p": 0.8}, false},
		{"usage", map[string]any{"stream_options": map[string]any{"include_usage": true}}, false},
		{"store false", map[string]any{"store": false}, false},
		{"store true", map[string]any{"store": true}, true},
		{"structured format", map[string]any{"response_format": map[string]any{"type": "json_object"}}, true},
		{"prediction", map[string]any{"prediction": map[string]any{"type": "content", "content": "old document"}}, true},
		{"provider search", map[string]any{"web_search_options": map[string]any{}}, true},
		{"new extension", map[string]any{"provider_extension": "preserve"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := aiChatTestBody(t, []any{map[string]any{"role": "user", "content": "task"}}, tc.options)
			r, err := NewAIRoutingChatRequest(body, "bounded_history")
			require.NoError(t, err)
			require.Equal(t, tc.native, r.RequiresNativeChat)
			require.Equal(t, body, r.HistoryBody)
		})
	}
}

func TestAIRoutingChatSignedHistoryAndStrictFunctions(t *testing.T) {
	for _, strict := range []any{false, true, "invalid"} {
		body := aiChatTestBody(t, []any{map[string]any{"role": "user", "content": "task"}}, map[string]any{"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "f", "strict": strict, "parameters": map[string]any{"type": "object"}}}}})
		r, err := NewAIRoutingChatRequest(body, "metadata")
		require.NoError(t, err)
		require.Equal(t, strict != false, r.RequiresNativeChat)
	}
	for _, choice := range []any{
		map[string]any{"type": "function", "function": map[string]any{"name": "f"}, "provider_extension": "keep"},
		map[string]any{"type": "function", "function": map[string]any{"name": "f", "strict": false}},
	} {
		r, err := NewAIRoutingChatRequest(aiChatTestBody(t, []any{map[string]any{"role": "user", "content": "task"}}, map[string]any{"tool_choice": choice}), "metadata")
		require.NoError(t, err)
		require.True(t, r.RequiresNativeChat, "additional tool choice semantics must reach the native provider")
	}
	body := aiChatTestBody(t, []any{
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{aiChatTestCall(GeminiResponsesToolCallPrefix+"private", "f")}},
		map[string]any{"role": "tool", "tool_call_id": GeminiResponsesToolCallPrefix + "private", "content": "result"},
		map[string]any{"role": "assistant", "content": "finished"},
		map[string]any{"role": "user", "content": "continue"},
	}, nil)
	r, err := NewAIRoutingChatRequest(body, "metadata")
	require.NoError(t, err)
	require.True(t, r.RequiresOrigin)
	require.Equal(t, []string{GeminiResponsesToolCallPrefix + "private"}, r.ToolResults)
	var many []any
	for i := range 65 {
		id := fmt.Sprintf("%s%d", GeminiResponsesToolCallPrefix, i)
		many = append(many, map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{aiChatTestCall(id, "f")}}, map[string]any{"role": "tool", "tool_call_id": id, "content": "result"}, map[string]any{"role": "assistant", "content": "done"})
	}
	_, err = NewAIRoutingChatRequest(aiChatTestBody(t, many, nil), "metadata")
	require.Error(t, err, "private state limit also applies to already closed calls")
}
