package dto

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGroupFromServiceExposesAIRoutingModeWithoutPolicy(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		source := &service.Group{ID: 8, Name: "routing group", Platform: service.PlatformComposite, AIRouting: enabled}
		for _, mapped := range []any{GroupFromService(source), GroupFromServiceShallow(source), GroupFromService(source).Localized("zh-CN"), GroupFromServiceAdmin(source)} {
			fields := marshalToMap(t, mapped)
			require.Equal(t, enabled, fields["ai_routing"])
			for _, privateField := range []string{"policy", "draft", "published", "prompt", "candidates", "decision_account_id", "decision_group_id", "target_account_id", "target_group_id"} {
				require.NotContains(t, fields, privateField)
			}
		}
	}
}
