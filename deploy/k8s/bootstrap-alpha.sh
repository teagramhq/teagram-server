#!/usr/bin/env bash
set -euo pipefail
set -f
umask 077

readonly INSTANCE=teagram-k3s
readonly NAMESPACE=teagram
readonly K3S_VERSION=v1.37.1+k3s1
readonly IMAGE_ALIAS=images:ubuntu/24.04
readonly KUBECONFIG_PATH=/root/teagram-k3s-deploy.kubeconfig
readonly BOOTSTRAP_MARKER=MAIN-3278
readonly K3S_TAILNET_HOST=teagram.tailaa4918.ts.net
readonly K3S_API_TAILNET_PORT=6443
readonly K3S_SERVICE_BACKUP=/etc/systemd/system/k3s.service.pre-main-3607
readonly K3S_EXEC='server --disable=traefik --disable=servicelb --disable=metrics-server --tls-san=teagram.tailaa4918.ts.net --write-kubeconfig-mode=0600'

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -- "$SCRIPT_DIR/../.." && pwd)
# shellcheck disable=SC1091
source "$SCRIPT_DIR/alpha-incus.sh"

fail() {
  printf 'k3s bootstrap: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
Usage: bootstrap-alpha.sh --dry-run | --apply

  --dry-run  Print the complete host action plan without contacting the host.
  --apply    Run as root on alpha. Safe to rerun after a successful bootstrap.
EOF
}

dry_run() {
  cat <<'EOF'
Would verify root access on alpha and the managed k3s LXC. It does not read from the deleted application LXC or change alpha's Tailscale configuration.
Would create or resume Incus LXC teagram-k3s from Ubuntu 24.04 with 4 CPUs, 8 GiB RAM, nesting, overlay and br_netfilter modules, and the MAIN-3278 ownership marker.
Would set the instance root disk to 80 GiB and attach host /dev/kmsg to the LXC.
Would start the LXC, install ca-certificates, curl, iptables, nftables, and openssl as needed, and select only nft-backed iptables alternatives.
Would install /etc/sysctl.d/90-teagram-k3s.conf in the LXC and enable IP forwarding plus bridge netfilter.
Would install pinned k3s v1.37.1+k3s1 with Traefik, ServiceLB, and metrics-server disabled, and add --tls-san=teagram.tailaa4918.ts.net. A rerun backs up the previous service unit, rotates the dynamic listener certificate, and restarts k3s only when that SAN is missing.
Rollback for the SAN update: restore /etc/systemd/system/k3s.service.pre-main-3607, run systemctl daemon-reload, then systemctl restart k3s. The listener certificate may retain the added DNS SAN; it does not change the old endpoint's validity.
Would verify that the API listener presents a certificate signed by the cluster server CA for teagram.tailaa4918.ts.net before writing the deploy kubeconfig.
Would create the teagram namespace with enforced Pod Security baseline, ServiceAccount teagram-deployer, namespace-only Role and RoleBinding, and token Secret.
Would create ServiceAccount teagram-agent with token automount disabled and a namespace-only read Role and RoleBinding.
Would install the cluster-scoped ValidatingAdmissionPolicy teagram-agent-pod-delete and binding, restricting agent pod deletes to controller-owned teagramd, linkselector, linklanding, or cloudflared pods.
Would leave workload ConfigMaps and Secrets to the cluster's external secret and configuration management.
Would write /root/teagram-k3s-deploy.kubeconfig with the namespace-restricted deployer token, cluster CA, and server https://teagram.tailaa4918.ts.net:6443. It never writes a cluster-admin kubeconfig.
Would verify the k3s node, disabled components, namespace boundary, and deploy kubeconfig permissions.
Would verify Pod Security Admission rejects a privileged pod that mounts a hostPath.
EOF
}

if [[ $# -ne 1 ]]; then
  usage >&2
  exit 64
fi
case "$1" in
  --dry-run) dry_run; exit 0 ;;
  --apply) ;;
  --help|-h) usage; exit 0 ;;
  *) usage >&2; exit 64 ;;
esac

