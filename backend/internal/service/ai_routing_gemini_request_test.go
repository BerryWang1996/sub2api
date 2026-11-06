//go:build unit

package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func aiGeminiTestBody(t *testing.T, contents []any, fields map[string]any) []byte {
	t.Helper()
	root := map[string]any{"contents": contents}
	for key, value := range fields {
		root[key] = value
	}
	body, err := json.Marshal(root)
	require.NoError(t, err)
	return body
}

func aiGeminiTestContent(role string, parts ...any) map[string]any {
	return map[string]any{"role": role, "parts": parts}
}

func TestAIRoutingGeminiRequestNativeMetadataAndReferences(t *testing.T) {
	body := aiGeminiTestBody(t, []any{
		aiGeminiTestContent("user", map[string]any{"text": "earlier"}),
		aiGeminiTestContent("model", map[string]any{"thought": true, "text": "private-thought", "thoughtSignature": "private-signature"}, map[string]any{"text": "visible reply"}),
		aiGeminiTestContent("user", map[string]any{"text": "continue password=private-secret"}, map[string]any{"file_data": map[string]any{"mime_type": "application/pdf", "file_uri": "files/private-document"}}),
	}, map[string]any{"cached_content": "cachedContents/private-cache", "systemInstruction": map[string]any{"parts": []any{map[string]any{"text": "private-system"}}}, "generation_config": map[string]any{"max_output_tokens": 128, "candidate_count": 1}, "tools": []any{map[string]any{"googleSearch": map[string]any{}}}})
	original := append([]byte(nil), body...)
	r, err := NewAIRoutingGeminiRequest(body, "client-alias", "streamGenerateContent", "bounded_history")
	require.NoError(t, err)
	require.Equal(t, original, body)
	require.Equal(t, original, r.HistoryBody)
	require.Equal(t, CompositeRouteEndpointGemini, r.Endpoint)
	require.Equal(t, "client-alias", r.PublicModel)
	require.Equal(t, "streamGenerateContent", r.GeminiAction)
	require.Equal(t, 128, r.MaxOutput)
	require.False(t, r.OutputLimitOmitted)
	require.True(t, r.RequiresNativeGemini, "candidateCount:1 must not clear cache/media/signature requirements")
	require.True(t, r.RequiresOrigin)
	require.Equal(t, []AIRoutingOriginRef{{Kind: "opaque", ID: AIRoutingOpaqueOriginID("private-signature")}}, r.OriginRefs)
	require.Contains(t, r.InputModalities, "document")
	refs, err := ParseAIRoutingGeminiReferences(body)
	require.NoError(t, err)
	require.Equal(t, "cachedContents/private-cache", refs.CachedContent)
	require.Equal(t, []string{"files/private-document"}, refs.FileURIs)
	r, err = NewAIRoutingGeminiRequest(body, "client-alias", "generateContent", "task_excerpt")
	require.NoError(t, err)
	require.Contains(t, r.Task, "continue")
	for _, secret := range []string{"private-system", "private-thought", "private-signature", "private-document", "private-cache", "private-secret"} {
		require.NotContains(t, r.Task, secret)
	}
}

func TestAIRoutingGeminiRequestMissingResultIDRequiresUniqueGatewayOrigin(t *testing.T) {
	id := GeminiResponsesToolCallPrefix + "fixture"
	call := map[string]any{"functionCall": map[string]any{"id": id, "name": "f", "args": map[string]any{"x": 1}}}
	result := map[string]any{"functionResponse": map[string]any{"name": "f", "response": map[string]any{"secret": "private-output"}}}
	body := aiGeminiTestBody(t, []any{aiGeminiTestContent("model", call), aiGeminiTestContent("user", result)}, nil)
	r, err := NewAIRoutingGeminiRequest(body, "public", "generateContent", "metadata")
	require.NoError(t, err)
	require.Equal(t, []string{id}, r.ToolResults)
	require.Equal(t, []AIRoutingOriginRef{{Kind: "tool_call", ID: id}}, r.OriginRefs)
	locators, err := AIRoutingGeminiToolResults(body)
	require.NoError(t, err)
	require.Equal(t, []AIRoutingGeminiToolResult{{ContentIndex: 1, PartIndex: 0, CallID: id, Inferred: true}}, locators)
	for _, modelParts := range [][]any{
		{map[string]any{"functionCall": map[string]any{"name": "f", "args": map[string]any{}}}},
		{map[string]any{"functionCall": map[string]any{"id": "external-id", "name": "f", "args": map[string]any{}}}},
		{call, call},
		{call, map[string]any{"functionCall": map[string]any{"id": GeminiResponsesToolCallPrefix + "other", "name": "g", "args": map[string]any{}}}},
	} {
		_, err := NewAIRoutingGeminiRequest(aiGeminiTestBody(t, []any{aiGeminiTestContent("model", modelParts...), aiGeminiTestContent("user", result)}, nil), "public", "generateContent", "metadata")
		require.Error(t, err, "no content hash, unique name, or parallel-order guessing grants a tool origin")
	}
	_, err = NewAIRoutingGeminiRequest(aiGeminiTestBody(t, []any{aiGeminiTestContent("user", result)}, nil), "public", "generateContent", "metadata")
	require.Error(t, err)
}

