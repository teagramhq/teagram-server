#!/usr/bin/env bash
set -euo pipefail

# Runs bootstrap-alpha.sh --apply against fake Incus and k3s. The
# source teagram LXC is absent in the fixture. The script needs root and writes
# /root, so run this as root or in a throwaway container.
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
bootstrap="$repo_root/deploy/k8s/bootstrap-alpha.sh"
mock="$repo_root/deploy/k8s/test-bootstrap-alpha-mock.py"
kubeconfig_path=/root/teagram-k3s-deploy.kubeconfig

fail() {
  printf 'bootstrap-alpha test: %s\n' "$*" >&2
  exit 1
}

[[ $(id -u) -eq 0 ]] || fail 'run as root (the bootstrap writes under /root)'
[[ ! -e $kubeconfig_path && ! -L $kubeconfig_path ]] ||
  fail "$kubeconfig_path exists; move it aside before running this test"
[[ ! -e /root/teagram-k3s-cutover.kubeconfig ]] ||
  fail 'bootstrap must not write a cluster-admin kubeconfig'

dry_run_output=$("$bootstrap" --dry-run)
grep -Fq 'ServiceAccount teagram-agent with token automount disabled' <<< "$dry_run_output" ||
  fail 'bootstrap dry-run did not show the dedicated agent ServiceAccount'
grep -Fq 'cluster-scoped ValidatingAdmissionPolicy teagram-agent-pod-delete' <<< "$dry_run_output" ||
  fail 'bootstrap dry-run did not show the cluster-scoped pod-delete policy'
grep -Fq -- '--tls-san=teagram.tailaa4918.ts.net' <<< "$dry_run_output" ||
  fail 'bootstrap dry-run did not show the tailnet API SAN'
grep -Fq 'rotates the dynamic listener certificate' <<< "$dry_run_output" ||
  fail 'bootstrap dry-run did not explain the dynamic listener certificate rotation'
grep -Fq 'verify that the API listener presents a certificate signed by the cluster server CA' <<< "$dry_run_output" ||
  fail 'bootstrap dry-run did not explain the CA-pinned endpoint verification'
grep -Fq 'restarts k3s only when that SAN is missing' <<< "$dry_run_output" ||
  fail 'bootstrap dry-run did not explain the idempotent API SAN update'
if grep -E 'Tailscale Serve|2222|16443|12222|authorized_keys|teagram-ops' <<< "$dry_run_output"; then
  fail 'bootstrap dry-run still describes the retired host Tailscale or SSH access'
fi

work=$(mktemp -d)
cleanup() { rm -rf -- "$work" "$kubeconfig_path"; }
trap cleanup EXIT

# Fixture values are fake. Every one must stay out of the script's output.
deployer_token='tgtest-deployer-token-7f3a9c'
host_tmp="$work/host-tmp"
mkdir -p "$host_tmp"
export TMPDIR="$host_tmp"
export MOCK_HOST_TMP="$host_tmp"
export MOCK_SECRET_VALUES
MOCK_SECRET_VALUES=$deployer_token

bin="$work/bin"
mkdir -p "$bin" "$work/state"
cat > "$bin/incus" <<EOF
#!/usr/bin/env bash
exec python3 "$mock" incus "\$@"
EOF
cat > "$bin/hostname" <<'EOF'
#!/usr/bin/env bash
printf 'alpha\n'
EOF
cat > "$bin/k3s" <<EOF
#!/usr/bin/env bash
exec python3 "$mock" k3s "\$@"
EOF
chmod 0755 "$bin/incus" "$bin/hostname" "$bin/k3s"

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

run_bootstrap() {
  local output=$1 actions=$2
  : > "$MOCK_ACTIONS"
  if ! PATH="$bin:$PATH" "$bootstrap" --apply > "$output" 2>&1; then
    cat "$output" >&2
    fail "bootstrap --apply exited non-zero ($output)"
  fi
  cp "$MOCK_ACTIONS" "$actions"
}

# First apply: builds the cluster LXC and writes only the restricted kubeconfig.
run_bootstrap "$work/run1.out" "$work/run1.actions"
grep -Fq 'k3s bootstrap complete' "$work/run1.out" || fail 'first run did not report completion'
grep -Fq "Warning: resource 'namespaces' is not namespace scoped" "$work/run1.out" ||
  fail 'cluster-scoped can-i warning was not exercised'
[[ $(stat -c '%a:%u' "$kubeconfig_path") == 600:0 ]] || fail 'kubeconfig is not a root-only file'
grep -Fq "$deployer_token" "$kubeconfig_path" || fail 'kubeconfig does not contain the deploy token'
python3 - "$kubeconfig_path" <<'PY'
import json
import pathlib
import sys

config = json.loads(pathlib.Path(sys.argv[1]).read_text())
cluster = config["clusters"][0]["cluster"]
assert cluster["server"] == "https://teagram.tailaa4918.ts.net:6443"
assert cluster["certificate-authority-data"]
assert "insecure-skip-tls-verify" not in cluster
user = config["users"][0]["user"]
assert user["token"]
assert config["contexts"][0]["context"]["namespace"] == "teagram"
PY
grep -Fxq 'k3s-install' "$work/run1.actions" || fail 'first run did not install k3s'
if grep -Fq 'kubeconfig-admin-view' "$work/run1.actions"; then
  fail 'bootstrap must not obtain a cluster-admin kubeconfig'
fi
grep -Fxq 'api-listener-tls-verify' "$work/run1.actions" ||
  fail 'first apply did not verify the tailnet API listener certificate'
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

