package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func aiGeminiNativeFixture() (*service.APIKey, service.AIRoutingCandidate) {
	id := int64(77)
	return &service.APIKey{ID: 11, UserID: 12, Group: &service.Group{ID: 13}}, service.AIRoutingCandidate{ID: "gemini", Model: "gemini-2.5-pro", Platform: service.PlatformGemini, TargetAccountID: &id}
}

func runAIGeminiNativeWriter(t *testing.T, store service.AIRoutingOriginStore, key *service.APIKey, chosen service.AIRoutingCandidate, stream bool, respond gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	router := gin.New()
	h := &GatewayHandler{}
	router.Use(func(c *gin.Context) {
		h.forwardAIGeminiWithOrigin(c, nil, store, key, service.AIRoutingRequest{PublicModel: "auto", GeminiAction: map[bool]string{true: "streamGenerateContent", false: "generateContent"}[stream]}, chosen)
	})
	router.POST("/", func(c *gin.Context) { setOpsSelectedAccount(c, 77, chosen.Platform); respond(c) })
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/", nil))
	return recorder
}

func TestAIGeminiNativeWriterPersistsBeforeExposureAndRestoresExactPart(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[stream], func(t *testing.T) {
			store := &aiResponsesMemoryStore{}
			key, chosen := aiGeminiNativeFixture()
			original := `{"functionCall":{"name":"f","args":{"n":9007199254740993},"id":"provider-id"},"thoughtSignature":"real-synthetic-signature"}`
			response := `{"candidates":[{"index":0,"content":{"role":"model","parts":[` + original + `]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":100,"candidatesTokenCount":20}}`
			recorder := runAIGeminiNativeWriter(t, store, key, chosen, stream, func(c *gin.Context) {
				if stream {
					c.Header("Content-Type", "text/event-stream")
					frame := "data: " + response + "\n\n"
					for _, b := range []byte(frame) {
						_, err := c.Writer.Write([]byte{b})
						require.NoError(t, err)
					}
				} else {
					c.Data(200, "application/json", []byte(response))
				}
			})
			require.Equal(t, 200, recorder.Code)
			payload := recorder.Body.String()
			if stream {
				payload = strings.TrimSpace(strings.TrimPrefix(payload, "data: "))
			}
			part := gjson.Get(payload, "candidates.0.content.parts.0")
			id := part.Get("functionCall.id").Str
			require.True(t, strings.HasPrefix(id, service.GeminiResponsesToolCallPrefix))
			require.NotEqual(t, "provider-id", id)
			require.Equal(t, "real-synthetic-signature", part.Get("thoughtSignature").Str)
			follow := []byte(`{"contents":[{"role":"model","parts":[` + part.Raw + `]},{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"ok":true}}}]}]}`)
			request, err := service.NewAIRoutingGeminiRequest(follow, "auto", "generateContent", "metadata")
			require.NoError(t, err)
			binding, err := loadAIGeminiBinding(t.Context(), store, key, request)
			require.NoError(t, err)
			require.Equal(t, newAIToolBinding(chosen, 77), binding)
			for _, ttl := range store.ttls {
				require.Equal(t, service.AIRoutingToolOriginTTL, ttl)
			}
			runAIGeminiNativeWriter(t, store, key, chosen, false, func(c *gin.Context) {
				out, err := service.RestoreAIRoutingGeminiNativeRequest(c.Request.Context(), follow)
				require.NoError(t, err)
				require.JSONEq(t, original, gjson.GetBytes(out, "contents.0.parts.0").Raw)
				require.Equal(t, "provider-id", gjson.GetBytes(out, "contents.1.parts.0.functionResponse.id").Str)
				c.JSON(200, gin.H{"usageMetadata": gin.H{}})
			})
			other := *key
			other.ID++
			_, err = loadAIGeminiBinding(t.Context(), store, &other, request)
			require.Error(t, err)
			responsesKey, err := aiResponsesOriginKey(key, service.AIRoutingOriginRef{Kind: "tool_call", ID: id})
			require.NoError(t, err)
			require.Empty(t, store.rows[responsesKey], "Gemini and Responses origins must not alias")
			storageKey, err := aiGeminiOriginKey(key, request.OriginRefs[0])
			require.NoError(t, err)
			var origin service.AIRoutingOriginBinding
			require.NoError(t, json.Unmarshal([]byte(store.rows[storageKey]), &origin))
			parentKey, err := aiGeminiOriginKey(key, service.AIRoutingOriginRef{Kind: "response", ID: origin.ResponseID})
			require.NoError(t, err)
			delete(store.rows, parentKey)
			_, err = loadAIGeminiBinding(t.Context(), store, key, request)
			require.Error(t, err, "revoking parent also revokes signature and calls")
		})
	}
}

