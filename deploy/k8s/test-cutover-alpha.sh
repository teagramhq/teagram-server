#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root"

plan="$(deploy/k8s/cutover-alpha.sh --dry-run)"
for required in \
  'root-only cutover kubeconfig' \
  'Would server-dry-run and apply deploy/k8s/prod-cutover' \
  'TG_BLOB_DIR=/var/lib/teagramd-blobs with TG_BLOB_S3_ENDPOINT unset' \
  'fence writes before the final PostgreSQL snapshot' \
  'stop all running Compose services except PostgreSQL' \
  'verify no PostgreSQL client connections remain' \
  'save a mode-0600 pg_dump' \
  'sent over a temporary loopback port-forward' \
  '8 GiB PostgreSQL PVC can hold both the protected dump and restored database' \
  'server_key.pem, enc_key.hex, and local blobs' \
  'teagramd-admin Secret' \
  'importer pod' \
  '/var/lib/teagram-k3s-cutover' \
  'encrypt and checksum-copy its objects to RustFS' \
  'all five tailnet-only Serve handlers' \
  'planned downtime from the write fence until all edges switch' \
  'Cloudflare dashboard change http://selector:8081 -> http://linkselector:8081' \
  'verify every k8s cloudflared pod has stopped before restoring routes' \
  'advertised MTProto TCP route on port 2443' \
  'explicit rollback after the first live Serve route' \
  'k3s writes since the switch are not carried back'; do
  if [[ $plan != *"$required"* ]]; then
    printf 'alpha cutover dry-run omitted: %s\n' "$required" >&2
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

cutover_image='ghcr.io/teagramhq/teagramd@sha256:e307d5d93ab000d61dc77251a599b00e031e6dac7cdf026ef8d2d621734e7c3f'
cutover_digest=${cutover_image#*@}
if ! grep --fixed-strings --quiet "digest: $cutover_digest" deploy/k8s/prod-cutover/kustomization.yaml; then
  printf 'alpha cutover does not pin the CI-verified blob-migration image\n' >&2
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
# Exercise the runner's actual validator with realistic CLI output and unsafe
# variants, independently of the disposable-cluster integration test.
eval "$(sed -n '/^check_serve_status() {/,/^set_serve_routes() {/p' deploy/k8s/cutover-alpha.sh | sed '$d')"
eval "$(sed -n '/^capture_compose_environment() {/,/^install_admin_secret() {/p' deploy/k8s/cutover-alpha.sh | sed '$d')"
fixture_source_env='TG_BLOB_DIR=/var/lib/teagramd-blobs'
capture_compose_environment() { printf '%s\n' "$fixture_source_env"; }
fail() { printf '%s\n' "$*" >&2; return 1; }
validate_source_blob_backend || {
  printf 'cutover rejected the local Compose blob source\n' >&2
  exit 1
}
fixture_source_env+=$'\nTG_BLOB_S3_ENDPOINT='
if validate_source_blob_backend 2>/dev/null; then
  printf 'cutover accepted a configured S3 Compose blob source\n' >&2
  exit 1
fi
fixture_source_env='TG_BLOB_DIR=/mnt/teagram-blobs'
if validate_source_blob_backend 2>/dev/null; then
  printf 'cutover accepted a nonstandard Compose blob directory\n' >&2
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
base = {"TCP": {"443": {"HTTPS": True}, "2443": {"TCPForward": "127.0.0.1:2443"}},
        "Web": {"alpha.example.ts.net:443": {"Handlers": {k: {"Proxy": v} for k, v in routes.items()}}}}
def write(name, value):
    (root / name).write_text(json.dumps(value))
write("serve-valid.json", base)
write("serve-funnel-off.json", {**base, "AllowFunnel": {"alpha.example.ts.net:443": False}})
write("serve-funnel.json", {**base, "AllowFunnel": {"alpha.example.ts.net:443": True}})
write("serve-invalid-funnel.json", {**base, "AllowFunnel": None})
write("serve-bad-https.json", {**base, "TCP": {**base["TCP"], "443": {"HTTPS": False}}})
write("serve-extra-port.json", {**base, "TCP": {**base["TCP"], "444": {"TCPForward": "127.0.0.1:1"}}})
PY
serve_args=(http://127.0.0.1:8081 http://127.0.0.1:2444 http://127.0.0.1:2445 127.0.0.1:2443)
check_serve_status "$fixture_dir/serve-valid.json" "${serve_args[@]}"
check_serve_status "$fixture_dir/serve-funnel-off.json" "${serve_args[@]}"
for invalid in serve-funnel serve-invalid-funnel serve-bad-https serve-extra-port; do
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
