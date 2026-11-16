package service

import (
	"encoding/json"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/geminicli"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

type modelQuotaCatalogEntry struct {
	id  string
	row map[string]json.RawMessage
}

func GatewayModelListingSource(platform string, availableModels, fallbackModels []string) []string {
	if platform == PlatformOpenAI && availableModels != nil {
		return availableModels
	}
	if len(availableModels) == 0 {
		return fallbackModels
	}
	if platform != PlatformAnthropic {
		return availableModels
	}
	seen := make(map[string]bool, len(availableModels)+len(fallbackModels))
	merged := make([]string, 0, len(availableModels)+len(fallbackModels))
	for _, src := range [][]string{availableModels, fallbackModels} {
		for _, id := range src {
			id = strings.TrimSpace(id)
			if id != "" && !seen[id] {
				seen[id] = true
				merged = append(merged, id)
			}
		}
	}
	return merged
}

func modelQuotaCatalog(group *Group, inventory ModelQuotaCatalogInventory) ([]modelQuotaCatalogEntry, error) {
	platform := group.Platform
	var ids []string
	defaultRows := false
	if platform == PlatformComposite {
		platforms := make(map[string]bool)
		for _, account := range inventory.Accounts {
			platforms[account.Platform] = true
		}
		for _, concrete := range domain.CompositePrecedencePlatformIDs() {
			models, _ := modelIDsFromCatalogAccounts(concrete, inventory.Accounts)
			if len(models) == 0 && (concrete != PlatformOpenAI || models == nil) && platforms[concrete] && !IsMultiProtocolAPIKeyProvider(concrete) {
				models = GatewayDefaultModelIDs(concrete)
			}
			ids = append(ids, models...)
		}
		for _, route := range inventory.Routes {
			if !route.Enabled {
				continue
			}
			if route.MatchType == CompositeRouteMatchPrefix {
				continue
			}
			ids = append(ids, route.PublicModel)
		}
		if len(ids) == 0 {
			ids = GatewayDefaultModelIDs(platform)
		}
		ids = group.ModelAllowlist.FilterForListing(ids)
	} else {
		models, _ := modelIDsFromCatalogAccounts(platform, inventory.Accounts)
		defaults := GatewayDefaultModelIDs(platform)
		ids = models
		if group.ModelAllowlistEnabled() {
			ids = group.ModelAllowlist.FilterForListing(GatewayModelListingSource(platform, models, defaults))
		} else if len(ids) == 0 && (platform != PlatformOpenAI || models == nil) {
			switch platform {
			case PlatformOpenAI, PlatformGemini, PlatformGrok, PlatformTypeSafe:
				ids = defaults
			default:
				ids = claude.DefaultModelIDs()
			}
			defaultRows = true
		}
	}
	seen := make(map[string]bool, len(ids))
	concreteIDs := make([]string, 0, len(ids))
	for _, id := range ids {
		if strings.Contains(id, "*") {
			continue
		}
		if id = strings.TrimSpace(id); id != "" && !seen[id] && group.ModelAllowlist.Allows(id) {
			seen[id] = true
			concreteIDs = append(concreteIDs, id)
		}
	}
	models := GatewayCatalogModels(platform, concreteIDs)
	if defaultRows {
		switch platform {
		case PlatformOpenAI:
			models = openai.DefaultModels
		case PlatformGemini:
			models = geminicli.DefaultModels
		case PlatformGrok, PlatformTypeSafe:
			// These providers use the same renderer for both modes.
		default:
			models = claude.DefaultModels
		}
	}
	encoded, err := json.Marshal(models)
	if err != nil {
		return nil, err
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &rows); err != nil {
		return nil, err
	}
	entries := make([]modelQuotaCatalogEntry, 0, len(rows))
	for _, row := range rows {
		var id string
		if err := json.Unmarshal(row["id"], &id); err != nil {
			return nil, err
		}
		if !seen[id] {
			continue
		}
		if _, exists := row[modelQuotaOutputField]; exists {
			return nil, ErrModelQuotaFieldConflict
		}
		entries = append(entries, modelQuotaCatalogEntry{id: id, row: row})
	}
	return entries, nil
}
