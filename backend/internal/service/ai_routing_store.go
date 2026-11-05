package service

import "context"

// AIRoutingStore persists tool provenance and bounded operator history.
// Callers retain authorization, key scoping, and request deadline ownership.
type AIRoutingStore interface {
	LoadToolBindings(ctx context.Context, keys []string) ([]string, error)
	SaveToolBinding(ctx context.Context, key, value string) error
	SaveTrace(ctx context.Context, groupID int64, trace AIRoutingTrace) error
	ListTraces(ctx context.Context, groupID int64) ([]AIRoutingTrace, error)
}
