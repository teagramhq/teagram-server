#!/usr/bin/env bash
set -euo pipefail

# Runs bootstrap-alpha.sh --apply twice against fake incus and tailscale. The
# script needs root and writes /root, so run this as root or in a throwaway
# container. Only the host and LXC control commands are faked.
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
bootstrap="$repo_root/deploy/k8s/bootstrap-alpha.sh"
mock="$repo_root/deploy/k8s/test-bootstrap-alpha-mock.py"
kubeconfig_path=/root/teagram-k3s-deploy.kubeconfig
cutover_kubeconfig_path=/root/teagram-k3s-cutover.kubeconfig

fail() {
  printf 'bootstrap-alpha test: %s\n' "$*" >&2
  exit 1
}

[[ $(id -u) -eq 0 ]] || fail 'run as root (the bootstrap writes under /root)'
for path in "$kubeconfig_path" "$cutover_kubeconfig_path"; do
  [[ ! -e $path && ! -L $path ]] ||
    fail "$path exists; move it aside before running this test"
done

dry_run_output=$("$bootstrap" --dry-run)
grep -Fq 'ServiceAccount teagram-agent with token automount disabled' <<< "$dry_run_output" ||
  fail 'bootstrap dry-run did not show the dedicated agent ServiceAccount'
grep -Fq 'cluster-scoped ValidatingAdmissionPolicy teagram-agent-pod-delete' <<< "$dry_run_output" ||
  fail 'bootstrap dry-run did not show the cluster-scoped pod-delete policy'

work=$(mktemp -d)
cleanup() {
  rm -rf -- "$work" "$kubeconfig_path" "$cutover_kubeconfig_path"
}
trap cleanup EXIT

# Fixture values are fake. Every one must stay out of the script's output.
pg_password='tgtest-postgres-pw-91c2'
tunnel_token='tgtest-cloudflared-token-55'
rustfs_access_key=$(printf 'rustfs-access-tgtest' | sha256sum | cut -c1-20)
rustfs_secret_key="$(printf 'e1%.0s' {1..32})"
blob_access_key_id='2b3c4d5e6f708192a3b4'
blob_secret_key="$(printf 'c3%.0s' {1..32})"
media_key='tgtest-media-key-body-1'
admin_cert='tgtest-k3s-admin-client-cert'
admin_key='tgtest-k3s-admin-client-key'
deployer_token='tgtest-deployer-token-7f3a9c'
dsn="postgres://postgres:${pg_password}@postgres:5432/teagram?sslmode=disable"
host_tmp="$work/host-tmp"
mkdir -p "$host_tmp"
export TMPDIR="$host_tmp"
export MOCK_HOST_TMP="$host_tmp"
export MOCK_SECRET_VALUES
MOCK_SECRET_VALUES=$(printf '%s\n' "$pg_password" "$dsn" "$tunnel_token" \
  "$rustfs_access_key" "$rustfs_secret_key" "$blob_access_key_id" "$blob_secret_key" \
  "$media_key" "$deployer_token" "$admin_cert" "$admin_key" \
  "$(printf '%s' "$admin_cert" | base64 -w0)" "$(printf '%s' "$admin_key" | base64 -w0)")

src="$work/src"
containers="$src/containers"
mkdir -p "$src/root/.ssh" "$containers"
printf '%s\n' \
  'command="/usr/local/sbin/teagram-ops",restrict ssh-ed25519 AAAATESTOPSKEY multica-daemon@delta' \
  'ssh-ed25519 AAAATESTOTHERKEY ops@other' > "$src/root/.ssh/authorized_keys"
for name in teagram-postgres-1 teagram-teagramd-1 teagram-rustfs-1 \
  teagram-link-edge-selector-1 teagram-web-cloudflared; do
  mkdir -p "$containers/$name"
done
printf '%s\n' "POSTGRES_PASSWORD=$pg_password" > "$containers/teagram-postgres-1/env"
printf '%s\n' \
  "TG_POSTGRES_DSN=$dsn" \
  'TG_PUBLIC_LINK_PREFIX=https://tgtest.example.invalid/l/' \
  'TG_ADVERTISE_ADDR=tgtest.example.invalid:443' \
  'TG_MEDIA_KEY_CURRENT=1' \
  "TG_BLOB_S3_ACCESS_KEY_ID=$blob_access_key_id" > "$containers/teagram-teagramd-1/env"
