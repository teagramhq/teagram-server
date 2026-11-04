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

production_images="$(yq -o=json '.images' deploy/k8s/prod/kustomization.yaml)"
jq --exit-status '
  length == 5
  and map(.name) == [
    "ghcr.io/teagramhq/teagramd",
    "ghcr.io/teagramhq/teagram-migrations",
    "ghcr.io/teagramhq/teagramd-mc-init",
    "ghcr.io/teagramhq/link-edge-landing",
    "ghcr.io/teagramhq/link-edge-selector"
  ]
  and all(.[];
    (.digest | test("^sha256:[0-9a-f]{64}$"))
    and .digest != ("sha256:" + ("0" * 64)))
' <<<"$production_images" >/dev/null ||
  fail 'production images must have five non-placeholder SHA-256 digests'

migrations_digest="$(jq -r '.[] | select(.name == "ghcr.io/teagramhq/teagram-migrations") | .digest | sub("^sha256:"; "")' <<<"$production_images")"
rustfs_init_digest="$(jq -r '.[] | select(.name == "ghcr.io/teagramhq/teagramd-mc-init") | .digest | sub("^sha256:"; "")' <<<"$production_images")"
production_migration_version="$(yq -r '.configMapGenerator[] | select(.name == "required-migration-version").literals[] | sub("^migration-version="; "")' deploy/k8s/prod/kustomization.yaml)"
[[ "$production_migration_version" =~ ^[0-9]{14}$ ]] ||
  fail 'production must pin a migration version alongside its image digests'
production_migration_behavior="$(yq -r '.configMapGenerator[] | select(.name == "required-migration-version").behavior' deploy/k8s/prod/kustomization.yaml)"
[[ "$production_migration_behavior" == replace ]] ||
  fail 'production must replace the base migration version to keep the release pin stable'
ci_migration_version="$(yq -r '.configMapGenerator[] | select(.name == "required-migration-version").literals[] | sub("^migration-version="; "")' deploy/k8s/ci/kustomization.yaml)"
[[ "$ci_migration_version" == "$(tr -d '\r\n' < deploy/k8s/base/migration-version)" ]] ||
  fail 'the CI overlay must track the current source migration version'
ci_migration_behavior="$(yq -r '.configMapGenerator[] | select(.name == "required-migration-version").behavior' deploy/k8s/ci/kustomization.yaml)"
[[ "$ci_migration_behavior" == replace ]] ||
  fail 'the CI overlay must replace the production migration pin with the source version'
migrations_job_name="$(yq -r '.configMapGenerator[] | select(.name == "db-migrations-job-revision").literals[] | sub("^job-name="; "")' deploy/k8s/prod/kustomization.yaml)"
rustfs_init_job_name="$(yq -r '.configMapGenerator[] | select(.name == "rustfs-init-job-revision").literals[] | sub("^job-name="; "")' deploy/k8s/prod/kustomization.yaml)"
[[ "$migrations_job_name" == "db-migrations-${migrations_digest:0:16}" ]] ||
  fail 'the migration Job suffix must match the pinned migration image digest'
[[ "$rustfs_init_job_name" == "rustfs-init-${rustfs_init_digest:0:16}" ]] ||
  fail 'the RustFS initializer Job suffix must match the pinned init image digest'

ci_needs="$(yq -o=json '.jobs.ci.needs' .github/workflows/ci.yml)"
jq --exit-status 'index("publish-k8s-images") != null' <<<"$ci_needs" >/dev/null ||
  fail 'the required ci aggregate must include image publication'
publisher_needs="$(yq -o=json '.jobs."publish-k8s-images".needs' .github/workflows/ci.yml)"
jq --exit-status 'index("ci") == null' <<<"$publisher_needs" >/dev/null ||
  fail 'the image publisher must not depend on the ci aggregate'
publisher_if="$(yq -r '.jobs."publish-k8s-images".if' .github/workflows/ci.yml)"
[[ "$publisher_if" != *'needs.ci.result'* ]] ||
  fail 'the image publisher must not form a dependency cycle with ci'
