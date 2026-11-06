//go:build unit

package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func aiResponsesTestBody(t *testing.T, input any, extra map[string]any) []byte {
	t.Helper()
	value := map[string]any{"model": "public-alias", "input": input, "store": false, "max_output_tokens": 512}
	for key, v := range extra {
		value[key] = v
	}
	body, err := json.Marshal(value)
	require.NoError(t, err)
	return body
}

func TestAIRoutingResponsesRequestDefaultsAndDisclosure(t *testing.T) {
	raw := []byte(`{"model":"any-alias","input":"visible task password=synthetic-secret","instructions":"private-instructions"}`)
	for _, mode := range []string{"metadata", "task_excerpt", "bounded_history"} {
		t.Run(mode, func(t *testing.T) {
			request, err := NewAIRoutingResponsesRequest(raw, mode)
			require.NoError(t, err)
			require.Equal(t, CompositeRouteEndpointResponses, request.Endpoint)
			require.Equal(t, "any-alias", request.PublicModel)
			require.True(t, request.StoreResponse)
			require.True(t, request.RequiresNativeResponses)
			require.True(t, request.RequiresStoredResponses)
			require.True(t, request.OutputLimitOmitted)
			require.Zero(t, request.MaxOutput)
			require.False(t, request.Background)
			require.False(t, request.RequiresOrigin)
			require.NotContains(t, request.Task, "private-instructions")
			require.NotContains(t, request.Task, "synthetic-secret")
			if mode == "task_excerpt" {
				require.Contains(t, request.Task, "visible task")
				require.Contains(t, request.Task, "[redacted]")
			} else {
				require.NotContains(t, request.Task, "visible task")
			}
			if mode == "bounded_history" {
				require.Equal(t, raw, request.HistoryBody)
				request.HistoryBody[0] = '!'
				require.Equal(t, byte('{'), raw[0])
			} else {
				require.Empty(t, request.HistoryBody)
			}
		})
	}
	body := aiResponsesTestBody(t, "task", map[string]any{"tools": []any{map[string]any{"type": "function", "name": "synthetic", "description": "private-description", "parameters": map[string]any{"Type": "private", "type": "string"}}}})
	r, err := NewAIRoutingResponsesRequest(body, "metadata")
	require.NoError(t, err)
	require.True(t, r.UsesTools)
	require.False(t, r.RequiresNativeResponses, "ordinary function tools retain existing equivalent bridge eligibility")
	require.False(t, r.RequiresStoredResponses)
	require.False(t, r.OutputLimitOmitted)
	require.Equal(t, 512, r.MaxOutput)
	require.NotContains(t, r.Task, "private-description")
}