printf '%s\n' \
  'TG_INVITE_WEB_SIGNUP_URL=https://web.tgtest.example.invalid/signup' \
  'TG_INVITE_DESKTOP_URL=https://desktop.tgtest.example.invalid/invite' \
  > "$containers/teagram-link-edge-selector-1/env"
printf '%s\n' "TUNNEL_TOKEN=$tunnel_token" > "$containers/teagram-web-cloudflared/env"
mkdir -p "$containers/teagram-rustfs-1/run/secrets" \
  "$containers/teagram-teagramd-1/run/secrets" \
  "$containers/teagram-teagramd-1/run/teagramd/media-keys"
printf '%s' "$rustfs_access_key" > "$containers/teagram-rustfs-1/run/secrets/rustfs-root-access-key"
printf '%s' "$rustfs_secret_key" > "$containers/teagram-rustfs-1/run/secrets/rustfs-root-secret-key"
printf '%s' "$blob_secret_key" > "$containers/teagram-teagramd-1/run/secrets/teagramd-blob-secret-key"
printf '%s' "$media_key" > "$containers/teagram-teagramd-1/run/teagramd/media-keys/1.key"
printf '%s' 'tgtest-media-key-body-2' > "$containers/teagram-teagramd-1/run/teagramd/media-keys/2.key"

bin="$work/bin"
mkdir -p "$bin" "$work/state"
cat > "$bin/incus" <<EOF
#!/usr/bin/env bash
exec python3 "$mock" incus "\$@"
EOF
cat > "$bin/tailscale" <<EOF
#!/usr/bin/env bash
exec python3 "$mock" tailscale "\$@"
EOF
cat > "$bin/hostname" <<'EOF'
#!/usr/bin/env bash
printf 'alpha\n'
EOF
cat > "$bin/k3s" <<EOF
#!/usr/bin/env bash
exec python3 "$mock" k3s "\$@"
EOF
chmod 0755 "$bin/incus" "$bin/tailscale" "$bin/hostname" "$bin/k3s"

export MOCK_STATE="$work/state"
export MOCK_SRC="$src"
export MOCK_ACTIONS="$work/actions.log"

run_bootstrap() {
  local output=$1 actions=$2
  : > "$MOCK_ACTIONS"
  if ! PATH="$bin:$PATH" "$bootstrap" --apply > "$output" 2>&1; then
    cat "$output" >&2
    fail "bootstrap --apply exited non-zero ($output)"
  fi
  cp "$MOCK_ACTIONS" "$actions"
}

# First apply: builds the LXC and writes separate deploy and cutover kubeconfigs.
run_bootstrap "$work/run1.out" "$work/run1.actions"
grep -Fq 'k3s bootstrap complete' "$work/run1.out" || fail 'first run did not report completion'
grep -Fq "Warning: resource 'namespaces' is not namespace scoped" "$work/run1.out" ||
  fail 'cluster-scoped can-i warning was not exercised'
[[ $(stat -c '%a:%u' "$kubeconfig_path") == 600:0 ]] || fail 'kubeconfig is not a root-only file'
grep -Fq "$deployer_token" "$kubeconfig_path" || fail 'kubeconfig does not contain the deploy token'
[[ $(stat -c '%a:%u' "$cutover_kubeconfig_path") == 600:0 ]] ||
  fail 'cutover kubeconfig is not a root-only file'
python3 - "$cutover_kubeconfig_path" <<'PY'
import json
import pathlib
import sys

config = json.loads(pathlib.Path(sys.argv[1]).read_text())
assert config["clusters"][0]["cluster"]["server"] == "https://100.100.1.2:6443"
user = config["users"][0]["user"]
assert user["client-certificate-data"]
assert user["client-key-data"]
assert "token" not in user
assert config["contexts"][0]["context"]["namespace"] == "teagram"
PY
if grep -Fq "$deployer_token" "$cutover_kubeconfig_path"; then
  fail 'cutover kubeconfig reused the restricted deployer token'
