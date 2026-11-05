package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/testutil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func aiToolSSE(id string) string {
	return fmt.Sprintf("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"tool_use\",\"id\":%q}}\n\n", id)
}

func newAIToolWriterTest(stream bool, bind func(string) error) (*aiToolWriter, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	contentType := "application/json"
	if stream {
		contentType = "text/event-stream"
	}
	c.Header("Content-Type", contentType)
	return &aiToolWriter{ResponseWriter: c.Writer, bind: bind}, recorder
}

func TestAIToolWriterSSEBindsBeforeCompleteEventAndPreservesFragments(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n"} {
		t.Run(fmt.Sprintf("newline=%q", newline), func(t *testing.T) {
			var recorder *httptest.ResponseRecorder
			calls := 0
			writer, output := newAIToolWriterTest(true, func(id string) error {
				calls++
				require.Equal(t, "tool-1", id)
				require.Equal(t, ": heartbeat\n\n", recorder.Body.String(), "the tool event must remain buffered during binding")
				return nil
			})
			recorder = output
			_, err := writer.WriteString(": heartbeat\n\n")
			require.NoError(t, err)
			require.Equal(t, ": heartbeat\n\n", output.Body.String(), "complete non-tool events flow before the answer ends")
			frame := strings.ReplaceAll(aiToolSSE("tool-1"), "\n", newline)
			for i := range len(frame) - 1 {
				_, err = writer.Write([]byte(frame[i : i+1]))
				require.NoError(t, err)
				writer.Flush()
				require.Equal(t, ": heartbeat\n\n", output.Body.String())
			}
			require.Zero(t, calls)
			_, err = writer.WriteString(frame[len(frame)-1:])
			require.NoError(t, err)
			require.Equal(t, 1, calls)
			require.Equal(t, ": heartbeat\n\n"+frame, output.Body.String())
			require.NoError(t, writer.finish())
		})
	}
}

func TestAIToolWriterFailedBindingRejectsFurtherWritesAndFlush(t *testing.T) {
	expected := errors.New("storage failed")
	writer, output := newAIToolWriterTest(true, func(string) error { return expected })
	failures := 0
	writer.onFailure = func(err error) { failures++; require.ErrorIs(t, err, expected) }
	sizeBefore := writer.Size()
	_, err := writer.WriteString(aiToolSSE("tool-1"))
	require.ErrorIs(t, err, expected)
	require.NotEqual(t, sizeBefore, writer.Size(), "the existing failover size guard must stop replay")
	require.True(t, writer.Written())
	writer.WriteHeaderNow()
	writer.Flush()
	_, err = writer.WriteString(aiToolSSE("tool-replayed"))
	require.ErrorIs(t, err, expected)
	require.ErrorIs(t, writer.finish(), expected)
	require.Equal(t, 1, failures)
	require.Empty(t, output.Body.String())
	require.False(t, output.Flushed)
}

func TestAIToolWriterJSONWaitsForCompletionAndAllBindings(t *testing.T) {
	for _, failSecond := range []bool{false, true} {
		t.Run(fmt.Sprint(failSecond), func(t *testing.T) {
			calls := []string{}
			writer, output := newAIToolWriterTest(false, func(id string) error {
				calls = append(calls, id)
				if failSecond && id == "second" {
					return errors.New("unavailable")
				}
				return nil
			})
			body := `{"content":[{"type":"tool_use","id":"first"},{"type":"tool_use","id":"second"}]}`
			for _, part := range []string{body[:17], body[17:]} {
				_, err := writer.WriteString(part)
				require.NoError(t, err)
				writer.WriteHeaderNow()
				writer.Flush()
				require.Empty(t, output.Body.String())
			}
			require.Empty(t, calls)
			err := writer.finish()
			require.Equal(t, []string{"first", "second"}, calls)
			if failSecond {
				require.Error(t, err)
				require.Empty(t, output.Body.String())
			} else {
				require.NoError(t, err)
				require.Equal(t, body, output.Body.String())
			}
		})
	}
}

func TestAIToolWriterRejectsIncompleteOrAmbiguousPayload(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream bool
		body   string
	}{
		{"incomplete-sse", true, strings.TrimSuffix(aiToolSSE("x"), "\n")},
		{"incomplete-json", false, `{"content":[{"type":"tool_use","id":"x"}`},
		{"multiple-json-documents", false, `{"content":[]} {"content":[{"type":"tool_use","id":"x"}]}`},
		{"empty-tool-id", true, aiToolSSE("")},
		{"long-tool-id", true, aiToolSSE(strings.Repeat("x", 257))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writer, output := newAIToolWriterTest(tc.stream, func(string) error { t.Fatal("invalid payload must not bind"); return nil })
			_, err := writer.WriteString(tc.body)
			if err == nil {
				err = writer.finish()
			}
			require.Error(t, err)
			require.Empty(t, output.Body.String())
		})
	}
}

