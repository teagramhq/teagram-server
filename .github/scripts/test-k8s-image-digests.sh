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
# shellcheck disable=SC2016 # GitHub expressions are expected as literal workflow text.
[[ "$revision_expression" == '${{ github.sha }}' ]] ||
  fail 'image tags must identify the tested merge commit'
merge_group_types="$(yq -r '.on.merge_group.types[]' .github/workflows/ci.yml)"
[[ "$merge_group_types" == checks_requested ]] ||
  fail 'the CI workflow must validate merge-queue commits'

publish_step="$(yq -r '.jobs."publish-k8s-images".steps[] | select(.name == "Publish immutable commit tags and capture digests") | .run' .github/workflows/ci.yml)"
[[ "$publish_step" == *'.github/scripts/publish-k8s-image-digests.sh'* ]] ||
  fail 'the publisher must use the tested digest capture script'
# shellcheck disable=SC2016 # Match the literal expression in the workflow source.
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
    previous_attempts=0
    if [[ -f "$DOCKER_PUSH_ATTEMPTS_FILE" ]]; then
      previous_attempts="$(awk -v tag="$tag" '$0 == tag { count++ } END { print count + 0 }' "$DOCKER_PUSH_ATTEMPTS_FILE")"
    fi
    printf '%s\n' "$tag" >> "$DOCKER_PUSH_ATTEMPTS_FILE"
    if [[ "${DENY_FIRST_TAG:-}" == "$tag" && "$previous_attempts" -eq 0 ]]; then
      printf 'error from registry: denied\ndenied\n' >&2
      exit 1
    fi
    if [[ "${DENY_ALWAYS_TAG:-}" == "$tag" ]]; then
      printf 'error from registry: denied\ndenied\n' >&2
      exit 1
    fi
    if [[ "${FAIL_TAG:-}" == "$tag" ]]; then
      printf 'registry connection failed\n' >&2
      exit 1
    fi
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
cat > "$test_dir/bin/sleep" <<'SLEEP'
#!/usr/bin/env bash
set -euo pipefail
printf 'sleep:%s\n' "$*" >> "$SLEEP_CALLS_FILE"
SLEEP
chmod 0755 "$test_dir/bin/sleep"

PATH="$test_dir/bin:$PATH" \
  EXPECTED_REVISION="$revision" \
  DOCKER_CALLS_FILE="$test_dir/docker-calls" \
  DOCKER_PUSH_ATTEMPTS_FILE="$test_dir/docker-push-attempts" \
  SLEEP_CALLS_FILE="$test_dir/sleep-calls" \
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
    fail "the immutable tag for ${image} must use the tested commit revision"
done

retry_tag="ghcr.io/teagramhq/teagramd-mc-init:sha-$revision"
: > "$test_dir/docker-calls"
: > "$test_dir/docker-push-attempts"
: > "$test_dir/sleep-calls"
PATH="$test_dir/bin:$PATH" \
  EXPECTED_REVISION="$revision" \
  DOCKER_CALLS_FILE="$test_dir/docker-calls" \
  DOCKER_PUSH_ATTEMPTS_FILE="$test_dir/docker-push-attempts" \
  SLEEP_CALLS_FILE="$test_dir/sleep-calls" \
  DENY_FIRST_TAG="$retry_tag" \
  .github/scripts/publish-k8s-image-digests.sh "$test_dir/retried-digests.json" "$revision"

[[ "$(awk -v tag="$retry_tag" '$0 == tag { count++ } END { print count + 0 }' "$test_dir/docker-push-attempts")" -eq 2 ]] ||
  fail 'a denied first push must be retried exactly once'
[[ "$(wc -l < "$test_dir/sleep-calls")" -eq 1 && "$(cat "$test_dir/sleep-calls")" == sleep:10 ]] ||
  fail 'a denied first push must wait ten seconds before its single retry'
[[ "$(jq -r '.images | length' "$test_dir/retried-digests.json")" -eq 5 ]] ||
  fail 'a successful retry must allow all five digests to be published'

: > "$test_dir/docker-calls"
: > "$test_dir/docker-push-attempts"
: > "$test_dir/sleep-calls"
if PATH="$test_dir/bin:$PATH" \
  EXPECTED_REVISION="$revision" \
  DOCKER_CALLS_FILE="$test_dir/docker-calls" \
  DOCKER_PUSH_ATTEMPTS_FILE="$test_dir/docker-push-attempts" \
  SLEEP_CALLS_FILE="$test_dir/sleep-calls" \
  FAIL_TAG="$retry_tag" \
  .github/scripts/publish-k8s-image-digests.sh "$test_dir/non-denied-digests.json" "$revision" \
  2> "$test_dir/non-denied-error"; then
  fail 'a non-denied push failure must remain fatal'
fi
grep --fixed-strings --quiet "Could not push $retry_tag: registry connection failed" "$test_dir/non-denied-error" ||
  fail 'a non-denied push failure must retain its exact diagnostic'
[[ ! -s "$test_dir/sleep-calls" \
  && "$(awk -v tag="$retry_tag" '$0 == tag { count++ } END { print count + 0 }' "$test_dir/docker-push-attempts")" -eq 1 ]] ||
  fail 'a non-denied push failure must not be retried'
[[ ! -e "$test_dir/non-denied-digests.json" ]] ||
  fail 'a non-denied push failure must not publish a digest file'

: > "$test_dir/docker-calls"
: > "$test_dir/docker-push-attempts"
: > "$test_dir/sleep-calls"
if PATH="$test_dir/bin:$PATH" \
  EXPECTED_REVISION="$revision" \
  DOCKER_CALLS_FILE="$test_dir/docker-calls" \
  DOCKER_PUSH_ATTEMPTS_FILE="$test_dir/docker-push-attempts" \
  SLEEP_CALLS_FILE="$test_dir/sleep-calls" \
  DENY_ALWAYS_TAG="$retry_tag" \
  .github/scripts/publish-k8s-image-digests.sh "$test_dir/persistent-denied-digests.json" "$revision" \
  2> "$test_dir/persistent-denied-error"; then
  fail 'a second denied push must remain fatal'
fi
grep --fixed-strings --quiet "Could not push $retry_tag after one denied retry: error from registry: denied" \
  "$test_dir/persistent-denied-error" ||
  fail 'a second denied push must identify the image and retry failure'
[[ "$(awk -v tag="$retry_tag" '$0 == tag { count++ } END { print count + 0 }' "$test_dir/docker-push-attempts")" -eq 2 ]] ||
  fail 'a denied push must never be retried more than once'
[[ "$(wc -l < "$test_dir/sleep-calls")" -eq 1 && "$(cat "$test_dir/sleep-calls")" == sleep:10 ]] ||
  fail 'a persistent denial must not add another retry delay'
[[ ! -e "$test_dir/persistent-denied-digests.json" ]] ||
  fail 'a persistent denial must not publish a digest file'

printf 'K8s image digest publication test passed\n'
