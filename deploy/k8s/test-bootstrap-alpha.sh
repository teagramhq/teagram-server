#!/usr/bin/env bash
set -euo pipefail

# Runs bootstrap-alpha.sh --apply twice against fake incus and tailscale. The
# source teagram LXC is absent in the fixture. The script needs root and writes
# /root, so run this as root or in a throwaway container.
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
admin_cert='tgtest-k3s-admin-client-cert'
admin_key='tgtest-k3s-admin-client-key'
deployer_token='tgtest-deployer-token-7f3a9c'
host_tmp="$work/host-tmp"
mkdir -p "$host_tmp"
export TMPDIR="$host_tmp"
export MOCK_HOST_TMP="$host_tmp"
export MOCK_SECRET_VALUES
MOCK_SECRET_VALUES=$(printf '%s\n' "$deployer_token" "$admin_cert" "$admin_key" \
  "$(printf '%s' "$admin_cert" | base64 -w0)" "$(printf '%s' "$admin_key" | base64 -w0)")

authorized_keys="$work/alpha-authorized_keys"
printf '%s\n' \
  'command="/usr/local/sbin/teagram-ops",restrict ssh-ed25519 AAAATESTOPSKEY multica-daemon@delta' \
  'ssh-ed25519 AAAATESTOTHERKEY ops@other' > "$authorized_keys"

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

# The fake API server answers `auth can-i` from the Role the bootstrap applies,
# so the bootstrap's boundary checks test deployer-access.yaml itself.
command -v yq >/dev/null || fail 'yq is required to read the deployer Role'
deployer_rules="$work/deployer-rules.json"
yq -o=json 'select(.kind == "Role" and .metadata.name == "teagram-deployer") | .rules' \
  "$repo_root/deploy/k8s/deployer-access.yaml" > "$deployer_rules"
python3 -c 'import json, sys; assert json.load(open(sys.argv[1]))' "$deployer_rules" ||
  fail 'the deployer Role has no rules'
export MOCK_DEPLOYER_RULES="$deployer_rules"

export MOCK_STATE="$work/state"
export MOCK_ACTIONS="$work/actions.log"
export TEAGRAM_K3S_AUTHORIZED_KEYS_PATH="$authorized_keys"

run_bootstrap() {
  local output=$1 actions=$2
  : > "$MOCK_ACTIONS"
  if ! PATH="$bin:$PATH" "$bootstrap" --apply > "$output" 2>&1; then
    cat "$output" >&2
    fail "bootstrap --apply exited non-zero ($output)"
  fi
  cp "$MOCK_ACTIONS" "$actions"
}

# First apply: builds the cluster LXC and writes separate kubeconfigs.
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
workload_resources = {
    "configmap/teagramd-runtime", "configmap/link-edge-runtime",
    "secret/postgres-credentials", "secret/rustfs-credentials",
    "secret/teagramd-blob-credentials", "secret/cloudflared-token",
    "secret/teagramd-media-keys",
}
if workload_resources & state["objects"].keys():
    raise SystemExit("bootstrap must leave workload configuration and secrets to external management")
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
if "pod/teagram-deployer-escape-check" in state["objects"]:
    raise SystemExit("the server-side escape probe created a pod")
PY

cp "$kubeconfig_path" "$work/kubeconfig.before"
cp "$cutover_kubeconfig_path" "$work/cutover-kubeconfig.before"

# Second apply: the source LXC and local key file are both unavailable. It
# must use the existing target key, change nothing, and still exit 0.
rm -f "$authorized_keys"
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
for secret in "$deployer_token" \
  "$admin_cert" "$admin_key" "$(printf '%s' "$admin_cert" | base64 -w0)" \
  "$(printf '%s' "$admin_key" | base64 -w0)"; do
  if grep -Fq -- "$secret" "$work/run1.out" "$work/run2.out"; then
    fail 'the script printed a secret value'
  fi
done

# Failure-path fixture is a fresh install, so restore the alpha-side key input.
printf '%s\n' \
  'command="/usr/local/sbin/teagram-ops",restrict ssh-ed25519 AAAATESTOPSKEY multica-daemon@delta' \
  'ssh-ed25519 AAAATESTOTHERKEY ops@other' > "$authorized_keys"
mkdir -p "$work/failure-state"
: > "$work/failure-actions"
if MOCK_STATE="$work/failure-state" MOCK_ACTIONS="$work/failure-actions" \
  MOCK_FAIL_APPLY_RESOURCE=namespace/teagram PATH="$bin:$PATH" \
  "$bootstrap" --apply > "$work/failure.out" 2>&1; then
  fail 'the injected kubectl apply failure was ignored'
fi
grep -Fq 'k3s could not apply namespace/teagram (exit 1): mock kubectl first diagnostic' \
  "$work/failure.out" || fail 'kubectl apply failure did not name the resource and first error line'
if grep -Fq 'mock kubectl second diagnostic' "$work/failure.out"; then
  fail 'kubectl apply failure printed more than the first stderr line'
fi
for secret in "$deployer_token" \
  "$admin_cert" "$admin_key" "$(printf '%s' "$admin_cert" | base64 -w0)" \
  "$(printf '%s' "$admin_key" | base64 -w0)"; do
  if grep -Fq -- "$secret" "$work/failure.out"; then
    fail 'the script printed a secret value on an apply failure'
  fi
done

# A Role missing a grant the deploy needs must fail the boundary check.
python3 - "$deployer_rules" "$work/narrow-rules.json" <<'PY'
import json
import sys

rules = json.load(open(sys.argv[1]))
narrowed = [rule for rule in rules if "cronjobs" not in rule.get("resources", [])]
if len(narrowed) == len(rules):
    raise SystemExit("the deployer Role grants no cronjobs access to remove")
json.dump(narrowed, open(sys.argv[2], "w"))
PY
mkdir -p "$work/narrow-state"
if MOCK_STATE="$work/narrow-state" MOCK_ACTIONS="$work/narrow-actions" \
  MOCK_DEPLOYER_RULES="$work/narrow-rules.json" PATH="$bin:$PATH" \
  "$bootstrap" --apply > "$work/narrow.out" 2>&1; then
  fail 'a deployer Role without cronjobs access passed the boundary check'
fi
grep -Fq 'the deploy ServiceAccount RBAC boundary is incorrect' "$work/narrow.out" ||
  fail 'a narrowed deployer Role failed for a reason other than the boundary check'

printf 'bootstrap-alpha apply test passed: first apply, no-op rerun, no secret output\n'
