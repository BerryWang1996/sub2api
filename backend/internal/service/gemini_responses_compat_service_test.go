package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func geminiResponsesTestService(body string) (*GeminiMessagesCompatService, *geminiCompatHTTPUpstreamStub, *Account) {
	upstream := &geminiCompatHTTPUpstreamStub{response: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Request-Id": {"gemini-bridge-test"}}, Body: io.NopCloser(strings.NewReader(body))}}
	return &GeminiMessagesCompatService{cfg: &config.Config{}, httpUpstream: upstream}, upstream,
		&Account{ID: 123, Platform: PlatformGemini, Type: AccountTypeAPIKey, Concurrency: 1, Credentials: map[string]any{"api_key": "synthetic-key"}}
}

func geminiResponsesTestCall(t *testing.T, svc *GeminiMessagesCompatService, account *Account, ctx context.Context, body string) (*ForwardResult, *httptest.ResponseRecorder, error) {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)).WithContext(ctx)
	result, err := svc.ForwardAsResponses(ctx, c, account, []byte(body), nil)
	return result, recorder, err
}

func geminiResponsesCompleted(body string, stream bool) gjson.Result {
	if !stream {
		return gjson.Parse(body)
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data:") {
			event := gjson.Parse(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			if event.Get("type").String() == "response.completed" || event.Get("type").String() == "response.incomplete" {
				return event.Get("response")
			}
		}
	}
	return gjson.Result{}
}

func TestGeminiResponsesRejectsAmbiguousNativeOutputBeforeBinding(t *testing.T) {
	for _, candidate := range []string{
		`{"finishReason":"STOP","FinishReason":"MAX_TOKENS"}`,
		`{"finishReason":null}`,
		`{"index":1,"finishReason":"STOP"}`,
		`{"content":{"parts":[{"functionCall":{"name":"f","args":{},"partialArgs":[{}]},"thoughtSignature":"private"}]},"finishReason":"STOP"}`,
		`{"content":{"parts":[{"functionCall":{"name":"f","args":{}},"ThoughtSignature":"private"}]},"finishReason":"STOP"}`,
	} {
		saved := 0
		callbacks := GeminiResponsesToolStateCallbacks{Save: func(context.Context, string, GeminiResponsesToolState) error { saved++; return nil }}
		_, _, _, err := prepareGeminiResponsesChunk(t.Context(), []byte(`{"candidates":[`+candidate+`]}`), callbacks, &geminiResponsesToolSequence{nativeIDs: map[string]bool{}})
		require.Error(t, err, candidate)
		require.Zero(t, saved)
	}
}

func TestGeminiForwardAsResponsesTextAndSameNameTools(t *testing.T) {
	gin.SetMode(gin.TestMode)
	raw := `{"responseId":"gemini_response","modelVersion":"gemini-2.5-flash","candidates":[{"content":{"parts":[{"text":"checking"},{"functionCall":{"name":"lookup","args":{"city":"Paris"},"id":"native_1"}},{"functionCall":{"name":"lookup","args":{"city":"Tokyo"},"id":"native_2"}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":100,"cachedContentTokenCount":20,"candidatesTokenCount":10,"thoughtsTokenCount":3}}`
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			response := raw
			if stream {
				response = "data: " + raw + "\n\n"
			}
			svc, upstream, account := geminiResponsesTestService(response)
			account.Credentials["model_mapping"] = map[string]any{"public-test": "gemini-2.5-flash"}
			body := fmt.Sprintf(`{"model":"public-test","store":false,"stream":%t,"instructions":"system-text","input":"latest-text","max_output_tokens":123,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}],"tool_choice":{"type":"function","name":"lookup"}}`, stream)
			result, recorder, err := geminiResponsesTestCall(t, svc, account, t.Context(), body)
			require.NoError(t, err, recorder.Body.String())
			require.Equal(t, "public-test", result.Model)
			require.Equal(t, "gemini-2.5-flash", result.UpstreamModel)
			require.Equal(t, 80, result.Usage.InputTokens)
			require.Equal(t, 20, result.Usage.CacheReadInputTokens)
			require.Equal(t, 13, result.Usage.OutputTokens)
			require.Equal(t, 1, upstream.calls)
			require.Contains(t, upstream.lastReq.URL.Path, "models/gemini-2.5-flash:")
			require.Equal(t, "synthetic-key", upstream.lastReq.Header.Get("x-goog-api-key"))
			wire, err := io.ReadAll(upstream.lastReq.Body)
			require.NoError(t, err)
			require.Equal(t, "ANY", gjson.GetBytes(wire, "toolConfig.functionCallingConfig.mode").String())
			require.Equal(t, "lookup", gjson.GetBytes(wire, "toolConfig.functionCallingConfig.allowedFunctionNames.0").String())
			require.Contains(t, string(wire), "system-text")
			require.Contains(t, string(wire), "latest-text")
			completed := geminiResponsesCompleted(recorder.Body.String(), stream)
			require.Equal(t, "completed", completed.Get("status").String())
			require.Equal(t, "public-test", completed.Get("model").String())
			require.EqualValues(t, 100, completed.Get("usage.input_tokens").Int())
			require.EqualValues(t, 13, completed.Get("usage.output_tokens").Int())
			var calls []gjson.Result
			for _, item := range completed.Get("output").Array() {
				if item.Get("type").String() == "function_call" {
					calls = append(calls, item)
				}
			}
			require.Len(t, calls, 2, recorder.Body.String())
			require.NotEqual(t, calls[0].Get("call_id").String(), calls[1].Get("call_id").String())
			require.JSONEq(t, `{"city":"Paris"}`, calls[0].Get("arguments").String())
			require.JSONEq(t, `{"city":"Tokyo"}`, calls[1].Get("arguments").String())
		})
	}
}

