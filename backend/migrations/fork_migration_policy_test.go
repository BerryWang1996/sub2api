package migrations

// Fork migration policy (see FORK_MAINTENANCE.md, "数据库迁移与上游同步").
//
// Upstream owns the numeric NNN_*.sql namespace and keeps adding files to it,
// so fork migrations live in fork_NNN_*.sql. Those names sort after every
// numeric upstream file, never collide with an upstream filename, and stay
// stable across syncs. Because long-lived fork databases apply new upstream
// files after fork files that a fresh install would run later, every fork
// migration must be safe to run against a schema that already contains its
// objects.

import (
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var forkMigrationName = regexp.MustCompile(`^fork_([0-9]{3})_[a-z0-9_]+\.sql$`)

func TestForkMigrationsUseForkNamespace(t *testing.T) {
	files, err := fs.Glob(FS, "*.sql")
	require.NoError(t, err)
	seen := map[string]string{}
	for _, name := range files {
		if !strings.HasPrefix(name, "fork_") {
			continue
		}
		m := forkMigrationName.FindStringSubmatch(name)
		require.NotNilf(t, m, "%s: fork migrations must be named fork_NNN_lower_snake.sql", name)
		if prev, ok := seen[m[1]]; ok {
			t.Fatalf("fork migration number %s used by both %s and %s", m[1], prev, name)
		}
		seen[m[1]] = name
	}
}

func TestForkMigrationsAreIdempotent(t *testing.T) {
	files, err := fs.Glob(FS, "fork_*.sql")
	require.NoError(t, err)
	for _, name := range files {
		content, err := FS.ReadFile(name)
		require.NoError(t, err)
		require.Emptyf(t, forkMigrationIdempotencyViolations(string(content)), "%s is not idempotent", name)
	}
}

func TestForkMigrationIdempotencyLint(t *testing.T) {
	ok := []string{
		"CREATE TABLE IF NOT EXISTS t (id BIGINT)",
		"create unique index concurrently if not exists i on t(id)",
		"ALTER TABLE t ADD COLUMN IF NOT EXISTS c INT, ADD COLUMN IF NOT EXISTS d INT",
		"ALTER TABLE t DROP COLUMN IF EXISTS c",
		"ALTER TABLE t ALTER COLUMN c DROP NOT NULL",
		"ALTER TABLE t ALTER COLUMN c DROP DEFAULT",
		"DROP INDEX CONCURRENTLY IF EXISTS i",
		"CREATE OR REPLACE FUNCTION f() RETURNS INT AS $$ SELECT 1 $$ LANGUAGE sql",
		"ALTER TYPE e ADD VALUE IF NOT EXISTS 'x'",
		"INSERT INTO t (id) VALUES (1) ON CONFLICT DO NOTHING",
		"INSERT INTO t (id) SELECT 1 WHERE NOT EXISTS (SELECT 1 FROM t)",
		"DO $$ BEGIN IF NOT EXISTS (SELECT 1) THEN ALTER TABLE t ADD CONSTRAINT k CHECK (c > 0); END IF; END $$",
		"DO $body$ BEGIN CREATE TYPE e AS ENUM ('a'); EXCEPTION WHEN duplicate_object THEN NULL; END $body$",
		"-- ALTER TABLE t ADD CONSTRAINT k CHECK (c > 0)\nSELECT 1",
	}
	for _, sql := range ok {
		require.Emptyf(t, forkMigrationIdempotencyViolations(sql), "%q", sql)
	}
	bad := []string{
		"CREATE TABLE t (id BIGINT)",
		"CREATE INDEX i ON t(id)",
		"ALTER TABLE t ADD COLUMN c INT",
		"ALTER TABLE t ADD c INT",
		"ALTER TABLE t ADD CONSTRAINT k CHECK (c > 0)",
		"ALTER TABLE t DROP COLUMN c",
		"DROP TABLE t",
		"CREATE FUNCTION f() RETURNS INT AS $$ SELECT 1 $$ LANGUAGE sql",
		"CREATE TYPE e AS ENUM ('a')",
		"CREATE TRIGGER g AFTER INSERT ON t FOR EACH ROW EXECUTE FUNCTION f()",
		"ALTER TABLE t RENAME COLUMN a TO b",
		"INSERT INTO t (id) VALUES (1)",
	}
	for _, sql := range bad {
		require.NotEmptyf(t, forkMigrationIdempotencyViolations(sql), "%q", sql)
	}
}

var (
	sqlLineComment  = regexp.MustCompile(`--[^\n]*`)
	sqlBlockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	sqlKeyword      = regexp.MustCompile(`(?i)\b(CREATE|DROP|ADD|RENAME)\s+`)

	idempotentCreate = regexp.MustCompile(`^(OR\s+REPLACE|TABLE\s+IF\s+NOT\s+EXISTS|(UNIQUE\s+)?INDEX\s+(CONCURRENTLY\s+)?IF\s+NOT\s+EXISTS|(SEQUENCE|EXTENSION|SCHEMA)\s+IF\s+NOT\s+EXISTS)\b`)
	idempotentDrop   = regexp.MustCompile(`^((TABLE|VIEW|SEQUENCE|TYPE|FUNCTION|TRIGGER|SCHEMA|EXTENSION|COLUMN|CONSTRAINT)\s+IF\s+EXISTS|INDEX\s+(CONCURRENTLY\s+)?IF\s+EXISTS|NOT\s+NULL|DEFAULT)\b`)
	idempotentAdd    = regexp.MustCompile(`^(COLUMN|VALUE)\s+IF\s+NOT\s+EXISTS\b`)
)

// forkMigrationIdempotencyViolations is a conservative lint: statements that
// can fail when their object already exists (or is already gone) must use the
// IF [NOT] EXISTS / OR REPLACE forms, or sit inside a DO block that checks the
// catalog first. Dollar-quoted bodies are trusted as guarded code.
func forkMigrationIdempotencyViolations(content string) []string {
	sql := stripDollarQuoted(sqlLineComment.ReplaceAllString(sqlBlockComment.ReplaceAllString(content, " "), " "))
	sql = strings.ToUpper(strings.Join(strings.Fields(sql), " "))
	var violations []string
	for _, loc := range sqlKeyword.FindAllStringSubmatchIndex(sql, -1) {
		keyword, rest := sql[loc[2]:loc[3]], sql[loc[1]:]
		var allowed *regexp.Regexp
		switch keyword {
		case "CREATE":
			allowed = idempotentCreate
		case "DROP":
			allowed = idempotentDrop
		case "ADD":
			allowed = idempotentAdd
		}
		if allowed == nil || !allowed.MatchString(rest) {
			violations = append(violations, fmt.Sprintf("%s %s", keyword, snippet(rest)))
		}
	}
	for _, stmt := range strings.Split(sql, ";") {
		stmt = strings.TrimSpace(stmt)
		if strings.HasPrefix(stmt, "INSERT ") && !strings.Contains(stmt, "ON CONFLICT") && !strings.Contains(stmt, "NOT EXISTS") {
			violations = append(violations, "INSERT without ON CONFLICT / WHERE NOT EXISTS: "+snippet(stmt))
		}
	}
	return violations
}

// stripDollarQuoted replaces $tag$...$tag$ bodies (DO blocks, function bodies)
// with a placeholder so the lint only inspects top-level statements.
func stripDollarQuoted(sql string) string {
	var b strings.Builder
	for {
		start := strings.IndexByte(sql, '$')
		if start < 0 {
			b.WriteString(sql)
			return b.String()
		}
		end := strings.IndexByte(sql[start+1:], '$')
		if end < 0 {
			b.WriteString(sql)
			return b.String()
		}
		tag := sql[start : start+end+2]
		if strings.ContainsAny(tag[1:len(tag)-1], " \t\n;()'") {
			b.WriteString(sql[:start+1])
			sql = sql[start+1:]
			continue
		}
		closeAt := strings.Index(sql[start+len(tag):], tag)
		if closeAt < 0 {
			b.WriteString(sql)
			return b.String()
		}
		b.WriteString(sql[:start])
		b.WriteString(" $BODY$ ")
		sql = sql[start+len(tag)+closeAt+len(tag):]
	}
}

func snippet(s string) string {
	if len(s) > 60 {
		return s[:60] + "..."
	}
	return s
}
