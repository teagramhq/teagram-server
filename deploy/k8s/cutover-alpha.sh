#!/usr/bin/env bash
set -euo pipefail
set -f
umask 077

readonly SOURCE_INSTANCE=teagram
readonly TARGET_INSTANCE=teagram-k3s
readonly NAMESPACE=teagram
readonly KUBECONFIG_PATH=/root/teagram-k3s-deploy.kubeconfig
readonly EVIDENCE_DIR=/var/lib/teagram-k3s-cutover
readonly PHASE_FILE=$EVIDENCE_DIR/phase
readonly LINKSELECTOR_PORT_FORWARD_UNIT=teagram-linkselector-port-forward.service
readonly TEAGRAMD_PORT_FORWARD_UNIT=teagramd-port-forward.service
readonly LINKSELECTOR_PORT=18081
readonly WEBSOCKET_PORT=18444
readonly ADMIN_PORT=18445
readonly POSTGRES_DUMP_DIR=/var/lib/postgresql/cutover-backups
readonly CLOUDFLARE_BEFORE=http://selector:8081
readonly CLOUDFLARE_AFTER=http://linkselector:8081
readonly COMPOSE_STOPPED_FILE=$EVIDENCE_DIR/source-compose-stopped
readonly MAX_LEGACY_BLOB_BYTES=$((20 * 1024 * 1024 * 1024))
readonly MAX_ENCRYPTED_BLOB_BYTES=$((18 * 1024 * 1024 * 1024))
readonly POSTGRES_PVC_BYTES=$((8 * 1024 * 1024 * 1024))
readonly MIN_STORAGE_RESERVE_BYTES=$((1024 * 1024 * 1024))

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -- "$SCRIPT_DIR/../.." && pwd)
SCRIPT_PATH=$(readlink -f -- "${BASH_SOURCE[0]}")

fail() {
  printf 'k3s cutover: %s\n' "$*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
Usage: cutover-alpha.sh --dry-run | --apply | --rollback

  --dry-run  Print the complete plan without contacting alpha or the cluster.
  --apply    Run once as root on alpha after bootstrap and before production data is in k3s.
  --rollback Restore the old Tailscale and Cloudflare edges; the old Compose data stays intact.
EOF
}

dry_run() {
  cat <<'EOF'
Would verify alpha, the bootstrap kubeconfig, source Compose containers, the five tailnet-only Serve handlers, required namespace resources, and an unused production namespace.
Would server-dry-run and apply deploy/k8s/prod-cutover: pinned production images, cloudflared=0, teagramd=0, suspended database and encrypted blob migrations, and the state PVCs.
Would wait for PostgreSQL and RustFS, then fence writes before the final PostgreSQL snapshot: stop all running Compose services except PostgreSQL, plus teagram-web-cloudflared, and verify no PostgreSQL client connections remain.
Would treat the planned downtime from the write fence until all edges switch as a maintenance window; any failure before that switch restarts every stopped Compose service.
Would save a mode-0600 pg_dump on the PostgreSQL PVC, restore it into k3s, then run the pinned Atlas migration Job.
Would check that the 8 GiB PostgreSQL PVC can hold both the protected dump and restored database with at least 1 GiB of free space left.
Would stream server_key.pem, enc_key.hex, and local blobs from the old container into the new PVCs without a host-side plaintext copy; key hashes are compared silently.
Would verify the old blob tree and its encrypted envelopes fit their PVCs and the target disk can hold both copies, encrypt and checksum-copy its objects to RustFS with the migrated media key, and keep a read-only source copy for recovery.
Would start teagramd, linklanding, and linkselector; install restartable, journal-logged port-forwards to linkselector, teagramd WebSocket, and admin Services, then verify them from both LXCs.
Would save all five tailnet-only Serve handlers, route / to linkselector and /apiws, /admin, and both discovery paths to the corresponding k3s Services, and verify Funnel remains off.
Would keep k8s cloudflared at zero until the Cloudflare dashboard change http://selector:8081 -> http://linkselector:8081; then start and verify the k3s connector.
Would keep all old Compose services stopped after cutover without modifying their volumes; retain the database dump and recovery evidence for at least seven days.
Rollback order: stop k3s teagramd and cloudflared and verify every k8s cloudflared pod has stopped before restoring routes; restore all five Serve handlers and Cloudflare to the old targets, restart the stopped Compose services, then stop both port-forwards. k3s writes since the switch are not carried back.
EOF
  printf 'Rollback command after review: sudo bash %q --rollback\n' "$SCRIPT_PATH"
}

if [[ $# -ne 1 ]]; then
  usage >&2
  exit 64
fi
case "$1" in
  --dry-run) dry_run; exit 0 ;;
  --apply|--rollback) ACTION=$1 ;;
  --help|-h) usage; exit 0 ;;
  *) usage >&2; exit 64 ;;
esac

[[ $(id -u) -eq 0 ]] || fail "run $ACTION as root on alpha"
[[ $(hostname -s) == alpha ]] || fail "run $ACTION on alpha"
[[ -t 0 ]] || fail "$ACTION requires an interactive terminal for the manual Cloudflare route change"

for command_name in incus kubectl readlink stat install sha256sum python3 awk grep sed tar flock date sleep; do
  command -v "$command_name" >/dev/null 2>&1 || fail "required command is missing: $command_name"
done

