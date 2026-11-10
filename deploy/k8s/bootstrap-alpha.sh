#!/usr/bin/env bash
set -euo pipefail
set -f
umask 077

readonly INSTANCE=teagram-k3s
readonly SOURCE_INSTANCE=teagram
readonly NAMESPACE=teagram
readonly K3S_VERSION=v1.37.1+k3s1
readonly IMAGE_ALIAS=images:ubuntu/24.04
readonly KUBECONFIG_PATH=/root/teagram-k3s-deploy.kubeconfig
readonly CUTOVER_KUBECONFIG_PATH=/root/teagram-k3s-cutover.kubeconfig
readonly BOOTSTRAP_MARKER=MAIN-3278
readonly K3S_API_HOST_PORT=16443
readonly OPS_SSH_HOST_PORT=12222
readonly K3S_API_TAILNET_PORT=6443
readonly OPS_SSH_TAILNET_PORT=2222
readonly ROOT_ACCESS_KEY_PATH=/run/secrets/rustfs-root-access-key
readonly ROOT_SECRET_KEY_PATH=/run/secrets/rustfs-root-secret-key
readonly BLOB_SECRET_KEY_PATH=/run/secrets/teagramd-blob-secret-key
readonly MEDIA_KEYS_PATH=/run/teagramd/media-keys

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
Would verify root access on alpha, its Tailscale address, the live teagram containers and source values, and existing Serve port conflicts.
Would create or resume Incus LXC teagram-k3s from Ubuntu 24.04 with 4 CPUs, 8 GiB RAM, nesting, overlay and br_netfilter modules, and the MAIN-3278 ownership marker.
Would set the instance root disk to 80 GiB and attach host /dev/kmsg to the LXC.
Would add or verify Incus proxies bound to alpha loopback: 16443 to LXC port 6443 and 12222 to LXC port 22.
Would start the LXC, install ca-certificates, curl, iptables, nftables, and OpenSSH as needed, and select only nft-backed iptables alternatives.
Would install /etc/sysctl.d/90-teagram-k3s.conf in the LXC and enable IP forwarding plus bridge netfilter.
Would install pinned k3s v1.37.1+k3s1 with Traefik, ServiceLB, and metrics-server disabled, then enable it and wait for a Ready node.
Would create /run/sshd before validating the SSH configuration, install the agents' forced-command SSH menu and the existing Multica public key, restrict root SSH to that command, disable password login and forwarding, and restart sshd only if its files change.
Would create the teagram namespace with enforced Pod Security baseline, ServiceAccount teagram-deployer, namespace-only Role and RoleBinding, and token Secret.
Would create ServiceAccount teagram-agent with token automount disabled and a namespace-only read Role and RoleBinding.
Would install the cluster-scoped ValidatingAdmissionPolicy teagram-agent-pod-delete and binding, restricting agent pod deletes to controller-owned teagramd, linkselector, linklanding, or cloudflared pods.
Would apply ConfigMaps teagramd-runtime and link-edge-runtime, plus Secrets postgres-credentials, rustfs-credentials, teagramd-blob-credentials, teagramd-media-keys, and cloudflared-token from the live teagram LXC (values are not displayed).
Would configure Tailscale Serve TCP routes on alpha ports 6443 and 2222 to the loopback proxies; no public listener is configured.
Would write /root/teagram-k3s-deploy.kubeconfig with the namespace-restricted deployer token, plus /root/teagram-k3s-cutover.kubeconfig with the k3s admin client certificate for root-only cutover, rollback, and reset. Both files use mode 0600; the deployer Role remains restricted.
Would verify the k3s node, disabled components, namespace boundary, Serve routes, and both kubeconfig permissions.
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

for command_name in id hostname incus tailscale python3 awk grep head cat mktemp rm wc stat; do
  command -v "$command_name" >/dev/null 2>&1 || fail "required host command is missing: $command_name"
done

