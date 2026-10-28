//go:build integration

package repository

// Fork upgrade-path check (see FORK_MAINTENANCE.md, "数据库迁移与上游同步").
//
// A fresh install runs every upstream NNN_*.sql file and then the fork_*.sql
// files. A long-lived fork database got there in a different order: it applied
// fork migrations (possibly under older names) before upstream files that were
// merged later. This test replays that history from real migration directories
// of earlier releases and requires the resulting schema to match a fresh
// install, so an upstream sync that clashes with fork DDL fails here instead of
// at production startup.
//
// It is skipped unless SUB2API_MIGRATION_BASELINES lists one or more
// directories holding the backend/migrations tree of an earlier ref (separated
// by the OS path-list separator). scripts/fork-migration-upgrade-test.sh fills
// it from git refs.

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

const migrationBaselinesEnv = "SUB2API_MIGRATION_BASELINES"

func TestForkMigrationUpgradePaths(t *testing.T) {
	var baselines []string
	for _, dir := range filepath.SplitList(os.Getenv(migrationBaselinesEnv)) {
		if dir = strings.TrimSpace(dir); dir != "" {
			baselines = append(baselines, dir)
		}
	}
	if len(baselines) == 0 {
		t.Skipf("set %s (or run scripts/fork-migration-upgrade-test.sh) to check upgrade paths", migrationBaselinesEnv)
	}

	ctx := context.Background()
	container, err := tcpostgres.Run(
		ctx,
		selectDockerImage(ctx, postgresImageTag),
		tcpostgres.WithDatabase("fork_upgrade"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		tcpostgres.BasicWaitStrategies(),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(ctx) })
	dsn, err := container.ConnectionString(ctx, "sslmode=disable", "TimeZone=UTC")
	require.NoError(t, err)
	admin, err := openSQLWithRetry(ctx, dsn, 30*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })

	openScenario := func(t *testing.T, name string) *sql.DB {
		t.Helper()
		_, err := admin.ExecContext(ctx, "CREATE DATABASE "+name)
		require.NoError(t, err)
		u, err := url.Parse(dsn)
		require.NoError(t, err)
		u.Path = "/" + name
		db, err := openSQLWithRetry(ctx, u.String(), 30*time.Second)
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		return db
	}

	fresh := openScenario(t, "fresh_install")
	require.NoError(t, ApplyMigrations(ctx, fresh), "fresh install")
	want := snapshotSchema(t, ctx, fresh)

	for i, dir := range baselines {
		label := filepath.Base(dir)
		t.Run(label, func(t *testing.T) {
			db := openScenario(t, fmt.Sprintf("upgrade_%d", i))
			require.NoError(t, applyMigrationsFS(ctx, db, os.DirFS(dir)), "apply baseline %s", dir)
			require.NoError(t, ApplyMigrations(ctx, db), "upgrade from %s", label)
			require.NoError(t, ApplyMigrations(ctx, db), "re-run after upgrade from %s", label)
			if diff := diffSchemaSnapshots(want, snapshotSchema(t, ctx, db)); diff != "" {
				t.Fatalf("schema after upgrading from %s differs from a fresh install (- fresh, + upgraded):\n%s", label, diff)
			}
		})
	}
}

// snapshotSchema renders the public schema as sorted "kind name: definition"
// lines. Constraint and index names are included because fork migrations look
// objects up by name.
func snapshotSchema(t *testing.T, ctx context.Context, db *sql.DB) map[string]string {
	t.Helper()
	queries := []string{
		`SELECT 'column ' || table_name || '.' || column_name,
		        data_type || ' nullable=' || is_nullable || ' default=' || COALESCE(column_default, '')
		   FROM information_schema.columns
		  WHERE table_schema = 'public' AND table_name <> 'schema_migrations'`,
		`SELECT 'constraint ' || conrelid::regclass::text || '.' || conname, pg_get_constraintdef(oid)
		   FROM pg_constraint
		  WHERE connamespace = 'public'::regnamespace AND conrelid <> 0`,
		`SELECT 'index ' || indexname, indexdef
		   FROM pg_indexes
		  WHERE schemaname = 'public' AND tablename <> 'schema_migrations'`,
		`SELECT 'trigger ' || tgrelid::regclass::text || '.' || tgname, pg_get_triggerdef(oid)
		   FROM pg_trigger
		  WHERE NOT tgisinternal AND tgrelid::regclass::text NOT LIKE 'pg_%'`,
		`SELECT 'function ' || p.oid::regprocedure::text, md5(pg_get_functiondef(p.oid))
		   FROM pg_proc p
		  WHERE p.pronamespace = 'public'::regnamespace AND p.prokind IN ('f', 'p')`,
		`SELECT 'view ' || viewname, md5(definition)
		   FROM pg_views
		  WHERE schemaname = 'public'`,
	}
	snapshot := map[string]string{}
	for _, query := range queries {
		rows, err := db.QueryContext(ctx, query)
		require.NoError(t, err)
		for rows.Next() {
			var key, value string
			require.NoError(t, rows.Scan(&key, &value))
			snapshot[key] = value
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
	}
	return snapshot
}

func diffSchemaSnapshots(want, got map[string]string) string {
	keys := make([]string, 0, len(want)+len(got))
	for key := range want {
		keys = append(keys, key)
	}
	for key := range got {
		if _, ok := want[key]; !ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		w, inWant := want[key]
		g, inGot := got[key]
		switch {
		case !inGot:
			fmt.Fprintf(&b, "- %s: %s\n", key, w)
		case !inWant:
			fmt.Fprintf(&b, "+ %s: %s\n", key, g)
		case w != g:
			fmt.Fprintf(&b, "- %s: %s\n+ %s: %s\n", key, w, key, g)
		}
	}
	return b.String()
}
