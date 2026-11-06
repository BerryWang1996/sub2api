package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestAIResponsesChatFallbackRequiresRealTerminalAndPreservesUsage(t *testing.T) {
	delta := `data: {"id":"chat","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}` + "\n\n"
	finished := `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	usage := `data: {"choices":[],"usage":{"prompt_tokens":9,"completion_tokens":3}}` + "\n\n"
	for _, tt := range []struct {
		name, frames string
		fail         bool
	}{
		{"real_finish_then_usage", delta + finished + usage + "data: [DONE]\n\n", false},
		{"real_finish_without_done", delta + finished + usage, false},
		{"eof", delta + usage, true},
		{"premature_done", delta + usage + "data: [DONE]\n\n", true},
		{"after_finish", delta + finished + usage + delta, true},
		{"second_incomplete_choice", delta + finished + usage + `data: {"choices":[{"index":1,"delta":{"content":"other"},"finish_reason":null}]}` + "\n\n", true},
		{"malformed_tail", delta + finished + usage + "data: {invalid\n\n", true},
		{"duplicate_members", delta + usage + `data: {"choices":[],"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: 1 << 20}}}
			c, rec := newAntigravityCompatContext(http.MethodPost, "/v1/responses", []byte(`{"stream":true}`))
			c.Request = c.Request.WithContext(WithAIRoutingExecutionRequirements(c.Request.Context(), AIRoutingExecutionRequirements{Endpoint: CompositeRouteEndpointResponses}))
			resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(tt.frames))}
			result, err := s.streamChatCompletionsAsResponses(c, resp, "model", nil, nil, false, nil, "model", "model", nil, nil, time.Now())
			require.NotNil(t, result)
			require.Equal(t, 9, result.Usage.InputTokens)
			require.Equal(t, 3, result.Usage.OutputTokens)
			if tt.fail {
				require.Error(t, err)
				require.NotContains(t, rec.Body.String(), "response.completed")
				require.NotContains(t, rec.Body.String(), "data: [DONE]")
			} else {
				require.NoError(t, err)
				require.Contains(t, rec.Body.String(), "response.completed")
			}
		})
	}
}

func TestAIResponsesChatFallbackCancellationAndOrdinaryEOF(t *testing.T) {
	for _, strict := range []bool{true, false} {
		c, rec := newAntigravityCompatContext(http.MethodPost, "/v1/responses", nil)
		if strict {
			ctx := WithAIRoutingExecutionRequirements(c.Request.Context(), AIRoutingExecutionRequirements{})
			ctx, cancel := context.WithCancel(ctx)
			cancel()
			c.Request = c.Request.WithContext(ctx)
		}
		resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n"))}
		s := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: 1 << 20}}}
		_, err := s.streamChatCompletionsAsResponses(c, resp, "model", nil, nil, false, nil, "model", "model", nil, nil, time.Now())
		if strict {
			require.Error(t, err)
			require.Empty(t, rec.Body.String())
		} else {
			require.NoError(t, err)
			require.Contains(t, rec.Body.String(), "response.completed", "ordinary compatibility is unchanged")
		}
	}
}

func TestAIMessagesChatFallbackRequiresActualFinish(t *testing.T) {
	for _, finish := range []bool{false, true} {
		c, rec := newAntigravityCompatContext(http.MethodPost, "/v1/messages", nil)
		c.Request = c.Request.WithContext(WithAIRoutingExecutionRequirements(c.Request.Context(), AIRoutingExecutionRequirements{Endpoint: CompositeRouteEndpointMessages}))
		frames := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n"
		if finish {
			frames += "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
		}
		frames += "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":3}}\n\n"
		resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(frames))}
		s := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: 1 << 20}}}
		result, err := s.streamChatCompletionsAsAnthropic(c, resp, "model", "model", "model", nil, nil, time.Now())
		require.NotNil(t, result)
		require.Equal(t, 9, result.Usage.InputTokens)
		require.Equal(t, 3, result.Usage.OutputTokens)
		if finish {
			require.NoError(t, err)
			require.Contains(t, rec.Body.String(), "message_stop")
		} else {
			require.Error(t, err)
			require.NotContains(t, rec.Body.String(), "message_stop")
		}
	}
}