fi
grep -Fxq 'k3s-install' "$work/run1.actions" || fail 'first run did not install k3s'
grep -Fxq 'kubeconfig-admin-view' "$work/run1.actions" ||
  fail 'bootstrap did not obtain the separate k3s admin kubeconfig'
grep -Fxq 'podsecurity-deny' "$work/run1.actions" || fail 'the escape pod was not rejected by admission'
python3 - "$MOCK_STATE/state.json" <<'PY'
import json
import sys

with open(sys.argv[1]) as handle:
    state = json.load(handle)
if state["pod_security_enforce"] != "baseline":
    raise SystemExit("namespace Pod Security baseline was not enabled")
if state["pod_security_enforce_version"] != "v1.37":
    raise SystemExit("namespace Pod Security baseline version was not pinned")
if state["token_wait_polls"] < 3 or state["ca_wait_polls"] < 3:
    raise SystemExit("the token and CA waits did not cover delayed population")
required_data = {
    "configmap/teagramd-runtime": {"TG_PUBLIC_LINK_PREFIX", "TG_ADVERTISE_ADDR", "TG_MEDIA_KEY_CURRENT"},
    "configmap/link-edge-runtime": {"TG_INVITE_WEB_SIGNUP_URL", "TG_INVITE_DESKTOP_URL"},
    "secret/postgres-credentials": {"POSTGRES_PASSWORD", "TG_POSTGRES_DSN"},
    "secret/rustfs-credentials": {"RUSTFS_ACCESS_KEY", "RUSTFS_SECRET_KEY"},
    "secret/teagramd-blob-credentials": {"TG_BLOB_S3_ACCESS_KEY_ID", "TG_BLOB_S3_SECRET_ACCESS_KEY"},
    "secret/cloudflared-token": {"token"},
    "secret/teagramd-media-keys": {"1.key", "2.key"},
}
missing_resources = required_data.keys() - state["objects"].keys()
if missing_resources:
    raise SystemExit(f"bootstrap did not apply expected resources: {sorted(missing_resources)}")
if "secret/teagram-deployer-token" not in state["objects"]:
    raise SystemExit("bootstrap did not apply the deployer token Secret")
required_agent_objects = {
    "serviceaccount/teagram-agent",
    "role/teagram-agent",
    "rolebinding/teagram-agent",
    "validatingadmissionpolicy/teagram-agent-pod-delete",
    "validatingadmissionpolicybinding/teagram-agent-pod-delete",
}
missing_agent_objects = required_agent_objects - state["objects"].keys()
if missing_agent_objects:
    raise SystemExit(f"bootstrap did not apply agent access resources: {sorted(missing_agent_objects)}")
if "secret/teagram-agent-token" in state["objects"]:
    raise SystemExit("bootstrap must not create an agent token Secret")
if "secret/ghcr-pull" in state["objects"]:
    raise SystemExit("bootstrap must not create a GHCR pull Secret for public images")
for resource, expected_keys in required_data.items():
    actual_keys = set(state.get("resource_data", {}).get(resource, {}))
    if actual_keys != expected_keys:
        raise SystemExit(f"{resource} data keys differ: {sorted(actual_keys)}")
if "pod/teagram-deployer-escape-check" in state["objects"]:
    raise SystemExit("the server-side escape probe created a pod")
PY

cp "$kubeconfig_path" "$work/kubeconfig.before"
cp "$cutover_kubeconfig_path" "$work/cutover-kubeconfig.before"

# Second apply: must change nothing and still exit 0.
run_bootstrap "$work/run2.out" "$work/run2.actions"
grep -Fq 'k3s bootstrap complete' "$work/run2.out" || fail 'rerun did not report completion'
if grep -E '^(init|device-add|device-override|start|serve |k3s-install|sysctl|systemctl|file-write |kubectl-apply )' \
  "$work/run2.actions"; then
  fail 'rerun performed a mutating action'