[[ "$publisher_if" != *"github.event_name == 'merge_group'"* ]] ||
  fail 'the image publisher must not run on merge-group commits'
ci_gate="$(yq -r '.jobs.ci.steps[] | select(.name == "Require all CI lanes to pass").run' .github/workflows/ci.yml)"
# shellcheck disable=SC2016 # Match the literal merge-group condition in the workflow.
[[ "$ci_gate" == *'PUBLISH_K8S_IMAGES_RESULT'* &&
   "$ci_gate" == *'if [[ "$GITHUB_EVENT_NAME" == merge_group ]]; then'* &&
   "$ci_gate" == *'expected_publish_result=skipped'* ]] ||
  fail 'the ci aggregate must require publication except for intentionally skipped events'
grep --fixed-strings --quiet \
  "\$PUBLISH_K8S_IMAGES_RESULT\" != \"\$expected_publish_result" <<<"$ci_gate" ||
  fail 'the ci aggregate must fail when image publication does not match the expected result'

manifest_check="$(yq -r '.jobs."check-k8s-manifests".steps[] | select(.name == "Render and validate production and CI overlays").run' .github/workflows/ci.yml)"
[[ "$manifest_check" == *'.github/scripts/check-k8s-prod-image-pins.sh'* ]] ||
  fail 'manifest validation must reject placeholder production pins and mismatched Job suffixes'

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

case "$1" in
  manifest)
    [[ "$2" == inspect ]] || exit 2
    reference="$3"
    if [[ "${UNPULLABLE_REFERENCE:-}" == "$reference" ]]; then
      printf 'no such manifest: %s\n' "$reference" >&2
      exit 1
    fi
    printf '{}\n'
    ;;
  pull)
    reference="${@: -1}"
    [[ "${UNPULLABLE_REFERENCE:-}" != "$reference" ]] || exit 1
    ;;
  image)
    [[ "$2" == inspect ]] || exit 2
    format="${@: -1}"
    [[ "$format" == *'org.opencontainers.image.revision'* ]] || exit 2
    case "$3" in
      ghcr.io/teagramhq/teagramd@*) printf '%s\n' "$EXPECTED_SERVER_REVISION" ;;
      ghcr.io/teagramhq/teagram-migrations@*) printf '%s\n' "$EXPECTED_MIGRATION_REVISION" ;;
      *) printf 'unexpected image revision request: %s\n' "$3" >&2; exit 2 ;;
    esac
    ;;
  *)
    printf 'unexpected docker command: %s\n' "$*" >&2
    exit 2
    ;;
esac
DOCKER
chmod 0755 "$test_dir/bin/docker"

cat > "$test_dir/bin/git" <<'GIT'
#!/usr/bin/env bash
set -euo pipefail

case "$1" in
  cat-file) exit 1 ;;
  fetch) exit 0 ;;
  ls-tree)
    revision="$3"
    case "$revision" in
      "$EXPECTED_SERVER_REVISION") tree="$EXPECTED_SERVER_TREE" ;;
      "$EXPECTED_MIGRATION_REVISION") tree="$EXPECTED_MIGRATION_TREE" ;;
      *) printf 'unexpected source revision: %s\n' "$revision" >&2; exit 2 ;;
    esac
    printf '%s\n' "$tree"
    ;;
  *)
    printf 'unexpected git command: %s\n' "$*" >&2
    exit 2
    ;;
esac
GIT
chmod 0755 "$test_dir/bin/git"