func TestGeminiForwardAsResponsesSignedToolsRestoreNativeParts(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			raw := `{"candidates":[{"content":{"parts":[{"thoughtSignature":"private-native-signature","functionCall":{"name":"lookup","args":{"city":"Paris"},"id":"provider_call_1"}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3}}`
			if stream {
				raw = "data: " + raw + "\n\n"
			}
			svc, upstream, account := geminiResponsesTestService(raw)
			states := map[string]GeminiResponsesToolState{}
			ctx := WithGeminiResponsesToolState(t.Context(), GeminiResponsesToolStateCallbacks{
				Save: func(_ context.Context, id string, state GeminiResponsesToolState) error {
					states[id] = state
					return nil
				},
				Load: func(_ context.Context, id string) (GeminiResponsesToolState, error) {
					state, exists := states[id]
					if !exists {
						return state, errors.New("unknown state")
					}
					return state, nil
				},
			})
			_, recorder, err := geminiResponsesTestCall(t, svc, account, ctx, fmt.Sprintf(`{"model":"gemini-2.5-flash","store":false,"stream":%t,"input":"lookup","max_output_tokens":100}`, stream))
			require.NoError(t, err, recorder.Body.String())
			require.Len(t, states, 1)
			require.NotContains(t, recorder.Body.String(), "private-native-signature")
			item := geminiResponsesCompleted(recorder.Body.String(), stream).Get("output.0")
			id := item.Get("call_id").String()
			require.True(t, strings.HasPrefix(id, GeminiResponsesToolCallPrefix))
			require.Contains(t, states, id)
			upstream.response.Body = io.NopCloser(strings.NewReader(`{"candidates":[{"content":{"parts":[{"text":"done"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2}}`))
			// Output-only continuations reconstruct the signed native function
			// call; the actual execution body is not the history projection.
			body := fmt.Sprintf(`{"model":"gemini-2.5-flash","store":false,"input":[{"type":"function_call_output","call_id":%q,"output":"sunny"}],"max_output_tokens":100}`, id)
			_, recorder, err = geminiResponsesTestCall(t, svc, account, ctx, body)
			require.NoError(t, err, recorder.Body.String())
			wire, err := io.ReadAll(upstream.lastReq.Body)
			require.NoError(t, err)
			require.Contains(t, string(wire), `"thoughtSignature":"private-native-signature"`)
			require.Contains(t, string(wire), `"id":"provider_call_1"`)
			require.Equal(t, "provider_call_1", gjson.GetBytes(wire, "contents.1.parts.0.functionResponse.id").String())
			require.NotContains(t, string(wire), geminiDummyThoughtSignature)
		})
	}
}

