//go:build integration

package repository

import (
	"context"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestModelQuotaRoutingIntegration(t *testing.T) {
	ctx := context.Background()
	tx := testTx(t)
	client := dbent.NewClient(dbent.Driver(entsql.NewDriver(dialect.Postgres, entsql.Conn{ExecQuerier: tx})))
	repo := &accountRepository{client: client, sql: tx}
	source := client.Group.Create().SetName("quota-mix-" + uuid.NewString()).SetPlatform(service.PlatformComposite).SaveX(ctx)
	target := client.Group.Create().SetName("quota-gemini-" + uuid.NewString()).SetPlatform(service.PlatformGemini).SaveX(ctx)
	unrelated := client.Group.Create().SetName("unrelated-" + uuid.NewString()).SetPlatform(service.PlatformAnthropic).SaveX(ctx)
	credentials := map[string]any{"model_mapping": map[string]any{"mix-opus": "claude-sonnet"}, "api_key": "never-projected"}
	cooling := client.Account.Create().SetName("cooling").SetPlatform(service.PlatformAnthropic).SetType(service.AccountTypeAPIKey).
		SetCredentials(credentials).SetExtra(map[string]any{"internal_secret": "never-projected"}).
		SetRateLimitResetAt(time.Now().Add(time.Hour)).SetSchedulable(true).SaveX(ctx)
	manual := client.Account.Create().SetName("manual").SetPlatform(service.PlatformAnthropic).SetType(service.AccountTypeAPIKey).
		SetCredentials(map[string]any{"model_mapping": map[string]any{"not-visible": "claude-sonnet"}}).SetSchedulable(false).SaveX(ctx)
	gemini := client.Account.Create().SetName("target").SetPlatform(service.PlatformGemini).SetType(service.AccountTypeAPIKey).SetSchedulable(true).SaveX(ctx)
	pin := client.Account.Create().SetName("pin").SetPlatform(service.PlatformOpenAI).SetType(service.AccountTypeOAuth).SetSchedulable(true).SaveX(ctx)
	foreign := client.Account.Create().SetName("foreign").SetPlatform(service.PlatformAnthropic).SetType(service.AccountTypeAPIKey).SetSchedulable(true).SaveX(ctx)
	client.AccountGroup.Create().SetAccountID(cooling.ID).SetGroupID(source.ID).SaveX(ctx)
	client.AccountGroup.Create().SetAccountID(manual.ID).SetGroupID(source.ID).SaveX(ctx)
	client.AccountGroup.Create().SetAccountID(gemini.ID).SetGroupID(target.ID).SaveX(ctx)
	client.AccountGroup.Create().SetAccountID(foreign.ID).SetGroupID(unrelated.ID).SaveX(ctx)
	client.CompositeModelRoute.Create().SetGroupID(source.ID).SetPublicModel("gemini-alias").SetTargetGroupID(target.ID).
		SetTargetPlatform(service.PlatformGemini).SetEndpoint(service.CompositeRouteEndpointGemini).SaveX(ctx)
	client.CompositeModelRoute.Create().SetGroupID(source.ID).SetPublicModel("codex-alias").SetTargetAccountID(pin.ID).
		SetTargetPlatform(service.PlatformOpenAI).SetEnabled(false).SaveX(ctx)

	t.Run("configured catalog includes cooling accounts and does not read secrets", func(t *testing.T) {
		catalog, err := repo.ReadModelQuotaCatalog(ctx, source.ID, true)
		require.NoError(t, err)
		require.Len(t, catalog.Accounts, 1)
		require.Equal(t, cooling.ID, catalog.Accounts[0].ID)
		require.Equal(t, "claude-sonnet", catalog.Accounts[0].GetModelMapping()["mix-opus"])
		require.NotContains(t, catalog.Accounts[0].Credentials, "api_key")
		require.NotContains(t, catalog.Accounts[0].Extra, "internal_secret")
		require.Len(t, catalog.Routes, 1)
		require.Equal(t, "gemini-alias", catalog.Routes[0].PublicModel)
		require.Equal(t, target.ID, *catalog.Routes[0].TargetGroupID)
	})

	t.Run("catalog preserves passthrough model filter for listing parity", func(t *testing.T) {
		group := client.Group.Create().SetName("quota-openai-" + uuid.NewString()).SetPlatform(service.PlatformOpenAI).SaveX(ctx)
		accounts := make(map[bool]int64, 2)
		for _, enabled := range []bool{false, true} {
			account := client.Account.Create().
				SetName("quota-filter-" + uuid.NewString()).
				SetPlatform(service.PlatformOpenAI).
				SetType(service.AccountTypeAPIKey).
				SetCredentials(map[string]any{"model_mapping": map[string]any{"mapped-only": "upstream"}}).
				SetExtra(map[string]any{"openai_passthrough": true, "openai_passthrough_model_filter": enabled}).
				SetSchedulable(true).
				SaveX(ctx)
			client.AccountGroup.Create().SetAccountID(account.ID).SetGroupID(group.ID).SaveX(ctx)
			accounts[enabled] = account.ID
		}

		catalog, err := repo.ReadModelQuotaCatalog(ctx, group.ID, false)
		require.NoError(t, err)
		require.Len(t, catalog.Accounts, 2)
		projected := make(map[int64]service.Account, len(catalog.Accounts))
		for _, account := range catalog.Accounts {
			projected[account.ID] = account
		}
		for enabled, accountID := range accounts {
			account, ok := projected[accountID]
			require.True(t, ok)
			require.Equal(t, true, account.Extra["openai_passthrough"])
			require.Equal(t, enabled, account.Extra["openai_passthrough_model_filter"])
			require.Equal(t, enabled, account.IsOpenAIPassthroughModelFilterEnabled())
		}
	})

	t.Run("Anthropic variant metadata is group scoped and byte bounded", func(t *testing.T) {
		extra := map[string]any{service.UpstreamModelMetadataExtraKey: service.UpstreamModelMetadataSnapshot{
			Models: map[string]service.UpstreamModelMetadata{"long": {ContextWindow: 1_000_000}},
		}}
		client.Account.UpdateOneID(foreign.ID).SetExtra(extra).SaveX(ctx)
		catalog, err := repo.ReadModelQuotaCatalog(ctx, unrelated.ID, false)
		require.NoError(t, err)
		require.Len(t, catalog.Accounts, 1)
		meta, ok := catalog.Accounts[0].GetUpstreamModelMetadata("long")
		require.True(t, ok)
		require.EqualValues(t, 1_000_000, meta.ContextWindow)

		extra[service.UpstreamModelMetadataExtraKey] = map[string]any{"source": strings.Repeat("a", modelQuotaMappingBytes+1)}
		client.Account.UpdateOneID(foreign.ID).SetExtra(extra).SaveX(ctx)
		_, err = repo.ReadModelQuotaCatalog(ctx, unrelated.ID, false)
		require.ErrorIs(t, err, service.ErrModelQuotaCatalogBudget)
		_, err = repo.ReadModelQuotaCatalog(ctx, source.ID, true)
		require.NoError(t, err)
	})

	t.Run("bounded authorized candidates deduplicate pin and group membership", func(t *testing.T) {
		bounded, err := repo.ReadModelQuotaTargets(ctx, []int64{target.ID}, []int64{pin.ID, gemini.ID}, 1)
		require.NoError(t, err)
		require.False(t, bounded.Complete)
		require.Len(t, bounded.Accounts, 1)
		full, err := repo.ReadModelQuotaTargets(ctx, []int64{target.ID}, []int64{pin.ID, gemini.ID}, 2)
		require.NoError(t, err)
		require.True(t, full.Complete)
		require.Equal(t, []service.ModelQuotaAccountLink{{ID: gemini.ID, Groups: []int64{target.ID}}, {ID: pin.ID, Groups: []int64{}}}, full.Accounts)
		require.NotContains(t, full.Groups, unrelated.ID)
	})
}
