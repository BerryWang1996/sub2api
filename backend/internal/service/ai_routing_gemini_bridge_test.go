package service

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestGeminiRequestToResponsesPreservesPublicSubsetAndToolIdentity(t *testing.T) {
	body := []byte(`{"systemInstruction":{"parts":[{"text":"system rule"}]},"contents":[{"role":"user","parts":[{"text":"first"},{"inlineData":{"mimeType":"image/png","data":"c3ludGhldGlj"}}]},{"role":"model","parts":[{"text":"working"},{"functionCall":{"id":"provider-a","name":"same","args":{"n":9007199254740993}}},{"functionCall":{"id":"provider-b","name":"same","args":{"n":2}}}]},{"role":"user","parts":[{"functionResponse":{"id":"provider-a","name":"same","response":{"ok":1}}},{"functionResponse":{"id":"provider-b","name":"same","response":{"ok":2}}},{"text":"continue"}]}],"generation_config":{"max_output_tokens":128,"temperature":0.2,"top_p":0.9},"tools":[{"function_declarations":[{"name":"same","description":"preserve description","parameters":{"type":"OBJECT","properties":{"n":{"type":"INTEGER"}},"required":["n"]}}]}],"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["same"]}}}`)
	before := string(body)
	out, err := GeminiRequestToResponses(body, "chosen-model", "generateContent", false)
	require.NoError(t, err)
	require.Equal(t, before, string(body))
	require.Equal(t, "chosen-model", gjson.GetBytes(out, "model").Str)
	require.False(t, gjson.GetBytes(out, "store").Bool())
	require.Equal(t, "system rule", gjson.GetBytes(out, "instructions").Str)
	require.Equal(t, int64(128), gjson.GetBytes(out, "max_output_tokens").Int())
	require.Equal(t, "integer", gjson.GetBytes(out, "tools.0.parameters.properties.n.type").Str)
	require.Equal(t, "same", gjson.GetBytes(out, "tool_choice.name").Str)
	var ids []string
	for _, item := range gjson.GetBytes(out, "input").Array() {
		if item.Get("type").Str == "function_call" {
			ids = append(ids, item.Get("call_id").Str)
		}
	}
	require.Equal(t, []string{"provider-a", "provider-b"}, ids)
	require.Contains(t, string(out), "9007199254740993")
	require.Contains(t, string(out), "data:image/png;base64,c3ludGhldGlj")
	require.Contains(t, string(out), "first")
	require.Contains(t, string(out), "working")
	require.Contains(t, string(out), "continue")
}

func TestGeminiRequestBridgeRefusesUnrepresentableControls(t *testing.T) {
	for _, extra := range []string{
		`"cachedContent":"cachedContents/private"`,
		`"generationConfig":{"candidateCount":2}`,
		`"generationConfig":{"stopSequences":["STOP"]}`,
		`"generationConfig":{"responseMimeType":"application/json"}`,
		`"tools":[{"googleSearch":{}}]`,
		`"toolConfig":{"functionCallingConfig":{"mode":"VALIDATED"}}`,
		`"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["a","b"]}}`,
		`"tools":[{"functionDeclarations":[{"name":"f","parameters":{"type":"OBJECT","nullable":true}}]}]`,
	} {
		_, err := GeminiRequestToResponses([]byte(`{"contents":[{"parts":[{"text":"hello"}]}],`+extra+`}`), "chosen", "generateContent", false)
		require.Error(t, err, extra)
	}
	for _, part := range []string{`{"fileData":{"mimeType":"image/png","fileUri":"files/private"}}`, `{"text":"hidden","thought":true}`, `{"text":"signed","thoughtSignature":"opaque"}`, `{"inlineData":{"mimeType":"audio/wav","data":"YQ=="}}`} {
		_, err := GeminiRequestToResponses([]byte(`{"contents":[{"parts":[`+part+`]}]}`), "chosen", "generateContent", false)
		require.Error(t, err, part)
	}
}

