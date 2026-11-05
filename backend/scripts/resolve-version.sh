#!/bin/sh
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
BACKEND_DIR="$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)"
REPO_DIR="$(CDPATH= cd -- "$BACKEND_DIR/.." && pwd)"
VERSION_FILE="$BACKEND_DIR/cmd/server/VERSION"
FILE_VERSION="$(tr -d '\r\n' < "$VERSION_FILE")"

if [ "${1:-}" = "--fork-release-tag" ]; then
  if ! printf '%s\n' "$FILE_VERSION" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)-barry\.[1-9][0-9]*$'; then
    echo 'VERSION must use vMAJOR.MINOR.PATCH-barry.N with a positive iteration' >&2
    exit 1
  fi
  if [ -n "${2:-}" ] && [ "$2" != "$FILE_VERSION" ]; then
    echo 'Release tag must exactly match backend/cmd/server/VERSION' >&2
    exit 1
  fi
  printf '%s\n' "$FILE_VERSION"
  exit 0
fi
if [ "$#" -gt 0 ]; then
  echo 'usage: resolve-version.sh [--fork-release-tag [tag]]' >&2
  exit 2
fi

# A fork checkout owns its release identity. An inherited upstream tag must
# not strip its fork suffix; the image workflow validates the same file.
case "$FILE_VERSION" in
  *-barry.*|barry-*) printf '%s\n' "$FILE_VERSION"; exit 0 ;;
esac

# Prefer the exact release tag when building from a tagged checkout so
# source builds from vX.Y.Z don't inherit the previous VERSION file value.
if command -v git >/dev/null 2>&1; then
  TAG="$(
    git -C "$REPO_DIR" describe --tags --exact-match --match 'v[0-9]*' 2>/dev/null || \
    git -C "$REPO_DIR" describe --tags --exact-match --match '[0-9]*' 2>/dev/null || \
    true
  )"
  if [ -n "$TAG" ]; then
    printf '%s\n' "${TAG#v}"
    exit 0
  fi
fi

printf '%s\n' "$FILE_VERSION"
