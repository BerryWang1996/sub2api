#!/usr/bin/env bash
# Replay database upgrades from earlier releases and compare the resulting
# schema with a fresh install of the current tree (needs Docker).
#
#   scripts/fork-migration-upgrade-test.sh <git-ref>...
#
# Each ref's backend/migrations directory is applied to an empty database,
# then the current migrations run on top. Typical refs: the fork commit before
# an upstream sync (what fork databases run today) and the latest upstream tag
# (what an official deployment switching to the fork runs today).
set -euo pipefail

if [ "$#" -eq 0 ]; then
  echo "usage: $0 <git-ref>..." >&2
  exit 2
fi

root=$(git rev-parse --show-toplevel)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

sep=":"
case "$(uname -s)" in MINGW*|MSYS*|CYGWIN*) sep=";" ;; esac

baselines=""
for ref in "$@"; do
  git -C "$root" rev-parse --verify --quiet "$ref^{commit}" >/dev/null || {
    echo "unknown ref: $ref" >&2
    exit 2
  }
  dir="$tmp/$(printf '%s' "$ref" | tr -c 'A-Za-z0-9._-' '_')"
  mkdir -p "$dir"
  # Raw blobs: migration checksums must match what that release embedded, so
  # no core.autocrlf conversion.
  git -C "$root" -c core.autocrlf=false archive "$ref" backend/migrations | tar -x -C "$dir" --strip-components=2
  if command -v cygpath >/dev/null 2>&1; then
    dir=$(cygpath -w "$dir")
  fi
  baselines="${baselines:+$baselines$sep}$dir"
done

cd "$root/backend"
SUB2API_MIGRATION_BASELINES="$baselines" \
  go test -tags integration -count=1 -run '^TestForkMigrationUpgradePaths$' -v ./internal/repository/
