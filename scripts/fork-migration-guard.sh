#!/usr/bin/env bash
# Check that fork migrations stay out of upstream's way (FORK_MAINTENANCE.md,
# "数据库迁移与上游同步").
#
#   scripts/fork-migration-guard.sh <upstream-ref> [<previous-fork-ref>]
#
# Errors (exit 1):
#   - a numeric NNN_*.sql file that upstream-ref does not have (fork
#     migrations must use fork_NNN_*.sql);
#   - an upstream migration whose content differs from upstream-ref (editing
#     applied upstream files breaks checksums when switching images);
#   - an upstream migration that HEAD deleted although HEAD contains
#     upstream-ref.
# Warnings (exit 0): upstream migrations added since previous-fork-ref that
# mention tables, columns, indexes, constraints or other objects defined by
# fork migrations. Review those by hand: on fork databases they run after the
# fork DDL, on fresh installs before it.
set -euo pipefail

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
  echo "usage: $0 <upstream-ref> [<previous-fork-ref>]" >&2
  exit 2
fi
upstream=$1
previous=${2:-}
dir=backend/migrations
# Ref to check; defaults to HEAD (override to audit another commit).
head=${FORK_MIGRATION_GUARD_REF:-HEAD}
root=$(git rev-parse --show-toplevel)
cd "$root"

errors=0
warnings=0
error() {
  errors=$((errors + 1))
  if [ -n "${GITHUB_ACTIONS:-}" ]; then echo "::error::$*"; else echo "ERROR: $*"; fi
}
warn() {
  warnings=$((warnings + 1))
  if [ -n "${GITHUB_ACTIONS:-}" ]; then echo "::warning::$*"; else echo "WARNING: $*"; fi
}

# "<name> <blob>" per migration file, one git call per ref.
list_blobs() { git ls-tree "$1:$dir" | awk -F'\t' '$2 ~ /\.sql$/ { split($1, m, " "); print $2, m[3] }'; }

upstream_blobs=$(list_blobs "$upstream")
head_blobs=$(list_blobs "$head")
upstream_files=$(cut -d' ' -f1 <<< "$upstream_blobs")
head_files=$(cut -d' ' -f1 <<< "$head_blobs")

# kind name: "missing" (fork file with a numeric name), "modified", "deleted".
check_upstream=0
if git merge-base --is-ancestor "$upstream" "$head"; then check_upstream=1; fi
while read -r kind name; do
  case "$kind" in
    missing) error "$dir/$name is not in $upstream; fork migrations must be named fork_NNN_<desc>.sql" ;;
    modified) error "$dir/$name differs from $upstream; never edit upstream migrations, add a fork_NNN_ migration instead" ;;
    deleted) error "$dir/$name exists in $upstream but was deleted in $head" ;;
  esac
done < <(
  awk -v check_upstream="$check_upstream" '
    FNR == NR { up[$1] = $2; next }
    { here[$1] = 1 }
    $1 ~ /^fork_/ || $1 ~ /^[0-9][0-9][0-9]-fork-pre_/ { next }
    !($1 in up) { print "missing", $1; next }
    up[$1] != $2 { print "modified", $1 }
    END { if (check_upstream) for (n in up) if (!(n in here)) print "deleted", n }
  ' <(printf '%s\n' "$upstream_blobs") <(printf '%s\n' "$head_blobs")
)

if [ -n "$previous" ]; then
  fork_sql=$(
    grep -E '^(fork_|[0-9]{3}-fork-pre_)' <<< "$head_files" | while IFS= read -r name; do
      git show "$head:$dir/$name"
    done | sed 's/--.*$//'
  )
  identifiers=$(
    {
      grep -oiP '\b(ADD\s+COLUMN(\s+IF\s+NOT\s+EXISTS)?|CREATE\s+(TABLE|(UNIQUE\s+)?INDEX(\s+CONCURRENTLY)?|SEQUENCE|TYPE|VIEW|(OR\s+REPLACE\s+)?(FUNCTION|TRIGGER|VIEW))(\s+IF\s+NOT\s+EXISTS)?|ALTER\s+TABLE(\s+IF\s+EXISTS)?(\s+ONLY)?|CONSTRAINT|conname\s*=)\s+[\x27"]?\K[a-z_][a-z0-9_]*' <<< "$fork_sql" || true
    } | tr 'A-Z' 'a-z' | grep -vxE 'if|not|exists|only|concurrently|on|public' | sort -u
  )
  previous_files=$(list_blobs "$previous" | cut -d" " -f1)
  new_upstream=$(comm -13 <(sort <<< "$previous_files") <(sort <<< "$upstream_files") | grep -v '^fork_' || true)
  if [ -n "$identifiers" ] && [ -n "$new_upstream" ]; then
    while IFS= read -r name; do
      [ -n "$name" ] || continue
      hits=$(git show "$upstream:$dir/$name" | sed 's/--.*$//' | grep -owiFf <(printf '%s\n' $identifiers) | tr 'A-Z' 'a-z' | sort -u | paste -sd, - || true)
      [ -z "$hits" ] || warn "new upstream migration $dir/$name touches objects used by fork migrations: $hits"
    done <<< "$new_upstream"
  fi
  echo "new upstream migrations since $previous: $(grep -c . <<< "$new_upstream" || true)"
fi

echo "fork migration guard: $errors error(s), $warnings warning(s)"
[ "$errors" -eq 0 ]
