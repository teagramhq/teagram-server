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
readonly PORT_FORWARD_UNIT=teagram-linkselector-port-forward.service
readonly PORT_FORWARD_PORT=18081
readonly POSTGRES_DUMP_DIR=/var/lib/postgresql/cutover-backups
readonly CLOUDFLARE_BEFORE=http://selector:8081
readonly CLOUDFLARE_AFTER=http://linkselector:8081
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
Would wait for PostgreSQL and RustFS, save a mode-0600 pg_dump on the PostgreSQL PVC, restore it into k3s, then run the pinned Atlas migration Job.
Would check that the 8 GiB PostgreSQL PVC can hold both the protected dump and restored database with at least 1 GiB of free space left.
Would stream server_key.pem, enc_key.hex, and local blobs from the old container into the new PVCs without a host-side plaintext copy; key hashes are compared silently.
Would verify the old blob tree and its encrypted envelopes fit their PVCs and the target disk can hold both copies, encrypt and checksum-copy its objects to RustFS with the migrated media key, and keep a read-only source copy for recovery.
Would start teagramd, linklanding, and linkselector; install a restartable, journal-logged port-forward in teagram-k3s and verify it from both LXCs.
Would save the old Serve table, change only HTTPS / to the k3s linkselector, and verify the other four handlers and Funnel-off state.
Would stop only teagram-web-cloudflared on the old LXC, print the Cloudflare dashboard change http://selector:8081 -> http://linkselector:8081, wait for operator confirmation, then scale k8s cloudflared to 1.
Would leave all old Compose data and workloads running, with the old connector stopped; retain the database dump and recovery evidence for at least seven days.
Rollback order: k8s cloudflared=0, Cloudflare route back to http://selector:8081, start teagram-web-cloudflared, restore only Serve / to http://127.0.0.1:8081, stop the new port-forward.
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

for command_name in incus kubectl readlink stat install sha256sum python3 awk grep tar flock date; do
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

serve_status_file() {
  local destination=$1
  incus exec "$SOURCE_INSTANCE" -- tailscale serve status --json > "$destination"
  chmod 0600 "$destination"
}

