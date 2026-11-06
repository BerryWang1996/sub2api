package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestAIRoutingGeminiNativeStateRestoresProviderPartsAndParallelOrder(t *testing.T) {
	for _, providerID := range []bool{false, true} {
		t.Run(fmt.Sprint(providerID), func(t *testing.T) {
			states := map[string]GeminiResponsesToolState{}
			opaque := map[string]json.RawMessage{}
			var calls []json.RawMessage
			for i := range 2 {
				part := []byte(fmt.Sprintf(`{"functionCall":{"name":"same","args":{"n":%d}},"thoughtSignature":"sig%d"}`, i, i))
				if providerID {
					var err error
					part, err = sjson.SetBytes(part, "functionCall.id", fmt.Sprintf("provider_%d", i))
					require.NoError(t, err)
				}
				id := fmt.Sprintf("%s%d", GeminiResponsesToolCallPrefix, i)
				states[id] = GeminiResponsesToolState{Name: "same", Arguments: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i)), Part: part, Batch: "batch", Index: i}
				opaque[AIRoutingOpaqueOriginID(fmt.Sprintf("sig%d", i))] = part
				visible, err := sjson.SetBytes(part, "functionCall.id", id)
				require.NoError(t, err)
				calls = append(calls, visible)
			}
			callJSON, err := json.Marshal(calls)
			require.NoError(t, err)
			body := []byte(fmt.Sprintf(`{"contents":[{"role":"model","parts":%s},{"role":"user","parts":[{"functionResponse":{"name":"same","id":"%s1","response":{"result":1}}},{"text":"retained text"},{"functionResponse":{"name":"same","id":"%s0","response":{"result":0}}}]}]}`, callJSON, GeminiResponsesToolCallPrefix, GeminiResponsesToolCallPrefix))
			before := string(body)
			ctx := WithAIRoutingGeminiNativeState(t.Context(), AIRoutingGeminiNativeStateCallbacks{LoadTool: func(_ context.Context, id string) (GeminiResponsesToolState, error) { return states[id], nil }, LoadOpaque: func(_ context.Context, id string) (json.RawMessage, error) { return opaque[id], nil }})
			out, err := RestoreAIRoutingGeminiNativeRequest(ctx, body)
			require.NoError(t, err)
			require.Equal(t, before, string(body))
			require.JSONEq(t, string(states[GeminiResponsesToolCallPrefix+"0"].Part), gjson.GetBytes(out, "contents.0.parts.0").Raw)
			require.JSONEq(t, string(states[GeminiResponsesToolCallPrefix+"1"].Part), gjson.GetBytes(out, "contents.0.parts.1").Raw)
			require.Equal(t, int64(0), gjson.GetBytes(out, "contents.1.parts.0.functionResponse.response.result").Int())
			require.Equal(t, "retained text", gjson.GetBytes(out, "contents.1.parts.1.text").Str)
			require.Equal(t, int64(1), gjson.GetBytes(out, "contents.1.parts.2.functionResponse.response.result").Int())
			require.NotContains(t, string(out), GeminiResponsesToolCallPrefix)
			if providerID {
				require.Equal(t, "provider_0", gjson.GetBytes(out, "contents.1.parts.0.functionResponse.id").Str)
			} else {
				require.False(t, gjson.GetBytes(out, "contents.1.parts.0.functionResponse.id").Exists())
			}
		})
	}
}

func TestAIRoutingGeminiNativeStateMissingIDNeedsOneProvenCall(t *testing.T) {
	id := GeminiResponsesToolCallPrefix + "one"
	state := GeminiResponsesToolState{Name: "f", Arguments: json.RawMessage(`{"n":9007199254740993}`), Part: json.RawMessage(`{"functionCall":{"name":"f","args":{"n":9007199254740993},"id":"provider"}}`)}
	ctx := WithAIRoutingGeminiNativeState(t.Context(), AIRoutingGeminiNativeStateCallbacks{LoadTool: func(_ context.Context, loaded string) (GeminiResponsesToolState, error) {
		require.Equal(t, id, loaded)
		return state, nil
	}})
	body := []byte(fmt.Sprintf(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"f","args":{"n":9007199254740993},"id":%q}}]},{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"ok":true}}}]}]}`, id))
	out, err := RestoreAIRoutingGeminiNativeRequest(ctx, body)
	require.NoError(t, err)
	require.Contains(t, string(out), "9007199254740993")
	require.Equal(t, "provider", gjson.GetBytes(out, "contents.1.parts.0.functionResponse.id").Str)
	for _, path := range []string{"contents.0.parts.0.functionCall.name", "contents.0.parts.0.functionCall.args.n", "contents.1.parts.0.functionResponse.name"} {
		changed, err := sjson.SetBytes(body, path, "mutated")
		require.NoError(t, err)
		_, err = RestoreAIRoutingGeminiNativeRequest(ctx, changed)
		require.Error(t, err, path)
	}
	_, err = RestoreAIRoutingGeminiNativeRequest(ctx, []byte(fmt.Sprintf(`{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"f","id":%q,"response":{}}}]}]}`, id)))
	require.Error(t, err, "a call ID is not an omitted stateless conversation")
}

func TestAIRoutingGeminiNativeStateOpaqueMutationCancellationAndNoAI(t *testing.T) {
	body := []byte(`{"contents":[{"role":"model","parts":[{"text":"answer","thoughtSignature":"sig"}]},{"role":"user","parts":[{"text":"continue"}]}]}`)
	ctx := WithAIRoutingGeminiNativeState(t.Context(), AIRoutingGeminiNativeStateCallbacks{LoadOpaque: func(_ context.Context, id string) (json.RawMessage, error) {
		require.Equal(t, AIRoutingOpaqueOriginID("sig"), id)
		return json.RawMessage(`{"text":"answer","thoughtSignature":"sig"}`), nil
	}})
	out, err := RestoreAIRoutingGeminiNativeRequest(ctx, body)
	require.NoError(t, err)
	require.JSONEq(t, string(body), string(out))
	changed, err := sjson.SetBytes(body, "contents.0.parts.0.text", "changed")
	require.NoError(t, err)
	_, err = RestoreAIRoutingGeminiNativeRequest(ctx, changed)
	require.True(t, IsAIRoutingCapacityError(err))
	ctx = WithAIRoutingGeminiNativeState(t.Context(), AIRoutingGeminiNativeStateCallbacks{LoadOpaque: func(context.Context, string) (json.RawMessage, error) { return nil, errors.New("expired") }})
	_, err = RestoreAIRoutingGeminiNativeRequest(ctx, body)
	require.True(t, IsAIRoutingCapacityError(err))
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = RestoreAIRoutingGeminiNativeRequest(ctx, body)
	require.Error(t, err)
	out, err = RestoreAIRoutingGeminiNativeRequest(t.Context(), []byte("not-json ordinary native"))
	require.NoError(t, err)
	require.Equal(t, "not-json ordinary native", string(out))
}
