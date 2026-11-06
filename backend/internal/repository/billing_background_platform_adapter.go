package repository

import "github.com/Wei-Shaw/sub2api/internal/service"

func ProvideBackgroundPlatformQuotaAdapter(repo UserPlatformQuotaRepository, background service.AIRoutingBackgroundRepository) service.UserPlatformQuotaRepository {
	return service.WithBackgroundPlatformQuotaViews(NewUserPlatformQuotaServiceAdapter(repo), background)
}
