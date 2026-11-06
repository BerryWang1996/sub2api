package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type aiResponsesMemoryStore struct {
	rows map[string]string
	ttls map[string]time.Duration
	err  error
}

func (s *aiResponsesMemoryStore) LoadOriginBindings(ctx context.Context, keys []string) ([]string, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if s.err != nil {
		return nil, s.err
	}
	rows := make([]string, len(keys))
	for i, key := range keys {
		rows[i] = s.rows[key]
	}
	return rows, nil
}

func (s *aiResponsesMemoryStore) SaveOriginBinding(ctx context.Context, key, value string, ttl time.Duration) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if s.err != nil {
		return s.err
	}
	if s.rows == nil {
		s.rows, s.ttls = make(map[string]string), make(map[string]time.Duration)
	}
	if old := s.rows[key]; old != "" && old != value {
		s.rows[key] = "conflict"
		return errors.New("conflict")
	}
	s.rows[key], s.ttls[key] = value, ttl
	return nil
}

func newAIResponsesWriterTest(stream bool, bind func(string, string, string) error) (*aiToolWriter, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	if stream {
		c.Header("Content-Type", "text/event-stream")
	} else {
		c.Header("Content-Type", "application/json")
	}
	return newAIResponsesWriter(c.Writer, bind, nil), recorder
}

func TestAIResponsesWriterBindsEveryIDBeforeFragmentedEvent(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n"} {
		t.Run(fmt.Sprintf("newline=%q", newline), func(t *testing.T) {
			bound := make(map[string]string)
			var recorder *httptest.ResponseRecorder
			writer, output := newAIResponsesWriterTest(true, func(kind, id, response string) error {
				require.NotContains(t, recorder.Body.String(), id, "ID must not be exposed before persistence")
				bound[kind+":"+id] = response
				return nil
			})
			recorder = output
			_, err := writer.WriteString(": heartbeat\n\n")
			require.NoError(t, err)
			frames := []string{
				`{"type":"response.created","response":{"id":"resp_A","output":[]}}`,
				`{"type":"response.output_item.added","item":{"type":"function_call","id":"item_A","call_id":"call_A","arguments":""}}`,
				`{"type":"response.function_call_arguments.delta","item_id":"item_A","call_id":"call_A","delta":"{}"}`,
				`{"type":"response.output_item.added","item":{"type":"reasoning","id":"reason_A","encrypted_content":"opaque synthetic"}}`,
			}
			for _, payload := range frames {
				frame := strings.ReplaceAll("data: "+payload+"\n\n", "\n", newline)
				before := output.Body.String()
				for i := range len(frame) - 1 {
					_, err = writer.WriteString(frame[i : i+1])
					require.NoError(t, err)
					writer.Flush()
					require.Equal(t, before, output.Body.String())
				}
				_, err = writer.WriteString(frame[len(frame)-1:])
				require.NoError(t, err)
				require.Equal(t, before+frame, output.Body.String())
			}
			require.NoError(t, writer.finish())
			require.Equal(t, map[string]string{"response:resp_A": "resp_A", "item:item_A": "resp_A", "tool_call:call_A": "resp_A", "item:reason_A": "resp_A", "opaque:" + service.AIRoutingOpaqueOriginID("opaque synthetic"): "resp_A"}, bound)
		})
	}
}

func TestAIResponsesWriterJSONFailureAndCancelStopsExposure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			expected := errors.New("synthetic binding conflict")
			writer, output := newAIResponsesWriterTest(false, func(kind, _, _ string) error {
				if fail && kind == "tool_call" {
					return expected
				}
				return nil
			})
			writer.onFailure = func(error) { cancel() }
			payload := `{"id":"resp_A","output":[{"id":"item_A","type":"function_call","call_id":"call_A","arguments":{"type":"custom","Type":"allowed","n":1e1000}}]}`
			_, err := writer.WriteString(payload[:25])
			require.NoError(t, err)
			_, err = writer.WriteString(payload[25:])
			require.NoError(t, err)
			writer.Flush()
			require.Empty(t, output.Body.String())
			err = writer.finish()
			if fail {
				require.ErrorIs(t, err, expected)
				require.ErrorIs(t, ctx.Err(), context.Canceled)
				require.True(t, writer.Written())
				_, err = writer.WriteString(payload)
				require.ErrorIs(t, err, expected)
				writer.Flush()
				require.Empty(t, output.Body.String())
			} else {
				require.NoError(t, err)
				require.Equal(t, payload, output.Body.String())
			}
		})
	}
}

