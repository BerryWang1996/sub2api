//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type aiBackgroundDisconnectWriter struct {
	header     http.Header
	once       sync.Once
	disconnect func()
}

func (w *aiBackgroundDisconnectWriter) Header() http.Header { return w.header }
func (w *aiBackgroundDisconnectWriter) WriteHeader(int)     {}
func (w *aiBackgroundDisconnectWriter) Flush()              {}
func (w *aiBackgroundDisconnectWriter) Write([]byte) (int, error) {
	w.once.Do(w.disconnect)
	return 0, errors.New("synthetic client disconnected")
}

func TestAIBackgroundObserverRecordsCreatedDuringRealHTTPDrain(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprint(passthrough), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			allowResponse := make(chan struct{})
			var seen atomic.Int32
			var terminalSeen atomic.Int32
			ctx = WithAIRoutingBackgroundResponseObserver(ctx, func(observed context.Context, account int64, id string) error {
				require.ErrorIs(t, observed.Err(), context.Canceled)
				require.EqualValues(t, 7, account)
				require.Equal(t, "resp_after_disconnect", id)
				seen.Add(1)
				return nil
			})
			ctx = WithAIRoutingBackgroundTerminalObserver(ctx, func(observed context.Context, account int64, id string, terminal AIRoutingBackgroundTerminal) error {
				require.ErrorIs(t, observed.Err(), context.Canceled)
				require.EqualValues(t, 7, account)
				require.Equal(t, "resp_after_disconnect", id)
				require.True(t, terminal.UsageKnown)
				require.Equal(t, 3, terminal.InputTokens)
				require.Equal(t, 1, terminal.OutputTokens)
				terminalSeen.Add(1)
				return nil
			})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				select {
				case <-allowResponse:
				case <-r.Context().Done():
					return
				}
				_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_after_disconnect\",\"status\":\"queued\",\"usage\":null}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_after_disconnect\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n")
			}))
			defer server.Close()
			response, err := server.Client().Get(server.URL)
			require.NoError(t, err)
			defer func() { _ = response.Body.Close() }()
			recorder := &aiBackgroundDisconnectWriter{header: make(http.Header), disconnect: func() { cancel(); close(allowResponse) }}
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
			s := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{StreamKeepaliveInterval: 1}}, toolCorrector: NewCodexToolCorrector()}
			account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
			if passthrough {
				_, err = s.handleStreamingResponsePassthrough(ctx, response, c, account, time.Now(), "gpt-5.1", "gpt-5.1")
			} else {
				_, err = s.handleStreamingResponse(ctx, response, c, account, time.Now(), "gpt-5.1", "gpt-5.1")
			}
			require.NoError(t, err)
			require.EqualValues(t, 1, seen.Load(), "the ID is durable even when the client writer is no longer called")
			require.EqualValues(t, 1, terminalSeen.Load(), "terminal usage survives the same cancelled drain")
		})
	}
}

func TestAIBackgroundObserverTerminalEvidenceIsValidatedBeforeExposure(t *testing.T) {
	for _, status := range []string{"completed", "failed", "cancelled", "incomplete"} {
		for _, sse := range []bool{false, true} {
			t.Run(fmt.Sprint(status, sse), func(t *testing.T) {
				var sequence []string
				ctx := WithAIRoutingBackgroundResponseObserver(t.Context(), func(context.Context, int64, string) error {
					sequence = append(sequence, "id")
					return nil
				})
				ctx = WithAIRoutingBackgroundTerminalObserver(ctx, func(_ context.Context, account int64, id string, terminal AIRoutingBackgroundTerminal) error {
					require.EqualValues(t, 7, account)
					require.Equal(t, "resp_evidence", id)
					require.Equal(t, status, terminal.Status)
					require.True(t, terminal.UsageKnown, "explicit zero usage remains known")
					require.Zero(t, terminal.InputTokens)
					require.Zero(t, terminal.OutputTokens)
					sequence = append(sequence, "terminal")
					return nil
				})
				body := fmt.Sprintf(`{"id":"resp_evidence","status":%q,"model":"gpt-5.1","usage":{"input_tokens":0,"output_tokens":0},"output":[]}`, status)
				if sse {
					body = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_evidence\",\"status\":\"queued\"}}\n\nevent: response." + status + "\ndata: {\"type\":\"response." + status + "\",\"response\":" + body + "}\n\n"
				}
				require.NoError(t, observeAIBackgroundResponseBody(ctx, 7, []byte(body)))
				require.Equal(t, []string{"id", "terminal"}, sequence)
			})
		}
	}
	for _, usage := range []string{"null", `{}`, `{"input_tokens":1}`, `{"input_tokens":1,"output_tokens":-1}`, `{"input_tokens":1,"output_tokens":1,"output_tokens_details":{"audio_tokens":1}}`} {
		calls := 0
		ctx := WithAIRoutingBackgroundTerminalObserver(t.Context(), func(_ context.Context, _ int64, _ string, terminal AIRoutingBackgroundTerminal) error {
			calls++
			require.False(t, terminal.UsageKnown)
			return nil
		})
		require.NoError(t, ObserveAIRoutingBackgroundResponsePayload(ctx, 7, []byte(`{"id":"resp_evidence","status":"completed","usage":`+usage+`}`), ""))
		require.Equal(t, 1, calls)
	}
	for _, status := range []string{"queued", "in_progress"} {
		ctx := WithAIRoutingBackgroundTerminalObserver(t.Context(), func(context.Context, int64, string, AIRoutingBackgroundTerminal) error {
			t.Fatal("progress usage must not settle the job")
			return nil
		})
		require.NoError(t, ObserveAIRoutingBackgroundResponsePayload(ctx, 7, []byte(fmt.Sprintf(`{"id":"resp_evidence","status":%q,"usage":{"input_tokens":10,"output_tokens":2}}`, status)), ""))
	}
}

