//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type aiNativeWSConn struct {
	mu       sync.Mutex
	events   chan []byte
	closed   chan struct{}
	onWrite  func([]byte)
	writes   [][]byte
	closeOne sync.Once
}

func (conn *aiNativeWSConn) WriteJSON(ctx context.Context, value any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	wire, err := json.Marshal(value)
	if err != nil {
		return err
	}
	conn.mu.Lock()
	conn.writes = append(conn.writes, wire)
	onWrite := conn.onWrite
	conn.mu.Unlock()
	if onWrite != nil {
		onWrite(wire)
	}
	return nil
}

func (conn *aiNativeWSConn) ReadMessage(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-conn.closed:
		return nil, io.EOF
	case body, ok := <-conn.events:
		if !ok {
			return nil, io.EOF
		}
		return body, nil
	}
}

func (conn *aiNativeWSConn) Ping(context.Context) error { return nil }
func (conn *aiNativeWSConn) Close() error {
	conn.closeOne.Do(func() { close(conn.closed) })
	return nil
}

func aiNativeWSTransportFixture(t *testing.T, revalidate func(context.Context) error, body string) (*AIRoutingWSNativeTurn, *aiNativeWSConn, *Account) {
	t.Helper()
	cfg := &config.Config{}
	cfg.Security.URLAllowlist.Enabled = false
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.APIKeyEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	cfg.Gateway.OpenAIWS.IngressModeDefault = OpenAIWSIngressModeCtxPool
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	cfg.Gateway.OpenAIWS.QueueLimitPerConn = 8
	cfg.Gateway.OpenAIWS.DialTimeoutSeconds = 1
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 1
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 1
	conn := &aiNativeWSConn{events: make(chan []byte, 16), closed: make(chan struct{})}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(&openAIWSClientConnCancelDialer{conn: conn})
	t.Cleanup(pool.Close)
	svc := &OpenAIGatewayService{cfg: cfg, cache: &stubGatewayCache{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), openaiWSPool: pool}
	account := &Account{ID: 9101, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"api_key": "fake-key"}, Extra: map[string]any{"openai_apikey_responses_websockets_v2_mode": OpenAIWSIngressModeCtxPool}}
	ctx := WithCompositeRouteDecision(t.Context(), CompositeRouteDecision{Matched: true, GroupID: 9, PublicModel: "public-alias", UpstreamModel: "gpt-5.1", Source: CompositeRouteSourceAI, AICandidateID: "selected", TargetAccountID: &account.ID, TargetPlatform: account.Platform})
	ctx = WithAIRoutingExecutionRequirements(ctx, AIRoutingExecutionRequirements{Endpoint: AIRoutingEndpointResponsesWS, InputModalities: []string{"text"}, InputBytes: int64(len(body)), MaxOutputTokens: 64, ContextWindowTokens: 500000})
	guard := &GatewayService{accountRepo: &aiOutboundAccountRepo{account: account}}
	ctx = guard.WithAIRoutingFinalCapacityGuard(ctx)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil).WithContext(ctx)
	if revalidate == nil {
		revalidate = func(context.Context) error { return nil }
	}
	turn, err := svc.NewAIRoutingWSNativeTurn(ctx, c, account, []byte(body), AIRoutingWSNativeTurnOptions{PublicModel: "public-alias", Revalidate: revalidate, DrainTimeout: 30 * time.Millisecond,
		CheckSteer: func(checkCtx context.Context, budget AIRoutingWSSteeringBudget) error {
			return guard.CheckAIRoutingWSSteeringCapacity(checkCtx, account.ID, "gpt-5.1", budget)
		},
		ReserveSteer: func(context.Context, AIRoutingWSSteeringBudget) (func(), error) { return func() {}, nil },
	})
	require.NoError(t, err)
	t.Cleanup(turn.Close)
	return turn, conn, account
}

const aiNativeWSCreate = `{"type":"response.create","stream_id":"planner","model":"gpt-5.1","input":"complete native input","max_output_tokens":64,"store":false,"metadata":{"large":9007199254740993}}`

