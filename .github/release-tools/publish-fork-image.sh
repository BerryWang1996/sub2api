#!/usr/bin/env bash
set -euo pipefail

[[ "$#" == 0 || "$#" == 1 && "$1" == --check-only ]] || { echo 'usage: publish-fork-image.sh [--check-only]' >&2; exit 2; }

[[ "${PUBLISH:-}" == true && "${GITHUB_REF:-}" == refs/heads/main ]] || { echo 'Publish requires main and publish=true' >&2; exit 1; }
[[ "${IMAGE:-}" == ghcr.io/berrywang1996/sub2api ]] || { echo 'Unexpected fork image repository' >&2; exit 1; }
[[ "${SOURCE_SHA:-}" =~ ^[a-f0-9]{40}$ ]] || { echo 'A full source commit SHA is required' >&2; exit 1; }
[[ -n "${RELEASE_TAG:-}" && -n "${GITHUB_OUTPUT:-}" ]] || { echo 'Release tag and GitHub output path are required' >&2; exit 1; }
root=$(cd -- "$(dirname -- "$0")/../.." && pwd)
sh "$root/backend/scripts/resolve-version.sh" --fork-release-tag "$RELEASE_TAG" >/dev/null

release_image="$IMAGE:$RELEASE_TAG"
sha_image="$IMAGE:sha-$SOURCE_SHA"
if existing_manifest=$(docker buildx imagetools inspect "$release_image" 2>&1); then
  echo 'Release tag already exists; refusing to overwrite it' >&2
  exit 1
fi
case "${existing_manifest,,}" in
  *'manifest unknown'*|*'manifest_unknown'*|*'404 not found'*|"error: $release_image: not found") ;;
  *) echo 'Cannot establish that the release tag is absent; publication stopped' >&2; exit 1 ;;
esac
[[ "${1:-}" != --check-only ]] || exit 0

# Resolve the smoke-tested image once, then tag that exact local image ID.
# Publication performs no build and cannot substitute a newly built image.
tested_id=$(docker image inspect sub2api-fork-smoke:local --format '{{.Id}}')
[[ "$tested_id" =~ ^sha256:[a-f0-9]{64}$ ]] || { echo 'Invalid tested image ID' >&2; exit 1; }
docker tag "$tested_id" "$release_image"
docker tag "$tested_id" "$sha_image"
docker push "$release_image"
docker push "$sha_image"

# Check both published tags before recording the immutable deployment ref.
release_manifest=$(docker buildx imagetools inspect "$release_image")
sha_manifest=$(docker buildx imagetools inspect "$sha_image")
release_digest=$(awk '$1 == "Digest:" {print $2; exit}' <<< "$release_manifest")
sha_digest=$(awk '$1 == "Digest:" {print $2; exit}' <<< "$sha_manifest")
[[ "$release_digest" =~ ^sha256:[a-f0-9]{64}$ && "$release_digest" == "$sha_digest" ]] || { echo 'Published tags do not resolve to the same valid digest' >&2; exit 1; }
# Fetch by immutable digest so a concurrent tag edit cannot change the object
# used to compare the pushed manifest with the local smoke-tested image.
config_digest=$(docker buildx imagetools inspect "$IMAGE@$release_digest" --raw | "${PYTHON:-python3}" -c 'import json, sys; print(json.load(sys.stdin)["config"]["digest"])')
# Classic Docker stores expose the config ID; containerd stores expose the
# manifest ID. Either identity must match the same immutable published object.
[[ "$release_digest" == "$tested_id" || "$config_digest" == "$tested_id" ]] || { echo 'Published manifest does not contain the smoke-tested image' >&2; exit 1; }
printf 'digest=%s\n' "$release_digest" >> "$GITHUB_OUTPUT"