func TestGeminiResponsesOutputStreamsTextAndSameNameCallsOnce(t *testing.T) {
	s := &GeminiResponsesOutput{Model: "public-alias"}
	text := `{"id":"msg","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}`
	a := `{"id":"item-a","type":"function_call","call_id":"call-a","name":"same","arguments":"{\"n\":9007199254740993}"}`
	b := `{"id":"item-b","type":"function_call","call_id":"call-b","name":"same","arguments":"{\"n\":2}"}`
	frames := []string{`{"type":"response.created","response":{"id":"resp"}}`, `{"type":"response.output_text.delta","item_id":"msg","output_index":0,"content_index":0,"delta":"hel"}`, `{"type":"response.output_item.done","output_index":0,"item":` + text + `}`, `{"type":"response.output_item.done","output_index":1,"item":` + a + `}`, `{"type":"response.output_item.done","output_index":2,"item":` + b + `}`, `{"type":"response.completed","response":{"id":"resp","status":"completed","output":[` + text + `,` + a + `,` + b + `],"usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"cached_tokens":30}}}}`}
	var all strings.Builder
	for i, frame := range frames {
		out, err := s.Convert([]byte(frame), true)
		require.NoError(t, err, frame)
		_, _ = all.Write(out)
		if i < len(frames)-1 {
			require.NotContains(t, string(out), "finishReason")
		}
	}
	require.True(t, s.Complete)
	require.Equal(t, 1, strings.Count(all.String(), `"id":"call-a"`))
	require.Equal(t, 1, strings.Count(all.String(), `"id":"call-b"`))
	require.Contains(t, all.String(), "9007199254740993")
	require.Contains(t, all.String(), `"text":"hel"`)
	require.Contains(t, all.String(), `"text":"lo"`)
	require.Contains(t, all.String(), `"cachedContentTokenCount":30`)
	require.Contains(t, all.String(), `"modelVersion":"public-alias"`)
	_, err := s.Convert([]byte(frames[len(frames)-1]), true)
	require.Error(t, err)
	nonstream := &GeminiResponsesOutput{Model: "alias"}
	out, err := nonstream.Convert([]byte(`{"object":"response","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[`+text+`],"usage":{"input_tokens":10,"output_tokens":20}}`), false)
	require.NoError(t, err)
	require.Equal(t, "MAX_TOKENS", gjson.GetBytes(out, "candidates.0.finishReason").Str)
}

func TestGeminiResponsesOutputRejectsMutationOpaqueAndUnboundedState(t *testing.T) {
	for _, body := range []string{
		`{"object":"response","status":"completed","output":[{"type":"reasoning","encrypted_content":"opaque"}]}`,
		`{"object":"response","status":"completed","output":[{"type":"web_search_call","id":"search"}]}`,
		`{"object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"x","annotations":[{"type":"url_citation"}]}]}]}`,
		`{"object":"response","status":"failed","output":[]}`,
	} {
		_, err := (&GeminiResponsesOutput{}).Convert([]byte(body), false)
		require.Error(t, err)
	}
	s := &GeminiResponsesOutput{}
	_, err := s.Convert([]byte(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"first"}`), true)
	require.NoError(t, err)
	_, err = s.Convert([]byte(`{"type":"response.completed","response":{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"different"}]}]}}`), true)
	require.Error(t, err)
	require.False(t, s.Complete)
	s = &GeminiResponsesOutput{}
	for i := range 65 {
		_, err = s.Convert([]byte(fmt.Sprintf(`{"type":"response.output_item.done","output_index":%d,"item":{"type":"function_call","id":"item%d","name":"f","call_id":"call%d","arguments":"{}"}}`, i, i, i)), true)
		if i == 64 {
			require.Error(t, err)
		} else {
			require.NoError(t, err)
		}
	}
	s = &GeminiResponsesOutput{}
	_, err = s.Convert([]byte(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"partial"}`), true)
	require.NoError(t, err)
	require.False(t, s.Complete, "partial delivery alone is not a complete response")
}