[[ $(id -u) -eq 0 ]] || fail 'run --apply as root on alpha'
[[ $(hostname -s) == alpha ]] || fail 'run --apply on alpha'

for command_name in id hostname incus python3 awk grep head cat mktemp rm wc stat; do
  command -v "$command_name" >/dev/null 2>&1 || fail "required host command is missing: $command_name"
done

install_file_if_changed() {
  local source_file=$1 destination=$2 mode=$3
  # shellcheck disable=SC2016
  if incus exec --mode=non-interactive "$INSTANCE" -- bash -c \
    'test ! -L "$1" && test "$(stat -c "%a:%u:%g" "$1" 2>/dev/null)" = "$2:0:0"' -- "$destination" "$mode" && \
    alpha_remote_file_matches "$INSTANCE" "$destination" "$source_file"; then
    return
  fi
  alpha_install_remote_file "$INSTANCE" "$destination" "$mode" < "$source_file"
  INSTALL_CHANGED=true
}

apply_manifest() {
  local resource=$1 kubectl_error status
  if kubectl_error=$(incus exec --mode=non-interactive "$INSTANCE" -- k3s kubectl apply -f - \
    2>&1 >/dev/null); then
    return 0
  else
    status=$?
  fi
  kubectl_error=${kubectl_error%%$'\n'*}
  fail "k3s could not apply $resource (exit $status): ${kubectl_error:-kubectl returned no stderr output}"
}

[[ -f $REPO_ROOT/deploy/k8s/deployer-access.yaml ]] || fail 'deployer access manifest is missing from this checkout'
[[ -f $REPO_ROOT/deploy/k8s/agent-access.yaml ]] || fail 'agent access manifest is missing from this checkout'
[[ -f $REPO_ROOT/deploy/k8s/agent-pod-delete-policy.yaml ]] || fail 'agent pod-delete policy manifest is missing from this checkout'
[[ -f $REPO_ROOT/deploy/k8s/check-agent-rbac.sh ]] || fail 'agent RBAC check is missing from this checkout'

[[ ! -e $KUBECONFIG_PATH && ! -L $KUBECONFIG_PATH ]] || {
  if [[ -L $KUBECONFIG_PATH || ! -f $KUBECONFIG_PATH ]]; then
    fail 'the deploy kubeconfig path exists and is not a regular file'
  fi
}
existing_instance=false
if incus info "$INSTANCE" >/dev/null 2>&1; then
  existing_instance=true
  marker=$(incus config get "$INSTANCE" "user.teagram-k3s-bootstrap" 2>/dev/null || true)
  [[ $marker == "$BOOTSTRAP_MARKER" ]] ||
    fail "Incus instance $INSTANCE already exists without this bootstrap marker"
fi

if [[ $existing_instance == false ]]; then
  incus init "$IMAGE_ALIAS" "$INSTANCE" \
    --config security.nesting=true \
    --config linux.kernel_modules=overlay,br_netfilter \
    --config limits.cpu=4 \
    --config limits.memory=8GiB \
    --config "user.teagram-k3s-bootstrap=$BOOTSTRAP_MARKER"
  incus config device override "$INSTANCE" root size=80GiB
  incus config device add "$INSTANCE" kmsg unix-char source=/dev/kmsg path=/dev/kmsg
else
  [[ $(incus config get "$INSTANCE" security.nesting) == true ]] ||
    fail 'the managed LXC has nesting disabled'
  [[ $(incus config get "$INSTANCE" linux.kernel_modules) == overlay,br_netfilter ]] ||
    fail 'the managed LXC kernel module configuration has drifted'
  [[ $(incus config get "$INSTANCE" limits.cpu) == 4 ]] || fail 'the managed LXC CPU limit has drifted'
  [[ $(incus config get "$INSTANCE" limits.memory) == 8GiB ]] || fail 'the managed LXC memory limit has drifted'
  root_size=$(incus config device get "$INSTANCE" root size 2>/dev/null || true)
  [[ $root_size == 80GiB ]] || {
    state=$(incus list "$INSTANCE" --format csv -c s)
    [[ $state == STOPPED ]] || fail 'the managed LXC root disk size has drifted'
    incus config device override "$INSTANCE" root size=80GiB
  }
  if ! incus config device show "$INSTANCE" | grep -Fq 'kmsg:'; then
    incus config device add "$INSTANCE" kmsg unix-char source=/dev/kmsg path=/dev/kmsg
  else
    [[ $(incus config device get "$INSTANCE" kmsg source) == /dev/kmsg && \
      $(incus config device get "$INSTANCE" kmsg path) == /dev/kmsg ]] ||
      fail 'the managed /dev/kmsg device has drifted'
  fi