func TestAIRoutingGeminiRequestExplicitToolIDsAndMediaMetadata(t *testing.T) {
	body := aiGeminiTestBody(t, []any{aiGeminiTestContent("user", map[string]any{"function_response": map[string]any{"id": "call_explicit", "name": "f", "response": map[string]any{"text": "private-result"}, "parts": []any{map[string]any{"fileData": map[string]any{"mimeType": "image/png", "fileUri": "gs://private-bucket/image"}}}}})}, nil)
	r, err := NewAIRoutingGeminiRequest(body, "public", "generateContent", "task_excerpt")
	require.NoError(t, err)
	require.True(t, r.RequiresNativeGemini)
	require.Contains(t, r.InputModalities, "image")
	require.NotContains(t, r.Task, "private-result")
	require.NotContains(t, r.Task, "private-bucket")
	refs, err := ParseAIRoutingGeminiReferences(body)
	require.NoError(t, err)
	require.Equal(t, []string{"gs://private-bucket/image"}, refs.FileURIs)
	locators, err := AIRoutingGeminiToolResults(body)
	require.NoError(t, err)
	require.False(t, locators[0].Inferred)
}

func TestAIRoutingGeminiRequestRejectsAmbiguity(t *testing.T) {
	for _, body := range []string{
		`{"contents":[],"contents":[]}`,
		`{"Contents":[{"parts":[{"text":"x"}]}]}`,
		`{"contents":[{"role":"assistant","parts":[{"text":"x"}]}]}`,
		`{"contents":[{"parts":[{"Text":"x"}]}]}`,
		`{"contents":[{"parts":[{"text":"x","thought":"true"}]}]}`,
		`{"contents":[{"parts":[{"text":"x","thoughtSignature":"a","thought_signature":"a"}]}]}`,
		`{"contents":[{"parts":[{"text":"x"}]}],"generationConfig":{"maxOutputTokens":128},"generation_config":{"max_output_tokens":128}}`,
		`{"contents":[{"parts":[{"text":"x"}]}],"generationConfig":{"maxOutputTokens":128,"max_output_tokens":128}}`,
		`{"contents":[{"parts":[{"text":"x"}]}],"generationConfig":{"maxOutputTokens":1.5}}`,
		`{"contents":[{"parts":[{"text":"x"}]}],"generationConfig":{"candidateCount":0}}`,
		`{"contents":[{"parts":[{"text":"x"}]}],"cachedContent":"a","cached_content":"a"}`,
		`{"contents":[{"parts":[{"text":"x"}]}],"model":"other"}`,
		`{"contents":[{"parts":[{"text":"x"}]}],"input":"alternate history"}`,
		`{"contents":[{"parts":[{"functionResponse":{"name":"f","response":{}}}]}]}`,
		`{"contents":[{"parts":[{"inlineData":{"mimeType":"image/png","data":"x"},"text":"x"}]}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			_, err := NewAIRoutingGeminiRequest([]byte(body), "public", "generateContent", "metadata")
			require.Error(t, err)
		})
	}
	_, err := NewAIRoutingGeminiRequest([]byte(`{"contents":[{"parts":[{"text":"x"}]}]}`), "../escape", "generateContent", "metadata")
	require.Error(t, err)
	_, err = NewAIRoutingGeminiRequest([]byte(`{"contents":[{"parts":[{"text":"x"}]}]}`), "public", "batchGenerateContent", "metadata")
	require.Error(t, err)
}

func TestAIRoutingGeminiRequestCountTokensAndNativeOnlyFeatures(t *testing.T) {
	body := []byte(`{"generateContentRequest":{"model":"models/public","contents":[{"role":"user","parts":[{"text":"count task"}]}],"generationConfig":{"maxOutputTokens":77}}}`)
	r, err := NewAIRoutingGeminiRequest(body, "public", "countTokens", "metadata")
	require.NoError(t, err)
	require.Equal(t, "countTokens", r.GeminiAction)
	require.Equal(t, AIRoutingGeminiCountTokens, r.Operation)
	require.Zero(t, r.MaxOutput)
	require.False(t, r.OutputLimitOmitted)
	_, err = NewAIRoutingGeminiRequest(body, "public", "generateContent", "metadata")
	require.Error(t, err)
	_, err = NewAIRoutingGeminiRequest([]byte(`{"contents":[],"generateContentRequest":{"contents":[{"parts":[{"text":"x"}]}]}}`), "public", "countTokens", "metadata")
	require.Error(t, err)
	for _, field := range []string{`"generationConfig":{"candidateCount":2}`, `"generationConfig":{"responseMimeType":"application/json"}`, `"safetySettings":[]`, `"tools":[{"googleSearch":{}}]`} {
		r, err = NewAIRoutingGeminiRequest([]byte(`{"contents":[{"parts":[{"text":"hello"}]}],`+field+`}`), "public", "generateContent", "metadata")
		require.NoError(t, err)
		require.True(t, r.RequiresNativeGemini, field)
	}
	var parts []string
	for i := 0; i < 65; i++ {
		parts = append(parts, fmt.Sprintf(`{"functionResponse":{"id":"call_%d","name":"f","response":{}}}`, i))
	}
	_, err = NewAIRoutingGeminiRequest([]byte(`{"contents":[{"role":"user","parts":[`+strings.Join(parts, ",")+`]}]}`), "public", "generateContent", "metadata")
	require.Error(t, err, "origin fan-out remains bounded")
}