func TestAIBackgroundObserverRejectsInvalidTerminalAndStopsOnPersistenceFailure(t *testing.T) {
	for _, body := range []string{
		`{"id":"resp_evidence","Status":"completed","usage":{"input_tokens":1,"output_tokens":1}}`,
		`{"id":"resp_evidence","status":"completed","model":42,"usage":{"input_tokens":1,"output_tokens":1}}`,
		`{"id":"resp_evidence","status":"completed","usage":{"input_tokens":1,"input_tokens":2,"output_tokens":1}}`,
		`{"type":"response.created","response":{"id":"resp_evidence","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"type":"response.completed","response":{"id":"resp_evidence","status":"failed","usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"type":"response.completed","response":{"id":"resp_evidence","usage":{"input_tokens":1,"output_tokens":1}}}`,
	} {
		ctx := WithAIRoutingBackgroundTerminalObserver(t.Context(), func(context.Context, int64, string, AIRoutingBackgroundTerminal) error {
			t.Fatal("invalid evidence cannot reach persistence")
			return nil
		})
		require.True(t, IsAIRoutingCapacityError(ObserveAIRoutingBackgroundResponsePayload(ctx, 7, []byte(body), "")), body)
	}
	for _, passthrough := range []bool{false, true} {
		for _, streaming := range []bool{false, true} {
			calls := 0
			ctx := WithAIRoutingBackgroundTerminalObserver(t.Context(), func(context.Context, int64, string, AIRoutingBackgroundTerminal) error {
				calls++
				return errors.New("synthetic terminal write failure")
			})
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
			s := &OpenAIGatewayService{cfg: &config.Config{}, toolCorrector: NewCodexToolCorrector()}
			body := `{"id":"resp_private","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`
			if streaming {
				body = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":" + body + "}\n\n"
			}
			resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
			account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
			var err error
			if streaming && passthrough {
				_, err = s.handleStreamingResponsePassthrough(ctx, resp, c, account, time.Now(), "gpt-5.1", "gpt-5.1")
			} else if streaming {
				_, err = s.handleStreamingResponse(ctx, resp, c, account, time.Now(), "gpt-5.1", "gpt-5.1")
			} else if passthrough {
				_, err = s.handleNonStreamingResponsePassthrough(ctx, resp, c, account, "gpt-5.1", "gpt-5.1")
			} else {
				_, err = s.handleNonStreamingResponse(ctx, resp, c, account, "gpt-5.1", "gpt-5.1")
			}
			require.True(t, IsAIRoutingCapacityError(err))
			require.Equal(t, 1, calls)
			require.NotContains(t, recorder.Body.String(), "resp_private")
		}
	}
}