check_serve_status() {
  local status_file=$1 expected_root=$2
  python3 -c '
import json
import pathlib
import sys

status = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
expected_root = sys.argv[2]
expected = {
    "/": expected_root,
    "/apiws": "http://127.0.0.1:2444/apiws",
    "/admin": "http://127.0.0.1:2445/admin",
    "/.well-known/teagramd/client": "http://127.0.0.1:2444/.well-known/teagramd/client",
    "/.well-known/telegramd/client": "http://127.0.0.1:2444/.well-known/telegramd/client",
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
' "$status_file" "$expected_root"
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
print_rollback_on_failure() {
  local status=$?
  [[ -z ${unit_file:-} ]] || rm -f -- "$unit_file"
  if (( status != 0 )) && [[ -f $PHASE_FILE ]]; then
    print_rollback_command >&2
  fi
}
trap print_rollback_on_failure EXIT

rollback() {
  local deployment_exists=false current_serve target_serve
  current_serve=$(mktemp)
  serve_status_file "$current_serve"
  if ! check_serve_status "$current_serve" http://127.0.0.1:8081 2>/dev/null; then
    [[ -f $EVIDENCE_DIR/serve-target && ! -L $EVIDENCE_DIR/serve-target ]] ||
      fail 'Tailscale Serve does not match the old root, and no verified cutover root was recorded'
    target_serve=$(<"$EVIDENCE_DIR/serve-target")
    [[ $target_serve =~ ^http://([0-9]{1,3}\.){3}[0-9]{1,3}:$PORT_FORWARD_PORT$ ]] ||
      fail 'recorded Tailscale Serve cutover root is invalid'
    check_serve_status "$current_serve" "$target_serve"
  fi
  rm -f -- "$current_serve"

  if kubectl get deployment cloudflared --namespace "$NAMESPACE" >/dev/null 2>&1; then
    deployment_exists=true
    kubectl scale deployment/cloudflared --replicas=0 --namespace "$NAMESPACE" >/dev/null
    kubectl rollout status deployment/cloudflared --namespace "$NAMESPACE" --timeout=180s >/dev/null 2>&1 || true
  fi

  printf 'Cloudflare dashboard route: %s -> %s\n' "$CLOUDFLARE_AFTER" "$CLOUDFLARE_BEFORE" >&2
  confirm ROUTE-RESTORED 'If the route is already on the before value, leave it there. Otherwise restore the before value now.'

  incus exec "$SOURCE_INSTANCE" -- docker start teagram-web-cloudflared >/dev/null
  incus exec "$SOURCE_INSTANCE" -- tailscale serve --bg --https=443 --set-path=/ \
    http://127.0.0.1:8081 >/dev/null
  if incus exec "$TARGET_INSTANCE" -- systemctl cat "$PORT_FORWARD_UNIT" >/dev/null 2>&1; then
    incus exec "$TARGET_INSTANCE" -- systemctl disable --now "$PORT_FORWARD_UNIT" >/dev/null 2>&1 || true
  fi
  local serve_after
  serve_after=$(mktemp)
  serve_status_file "$serve_after"
  check_serve_status "$serve_after" http://127.0.0.1:8081
  rm -f -- "$serve_after"
  record_phase rolled-back
  if [[ $deployment_exists == true ]]; then
    printf 'Rollback complete: the old connector and Serve root are restored; k8s cloudflared is scaled to 0.\n'
  else
    printf 'Rollback complete: the old connector and Serve root are restored.\n'
  fi
}

if [[ $ACTION == --rollback ]]; then
  [[ -f $PHASE_FILE && ! -L $PHASE_FILE ]] || fail 'no recorded cutover phase exists to roll back'
  kubectl get namespace "$NAMESPACE" >/dev/null 2>&1 || fail 'teagram namespace is unavailable; cannot verify the rollback target'
  check_source_container teagram-teagramd-1
  check_source_container teagram-postgres-1
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
check_serve_status "$SERVE_BEFORE" http://127.0.0.1:8081

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
except (tarfile.TarError, OSError, ValueError):
    raise SystemExit("k3s cutover: legacy blob tree contains an unsupported entry")
print(f"{total}\t{encrypted_total}\t{objects}")
') || fail 'could not inspect the source blob volume safely'
[[ $blob_sizes =~ ^[0-9]+$'\t'[0-9]+$'\t'[0-9]+$ ]] || fail 'source blob size is unavailable'
IFS=$'\t' read -r blob_bytes encrypted_blob_bytes blob_objects <<< "$blob_sizes"
(( blob_bytes <= MAX_LEGACY_BLOB_BYTES )) || fail 'source blob volume exceeds the 20 GiB retained PVC'
(( encrypted_blob_bytes <= MAX_ENCRYPTED_BLOB_BYTES )) || fail 'encrypted blob volume exceeds the 18 GiB cutover budget for the 20 GiB RustFS PVC'

kubectl apply --dry-run=server --kustomize "$REPO_ROOT/deploy/k8s/prod-cutover" >/dev/null
record_phase prepared
mkdir -p "$EVIDENCE_DIR"
chmod 0700 "$EVIDENCE_DIR"
install -o root -g root -m 0600 "$SERVE_BEFORE" "$EVIDENCE_DIR/serve-before.json"
rm -f -- "$SERVE_BEFORE"
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

copy_secret_file() {
  local source_path=$1 file_name=$2 source_hash destination_hash
  source_hash=$(incus exec "$SOURCE_INSTANCE" -- docker cp \
    "teagram-teagramd-1:$source_path" - |
    tar -xOf - "$file_name" | sha256sum | awk '{print $1}')
  [[ $source_hash =~ ^[0-9a-f]{64}$ ]] || fail "source key file could not be verified: $file_name"
  incus exec "$SOURCE_INSTANCE" -- docker cp "teagram-teagramd-1:$source_path" - |
    kubectl exec --namespace "$NAMESPACE" --stdin pod/cutover-volume-importer -c importer -- \
      tar -xpf - -C /target/key-data
  destination_hash=$(kubectl exec --namespace "$NAMESPACE" pod/cutover-volume-importer -c importer -- \
    sha256sum "/target/key-data/$file_name" | awk '{print $1}')
  [[ $destination_hash == "$source_hash" ]] || fail "copied identity file did not match its source: $file_name"
}

copy_secret_file /var/lib/teagramd/server_key.pem server_key.pem
copy_secret_file /var/lib/teagramd/enc_key.hex enc_key.hex
incus exec "$SOURCE_INSTANCE" -- docker cp teagram-teagramd-1:/var/lib/teagramd-blobs/. - |
  kubectl exec --namespace "$NAMESPACE" --stdin pod/cutover-volume-importer -c importer -- \
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
unit_file=$(mktemp)
cat > "$unit_file" <<EOF
[Unit]
Description=Loopback-to-k3s linkselector port-forward
After=k3s.service
Requires=k3s.service

[Service]
Type=simple
Environment=KUBECONFIG=/etc/rancher/k3s/k3s.yaml
ExecStart=/usr/local/bin/k3s kubectl port-forward --namespace teagram --address $linkselector_ip service/linkselector $PORT_FORWARD_PORT:8081
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
  "/etc/systemd/system/$PORT_FORWARD_UNIT" < "$unit_file"
incus exec "$TARGET_INSTANCE" -- systemctl daemon-reload
incus exec "$TARGET_INSTANCE" -- systemctl enable --now "$PORT_FORWARD_UNIT" >/dev/null
incus exec "$TARGET_INSTANCE" -- curl --fail --silent --show-error --max-time 10 \
  "http://$linkselector_ip:$PORT_FORWARD_PORT/+k3s-smoke" --output /dev/null
incus exec "$SOURCE_INSTANCE" -- busybox wget -q -O - \
  "http://$linkselector_ip:$PORT_FORWARD_PORT/+k3s-smoke" |
  grep --fixed-strings --quiet 'Open this in Teagram' || fail 'old LXC cannot reach the new selector privately'

record_phase ready-for-edge
printf '%s\n' "http://$linkselector_ip:$PORT_FORWARD_PORT" > "$EVIDENCE_DIR/serve-target"
chmod 0600 "$EVIDENCE_DIR/serve-target"
printf 'Cloudflare dashboard route: %s -> %s\n' "$CLOUDFLARE_BEFORE" "$CLOUDFLARE_AFTER" >&2
incus exec "$SOURCE_INSTANCE" -- tailscale serve --bg --https=443 --set-path=/ \
  "http://$linkselector_ip:$PORT_FORWARD_PORT" >/dev/null
SERVE_AFTER=$(mktemp)
serve_status_file "$SERVE_AFTER"
check_serve_status "$SERVE_AFTER" "http://$linkselector_ip:$PORT_FORWARD_PORT"
install -o root -g root -m 0600 "$SERVE_AFTER" "$EVIDENCE_DIR/serve-after.json"
rm -f -- "$SERVE_AFTER"

record_phase serve-switched
incus exec "$SOURCE_INSTANCE" -- docker stop --time 30 teagram-web-cloudflared >/dev/null
record_phase old-connector-stopped
confirm ROUTE-SWITCHED "Change the Cloudflare tunnel route from $CLOUDFLARE_BEFORE to $CLOUDFLARE_AFTER. The old Compose connector is stopped; this change is manual because no API credential is provisioned."
record_phase route-switched
kubectl scale deployment/cloudflared --replicas=1 --namespace "$NAMESPACE" >/dev/null
kubectl rollout status deployment/cloudflared --namespace "$NAMESPACE" --timeout=600s >/dev/null
[[ $(container_status teagram-web-cloudflared) == exited ]] || fail 'the old Cloudflare connector restarted unexpectedly'
check_source_container teagram-teagramd-1
check_source_container teagram-postgres-1
check_source_container teagram-link-edge-selector-1

record_phase complete
printf 'k3s cutover complete: postgres_dump=%s legacy_blob_bytes=%s encrypted_blob_bytes=%s blob_objects=%s evidence=%s\n' \
  "$dump_path" "$blob_bytes" "$encrypted_blob_bytes" "$blob_objects" "$EVIDENCE_DIR"
print_rollback_command
