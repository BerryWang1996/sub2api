package service

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	modelQuotaMaxModels       = 200
	modelQuotaMaxAccounts     = 500
	modelQuotaMaxCombinations = 2000
	modelQuotaMaxBytes        = 1 << 20
)

var (
	ErrModelQuotaResultTooLarge = infraerrors.ServiceUnavailable("quota_result_too_large", "Model quota result exceeds the response limit")
	ErrModelQuotaCatalogBudget  = infraerrors.ServiceUnavailable("catalog_budget_exceeded", "Model quota catalog exceeds the configuration limit")
	ErrModelQuotaFieldConflict  = infraerrors.ServiceUnavailable("quota_catalog_field_conflict", "Model quota would overwrite model catalog metadata")
)

var modelQuotaEndpoints = []string{CompositeRouteEndpointMessages, CompositeRouteEndpointChatCompletions, CompositeRouteEndpointResponses, CompositeRouteEndpointGemini}

type modelQuotaPlannedRoute struct {
	decision   *CompositeRouteDecision
	candidates []modelQuotaCandidate
	reason     string
	readFailed bool
}

// GetModelsWithQuota summarizes configured routes and their most useful candidate.
func (s *GatewayService) GetModelsWithQuota(ctx context.Context, key *APIKey, subscription *UserSubscription, endpoint string) (*ModelQuotaResponse, error) {
	if s == nil || key == nil || key.Group == nil || key.GroupID == nil || key.Group.ID != *key.GroupID || key.Group.ID <= 0 || !key.Group.IsActive() {
		return nil, errors.New("model quota requires an authorized source group")
	}
	if key.Group.AIRouting || key.Group.Platform == PlatformOpenAI && key.Group.CodexModelsManifestConfig.Enabled {
		return nil, errors.New("model quota does not support the group's model catalog")
	}
	if endpoint == "" {
		endpoint = CompositeRouteEndpointMessages
	}
	if !slices.Contains(modelQuotaEndpoints, endpoint) {
		return nil, errors.New("model quota endpoint is invalid")
	}
	reader, ok := s.accountRepo.(ModelQuotaRoutingReader)
	if !ok {
		return nil, errors.New("model quota configuration reader is unavailable")
	}
	inventory, err := reader.ReadModelQuotaCatalog(ctx, key.Group.ID, key.Group.Platform == PlatformComposite)
	if err != nil {
		return nil, err
	}
	entries, err := modelQuotaCatalog(key.Group, inventory)
	if err != nil {
		return nil, err
	}
	planned, groupIDs, pinIDs, platforms := planModelQuotaRoutes(key.Group, inventory, entries, endpoint)
	now := time.Now()
	admission, platformConstraints := s.billingCacheService.ReadModelQuotaBilling(ctx, key, subscription, platforms, now)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.populateModelQuotaRoutes(ctx, reader, key, groupIDs, pinIDs, platformConstraints, planned, now)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return renderModelQuotaResponse(entries, planned, admission, endpoint, now)
}

func planModelQuotaRoutes(group *Group, inventory ModelQuotaCatalogInventory, entries []modelQuotaCatalogEntry, endpoint string) ([]modelQuotaPlannedRoute, []int64, []int64, []string) {
	planned := make([]modelQuotaPlannedRoute, len(entries))
	groups := make(map[int64]bool)
	pins := make(map[int64]bool)
	platforms := make(map[string]bool)
	var groupIDs, pinIDs []int64
	var quotaPlatforms []string
	for i, entry := range entries {
		if i >= modelQuotaMaxModels {
			planned[i] = modelQuotaPlannedRoute{reason: "quota_budget_exceeded", readFailed: true}
			continue
		}
		route := planModelQuotaRoute(group, inventory, entry.id, endpoint)
		planned[i] = route
		d := route.decision
		if d == nil {
			continue
		}
		groupID := group.ID
		if d.TargetGroupID != nil {
			groupID = *d.TargetGroupID
		}
		if d.TargetAccountID != nil {
			if id := *d.TargetAccountID; !pins[id] {
				pins[id] = true
				pinIDs = append(pinIDs, id)
			}
		} else if !groups[groupID] {
			groups[groupID] = true
			groupIDs = append(groupIDs, groupID)
		}
		if !platforms[d.TargetPlatform] {
			platforms[d.TargetPlatform] = true
			quotaPlatforms = append(quotaPlatforms, d.TargetPlatform)
		}
	}
	return planned, groupIDs, pinIDs, quotaPlatforms
}

func renderModelQuotaResponse(entries []modelQuotaCatalogEntry, planned []modelQuotaPlannedRoute, admission ModelQuotaKeyAdmission, endpoint string, now time.Time) (*ModelQuotaResponse, error) {
	response := &ModelQuotaResponse{
		Object: "list",
		Data:   make([]map[string]json.RawMessage, 0, len(entries)),
		Quota: ModelQuotaEnvelope{
			SchemaVersion: modelQuotaSchemaVersion, Endpoint: endpoint,
			QueriedAt: modelQuotaDisplayTime(now), KeyAdmission: admission,
		},
	}
	ids := make(modelQuotaPoolIDs)
	quotaBytes := 0
	for i, entry := range entries {
		route := planned[i]
		unknown := ""
		if route.readFailed {
			unknown = route.reason
		}
		model := aggregateModelQuota(route.candidates, unknown, ids, now)
		if route.reason != "" && !route.readFailed {
			model.Reasons = uniqueModelQuotaStrings(append(model.Reasons, route.reason))
		}
		encoded, err := json.Marshal(model)
		if err != nil {
			return nil, err
		}
		quotaBytes += len(encoded)
		if quotaBytes > modelQuotaMaxBytes {
			return nil, ErrModelQuotaResultTooLarge
		}
		entry.row[modelQuotaOutputField] = encoded
		response.Data = append(response.Data, entry.row)
	}
	return response, nil
}
