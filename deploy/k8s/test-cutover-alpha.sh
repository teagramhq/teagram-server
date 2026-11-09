#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root"

plan="$(deploy/k8s/cutover-alpha.sh --dry-run)"
for required in \
  'root-only cutover kubeconfig' \
  'Would server-dry-run and apply deploy/k8s/prod-cutover' \
  'source Compose containers and encrypted S3 authority' \
  'live teagramd and migration image revisions' \
  'main-branch build provenance for all five resolved image digests and the exact live revision' \
  'fence writes before the final PostgreSQL snapshot' \
  'stop all running Compose services except PostgreSQL and RustFS' \
  'keep source RustFS available for the post-fence S3 listing and mirror' \
  'PostgreSQL and RustFS stay running' \
  'verify no PostgreSQL client connections remain' \
  'save a mode-0600 pg_dump' \
  'sent over a temporary loopback port-forward' \
  '8 GiB PostgreSQL PVC can hold both the protected dump and restored database' \
  'server_key.pem, enc_key.hex, the blob-mode authority, and the retained local blob volume' \
  'compare the target Secret against the live encrypted-media key set without printing key values' \
  'teagramd-admin Secret' \
  'importer pod' \
  '/var/lib/teagram-k3s-cutover' \
  'mirror the already-encrypted source S3 objects into the new RustFS bucket without changing their bytes' \
  'object count, total bytes, and SHA-256 of every source and target object' \
  'the five tailnet-only HTTPS handlers' \
  'the old MTProto and Bot API TCP routes' \
  'enable PROXY protocol on the k3s Bot API relay' \
  'planned downtime from the write fence until all edges switch' \
  'Cloudflare tunnel route changes:' \
  'web.teagram.io: http://127.0.0.1:8081 -> http://linkselector:8081' \
  'server.teagram.io ^/apiws: http://127.0.0.1:2446 -> http://teagramd:2446' \
  'server.teagram.io ^/\.well-known/teagramd/client: http://127.0.0.1:2446 -> http://teagramd:2446' \
  'verify every k8s cloudflared pod has stopped before restoring routes' \
  'advertised MTProto TCP route on port 2443' \
  'explicit rollback after the first live Serve route' \
  'source RustFS bucket, old Compose volumes, database dump, and recovery evidence for at least seven days' \
  'PostgreSQL and source RustFS are already running' \
  'k3s writes since the switch are not copied back to the source bucket'; do
  if [[ $plan != *"$required"* ]]; then
    printf 'alpha cutover dry-run omitted: %s\n' "$required" >&2
    exit 1
  fi
done

# shellcheck disable=SC2016
for required_rollback in \
  'confirm ROUTE-RESTORED "Restore these Cloudflare tunnel routes (after -> before):' \
  'rollback)' \
  'server.teagram.io $CLOUDFLARE_APIWS_PATH: $CLOUDFLARE_SERVER_AFTER -> $CLOUDFLARE_SERVER_BEFORE' \
  'server.teagram.io $CLOUDFLARE_DISCOVERY_PATH: $CLOUDFLARE_SERVER_AFTER -> $CLOUDFLARE_SERVER_BEFORE'; do
  if ! grep --fixed-strings --quiet -- "$required_rollback" deploy/k8s/cutover-alpha.sh; then
    printf 'alpha cutover rollback route plan omitted: %s\n' "$required_rollback" >&2
    exit 1
  fi
done

# These phrases are literal documentation checks, including backticks.
# shellcheck disable=SC2016
for required_runbook in \
  'must stay online because the post-fence source S3 listing' \
  'bucket remains the pre-switch recovery copy for at least seven days' \
  'PostgreSQL and source RustFS were already running' \
  'GitHub CLI (`gh`)' \
  'GH_TOKEN' \
  'GHCR_USERNAME' \
  'sudo --preserve-env=GH_TOKEN,GHCR_USERNAME' \
  'writes GHCR auth to a mode-0600 temporary' \
  'Docker config' \
  'gh attestation verify' \
  'source ref `refs/heads/main`' \
  '`.github/workflows/ci.yml` as the signer'; do
  if ! grep --fixed-strings --quiet -- "$required_runbook" deploy/k8s/README.md; then
    printf 'alpha cutover runbook omitted: %s\n' "$required_runbook" >&2
    exit 1
  fi
done

if ! grep --fixed-strings --quiet -- \
  'readonly KUBECONFIG_PATH=/root/teagram-k3s-cutover.kubeconfig' deploy/k8s/cutover-alpha.sh ||
  ! grep --fixed-strings --quiet -- \
  'KUBECONFIG=/root/teagram-k3s-cutover.kubeconfig kubectl delete' deploy/k8s/README.md; then
  printf 'alpha cutover or retry cleanup does not use the root-only admin kubeconfig\n' >&2
  exit 1