fi

instance_state=$(incus list "$INSTANCE" --format csv -c s)
if [[ $instance_state == STOPPED ]]; then
  incus start "$INSTANCE"
elif [[ $instance_state != RUNNING ]]; then
  fail "Incus instance $INSTANCE is in unexpected state: $instance_state"
fi

if ! incus exec --mode=non-interactive "$INSTANCE" -- bash -s <<'EOF'
  set -euo pipefail
  export DEBIAN_FRONTEND=noninteractive
  packages_missing=false
  for package in ca-certificates curl iptables nftables openssl; do
    if ! dpkg-query -W -f='${db:Status-Status}' "$package" 2>/dev/null | grep -qx installed; then
      packages_missing=true
    fi
  done
  if [[ $packages_missing == true ]]; then
    apt-get update
    apt-get install -y ca-certificates curl iptables nftables openssl
  fi
  update-alternatives --set iptables /usr/sbin/iptables-nft
  update-alternatives --set ip6tables /usr/sbin/ip6tables-nft
  update-alternatives --set arptables /usr/sbin/arptables-nft
  update-alternatives --set ebtables /usr/sbin/ebtables-nft
  iptables --version | grep -Fq "(nf_tables)"
  ip6tables --version | grep -Fq "(nf_tables)"
  test -c /dev/kmsg
  test -d /sys/module/overlay
  test -d /sys/module/br_netfilter
EOF
then
  fail 'the k3s LXC package or kernel preflight failed'
fi

sysctl_file=$(alpha_mktemp_file "${TMPDIR:-/tmp}" teagram-k3s-sysctl)
trap 'rm -f -- "$sysctl_file"' EXIT
cat > "$sysctl_file" <<'EOF'
net.ipv4.ip_forward = 1
net.bridge.bridge-nf-call-iptables = 1
net.bridge.bridge-nf-call-ip6tables = 1
EOF
INSTALL_CHANGED=false
install_file_if_changed "$sysctl_file" /etc/sysctl.d/90-teagram-k3s.conf 644
if [[ $INSTALL_CHANGED == true ]]; then
  incus exec "$INSTANCE" -- sysctl --load=/etc/sysctl.d/90-teagram-k3s.conf >/dev/null
fi

