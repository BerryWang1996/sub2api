package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/testutil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAIChatWriterBindsFragmentedIDsBeforeExposure(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Header("Content-Type", "text/event-stream")
	var bound []string
	w := newAIChatWriter(c.Writer, func(id string) error {
		require.NotContains(t, recorder.Body.String(), "call_")
		bound = append(bound, id)
		return nil
	}, nil)
	frames := []string{
		`{"choices":[{"index":0,"delta":{"content":"visible text"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_","type":"function","function":{"name":"scan","arguments":"{"}}]},"finish_reason":null},{"index":1,"delta":{"tool_calls":[{"index":0,"id":"call_b","type":"function","function":{"name":"scan","arguments":"{}"}}]},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a"}]},"finish_reason":null}]}`,
		`{"choices":[{"index":1,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"}"}}]},"finish_reason":"tool_calls"}]}`,
	}
	var expected strings.Builder
	for i, frame := range frames {
		wire := "data: " + frame + "\n\n"
		_, _ = expected.WriteString(wire)
		for _, part := range []string{wire[:7], wire[7:]} {
			_, err := w.WriteString(part)
			require.NoError(t, err)
		}
		w.Flush()
		if i < len(frames)-1 {
			require.NotContains(t, recorder.Body.String(), "call_")
		}
		require.Contains(t, recorder.Body.String(), "visible text")
	}
	require.ElementsMatch(t, []string{"call_a", "call_b"}, bound)
	_, err := w.WriteString("data: [DONE]\n\n")
	require.NoError(t, err)
	require.NoError(t, w.finish())
	require.Equal(t, expected.String()+"data: [DONE]\n\n", recorder.Body.String())
}