container_env() {
  local container=$1 key=$2 required=${3:-true}
  incus exec "$SOURCE_INSTANCE" -- docker inspect \
    --format '{{range .Config.Env}}{{println .}}{{end}}' "$container" |
    python3 -c '
import sys

key, required = sys.argv[1], sys.argv[2] == "true"
matches = [line.split("=", 1)[1] for line in sys.stdin.read().splitlines()
           if line.startswith(key + "=")]
if len(matches) > 1 or (required and len(matches) != 1):
    print("k3s bootstrap: expected one source value for " + key, file=sys.stderr)
    raise SystemExit(1)
value = matches[0] if matches else ""
if "\n" in value or "\r" in value or "\0" in value:
    print("k3s bootstrap: source value cannot be represented safely: " + key,
          file=sys.stderr)
    raise SystemExit(1)
sys.stdout.write(value)
' "$key" "$required"
}

container_file_value() {
  local container=$1 path=$2
  incus exec "$SOURCE_INSTANCE" -- docker cp "$container:$path" - |
    python3 -c '
import sys
import tarfile

value = None
file_count = 0
with tarfile.open(fileobj=sys.stdin.buffer, mode="r|*") as archive:
    for member in archive:
        if not member.isfile():
            continue
        file_count += 1
        if file_count > 1 or member.size == 0 or member.size > 65536:
            print("k3s bootstrap: source secret file is missing or has an invalid size",
                  file=sys.stderr)
            raise SystemExit(1)
        extracted = archive.extractfile(member)
        if extracted is None:
            print("k3s bootstrap: source secret file could not be read", file=sys.stderr)
            raise SystemExit(1)
        value = extracted.read()
if file_count != 1 or value is None:
    print("k3s bootstrap: source secret file is missing or has an invalid size",
          file=sys.stderr)
    raise SystemExit(1)
try:
    text = value.decode("ascii")
except UnicodeDecodeError:
    print("k3s bootstrap: source secret file is not ASCII text", file=sys.stderr)
    raise SystemExit(1)
if "\n" in text or "\r" in text or "\0" in text:
    print("k3s bootstrap: source secret file contains unsupported line breaks",
          file=sys.stderr)
    raise SystemExit(1)
sys.stdout.write(text)
'
}

container_token() {
  local value
  value=$(container_env teagram-web-cloudflared TUNNEL_TOKEN false)
  if [[ -z $value ]]; then
    value=$(container_env teagram-web-cloudflared TUNNEL_TOKEN_FILE false)
    if [[ -n $value ]]; then
      value=$(container_file_value teagram-web-cloudflared "$value")
    fi
  fi
  if [[ -z $value ]]; then
    value=$(incus exec "$SOURCE_INSTANCE" -- docker inspect \
      --format '{{json .Config.Cmd}}' teagram-web-cloudflared |
      python3 -c '
import json
import sys

args = json.load(sys.stdin)
value = ""
for index, arg in enumerate(args):
    if arg == "--token" and index + 1 < len(args):
        value = args[index + 1]
    elif arg.startswith("--token="):
        value = arg.split("=", 1)[1]
if value:
    sys.stdout.write(value)
')
  fi
  [[ -n $value ]] || fail 'the running Cloudflare tunnel token is unavailable on teagram'
  [[ $value != *$'\n'* && $value != *$'\r'* ]] || fail 'the Cloudflare tunnel token has an unsupported format'
  printf '%s' "$value"
}

serve_port_state() {
  local config=$1 port=$2 expected=$3
  printf '%s' "$config" | python3 -c '
import json
import sys

config = json.load(sys.stdin)
if not isinstance(config, dict):
    config = {}
port, expected = sys.argv[1], sys.argv[2]
ports = config.get("TCP") or {}
if not isinstance(ports, dict):
    ports = {}
entry = ports.get(port)
if entry is None:
    print("missing")
elif entry.get("TCPForward") == expected:
    print("matching")
else:
    print("conflict")
funnel = config.get("AllowFunnel", {})
if isinstance(funnel, dict) and any(key.endswith(":" + port) and enabled for key, enabled in funnel.items()):
    print("funnel")
' "$port" "$expected"
}

validate_serve_config() {
  local config=$1 port=$2 host_port=$3 state base_state
  state=$(serve_port_state "$config" "$port" "127.0.0.1:$host_port")
  base_state=${state%%$'\n'*}
  [[ $state != *funnel* ]] || fail "Tailscale Funnel is allowed for port $port"
  [[ $base_state == missing || $base_state == matching ]] ||
    fail "Tailscale Serve port $port is already mapped to a different target"
}

ensure_serve_port() {
  local port=$1 host_port=$2 config state
  config=$(tailscale serve status --json)
  validate_serve_config "$config" "$port" "$host_port"
  state=$(serve_port_state "$config" "$port" "127.0.0.1:$host_port")
  if [[ ${state%%$'\n'*} == missing ]]; then
    tailscale serve --yes --bg --tcp="$port" "tcp://127.0.0.1:$host_port" >/dev/null
  fi
  config=$(tailscale serve status --json)
  state=$(serve_port_state "$config" "$port" "127.0.0.1:$host_port")
  [[ $state == matching ]] || fail "Tailscale Serve failed to configure tailnet port $port"
}

ensure_proxy() {
  local name=$1 listen=$2 connect=$3 devices actual expected
  devices=$(incus config device show "$INSTANCE")
  if grep -Fq "${name}:" <<<"$devices"; then
    for property in type listen connect bind; do
      case $property in
        type) expected=proxy ;;
        listen) expected=$listen ;;
        connect) expected=$connect ;;
        bind) expected=host ;;
      esac
      actual=$(incus config device get "$INSTANCE" "$name" "$property")
      [[ $actual == "$expected" ]] || fail "Incus proxy device $name has unexpected $property"
    done
  else
    incus config device add "$INSTANCE" "$name" proxy \
      "listen=$listen" "connect=$connect" bind=host
  fi
}

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