func TestAIToolWriterKeepsEventJSONAndToolCountBounds(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, extra := range []int{0, 1} {
			t.Run(fmt.Sprintf("stream=%v/extra=%d", stream, extra), func(t *testing.T) {
				prefix, suffix, limit := `{"padding":"`, `","content":[{"type":"tool_use","id":"x"}]}`, aiToolJSONLimit
				if stream {
					prefix, suffix, limit = "data: {\"padding\":\"", "\",\"type\":\"content_block_start\",\"content_block\":{\"type\":\"tool_use\",\"id\":\"x\"}}\n\n", aiToolEventLimit
				}
				body := prefix + strings.Repeat("x", limit-len(prefix)-len(suffix)+extra) + suffix
				calls := 0
				writer, output := newAIToolWriterTest(stream, func(string) error { calls++; return nil })
				_, err := writer.WriteString(body)
				if err == nil {
					err = writer.finish()
				}
				if extra == 0 {
					require.NoError(t, err)
					require.Equal(t, body, output.Body.String())
					require.Equal(t, 1, calls)
				} else {
					require.Error(t, err)
					require.Empty(t, output.Body.String())
					require.Zero(t, calls)
				}
			})
		}
	}
	writer, output := newAIToolWriterTest(true, func(string) error { return nil })
	for i := range aiToolCountLimit {
		_, err := writer.WriteString(aiToolSSE(fmt.Sprint(i)))
		require.NoError(t, err)
	}
	_, err := writer.WriteString(aiToolSSE("0"))
	require.NoError(t, err, "a repeated ID reuses its already confirmed binding")
	_, err = writer.WriteString(aiToolSSE("overflow"))
	require.Error(t, err)
	require.NotContains(t, output.Body.String(), "overflow")
}

func TestAIToolWriterSupportsMultilineJSONAndToolInputCaseVariants(t *testing.T) {
	ids := []string{}
	writer, output := newAIToolWriterTest(true, func(id string) error { ids = append(ids, id); return nil })
	frame := "event: content_block_start\ndata: {\"type\":\"content_block_start\",\ndata: \"content_block\":{\"type\":\"tool_use\",\"id\":\"tool-1\",\"input\":{\"type\":1,\"Type\":2,\"id\":3,\"ID\":4,\"n\":1e1000}}}\n\n"
	_, err := writer.WriteString(frame)
	require.NoError(t, err)
	require.Equal(t, []string{"tool-1"}, ids)
	require.Equal(t, frame, output.Body.String())
}

func TestAIToolWriterRejectsDuplicateAndCaseVariantProtocolMembers(t *testing.T) {
	blocks := []string{
		`{"type":"tool_use","type":"text","id":"tool-1"}`,
		`{"type":"tool_use","Type":"text","id":"tool-1"}`,
		`{"type":"tool_use","id":"tool-1","ID":"other"}`,
		`{"type":"tool_use","id":"tool-1","input":{"a":1,"a":2}}`,
	}
	for i, block := range blocks {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("block=%d/stream=%v", i, stream), func(t *testing.T) {
				body := `{"content":[` + block + `]}`
				if stream {
					body = "data: {\"type\":\"content_block_start\",\"content_block\":" + block + "}\n\n"
				}
				writer, output := newAIToolWriterTest(stream, func(string) error { t.Fatal("ambiguous JSON must not bind"); return nil })
				_, err := writer.WriteString(body)
				if err == nil {
					err = writer.finish()
				}
				require.Error(t, err)
				require.Empty(t, output.Body.String())
			})
		}
	}
	for _, body := range []string{
		`{"content":[{"type":"tool_use","id":"x"}],"content":[]}`,
		`{"content":[{"type":"tool_use","id":"x"}],"Content":[]}`,
		`{"type":"content_block_start","content_block":{"type":"tool_use","id":"x"},"Content_Block":{"type":"text"}}`,
		`{"type":"message_start","message":{"content":[{"type":"tool_use","id":"x"}]},"Message":{"content":[]}}`,
		`{"type":"message_start","message":{"content":[{"type":"tool_use","id":"x"}],"Content":[]}}`,
	} {
		writer, output := newAIToolWriterTest(true, func(string) error { t.Fatal("ambiguous envelope must not bind"); return nil })
		_, err := writer.WriteString("data: " + body + "\n\n")
		require.Error(t, err)
		require.Empty(t, output.Body.String())
	}
}