fi
if sed -n '/^After accepting the failed target copy/,$p' deploy/k8s/README.md |
  grep --fixed-strings --quiet -- 'KUBECONFIG=/root/teagram-k3s-deploy.kubeconfig'; then
  printf 'alpha cutover retry cleanup uses the restricted deployer kubeconfig\n' >&2
  exit 1
fi

for required in \
  'Retry after a pre-switch failure' \
  '-k deploy/k8s/prod-cutover' \
  'secret teagramd-admin' \
  'data-postgres-0' \
  'data-rustfs-0' \
  'key-data-teagramd-0' \
  'legacy-blobs-teagramd-0' \
  'blob-mode-teagramd-0' \
  '/var/lib/teagram-k3s-cutover'; do
  if [[ $required == 'Retry after a pre-switch failure' ]]; then
    file=deploy/k8s/cutover-alpha.sh
  else
    file=deploy/k8s/README.md
  fi
  if ! grep --fixed-strings --quiet -- "$required" "$file"; then
    printf 'alpha cutover retry instructions omit: %s\n' "$required" >&2
    exit 1
  fi
done

if ! grep --fixed-strings --quiet -- "--tcp=\"\$MTPROTO_PORT\"" deploy/k8s/cutover-alpha.sh ||
  ! grep --fixed-strings --quiet "\"\$MTPROTO_PORT:\$MTPROTO_PORT" deploy/k8s/cutover-alpha.sh ||
  ! grep --fixed-strings --quiet '"TCP"' deploy/k8s/cutover-alpha.sh ||
  ! grep --fixed-strings --quiet 'https://127.0.0.1:6443' deploy/k8s/cutover-alpha.sh; then
  printf 'alpha cutover does not switch and verify the advertised MTProto TCP route\n' >&2
  exit 1
fi

for helper in kubectl_resource_exists wait_for_no_pods recover_after_serve_switch; do
  if ! grep --extended-regexp --quiet "^${helper}\\(\\)" deploy/k8s/cutover-alpha.sh; then
    printf 'alpha cutover does not provide its guarded recovery helper: %s\n' "$helper" >&2
    exit 1
  fi
done

if ! grep --fixed-strings --quiet 'test-cutover-alpha-k3s.sh' .github/workflows/ci.yml ||
  ! grep --fixed-strings --quiet 'cutover-alpha.sh --apply' deploy/k8s/test-cutover-alpha-k3s.sh ||
  ! grep --fixed-strings --quiet 'cutover-alpha.sh --rollback' deploy/k8s/test-cutover-alpha-k3s.sh; then
  printf 'k3s cutover integration does not execute the actual apply and rollback runner\n' >&2
  exit 1
fi
# shellcheck disable=SC2016
if ! grep --fixed-strings --quiet \
  'LINK_SELECTOR_IMAGE=${CUTOVER_TEST_LINK_SELECTOR_IMAGE:-ghcr.io/teagramhq/link-edge-selector@$selector_digest}' \
  deploy/k8s/cutover-alpha.sh ||
  ! grep --fixed-strings --quiet \
    'CUTOVER_TEST_LINK_SELECTOR_IMAGE=ghcr.io/teagramhq/link-edge-selector:ci' \
    deploy/k8s/test-cutover-alpha-k3s.sh; then
  printf 'k3s cutover integration does not run the selector image built for this revision\n' >&2
  exit 1
fi

prod_app_digest=$(yq -r '.images[] | select(.name == "ghcr.io/teagramhq/teagramd") | .digest' \
  deploy/k8s/prod/kustomization.yaml)
prod_migrations_digest=$(yq -r '.images[] | select(.name == "ghcr.io/teagramhq/teagram-migrations") | .digest' \
  deploy/k8s/prod/kustomization.yaml)
prod_helper_digest=$(yq -r '.images[] | select(.name == "ghcr.io/teagramhq/teagramd-mc-init") | .digest' \
  deploy/k8s/prod/kustomization.yaml)
prod_landing_digest=$(yq -r '.images[] | select(.name == "ghcr.io/teagramhq/link-edge-landing") | .digest' \
  deploy/k8s/prod/kustomization.yaml)
prod_selector_digest=$(yq -r '.images[] | select(.name == "ghcr.io/teagramhq/link-edge-selector") | .digest' \
  deploy/k8s/prod/kustomization.yaml)
cutover_migrations_digest=$(yq -r '.images[] | select(.name == "ghcr.io/teagramhq/teagram-migrations") | .digest' \
  deploy/k8s/prod-cutover/kustomization.yaml)
[[ $cutover_migrations_digest == null ]] && cutover_migrations_digest=''
if [[ ! $prod_app_digest =~ ^sha256:[0-9a-f]{64}$ ||
  ! $prod_migrations_digest =~ ^sha256:[0-9a-f]{64}$ ||
  ! $prod_helper_digest =~ ^sha256:[0-9a-f]{64}$ ||
  ! $prod_landing_digest =~ ^sha256:[0-9a-f]{64}$ ||
  ! $prod_selector_digest =~ ^sha256:[0-9a-f]{64}$ ||
  ( -n $cutover_migrations_digest && $cutover_migrations_digest != "$prod_migrations_digest" ) ]] ||
  ! grep --fixed-strings --quiet -- '  - ../prod' deploy/k8s/prod-cutover/kustomization.yaml ||
  grep --fixed-strings --quiet 'local-blob-migration' deploy/k8s/prod-cutover/kustomization.yaml; then
  printf 'alpha cutover does not use the shared pinned migration image and Job\n' >&2
  exit 1
