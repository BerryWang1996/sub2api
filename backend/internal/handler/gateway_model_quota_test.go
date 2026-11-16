package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestModelQuotaAIGroupReturnsNotImplemented(t *testing.T) {
	gin.SetMode(gin.TestMode)
	group := &service.Group{ID: 42, Platform: service.PlatformComposite, AIRouting: true, Status: service.StatusActive}
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models?include=quota", nil)
	c.Set(string(middleware.ContextKeyAPIKey), &service.APIKey{GroupID: &group.ID, Group: group})
	(&GatewayHandler{}).ModelQuota(c, "")
	require.Equal(t, http.StatusNotImplemented, response.Code)
	require.Contains(t, response.Body.String(), "quota_catalog_unsupported")
}
