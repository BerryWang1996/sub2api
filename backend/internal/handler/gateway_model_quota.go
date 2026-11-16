package handler

import (
	"errors"
	"net/http"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// ModelQuota serves the authenticated model catalog with quota summaries.
func (h *GatewayHandler) ModelQuota(c *gin.Context, endpoint string) {
	key, ok := middleware.GetAPIKeyFromContext(c)
	if !ok || key == nil || key.Group == nil {
		middleware.AbortWithError(c, http.StatusForbidden, "GROUP_REQUIRED", "An active source group is required")
		return
	}
	ctx := c.Request.Context()
	if key.Group.AIRouting {
		middleware.AbortWithError(c, http.StatusNotImplemented, "quota_catalog_unsupported", "Model quota is unavailable for AI routing groups")
		return
	}
	if key.Group.Platform == service.PlatformOpenAI && key.Group.CodexModelsManifestConfig.Enabled {
		middleware.AbortWithError(c, http.StatusNotImplemented, "quota_catalog_unsupported", "Model quota is unavailable for a live upstream model catalog")
		return
	}
	if h == nil || h.gatewayService == nil {
		middleware.AbortWithError(c, http.StatusServiceUnavailable, "quota_unavailable", "Model quota is unavailable")
		return
	}
	subscription, _ := middleware.GetSubscriptionFromContext(c)
	result, err := h.gatewayService.GetModelsWithQuota(ctx, key, subscription, endpoint)
	if ctx.Err() != nil {
		c.Status(statusClientClosedRequest)
		return
	}
	switch {
	case errors.Is(err, service.ErrModelQuotaResultTooLarge), errors.Is(err, service.ErrModelQuotaCatalogBudget):
		middleware.AbortWithError(c, infraerrors.Code(err), infraerrors.Reason(err), infraerrors.Message(err))
	case err != nil || result == nil:
		middleware.AbortWithError(c, http.StatusServiceUnavailable, "quota_unavailable", "Unable to read model quota")
	default:
		c.JSON(http.StatusOK, result)
	}
}