fi

for path in \
  '/apiws' \
  '/admin' \
  '/.well-known/teagramd/client' \
  '/.well-known/telegramd/client'; do
  if ! grep --fixed-strings --quiet "$path" deploy/k8s/cutover-alpha.sh; then
    printf 'alpha cutover does not route Serve path: %s\n' "$path" >&2
    exit 1
  fi
done

if ! grep --extended-regexp --quiet 'backend_type.*client backend.*pg_backend_pid' deploy/k8s/cutover-alpha.sh; then
  printf 'alpha cutover does not verify source PostgreSQL client sessions are fenced\n' >&2
  exit 1
fi

if ! grep --fixed-strings --quiet 'TG_ADMIN_ORIGIN' deploy/k8s/cutover-alpha.sh ||
  ! grep --fixed-strings --quiet 'TG_ADMIN_ORIGIN' deploy/k8s/prod/teagramd-listeners.yaml; then
  printf 'alpha cutover does not preserve the existing admin origin configuration\n' >&2
  exit 1
fi

if [[ $plan == *'TUNNEL_TOKEN'* || $plan == *'POSTGRES_PASSWORD'* || $plan == *'server_key.pem SHA'* ]]; then
  printf 'alpha cutover dry-run exposed a secret identifier or key fingerprint\n' >&2
  exit 1
fi

fixture_dir=$(mktemp -d)
trap 'rm -rf -- "$fixture_dir"' EXIT
mkdir -p "$fixture_dir/bin" "$fixture_dir/state" "$fixture_dir/teagram-cutover-runner"
# Exercise the live-image renderer with digest-derived Job names that differ
# from the committed production image pins. The cutover and normal prod Jobs
# must keep matching selectors and pod labels after the image changes.
prod_migration_job="db-migrations-$(printf '%s' "$prod_migrations_digest" | cut -c8-23)"
prod_rustfs_job="rustfs-init-$(printf '%s' "$prod_helper_digest" | cut -c8-23)"
live_migrations_digest=sha256:0123456789abcdef000000000000000000000000000000000000000000000000
live_helper_digest=sha256:fedcba9876543210000000000000000000000000000000000000000000000000
live_migration_job="db-migrations-$(printf '%s' "$live_migrations_digest" | cut -c8-23)"
live_rustfs_job="rustfs-init-$(printf '%s' "$live_helper_digest" | cut -c8-23)"
if [[ $live_migration_job == "$prod_migration_job" || $live_rustfs_job == "$prod_rustfs_job" ]]; then
  printf 'renderer regression fixture must use live Job digests different from production pins\n' >&2
  exit 1
fi
cat > "$fixture_dir/prod-cutover.yaml" <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: $prod_migration_job
spec:
  selector:
    matchLabels:
      job-revision: $prod_migration_job
  template:
    metadata:
      labels:
        job-revision: $prod_migration_job
---
apiVersion: batch/v1
kind: Job
metadata:
  name: $prod_rustfs_job
spec:
  selector:
    matchLabels:
      job-revision: $prod_rustfs_job
  template:
    metadata:
      labels:
        job-revision: $prod_rustfs_job
EOF
eval "$(sed -n '/^render_cutover_manifest() {/,/^check_source_migration_version() {/p' \
  deploy/k8s/cutover-alpha.sh | sed '$d')"
kubectl() {
  [[ $1 == kustomize && $2 == "$REPO_ROOT/deploy/k8s/prod-cutover" ]] || return 1
  cat "$fixture_dir/prod-cutover.yaml"
}
REPO_ROOT=$repo_root
kubectl kustomize "$REPO_ROOT/deploy/k8s/prod-cutover" >/dev/null
export NAMESPACE=teagram-cutover
export APP_IMAGE=ghcr.io/teagramhq/teagramd@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
export MIGRATIONS_IMAGE=ghcr.io/teagramhq/teagram-migrations@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
export K8S_MC_IMAGE=ghcr.io/teagramhq/teagramd-mc-init@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
export LINK_LANDING_IMAGE=ghcr.io/teagramhq/link-edge-landing@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
export LINK_SELECTOR_IMAGE=ghcr.io/teagramhq/link-edge-selector@sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
export MIGRATIONS_JOB_NAME=$live_migration_job
export RUSTFS_INIT_JOB_NAME=$live_rustfs_job
export REQUIRED_MIGRATION_VERSION=20261105000021
render_cutover_manifest "$fixture_dir/rendered-cutover.yaml"
for dynamic_job in "$live_migration_job" "$live_rustfs_job"; do
  if ! EXPECTED_JOB_NAME="$dynamic_job" yq --exit-status \
    'select(.kind == "Job" and .metadata.name == strenv(EXPECTED_JOB_NAME)) |
      (.metadata.name == strenv(EXPECTED_JOB_NAME) and
       .spec.selector.matchLabels."job-revision" == strenv(EXPECTED_JOB_NAME) and
       .spec.template.metadata.labels."job-revision" == strenv(EXPECTED_JOB_NAME))' \
    "$fixture_dir/rendered-cutover.yaml" >/dev/null; then
    printf 'renderer did not apply the dynamic Job name to its selector and pod label: %s\n' \
      "$dynamic_job" >&2
    exit 1
  fi
