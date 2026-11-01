#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root"

fail() {
  printf 'K8s image digest publication test: %s\n' "$1" >&2
  exit 1
}

revision=0123456789abcdef0123456789abcdef01234567
revision_expression="$(yq -r '.env.K8S_IMAGE_REVISION' .github/workflows/ci.yml)"
[[ "$revision_expression" == *"github.event.pull_request.head.sha"* &&
   "$revision_expression" == *"github.sha"* ]] ||
  fail 'image tags must identify the PR head, or the tested commit outside pull requests'

publish_step="$(yq -r '.jobs."publish-k8s-images".steps[] | select(.name == "Publish immutable commit tags and capture digests") | .run' .github/workflows/ci.yml)"
[[ "$publish_step" == *'.github/scripts/publish-k8s-image-digests.sh'* ]] ||
  fail 'the publisher must use the tested digest capture script'
[[ "$(yq -r '.jobs."publish-k8s-images".steps[] | select(.name == "Upload immutable image digests") | .with.name' .github/workflows/ci.yml)" == 'k8s-image-digests-${{ env.K8S_IMAGE_REVISION }}' ]] ||
  fail 'the digest artifact must be named for the image source revision'

test_dir="$(mktemp -d)"
trap 'rm -rf "$test_dir"' EXIT
mkdir -p "$test_dir/bin"
cat > "$test_dir/bin/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail

[[ "$1" == image ]] || exit 2
case "$2" in
  inspect)
    image="$3"
    format="${@: -1}"
    if [[ "$format" == *'org.opencontainers.image.revision'* ]]; then
      printf '%s\n' "$EXPECTED_REVISION"
    elif [[ "$format" == *'.RepoDigests'* ]]; then
      image_name="${image%%:sha-*}"
      printf '%s@sha256:%064x\n' "$image_name" 1
    else
      printf 'unexpected inspect format: %s\n' "$format" >&2
      exit 2
    fi
    ;;
  tag)
    ;;
  push)
    tag="${@: -1}"
    printf 'push:%s\n' "$tag" >> "$DOCKER_CALLS_FILE"
    # docker image push --quiet prints a tag/reference, not a bare digest.
    printf '%s\n' "$tag"
    ;;
  *)
    printf 'unexpected docker command: %s\n' "$*" >&2
    exit 2
    ;;
esac
DOCKER
chmod 0755 "$test_dir/bin/docker"

PATH="$test_dir/bin:$PATH" \
  EXPECTED_REVISION="$revision" \
  DOCKER_CALLS_FILE="$test_dir/docker-calls" \
  .github/scripts/publish-k8s-image-digests.sh "$test_dir/digests.json" "$revision"

jq --exit-status --arg revision "$revision" '
  .commit == $revision
  and [.images[].name] == [
    "ghcr.io/teagramhq/teagramd",
    "ghcr.io/teagramhq/teagram-migrations",
    "ghcr.io/teagramhq/teagramd-mc-init",
    "ghcr.io/teagramhq/link-edge-landing",
    "ghcr.io/teagramhq/link-edge-selector"
  ]
  and all(.images[]; (.reference | test("@sha256:[0-9a-f]{64}$")))
' "$test_dir/digests.json" >/dev/null || fail 'the artifact must contain all five immutable image digests'

for image in \
  ghcr.io/teagramhq/teagramd \
  ghcr.io/teagramhq/teagram-migrations \
  ghcr.io/teagramhq/teagramd-mc-init \
  ghcr.io/teagramhq/link-edge-landing \
  ghcr.io/teagramhq/link-edge-selector; do
  grep --fixed-strings --line-regexp --quiet "push:${image}:sha-${revision}" "$test_dir/docker-calls" ||
    fail "the immutable tag for ${image} must use the PR head revision"
done

printf 'K8s image digest publication test passed\n'