func TestAIToolWriterWithoutBindingAndErrorResponsesRemainTransparent(t *testing.T) {
	writer, output := newAIToolWriterTest(false, nil)
	_, err := writer.WriteString("legacy response")
	require.NoError(t, err)
	require.Equal(t, "legacy response", output.Body.String())
	writer, output = newAIToolWriterTest(false, func(string) error { t.Fatal("error response must not bind"); return nil })
	writer.WriteHeader(http.StatusBadGateway)
	_, err = writer.WriteString(`{"error":"upstream failed"}`)
	require.NoError(t, err)
	require.NoError(t, writer.finish())
	require.Equal(t, http.StatusBadGateway, output.Code)
	require.Contains(t, output.Body.String(), "upstream failed")
}

func TestAIToolBindingRedisConflictsAndErrorsFailBeforeExposure(t *testing.T) {
	for _, mode := range []string{"conflict", "unavailable", "missing-account"} {
		for _, transport := range []string{"json", "sse", "sse-uncommitted"} {
			t.Run(mode+"/"+transport, func(t *testing.T) {
				stream := transport != "json"
				store, server := testutil.NewRedisAIRoutingStore(t)
				groupID, accountID := int64(4), int64(7)
				key := &service.APIKey{ID: 1, UserID: 2, GroupID: &groupID, Group: &service.Group{ID: groupID, Platform: service.PlatformComposite, AIRouting: true, Status: service.StatusActive}}
				candidate := service.AIRoutingCandidate{ID: "only", Enabled: true, Platform: service.PlatformAnthropic, Model: "private", TargetAccountID: &accountID}
				runtime := &aiRoutingRuntime{store: store, policies: &aiHTTPPolicy{policy: &service.AIRoutingPolicy{GroupID: groupID, Version: 1, Enabled: true}}}
				runtime.eligible = func(context.Context, *service.APIKey, *service.AIRoutingPolicy, service.AIRoutingRequest, string, int64) []service.AIRoutingCandidate {
					return []service.AIRoutingCandidate{candidate}
				}
				h := &GatewayHandler{aiRouting: runtime}
				engine := gin.New()
				engine.Use(func(c *gin.Context) { c.Set(string(middleware.ContextKeyAPIKey), key) }, h.AIRoute)
				executions := 0
				engine.POST("/v1/messages", func(c *gin.Context) {
					executions++
					if mode != "missing-account" {
						c.Set(opsAccountIDKey, accountID)
					}
					if mode == "unavailable" {
						server.SetError("ERR unavailable")
					}
					if mode == "conflict" {
						other, err := json.Marshal(newAIToolBinding(candidate, accountID+1))
						require.NoError(t, err)
						require.NoError(t, server.Set(aiToolKey(key, "alias", "secret-tool"), string(other)))
					}
					if stream {
						c.Header("Content-Type", "text/event-stream")
						c.Writer.WriteHeaderNow()
						c.Writer.Flush()
						if transport == "sse" {
							_, err := c.Writer.WriteString(": heartbeat\n\n")
							require.NoError(t, err)
						}
						_, err := c.Writer.WriteString(aiToolSSE("secret-tool"))
						require.Error(t, err)
						require.ErrorIs(t, c.Request.Context().Err(), context.Canceled)
						require.True(t, service.IsResponseCommitted(c))
						_, err = c.Writer.WriteString(aiToolSSE("replayed-tool"))
						require.Error(t, err)
					} else {
						c.JSON(http.StatusOK, gin.H{"content": []aiToolBlock{{Type: "tool_use", ID: "secret-tool"}}})
					}
				})
				body := fmt.Sprintf(`{"model":"alias","stream":%v,"messages":[{"role":"user","content":"scan"}],"tools":[{"name":"scan","input_schema":{"type":"object"}}]}`, stream)
				recorder := httptest.NewRecorder()
				engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
				require.Equal(t, 1, executions)
				require.Contains(t, recorder.Body.String(), "ai_tool_binding_unavailable")
				require.NotContains(t, recorder.Body.String(), "secret-tool")
				require.NotContains(t, recorder.Body.String(), "replayed-tool")
				if transport == "sse" {
					require.Equal(t, http.StatusOK, recorder.Code)
					require.Equal(t, 1, strings.Count(recorder.Body.String(), `"type":"error"`))
				} else {
					require.Equal(t, http.StatusServiceUnavailable, recorder.Code)
					require.Contains(t, recorder.Header().Get("Content-Type"), "application/json")
				}
			})
		}
	}
}