export KUBECONFIG=$KUBECONFIG_PATH
[[ -f $KUBECONFIG_PATH && ! -L $KUBECONFIG_PATH ]] || fail 'bootstrap deploy kubeconfig is missing or not a regular file'
[[ $(stat -c '%a:%u' "$KUBECONFIG_PATH") == 600:0 ]] || fail 'bootstrap deploy kubeconfig must be root-owned mode 0600'
incus info "$SOURCE_INSTANCE" >/dev/null 2>&1 || fail 'the old teagram LXC is unavailable'
incus info "$TARGET_INSTANCE" >/dev/null 2>&1 || fail 'the teagram-k3s LXC is unavailable'
[[ $(incus list "$TARGET_INSTANCE" --format csv -c s) == RUNNING ]] || fail 'teagram-k3s is not running'
incus exec "$SOURCE_INSTANCE" -- command -v docker >/dev/null || fail 'docker is unavailable in the old teagram LXC'
incus exec "$SOURCE_INSTANCE" -- command -v tailscale >/dev/null || fail 'tailscale is unavailable in the old teagram LXC'
incus exec "$SOURCE_INSTANCE" -- busybox wget --help >/dev/null 2>&1 || fail 'BusyBox wget is unavailable in the old teagram LXC'
incus exec "$TARGET_INSTANCE" -- command -v systemctl >/dev/null || fail 'systemctl is unavailable in teagram-k3s'
incus exec "$TARGET_INSTANCE" -- command -v k3s >/dev/null || fail 'k3s is unavailable in teagram-k3s'
incus exec "$TARGET_INSTANCE" -- command -v ip >/dev/null || fail 'ip is unavailable in teagram-k3s'
incus exec "$TARGET_INSTANCE" -- command -v awk >/dev/null || fail 'awk is unavailable in teagram-k3s'
incus exec "$TARGET_INSTANCE" -- command -v curl >/dev/null || fail 'curl is unavailable in teagram-k3s'

exec 9>/run/lock/teagram-k3s-cutover.lock
flock -n 9 || fail 'another k3s cutover or rollback is running'

record_phase() {
  local next=$1 temporary
  install -d -o root -g root -m 0700 "$EVIDENCE_DIR"
  temporary=$(mktemp "$EVIDENCE_DIR/.phase.XXXXXX")
  printf '%s\n' "$next" > "$temporary"
  chmod 0600 "$temporary"
  mv -f -- "$temporary" "$PHASE_FILE"
}

container_status() {
  incus exec "$SOURCE_INSTANCE" -- docker inspect --format '{{.State.Status}}' "$1"
}

check_source_container() {
  local container=$1
  [[ $(container_status "$container") == running ]] || fail "old Compose container is not running: $container"
}

record_stopped_compose_container() {
  local container=$1
  if ! grep --fixed-strings --line-regexp --quiet -- "$container" "$COMPOSE_STOPPED_FILE"; then
    printf '%s\n' "$container" >> "$COMPOSE_STOPPED_FILE"
  fi
}