func TestGeminiForwardAsResponsesFailsClosedBeforeNativeStateLoss(t *testing.T) {
	for _, field := range []string{`"store":true`, `"previous_response_id":"resp_other","store":false`, `"background":true,"store":false`, `"store":false,"input":[{"type":"item_reference","id":"item_other"}]`, `"store":false,"input":[{"type":"reasoning","encrypted_content":"opaque"}]`, `"store":false,"tools":[{"type":"web_search"}]`, `"store":false,"cachedContent":"cachedContents/other"`, `"store":false,"text":{"format":{"type":"json_schema"}}`} {
		t.Run(field, func(t *testing.T) {
			svc, upstream, account := geminiResponsesTestService(`{}`)
			input := `,"input":"hello"`
			if strings.Contains(field, `"input":`) {
				input = ""
			}
			_, recorder, err := geminiResponsesTestCall(t, svc, account, t.Context(), `{"model":"gemini-2.5-flash",`+field+input+`}`)
			require.Error(t, err)
			require.Equal(t, http.StatusBadRequest, recorder.Code)
			require.Zero(t, upstream.calls)
		})
	}
}

func TestGeminiForwardAsResponsesIncompleteStreamNeverCompletesOrRetries(t *testing.T) {
	for _, suffix := range []string{"", "data: [DONE]\n\n", "data: invalid\n\n", "data: {\"candidates\":[{\"finishReason\":\"SAFETY\"}]}\n\n"} {
		svc, upstream, account := geminiResponsesTestService("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]}}],\"usageMetadata\":{\"promptTokenCount\":8,\"candidatesTokenCount\":2}}\n\n" + suffix)
		result, recorder, err := geminiResponsesTestCall(t, svc, account, t.Context(), `{"model":"gemini-2.5-flash","store":false,"stream":true,"input":"hello","max_output_tokens":100}`)
		require.Error(t, err)
		require.NotNil(t, result)
		require.Equal(t, 8, result.Usage.InputTokens)
		require.Contains(t, recorder.Body.String(), "partial")
		require.NotContains(t, recorder.Body.String(), "response.completed")
		require.NotContains(t, recorder.Body.String(), "data: [DONE]")
		require.Equal(t, 1, upstream.calls)
	}
}

func TestGeminiForwardAsResponsesStateFailureCannotExposeCallID(t *testing.T) {
	for _, callbacks := range []GeminiResponsesToolStateCallbacks{{}, {Save: func(context.Context, string, GeminiResponsesToolState) error { return errors.New("store unavailable") }}} {
		svc, upstream, account := geminiResponsesTestService(`{"candidates":[{"content":{"parts":[{"thoughtSignature":"private","functionCall":{"name":"lookup","args":{}}}]},"finishReason":"STOP"}]}`)
		ctx := WithGeminiResponsesToolState(t.Context(), callbacks)
		_, recorder, err := geminiResponsesTestCall(t, svc, account, ctx, `{"model":"gemini-2.5-flash","store":false,"input":"hello"}`)
		require.True(t, IsAIRoutingCapacityError(err))
		require.NotContains(t, recorder.Body.String(), "call_id")
		require.NotContains(t, recorder.Body.String(), "private")
		require.Equal(t, 1, upstream.calls)
	}
}

func TestGeminiResponsesReplayRejectsMutationAndKeepsClosedHistoryOrigin(t *testing.T) {
	id := GeminiResponsesToolCallPrefix + "test"
	state := GeminiResponsesToolState{Name: "lookup", Arguments: json.RawMessage(`{"n":9007199254740993}`), Part: json.RawMessage(`{"thoughtSignature":"private","functionCall":{"name":"lookup","args":{"n":9007199254740993}}}`)}
	ctx := WithGeminiResponsesToolState(t.Context(), GeminiResponsesToolStateCallbacks{Load: func(context.Context, string) (GeminiResponsesToolState, error) { return state, nil }})
	for _, arg := range []string{`{"n":9007199254740992}`, `{"n":7}`} {
		svc, upstream, account := geminiResponsesTestService(`{}`)
		body, err := json.Marshal(map[string]any{"model": "gemini-2.5-flash", "store": false, "input": []any{map[string]any{"type": "function_call", "call_id": id, "name": "lookup", "arguments": arg}}})
		require.NoError(t, err)
		_, _, err = geminiResponsesTestCall(t, svc, account, ctx, string(body))
		require.True(t, IsAIRoutingCapacityError(err))
		require.Zero(t, upstream.calls)
	}
	body := []byte(fmt.Sprintf(`{"model":"gemini-2.5-flash","store":false,"input":[{"type":"function_call","call_id":%q,"name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":%q,"output":"result"},{"role":"assistant","content":"done"},{"role":"user","content":"next topic"}]}`, id, id))
	request, err := NewAIRoutingResponsesRequest(body, "metadata")
	require.NoError(t, err)
	require.Empty(t, request.ToolResults)
	require.Equal(t, []AIRoutingOriginRef{{Kind: "tool_call", ID: id}}, request.OriginRefs)
}