done
if grep --fixed-strings --quiet -- "job-revision: $prod_migration_job" "$fixture_dir/rendered-cutover.yaml" ||
  grep --fixed-strings --quiet -- "job-revision: $prod_rustfs_job" "$fixture_dir/rendered-cutover.yaml"; then
  printf 'renderer left a production Job selector or pod label beside a dynamic Job name\n' >&2
  exit 1
fi
unset -f kubectl
# Exercise the production release lookup branch with fake Incus image output.
# A stale published revision must stop validation; matching revision labels
# must yield immutable image references and digest-derived Job names.
cat > "$fixture_dir/release-prod-cutover.yaml" <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: $prod_migration_job
spec:
  selector:
    matchLabels:
      job-revision: $prod_migration_job
  template:
    metadata:
      labels:
        job-revision: $prod_migration_job
    spec:
      containers:
        - name: migrations
          image: ghcr.io/teagramhq/teagram-migrations@$prod_migrations_digest
---
apiVersion: batch/v1
kind: Job
metadata:
  name: $prod_rustfs_job
spec:
  selector:
    matchLabels:
      job-revision: $prod_rustfs_job
  template:
    metadata:
      labels:
        job-revision: $prod_rustfs_job
    spec:
      containers:
        - name: rustfs-init
          image: ghcr.io/teagramhq/teagramd-mc-init@$prod_helper_digest
---
apiVersion: v1
kind: Pod
metadata:
  name: cutover-release-images
spec:
  containers:
    - name: teagramd
      image: ghcr.io/teagramhq/teagramd@$prod_app_digest
    - name: linklanding
      image: ghcr.io/teagramhq/link-edge-landing@$prod_landing_digest
    - name: linkselector
      image: ghcr.io/teagramhq/link-edge-selector@$prod_selector_digest
EOF