func TestAIBackgroundObserverUnaryQueuedPreservesUnknownUsage(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, status := range []string{"queued", "in_progress", "completed", "cancelled"} {
			t.Run(fmt.Sprint(passthrough)+status, func(t *testing.T) {
				payload := fmt.Sprintf(`{"id":"resp_queued","object":"response","status":%q,"model":"gpt-5.1","usage":null,"output":[]}`, status)
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				ctx := WithAIRoutingBackgroundSubmission(t.Context(), nil)
				calls := 0
				ctx = WithAIRoutingBackgroundResponseObserver(ctx, func(context.Context, int64, string) error {
					calls++
					require.Empty(t, recorder.Body.String())
					return nil
				})
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
				s := &OpenAIGatewayService{cfg: &config.Config{}, toolCorrector: NewCodexToolCorrector()}
				account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
				resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(payload))}
				var err error
				if passthrough {
					_, err = s.handleNonStreamingResponsePassthrough(ctx, resp, c, account, "gpt-5.1", "gpt-5.1")
				} else {
					_, err = s.handleNonStreamingResponse(ctx, resp, c, account, "gpt-5.1", "gpt-5.1")
				}
				require.NoError(t, err)
				require.Equal(t, 1, calls)
				require.Contains(t, recorder.Body.String(), `"usage":null`)
				require.NotContains(t, recorder.Body.String(), `"input_tokens":0`)
			})
		}
	}
	// The same missing usage retains the pre-existing ordinary validation.
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	s := &OpenAIGatewayService{cfg: &config.Config{}}
	_, err := s.handleNonStreamingResponse(t.Context(), &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"resp_queued","status":"queued","usage":null}`))}, c, &Account{ID: 7, Type: AccountTypeAPIKey}, "gpt-5.1", "gpt-5.1")
	require.ErrorContains(t, err, "invalid json")
}

func TestAIBackgroundObserverRejectsAmbiguityAndPersistenceFailureBeforeExposure(t *testing.T) {
	for _, body := range []string{`{"id":"resp_one","id":"resp_two"}`, `{"id":"resp_one","Id":"resp_two"}`, `{"response":{"id":"resp_one","Id":"resp_two"}}`, `{"id":"resp_one","response_id":"resp_two"}`, `{"response":{"id":42}}`, strings.Repeat("x", (4<<20)+1)} {
		calls := 0
		ctx := WithAIRoutingBackgroundResponseObserver(t.Context(), func(context.Context, int64, string) error { calls++; return nil })
		require.True(t, IsAIRoutingCapacityError(observeAIBackgroundResponseBody(ctx, 7, []byte(body))))
		require.Zero(t, calls)
	}
	for _, passthrough := range []bool{false, true} {
		ctx := WithAIRoutingBackgroundResponseObserver(t.Context(), func(context.Context, int64, string) error { return errors.New("synthetic SQL unavailable") })
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
		s := &OpenAIGatewayService{cfg: &config.Config{}, toolCorrector: NewCodexToolCorrector()}
		resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_private\"}}\n\n"))}
		account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
		var err error
		if passthrough {
			_, err = s.handleStreamingResponsePassthrough(ctx, resp, c, account, time.Now(), "gpt-5.1", "gpt-5.1")
		} else {
			_, err = s.handleStreamingResponse(ctx, resp, c, account, time.Now(), "gpt-5.1", "gpt-5.1")
		}
		require.True(t, IsAIRoutingCapacityError(err))
		require.NotContains(t, recorder.Body.String(), "resp_private")
	}
}

type backgroundDeleteRepo struct {
	AIRoutingBackgroundRepository
	job *AIRoutingBackgroundJob
}

func (r *backgroundDeleteRepo) GetBackgroundByResponse(context.Context, int64, string) (*AIRoutingBackgroundJob, error) {
	return r.job, nil
}
func TestAIBackgroundDeleteRequiresSettledSyncedStrictOwner(t *testing.T) {
	group := int64(3)
	key := &APIKey{ID: 2, UserID: 1, GroupID: &group, Group: &Group{ID: group}}
	j := &AIRoutingBackgroundJob{SourceGroupID: 3, UserID: 1, APIKeyID: 2, AccountID: 7, ResponseID: "resp_done", Status: AIBackgroundSettled, CacheSynced: true}
	s := NewAIRoutingResponsesBackgroundService(&backgroundDeleteRepo{job: j}, nil)
	require.NoError(t, s.CanDeleteResponse(t.Context(), key, 7, "resp_done"))
	for _, status := range []string{AIBackgroundAdmitted, AIBackgroundSubmitting, AIBackgroundActive, AIBackgroundUsageUnknown, AIBackgroundReady, AIBackgroundReleased} {
		j.Status = status
		require.ErrorIs(t, s.CanDeleteResponse(t.Context(), key, 7, "resp_done"), ErrAIBackgroundRecoveryPending)
	}
	j.Status = AIBackgroundSettled
	j.CacheSynced = false
	require.Error(t, s.CanDeleteResponse(t.Context(), key, 7, "resp_done"))
	j.CacheSynced = true
	key.ID++
	require.Error(t, s.CanDeleteResponse(t.Context(), key, 7, "resp_done"))
	key.ID--
	key.UserID++
	require.Error(t, s.CanDeleteResponse(t.Context(), key, 7, "resp_done"))
	key.UserID--
	key.Group.ID++
	require.Error(t, s.CanDeleteResponse(t.Context(), key, 7, "resp_done"))
	key.Group.ID--
	require.Error(t, s.CanDeleteResponse(t.Context(), key, 8, "resp_done"))
	require.Error(t, s.CanDeleteResponse(t.Context(), key, 7, "resp_other"))
}
