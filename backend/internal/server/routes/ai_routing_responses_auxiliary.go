package routes

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// Defense in depth: an auxiliary count must never fall through to a Responses
// generation bridge, even if candidate capability admission is later extended.
func rejectUnsupportedAIResponsesInputTokens(c *gin.Context) bool {
	if !service.IsOpenAIResponsesInputTokensRequestPath(c) {
		return false
	}
	key, ok := middleware.GetAPIKeyFromContext(c)
	if !ok || key == nil || key.Group == nil || !key.Group.AIRouting || key.Group.Platform != service.PlatformComposite || domain.UsesOpenAIGateway(getGroupPlatform(c)) {
		return false
	}
	c.Header("X-Sub2API-Token-Count-Source", "unsupported")
	c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"object": "response.input_tokens", "count_source": "unsupported", "estimated": false,
		"error": gin.H{"type": "input_tokens_unsupported", "message": "The selected destination does not support this token-counting operation."}})
	return true
}