restart_stopped_compose_services() {
  local container status=0 index
  local -a containers=()
  [[ -f $COMPOSE_STOPPED_FILE && ! -L $COMPOSE_STOPPED_FILE ]] ||
    fail 'stopped Compose service evidence is missing; refusing an incomplete rollback'
  [[ $(stat -c '%a:%u' "$COMPOSE_STOPPED_FILE") == 600:0 ]] ||
    fail 'stopped Compose service evidence is not root-owned mode 0600'

  mapfile -t containers < "$COMPOSE_STOPPED_FILE"
  for ((index = ${#containers[@]} - 1; index >= 0; index--)); do
    container=${containers[index]}
    [[ -z $container || $container == teagram-web-cloudflared ]] && continue
    [[ $container =~ ^[[:alnum:]_.-]+$ ]] || {
      printf 'k3s cutover: invalid Compose container name in recovery evidence\n' >&2
      status=1
      continue
    }
    if ! incus exec "$SOURCE_INSTANCE" -- docker start "$container" >/dev/null; then
      printf 'k3s cutover: could not restart a stopped Compose service\n' >&2
      status=1
    fi
  done

  if grep --fixed-strings --line-regexp --quiet -- teagram-web-cloudflared "$COMPOSE_STOPPED_FILE"; then
    if ! incus exec "$SOURCE_INSTANCE" -- docker start teagram-web-cloudflared >/dev/null; then
      printf 'k3s cutover: could not restart the old Cloudflare connector\n' >&2
      status=1
    fi
  fi
  return "$status"
}

verify_no_source_postgres_clients() {
  local connections attempt
  for ((attempt = 1; attempt <= 30; attempt++)); do
    connections=$(incus exec "$SOURCE_INSTANCE" -- docker exec teagram-postgres-1 \
      psql --username=postgres --dbname=postgres --tuples-only --no-align \
        --command="SELECT count(*) FROM pg_stat_activity WHERE datname = 'teagram' AND backend_type = 'client backend' AND pid <> pg_backend_pid()") ||
      fail 'could not verify source PostgreSQL client connections'
    [[ $connections =~ ^[0-9]+$ ]] || fail 'source PostgreSQL client connection count is invalid'
    [[ $connections == 0 ]] && return 0
    sleep 1
  done
  fail 'source PostgreSQL still has client connections after the Compose write fence'
}

fence_source_writes() {
  local running_ids container_id service container_name
  install -d -o root -g root -m 0700 "$EVIDENCE_DIR"
  : > "$COMPOSE_STOPPED_FILE"
  chmod 0600 "$COMPOSE_STOPPED_FILE"

  record_stopped_compose_container teagram-web-cloudflared
  incus exec "$SOURCE_INSTANCE" -- docker stop --time 30 teagram-web-cloudflared >/dev/null

  running_ids=$(incus exec "$SOURCE_INSTANCE" -- docker ps --quiet \
    --filter label=com.docker.compose.project=teagram) || fail 'could not enumerate running Compose services'
  [[ -n $running_ids ]] || fail 'no running teagram Compose services were found for the write fence'
  while IFS= read -r container_id; do
    [[ -n $container_id ]] || continue
    service=$(incus exec "$SOURCE_INSTANCE" -- docker inspect \
      --format '{{ index .Config.Labels "com.docker.compose.service" }}' "$container_id") ||
      fail 'could not identify a running teagram Compose service'
    [[ -n $service ]] || fail 'a running teagram Compose container has no service label'
    [[ $service == postgres ]] && continue
    container_name=$(incus exec "$SOURCE_INSTANCE" -- docker inspect --format '{{.Name}}' "$container_id" | sed 's#^/##') ||
      fail 'could not read a running teagram Compose container name'
    [[ $container_name =~ ^[[:alnum:]_.-]+$ ]] || fail 'a running teagram Compose container has an invalid name'
    record_stopped_compose_container "$container_name"
    incus exec "$SOURCE_INSTANCE" -- docker stop --time 120 "$container_name" >/dev/null ||
      fail 'could not stop every running Compose writer before the PostgreSQL snapshot'
  done <<< "$running_ids"

  check_source_container teagram-postgres-1
  [[ $(container_status teagram-web-cloudflared) == exited ]] ||
    fail 'the old Cloudflare connector is not stopped'
  while IFS= read -r container_name; do
    [[ -z $container_name ]] && continue
    [[ $(container_status "$container_name") == exited ]] ||
      fail 'a Compose service restarted unexpectedly while the source write fence is active'
  done < "$COMPOSE_STOPPED_FILE"
  verify_no_source_postgres_clients
  record_phase fenced
}

capture_admin_token_hash() {
  incus exec "$SOURCE_INSTANCE" -- docker inspect \
    --format '{{range .Config.Env}}{{println .}}{{end}}' teagram-teagramd-1 |
    awk -F= '$1 == "TG_ADMIN_TOKEN_HASH" { print substr($0, index($0, "=") + 1) }'
}

capture_admin_origin() {
  incus exec "$SOURCE_INSTANCE" -- docker inspect \
    --format '{{range .Config.Env}}{{println .}}{{end}}' teagram-teagramd-1 |
    awk -F= '$1 == "TG_ADMIN_ORIGIN" { print substr($0, index($0, "=") + 1) }'
}

install_admin_secret() {
  local admin_hash_file=$1 admin_origin_file=$2 admin_hash admin_origin
  admin_hash=$(capture_admin_token_hash)
  admin_origin=$(capture_admin_origin)
  [[ $admin_hash =~ ^[0-9a-f]{64}$ ]] ||
    fail 'the old teagramd has no valid TG_ADMIN_TOKEN_HASH for the preserved /admin handler'
  printf '%s' "$admin_hash" > "$admin_hash_file"
  printf '%s' "$admin_origin" > "$admin_origin_file"
  chmod 0600 "$admin_hash_file"
  chmod 0600 "$admin_origin_file"
  kubectl create secret generic teagramd-admin --namespace "$NAMESPACE" \
    --from-file=TG_ADMIN_TOKEN_HASH="$admin_hash_file" \
    --from-file=TG_ADMIN_ORIGIN="$admin_origin_file" --dry-run=client --output yaml |
    kubectl apply --filename - >/dev/null
}

serve_status_file() {
  local destination=$1
  incus exec "$SOURCE_INSTANCE" -- tailscale serve status --json > "$destination"
  chmod 0600 "$destination"
}

check_serve_status() {
  local status_file=$1 expected_root=$2 websocket_base=$3 admin_base=$4
  python3 -c '
import json
import pathlib
import sys

status = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
expected_root = sys.argv[2]
websocket_base = sys.argv[3]
admin_base = sys.argv[4]
expected = {
    "/": expected_root,
    "/apiws": websocket_base + "/apiws",
    "/admin": admin_base + "/admin",
    "/.well-known/teagramd/client": websocket_base + "/.well-known/teagramd/client",
    "/.well-known/telegramd/client": websocket_base + "/.well-known/telegramd/client",
}
web = status.get("Web")
if not isinstance(web, dict):
    raise SystemExit("k3s cutover: Tailscale Serve has no Web handler table")
handlers = {}
for entry in web.values():
    if isinstance(entry, dict) and isinstance(entry.get("Handlers"), dict):
        handlers.update(entry["Handlers"])
if set(handlers) != set(expected):
    raise SystemExit("k3s cutover: Tailscale Serve handler paths changed")
for path, target in expected.items():
    handler = handlers[path]
    if not isinstance(handler, dict) or handler.get("Proxy") != target:
        raise SystemExit("k3s cutover: Tailscale Serve handler target changed: " + path)
if "AllowFunnel" not in status or not isinstance(status["AllowFunnel"], dict):
    raise SystemExit("k3s cutover: Tailscale Funnel state is missing or invalid")
funnel = status["AllowFunnel"]
if any(value is not False for value in funnel.values()):
    raise SystemExit("k3s cutover: Tailscale Funnel is enabled or its state is unknown")
' "$status_file" "$expected_root" "$websocket_base" "$admin_base"
}

set_serve_routes() {
  local root=$1 websocket_base=$2 admin_base=$3
  incus exec "$SOURCE_INSTANCE" -- tailscale serve --bg --https=443 --set-path=/ "$root" >/dev/null
  incus exec "$SOURCE_INSTANCE" -- tailscale serve --bg --https=443 --set-path=/apiws \
    "$websocket_base/apiws" >/dev/null
  incus exec "$SOURCE_INSTANCE" -- tailscale serve --bg --https=443 --set-path=/admin \
    "$admin_base/admin" >/dev/null
  incus exec "$SOURCE_INSTANCE" -- tailscale serve --bg --https=443 \
    --set-path=/.well-known/teagramd/client "$websocket_base/.well-known/teagramd/client" >/dev/null
  incus exec "$SOURCE_INSTANCE" -- tailscale serve --bg --https=443 \
    --set-path=/.well-known/telegramd/client "$websocket_base/.well-known/telegramd/client" >/dev/null
}

wait_for_no_pods() {
  local selector=$1 timeout_seconds=$2 elapsed=0 pods
  while (( elapsed < timeout_seconds )); do
    pods=$(kubectl get pods --namespace "$NAMESPACE" --selector="$selector" \
      --output 'jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}') || return 1
    [[ -z $pods ]] && return 0
    sleep 2
    elapsed=$((elapsed + 2))
  done
  return 1
}

stop_k8s_connector() {
  if kubectl get deployment cloudflared --namespace "$NAMESPACE" >/dev/null 2>&1; then
    kubectl scale deployment/cloudflared --replicas=0 --namespace "$NAMESPACE" >/dev/null
    wait_for_no_pods app.kubernetes.io/name=cloudflared 180 ||
      fail 'k8s cloudflared pods did not stop; refusing to change the route or start the old connector'
  fi
}

stop_k8s_application() {
  if kubectl get statefulset teagramd --namespace "$NAMESPACE" >/dev/null 2>&1; then
    kubectl scale statefulset/teagramd --replicas=0 --namespace "$NAMESPACE" >/dev/null
    wait_for_no_pods app.kubernetes.io/name=teagramd 180 ||
      fail 'k8s teagramd pods did not stop; refusing to restart the old Compose writers'
  fi
}

confirm() {
  local expected=$1 prompt=$2 response
  printf '%s\n' "$prompt" >&2
  printf 'Type %s to continue: ' "$expected" >&2
  IFS= read -r response
  [[ $response == "$expected" ]] || fail 'operator did not confirm the exact route state'
}

print_rollback_command() {
  printf 'Rollback command: sudo bash %q --rollback\n' "$SCRIPT_PATH"
}

unit_file=''
admin_hash_file=''
admin_origin_file=''
restore_old_serve() {
  local serve_after
  set_serve_routes http://127.0.0.1:8081 http://127.0.0.1:2444 http://127.0.0.1:2445 || return 1
  serve_after=$(mktemp) || return 1
  if ! serve_status_file "$serve_after" ||
    ! check_serve_status "$serve_after" http://127.0.0.1:8081 http://127.0.0.1:2444 http://127.0.0.1:2445; then
    rm -f -- "$serve_after"
    return 1
  fi
  rm -f -- "$serve_after"
}

stop_port_forward_units() {
  local unit
  for unit in "$LINKSELECTOR_PORT_FORWARD_UNIT" "$TEAGRAMD_PORT_FORWARD_UNIT"; do
    if incus exec "$TARGET_INSTANCE" -- systemctl cat "$unit" >/dev/null 2>&1; then
      incus exec "$TARGET_INSTANCE" -- systemctl disable --now "$unit" >/dev/null || return 1
    fi
  done
}

recover_before_edge_switch() {
  local phase=${1:-} recovery_failed=false route_may_have_changed=false
  if kubectl get deployment cloudflared --namespace "$NAMESPACE" >/dev/null 2>&1; then
    if ! kubectl scale deployment/cloudflared --replicas=0 --namespace "$NAMESPACE" >/dev/null ||
      ! wait_for_no_pods app.kubernetes.io/name=cloudflared 180; then
      printf 'k3s cutover: failed to stop and verify the k8s Cloudflare connector during recovery\n' >&2
      recovery_failed=true
    fi
  fi
  if kubectl get statefulset teagramd --namespace "$NAMESPACE" >/dev/null 2>&1; then
    if ! kubectl scale statefulset/teagramd --replicas=0 --namespace "$NAMESPACE" >/dev/null ||
      ! wait_for_no_pods app.kubernetes.io/name=teagramd 180; then
      printf 'k3s cutover: failed to stop k8s teagramd during recovery\n' >&2
      recovery_failed=true
    fi
  fi

  case "$phase" in
    route-change-requested|route-switched|k8s-connector-started)
      route_may_have_changed=true
      ;;
  esac
  if [[ $phase == serve-switch-started || $phase == serve-switched || $route_may_have_changed == true ]]; then
    if ! restore_old_serve; then
      printf 'k3s cutover: failed to restore the five old Tailscale Serve handlers\n' >&2
      recovery_failed=true
    fi
  fi
  if [[ $route_may_have_changed == true ]]; then
    printf 'Cloudflare dashboard route: %s -> %s\n' "$CLOUDFLARE_AFTER" "$CLOUDFLARE_BEFORE" >&2
    if ! confirm ROUTE-RESTORED 'Restore the Cloudflare route to the before value before Compose services restart.'; then
      printf 'k3s cutover: Cloudflare route restoration was not confirmed\n' >&2
      recovery_failed=true
    fi
  fi
  if ! stop_port_forward_units; then
    printf 'k3s cutover: failed to stop target port-forward units during recovery\n' >&2
    recovery_failed=true
  fi
  if [[ -f $COMPOSE_STOPPED_FILE && $recovery_failed == false ]]; then
    if ! restart_stopped_compose_services; then
      printf 'k3s cutover: one or more old Compose services need manual restart\n' >&2
      recovery_failed=true
    fi
  fi
  if [[ $recovery_failed == true ]]; then
    printf 'k3s cutover: recovery was incomplete; inspect the phase and stopped-service evidence before proceeding\n' >&2
  fi
}

handle_exit() {
  local status=$? phase=''
  trap - EXIT
  [[ -z ${unit_file:-} ]] || rm -f -- "$unit_file"
  [[ -z ${admin_hash_file:-} ]] || rm -f -- "$admin_hash_file"
  [[ -z ${admin_origin_file:-} ]] || rm -f -- "$admin_origin_file"
  if (( status != 0 )) && [[ -f $PHASE_FILE ]]; then
    phase=$(<"$PHASE_FILE")
    if [[ $phase != complete && $phase != rolled-back ]]; then
      recover_before_edge_switch "$phase"
      print_rollback_command >&2
    fi
  fi
  exit "$status"
}
trap handle_exit EXIT

rollback() {
  local current_serve target_root target_websocket target_admin target_ip serve_after
  current_serve=$(mktemp)
  serve_status_file "$current_serve"
  if ! check_serve_status "$current_serve" http://127.0.0.1:8081 \
    http://127.0.0.1:2444 http://127.0.0.1:2445 2>/dev/null; then
    [[ -f $EVIDENCE_DIR/serve-target && ! -L $EVIDENCE_DIR/serve-target ]] ||
      fail 'Tailscale Serve is not on the old routes, and no verified cutover routes were recorded'
    mapfile -t target_routes < "$EVIDENCE_DIR/serve-target"
    [[ ${#target_routes[@]} -eq 3 ]] || fail 'recorded Tailscale Serve cutover routes are incomplete'
    target_root=${target_routes[0]}
    target_websocket=${target_routes[1]}
    target_admin=${target_routes[2]}
    [[ $target_root =~ ^http://(([0-9]{1,3}\.){3}[0-9]{1,3}):$LINKSELECTOR_PORT$ ]] ||
      fail 'recorded Tailscale Serve cutover root is invalid'
    target_ip=${BASH_REMATCH[1]}
    [[ $target_websocket == "http://$target_ip:$WEBSOCKET_PORT" &&
      $target_admin == "http://$target_ip:$ADMIN_PORT" ]] ||
      fail 'recorded Tailscale Serve cutover service addresses are invalid'
    check_serve_status "$current_serve" "$target_root" "$target_websocket" "$target_admin"
  fi
  rm -f -- "$current_serve"

  stop_k8s_connector
  stop_k8s_application
  set_serve_routes http://127.0.0.1:8081 http://127.0.0.1:2444 http://127.0.0.1:2445
  serve_after=$(mktemp)
  serve_status_file "$serve_after"
  check_serve_status "$serve_after" http://127.0.0.1:8081 http://127.0.0.1:2444 http://127.0.0.1:2445
  rm -f -- "$serve_after"

  printf 'Cloudflare dashboard route: %s -> %s\n' "$CLOUDFLARE_AFTER" "$CLOUDFLARE_BEFORE" >&2
  confirm ROUTE-RESTORED 'If the route is already on the before value, leave it there. Otherwise restore the before value now.'
  stop_port_forward_units || fail 'could not stop all target port-forwards before restarting Compose'
  if [[ -f $COMPOSE_STOPPED_FILE ]]; then
    restart_stopped_compose_services
  fi
  check_source_container teagram-teagramd-1
  check_source_container teagram-postgres-1
  check_source_container teagram-web-cloudflared
  record_phase rolled-back
  printf 'Rollback complete: all five Serve handlers, the old connector, and stopped Compose services are restored. k3s writes since the switch are not carried back.\n'
}

if [[ $ACTION == --rollback ]]; then
  [[ -f $PHASE_FILE && ! -L $PHASE_FILE ]] || fail 'no recorded cutover phase exists to roll back'
  kubectl get namespace "$NAMESPACE" >/dev/null 2>&1 || fail 'teagram namespace is unavailable; cannot verify the rollback target'
  check_source_container teagram-postgres-1
  trap - EXIT
  rollback
  exit 0
fi

[[ ! -e $PHASE_FILE ]] || fail 'a prior cutover phase exists; inspect the evidence and use --rollback before attempting another run'
for resource in \
  statefulset/postgres statefulset/teagramd statefulset/rustfs \
  deployment/cloudflared deployment/linkselector deployment/linklanding \
  service/postgres service/postgres-headless service/rustfs service/rustfs-headless \
  service/teagramd service/teagramd-headless service/linkselector service/linklanding \
  service/cloudflared-metrics pod/cutover-volume-importer; do
  if kubectl get "$resource" --namespace "$NAMESPACE" >/dev/null 2>&1; then
    fail "target production workload already exists: $resource"
  fi
done
for selector in app.kubernetes.io/name=db-migrations app.kubernetes.io/name=rustfs-init app.kubernetes.io/name=local-blob-migration; do
  existing_jobs=$(kubectl get jobs --namespace "$NAMESPACE" --selector="$selector" --output name)
  [[ -z $existing_jobs ]] || fail "target production Job already exists: $selector"
done
for claim in data-postgres-0 data-rustfs-0 key-data-teagramd-0 legacy-blobs-teagramd-0 blob-mode-teagramd-0; do
  if kubectl get pvc "$claim" --namespace "$NAMESPACE" >/dev/null 2>&1; then
    fail "target cutover PVC already exists: $claim"
  fi
done
kubectl get namespace "$NAMESPACE" >/dev/null 2>&1 || fail 'the bootstrap teagram namespace is missing'
for secret in postgres-credentials rustfs-credentials teagramd-blob-credentials teagramd-media-keys cloudflared-token ghcr-pull; do
  kubectl get secret "$secret" --namespace "$NAMESPACE" >/dev/null 2>&1 || fail "required namespace Secret is missing: $secret"
done
for configmap in teagramd-runtime link-edge-runtime; do
  kubectl get configmap "$configmap" --namespace "$NAMESPACE" >/dev/null 2>&1 || fail "required namespace ConfigMap is missing: $configmap"
done

check_source_container teagram-teagramd-1
check_source_container teagram-postgres-1
check_source_container teagram-link-edge-selector-1
check_source_container teagram-web-cloudflared
incus exec "$SOURCE_INSTANCE" -- docker exec teagram-teagramd-1 test -s /var/lib/teagramd/server_key.pem
incus exec "$SOURCE_INSTANCE" -- docker exec teagram-teagramd-1 test -s /var/lib/teagramd/enc_key.hex
incus exec "$SOURCE_INSTANCE" -- docker exec teagram-teagramd-1 test -d /var/lib/teagramd-blobs

SERVE_BEFORE=$(mktemp)
serve_status_file "$SERVE_BEFORE"
check_serve_status "$SERVE_BEFORE" http://127.0.0.1:8081 http://127.0.0.1:2444 http://127.0.0.1:2445

record_phase prepared
install -d -o root -g root -m 0700 "$EVIDENCE_DIR"
install -o root -g root -m 0600 "$SERVE_BEFORE" "$EVIDENCE_DIR/serve-before.json"
rm -f -- "$SERVE_BEFORE"
if kubectl get secret teagramd-admin --namespace "$NAMESPACE" >/dev/null 2>&1; then
  fail 'target teagramd-admin Secret already exists; refusing to overwrite the cutover identity'
fi
admin_hash_file=$(mktemp)
admin_origin_file=$(mktemp)
install_admin_secret "$admin_hash_file" "$admin_origin_file"
rm -f -- "$admin_hash_file" "$admin_origin_file"
admin_hash_file=''
admin_origin_file=''
kubectl apply --dry-run=server --kustomize "$REPO_ROOT/deploy/k8s/prod-cutover" >/dev/null
record_phase applying
kubectl apply --kustomize "$REPO_ROOT/deploy/k8s/prod-cutover" >/dev/null

kubectl rollout status statefulset/postgres --namespace "$NAMESPACE" --timeout=600s >/dev/null
kubectl rollout status statefulset/rustfs --namespace "$NAMESPACE" --timeout=600s >/dev/null
kubectl rollout status deployment/linklanding --namespace "$NAMESPACE" --timeout=300s >/dev/null
kubectl rollout status deployment/linkselector --namespace "$NAMESPACE" --timeout=300s >/dev/null
kubectl wait --for=condition=complete jobs \
  --selector=app.kubernetes.io/name=rustfs-init --namespace "$NAMESPACE" --timeout=600s >/dev/null
[[ $(kubectl get deployment cloudflared --namespace "$NAMESPACE" -o jsonpath='{.spec.replicas}') == 0 ]] ||
  fail 'k8s cloudflared must remain at zero replicas until the edge switch'

fence_source_writes

blob_sizes=$(incus exec "$SOURCE_INSTANCE" -- docker cp \
  teagram-teagramd-1:/var/lib/teagramd-blobs/. - |
  python3 -c '
import sys
import tarfile
from pathlib import PurePosixPath

total = 0
encrypted_total = 0
objects = 0
chunk_size = 16 * 1024
try:
    with tarfile.open(fileobj=sys.stdin.buffer, mode="r|*") as archive:
        for member in archive:
            path = PurePosixPath(member.name)
            if path.is_absolute() or any(part == ".." for part in path.parts):
                raise ValueError("unsafe path")
            if member.isdir():
                continue
            if not member.isfile() or member.size < 0:
                raise ValueError("non-regular entry")
            total += member.size
            chunks = max(1, (member.size + chunk_size - 1) // chunk_size)
            encrypted_total += 102 + member.size + 16 * chunks
            objects += 1
    while sys.stdin.buffer.read(1024 * 1024):
        pass
except (tarfile.TarError, OSError, ValueError):
    raise SystemExit("k3s cutover: legacy blob tree contains an unsupported entry")
print(f"{total}\t{encrypted_total}\t{objects}")
') || fail 'could not inspect the source blob volume safely after the write fence'
[[ $blob_sizes =~ ^[0-9]+$'\t'[0-9]+$'\t'[0-9]+$ ]] || fail 'source blob size is unavailable'
IFS=$'\t' read -r blob_bytes encrypted_blob_bytes blob_objects <<< "$blob_sizes"
(( blob_bytes <= MAX_LEGACY_BLOB_BYTES )) || fail 'source blob volume exceeds the 20 GiB retained PVC'
(( encrypted_blob_bytes <= MAX_ENCRYPTED_BLOB_BYTES )) || fail 'encrypted blob volume exceeds the 18 GiB cutover budget for the 20 GiB RustFS PVC'

kubectl exec --namespace "$NAMESPACE" postgres-0 -c postgres -- pg_isready -U postgres -d teagram >/dev/null
source_database_bytes=$(incus exec "$SOURCE_INSTANCE" -- docker exec teagram-postgres-1 \
  psql --username=postgres --dbname=postgres --tuples-only --no-align \
    --command="SELECT pg_database_size('teagram')")
[[ $source_database_bytes =~ ^[0-9]+$ ]] || fail 'could not estimate the source PostgreSQL database size'
(( source_database_bytes > 0 )) || fail 'source PostgreSQL database size is empty'
target_used_bytes=$(kubectl exec --namespace "$NAMESPACE" postgres-0 -c postgres -- \
  du -sk /var/lib/postgresql | awk '{ print $1 * 1024 }')
[[ $target_used_bytes =~ ^[0-9]+$ ]] || fail 'could not inspect used space on the target PostgreSQL PVC'
target_available_bytes=$(kubectl exec --namespace "$NAMESPACE" postgres-0 -c postgres -- \
  df -Pk /var/lib/postgresql | awk 'END { print $4 * 1024 }')
[[ $target_available_bytes =~ ^[0-9]+$ ]] || fail 'could not inspect free space on the target PostgreSQL PVC'
(( target_available_bytes > MIN_STORAGE_RESERVE_BYTES )) || fail 'target PostgreSQL PVC has less than 1 GiB free before the restore'
(( target_used_bytes + 2 * source_database_bytes + MIN_STORAGE_RESERVE_BYTES <= POSTGRES_PVC_BYTES )) ||
  fail 'source PostgreSQL database and dump exceed the 8 GiB target PVC budget'
(( source_database_bytes <= (target_available_bytes - MIN_STORAGE_RESERVE_BYTES) / 2 )) ||
  fail 'source PostgreSQL database is too large for the target PVC to retain both its dump and restored data'
dump_stamp=$(date -u +%Y%m%dT%H%M%SZ)
dump_path="$POSTGRES_DUMP_DIR/source-$dump_stamp.dump"
kubectl exec --namespace "$NAMESPACE" postgres-0 -c postgres -- \
  sh -ec 'install -d -m 0700 /var/lib/postgresql/cutover-backups' >/dev/null
# shellcheck disable=SC2016 # The inner sh expands $1 from its argument list.
incus exec "$SOURCE_INSTANCE" -- docker exec teagram-postgres-1 \
  pg_dump --format=custom --no-owner --no-acl --username=postgres --dbname=teagram |
  kubectl exec --namespace "$NAMESPACE" --stdin postgres-0 -c postgres -- \
    sh -ec 'umask 077; cat > "$1"' -- "$dump_path"
kubectl exec --namespace "$NAMESPACE" postgres-0 -c postgres -- \
  chmod 0600 "$dump_path"
[[ $(kubectl exec --namespace "$NAMESPACE" postgres-0 -c postgres -- stat -c '%a:%u' "$dump_path") == 600:70 ]] ||
  fail 'PostgreSQL cutover dump is not mode 0600 and owned by postgres'
dump_bytes=$(kubectl exec --namespace "$NAMESPACE" postgres-0 -c postgres -- stat -c '%s' "$dump_path")
[[ $dump_bytes =~ ^[0-9]+$ ]] || fail 'could not verify the PostgreSQL cutover dump size'
(( dump_bytes > 0 )) || fail 'PostgreSQL cutover dump is empty'
target_available_bytes=$(kubectl exec --namespace "$NAMESPACE" postgres-0 -c postgres -- \
  df -Pk /var/lib/postgresql | awk 'END { print $4 * 1024 }')
[[ $target_available_bytes =~ ^[0-9]+$ ]] || fail 'could not recheck free space on the target PostgreSQL PVC'
target_used_bytes=$(kubectl exec --namespace "$NAMESPACE" postgres-0 -c postgres -- \
  du -sk /var/lib/postgresql | awk '{ print $1 * 1024 }')
[[ $target_used_bytes =~ ^[0-9]+$ ]] || fail 'could not recheck used space on the target PostgreSQL PVC'
(( source_database_bytes + MIN_STORAGE_RESERVE_BYTES <= target_available_bytes )) ||
  fail 'the PostgreSQL dump leaves insufficient headroom to restore the source database'
(( target_used_bytes + source_database_bytes + MIN_STORAGE_RESERVE_BYTES <= POSTGRES_PVC_BYTES )) ||
  fail 'the PostgreSQL dump and restored database exceed the 8 GiB target PVC budget'
# shellcheck disable=SC2016 # The inner sh expands $POSTGRES_PASSWORD and $1 in the pod.
kubectl exec --namespace "$NAMESPACE" postgres-0 -c postgres -- \
  sh -ec 'PGPASSWORD="$POSTGRES_PASSWORD" pg_restore --clean --if-exists --no-owner --no-acl --exit-on-error --username=postgres --dbname=teagram "$1"' \
  -- "$dump_path"
printf '%s\n' "$dump_path" > "$EVIDENCE_DIR/postgres-dump-path"
chmod 0600 "$EVIDENCE_DIR/postgres-dump-path"
record_phase database-restored
target_available_bytes=$(kubectl exec --namespace "$NAMESPACE" postgres-0 -c postgres -- \
  df -Pk /var/lib/postgresql | awk 'END { print $4 * 1024 }')
[[ $target_available_bytes =~ ^[0-9]+$ ]] || fail 'could not inspect target disk space before importing blobs'
(( blob_bytes + encrypted_blob_bytes + MIN_STORAGE_RESERVE_BYTES <= target_available_bytes )) ||
  fail 'target disk cannot hold both the retained local blobs and encrypted RustFS copies with 1 GiB free'

for claim in key-data-teagramd-0 legacy-blobs-teagramd-0 blob-mode-teagramd-0; do
  kubectl wait --for=jsonpath='{.status.phase}'=Bound "pvc/$claim" \
    --namespace "$NAMESPACE" --timeout=600s >/dev/null
done
kubectl wait --for=condition=Ready pod/cutover-volume-importer --namespace "$NAMESPACE" --timeout=300s >/dev/null

# Run a pipeline consumer and then drain whatever it left unread. A consumer
# such as tar stops at the end-of-archive marker; without the drain the
# producer is killed by SIGPIPE (exit 141) while still writing archive padding.
drain_after() {
  local status=0
  "$@" || status=$?
  cat >/dev/null
  return "$status"
}

copy_secret_file() {
  local source_path=$1 file_name=$2 source_hash destination_hash
  source_hash=$(incus exec "$SOURCE_INSTANCE" -- docker cp \
    "teagram-teagramd-1:$source_path" - |
    drain_after tar -xOf - "$file_name" | sha256sum | awk '{print $1}')
  [[ $source_hash =~ ^[0-9a-f]{64}$ ]] || fail "source key file could not be verified: $file_name"
  incus exec "$SOURCE_INSTANCE" -- docker cp "teagram-teagramd-1:$source_path" - |
    drain_after kubectl exec --namespace "$NAMESPACE" --stdin pod/cutover-volume-importer -c importer -- \
      tar -xpf - -C /target/key-data
  destination_hash=$(kubectl exec --namespace "$NAMESPACE" pod/cutover-volume-importer -c importer -- \
    sha256sum "/target/key-data/$file_name" | awk '{print $1}')
  [[ $destination_hash == "$source_hash" ]] || fail "copied identity file did not match its source: $file_name"
}

copy_secret_file /var/lib/teagramd/server_key.pem server_key.pem
copy_secret_file /var/lib/teagramd/enc_key.hex enc_key.hex
incus exec "$SOURCE_INSTANCE" -- docker cp teagram-teagramd-1:/var/lib/teagramd-blobs/. - |
  drain_after kubectl exec --namespace "$NAMESPACE" --stdin pod/cutover-volume-importer -c importer -- \
    tar -xpf - -C /target/legacy-blobs
kubectl exec --namespace "$NAMESPACE" pod/cutover-volume-importer -c importer -- \
  sh -ec 'chown -R 65532:65532 /target/key-data /target/legacy-blobs /target/blob-mode; chmod 0700 /target/key-data /target/blob-mode; chmod 0600 /target/key-data/server_key.pem /target/key-data/enc_key.hex' >/dev/null
kubectl delete pod/cutover-volume-importer --namespace "$NAMESPACE" --wait=true --timeout=300s >/dev/null

migration_jobs=()
while IFS= read -r job; do
  [[ -z $job ]] || migration_jobs+=("$job")
done < <(kubectl get jobs --namespace "$NAMESPACE" \
  --selector=app.kubernetes.io/name=db-migrations --output name)
[[ ${#migration_jobs[@]} -eq 1 ]] || fail 'expected exactly one pinned Atlas migration Job'
kubectl patch "${migration_jobs[0]}" --namespace "$NAMESPACE" --type=merge \
  --patch '{"spec":{"suspend":false}}' >/dev/null
kubectl wait --for=condition=complete "${migration_jobs[0]}" \
  --namespace "$NAMESPACE" --timeout=900s >/dev/null

kubectl patch job/local-blob-migration --namespace "$NAMESPACE" --type=merge \
  --patch '{"spec":{"suspend":false}}' >/dev/null
kubectl wait --for=condition=complete job/local-blob-migration \
  --namespace "$NAMESPACE" --timeout=21600s >/dev/null
migration_summary=$(kubectl logs --namespace "$NAMESPACE" job/local-blob-migration -c migrate |
  sed -n '/^blob_copy=pass mode=encrypted /p')
[[ $migration_summary == blob_copy=pass\ mode=encrypted\ objects=* ]] ||
  fail 'encrypted legacy blob migration did not report a verified copy'

kubectl scale statefulset/teagramd --replicas=1 --namespace "$NAMESPACE" >/dev/null
kubectl rollout status statefulset/teagramd --namespace "$NAMESPACE" --timeout=900s >/dev/null
kubectl rollout status deployment/linklanding --namespace "$NAMESPACE" --timeout=300s >/dev/null
kubectl rollout status deployment/linkselector --namespace "$NAMESPACE" --timeout=300s >/dev/null

# shellcheck disable=SC2016 # The awk program runs in the target LXC's shell.
linkselector_ip=$(incus exec "$TARGET_INSTANCE" -- sh -ec \
  'ip -o -4 addr show dev eth0 scope global | awk "NR == 1 {split(\$4, a, \"/\"); print a[1]}"')
[[ $linkselector_ip =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || fail 'could not identify the private teagram-k3s LXC address'
install_port_forward_unit() {
  local unit=$1 description=$2 target=$3 ports=$4
  unit_file=$(mktemp)
  cat > "$unit_file" <<EOF
[Unit]
Description=$description
After=k3s.service
Requires=k3s.service

[Service]
Type=simple
Environment=KUBECONFIG=/etc/rancher/k3s/k3s.yaml
ExecStart=/usr/local/bin/k3s kubectl port-forward --namespace teagram --address $linkselector_ip $target $ports
Restart=always
RestartSec=5s
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF
  incus exec "$TARGET_INSTANCE" -- install -o root -g root -m 0644 /dev/stdin \
    "/etc/systemd/system/$unit" < "$unit_file"
  rm -f -- "$unit_file"
  unit_file=''
  incus exec "$TARGET_INSTANCE" -- systemctl daemon-reload
  incus exec "$TARGET_INSTANCE" -- systemctl enable --now "$unit" >/dev/null
}

install_port_forward_unit "$LINKSELECTOR_PORT_FORWARD_UNIT" \
  'Loopback-to-k3s linkselector port-forward' service/linkselector "$LINKSELECTOR_PORT:8081"
install_port_forward_unit "$TEAGRAMD_PORT_FORWARD_UNIT" \
  'Tailnet-only teagramd WebSocket and admin port-forward' service/teagramd \
  "$WEBSOCKET_PORT:2444 $ADMIN_PORT:2445"
incus exec "$TARGET_INSTANCE" -- curl --fail --silent --show-error --max-time 10 \
  "http://$linkselector_ip:$LINKSELECTOR_PORT/+k3s-smoke" --output /dev/null
incus exec "$SOURCE_INSTANCE" -- busybox wget -q -O - \
  "http://$linkselector_ip:$LINKSELECTOR_PORT/+k3s-smoke" |
  grep --fixed-strings --quiet 'Open this in Teagram' || fail 'old LXC cannot reach the new selector privately'
websocket_status=$(incus exec "$TARGET_INSTANCE" -- curl --silent --show-error --output /dev/null \
  --write-out '%{http_code}' --max-time 10 "http://$linkselector_ip:$WEBSOCKET_PORT/apiws") ||
  fail 'the new WebSocket Service is not reachable from teagram-k3s'
[[ $websocket_status != 000 ]] || fail 'the new WebSocket Service returned no HTTP response'
incus exec "$SOURCE_INSTANCE" -- busybox wget -q -O - \
  "http://$linkselector_ip:$WEBSOCKET_PORT/.well-known/teagramd/client" |
  grep --fixed-strings --quiet 'rsa_spki' || fail 'the old LXC cannot reach k3s client discovery privately'
admin_status=$(incus exec "$TARGET_INSTANCE" -- curl --silent --show-error --output /dev/null \
  --write-out '%{http_code}' --max-time 10 "http://$linkselector_ip:$ADMIN_PORT/admin") ||
  fail 'the new admin Service is not reachable from teagram-k3s'
[[ $admin_status =~ ^(200|302|401|403)$ ]] || fail 'the new admin Service returned an unexpected HTTP status'

record_phase ready-for-edge
printf '%s\n%s\n%s\n' "http://$linkselector_ip:$LINKSELECTOR_PORT" \
  "http://$linkselector_ip:$WEBSOCKET_PORT" "http://$linkselector_ip:$ADMIN_PORT" > "$EVIDENCE_DIR/serve-target"
chmod 0600 "$EVIDENCE_DIR/serve-target"
printf 'Cloudflare dashboard route: %s -> %s\n' "$CLOUDFLARE_BEFORE" "$CLOUDFLARE_AFTER" >&2
record_phase serve-switch-started
set_serve_routes "http://$linkselector_ip:$LINKSELECTOR_PORT" \
  "http://$linkselector_ip:$WEBSOCKET_PORT" "http://$linkselector_ip:$ADMIN_PORT"
SERVE_AFTER=$(mktemp)
serve_status_file "$SERVE_AFTER"
check_serve_status "$SERVE_AFTER" "http://$linkselector_ip:$LINKSELECTOR_PORT" \
  "http://$linkselector_ip:$WEBSOCKET_PORT" "http://$linkselector_ip:$ADMIN_PORT"
install -o root -g root -m 0600 "$SERVE_AFTER" "$EVIDENCE_DIR/serve-after.json"
rm -f -- "$SERVE_AFTER"

record_phase serve-switched
[[ $(container_status teagram-web-cloudflared) == exited ]] ||
  fail 'the old Cloudflare connector restarted unexpectedly while the k8s connector is still off'
wait_for_no_pods app.kubernetes.io/name=cloudflared 1 ||
  fail 'k8s Cloudflare connector pods exist before the Cloudflare route switch'
record_phase route-change-requested
confirm ROUTE-SWITCHED "Change the Cloudflare tunnel route from $CLOUDFLARE_BEFORE to $CLOUDFLARE_AFTER. The old Compose connector is stopped; this change is manual because no API credential is provisioned."
record_phase route-switched
kubectl scale deployment/cloudflared --replicas=1 --namespace "$NAMESPACE" >/dev/null
kubectl rollout status deployment/cloudflared --namespace "$NAMESPACE" --timeout=600s >/dev/null
[[ $(container_status teagram-web-cloudflared) == exited ]] || fail 'the old Cloudflare connector restarted unexpectedly'
check_source_container teagram-postgres-1
record_phase k8s-connector-started

record_phase complete
printf 'k3s cutover complete: postgres_dump=%s legacy_blob_bytes=%s encrypted_blob_bytes=%s blob_objects=%s evidence=%s\n' \
  "$dump_path" "$blob_bytes" "$encrypted_blob_bytes" "$blob_objects" "$EVIDENCE_DIR"
print_rollback_command