func TestGeminiForwardAsResponsesParallelSignedResultsRestoreOriginalOrder(t *testing.T) {
	for _, providerIDs := range []bool{false, true} {
		t.Run(fmt.Sprintf("providerIDs=%t", providerIDs), func(t *testing.T) {
			ids := []string{"", ""}
			if providerIDs {
				ids = []string{`,"id":"native_first"`, `,"id":"native_second"`}
			}
			raw := fmt.Sprintf(`{"candidates":[{"content":{"parts":[{"thoughtSignature":"first-call-signature","functionCall":{"name":"same_name","args":{"n":1}%s}},{"functionCall":{"name":"same_name","args":{"n":2}%s}}]},"finishReason":"STOP"}]}`, ids[0], ids[1])
			svc, upstream, account := geminiResponsesTestService(raw)
			states := map[string]GeminiResponsesToolState{}
			ctx := WithGeminiResponsesToolState(t.Context(), GeminiResponsesToolStateCallbacks{
				Save: func(_ context.Context, id string, state GeminiResponsesToolState) error {
					states[id] = state
					return nil
				},
				Load: func(_ context.Context, id string) (GeminiResponsesToolState, error) { return states[id], nil },
			})
			_, recorder, err := geminiResponsesTestCall(t, svc, account, ctx, `{"model":"gemini-2.5-flash","store":false,"input":"hello"}`)
			require.NoError(t, err, recorder.Body.String())
			calls := gjson.Get(recorder.Body.String(), "output").Array()
			require.Len(t, calls, 2)
			first, second := calls[0].Get("call_id").String(), calls[1].Get("call_id").String()
			require.NotEmpty(t, states[first].Batch)
			require.Equal(t, states[first].Batch, states[second].Batch)
			require.Less(t, states[first].Index, states[second].Index)
			upstream.response.Body = io.NopCloser(strings.NewReader(`{"candidates":[{"content":{"parts":[{"text":"done"}]},"finishReason":"STOP"}]}`))
			body := fmt.Sprintf(`{"model":"gemini-2.5-flash","store":false,"input":[{"type":"function_call_output","call_id":%q,"output":"second-result"},{"type":"function_call_output","call_id":%q,"output":"first-result"}]}`, second, first)
			_, recorder, err = geminiResponsesTestCall(t, svc, account, ctx, body)
			require.NoError(t, err, recorder.Body.String())
			wire, err := io.ReadAll(upstream.lastReq.Body)
			require.NoError(t, err)
			require.Len(t, gjson.GetBytes(wire, "contents").Array(), 2)
			require.EqualValues(t, 1, gjson.GetBytes(wire, "contents.0.parts.0.functionCall.args.n").Int())
			require.EqualValues(t, 2, gjson.GetBytes(wire, "contents.0.parts.1.functionCall.args.n").Int())
			require.Equal(t, "first-call-signature", gjson.GetBytes(wire, "contents.0.parts.0.thoughtSignature").String())
			require.False(t, gjson.GetBytes(wire, "contents.0.parts.1.thoughtSignature").Exists())
			require.Equal(t, "first-result", gjson.GetBytes(wire, "contents.1.parts.0.functionResponse.response.content").String())
			require.Equal(t, "second-result", gjson.GetBytes(wire, "contents.1.parts.1.functionResponse.response.content").String())
		})
	}
}