valid_prod_manifest="$test_dir/k8s-prod.yaml"
teagramd_digest="$(printf '1%.0s' {1..64})"
migrations_digest="$(printf '2%.0s' {1..64})"
rustfs_init_digest="$(printf '3%.0s' {1..64})"
landing_digest="$(printf '4%.0s' {1..64})"
selector_digest="$(printf '5%.0s' {1..64})"
expected_migration_revision=abcdef0123456789abcdef0123456789abcdef01
expected_server_revision=abcdef0123456789abcdef0123456789abcdef02
export EXPECTED_MIGRATION_REVISION="$expected_migration_revision"
export EXPECTED_SERVER_REVISION="$expected_server_revision"
export EXPECTED_SERVER_TREE=$'100644 blob aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\tmigrations/20261102000002_existing.sql'
export EXPECTED_MIGRATION_TREE="$EXPECTED_SERVER_TREE"
printf '%s\n' \
  'apiVersion: v1' \
  'kind: Pod' \
  'spec:' \
  '  initContainers:' \
  '    - name: teagramd' \
  "      image: ghcr.io/teagramhq/teagramd@sha256:$teagramd_digest" \
  '    - name: migrations' \
  "      image: ghcr.io/teagramhq/teagram-migrations@sha256:$migrations_digest" \
  '    - name: rustfs-init' \
  "      image: ghcr.io/teagramhq/teagramd-mc-init@sha256:$rustfs_init_digest" \
  '  containers:' \
  '    - name: link-landing' \
  "      image: ghcr.io/teagramhq/link-edge-landing@sha256:$landing_digest" \
  '    - name: link-selector' \
  "      image: ghcr.io/teagramhq/link-edge-selector@sha256:$selector_digest" \
  '---' \
  'apiVersion: batch/v1' \
  'kind: Job' \
  'metadata:' \
  "  name: db-migrations-${migrations_digest:0:16}" \
  '---' \
  'apiVersion: batch/v1' \
  'kind: Job' \
  'metadata:' \
  "  name: rustfs-init-${rustfs_init_digest:0:16}" \
  '---' \
  'apiVersion: v1' \
  "kind: ConfigMap" \
  "metadata:" \
  "  name: required-migration-version-test" \
  "data:" \
  "  migration-version: \"$production_migration_version\"" > "$valid_prod_manifest"
PATH="$test_dir/bin:$PATH" \
  EXPECTED_MIGRATION_REVISION="$expected_migration_revision" \
  bash .github/scripts/check-k8s-prod-image-pins.sh "$valid_prod_manifest" >/dev/null ||
  fail 'the production image pin validator must accept valid images and matching Job suffixes'

drifted_migration_manifest="$test_dir/k8s-prod-drifted-migration.yaml"
sed "s|migration-version: \"$production_migration_version\"|migration-version: \"99999999999999\"|" \
  "$valid_prod_manifest" > "$drifted_migration_manifest"
if PATH="$test_dir/bin:$PATH" \
  EXPECTED_MIGRATION_REVISION="$expected_migration_revision" \
  bash .github/scripts/check-k8s-prod-image-pins.sh "$drifted_migration_manifest" \
  >"$test_dir/drifted-migration-output" 2>"$test_dir/drifted-migration-error"; then
  fail 'the production image pin validator must reject a migration version drift'
fi
grep --fixed-strings --quiet 'migration version' "$test_dir/drifted-migration-error" ||
  fail 'a migration version drift rejection must identify the mismatch'

placeholder_prod_manifest="$test_dir/k8s-prod-placeholder.yaml"
zero_digest="$(printf '%064d' 0)"
sed "s|teagramd@sha256:$teagramd_digest|teagramd@sha256:$zero_digest|" \
  "$valid_prod_manifest" > "$placeholder_prod_manifest"
if PATH="$test_dir/bin:$PATH" bash .github/scripts/check-k8s-prod-image-pins.sh "$placeholder_prod_manifest" \
  >"$test_dir/placeholder-output" 2>"$test_dir/placeholder-error"; then
  fail 'the production image pin validator must reject all-zero digests'
fi
grep --fixed-strings --quiet 'all-zero placeholder' "$test_dir/placeholder-error" ||
  fail 'a placeholder digest rejection must identify the invalid pin'