live_revision=0123456789abcdef0123456789abcdef01234567
mismatched_revision=ffffffffffffffffffffffffffffffffffffffff
test_release_app_digest="sha256:$(printf '%016x%048x' 0xa0a0a0a0a0a0a0a0 0)"
test_untrusted_app_digest=sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
test_release_migrations_digest="sha256:$(printf '%016x%048x' 0xb1b1b1b1b1b1b1b1 0)"
test_release_helper_digest="sha256:$(printf '%016x%048x' 0xc2c2c2c2c2c2c2c2 0)"
test_release_landing_digest="sha256:$(printf '%016x%048x' 0xd3d3d3d3d3d3d3d3 0)"
test_release_selector_digest="sha256:$(printf '%016x%048x' 0xe4e4e4e4e4e4e4e4 0)"
run_production_release_case() (
  local mode=$1 rendered_manifest=$2 auth_config_dir
  unset GH_TOKEN GHCR_USERNAME
  AUTH_CONFIG_LOG=$fixture_dir/production-auth-config-paths
  export AUTH_CONFIG_LOG
  if [[ $mode != missing-auth ]]; then
    export GH_TOKEN=cutover-test-token GHCR_USERNAME=cutover-test-user
  fi
  export SOURCE_INSTANCE=teagram
  export TEST_MODE=false
  export LIVE_REVISION=$live_revision
  export MISMATCHED_REVISION=$mismatched_revision
  export RELEASE_CASE=$mode
  RELEASE_APP_DIGEST=$test_release_app_digest
  RELEASE_MIGRATIONS_DIGEST=$test_release_migrations_digest
  RELEASE_HELPER_DIGEST=$test_release_helper_digest
  RELEASE_LANDING_DIGEST=$test_release_landing_digest
  RELEASE_SELECTOR_DIGEST=$test_release_selector_digest
  TRUSTED_APP_DIGEST=$RELEASE_APP_DIGEST
  TRUSTED_MIGRATIONS_DIGEST=$RELEASE_MIGRATIONS_DIGEST
  TRUSTED_HELPER_DIGEST=$RELEASE_HELPER_DIGEST
  TRUSTED_LANDING_DIGEST=$RELEASE_LANDING_DIGEST
  TRUSTED_SELECTOR_DIGEST=$RELEASE_SELECTOR_DIGEST
  if [[ $mode == digest-mismatch ]]; then
    TRUSTED_APP_DIGEST=$test_untrusted_app_digest
  fi
  export RELEASE_APP_DIGEST RELEASE_MIGRATIONS_DIGEST RELEASE_HELPER_DIGEST \
    RELEASE_LANDING_DIGEST RELEASE_SELECTOR_DIGEST TRUSTED_APP_DIGEST \
    TRUSTED_MIGRATIONS_DIGEST TRUSTED_HELPER_DIGEST TRUSTED_LANDING_DIGEST \
    TRUSTED_SELECTOR_DIGEST
  export NAMESPACE=teagram-cutover
  export REPO_ROOT=$repo_root
  eval "$(sed -n '/^source_container_revision() {/,/^prod_image_digest() {/p' \
    deploy/k8s/cutover-alpha.sh | sed '$d')"
  eval "$(sed -n '/^source_docker_helper_enabled() {/,/^resolve_image_revision() {/p' \
    deploy/k8s/cutover-alpha.sh | sed '$d')"
  eval "$(sed -n '/^require_release_auth() {/,/^}/p' deploy/k8s/cutover-alpha.sh)"
  eval "$(sed -n '/^verify_main_release_image() {/,/^resolve_image_revision() {/p' \
    deploy/k8s/cutover-alpha.sh | sed '$d')"
  eval "$(sed -n '/^resolve_image_revision() {/,/^resolve_release_image() {/p' \
    deploy/k8s/cutover-alpha.sh | sed '$d')"
  eval "$(sed -n '/^resolve_release_image() {/,/^image_latest_migration() {/p' \
    deploy/k8s/cutover-alpha.sh | sed '$d')"
  eval "$(sed -n '/^image_latest_migration() {/,/^validate_release_for_source() {/p' \
    deploy/k8s/cutover-alpha.sh | sed '$d')"
  eval "$(sed -n '/^validate_release_for_source() {/,/^render_cutover_manifest() {/p' \
    deploy/k8s/cutover-alpha.sh | sed '$d')"
  fail() { printf 'k3s cutover: %s\n' "$*" >&2; exit 1; }
  # shellcheck disable=SC2329 # This test invokes the stub through eval'd source.
  gh() {
    [[ -n ${GH_TOKEN:-} && ${DOCKER_CONFIG:-} == /* &&
      -d $DOCKER_CONFIG && $(stat -c '%a' "$DOCKER_CONFIG") == 700 &&
      -f $DOCKER_CONFIG/config.json &&
      $(stat -c '%a' "$DOCKER_CONFIG/config.json") == 600 ]] || return 91
    python3 -c '
import base64
import json
import sys

with open(sys.argv[1], encoding="utf-8") as config_file:
    auth = json.load(config_file)["auths"]["ghcr.io"]["auth"]
assert base64.b64decode(auth).decode() == sys.argv[2] + ":" + sys.argv[3]
' "$DOCKER_CONFIG/config.json" "$GHCR_USERNAME" "$GH_TOKEN" || return 91
    printf '%s\n' "$DOCKER_CONFIG" >> "$AUTH_CONFIG_LOG"
    [[ $1 == attestation && $2 == verify && $4 == --repo &&
      $5 == teagramhq/teagram-server && $6 == --source-ref &&
      $7 == refs/heads/main && $8 == --source-digest &&
      $9 == "$LIVE_REVISION" && ${10} == --signer-workflow &&
      ${11} == teagramhq/teagram-server/.github/workflows/ci.yml &&
      ${12} == --format && ${13} == json ]] || return 91
    local expected_digest
    case $3 in
      oci://ghcr.io/teagramhq/teagramd@*) expected_digest=$TRUSTED_APP_DIGEST ;;
      oci://ghcr.io/teagramhq/teagram-migrations@*) expected_digest=$TRUSTED_MIGRATIONS_DIGEST ;;
      oci://ghcr.io/teagramhq/teagramd-mc-init@*) expected_digest=$TRUSTED_HELPER_DIGEST ;;
      oci://ghcr.io/teagramhq/link-edge-landing@*) expected_digest=$TRUSTED_LANDING_DIGEST ;;
      oci://ghcr.io/teagramhq/link-edge-selector@*) expected_digest=$TRUSTED_SELECTOR_DIGEST ;;
      *) return 91 ;;
    esac
    [[ $3 == *@$expected_digest ]]
  }
  incus() {
    [[ $1 == exec && $2 == "$SOURCE_INSTANCE" && $3 == -- ]] || return 91
    shift 3
    [[ $1 == docker ]] || return 91
    shift
    case $1 in
      inspect)
        [[ $2 == --format && $4 == teagram-teagramd-1 ]] || return 91
        printf '%s\n' "$LIVE_REVISION"
        ;;
      pull) return 0 ;;
      image)
        [[ $2 == inspect && $4 == --format ]] || return 91
        local image=$3 format=$5
        if [[ $format == *RepoDigests* ]]; then
          case $image in
            ghcr.io/teagramhq/teagramd:sha-*)
              printf 'ghcr.io/teagramhq/teagramd@%s\n' "$RELEASE_APP_DIGEST" ;;
            ghcr.io/teagramhq/teagram-migrations:sha-*)
              printf 'ghcr.io/teagramhq/teagram-migrations@%s\n' "$RELEASE_MIGRATIONS_DIGEST" ;;
            ghcr.io/teagramhq/teagramd-mc-init:sha-*)
              printf 'ghcr.io/teagramhq/teagramd-mc-init@%s\n' "$RELEASE_HELPER_DIGEST" ;;
            ghcr.io/teagramhq/link-edge-landing:sha-*)
              printf 'ghcr.io/teagramhq/link-edge-landing@%s\n' "$RELEASE_LANDING_DIGEST" ;;
            ghcr.io/teagramhq/link-edge-selector:sha-*)
              printf 'ghcr.io/teagramhq/link-edge-selector@%s\n' "$RELEASE_SELECTOR_DIGEST" ;;
            *) return 91 ;;
          esac
        elif [[ $RELEASE_CASE == mismatch &&
          $image == "ghcr.io/teagramhq/teagramd:sha-$LIVE_REVISION" ]]; then
          printf '%s\n' "$MISMATCHED_REVISION"
        else
          printf '%s\n' "$LIVE_REVISION"
        fi
        ;;
      run) printf '20261105000021\n' ;;
      *) return 91 ;;
    esac
  }
  fence_source_writes() { printf 'fence reached\n'; }
  kubectl() {
    [[ $1 == kustomize && $2 == "$REPO_ROOT/deploy/k8s/prod-cutover" ]] || return 91
    cat "$fixture_dir/release-prod-cutover.yaml"
  }
  incus exec "$SOURCE_INSTANCE" -- docker inspect --format revision teagram-teagramd-1 >/dev/null
  kubectl kustomize "$REPO_ROOT/deploy/k8s/prod-cutover" >/dev/null
  if [[ $mode == missing-auth ]]; then
    require_release_auth
  fi
  validate_release_for_source || return $?
  if [[ $mode == matching ]]; then
    [[ -s $AUTH_CONFIG_LOG ]] || fail 'production provenance validation did not authenticate GHCR'
    while IFS= read -r auth_config_dir; do
      [[ ! -e $auth_config_dir ]] ||
        fail 'production provenance validation did not remove its temporary GHCR credentials'
    done < "$AUTH_CONFIG_LOG"
  fi
  if [[ $mode == mismatch || $mode == digest-mismatch ]]; then
    fence_source_writes
    return 0
  fi
  [[ $mode == matching ]] || return 0
  [[ $LIVE_SOURCE_REVISION == "$LIVE_REVISION" ]] ||
    fail 'production release validation lost the live source revision'
  [[ $APP_IMAGE == "ghcr.io/teagramhq/teagramd@$RELEASE_APP_DIGEST" &&
    $MIGRATIONS_IMAGE == "ghcr.io/teagramhq/teagram-migrations@$RELEASE_MIGRATIONS_DIGEST" &&
    $K8S_MC_IMAGE == "ghcr.io/teagramhq/teagramd-mc-init@$RELEASE_HELPER_DIGEST" &&
    $LINK_LANDING_IMAGE == "ghcr.io/teagramhq/link-edge-landing@$RELEASE_LANDING_DIGEST" &&
    $LINK_SELECTOR_IMAGE == "ghcr.io/teagramhq/link-edge-selector@$RELEASE_SELECTOR_DIGEST" ]] ||
    fail 'production release validation did not retain the published immutable digests'
  expected_migration_job="db-migrations-$(printf '%s' "$RELEASE_MIGRATIONS_DIGEST" | cut -c8-23)"
  expected_rustfs_job="rustfs-init-$(printf '%s' "$RELEASE_HELPER_DIGEST" | cut -c8-23)"
  [[ $MIGRATIONS_JOB_NAME == "$expected_migration_job" &&
    $RUSTFS_INIT_JOB_NAME == "$expected_rustfs_job" &&
    $REQUIRED_MIGRATION_VERSION == 20261105000021 ]] ||
    fail 'production release validation did not derive migration metadata from the live images'
  render_cutover_manifest "$rendered_manifest"
  for expected_image in "$APP_IMAGE" "$MIGRATIONS_IMAGE" "$K8S_MC_IMAGE" \
    "$LINK_LANDING_IMAGE" "$LINK_SELECTOR_IMAGE"; do
    grep --fixed-strings --quiet -- "image: $expected_image" "$rendered_manifest" ||
      fail "cutover render omitted published immutable image $expected_image"
  done
  for expected_job in "$MIGRATIONS_JOB_NAME" "$RUSTFS_INIT_JOB_NAME"; do
    EXPECTED_JOB_NAME=$expected_job yq --exit-status \
      'select(.kind == "Job" and .metadata.name == strenv(EXPECTED_JOB_NAME)) |
        (.metadata.name == strenv(EXPECTED_JOB_NAME) and
         .spec.selector.matchLabels."job-revision" == strenv(EXPECTED_JOB_NAME) and
         .spec.template.metadata.labels."job-revision" == strenv(EXPECTED_JOB_NAME))' \
      "$rendered_manifest" >/dev/null ||
      fail "cutover render omitted digest-derived Job name and labels $expected_job"
  done
)

if mismatch_output=$(run_production_release_case mismatch "$fixture_dir/release-mismatch.yaml" 2>&1); then
  printf 'production release validation accepted a published image with a mismatched revision\n' >&2
  exit 1
fi
for expected_revision in "$live_revision" "$mismatched_revision"; do
  if [[ $mismatch_output != *"$expected_revision"* ]]; then
    printf 'production release mismatch did not report both revisions: %s\n' "$mismatch_output" >&2
    exit 1
  fi
done
if [[ $mismatch_output != *'published image ghcr.io/teagramhq/teagramd:sha-'* ]]; then
  printf 'production release mismatch did not identify the rejected published image: %s\n' "$mismatch_output" >&2
  exit 1
fi
if [[ $mismatch_output == *'fence reached'* ]]; then
  printf 'production release mismatch reached the write fence: %s\n' "$mismatch_output" >&2
  exit 1
fi
if missing_auth_output=$(run_production_release_case missing-auth \
  "$fixture_dir/release-missing-auth.yaml" 2>&1); then
  printf 'production release validation proceeded without its required registry credentials\n' >&2
  exit 1
fi
if [[ $missing_auth_output != *'set GH_TOKEN and GHCR_USERNAME'* ||
  $missing_auth_output == *'fence reached'* ]]; then
  printf 'production release validation did not reject missing GHCR credentials before fencing: %s\n' \
    "$missing_auth_output" >&2
  exit 1
fi
if digest_mismatch_output=$(run_production_release_case digest-mismatch \
  "$fixture_dir/release-digest-mismatch.yaml" 2>&1); then
  printf 'production release validation accepted a digest without matching main CI provenance\n' >&2
  exit 1
fi
for expected_digest in "$test_release_app_digest" "$live_revision"; do
  if [[ $digest_mismatch_output != *"$expected_digest"* ]]; then
    printf 'production provenance mismatch did not report the digest and source revision: %s\n' \
      "$digest_mismatch_output" >&2
    exit 1
  fi
done
if [[ $digest_mismatch_output != *'no valid main CI attestation'* ||
  $digest_mismatch_output == *'fence reached'* ]]; then
  printf 'production digest mismatch did not fail before fencing: %s\n' \
    "$digest_mismatch_output" >&2
  exit 1
fi
run_production_release_case matching "$fixture_dir/release-rendered.yaml"
# Exercise the runner's actual validator with realistic CLI output and unsafe
# variants, independently of the disposable-cluster integration test.
eval "$(sed -n '/^check_serve_status() {/,/^set_serve_routes() {/p' deploy/k8s/cutover-alpha.sh | sed '$d')"
eval "$(sed -n '/^capture_compose_environment() {/,/^install_admin_secret() {/p' deploy/k8s/cutover-alpha.sh | sed '$d')"
fixture_source_env=$'TG_BLOB_S3_ENDPOINT=http://rustfs:9000\nTG_BLOB_S3_BUCKET=teagram\nTG_BLOB_S3_PREFIX=teagramd/\nTG_BLOB_S3_ACCESS_KEY_ID=0123456789abcdef0123\nTG_BLOB_S3_SECRET_ACCESS_KEY_FILE=/run/secrets/teagramd-blob-secret-key\nTG_MEDIA_KEY_CURRENT=1'
capture_compose_environment() { printf '%s\n' "$fixture_source_env"; }
fail() { printf '%s\n' "$*" >&2; return 1; }
validate_source_blob_backend || {
  printf 'cutover rejected the encrypted S3 Compose blob source\n' >&2
  exit 1
}
fixture_source_env=$'TG_BLOB_S3_ENDPOINT=http://other:9000\nTG_BLOB_S3_BUCKET=teagram\nTG_BLOB_S3_PREFIX=teagramd/\nTG_BLOB_S3_ACCESS_KEY_ID=0123456789abcdef0123\nTG_BLOB_S3_SECRET_ACCESS_KEY_FILE=/run/secrets/teagramd-blob-secret-key\nTG_MEDIA_KEY_CURRENT=1'
if validate_source_blob_backend 2>/dev/null; then
  printf 'cutover accepted an unrecognized S3 endpoint\n' >&2
  exit 1
fi
fixture_source_env=$'TG_BLOB_S3_ENDPOINT=http://rustfs:9000\nTG_BLOB_S3_BUCKET=teagram\nTG_BLOB_S3_PREFIX=teagramd/\nTG_BLOB_S3_ACCESS_KEY_ID=not-a-valid-key\nTG_BLOB_S3_SECRET_ACCESS_KEY_FILE=/run/secrets/teagramd-blob-secret-key\nTG_MEDIA_KEY_CURRENT=1'
if validate_source_blob_backend 2>/dev/null; then
  printf 'cutover accepted an invalid source S3 access key\n' >&2
  exit 1
fi
python3 - "$fixture_dir" <<'PY'
import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
routes = {
    "/": "http://127.0.0.1:8081",
    "/apiws": "http://127.0.0.1:2444/apiws",
    "/admin": "http://127.0.0.1:2445/admin",
    "/.well-known/teagramd/client": "http://127.0.0.1:2444/.well-known/teagramd/client",
    "/.well-known/telegramd/client": "http://127.0.0.1:2444/.well-known/telegramd/client",
}
base = {"TCP": {"443": {"HTTPS": True}, "2443": {"TCPForward": "127.0.0.1:2443"},
                "2447": {"TCPForward": "127.0.0.1:2447", "ProxyProtocol": 1}},
        "Web": {"alpha.example.ts.net:443": {"Handlers": {k: {"Proxy": v} for k, v in routes.items()}}}}
def write(name, value):
    (root / name).write_text(json.dumps(value))
write("serve-valid.json", base)
write("serve-funnel-off.json", {**base, "AllowFunnel": {"alpha.example.ts.net:443": False}})
write("serve-funnel.json", {**base, "AllowFunnel": {"alpha.example.ts.net:443": True}})
write("serve-invalid-funnel.json", {**base, "AllowFunnel": None})
write("serve-bad-https.json", {**base, "TCP": {**base["TCP"], "443": {"HTTPS": False}}})
write("serve-extra-port.json", {**base, "TCP": {**base["TCP"], "444": {"TCPForward": "127.0.0.1:1"}}})
write("serve-missing-bot-api.json", {**base, "TCP": {k: v for k, v in base["TCP"].items() if k != "2447"}})
write("serve-bad-bot-api-proxy-protocol.json", {**base, "TCP": {**base["TCP"], "2447": {"TCPForward": "127.0.0.1:2447"}}})
PY
serve_args=(http://127.0.0.1:8081 http://127.0.0.1:2444 http://127.0.0.1:2445 \
  127.0.0.1:2443 127.0.0.1:2447)
check_serve_status "$fixture_dir/serve-valid.json" "${serve_args[@]}"
check_serve_status "$fixture_dir/serve-funnel-off.json" "${serve_args[@]}"
for invalid in serve-funnel serve-invalid-funnel serve-bad-https serve-extra-port \
  serve-missing-bot-api serve-bad-bot-api-proxy-protocol; do
  if check_serve_status "$fixture_dir/$invalid.json" "${serve_args[@]}" 2>/dev/null; then
    printf 'cutover accepted unsafe Serve state: %s\n' "$invalid" >&2
    exit 1
  fi
done
cat > "$fixture_dir/bin/kubectl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[[ $* == 'get deployment/cloudflared --namespace teagram -o jsonpath={.spec.replicas}' ]]
printf '1\n'
SH
chmod 0700 "$fixture_dir/bin/kubectl"
if ! env "INCUS_STATE_DIR=$fixture_dir/state" \
  bash deploy/k8s/test-cutover-alpha-incus.sh exec teagram-k3s -- systemctl daemon-reload; then
  printf 'cutover fixture could not reload systemd without a unit name\n' >&2
  exit 1
fi
fixture=(env "PATH=$fixture_dir/bin:$PATH" "RUNNER_TEMP=$fixture_dir" \
  "INCUS_STATE_DIR=$fixture_dir/state" CUTOVER_FAIL_OLD_CONNECTOR_CHECK=true \
  bash deploy/k8s/test-cutover-alpha-incus.sh exec teagram -- \
  docker inspect --format '{{.State.Status}}' teagram-web-cloudflared)

assert_connector_running() {
  local output
  if ! output=$("${fixture[@]}" 2>"$fixture_dir/error") ||
    [[ $output != running || -s $fixture_dir/error ]]; then
    cat "$fixture_dir/error" >&2
    printf 'cutover fixture did not report the running source connector\n' >&2
    exit 1
  fi
}

assert_connector_running
printf 'fenced\n' > "$fixture_dir/teagram-cutover-runner/phase"
assert_connector_running
printf 'route-switched\n' > "$fixture_dir/teagram-cutover-runner/phase"
status=0
"${fixture[@]}" > "$fixture_dir/output" 2> "$fixture_dir/error" || status=$?
if [[ $status -ne 42 ]] || ! grep --fixed-strings --quiet \
  'injected disposable-cluster failure after the Cloudflare connector started' "$fixture_dir/error"; then
  printf 'cutover fixture did not inject the post-switch connector failure\n' >&2
  exit 1
fi
assert_connector_running

printf 'alpha cutover dry-run contract passed\n'