func TestAIChatWriterRejectsUnprovenAndAmbiguousTools(t *testing.T) {
	for _, tc := range []struct {
		name, wire        string
		stream, saveFails bool
	}{
		{"JSON storage failure", `{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_secret","type":"function","function":{"name":"scan","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`, false, true},
		{"legacy function", `{"choices":[{"index":0,"message":{"role":"assistant","function_call":{"name":"scan","arguments":"{}"}},"finish_reason":"function_call"}]}`, false, false},
		{"missing ID", `{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"scan","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`, false, false},
		{"duplicate choice tool ID", `{"choices":[{"index":0,"message":{"tool_calls":[{"id":"call_same","function":{"name":"scan","arguments":"{}"}}]},"finish_reason":"tool_calls"},{"index":1,"message":{"tool_calls":[{"id":"call_same","function":{"name":"scan","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`, false, false},
		{"truncated tool stream", "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_secret\",\"function\":{\"name\":\"scan\"}}]},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n", true, false},
		{"case collision", `{"choices":[{"index":0,"message":{"tool_calls":[],"Tool_Calls":[{"id":"call_secret"}]}}]}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			if tc.stream {
				c.Header("Content-Type", "text/event-stream")
			}
			failures := 0
			w := newAIChatWriter(c.Writer, func(string) error {
				if tc.saveFails {
					return errors.New("synthetic storage failure")
				}
				return nil
			}, func(error) { failures++ })
			_, err := w.WriteString(tc.wire)
			if err == nil {
				err = w.finish()
			}
			require.Error(t, err)
			require.Equal(t, 1, failures)
			require.Empty(t, recorder.Body.String(), "unbound tool bytes must not escape")
		})
	}
}

func TestAIChatBindingSeparatesProtocolsKeysAndConflictingOrigins(t *testing.T) {
	store, _ := testutil.NewRedisAIRoutingStore(t)
	key := &service.APIKey{ID: 9, UserID: 42, Group: &service.Group{ID: 17}}
	target := aiToolBinding{Candidate: "picked", Account: 75, Model: "executor", Platform: service.PlatformOpenAI, GroupTarget: 61}
	ctx := context.Background()
	req := service.AIRoutingRequest{PublicModel: "changed-alias", ToolResults: []string{"call_1"}, RequiresOrigin: true}
	require.NoError(t, saveAIToolBinding(ctx, store, key, req.PublicModel, "call_1", target))
	_, err := loadAIChatBinding(ctx, store, key, req)
	require.Error(t, err, "Messages origin cannot authorize Chat")
	require.NoError(t, saveAIChatBinding(ctx, store, key, "call_1", target))
	got, err := loadAIChatBinding(ctx, store, key, req)
	require.NoError(t, err)
	require.Equal(t, target, got)
	key.ID++
	_, err = loadAIChatBinding(ctx, store, key, req)
	require.Error(t, err)
	key.ID--
	other := target
	other.Account++
	require.Error(t, saveAIChatBinding(ctx, store, key, "call_1", other))
	_, err = loadAIChatBinding(ctx, store, key, req)
	require.Error(t, err, "cross-instance conflicts poison the binding")
}

func TestAIChatWriterRejectsDuplicateToolPosition(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Header("Content-Type", "text/event-stream")
	w := newAIChatWriter(c.Writer, func(string) error { t.Fatal("ambiguous frame cannot bind"); return nil }, nil)
	_, err := w.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_a\",\"function\":{\"name\":\"scan\"}},{\"index\":0,\"id\":\"call_b\"}]},\"finish_reason\":null}]}\n\n")
	require.Error(t, err)
	require.Empty(t, rec.Body.String())
}

func TestAIChatWriterDoneIsATerminalBoundary(t *testing.T) {
	for _, late := range []string{
		`{"choices":[{"index":0,"delta":{"content":"late"},"finish_reason":null}]}`,
		`{"choices":[],"usage":{"prompt_tokens":1}}`,
		`{"choices":[{"index":1,"delta":{"tool_calls":[{"index":0,"id":"call_late","function":{"name":"scan"}}]},"finish_reason":null}]}`,
	} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Header("Content-Type", "text/event-stream")
		w := newAIChatWriter(c.Writer, func(string) error { return nil }, nil)
		_, err := w.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		require.NoError(t, err)
		before := rec.Body.String()
		_, err = w.WriteString("data: " + late + "\n\n")
		require.Error(t, err)
		require.Equal(t, before, rec.Body.String())
	}
}

func TestAIChatWriterChoiceCompletionIsTerminal(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Header("Content-Type", "text/event-stream")
	w := newAIChatWriter(c.Writer, func(string) error { return nil }, nil)
	_, err := w.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	require.NoError(t, err)
	before := rec.Body.String()
	_, err = w.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"late\"},\"finish_reason\":null}]}\n\n")
	require.Error(t, err)
	require.Equal(t, before, rec.Body.String())
}

func TestAIChatWriterEarlyDoneEOFAndUpstreamError(t *testing.T) {
	start := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_hidden\",\"function\":{\"name\":\"scan\"}}]},\"finish_reason\":null}]}\n\n"
	for _, end := range []string{"", "data: [DONE]\n\n", "data: {\"error\":{\"type\":\"upstream_error\",\"message\":\"synthetic failure\"}}\n\n"} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Header("Content-Type", "text/event-stream")
		bound := 0
		w := newAIChatWriter(c.Writer, func(string) error { bound++; return nil }, nil)
		_, err := w.WriteString(start)
		require.NoError(t, err)
		_, err = w.WriteString(end)
		if err == nil {
			err = w.finish()
		}
		if strings.Contains(end, "synthetic failure") {
			require.NoError(t, err)
			require.Contains(t, rec.Body.String(), "synthetic failure")
		} else {
			require.Error(t, err)
			require.Empty(t, rec.Body.String())
		}
		require.Zero(t, bound)
		require.NotContains(t, rec.Body.String(), "call_hidden")
	}
}

func TestAIChatWriterBoundsAndSameNameParallelTools(t *testing.T) {
	for _, count := range []int{64, 65} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Header("Content-Type", "application/json")
		calls := make([]string, count)
		for i := range calls {
			calls[i] = fmt.Sprintf(`{"id":"call_%d","type":"function","function":{"name":"same","arguments":"{}"}}`, i)
		}
		body := `{"choices":[{"index":0,"message":{"tool_calls":[` + strings.Join(calls, ",") + `]},"finish_reason":"tool_calls"}]}`
		bound := 0
		w := newAIChatWriter(c.Writer, func(string) error { bound++; require.Empty(t, rec.Body.String()); return nil }, nil)
		_, err := w.WriteString(body)
		require.NoError(t, err)
		err = w.finish()
		if count == 64 {
			require.NoError(t, err)
			require.Equal(t, 64, bound)
			require.JSONEq(t, body, rec.Body.String())
		} else {
			require.Error(t, err)
			require.Empty(t, rec.Body.String())
		}
	}
	for _, limit := range []string{"event", "held", "json"} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		w := newAIChatWriter(c.Writer, func(string) error { return nil }, nil)
		var err error
		if limit == "json" {
			c.Header("Content-Type", "application/json")
			_, err = w.WriteString(strings.Repeat(" ", aiToolJSONLimit+1))
		} else {
			c.Header("Content-Type", "text/event-stream")
			if limit == "event" {
				_, err = w.WriteString("data: " + strings.Repeat(" ", aiToolEventLimit))
			} else {
				_, err = w.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_hidden\",\"function\":{\"name\":\"scan\"}}]},\"finish_reason\":null}]}\n\n")
				require.NoError(t, err)
				padding := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" + strings.Repeat("x", 1<<16) + "\"},\"finish_reason\":null}]}\n\n"
				for i := 0; i < 100 && err == nil; i++ {
					_, err = w.WriteString(padding)
				}
			}
		}
		require.Error(t, err, limit)
		require.Empty(t, rec.Body.String())
		require.LessOrEqual(t, len(w.gate.pending), aiToolJSONLimit)
	}
}