install_k3s() {
  local skip_download=$1 install_exec=$2 release_tag=${K3S_VERSION//+/%2B} download_flag=''
  if [[ $skip_download == true ]]; then
    download_flag='INSTALL_K3S_SKIP_DOWNLOAD=true '
  fi
  incus exec "$INSTANCE" -- bash -c \
    "set -o pipefail; curl --fail --silent --show-error --location https://raw.githubusercontent.com/k3s-io/k3s/$release_tag/install.sh | ${download_flag}INSTALL_K3S_VERSION='$K3S_VERSION' INSTALL_K3S_EXEC='$install_exec' INSTALL_K3S_SKIP_START=true sh -"
}

restore_previous_k3s_unit() {
  incus exec "$INSTANCE" -- install -o root -g root -m 0600 \
    "$K3S_SERVICE_BACKUP" /etc/systemd/system/k3s.service || return 1
  incus exec "$INSTANCE" -- systemctl daemon-reload || return 1
  incus exec "$INSTANCE" -- systemctl restart k3s || return 1
}

verify_k3s_api_tls_san() {
  incus exec "$INSTANCE" -- openssl s_client \
    -connect "127.0.0.1:$K3S_API_TAILNET_PORT" \
    -servername "$K3S_TAILNET_HOST" \
    -verify 5 \
    -verify_hostname "$K3S_TAILNET_HOST" \
    -verify_return_error \
    -CAfile /var/lib/rancher/k3s/server/tls/server-ca.crt \
    </dev/null 2>&1 | grep -Fq 'Verify return code: 0 (ok)'
}

rotate_k3s_api_listener_certificate() {
  if ! incus exec "$INSTANCE" -- systemctl stop k3s; then
    return 1
  fi
  if ! incus exec "$INSTANCE" -- k3s certificate rotate --service k3s-server; then
    return 1
  fi
  incus exec "$INSTANCE" -- systemctl start k3s
}

if ! incus exec "$INSTANCE" -- bash -c 'command -v k3s >/dev/null 2>&1'; then
  install_k3s false "$K3S_EXEC"
else
  installed_version=$(incus exec "$INSTANCE" -- k3s --version | head -n 1)
  [[ $installed_version == "k3s version $K3S_VERSION"* ]] ||
    fail 'the managed LXC has an unexpected k3s version'
fi
unit=$(incus exec "$INSTANCE" -- cat /etc/systemd/system/k3s.service)
for expected_flag in --disable=traefik --disable=servicelb --disable=metrics-server \
  --write-kubeconfig-mode=0600; do
  grep -Fq -- "$expected_flag" <<<"$unit" || fail 'the installed k3s flags have drifted'
done
if ! grep -Fq -- "--tls-san=$K3S_TAILNET_HOST" <<<"$unit"; then
  if incus exec "$INSTANCE" -- test -e "$K3S_SERVICE_BACKUP"; then
    if ! incus exec "$INSTANCE" -- test -f "$K3S_SERVICE_BACKUP" ||
      incus exec "$INSTANCE" -- test -L "$K3S_SERVICE_BACKUP"; then
      fail 'the saved k3s service rollback file is not a regular file'
    fi
    [[ $(incus exec "$INSTANCE" -- stat -c '%a:%u:%g' "$K3S_SERVICE_BACKUP") == 600:0:0 ]] ||
      fail 'the saved k3s service rollback file is not root-only'
  else
    incus exec "$INSTANCE" -- install -o root -g root -m 0600 \
      /etc/systemd/system/k3s.service "$K3S_SERVICE_BACKUP"
  fi
  install_exec=$K3S_EXEC
  while IFS= read -r previous_san; do
    [[ -n $previous_san && $previous_san != "$K3S_TAILNET_HOST" ]] || continue
    if python3 - "$previous_san" <<'PY'
import ipaddress
import sys

try:
    address = ipaddress.ip_address(sys.argv[1])
except ValueError:
    raise SystemExit(1)
if address.version != 4 or address not in ipaddress.ip_network("100.64.0.0/10"):
    raise SystemExit(1)
PY
    then
      install_exec+=" --tls-san=$previous_san"
    else
      printf 'k3s bootstrap: dropping previous API SAN outside 100.64.0.0/10: %s\n' \
        "$previous_san" >&2
    fi
  done < <(grep --only-matching --extended-regexp -- '--tls-san=[^[:space:]]+' <<<"$unit" | cut -d= -f2- || true)
  install_k3s true "$install_exec"
  unit=$(incus exec "$INSTANCE" -- cat /etc/systemd/system/k3s.service)
  grep -Fq -- "--tls-san=$K3S_TAILNET_HOST" <<<"$unit" ||
    fail 'the k3s installer did not add the tailnet API SAN'
  incus exec "$INSTANCE" -- systemctl daemon-reload
  if ! rotate_k3s_api_listener_certificate; then
    restore_previous_k3s_unit ||
      fail "restore failed; recover from $K3S_SERVICE_BACKUP"
    fail 'k3s could not rotate its API listener certificate; the previous unit was restored'
  fi
fi
grep -Fq -- "--tls-san=$K3S_TAILNET_HOST" <<<"$unit" ||
  fail 'the installed k3s flags have drifted'
incus exec "$INSTANCE" -- systemctl enable --now k3s
incus exec "$INSTANCE" -- k3s kubectl wait \
  --for=condition=Ready node --all --timeout=300s >/dev/null

if ! verify_k3s_api_tls_san; then
  if ! incus exec "$INSTANCE" -- test -e "$K3S_SERVICE_BACKUP"; then
    incus exec "$INSTANCE" -- install -o root -g root -m 0600 \
      /etc/systemd/system/k3s.service "$K3S_SERVICE_BACKUP"
  fi
  if ! rotate_k3s_api_listener_certificate; then
    restore_previous_k3s_unit ||
      fail "restore failed; recover from $K3S_SERVICE_BACKUP"
    fail 'k3s could not rotate its API listener certificate; the previous unit was restored'
  fi
  if ! verify_k3s_api_tls_san; then
    restore_previous_k3s_unit ||
      fail "restore failed; recover from $K3S_SERVICE_BACKUP"
    fail 'the API listener does not present a CA-valid certificate for the tailnet DNS name; the previous unit was restored'
  fi
fi

namespace_file="$REPO_ROOT/deploy/k8s/base/namespace.yaml"
apply_manifest "namespace/$NAMESPACE" < "$namespace_file"
namespace_json=$(incus exec "$INSTANCE" -- k3s kubectl get namespace "$NAMESPACE" --output json)
if ! printf '%s' "$namespace_json" | python3 -c '
import json
import sys

try:
    labels = json.load(sys.stdin).get("metadata", {}).get("labels", {})
except (json.JSONDecodeError, AttributeError):
    raise SystemExit(1)
if (not isinstance(labels, dict) or
        labels.get("pod-security.kubernetes.io/enforce") != "baseline" or
        labels.get("pod-security.kubernetes.io/enforce-version") != "v1.37"):
    raise SystemExit(1)
'; then
  fail 'the teagram namespace does not enforce the Pod Security baseline'
fi
apply_manifest 'deployer RBAC and token manifest' < "$REPO_ROOT/deploy/k8s/deployer-access.yaml"
apply_manifest 'agent namespace-scoped RBAC manifest' < "$REPO_ROOT/deploy/k8s/agent-access.yaml"
apply_manifest 'agent pod-delete admission policy and binding' < "$REPO_ROOT/deploy/k8s/agent-pod-delete-policy.yaml"
bash "$REPO_ROOT/deploy/k8s/check-agent-rbac.sh" \
  incus exec --mode=non-interactive "$INSTANCE" -- k3s kubectl

admission_result=$(incus exec "$INSTANCE" -- k3s kubectl \
  create --dry-run=server -f - 2>&1 <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: teagram-deployer-escape-check
  namespace: teagram
spec:
  hostPID: true
  containers:
    - name: escape
      image: busybox:1.36
      command: ["sleep", "3600"]
      securityContext:
        privileged: true
      volumeMounts:
        - name: host-root
          mountPath: /host
  volumes:
    - name: host-root
      hostPath:
        path: /
        type: Directory
EOF
) && fail 'Pod Security Admission accepted a privileged hostPath pod'
[[ $admission_result == *'violates PodSecurity'* && $admission_result == *'hostPath'* && \
  $admission_result == *'privileged'* ]] ||
  fail 'the unsafe pod was not rejected by the Pod Security baseline'

incus exec "$INSTANCE" -- k3s kubectl wait --for=create secret/teagram-deployer-token \
  --namespace "$NAMESPACE" --timeout=60s >/dev/null
# The controller fills token and ca.crt after the Secret object exists, so wait for both keys.
for data_key in '{.data.token}' '{.data.ca\.crt}'; do
  if ! incus exec "$INSTANCE" -- k3s kubectl wait --for="jsonpath=$data_key" \
    secret/teagram-deployer-token --namespace "$NAMESPACE" --timeout=120s >/dev/null; then
    fail 'the deploy token or cluster CA was not populated in time'
  fi
done
incus exec "$INSTANCE" -- k3s kubectl get secret teagram-deployer-token \
  --namespace "$NAMESPACE" --output json |
  python3 -c '
import json
import ipaddress
import os
import pathlib
import stat
import sys
import tempfile
import base64
from urllib.parse import urlsplit

secret = json.load(sys.stdin)
encoded_token = secret.get("data", {}).get("token")
ca = secret.get("data", {}).get("ca.crt")
if not encoded_token or not ca:
    print("k3s bootstrap: deploy token or cluster CA is not ready", file=sys.stderr)
    raise SystemExit(1)
try:
    token = base64.b64decode(encoded_token, validate=True).decode("ascii")
    base64.b64decode(ca, validate=True)
except (ValueError, UnicodeDecodeError):
    print("k3s bootstrap: deploy token or cluster CA is invalid", file=sys.stderr)
    raise SystemExit(1)
server = "https://" + sys.argv[1] + ":" + sys.argv[3]
expected_port = int(sys.argv[3])
config = {
    "apiVersion": "v1",
    "kind": "Config",
    "clusters": [{"name": "teagram-k3s", "cluster": {
        "server": server, "certificate-authority-data": ca,
    }}],
    "contexts": [{"name": "teagram-k3s", "context": {
        "cluster": "teagram-k3s", "namespace": "teagram", "user": "teagram-deployer",
    }}],
    "current-context": "teagram-k3s",
    "users": [{"name": "teagram-deployer", "user": {"token": token}}],
}
path = pathlib.Path(sys.argv[2])
payload = json.dumps(config, separators=(",", ":")) + "\n"
if path.exists():
    if path.is_symlink() or not path.is_file() or path.stat().st_uid != 0:
        print("k3s bootstrap: deploy kubeconfig path is not a root-owned regular file", file=sys.stderr)
        raise SystemExit(1)
    try:
        existing = json.loads(path.read_text())
    except (OSError, json.JSONDecodeError):
        print("k3s bootstrap: existing deploy kubeconfig is unreadable", file=sys.stderr)
        raise SystemExit(1)
    if existing == config:
        if stat.S_IMODE(path.stat().st_mode) != 0o600:
            os.chmod(path, 0o600)
        raise SystemExit(0)
    try:
        existing_server = existing["clusters"][0]["cluster"]["server"]
        previous_url = urlsplit(existing_server)
        previous_port = previous_url.port
        no_extra_url_parts = not (previous_url.path or previous_url.query or
                                  previous_url.fragment or previous_url.username or
                                  previous_url.password)
        server_is_current = (previous_url.scheme == "https" and
                             previous_url.hostname == sys.argv[1] and
                             previous_port == expected_port and no_extra_url_parts)
        server_is_legacy = False
        if not server_is_current:
            previous_ip = ipaddress.ip_address(previous_url.hostname or "")
            server_is_legacy = (previous_url.scheme == "https" and
                                previous_ip.version == 4 and
                                previous_port == expected_port and no_extra_url_parts and
                                previous_ip in ipaddress.ip_network("100.64.0.0/10"))
    except (IndexError, KeyError, TypeError, ValueError, json.JSONDecodeError):
        print("k3s bootstrap: existing deploy kubeconfig server must use the cluster tailnet endpoint or a legacy tailnet IP", file=sys.stderr)
        raise SystemExit(1)
    if not (server_is_current or server_is_legacy):
        print("k3s bootstrap: existing deploy kubeconfig server must use the cluster tailnet endpoint or a legacy tailnet IP", file=sys.stderr)
        raise SystemExit(1)
    previous_config = json.loads(json.dumps(config))
    previous_config["clusters"][0]["cluster"]["server"] = existing_server
    if existing != previous_config:
        print("k3s bootstrap: existing deploy kubeconfig token or CA does not match this cluster", file=sys.stderr)
        raise SystemExit(1)
fd, temporary = tempfile.mkstemp(prefix=".teagram-k3s-kubeconfig.", dir=str(path.parent))
try:
    os.fchmod(fd, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as output:
        output.write(payload)
        output.flush()
        os.fsync(output.fileno())
    os.replace(temporary, path)
except BaseException:
    try:
        os.unlink(temporary)
    except FileNotFoundError:
        pass
    raise
' "$K3S_TAILNET_HOST" "$KUBECONFIG_PATH" "$K3S_API_TAILNET_PORT"

# `can-i` warns on stderr for cluster-scoped resources; compare stdout and status.
expect_deployer_can_i() {
  local expected=$1 verb=$2 resource=$3 answer status
  shift 3
  if answer=$(incus exec "$INSTANCE" -- k3s kubectl auth can-i "$verb" "$resource" \
    --as=system:serviceaccount:teagram:teagram-deployer "$@"); then
    status=0
  else
    status=$?
  fi
  case "$expected:$status:$answer" in
    yes:0:yes|no:1:no) ;;
    *) fail 'the deploy ServiceAccount RBAC boundary is incorrect' ;;
  esac
}

