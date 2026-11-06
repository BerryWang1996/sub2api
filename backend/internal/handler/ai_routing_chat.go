package handler

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *GatewayHandler) forwardAIChatWithOrigin(c *gin.Context, runtime *aiRoutingRuntime, key *service.APIKey, chosen service.AIRoutingCandidate) {
	base := c.Writer
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	originStore, _ := runtime.store.(service.AIRoutingOriginStore)
	ctx = withAIGeminiResponseState(ctx, originStore, key, chosen, func() int64 { return c.GetInt64(opsAccountIDKey) }, service.CompositeRouteEndpointChatCompletions)
	c.Request = c.Request.WithContext(ctx)
	var selected int64
	var writer *aiChatWriter
	writer = newAIChatWriter(base, func(id string) error {
		account := c.GetInt64(opsAccountIDKey)
		if selected != 0 && account != selected {
			return &service.AIRoutingCapacityError{Check: service.AIRoutingCapabilityCheck{Reason: "execution_origin_unavailable"}}
		}
		selected = account
		return saveAIChatBinding(ctx, runtime.store, key, id, newAIToolBinding(chosen, account))
	}, func(error) {
		cancelled := ctx.Err() != nil
		c.Abort()
		cancel()
		c.Writer = base
		payload := gin.H{"error": gin.H{"type": "api_error", "code": "ai_tool_binding_unavailable", "message": "Chat tool continuation could not be recorded."}}
		// Keep the failover barrier after client cancellation without writing
		// another event to the dead stream.
		if !cancelled && !base.Written() {
			c.Header("Content-Type", "application/json")
			c.Header("Content-Length", "")
			c.JSON(http.StatusServiceUnavailable, payload)
		} else if !cancelled && writer.isStream() {
			body, _ := json.Marshal(payload)
			_, _ = base.Write(append(append([]byte("data: "), body...), []byte("\n\ndata: [DONE]\n\n")...))
			base.Flush()
		}
		service.MarkResponseCommitted(c)
		c.Writer = writer
	})
	c.Writer = writer
	defer func() { c.Writer = base }()
	c.Next()
	_ = writer.finish()
}