func TestAIRoutingResponsesRequestPendingAndCompletedTools(t *testing.T) {
	call := map[string]any{"type": "function_call", "id": "fc_old", "call_id": "call_old", "name": "f", "arguments": `{"private":"arguments"}`}
	output := map[string]any{"type": "function_call_output", "call_id": "call_old", "output": "private-result"}
	for _, tc := range []struct {
		name   string
		suffix []any
		bound  bool
	}{
		{"pending result", nil, true},
		{"pending result plus instruction", []any{map[string]any{"role": "user", "content": "please continue"}}, true},
		{"assistant incomplete prefill", []any{map[string]any{"type": "message", "role": "assistant", "status": "in_progress", "content": "prefill"}}, true},
		{"closed history then new task", []any{map[string]any{"role": "assistant", "content": "finished"}, map[string]any{"role": "user", "content": "new task"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := append([]any{map[string]any{"role": "user", "content": "old task"}, call, output}, tc.suffix...)
			r, err := NewAIRoutingResponsesRequest(aiResponsesTestBody(t, input, nil), "task_excerpt")
			require.NoError(t, err)
			require.True(t, r.UsesTools)
			require.Equal(t, tc.bound, r.RequiresOrigin)
			require.False(t, r.RequiresNativeResponses)
			if tc.bound {
				require.Equal(t, []AIRoutingOriginRef{{Kind: "tool_call", ID: "call_old"}}, r.OriginRefs)
				require.Equal(t, []string{"call_old"}, r.ToolResults)
			} else {
				require.Empty(t, r.OriginRefs)
				require.Empty(t, r.ToolResults)
			}
			for _, secret := range []string{"private-result", "arguments", "call_old", "fc_old"} {
				require.NotContains(t, r.Task, secret)
			}
		})
	}
}

func TestAIRoutingResponsesRequestOriginKindsAndStorage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		input  any
		extra  map[string]any
		refs   []AIRoutingOriginRef
		stored bool
	}{
		{"previous", "continue", map[string]any{"previous_response_id": "resp_original"}, []AIRoutingOriginRef{{Kind: "response", ID: "resp_original", RequiresStored: true}}, true},
		{"item reference", []any{map[string]any{"type": "item_reference", "id": "msg_original"}}, nil, []AIRoutingOriginRef{{Kind: "item", ID: "msg_original", RequiresStored: true}}, true},
		{"conversation", "continue", map[string]any{"conversation": map[string]any{"id": "conv_original"}}, []AIRoutingOriginRef{{Kind: "conversation", ID: "conv_original", RequiresStored: true}}, true},
		{"inline encrypted with id", []any{map[string]any{"type": "reasoning", "id": "rs_original", "encrypted_content": "private-cipher", "summary": []any{map[string]any{"type": "summary_text", "text": "private-summary"}}}}, nil, []AIRoutingOriginRef{{Kind: "item", ID: "rs_original"}}, false},
		{"inline encrypted without id", []any{map[string]any{"type": "reasoning", "encrypted_content": "private-cipher"}}, nil, []AIRoutingOriginRef{{Kind: "opaque", ID: AIRoutingOpaqueOriginID("private-cipher")}}, false},
		{"server tool", []any{map[string]any{"type": "web_search_call", "id": "ws_original", "action": map[string]any{"query": "private-query"}}}, nil, []AIRoutingOriginRef{{Kind: "item", ID: "ws_original"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewAIRoutingResponsesRequest(aiResponsesTestBody(t, tc.input, tc.extra), "task_excerpt")
			require.NoError(t, err)
			require.True(t, r.RequiresOrigin)
			require.True(t, r.RequiresNativeResponses)
			require.Equal(t, tc.stored, r.RequiresStoredResponses)
			require.Equal(t, tc.refs, r.OriginRefs)
			for _, secret := range []string{"private-cipher", "private-summary", "private-query", "resp_original", "rs_original", "conv_original", "msg_original"} {
				require.NotContains(t, r.Task, secret)
			}
		})
	}
	require.Equal(t, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", AIRoutingOpaqueOriginID("abc"))
	r, err := NewAIRoutingResponsesRequest([]byte(`{"model":"x","store":false,"previous_response_id":"resp_only"}`), "metadata")
	require.NoError(t, err)
	require.True(t, r.RequiresOrigin)
	r, err = NewAIRoutingResponsesRequest(aiResponsesTestBody(t, "work", map[string]any{"background": true}), "metadata")
	require.NoError(t, err)
	require.True(t, r.Background)
	require.True(t, r.RequiresStoredResponses)
}

func TestAIRoutingResponsesRequestRejectsAmbiguityAndConflicts(t *testing.T) {
	for _, raw := range []string{
		`{"model":"x","input":"a","input":"b"}`,
		`{"model":"x","Input":"a"}`,
		`{"model":"x","input":"a","Store":false}`,
		`{"model":"x","input":"a","store":null}`,
		`{"model":"x","input":"a","background":1}`,
		`{"model":"x","input":"a","max_output_tokens":1.5}`,
		`{"model":"x","input":"a","max_output_tokens":0}`,
		`{"model":"x","input":"a","max_output_tokens":9223372036854775808}`,
		`{"model":"x","input":"a","messages":[]}`,
		`{"model":"x","input":"a","prompt":{"id":"prompt_unknown"}}`,
		`{"model":"x","input":"a","max_tokens":100}`,
		`{"model":"x","input":[{"role":"user","content":[{"type":"input_text","Text":"secret"}]}]}`,
		`{"model":"x","input":[{"role":"user","content":[{"type":"input_text","text":7}]}]}`,
		`{"model":"x","input":[{"role":"tool","content":"result"}]}`,
		`{"model":"x","input":[{"type":"unknown","text":"not a task"}]}`,
		`{"model":"x","input":[{"type":"function_call_output","Call_id":"call_x","output":"a"}]}`,
		`{"model":"x","input":[{"type":"function_call_output","id":"fc_x","output":"a"}]}`,
		`{"model":"x","input":[{"type":"function_call_output","call_id":"call_x","output":"a"},{"type":"function_call_output","call_id":"call_x","output":"b"}]}`,
		`{"model":"x","input":[{"type":"function_call","call_id":"call_x","name":"f","arguments":"{}"},{"type":"custom_tool_call_output","call_id":"call_x","output":"a"}]}`,
		`{"model":"x","input":[{"role":"user","id":"msg_x","content":"a"},{"role":"user","id":"msg_x","content":"b"}]}`,
		`{"model":"x","input":[],"previous_response_id":"resp_x","conversation":"conv_x"}`,
		`{"model":"x","input":[],"conversation":{"ID":"conv_x"}}`,
		`{"model":"x","input":[{"type":"reasoning","encrypted_content":7}]}`,
		`{"model":"x","input":[{"type":"compaction"}]}`,
		`{"model":"x","input":[{"type":"web_search_call","action":{}}]}`,
		`{"model":"x","input":[{"role":"user","content":[{"type":"input_file","file_id":"file_unknown"}]}],"previous_response_id":"resp_other"}`,
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := NewAIRoutingResponsesRequest([]byte(raw), "bounded_history")
			require.Error(t, err)
		})
	}
	_, err := NewAIRoutingResponsesRequest([]byte("{\"model\":\"x\",\"input\":\"\xff\"}"), "metadata")
	require.Error(t, err)
	items := make([]any, 65)
	for i := range items {
		items[i] = map[string]any{"type": "item_reference", "id": strings.Repeat("x", i+1)}
	}
	_, err = NewAIRoutingResponsesRequest(aiResponsesTestBody(t, items, nil), "metadata")
	require.Error(t, err, "origin lookup fan-out is bounded")
	for _, size := range []int{256, 257} {
		body := aiResponsesTestBody(t, "continue", map[string]any{"previous_response_id": strings.Repeat("r", size)})
		_, err = NewAIRoutingResponsesRequest(body, "metadata")
		if size == 256 {
			require.NoError(t, err)
		} else {
			require.Error(t, err, "the parser and origin writer share the same ID bound")
		}
	}
}

