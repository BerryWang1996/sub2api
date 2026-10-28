#!/usr/bin/env bash
# Merge upstream Wei-Shaw/sub2api main into the current fork branch and run
# the migration checks (FORK_MAINTENANCE.md, "数据库迁移与上游同步").
#
#   scripts/fork-sync.sh                      # fetch, merge, check
#   scripts/fork-sync.sh --check-only [<ref>] # check HEAD; <ref> is the fork
#                                             # commit before the sync (default:
#                                             # first parent of a merge HEAD)
#
# Never pushes. The upgrade-path test needs Docker; set SKIP_UPGRADE_TEST=1 to
# skip it (not recommended when upstream added migrations).
set -euo pipefail

root=$(git rev-parse --show-toplevel)
cd "$root"

upstream_remote=${UPSTREAM_REMOTE:-$(git remote -v | awk '$2 ~ /github\.com[:\/]Wei-Shaw\/sub2api(\.git)?$/ && $3 == "(fetch)" { print $1; exit }')}
if [ -z "$upstream_remote" ]; then
  echo "no remote points at Wei-Shaw/sub2api; add one or set UPSTREAM_REMOTE" >&2
  exit 2
fi
upstream_ref="$upstream_remote/main"

if [ "${1:-}" = "--check-only" ]; then
  previous=${2:-}
  if [ -z "$previous" ]; then
    git rev-parse --verify --quiet HEAD^2 >/dev/null || {
      echo "HEAD is not a merge commit; pass the pre-sync fork ref explicitly" >&2
      exit 2
    }
    previous=$(git rev-parse HEAD^1)
  fi
else
  if ! git diff --quiet || ! git diff --cached --quiet; then
    echo "commit or set aside tracked changes before syncing" >&2
    exit 1
  fi
  git fetch --tags "$upstream_remote" main
  previous=$(git rev-parse HEAD)
  if git merge-base --is-ancestor "$upstream_ref" HEAD; then
    echo "already contains $upstream_ref ($(git rev-parse --short "$upstream_ref")); checking anyway"
  elif ! git merge --no-ff --no-edit -m "Merge Wei-Shaw/sub2api main ($(git rev-parse "$upstream_ref"))" "$upstream_ref"; then
    echo
    echo "Merge stopped on conflicts. Resolve them keeping fork changes, never edit"
    echo "upstream NNN_*.sql files, commit, then run: $0 --check-only $previous"
    exit 1
  fi
fi

latest_tag=$(git describe --tags --abbrev=0 "$upstream_ref" 2>/dev/null || true)
echo "== migration guard (previous fork ref $(git rev-parse --short "$previous"))"
bash scripts/fork-migration-guard.sh "$upstream_ref" "$previous"
echo "== migration policy tests"
(cd backend && go test -count=1 ./migrations/)
if [ -n "${SKIP_UPGRADE_TEST:-}" ]; then
  echo "== upgrade-path test skipped (SKIP_UPGRADE_TEST)"
else
  echo "== upgrade-path test from $(git rev-parse --short "$previous")${latest_tag:+ and $latest_tag}"
  bash scripts/fork-migration-upgrade-test.sh "$previous" ${latest_tag:+"$latest_tag"}
fi
echo
echo "Sync checks passed. Review any guard warnings, run the full test suite, then push."
