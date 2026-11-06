package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAIBackgroundReleaseAdminJobsRouteIsNotRegistered(t *testing.T) {
	router := gin.New()
	registerGroupRoutes(router.Group("/api/v1/admin"), &handler.Handlers{Admin: &handler.AdminHandlers{Group: &adminhandler.GroupHandler{}}})
	for _, route := range router.Routes() {
		require.NotContains(t, route.Path, "background-jobs")
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/admin/groups/8/ai-routing/background-jobs", nil))
	require.Equal(t, 404, w.Code)
}