func TestAIRoutingResponsesRequestToolMediaIsNotTaskText(t *testing.T) {
	body := aiResponsesTestBody(t, []any{
		map[string]any{"type": "function_call_output", "call_id": "call_x", "output": []any{map[string]any{"type": "input_text", "text": "private-result"}, map[string]any{"type": "input_image", "image_url": "https://synthetic.invalid/private"}}},
		map[string]any{"role": "user", "content": "please continue"},
	}, nil)
	r, err := NewAIRoutingResponsesRequest(body, "task_excerpt")
	require.NoError(t, err)
	require.True(t, r.HasImages)
	require.Contains(t, r.InputModalities, "image")
	require.Contains(t, r.Task, "please continue")
	require.NotContains(t, r.Task, "private-result")
	require.NotContains(t, r.Task, "synthetic.invalid")
}

func TestAIRoutingResponsesRequestStoredOriginIsPerReference(t *testing.T) {
	opaque := map[string]any{"type": "reasoning", "id": "rs_shared", "encrypted_content": "synthetic-inline-cipher"}
	reference := map[string]any{"type": "item_reference", "id": "rs_shared"}
	request, err := NewAIRoutingResponsesRequest(aiResponsesTestBody(t, []any{opaque}, map[string]any{"store": true}), "metadata")
	require.NoError(t, err)
	require.True(t, request.RequiresStoredResponses, "storage of this newly created response is required")
	require.Equal(t, []AIRoutingOriginRef{{Kind: "item", ID: "rs_shared"}}, request.OriginRefs, "inline encrypted replay does not require storage of the old response")
	for _, items := range [][]any{{opaque, reference}, {reference, opaque}} {
		request, err = NewAIRoutingResponsesRequest(aiResponsesTestBody(t, items, nil), "metadata")
		require.NoError(t, err)
		require.Len(t, request.OriginRefs, 1, "different uses of one ID must not split the lookup")
		require.Equal(t, AIRoutingOriginRef{Kind: "item", ID: "rs_shared", RequiresStored: true}, request.OriginRefs[0])
		require.True(t, request.RequiresStoredResponses)
	}
}

func TestAIRoutingResponsesRequestCompactionTriggerIsNativeMetadata(t *testing.T) {
	request, err := NewAIRoutingResponsesRequest([]byte(`{"model":"test","store":false,"input":[{"role":"user","content":"visible request"},{"type":"compaction_trigger"}]}`), "task_excerpt")
	require.NoError(t, err)
	require.True(t, request.RequiresNativeResponses)
	require.False(t, request.RequiresStoredResponses)
	require.False(t, request.RequiresOrigin)
	require.False(t, request.UsesTools)
	require.Empty(t, request.OriginRefs)
	require.Contains(t, request.Task, "visible request")
	require.NotContains(t, request.Task, "compaction_trigger")
}