wrong_job_prod_manifest="$test_dir/k8s-prod-wrong-job.yaml"
sed "s|db-migrations-${migrations_digest:0:16}|db-migrations-0000000000000000|" \
  "$valid_prod_manifest" > "$wrong_job_prod_manifest"
if PATH="$test_dir/bin:$PATH" \
  EXPECTED_MIGRATION_REVISION="$expected_migration_revision" \
  bash .github/scripts/check-k8s-prod-image-pins.sh "$wrong_job_prod_manifest" \
  >"$test_dir/wrong-job-output" 2>"$test_dir/wrong-job-error"; then
  fail 'the production image pin validator must reject a migration Job suffix mismatch'
fi
grep --fixed-strings --quiet 'migration Job name' "$test_dir/wrong-job-error" ||
  fail 'a migration Job suffix rejection must identify the mismatch'

divergent_migration_prod_manifest="$test_dir/k8s-prod-divergent-migrations.yaml"
divergent_migration_version=20261105000001
sed "s|migration-version: \"$production_migration_version\"|migration-version: \"$divergent_migration_version\"|" \
  "$valid_prod_manifest" > "$divergent_migration_prod_manifest"
if PATH="$test_dir/bin:$PATH" \
  EXPECTED_SERVER_TREE=$'100644 blob aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\tmigrations/20261102000002_existing.sql\n100644 blob bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\tmigrations/20261104000001_required.sql' \
  EXPECTED_MIGRATION_TREE=$'100644 blob aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\tmigrations/20261102000002_existing.sql\n100644 blob cccccccccccccccccccccccccccccccccccccccc\tmigrations/20261105000001_other.sql' \
  bash .github/scripts/check-k8s-prod-image-pins.sh "$divergent_migration_prod_manifest" \
  >"$test_dir/divergent-migration-output" 2>"$test_dir/divergent-migration-error"; then
  fail 'the production migration image must contain the exact migrations required by the pinned teagramd image'
fi
grep --fixed-strings --quiet 'lacks the exact migration migrations/20261104000001_required.sql' \
  "$test_dir/divergent-migration-error" ||
  fail 'a divergent migration image rejection must identify the missing server migration'

if PATH="$test_dir/bin:$PATH" \
  EXPECTED_SERVER_TREE=$'100644 blob aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\tmigrations/20261102000002_existing.sql' \
  EXPECTED_MIGRATION_TREE=$'100644 blob bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\tmigrations/20261102000002_existing.sql' \
  bash .github/scripts/check-k8s-prod-image-pins.sh "$valid_prod_manifest" \
  >"$test_dir/different-migration-blob-output" 2>"$test_dir/different-migration-blob-error"; then
  fail 'the production migration image must contain the exact migration contents required by the pinned teagramd image'
fi
grep --fixed-strings --quiet 'lacks the exact migration migrations/20261102000002_existing.sql' \
  "$test_dir/different-migration-blob-error" ||
  fail 'a differing migration blob rejection must identify the changed migration path'

unpullable_digest="$(printf '9%.0s' {1..64})"
unpullable_reference="ghcr.io/teagramhq/teagramd@sha256:$unpullable_digest"
unpullable_prod_manifest="$test_dir/k8s-prod-unpullable.yaml"
sed "s|teagramd@sha256:$teagramd_digest|teagramd@sha256:$unpullable_digest|" \
  "$valid_prod_manifest" > "$unpullable_prod_manifest"
if PATH="$test_dir/bin:$PATH" UNPULLABLE_REFERENCE="$unpullable_reference" \
  bash .github/scripts/check-k8s-prod-image-pins.sh "$unpullable_prod_manifest" \
  >"$test_dir/unpullable-output" 2>"$test_dir/unpullable-error"; then
  fail 'the production image pin validator must reject registry references that do not resolve'
fi
grep --fixed-strings --quiet "$unpullable_reference" "$test_dir/unpullable-error" ||
  fail 'an unpullable digest rejection must identify the unavailable registry reference'

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
