package service

import (
	"context"
	"slices"
	"strings"
)

func (s *GatewayService) ExpandAnthropicLongContextModels(ctx context.Context, group *Group, models []string) []string {
	var accounts []Account
	var err error
	if group != nil {
		accounts, err = s.accountRepo.ListSchedulableByGroupID(ctx, group.ID)
	} else {
		accounts, err = s.accountRepo.ListSchedulable(ctx)
	}
	if err != nil {
		accounts = nil
	}
	result, _ := anthropicListingModels(group, models, accounts, 0)
	return result
}

// A zero comparison limit is used by ordinary discovery. Quota discovery uses
// the same projected account data and listing rules under its request budget.
func anthropicListingModels(group *Group, available []string, accounts []Account, limit int) ([]string, error) {
	allowlisted := group != nil && group.ModelAllowlistEnabled()
	source := available
	if allowlisted || len(source) == 0 {
		source = GatewayModelListingSource(PlatformAnthropic, available, GatewayDefaultModelIDs(PlatformAnthropic))
	}
	type catalog struct {
		account *Account
		models  map[string]UpstreamModelMetadata
	}
	var catalogs []catalog
	for i := range accounts {
		account := &accounts[i]
		if account.IsAnthropic() {
			if snapshot := account.GetUpstreamModelMetadataSnapshot(); snapshot != nil {
				catalogs = append(catalogs, catalog{account, snapshot.Models})
			}
		}
	}
	result := cloneStringSlice(source)
	seen := make(map[string]bool, len(source))
	for _, model := range source {
		seen[strings.ToLower(model)] = true
	}
	checks := 0
	for _, model := range source {
		if strings.HasSuffix(strings.ToLower(model), claudeCodeLongContextModelSuffix) || strings.Contains(model, "*") {
			continue
		}
		var routedAccountIDs []int64
		if group != nil {
			routedAccountIDs = group.GetRoutingAccountIDs(model)
		}
		variant := model + claudeCodeLongContextModelSuffix
		if seen[strings.ToLower(variant)] {
			continue
		}
		for _, catalog := range catalogs {
			checks++
			if limit > 0 && checks > limit {
				return nil, ErrModelQuotaCatalogBudget
			}
			account := catalog.account
			if !account.IsModelSupported(model) ||
				(len(routedAccountIDs) > 0 && !slices.Contains(routedAccountIDs, account.ID)) {
				continue
			}
			metadata := catalog.models[strings.TrimSpace(account.GetMappedModel(model))]
			if metadata.ContextWindow >= 900_000 {
				result = append(result, variant)
				seen[strings.ToLower(variant)] = true
				break
			}
		}
	}
	if len(available) == 0 && len(result) == len(source) && !allowlisted {
		return available, nil // preserve the default model rows and their metadata
	}
	return result, nil
}
