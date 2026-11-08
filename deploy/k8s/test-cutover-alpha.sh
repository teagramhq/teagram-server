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
  'Cloudflare dashboard change http://selector:8081 -> http://linkselector:8081' \
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

for required_runbook in \
  'must stay online because the post-fence source S3 listing' \
  'bucket remains the pre-switch recovery copy for at least seven days' \
  'PostgreSQL and source RustFS were already running'; do
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

prod_migrations_digest=$(yq -r '.images[] | select(.name == "ghcr.io/teagramhq/teagram-migrations") | .digest' \
  deploy/k8s/prod/kustomization.yaml)
prod_helper_digest=$(yq -r '.images[] | select(.name == "ghcr.io/teagramhq/teagramd-mc-init") | .digest' \
  deploy/k8s/prod/kustomization.yaml)
cutover_migrations_digest=$(yq -r '.images[] | select(.name == "ghcr.io/teagramhq/teagram-migrations") | .digest' \
  deploy/k8s/prod-cutover/kustomization.yaml)
[[ $cutover_migrations_digest == null ]] && cutover_migrations_digest=''
if [[ ! $prod_migrations_digest =~ ^sha256:[0-9a-f]{64}$ ||
  ! $prod_helper_digest =~ ^sha256:[0-9a-f]{64}$ ||
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
NAMESPACE=teagram-cutover
APP_IMAGE=ghcr.io/teagramhq/teagramd@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
MIGRATIONS_IMAGE=ghcr.io/teagramhq/teagram-migrations@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
K8S_MC_IMAGE=ghcr.io/teagramhq/teagramd-mc-init@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
LINK_LANDING_IMAGE=ghcr.io/teagramhq/link-edge-landing@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
LINK_SELECTOR_IMAGE=ghcr.io/teagramhq/link-edge-selector@sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
MIGRATIONS_JOB_NAME=$live_migration_job
RUSTFS_INIT_JOB_NAME=$live_rustfs_job
REQUIRED_MIGRATION_VERSION=20261105000021
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