fi
grep -Fxq 'k3s-version-check' "$work/run2.actions" || fail 'rerun skipped the installed k3s version check'
cmp -s "$work/kubeconfig.before" "$kubeconfig_path" || fail 'rerun changed the deploy kubeconfig'
cmp -s "$work/cutover-kubeconfig.before" "$cutover_kubeconfig_path" ||
  fail 'rerun changed the cutover kubeconfig'

# No fixture secret may appear in what the script prints, on either run.
for secret in "$pg_password" "$dsn" "$tunnel_token" "$rustfs_access_key" "$rustfs_secret_key" \
  "$blob_access_key_id" "$blob_secret_key" "$media_key" "$deployer_token" \
  "$admin_cert" "$admin_key" "$(printf '%s' "$admin_cert" | base64 -w0)" \
  "$(printf '%s' "$admin_key" | base64 -w0)"; do
  if grep -Fq -- "$secret" "$work/run1.out" "$work/run2.out"; then
    fail 'the script printed a secret value'
  fi
done

mkdir -p "$work/failure-state"
: > "$work/failure-actions"
if MOCK_STATE="$work/failure-state" MOCK_ACTIONS="$work/failure-actions" \
  MOCK_FAIL_APPLY_RESOURCE=secret/postgres-credentials PATH="$bin:$PATH" \
  "$bootstrap" --apply > "$work/failure.out" 2>&1; then
  fail 'the injected kubectl apply failure was ignored'
fi
grep -Fq 'k3s could not apply secret/postgres-credentials (exit 1): mock kubectl first diagnostic' \
  "$work/failure.out" || fail 'kubectl apply failure did not name the resource and first error line'
if grep -Fq 'mock kubectl second diagnostic' "$work/failure.out"; then
  fail 'kubectl apply failure printed more than the first stderr line'
fi
for secret in "$pg_password" "$dsn" "$tunnel_token" "$rustfs_access_key" "$rustfs_secret_key" \
  "$blob_access_key_id" "$blob_secret_key" "$media_key" "$deployer_token" \
  "$admin_cert" "$admin_key" "$(printf '%s' "$admin_cert" | base64 -w0)" \
  "$(printf '%s' "$admin_key" | base64 -w0)"; do
  if grep -Fq -- "$secret" "$work/failure.out"; then
    fail 'the script printed a secret value on an apply failure'
  fi
done

mkdir -p "$work/create-failure-state"
: > "$work/create-failure-actions"
if MOCK_STATE="$work/create-failure-state" MOCK_ACTIONS="$work/create-failure-actions" \
  MOCK_FAIL_CREATE_RESOURCE=secret/postgres-credentials PATH="$bin:$PATH" \
  "$bootstrap" --apply > "$work/create-failure.out" 2>&1; then
  fail 'the injected kubectl create failure was ignored'
fi
grep -Fq 'k3s could not create secret/postgres-credentials (exit 1): mock kubectl create first diagnostic' \
  "$work/create-failure.out" || fail 'kubectl create failure did not name the resource and first error line'
if grep -Eq 'could not apply|no objects passed to apply|mock kubectl create second diagnostic' \
  "$work/create-failure.out"; then
  fail 'kubectl create failure was replaced by an apply error or printed more than the first stderr line'
fi
if [[ $(grep -Fc 'mock kubectl create first diagnostic' "$work/create-failure.out") -ne 1 ]]; then
  fail 'kubectl create failure printed its stderr outside the named error'
fi
# Namespace, RBAC, and the two ConfigMaps are applied before the first Secret create.
if [[ $(grep -Fc 'kubectl-apply-attempt' "$work/create-failure-actions") -ne 6 ]]; then
  fail 'kubectl apply did not stop at the failed create'
fi
for secret in "$pg_password" "$dsn" "$tunnel_token" "$rustfs_access_key" "$rustfs_secret_key" \
  "$blob_access_key_id" "$blob_secret_key" "$media_key" "$deployer_token"; do
  if grep -Fq -- "$secret" "$work/create-failure.out"; then
    fail 'the script printed a secret value on a create failure'
  fi
done

printf 'bootstrap-alpha apply test passed: first apply, no-op rerun, no secret output\n'
