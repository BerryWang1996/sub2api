package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type aiAvailableGroupRepository struct{ service.GroupRepository }

func (*aiAvailableGroupRepository) ListActive(context.Context) ([]service.Group, error) {
	return []service.Group{{ID: 8, Name: "AI group", Platform: service.PlatformComposite, AIRouting: true, Status: service.StatusActive}}, nil
}

type aiAvailableUserRepository struct{ service.UserRepository }

func (*aiAvailableUserRepository) GetByID(_ context.Context, id int64) (*service.User, error) {
	return &service.User{ID: id}, nil
}

type aiAvailableSubscriptionRepository struct {
	service.UserSubscriptionRepository
}

func (*aiAvailableSubscriptionRepository) ListActiveByUserID(context.Context, int64) ([]service.UserSubscription, error) {
	return nil, nil
}

func TestGetAvailableGroupsReturnsAIRoutingModeToOrdinaryUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := service.NewAPIKeyService(nil, &aiAvailableUserRepository{}, &aiAvailableGroupRepository{}, &aiAvailableSubscriptionRepository{}, nil, nil, nil)
	h := NewAPIKeyHandler(svc)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/groups/available", nil)
	c.Request.Header.Set("Accept-Language", "zh-CN")
	c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 42})
	h.GetAvailableGroups(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var body struct {
		Data []map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Data, 1)
	require.Equal(t, true, body.Data[0]["ai_routing"])
	for _, field := range []string{"policy", "prompt", "candidates", "decision_account_id", "decision_group_id", "target_account_id", "target_group_id"} {
		require.NotContains(t, body.Data[0], field)
	}
}
