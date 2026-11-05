#!/usr/bin/env bash
set -euo pipefail

root=$(cd -- "$(dirname -- "$0")/../.." && pwd)
temporary_root=$(mktemp -d "${TMPDIR:-/tmp}/sub2api-version-test.XXXXXX")
temporary_root=$(cd -- "$temporary_root" && pwd)
cleanup() {
  # Delete only this test's resolved, uniquely named temporary workspace.
  [[ "$temporary_root" == */sub2api-version-test.* && -f "$temporary_root/.version-test" ]] || return 1
  rm -rf -- "$temporary_root"
}
touch "$temporary_root/.version-test"
trap cleanup EXIT
mkdir -p "$temporary_root/backend/scripts" "$temporary_root/backend/cmd/server" "$temporary_root/bin"
cp "$root/backend/scripts/resolve-version.sh" "$temporary_root/backend/scripts/resolve-version.sh"
resolver="$temporary_root/backend/scripts/resolve-version.sh"
version_file="$temporary_root/backend/cmd/server/VERSION"

# Controlled tag discovery exercises the upstream path without creating tags
# or commits in either the real checkout or the test workspace.
cat > "$temporary_root/bin/git" <<'EOF'
#!/bin/sh
[ -n "${TEST_EXACT_TAG:-}" ] || exit 1
printf '%s\n' "$TEST_EXACT_TAG"
EOF
chmod +x "$temporary_root/bin/git"
export PATH="$temporary_root/bin:$PATH"

printf 'v0.2.19-barry.2\r\n' > "$version_file"
[[ "$(sh "$resolver")" == v0.2.19-barry.2 ]]
[[ "$(TEST_EXACT_TAG=v0.2.19 sh "$resolver")" == v0.2.19-barry.2 ]]
[[ "$(sh "$resolver" --fork-release-tag v0.2.19-barry.2)" == v0.2.19-barry.2 ]]
[[ "$(sh "$resolver" --fork-release-tag '')" == v0.2.19-barry.2 ]]
for mismatched_tag in v0.2.19-barry.1 v0.2.19-barry.10 barry-20261028.1 v0.2.19 0.2.19-barry.2; do
  if sh "$resolver" --fork-release-tag "$mismatched_tag" >/dev/null 2>&1; then
    echo "Unexpectedly accepted mismatched tag $mismatched_tag" >&2
    exit 1
  fi
done
for invalid_version in v0.2.19-barry.0 v0.2.19-barry.02 v0.2.19-barry.sha barry-20261028.1 0.2.19; do
  printf '%s\n' "$invalid_version" > "$version_file"
  if sh "$resolver" --fork-release-tag >/dev/null 2>&1; then
    echo "Unexpectedly accepted invalid new release $invalid_version" >&2
    exit 1
  fi
done
printf 'v0.2.19-barry.10\n' > "$version_file"
[[ "$(sh "$resolver" --fork-release-tag v0.2.19-barry.10)" == v0.2.19-barry.10 ]]
printf '0.2.19\n' > "$version_file"
[[ "$(TEST_EXACT_TAG=v0.2.20 sh "$resolver")" == 0.2.20 ]]
[[ "$(TEST_EXACT_TAG= sh "$resolver")" == 0.2.19 ]]
printf '0.2.19-barry.abcdef123456\n' > "$version_file"
[[ "$(TEST_EXACT_TAG=v0.2.19 sh "$resolver")" == 0.2.19-barry.abcdef123456 ]]
echo 'Fork version resolution and tag agreement passed'
