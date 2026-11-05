package service

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAPIKeyAuthSnapshotAIRoutingRoundTrip(t *testing.T) {
	for _, ai := range []bool{false, true} {
		t.Run(fmt.Sprint(ai), func(t *testing.T) {
			groupID := int64(50)
			key := &APIKey{ID: 82, UserID: 40, GroupID: &groupID, Key: "synthetic-mode-key", Status: StatusActive,
				User:  &User{ID: 40, Status: StatusActive},
				Group: &Group{ID: groupID, Platform: PlatformComposite, Status: StatusActive, Hydrated: true, AIRouting: ai}}
			svc := &APIKeyService{}
			payload, err := json.Marshal(&APIKeyAuthCacheEntry{Snapshot: svc.snapshotFromAPIKey(context.Background(), key)})
			require.NoError(t, err)
			var cached APIKeyAuthCacheEntry
			require.NoError(t, json.Unmarshal(payload, &cached))
			materialized, used, err := svc.applyAuthCacheEntry(key.Key, &cached)
			require.NoError(t, err)
			require.True(t, used)
			require.NotNil(t, materialized.Group)
			require.Equal(t, ai, materialized.Group.AIRouting)
			require.Equal(t, PlatformComposite, materialized.Group.Platform)
		})
	}
}

func TestAPIKeyServiceRejectsV25AuthSnapshotWithoutAIRoutingMode(t *testing.T) {
	svc := &APIKeyService{}
	entry := &APIKeyAuthCacheEntry{Snapshot: &APIKeyAuthSnapshot{Version: 25, Group: &APIKeyAuthGroupSnapshot{ID: 50, Platform: PlatformComposite}}}
	key, used, err := svc.applyAuthCacheEntry("synthetic-legacy-key", entry)
	require.NoError(t, err)
	require.False(t, used, "a pre-routing-mode snapshot must trigger a database reload")
	require.Nil(t, key)
}