func TestAIResponsesWriterRejectsAmbiguousOrUnboundIDs(t *testing.T) {
	for name, payload := range map[string]string{
		"duplicate":        `{"id":"resp_A","id":"resp_B"}`,
		"response-case":    `{"id":"resp_A","Response":{"id":"resp_B"}}`,
		"item-case":        `{"id":"resp_A","output":[{"id":"item_A","Call_ID":"call_A"}]}`,
		"no-response":      `{"type":"response.output_item.added","item":{"id":"item_A","type":"function_call","call_id":"call_A"}}`,
		"no-call":          `{"id":"resp_A","output":[{"id":"item_A","type":"function_call"}]}`,
		"changed-response": `{"response_id":"resp_A","response":{"id":"resp_B"}}`,
		"numeric-id":       `{"id":"resp_A","output":[{"id":12}]}`,
		"conversation":     `{"id":"resp_A","conversation":{"id":"conv_A"},"output":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			writer, output := newAIResponsesWriterTest(false, func(string, string, string) error { return nil })
			_, err := writer.WriteString(payload)
			require.NoError(t, err)
			require.Error(t, writer.finish())
			require.Empty(t, output.Body.String())
		})
	}
}

func TestAIResponsesWriterRetainsBoundsAndErrorStatusGuard(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, extra := range []int{0, 1} {
			t.Run(fmt.Sprintf("stream=%v/extra=%d", stream, extra), func(t *testing.T) {
				prefix, suffix, limit := `{"id":"resp_A","padding":"`, `"}`, aiToolJSONLimit
				if stream {
					prefix, suffix, limit = "data: "+prefix, suffix+"\n\n", aiToolEventLimit
				}
				payload := prefix + strings.Repeat("x", limit-len(prefix)-len(suffix)+extra) + suffix
				writer, output := newAIResponsesWriterTest(stream, func(string, string, string) error { return nil })
				_, err := writer.WriteString(payload)
				if err == nil {
					err = writer.finish()
				}
				if extra > 0 {
					require.Error(t, err)
					require.Empty(t, output.Body.String())
				} else {
					require.NoError(t, err)
					require.Equal(t, payload, output.Body.String())
				}
			})
		}
	}
	writer, output := newAIResponsesWriterTest(false, func(string, string, string) error { return errors.New("unavailable") })
	writer.WriteHeader(400)
	_, err := writer.WriteString(`{"response":{"id":"resp_failed"}}`)
	require.NoError(t, err)
	require.Error(t, writer.finish())
	require.Empty(t, output.Body.String(), "error responses cannot expose unbound resource identifiers")
}

func TestAIResponsesWriterToolCountAndIncompleteEvent(t *testing.T) {
	for _, count := range []int{64, 65} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			items := make([]string, count)
			for i := range count {
				items[i] = fmt.Sprintf(`{"type":"function_call","call_id":"call_%d"}`, i)
			}
			payload := `{"id":"resp_A","output":[` + strings.Join(items, ",") + `]}`
			writer, output := newAIResponsesWriterTest(false, func(string, string, string) error { return nil })
			_, err := writer.WriteString(payload)
			require.NoError(t, err)
			err = writer.finish()
			if count == 64 {
				require.NoError(t, err)
				require.Equal(t, payload, output.Body.String())
			} else {
				require.Error(t, err)
				require.Empty(t, output.Body.String())
			}
		})
	}
	writer, output := newAIResponsesWriterTest(true, func(string, string, string) error { t.Fatal("incomplete event cannot bind"); return nil })
	_, err := writer.WriteString("data: {\"response_id\":\"resp_A\"}\n")
	require.NoError(t, err)
	require.Error(t, writer.finish())
	require.Empty(t, output.Body.String())
}

func TestAIResponsesOriginKeyIsolationAndMetadata(t *testing.T) {
	key := &service.APIKey{ID: 3, UserID: 2, Group: &service.Group{ID: 1}}
	store := &aiResponsesMemoryStore{}
	origin := service.AIRoutingOriginBinding{Candidate: "c1", Account: 7, Model: "model", Platform: "openai", AccountTarget: 7, ResponseID: "resp_A", Native: true, Store: true}
	refs := []service.AIRoutingOriginRef{{Kind: "response", ID: "resp_A"}, {Kind: "tool_call", ID: "call_A"}}
	for _, ref := range refs {
		require.NoError(t, saveAIResponsesBinding(t.Context(), store, key, ref, origin))
		storageKey, err := aiResponsesOriginKey(key, ref)
		require.NoError(t, err)
		require.Equal(t, service.AIRoutingResponseOriginTTL, store.ttls[storageKey])
	}
	req := service.AIRoutingRequest{PublicModel: "different-authorized-alias", PreviousResponseID: "resp_A", RequiresOrigin: true, OriginRefs: refs}
	target, loaded, err := loadAIResponsesBinding(t.Context(), store, key, req)
	require.NoError(t, err)
	require.Equal(t, aiResponsesToolBinding(origin), target)
	require.Equal(t, &origin, loaded)
	for _, other := range []*service.APIKey{
		{ID: 4, UserID: 2, Group: &service.Group{ID: 1}},
		{ID: 3, UserID: 4, Group: &service.Group{ID: 1}},
		{ID: 3, UserID: 2, Group: &service.Group{ID: 4}},
	} {
		_, _, err = loadAIResponsesBinding(t.Context(), store, other, req)
		require.Error(t, err)
	}
	wrongKind := req
	wrongKind.OriginRefs = []service.AIRoutingOriginRef{{Kind: "item", ID: "call_A"}}
	_, _, err = loadAIResponsesBinding(t.Context(), store, key, wrongKind)
	require.Error(t, err)
	otherOrigin := origin
	otherOrigin.Account, otherOrigin.AccountTarget = 8, 8
	otherOrigin.ResponseID = "resp_other"
	require.NoError(t, saveAIResponsesBinding(t.Context(), store, key, service.AIRoutingOriginRef{Kind: "response", ID: "resp_other"}, otherOrigin))
	otherRef := service.AIRoutingOriginRef{Kind: "item", ID: "item_other"}
	require.NoError(t, saveAIResponsesBinding(t.Context(), store, key, otherRef, otherOrigin))
	req.OriginRefs = append(refs, otherRef)
	_, _, err = loadAIResponsesBinding(t.Context(), store, key, req)
	require.ErrorContains(t, err, "different execution origins")
	for _, native := range []bool{false, true} {
		for _, stored := range []bool{false, true} {
			t.Run(fmt.Sprintf("native=%v/store=%v", native, stored), func(t *testing.T) {
				modified := origin
				modified.Native, modified.Store = native, stored
				raw, marshalErr := json.Marshal(modified)
				require.NoError(t, marshalErr)
				storageKey, keyErr := aiResponsesOriginKey(key, refs[0])
				require.NoError(t, keyErr)
				store.rows[storageKey] = string(raw)
				_, _, loadErr := loadAIResponsesBinding(t.Context(), store, key, service.AIRoutingRequest{OriginRefs: refs[:1], RequiresOrigin: true})
				if native && stored {
					require.NoError(t, loadErr)
					require.Equal(t, service.AIRoutingResponseOriginTTL, modified.TTL("tool_call"))
				} else {
					require.Error(t, loadErr)
					require.Equal(t, service.AIRoutingToolOriginTTL, modified.TTL("tool_call"))
				}
				require.Equal(t, service.AIRoutingResponseOriginTTL, modified.TTL("response"))
			})
		}
	}
}