func TestAIGeminiNativeWriterFailuresExposeNoUnboundCall(t *testing.T) {
	key, chosen := aiGeminiNativeFixture()
	for _, raw := range []string{
		`{"candidates":[{"content":{"parts":[{"FunctionCall":{"name":"f","args":{}}}]}}]}`,
		`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":{},"id":42}}]}}]}`,
		`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":{}},"function_call":{"name":"g","args":{}}}]}}]}`,
	} {
		recorder := runAIGeminiNativeWriter(t, &aiResponsesMemoryStore{}, key, chosen, false, func(c *gin.Context) { c.Data(200, "application/json", []byte(raw)) })
		require.Equal(t, 503, recorder.Code)
		require.NotContains(t, recorder.Body.String(), service.GeminiResponsesToolCallPrefix)
	}
	store := &aiResponsesMemoryStore{err: errors.New("offline")}
	recorder := runAIGeminiNativeWriter(t, store, key, chosen, false, func(c *gin.Context) {
		c.Data(200, "application/json", []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f","args":{}}}]}}]}`))
	})
	require.Equal(t, 503, recorder.Code)
	require.NotContains(t, recorder.Body.String(), "functionCall")
}

func TestAIGeminiNativeStateCannotSubstituteAnotherParent(t *testing.T) {
	store := &aiResponsesMemoryStore{}
	key, chosen := aiGeminiNativeFixture()
	target := newAIToolBinding(chosen, 77)
	origin := service.AIRoutingOriginBinding{Candidate: target.Candidate, Account: 77, Model: target.Model, Platform: target.Platform, AccountTarget: 77, ResponseID: "parent-a", Native: true}
	ctx := context.Background()
	ref := service.AIRoutingOriginRef{Kind: "tool_call", ID: service.GeminiResponsesToolCallPrefix + "one"}
	state := &aiGeminiNativeStoredState{Origin: origin, Tool: &service.GeminiResponsesToolState{Name: "f", Arguments: json.RawMessage(`{}`), Part: json.RawMessage(`{"functionCall":{"name":"f","args":{}}}`)}}
	require.NoError(t, saveAIGeminiOrigin(ctx, store, key, service.AIRoutingOriginRef{Kind: "response", ID: origin.ResponseID}, origin, nil))
	require.NoError(t, saveAIGeminiOrigin(ctx, store, key, ref, origin, state))
	state.Origin.ResponseID = "parent-b"
	raw, err := json.Marshal(state)
	require.NoError(t, err)
	storageKey, err := aiGeminiOriginKey(key, ref)
	require.NoError(t, err)
	store.rows[storageKey+":state"] = string(raw)
	runAIGeminiNativeWriter(t, store, key, chosen, false, func(c *gin.Context) {
		_, err := service.RestoreAIRoutingGeminiNativeRequest(c.Request.Context(), []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"id":"`+ref.ID+`","name":"f","args":{}}}]},{"role":"user","parts":[{"functionResponse":{"id":"`+ref.ID+`","name":"f","response":{}}}]}]}`))
		require.True(t, service.IsAIRoutingCapacityError(err))
		c.JSON(200, gin.H{})
	})
}
