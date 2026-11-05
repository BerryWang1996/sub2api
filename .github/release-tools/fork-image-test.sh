#!/usr/bin/env bash
set -euo pipefail

root=$(cd -- "$(dirname -- "$0")/../.." && pwd)
temporary_root=$(mktemp -d "${TMPDIR:-/tmp}/sub2api-image-test.XXXXXX")
temporary_root=$(cd -- "$temporary_root" && pwd)
cleanup() {
  [[ "$temporary_root" == */sub2api-image-test.* && -f "$temporary_root/.image-test" ]] || return 1
  rm -rf -- "$temporary_root"
}
touch "$temporary_root/.image-test"
trap cleanup EXIT
mkdir -p "$temporary_root/bin"
export FAKE_DOCKER_LOG="$temporary_root/docker.log"
export FAKE_DOCKER_STATE="$temporary_root/pushed"
export PUBLISH=true GITHUB_REF=refs/heads/main IMAGE=ghcr.io/berrywang1996/sub2api
export SOURCE_SHA=0123456789abcdef0123456789abcdef01234567
export RELEASE_TAG
RELEASE_TAG=$(sh "$root/backend/scripts/resolve-version.sh" --fork-release-tag)
export GITHUB_OUTPUT="$temporary_root/output"
export CONFIG_DIGEST="sha256:$(printf '%064d' 1)"
export TESTED_ID="$CONFIG_DIGEST"
export PUBLISHED_DIGEST="sha256:$(printf '%064d' 2)"
export OTHER_DIGEST="sha256:$(printf '%064d' 3)"

cat > "$temporary_root/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$FAKE_DOCKER_LOG"
case "$1 $2" in
  'image inspect') printf '%s\n' "$TESTED_ID" ;;
  'buildx imagetools')
    if [[ "${FAKE_MODE:-}" == exists ]]; then
      printf 'Digest: %s\n' "$PUBLISHED_DIGEST"
    elif [[ -s "$FAKE_DOCKER_STATE" ]]; then
      if [[ "${5:-}" == --raw ]]; then
        config_digest="$CONFIG_DIGEST"
        [[ "${FAKE_MODE:-}" != wrong_config ]] || config_digest="$PUBLISHED_DIGEST"
        printf '{"config":{"digest":"%s"}}\n' "$config_digest"
        exit 0
      fi
      digest="$PUBLISHED_DIGEST"
      if [[ "${FAKE_MODE:-}" == mismatch && "$4" == *:sha-* ]]; then
        digest="$TESTED_ID"
      elif [[ "${FAKE_MODE:-}" == containerd_mismatch && "$4" == *:sha-* ]]; then
        digest="$OTHER_DIGEST"
      fi
      printf 'Name: %s\nDigest: %s\n' "$4" "$digest"
    else
      case "${FAKE_MODE:-}" in
        missing_404) echo 'ERROR: unexpected status from HEAD request: 404 Not Found' >&2 ;;
        missing_short) printf 'ERROR: %s: not found\n' "$4" >&2 ;;
        auth) echo 'ERROR: failed to authorize: unauthorized: authentication required' >&2 ;;
        network) echo 'ERROR: failed to do request: TLS handshake timeout' >&2 ;;
        unknown) echo 'ERROR: unexpected registry failure' >&2 ;;
        *) echo 'ERROR: manifest unknown' >&2 ;;
      esac
      exit 1
    fi
    ;;
  *)
    case "$1" in
      tag) [[ "$2" == "$TESTED_ID" ]] ;;
      push) printf '%s\n' "$2" >> "$FAKE_DOCKER_STATE" ;;
      *) echo "Unexpected Docker command $*" >&2; exit 1 ;;
    esac
    ;;
esac
EOF
chmod +x "$temporary_root/bin/docker"
export PATH="$temporary_root/bin:$PATH"
[[ "$(command -v docker)" == "$temporary_root/bin/docker" ]] || exit 1

for scenario in missing_manifest missing_404 missing_short containerd_manifest; do
  : > "$FAKE_DOCKER_LOG"
  : > "$FAKE_DOCKER_STATE"
  : > "$GITHUB_OUTPUT"
  export FAKE_MODE="$scenario" TESTED_ID="$CONFIG_DIGEST"
  [[ "$scenario" != containerd_manifest ]] || export TESTED_ID="$PUBLISHED_DIGEST"
  bash "$root/.github/release-tools/publish-fork-image.sh" --check-only
  [[ ! -s "$GITHUB_OUTPUT" && ! -s "$FAKE_DOCKER_STATE" ]]
  ! grep -q '^image inspect\|^tag \|^push ' "$FAKE_DOCKER_LOG"
  : > "$FAKE_DOCKER_LOG"
  bash "$root/.github/release-tools/publish-fork-image.sh"
  [[ "$(cat "$GITHUB_OUTPUT")" == "digest=$PUBLISHED_DIGEST" ]]
  [[ "$(awk '$1 == "tag" {n++} END {print n+0}' "$FAKE_DOCKER_LOG")" == 2 ]]
  [[ "$(awk '$1 == "push" {n++} END {print n+0}' "$FAKE_DOCKER_LOG")" == 2 ]]
  grep -Fx "push $IMAGE:$RELEASE_TAG" "$FAKE_DOCKER_LOG" >/dev/null
  grep -Fx "push $IMAGE:sha-$SOURCE_SHA" "$FAKE_DOCKER_LOG" >/dev/null
  grep -Fx "buildx imagetools inspect $IMAGE@$PUBLISHED_DIGEST --raw" "$FAKE_DOCKER_LOG" >/dev/null
done

for scenario in exists mismatch wrong_config wrong_identity containerd_mismatch wrong_branch disabled auth network unknown; do
  : > "$FAKE_DOCKER_LOG"
  : > "$FAKE_DOCKER_STATE"
  : > "$GITHUB_OUTPUT"
  export FAKE_MODE="$scenario" GITHUB_REF=refs/heads/main PUBLISH=true TESTED_ID="$CONFIG_DIGEST"
  [[ "$scenario" != wrong_identity ]] || export TESTED_ID="$OTHER_DIGEST"
  [[ "$scenario" != containerd_mismatch ]] || export TESTED_ID="$PUBLISHED_DIGEST"
  [[ "$scenario" != wrong_branch ]] || export GITHUB_REF=refs/heads/feature
  [[ "$scenario" != disabled ]] || export PUBLISH=false
  if bash "$root/.github/release-tools/publish-fork-image.sh" >/dev/null 2>&1; then
    echo "Unexpected publication success for $scenario" >&2
    exit 1
  fi
  [[ ! -s "$GITHUB_OUTPUT" ]]
  if [[ "$scenario" != mismatch && "$scenario" != wrong_config && "$scenario" != wrong_identity && "$scenario" != containerd_mismatch ]]; then
    ! grep -q '^push ' "$FAKE_DOCKER_LOG"
  fi
done
echo 'Offline fork image publication checks passed'
