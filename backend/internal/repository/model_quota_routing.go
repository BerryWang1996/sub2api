package repository

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Wei-Shaw/sub2api/ent/compositemodelroute"
	dbgroup "github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

const (
	modelQuotaCatalogAccounts = 2000
	modelQuotaCatalogRoutes   = 500
	modelQuotaMappingBytes    = 2 << 20
)

var _ service.ModelQuotaRoutingReader = (*accountRepository)(nil)

func (r *accountRepository) ReadModelQuotaCatalog(ctx context.Context, groupID int64, withRoutes bool) (service.ModelQuotaCatalogInventory, error) {
	var result service.ModelQuotaCatalogInventory
	if groupID <= 0 {
		return result, errors.New("model quota catalog requires a source group")
	}
	rows, err := r.sql.QueryContext(ctx, `
		WITH members AS MATERIALIZED (
			SELECT a.id, a.platform, a.type, a.credentials->'model_mapping' AS mapping,
			       CASE WHEN g.platform = 'anthropic' AND a.platform = 'anthropic'
			            THEN a.extra->'upstream_model_metadata' END AS model_metadata,
			       jsonb_build_object('openai_passthrough', a.extra->'openai_passthrough',
			           'openai_passthrough_model_filter', a.extra->'openai_passthrough_model_filter',
			           'openai_oauth_passthrough', a.extra->'openai_oauth_passthrough',
			           'mixed_scheduling', a.extra->'mixed_scheduling') AS flags
			FROM account_groups ag JOIN accounts a ON a.id = ag.account_id
			JOIN groups g ON g.id = ag.group_id
			WHERE ag.group_id = $1 AND a.deleted_at IS NULL AND a.status = $2 AND a.schedulable
			ORDER BY a.id LIMIT $3
		), budget AS (
			SELECT COALESCE(sum(octet_length(mapping::text)), 0)
			     + COALESCE(sum(octet_length(model_metadata::text)), 0) AS bytes FROM members
		)
		SELECT id, platform, type, CASE WHEN bytes <= $4 THEN mapping END,
		       CASE WHEN bytes <= $4 THEN model_metadata END, flags, bytes > $4
		FROM members CROSS JOIN budget ORDER BY id`, groupID, service.StatusActive, modelQuotaCatalogAccounts+1, modelQuotaMappingBytes)
	if err != nil {
		return result, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var account service.Account
		var mapping, metadata, flags []byte
		var overBudget bool
		if err := rows.Scan(&account.ID, &account.Platform, &account.Type, &mapping, &metadata, &flags, &overBudget); err != nil {
			return result, err
		}
		if overBudget || len(result.Accounts) == modelQuotaCatalogAccounts {
			return service.ModelQuotaCatalogInventory{}, service.ErrModelQuotaCatalogBudget
		}
		if len(mapping) > 0 {
			var raw any
			if err := json.Unmarshal(mapping, &raw); err != nil {
				return result, err
			}
			account.Credentials = map[string]any{"model_mapping": raw}
		}
		if err := json.Unmarshal(flags, &account.Extra); err != nil {
			return result, err
		}
		if len(metadata) > 0 {
			account.Extra[service.UpstreamModelMetadataExtraKey] = json.RawMessage(metadata)
		}
		result.Accounts = append(result.Accounts, account)
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	if !withRoutes {
		return result, nil
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	routes, err := clientFromContext(ctx, r.client).CompositeModelRoute.Query().
		Where(compositemodelroute.GroupIDEQ(groupID), compositemodelroute.EnabledEQ(true)).
		Order(compositemodelroute.ByPriority(), compositemodelroute.ByID()).
		Select(compositemodelroute.FieldID, compositemodelroute.FieldGroupID, compositemodelroute.FieldPublicModel,
			compositemodelroute.FieldMatchType, compositemodelroute.FieldTargetPlatform, compositemodelroute.FieldTargetGroupID,
			compositemodelroute.FieldTargetAccountID, compositemodelroute.FieldUpstreamModel, compositemodelroute.FieldEndpoint,
			compositemodelroute.FieldPriority, compositemodelroute.FieldEnabled).
		Limit(modelQuotaCatalogRoutes + 1).All(ctx)
	if err != nil {
		return result, err
	}
	if len(routes) > modelQuotaCatalogRoutes {
		return service.ModelQuotaCatalogInventory{}, service.ErrModelQuotaCatalogBudget
	}
	for _, route := range routes {
		result.Routes = append(result.Routes, *compositeModelRouteEntityToService(route))
	}
	return result, nil
}

func (r *accountRepository) ReadModelQuotaTargets(ctx context.Context, groupIDs, pinIDs []int64, limit int) (service.ModelQuotaTargetInventory, error) {
	result := service.ModelQuotaTargetInventory{Groups: make(map[int64]service.Group), Complete: true}
	if limit <= 0 || limit > 500 || len(groupIDs) > 501 || len(pinIDs) > 500 {
		return result, service.ErrModelQuotaCatalogBudget
	}
	groups, err := clientFromContext(ctx, r.client).Group.Query().Where(dbgroup.IDIn(groupIDs...)).All(ctx)
	if err != nil {
		return result, err
	}
	activeGroups := make([]int64, 0, len(groups))
	for _, stored := range groups {
		group := groupEntityToService(stored)
		result.Groups[group.ID] = *group
		if group.IsActive() {
			activeGroups = append(activeGroups, group.ID)
		}
	}
	rows, err := r.sql.QueryContext(ctx, `
		SELECT a.id, ARRAY(SELECT ag.group_id FROM account_groups ag
			WHERE ag.account_id = a.id AND ag.group_id = ANY($1) ORDER BY ag.group_id)
		FROM accounts a WHERE a.deleted_at IS NULL AND (a.id = ANY($2) OR EXISTS (
			SELECT 1 FROM account_groups ag WHERE ag.account_id = a.id AND ag.group_id = ANY($1)))
		ORDER BY a.id LIMIT $3`, pq.Array(activeGroups), pq.Array(pinIDs), limit+1)
	if err != nil {
		return result, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var link service.ModelQuotaAccountLink
		if err := rows.Scan(&link.ID, pq.Array(&link.Groups)); err != nil {
			return result, err
		}
		if len(result.Accounts) == limit {
			result.Complete = false
			break
		}
		result.Accounts = append(result.Accounts, link)
	}
	return result, rows.Err()
}