# Simulate the existing cluster from MAIN-3278 before this change: its unit and
# deploy kubeconfig use alpha's old tailnet IPv4 endpoint.
python3 - "$MOCK_STATE/lxc/etc/systemd/system/k3s.service" "$kubeconfig_path" <<'PY'
import json
import pathlib
import sys

unit = pathlib.Path(sys.argv[1])
unit.write_text(unit.read_text().replace(
    "--tls-san=teagram.tailaa4918.ts.net", "--tls-san=100.100.1.2"))
state_path = unit.parents[4] / "state.json"
state = json.loads(state_path.read_text())
state["api_tls_san"] = False
state_path.write_text(json.dumps(state))
config_path = pathlib.Path(sys.argv[2])
config = json.loads(config_path.read_text())
config["clusters"][0]["cluster"]["server"] = "https://100.100.1.2:6443"
config_path.write_text(json.dumps(config, separators=(",", ":")) + "\n")
PY

# The migration retains the old SAN for rollback, rotates the dynamic API
# listener certificate, and rewrites the deploy kubeconfig to the cluster
# device's MagicDNS endpoint.
run_bootstrap "$work/run2.out" "$work/run2.actions"
grep -Fq 'k3s bootstrap complete' "$work/run2.out" || fail 'SAN migration did not report completion'
grep -Fxq 'k3s-install-skip-download' "$work/run2.actions" ||
  fail 'SAN migration did not update the unit without downloading k3s'
grep -Fxq 'certificate-rotate-k3s-server' "$work/run2.actions" ||
  fail 'SAN migration did not rotate the dynamic API listener certificate'
grep -Fxq 'systemctl stop k3s' "$work/run2.actions" ||
  fail 'SAN migration did not stop k3s for certificate rotation'
grep -Fxq 'systemctl start k3s' "$work/run2.actions" ||
  fail 'SAN migration did not start k3s after certificate rotation'
grep -Fq -- '--tls-san=100.100.1.2' \
  "$MOCK_STATE/lxc/etc/systemd/system/k3s.service.pre-main-3607" ||
  fail 'SAN migration did not preserve the old unit for rollback'
grep -Fq -- '--tls-san=100.100.1.2' \
  "$MOCK_STATE/lxc/etc/systemd/system/k3s.service" ||
  fail 'SAN migration did not retain the previous endpoint SAN'
grep -Fq -- '--tls-san=teagram.tailaa4918.ts.net' \
  "$MOCK_STATE/lxc/etc/systemd/system/k3s.service" ||
  fail 'SAN migration did not install the cluster device SAN'
[[ $(stat -c '%a:%u' "$kubeconfig_path") == 600:0 ]] ||
  fail 'migrated kubeconfig is not root-only'
python3 - "$kubeconfig_path" <<'PY'
import json
import pathlib
import sys

config = json.loads(pathlib.Path(sys.argv[1]).read_text())
cluster = config["clusters"][0]["cluster"]
assert cluster["server"] == "https://teagram.tailaa4918.ts.net:6443"
assert cluster["certificate-authority-data"]
assert "insecure-skip-tls-verify" not in cluster
PY

# A clean third run verifies the SAN, keeps the kubeconfig unchanged, and does
# not reinstall, rotate, or restart anything.
cp "$kubeconfig_path" "$work/kubeconfig.migrated"
run_bootstrap "$work/run3.out" "$work/run3.actions"
grep -Fq 'k3s bootstrap complete' "$work/run3.out" || fail 'rerun did not report completion'
if grep -E '^(init|device-add|device-override|start|serve |k3s-install|certificate-|sysctl|systemctl|file-write |kubectl-apply )' \
  "$work/run3.actions"; then
  fail 'clean rerun performed a mutating action'
fi
grep -Fxq 'k3s-version-check' "$work/run3.actions" || fail 'rerun skipped the installed k3s version check'
grep -Fxq 'api-listener-tls-verify' "$work/run3.actions" ||
  fail 'clean rerun did not verify the tailnet API listener certificate'
cmp -s "$work/kubeconfig.migrated" "$kubeconfig_path" || fail 'clean rerun changed the deploy kubeconfig'

# Repair an endpoint whose systemd unit already has the SAN but whose actual
# dynamic listener certificate does not.
python3 - "$MOCK_STATE/state.json" <<'PY'
import json
import pathlib
import sys

state_path = pathlib.Path(sys.argv[1])
state = json.loads(state_path.read_text())
state["api_tls_san"] = False
state_path.write_text(json.dumps(state))
PY
run_bootstrap "$work/run4.out" "$work/run4.actions"
grep -Fq 'k3s bootstrap complete' "$work/run4.out" || fail 'listener certificate recovery did not complete'
grep -Fxq 'certificate-rotate-k3s-server' "$work/run4.actions" ||
  fail 'a mismatched listener certificate was not rotated'
grep -Fxq 'api-listener-tls-verify' "$work/run4.actions" ||
  fail 'listener certificate recovery did not verify the replacement certificate'
if grep -Fxq 'k3s-install-skip-download' "$work/run4.actions"; then
  fail 'listener certificate recovery unnecessarily reinstalled k3s'
fi

# No fixture secret may appear in what the script prints, on either run.
if grep -Fq -- "$deployer_token" "$work/run1.out" "$work/run2.out" "$work/run3.out" "$work/run4.out"; then
  fail 'the script printed a secret value'
fi

# Failure-path fixture is a fresh install.
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
if grep -Fq -- "$deployer_token" "$work/failure.out"; then
  fail 'the script printed a secret value on an apply failure'
fi

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