expect_deployer_can_i yes create deployments.apps --namespace teagram
expect_deployer_can_i yes patch deployments.apps --namespace teagram
expect_deployer_can_i yes patch statefulsets.apps --namespace teagram
expect_deployer_can_i yes get replicasets.apps --namespace teagram
expect_deployer_can_i yes list replicasets.apps --namespace teagram
expect_deployer_can_i yes get controllerrevisions.apps --namespace teagram
expect_deployer_can_i yes list controllerrevisions.apps --namespace teagram
for history_resource in replicasets.apps controllerrevisions.apps; do
  for history_verb in create update patch delete; do
    expect_deployer_can_i no "$history_verb" "$history_resource" --namespace teagram
  done
done
expect_deployer_can_i yes create jobs.batch --namespace teagram
expect_deployer_can_i yes get jobs.batch --namespace teagram
expect_deployer_can_i yes patch jobs.batch --namespace teagram
expect_deployer_can_i yes create cronjobs.batch --namespace teagram
expect_deployer_can_i yes patch cronjobs.batch --namespace teagram
expect_deployer_can_i yes patch services --namespace teagram
expect_deployer_can_i yes create configmaps --namespace teagram
expect_deployer_can_i yes get persistentvolumeclaims --namespace teagram
expect_deployer_can_i yes create persistentvolumeclaims --namespace teagram
expect_deployer_can_i yes patch persistentvolumeclaims --namespace teagram
expect_deployer_can_i yes list pods --namespace teagram
expect_deployer_can_i yes get pods --subresource=log --namespace teagram
expect_deployer_can_i yes delete pods --namespace teagram
expect_deployer_can_i yes list events --namespace teagram
expect_deployer_can_i no create pods --namespace teagram
for secret_verb in get list watch create update patch delete; do
  expect_deployer_can_i no "$secret_verb" secrets --namespace teagram