func TestAIRoutingWSNativeTransportPendingSteerKeepsExactContinuationLease(t *testing.T) {
	turn, conn, account := aiNativeWSTransportFixture(t, nil, aiNativeWSCreate)
	conn.onWrite = func(body []byte) {
		if gjson.GetBytes(body, "type").Str == "response.steer" {
			conn.events <- []byte(`{"type":"response.steer.pending","required_input":"tool_outputs"}`)
		}
	}
	conn.events <- []byte(`{"type":"response.created","response":{"id":"resp_tool"}}`)
	conn.events <- []byte(`{"type":"response.completed","response":{"id":"resp_tool","usage":{"input_tokens":3,"output_tokens":2}}}`)
	result, err := turn.Run(func(event []byte) error {
		if gjson.GetBytes(event, "type").Str == "response.created" {
			return turn.Steer(t.Context(), []byte(`{"type":"response.steer","previous_response_id":"resp_tool","input":"after tools"}`))
		}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 3, result.Usage.InputTokens)
	ctx := turn.gin.Request.Context()
	next, err := turn.service.NewAIRoutingWSNativeTurn(ctx, turn.gin, account, []byte(aiNativeWSCreate), AIRoutingWSNativeTurnOptions{PublicModel: "public-alias", PreferredConnID: turn.ConnID(), ForcePreferredConn: true, Revalidate: func(context.Context) error { return nil }})
	require.NoError(t, err)
	t.Cleanup(next.Close)
	require.Equal(t, turn.ConnID(), next.ConnID())
	conn.events <- []byte(`{"type":"response.created","response":{"id":"resp_after_tools"}}`)
	conn.events <- []byte(`{"type":"response.completed","response":{"id":"resp_after_tools","usage":{"input_tokens":4,"output_tokens":1}}}`)
	result, err = next.Run(func([]byte) error { return nil })
	require.NoError(t, err)
	require.Equal(t, 4, result.Usage.InputTokens)
	require.Len(t, conn.writes, 3)
}

func TestAIRoutingWSNativeTransportCancellationDrainIsBounded(t *testing.T) {
	turn, conn, _ := aiNativeWSTransportFixture(t, nil, aiNativeWSCreate)
	conn.events <- []byte(`{"type":"response.created","response":{"id":"resp_cancel"}}`)
	started := time.Now()
	result, err := turn.Run(func([]byte) error { turn.Cancel(); return nil })
	require.Error(t, err)
	require.True(t, result.ClientDisconnect)
	require.Less(t, time.Since(started), 500*time.Millisecond)
	select {
	case <-conn.closed:
	default:
		t.Fatal("unconsumed canceled connection was returned healthy")
	}
	require.Len(t, conn.writes, 1, "no invented cancel command or automatic replay")
}

func TestAIRoutingWSNativeTransportBindingFailureRetainsCauseAndUsage(t *testing.T) {
	turn, conn, _ := aiNativeWSTransportFixture(t, nil, aiNativeWSCreate)
	conn.events <- []byte(`{"type":"response.created","response":{"id":"resp_binding"}}`)
	conn.events <- []byte(`{"type":"response.completed","response":{"id":"resp_binding","usage":{"input_tokens":6,"output_tokens":2}}}`)
	failure := errors.New("synthetic origin storage failure")
	result, err := turn.Run(func([]byte) error { return failure })
	require.Same(t, failure, err)
	require.True(t, result.ClientDisconnect)
	require.Equal(t, 6, result.Usage.InputTokens)
}

func TestAIRoutingWSNativeTransportUsesRealPoolAndImmutableWire(t *testing.T) {
	checks := 0
	turn, conn, _ := aiNativeWSTransportFixture(t, func(context.Context) error { checks++; return nil }, aiNativeWSCreate)
	conn.events <- []byte(`{"type":"response.created","stream_id":"planner","response":{"id":"resp_first","model":"gpt-5.1"}}`)
	conn.events <- []byte(`{"type":"response.completed","stream_id":"planner","response":{"id":"resp_first","model":"gpt-5.1","usage":{"input_tokens":5,"output_tokens":2}}}`)
	var events []string
	result, err := turn.Run(func(event []byte) error { events = append(events, string(event)); return nil })
	require.NoError(t, err)
	require.Equal(t, 2, checks, "latest admission runs before dial and again after lease acquisition")
	require.Len(t, conn.writes, 1)
	require.Equal(t, "9007199254740993", gjson.GetBytes(conn.writes[0], "metadata.large").Raw)
	require.Equal(t, "planner", gjson.GetBytes(conn.writes[0], "stream_id").Str)
	require.Equal(t, "complete native input", gjson.GetBytes(conn.writes[0], "input").Str)
	require.Len(t, events, 2)
	require.Contains(t, events[0], "response.created")
	require.NotEmpty(t, turn.ConnID())
	require.Equal(t, "public-alias", result.Model)
	require.Equal(t, "gpt-5.1", result.UpstreamModel)
	require.Equal(t, "gpt-5.1", result.BillingModel)
	require.Equal(t, 5, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.OutputTokens)
}

func TestAIRoutingWSNativeTransportSteerUsesRunningLeaseAndSumsSuccessorUsage(t *testing.T) {
	checks := 0
	turn, conn, _ := aiNativeWSTransportFixture(t, func(context.Context) error { checks++; return nil }, aiNativeWSCreate)
	conn.onWrite = func(body []byte) {
		if gjson.GetBytes(body, "type").Str == "response.create" {
			conn.events <- []byte(`{"type":"response.created","response":{"id":"resp_original","model":"gpt-5.1"}}`)
			return
		}
		conn.events <- []byte(`{"type":"response.steer.accepted","steer":{"id":"steer_a","previous_response_id":"resp_original"}}`)
		conn.events <- []byte(`{"type":"response.incomplete","response":{"id":"resp_original","incomplete_details":{"reason":"steered"},"usage":{"input_tokens":3,"output_tokens":1}}}`)
		conn.events <- []byte(`{"type":"response.created","response":{"id":"resp_successor","model":"gpt-5.1"}}`)
		conn.events <- []byte(`{"type":"response.completed","response":{"id":"resp_successor","usage":{"input_tokens":2,"output_tokens":4}}}`)
	}
	var events []string
	result, err := turn.Run(func(event []byte) error {
		events = append(events, string(event))
		if gjson.GetBytes(event, "response.id").Str == "resp_original" && gjson.GetBytes(event, "type").Str == "response.created" {
			require.Error(t, turn.Steer(t.Context(), []byte(`{"type":"response.steer","previous_response_id":"resp_other_lane","input":"bad"}`)))
			return turn.Steer(t.Context(), []byte(`{"type":"response.steer","previous_response_id":"resp_original","input":"updated instruction"}`))
		}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 3, checks)
	require.Len(t, conn.writes, 2)
	require.Equal(t, "response.steer", gjson.GetBytes(conn.writes[1], "type").Str)
	require.Len(t, events, 5, "do not stop at the original steered terminal or successor.created")
	require.Equal(t, "resp_successor", result.ResponseID)
	require.Equal(t, 5, result.Usage.InputTokens)
	require.Equal(t, 5, result.Usage.OutputTokens)
}

func TestAIRoutingWSNativeTransportRevalidatesBeforeCreateAndSteer(t *testing.T) {
	for _, phase := range []string{"create", "steer"} {
		t.Run(phase, func(t *testing.T) {
			checks := 0
			denied := errors.New("synthetic key revoked")
			turn, conn, _ := aiNativeWSTransportFixture(t, func(context.Context) error {
				checks++
				if phase == "create" && checks == 2 || phase == "steer" && checks == 3 {
					return denied
				}
				return nil
			}, aiNativeWSCreate)
			conn.events <- []byte(`{"type":"response.created","response":{"id":"resp_a"}}`)
			conn.events <- []byte(`{"type":"response.completed","response":{"id":"resp_a","usage":{"input_tokens":1,"output_tokens":1}}}`)
			result, err := turn.Run(func(event []byte) error {
				if phase == "steer" && gjson.GetBytes(event, "type").Str == "response.created" {
					require.Same(t, denied, turn.Steer(t.Context(), []byte(`{"type":"response.steer","previous_response_id":"resp_a","input":"update"}`)))
				}
				return nil
			})
			if phase == "create" {
				require.Same(t, denied, err)
				require.Empty(t, conn.writes)
				require.Zero(t, result.Usage.InputTokens)
			} else {
				require.NoError(t, err)
				require.Len(t, conn.writes, 1)
			}
		})
	}
}

func TestAIRoutingWSNativeTransportCancellationDrainsKnownUsageWithoutCancelFrame(t *testing.T) {
	turn, conn, _ := aiNativeWSTransportFixture(t, nil, aiNativeWSCreate)
	conn.events <- []byte(`{"type":"response.created","response":{"id":"resp_cancel"}}`)
	conn.events <- []byte(`{"type":"response.completed","response":{"id":"resp_cancel","usage":{"input_tokens":7,"output_tokens":3}}}`)
	emitted := 0
	result, err := turn.Run(func([]byte) error { emitted++; turn.Cancel(); return nil })
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, result.ClientDisconnect)
	require.Equal(t, 1, emitted)
	require.Equal(t, 7, result.Usage.InputTokens)
	require.Equal(t, 3, result.Usage.OutputTokens)
	require.Len(t, conn.writes, 1, "local cancellation must not forge provider acknowledgement or a control frame")
}

func TestAIRoutingWSNativeTransportPartialUsageSurvivesBrokenSuccessor(t *testing.T) {
	turn, conn, _ := aiNativeWSTransportFixture(t, nil, aiNativeWSCreate)
	conn.events <- []byte(`{"type":"response.created","response":{"id":"resp_partial"}}`)
	conn.events <- []byte(`{"type":"response.incomplete","response":{"id":"resp_partial","incomplete_details":{"reason":"steered"},"usage":{"input_tokens":4,"output_tokens":2}}}`)
	close(conn.events)
	result, err := turn.Run(func([]byte) error { return nil })
	require.Error(t, err)
	require.Equal(t, 4, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.OutputTokens)
	require.Len(t, conn.writes, 1)
}

func TestAIRoutingWSNativeTransportRejectsWrongLaneAndChangedModel(t *testing.T) {
	for _, mode := range []string{"lane", "mapping"} {
		t.Run(mode, func(t *testing.T) {
			turn, conn, account := aiNativeWSTransportFixture(t, nil, aiNativeWSCreate)
			conn.events <- []byte(`{"type":"response.created","stream_id":"other","response":{"id":"resp_other"}}`)
			if mode == "mapping" {
				account.Credentials["model_mapping"] = map[string]any{"gpt-5.1": "gpt-5.4"}
			}
			emitted := 0
			_, err := turn.Run(func([]byte) error { emitted++; return nil })
			require.Error(t, err)
			require.Zero(t, emitted)
			if mode == "mapping" {
				require.Empty(t, conn.writes)
			}
		})
	}
	for _, lane := range []string{"", "invalid lane", "../slash", strings.Repeat("x", 257)} {
		require.False(t, aiRoutingWSValidLane(lane))
	}
	require.True(t, aiRoutingWSValidLane("branch_A-2.1"))
}