func TestGeminiForwardAsResponsesTextHistoryAndProtocolBounds(t *testing.T) {
	svc, upstream, account := geminiResponsesTestService(`{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"MAX_TOKENS"}]}`)
	_, recorder, err := geminiResponsesTestCall(t, svc, account, t.Context(), `{"model":"gemini-2.5-flash","store":false,"instructions":"system","input":[{"role":"user","content":"first"},{"role":"assistant","content":"middle"},{"role":"user","content":"last"}]}`)
	require.NoError(t, err, recorder.Body.String())
	require.Equal(t, "incomplete", gjson.Get(recorder.Body.String(), "status").String())
	wire, err := io.ReadAll(upstream.lastReq.Body)
	require.NoError(t, err)
	for _, value := range []string{"first", "middle", "last", "system"} {
		require.Contains(t, string(wire), value)
	}
	for _, response := range []string{
		`{"candidates":[{"content":{"parts":[{"text":"first"}]},"finishReason":"STOP"},{"content":{"parts":[{"text":"second"}]},"finishReason":"STOP"}]}`,
		`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":{},"id":"same"}},{"functionCall":{"name":"f","args":{},"id":"same"}}]},"finishReason":"STOP"}]}`,
	} {
		svc, upstream, account = geminiResponsesTestService(response)
		_, recorder, err = geminiResponsesTestCall(t, svc, account, t.Context(), `{"model":"gemini-2.5-flash","store":false,"input":"x"}`)
		require.Error(t, err)
		require.NotContains(t, recorder.Body.String(), `"status":"completed"`)
		require.Equal(t, 1, upstream.calls)
	}
	svc, _, account = geminiResponsesTestService("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"" + strings.Repeat("a", 1024) + "\"}]},\"finishReason\":\"STOP\"}]}\n\n")
	svc.cfg.Gateway.MaxLineSize = 256
	_, recorder, err = geminiResponsesTestCall(t, svc, account, t.Context(), `{"model":"gemini-2.5-flash","store":false,"stream":true,"input":"x"}`)
	require.Error(t, err)
	require.NotContains(t, recorder.Body.String(), "response.completed")
}

func TestGeminiForwardAsResponsesPreservesExactArgumentsAndCancellation(t *testing.T) {
	raw := `{"candidates":[{"content":{"parts":[{"thoughtSignature":"private","functionCall":{"name":"f","args":{"integer":9007199254740993}}}]},"finishReason":"STOP"}]}`
	svc, _, account := geminiResponsesTestService("data: " + raw + "\n\n")
	ctx := WithGeminiResponsesToolState(t.Context(), GeminiResponsesToolStateCallbacks{Save: func(_ context.Context, _ string, state GeminiResponsesToolState) error {
		require.Equal(t, `{"integer":9007199254740993}`, string(state.Arguments))
		return nil
	}})
	_, recorder, err := geminiResponsesTestCall(t, svc, account, ctx, `{"model":"gemini-2.5-flash","store":false,"stream":true,"input":"x"}`)
	require.NoError(t, err)
	completed := geminiResponsesCompleted(recorder.Body.String(), true)
	require.Equal(t, `{"integer":9007199254740993}`, completed.Get("output.0.arguments").String())
	svc, _, account = geminiResponsesTestService(raw)
	cancelCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx = WithGeminiResponsesToolState(cancelCtx, GeminiResponsesToolStateCallbacks{Save: func(context.Context, string, GeminiResponsesToolState) error { cancel(); return nil }})
	_, recorder, err = geminiResponsesTestCall(t, svc, account, ctx, `{"model":"gemini-2.5-flash","store":false,"input":"x"}`)
	require.True(t, IsAIRoutingCapacityError(err))
	require.Empty(t, recorder.Body.String(), "cancellation after storing state must not expose a call")
}

func TestGeminiForwardAsResponsesOAuthBuffersNativeStream(t *testing.T) {
	svc, upstream, account := geminiResponsesTestService("data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"wrapped answer\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":7,\"candidatesTokenCount\":3}}}\n\n")
	svc.tokenProvider = &GeminiTokenProvider{}
	account.Type = AccountTypeOAuth
	account.Credentials = map[string]any{"access_token": "synthetic-oauth-token", "project_id": "synthetic-project"}
	result, recorder, err := geminiResponsesTestCall(t, svc, account, t.Context(), `{"model":"gemini-2.5-flash","store":false,"input":"hello"}`)
	require.NoError(t, err, recorder.Body.String())
	require.Equal(t, "/v1internal:streamGenerateContent", upstream.lastReq.URL.Path)
	require.Equal(t, "Bearer synthetic-oauth-token", upstream.lastReq.Header.Get("Authorization"))
	require.Equal(t, "completed", gjson.Get(recorder.Body.String(), "status").String())
	require.Contains(t, recorder.Body.String(), "wrapped answer")
	require.False(t, result.Stream)
	require.Equal(t, 7, result.Usage.InputTokens)
	require.Equal(t, 3, result.Usage.OutputTokens)
}
