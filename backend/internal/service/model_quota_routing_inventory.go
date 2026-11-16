package service

import "context"

// ModelQuotaCatalogInventory contains only attributes needed to enumerate public
// model IDs. The account projection must not include upstream credentials.
type ModelQuotaCatalogInventory struct {
	Accounts []Account
	Routes   []CompositeModelRoute
}

// ModelQuotaTargetInventory records candidate membership and destination group
// validity before any fully populated account can be loaded.
type ModelQuotaTargetInventory struct {
	Groups   map[int64]Group
	Accounts []ModelQuotaAccountLink
	Complete bool
}

type ModelQuotaAccountLink struct {
	ID     int64
	Groups []int64
}

// ModelQuotaRoutingReader is used exclusively by the opt-in model directory.
// A missing implementation is never permission to select a wider account pool.
type ModelQuotaRoutingReader interface {
	ReadModelQuotaCatalog(ctx context.Context, groupID int64, withRoutes bool) (ModelQuotaCatalogInventory, error)
	ReadModelQuotaTargets(ctx context.Context, groupIDs, pinIDs []int64, limit int) (ModelQuotaTargetInventory, error)
}