apply_env_resource() {
  local kind=$1 name=$2
  local kubectl_error status operation kubectl_status
  local -a create_args=(create "$kind")
  shift 2
  if [[ $kind == secret ]]; then
    create_args+=(generic)
  fi
  create_args+=("$name" --namespace "$NAMESPACE" --dry-run=client -o json)
  # The temporary env file stays on the LXC's memory-backed /run filesystem.
  # shellcheck disable=SC2016
  if kubectl_error=$(printf '%s\n' "$@" |
    incus exec --mode=non-interactive "$INSTANCE" -- bash -c '
      env_file=$(mktemp /run/teagram-k3s-env.XXXXXX) || {
        status=$?
        printf "K3S_BOOTSTRAP_FAILURE create %s\n" "$status" >&2
        exit "$status"
      }
      trap '\''rm -f -- "$env_file"'\'' EXIT HUP INT TERM
      cat > "$env_file"
      input_status=$?
      if (( input_status != 0 )); then
        printf "K3S_BOOTSTRAP_FAILURE create %s\n" "$input_status" >&2
        exit "$input_status"
      fi
      manifest=$(k3s kubectl "$@" "--from-env-file=$env_file")
      create_status=$?
      if (( create_status != 0 )); then
        printf "K3S_BOOTSTRAP_FAILURE create %s\n" "$create_status" >&2
        exit "$create_status"
      fi
      printf "%s" "$manifest" | k3s kubectl apply -f -
      apply_status=$?
      if (( apply_status != 0 )); then
        printf "K3S_BOOTSTRAP_FAILURE apply %s\n" "$apply_status" >&2
        exit "$apply_status"
      fi
    ' -- "${create_args[@]}" 2>&1 >/dev/null); then
    return 0
  else
    status=$?
  fi
  operation='create or apply'
  kubectl_status=$status
  if [[ $kubectl_error =~ K3S_BOOTSTRAP_FAILURE[[:space:]]+(create|apply)[[:space:]]+([0-9]+) ]]; then
    operation=${BASH_REMATCH[1]}
    kubectl_status=${BASH_REMATCH[2]}
  fi
  kubectl_error=${kubectl_error%%$'\n'*}
  [[ $kubectl_error != K3S_BOOTSTRAP_FAILURE* ]] || kubectl_error=''
  fail "k3s could not $operation $kind/$name (exit $kubectl_status): ${kubectl_error:-kubectl returned no stderr output}"
}

assert_env_line() {
  local key=$1 value=$2
  [[ $value != *$'\n'* && $value != *$'\r'* ]] || fail "source value cannot be represented safely: $key"
}

assert_hex_length() {
  local name=$1 value=$2 length=$3
  [[ $value =~ ^[0-9a-f]+$ && ${#value} -eq $length ]] ||
    fail "live teagram credential has an invalid format: $name"
}

[[ -f $REPO_ROOT/deploy/k8s/teagram-k3s-ops.sh ]] || fail 'forced-command menu is missing from this checkout'
[[ -f $REPO_ROOT/deploy/k8s/deployer-access.yaml ]] || fail 'deployer access manifest is missing from this checkout'
[[ -f $REPO_ROOT/deploy/k8s/agent-access.yaml ]] || fail 'agent access manifest is missing from this checkout'
[[ -f $REPO_ROOT/deploy/k8s/agent-pod-delete-policy.yaml ]] || fail 'agent pod-delete policy manifest is missing from this checkout'
[[ -f $REPO_ROOT/deploy/k8s/check-agent-rbac.sh ]] || fail 'agent RBAC check is missing from this checkout'
incus info "$SOURCE_INSTANCE" >/dev/null 2>&1 || fail 'the live teagram LXC is unavailable'
incus exec "$SOURCE_INSTANCE" -- docker inspect teagram-teagramd-1 teagram-postgres-1 \
  teagram-rustfs-1 teagram-link-edge-selector-1 teagram-web-cloudflared >/dev/null ||
  fail 'one or more live teagram source workloads are unavailable'

TAILNET_IP=$(tailscale ip -4 | head -n 1)
[[ $TAILNET_IP =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || fail 'alpha has no Tailscale IPv4 address'
[[ ! -e $KUBECONFIG_PATH && ! -L $KUBECONFIG_PATH ]] || {
  if [[ -L $KUBECONFIG_PATH || ! -f $KUBECONFIG_PATH ]]; then
    fail 'the deploy kubeconfig path exists and is not a regular file'
  fi
}
[[ ! -L $CUTOVER_KUBECONFIG_PATH && ( ! -e $CUTOVER_KUBECONFIG_PATH || -f $CUTOVER_KUBECONFIG_PATH ) ]] ||
  fail 'the root-only cutover kubeconfig path is not a regular file'

POSTGRES_PASSWORD=$(container_env teagram-postgres-1 POSTGRES_PASSWORD)
TG_POSTGRES_DSN=$(container_env teagram-teagramd-1 TG_POSTGRES_DSN)
[[ $TG_POSTGRES_DSN == "postgres://postgres:${POSTGRES_PASSWORD}@postgres:5432/teagram?sslmode=disable" ]] ||
  fail 'live Postgres password and DSN do not match the k8s workload layout'
TG_PUBLIC_LINK_PREFIX=$(container_env teagram-teagramd-1 TG_PUBLIC_LINK_PREFIX)
TG_ADVERTISE_ADDR=$(container_env teagram-teagramd-1 TG_ADVERTISE_ADDR false)
TG_MEDIA_KEY_CURRENT=$(container_env teagram-teagramd-1 TG_MEDIA_KEY_CURRENT)
TG_INVITE_WEB_SIGNUP_URL=$(container_env teagram-link-edge-selector-1 TG_INVITE_WEB_SIGNUP_URL)
TG_INVITE_DESKTOP_URL=$(container_env teagram-link-edge-selector-1 TG_INVITE_DESKTOP_URL false)
RUSTFS_ACCESS_KEY=$(container_file_value teagram-rustfs-1 "$ROOT_ACCESS_KEY_PATH")
RUSTFS_SECRET_KEY=$(container_file_value teagram-rustfs-1 "$ROOT_SECRET_KEY_PATH")
TG_BLOB_S3_ACCESS_KEY_ID=$(container_env teagram-teagramd-1 TG_BLOB_S3_ACCESS_KEY_ID)
TG_BLOB_S3_SECRET_ACCESS_KEY=$(container_file_value teagram-teagramd-1 "$BLOB_SECRET_KEY_PATH")
CLOUDFLARED_TOKEN=$(container_token)
AGENT_AUTHORIZED_KEY=$(incus exec "$SOURCE_INSTANCE" -- awk \
  '/command="\/usr\/local\/sbin\/teagram-ops"/ && /multica-daemon@delta/ {print}' \
  /root/.ssh/authorized_keys)
[[ -n $AGENT_AUTHORIZED_KEY && $AGENT_AUTHORIZED_KEY != *$'\n'* ]] ||
  fail 'the live teagram LXC has no unique forced-command agent key'

assert_hex_length RUSTFS_ACCESS_KEY "$RUSTFS_ACCESS_KEY" 20
assert_hex_length RUSTFS_SECRET_KEY "$RUSTFS_SECRET_KEY" 64
assert_hex_length TG_BLOB_S3_ACCESS_KEY_ID "$TG_BLOB_S3_ACCESS_KEY_ID" 20
assert_hex_length TG_BLOB_S3_SECRET_ACCESS_KEY "$TG_BLOB_S3_SECRET_ACCESS_KEY" 64
assert_env_line POSTGRES_PASSWORD "$POSTGRES_PASSWORD"
assert_env_line TG_POSTGRES_DSN "$TG_POSTGRES_DSN"
assert_env_line TG_PUBLIC_LINK_PREFIX "$TG_PUBLIC_LINK_PREFIX"
assert_env_line TG_ADVERTISE_ADDR "$TG_ADVERTISE_ADDR"
[[ $TG_MEDIA_KEY_CURRENT =~ ^[1-9][0-9]*$ ]] || fail 'live media encryption key version is invalid'
assert_env_line TG_INVITE_WEB_SIGNUP_URL "$TG_INVITE_WEB_SIGNUP_URL"
assert_env_line TG_INVITE_DESKTOP_URL "$TG_INVITE_DESKTOP_URL"
assert_env_line CLOUDFLARED_TOKEN "$CLOUDFLARED_TOKEN"
[[ -n $TG_PUBLIC_LINK_PREFIX && -n $TG_INVITE_WEB_SIGNUP_URL ]] ||
  fail 'a required live public URL is missing'

MEDIA_KEY_NAMES=$(incus exec "$SOURCE_INSTANCE" -- docker cp \
  teagram-teagramd-1:"$MEDIA_KEYS_PATH/." - |
  python3 -c '
import re
import sys
import tarfile

names = []
with tarfile.open(fileobj=sys.stdin.buffer, mode="r|*") as archive:
    for member in archive:
        name = member.name.rsplit("/", 1)[-1]
        if member.isfile() and re.fullmatch(r"[0-9]+\.key", name):
            if member.size < 1 or member.size > 65536:
                print("k3s bootstrap: a live media key has an invalid size", file=sys.stderr)
                raise SystemExit(1)
            names.append(name)
if "1.key" not in names:
    print("k3s bootstrap: the live current media key 1.key is missing", file=sys.stderr)
    raise SystemExit(1)
if len(names) != len(set(names)):
    print("k3s bootstrap: duplicate live media key files were found", file=sys.stderr)
    raise SystemExit(1)
print("\n".join(sorted(names, key=lambda name: int(name.split(".", 1)[0]))))
')
[[ -n $MEDIA_KEY_NAMES ]] || fail 'no live media key files were found'
grep --fixed-strings --line-regexp --quiet "$TG_MEDIA_KEY_CURRENT.key" <<< "$MEDIA_KEY_NAMES" ||
  fail 'live media encryption key version does not match the mounted key set'

existing_instance=false
if incus info "$INSTANCE" >/dev/null 2>&1; then
  existing_instance=true
  marker=$(incus config get "$INSTANCE" "user.teagram-k3s-bootstrap" 2>/dev/null || true)
  [[ $marker == "$BOOTSTRAP_MARKER" ]] ||
    fail "Incus instance $INSTANCE already exists without this bootstrap marker"
fi

SERVE_CONFIG=$(tailscale serve status --json)
validate_serve_config "$SERVE_CONFIG" "$K3S_API_TAILNET_PORT" "$K3S_API_HOST_PORT"
validate_serve_config "$SERVE_CONFIG" "$OPS_SSH_TAILNET_PORT" "$OPS_SSH_HOST_PORT"

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

ensure_proxy api "tcp:127.0.0.1:$K3S_API_HOST_PORT" 'tcp:127.0.0.1:6443'
ensure_proxy ops-ssh "tcp:127.0.0.1:$OPS_SSH_HOST_PORT" 'tcp:127.0.0.1:22'

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
  for package in ca-certificates curl iptables nftables openssh-server; do
    if ! dpkg-query -W -f='${db:Status-Status}' "$package" 2>/dev/null | grep -qx installed; then
      packages_missing=true
    fi
  done
  if [[ $packages_missing == true ]]; then
    apt-get update
    apt-get install -y ca-certificates curl iptables nftables openssh-server
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
ops_ssh_config=$(alpha_mktemp_file "${TMPDIR:-/tmp}" teagram-k3s-ops-ssh)
trap 'rm -f -- "$sysctl_file" "$ops_ssh_config"' EXIT
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

if ! incus exec "$INSTANCE" -- bash -c 'command -v k3s >/dev/null 2>&1'; then
  k3s_release_tag=${K3S_VERSION//+/%2B}
  incus exec "$INSTANCE" -- bash -c \
    "set -o pipefail; curl --fail --silent --show-error --location https://raw.githubusercontent.com/k3s-io/k3s/$k3s_release_tag/install.sh | INSTALL_K3S_VERSION='$K3S_VERSION' INSTALL_K3S_EXEC='server --disable=traefik --disable=servicelb --disable=metrics-server --tls-san=$TAILNET_IP --write-kubeconfig-mode=0600' sh -"
else
  installed_version=$(incus exec "$INSTANCE" -- k3s --version | head -n 1)
  [[ $installed_version == "k3s version $K3S_VERSION"* ]] ||
    fail 'the managed LXC has an unexpected k3s version'
fi
unit=$(incus exec "$INSTANCE" -- cat /etc/systemd/system/k3s.service)
for expected_flag in --disable=traefik --disable=servicelb --disable=metrics-server \
  "--tls-san=$TAILNET_IP" --write-kubeconfig-mode=0600; do
  grep -Fq -- "$expected_flag" <<<"$unit" || fail 'the installed k3s flags have drifted'
done
incus exec "$INSTANCE" -- systemctl enable --now k3s
incus exec "$INSTANCE" -- k3s kubectl wait \
  --for=condition=Ready node --all --timeout=300s >/dev/null

cat > "$ops_ssh_config" <<'EOF'
PermitRootLogin forced-commands-only
PasswordAuthentication no
KbdInteractiveAuthentication no
PubkeyAuthentication yes
AllowUsers root
AllowTcpForwarding no
AllowAgentForwarding no
X11Forwarding no
PermitTunnel no
PermitTTY no
EOF
incus exec "$INSTANCE" -- install -d -o root -g root -m 0700 /root/.ssh
remote_keys=$(incus exec "$INSTANCE" -- cat /root/.ssh/authorized_keys 2>/dev/null || true)
remote_key_mode=$(incus exec "$INSTANCE" -- stat -c '%a:%u:%g' /root/.ssh/authorized_keys 2>/dev/null || true)
INSTALL_CHANGED=false
if [[ -n $remote_keys && $remote_keys != "$AGENT_AUTHORIZED_KEY" ]]; then
  fail 'the managed LXC has unexpected authorized keys'
fi
if [[ $remote_keys != "$AGENT_AUTHORIZED_KEY" || $remote_key_mode != 600:0:0 ]]; then
  printf '%s\n' "$AGENT_AUTHORIZED_KEY" |
    alpha_install_remote_file "$INSTANCE" /root/.ssh/authorized_keys 0600
  INSTALL_CHANGED=true
fi
install_file_if_changed "$ops_ssh_config" /etc/ssh/sshd_config.d/00-teagram-k3s-ops.conf 644
install_file_if_changed "$REPO_ROOT/deploy/k8s/teagram-k3s-ops.sh" \
  /usr/local/sbin/teagram-ops 700
alpha_ensure_sshd_runtime_dir "$INSTANCE"
incus exec "$INSTANCE" -- sshd -t
if ! incus exec "$INSTANCE" -- systemctl is-enabled --quiet ssh; then
  incus exec "$INSTANCE" -- systemctl enable ssh
fi
if [[ $INSTALL_CHANGED == true ]]; then
  incus exec "$INSTANCE" -- systemctl restart ssh
elif ! incus exec "$INSTANCE" -- systemctl is-active --quiet ssh; then
  incus exec "$INSTANCE" -- systemctl start ssh
fi

ensure_serve_port "$K3S_API_TAILNET_PORT" "$K3S_API_HOST_PORT"
ensure_serve_port "$OPS_SSH_TAILNET_PORT" "$OPS_SSH_HOST_PORT"

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

apply_env_resource configmap teagramd-runtime \
  "TG_PUBLIC_LINK_PREFIX=$TG_PUBLIC_LINK_PREFIX" \
  "TG_ADVERTISE_ADDR=$TG_ADVERTISE_ADDR" \
  "TG_MEDIA_KEY_CURRENT=$TG_MEDIA_KEY_CURRENT"
apply_env_resource configmap link-edge-runtime \
  "TG_INVITE_WEB_SIGNUP_URL=$TG_INVITE_WEB_SIGNUP_URL" \
  "TG_INVITE_DESKTOP_URL=$TG_INVITE_DESKTOP_URL"
apply_env_resource secret postgres-credentials \
  "POSTGRES_PASSWORD=$POSTGRES_PASSWORD" \
  "TG_POSTGRES_DSN=$TG_POSTGRES_DSN"
apply_env_resource secret rustfs-credentials \
  "RUSTFS_ACCESS_KEY=$RUSTFS_ACCESS_KEY" \
  "RUSTFS_SECRET_KEY=$RUSTFS_SECRET_KEY"
apply_env_resource secret teagramd-blob-credentials \
  "TG_BLOB_S3_ACCESS_KEY_ID=$TG_BLOB_S3_ACCESS_KEY_ID" \
  "TG_BLOB_S3_SECRET_ACCESS_KEY=$TG_BLOB_S3_SECRET_ACCESS_KEY"
apply_env_resource secret cloudflared-token "token=$CLOUDFLARED_TOKEN"
incus exec "$SOURCE_INSTANCE" -- docker cp teagram-teagramd-1:"$MEDIA_KEYS_PATH/." - |
  python3 -c '
import base64
import json
import re
import sys
import tarfile

data = {}
with tarfile.open(fileobj=sys.stdin.buffer, mode="r|*") as archive:
    for member in archive:
        name = member.name.rsplit("/", 1)[-1]
        if member.isfile() and re.fullmatch(r"[0-9]+\.key", name):
            if member.size < 1 or member.size > 65536 or name in data:
                print("k3s bootstrap: a live media key is duplicated or has an invalid size", file=sys.stderr)
                raise SystemExit(1)
            data[name] = base64.b64encode(archive.extractfile(member).read()).decode()
if "1.key" not in data:
    print("k3s bootstrap: the live current media key 1.key is missing", file=sys.stderr)
    raise SystemExit(1)
secret = {
    "apiVersion": "v1",
    "kind": "Secret",
    "metadata": {"name": "teagramd-media-keys", "namespace": "teagram"},
    "type": "Opaque",
    "data": data,
}
json.dump(secret, sys.stdout, separators=(",", ":"))
' | apply_manifest secret/teagramd-media-keys

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
import os
import pathlib
import stat
import sys
import tempfile
import base64

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
server = "https://" + sys.argv[1] + ":6443"
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
    if existing != config:
        print("k3s bootstrap: existing deploy kubeconfig does not match this cluster", file=sys.stderr)
        raise SystemExit(1)
    if stat.S_IMODE(path.stat().st_mode) != 0o600:
        os.chmod(path, 0o600)
    raise SystemExit(0)
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
' "$TAILNET_IP" "$KUBECONFIG_PATH"

# The interactive cutover and its cleanup need cluster-admin operations. Keep
# that client certificate separate from the restricted deployer token above.
incus exec "$INSTANCE" -- k3s kubectl config view --raw --flatten --minify --output=json |
  python3 -c '
import base64
import json
import os
import pathlib
import stat
import sys
import tempfile

config = json.load(sys.stdin)
tailnet_ip, path_text = sys.argv[1:]
path = pathlib.Path(path_text)
contexts = config.get("contexts", [])
clusters = config.get("clusters", [])
users = config.get("users", [])
current = config.get("current-context")
if (len(contexts) != 1 or len(clusters) != 1 or len(users) != 1 or
        not current or contexts[0].get("name") != current):
    print("k3s bootstrap: k3s admin kubeconfig is not a single flattened context",
          file=sys.stderr)
    raise SystemExit(1)
context = contexts[0].get("context", {})
cluster_name = context.get("cluster")
user_name = context.get("user")
cluster_entries = [entry["cluster"] for entry in clusters
                   if entry.get("name") == cluster_name]
user_entries = [entry["user"] for entry in users if entry.get("name") == user_name]
if len(cluster_entries) != 1 or len(user_entries) != 1:
    print("k3s bootstrap: k3s admin kubeconfig references are invalid", file=sys.stderr)
    raise SystemExit(1)
cluster, user = cluster_entries[0], user_entries[0]
if cluster.get("server") != "https://127.0.0.1:6443":
    print("k3s bootstrap: k3s admin kubeconfig has an unexpected API endpoint", file=sys.stderr)
    raise SystemExit(1)
for value, description in (
        (cluster.get("certificate-authority-data"), "cluster CA"),
        (user.get("client-certificate-data"), "admin client certificate"),
        (user.get("client-key-data"), "admin client key")):
    try:
        if not value:
            raise ValueError("missing")
        base64.b64decode(value, validate=True)
    except (ValueError, TypeError):
        print("k3s bootstrap: k3s admin kubeconfig has an invalid " + description,
              file=sys.stderr)
        raise SystemExit(1)
if any(field in user for field in ("token", "exec", "auth-provider")):
    print("k3s bootstrap: k3s admin kubeconfig is not certificate-authenticated",
          file=sys.stderr)
    raise SystemExit(1)
cluster["server"] = "https://" + tailnet_ip + ":6443"
context["namespace"] = "teagram"
payload = json.dumps(config, separators=(",", ":")) + "\n"
if path.is_symlink() or (path.exists() and
        (not path.is_file() or path.stat().st_uid != 0)):
    print("k3s bootstrap: cutover kubeconfig is not a root-owned regular file",
          file=sys.stderr)
    raise SystemExit(1)
if path.exists():
    try:
        existing = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        print("k3s bootstrap: existing cutover kubeconfig is unreadable",
              file=sys.stderr)
        raise SystemExit(1)
    if existing != config:
        print("k3s bootstrap: existing cutover kubeconfig does not match this cluster",
              file=sys.stderr)
        raise SystemExit(1)
    if stat.S_IMODE(path.stat().st_mode) != 0o600:
        os.chmod(path, 0o600)
else:
    fd, temporary = tempfile.mkstemp(prefix=".teagram-k3s-cutover-kubeconfig.",
                                     dir=str(path.parent))
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
' "$TAILNET_IP" "$CUTOVER_KUBECONFIG_PATH"

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
expect_deployer_can_i yes patch services --namespace teagram
expect_deployer_can_i yes create configmaps --namespace teagram
expect_deployer_can_i yes get persistentvolumeclaims --namespace teagram
expect_deployer_can_i yes list pods --namespace teagram
expect_deployer_can_i yes get pods --subresource=log --namespace teagram
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
[[ $(stat -c '%a:%u' "$CUTOVER_KUBECONFIG_PATH") == 600:0 ]] ||
  fail 'the cutover kubeconfig is not root-only'

printf 'k3s bootstrap complete: version=%s api=%s:%s namespace=%s deploy_kubeconfig=%s cutover_kubeconfig=%s (both mode 0600)\n' \
  "$K3S_VERSION" "$TAILNET_IP" "$K3S_API_TAILNET_PORT" "$NAMESPACE" \
  "$KUBECONFIG_PATH" "$CUTOVER_KUBECONFIG_PATH"