done
expect_deployer_can_i no create roles.rbac.authorization.k8s.io --namespace teagram
expect_deployer_can_i no create rolebindings.rbac.authorization.k8s.io --namespace teagram
expect_deployer_can_i no create serviceaccounts --namespace teagram
expect_deployer_can_i no create serviceaccounts --subresource=token --namespace teagram
expect_deployer_can_i no '*' '*' --namespace teagram
expect_deployer_can_i no '*' '*' --namespace default
expect_deployer_can_i no '*' namespaces

test "$(incus exec "$INSTANCE" -- k3s kubectl get nodes --no-headers | wc -l)" -ge 1 ||
  fail 'the k3s node list is empty'
test "$(incus exec "$INSTANCE" -- k3s kubectl get pods --namespace kube-system --no-headers | wc -l)" -ge 1 ||
  fail 'the k3s system pod list is empty'
if incus exec "$INSTANCE" -- ss -lnt | awk 'NR > 1 {print $4}' | grep -Eq '(^|:)6443$'; then
  :
else
  fail 'the k3s API is not listening inside the LXC'
fi
[[ $(stat -c '%a:%u' "$KUBECONFIG_PATH") == 600:0 ]] ||
  fail 'the deploy kubeconfig is not root-only'

printf 'k3s bootstrap complete: version=%s api=%s:%s namespace=%s deploy_kubeconfig=%s (mode 0600)\n' \
  "$K3S_VERSION" "$K3S_TAILNET_HOST" "$K3S_API_TAILNET_PORT" "$NAMESPACE" \
  "$KUBECONFIG_PATH"
