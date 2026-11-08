#!/usr/bin/env bash
set -euo pipefail
set -f
umask 077

readonly SOURCE_INSTANCE=teagram
readonly TARGET_INSTANCE=teagram-k3s
TEST_MODE=false
if [[ ${2:-} == --test-mode ]]; then
  TEST_MODE=true
  # Name the failing command in the disposable-cluster log; many checks exit
  # under set -e without a message of their own.
  set -E
  trap 'printf "k3s cutover: command failed at line %s: %s\n" "$LINENO" "$BASH_COMMAND" >&2' ERR
  readonly NAMESPACE=teagram-cutover
  readonly KUBECONFIG_PATH=${KUBECONFIG:?--test-mode requires a disposable KUBECONFIG}
  readonly EVIDENCE_DIR=${RUNNER_TEMP:?--test-mode requires RUNNER_TEMP}/teagram-cutover-runner
  readonly LOCK_PATH=$EVIDENCE_DIR/cutover.lock
else
  readonly NAMESPACE=teagram
  readonly KUBECONFIG_PATH=/root/teagram-k3s-cutover.kubeconfig
  readonly EVIDENCE_DIR=/var/lib/teagram-k3s-cutover
  readonly LOCK_PATH=/run/lock/teagram-k3s-cutover.lock
fi
readonly PHASE_FILE=$EVIDENCE_DIR/phase
readonly LINKSELECTOR_PORT_FORWARD_UNIT=teagram-linkselector-port-forward.service
readonly TEAGRAMD_PORT_FORWARD_UNIT=teagramd-port-forward.service
readonly RUSTFS_PORT_FORWARD_UNIT=teagram-rustfs-port-forward.service
readonly LINKSELECTOR_PORT=18081
readonly WEBSOCKET_PORT=18444
readonly ADMIN_PORT=18445
readonly MTPROTO_PORT=2443
readonly BOT_API_PORT=2447
readonly RUSTFS_PORT=19000
readonly POSTGRES_DUMP_DIR=/var/lib/postgresql/cutover-backups
readonly CLOUDFLARE_BEFORE=http://selector:8081
readonly CLOUDFLARE_AFTER=http://linkselector:8081
readonly COMPOSE_STOPPED_FILE=$EVIDENCE_DIR/source-compose-stopped
readonly MAX_LEGACY_BLOB_BYTES=$((20 * 1024 * 1024 * 1024))
readonly MAX_ENCRYPTED_BLOB_BYTES=$((18 * 1024 * 1024 * 1024))
readonly POSTGRES_PVC_BYTES=$((8 * 1024 * 1024 * 1024))
readonly MIN_STORAGE_RESERVE_BYTES=$((1024 * 1024 * 1024))
MC_HELPER_CONTAINER=''
APP_IMAGE=''
MC_IMAGE=''
MIGRATIONS_JOB_NAME=''
REQUIRED_MIGRATION_VERSION=''
LIVE_SOURCE_REVISION=''
PINNED_SOURCE_REVISION=''
target_lxc_ip=''
MC_CLIENT_POD_CREATED=false
S3_INVENTORY_SCRATCH=''

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
       cutover-alpha.sh --apply|--rollback --test-mode (disposable CI namespace only)

  --dry-run  Print the complete plan without contacting alpha or the cluster.
  --apply    Run once as root on alpha after bootstrap and before production data is in k3s.
  --rollback Restore the old Tailscale and Cloudflare edges; the old Compose data stays intact.
EOF
}

dry_run() {
  cat <<'EOF'
Would verify alpha, the root-only cutover kubeconfig, source Compose containers and encrypted S3 authority, the live teagramd and migration image revisions, required migration version and Job name, the five tailnet-only HTTPS handlers, the old MTProto and Bot API TCP routes, required namespace resources, and an unused production namespace.
Would server-dry-run and apply deploy/k8s/prod-cutover: pinned production and teagram-web images, cloudflared=0, teagramd=0, suspended database migrations, the web Service, and the state PVCs.
Would create the teagramd-admin Secret from the old Compose environment without printing its values.
Would use a temporary importer pod to prepare PVC ownership and permissions, copy state, and remove the pod.
Would keep private phase, Serve route, and recovery evidence under /var/lib/teagram-k3s-cutover.
Would wait for PostgreSQL and RustFS, install a target-only RustFS port-forward, then fence writes before the final PostgreSQL snapshot: stop all running Compose services except PostgreSQL and RustFS, plus teagram-web-cloudflared; keep source RustFS available for the post-fence S3 listing and mirror, and verify no PostgreSQL client connections remain.
Would treat the planned downtime from the write fence until all edges switch as a maintenance window; any failure before the first live edge switch restarts only the Compose services stopped by the runner, while PostgreSQL and RustFS stay running.
Would save a mode-0600 pg_dump on the PostgreSQL PVC (spooled through a private root-only file, sent over a temporary loopback port-forward, and checked by SHA-256 before the restore, then the spool is removed), restore it into k3s, then run the pinned Atlas migration Job.
Would check that the 8 GiB PostgreSQL PVC can hold both the protected dump and restored database with at least 1 GiB of free space left.
Would stream server_key.pem, enc_key.hex, the blob-mode authority, and the retained local blob volume from the old container into the new PVCs without a host-side plaintext copy; compare the target Secret against the live encrypted-media key set without printing key values.
Would mirror the already-encrypted source S3 objects into the new RustFS bucket without changing their bytes, then compare the object count, total bytes, and SHA-256 of every source and target object before switching routes.
Would start teagramd, teagram-web, linklanding, and linkselector; install restartable, journal-logged port-forwards to linkselector, MTProto TCP, WebSocket, admin, and Bot API Services, then verify them from both LXCs.
Would save the five tailnet-only HTTPS handlers, the advertised MTProto TCP route on port 2443, and the Bot API TCP route on port 2447; route / to linkselector, preserve the WebSocket, admin, and discovery paths, and enable PROXY protocol on the k3s Bot API relay. Funnel remains off.
Would keep k8s cloudflared at zero until the Cloudflare dashboard change http://selector:8081 -> http://linkselector:8081; then start and verify the k3s connector.
Would leave the old Compose application and edge services stopped after cutover; PostgreSQL and source RustFS stay running with their existing data. Retain the source RustFS bucket, old Compose volumes, database dump, and recovery evidence for at least seven days; the runner does not stop or remove PostgreSQL or RustFS.
Rollback order: stop k3s teagramd and cloudflared and verify every k8s cloudflared pod has stopped before restoring routes; restore all five HTTPS handlers, both TCP routes including the old Bot API route, and Cloudflare to the old targets, restart only the Compose services stopped by apply, then stop both port-forwards. PostgreSQL and source RustFS are already running; k3s writes since the switch are not copied back to the source bucket.
After the first live Serve route, a failure requires explicit rollback after the first live Serve route; automatic recovery will not switch clients back to Compose and discard k3s writes.
EOF
  printf 'Rollback command after review: sudo bash %q --rollback\n' "$SCRIPT_PATH"
}

if [[ $# -lt 1 || $# -gt 2 || ( $# -eq 2 && ${2:-} != --test-mode ) ]]; then
  usage >&2
  exit 64
fi
case "$1" in
  --dry-run) dry_run; exit 0 ;;
  --apply|--rollback) ACTION=$1 ;;
  --help|-h) usage; exit 0 ;;
  *) usage >&2; exit 64 ;;
esac

if [[ $TEST_MODE == false ]]; then
  [[ $(id -u) -eq 0 ]] || fail "run $ACTION as root on alpha"
  [[ $(hostname -s) == alpha ]] || fail "run $ACTION on alpha"
  [[ -t 0 ]] || fail "$ACTION requires an interactive terminal for the manual Cloudflare route change"
fi

for command_name in incus kubectl readlink stat install sha256sum python3 awk grep sed tar flock date sleep wc; do
  command -v "$command_name" >/dev/null 2>&1 || fail "required command is missing: $command_name"
done

export KUBECONFIG=$KUBECONFIG_PATH
[[ -f $KUBECONFIG_PATH && ! -L $KUBECONFIG_PATH ]] || fail 'root-only cutover kubeconfig is missing or not a regular file'
if [[ $TEST_MODE == false ]]; then
  [[ $(stat -c '%a:%u' "$KUBECONFIG_PATH") == 600:0 ]] || fail 'root-only cutover kubeconfig must be root-owned mode 0600'
else
  [[ $(stat -c '%a' "$KUBECONFIG_PATH") == 600 ]] || fail 'disposable test kubeconfig must have mode 0600'
fi
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

if [[ $TEST_MODE == true ]]; then
  [[ $(kubectl config view --minify --output 'jsonpath={.clusters[0].cluster.server}') == https://127.0.0.1:6443 ]] ||
    fail 'test mode is restricted to the loopback disposable k3s API'
  [[ $(kubectl get namespace "$SOURCE_INSTANCE" --output 'jsonpath={.metadata.labels.cutover-test}') == true ]] ||
    fail 'test mode requires the source namespace to carry cutover-test=true'
  [[ $(kubectl get namespace "$NAMESPACE" --output 'jsonpath={.metadata.labels.cutover-test}') == true ]] ||
    fail 'test mode requires the target namespace to carry cutover-test=true'
fi

if [[ $TEST_MODE == true ]]; then
  install -d -m 0700 "$EVIDENCE_DIR"
fi
exec 9>"$LOCK_PATH"
flock -n 9 || fail 'another k3s cutover or rollback is running'

record_phase() {
  local next=$1 temporary
  ensure_private_dir "$EVIDENCE_DIR"
  temporary=$(mktemp "$EVIDENCE_DIR/.phase.XXXXXX")
  printf '%s\n' "$next" > "$temporary"
  chmod 0600 "$temporary"
  mv -f -- "$temporary" "$PHASE_FILE"
}

ensure_private_dir() {
  if [[ $TEST_MODE == true ]]; then
    install -d -m 0700 "$1"
  else
    install -d -o root -g root -m 0700 "$1"
  fi
}

install_private_file() {
  local mode=$1 source=$2 destination=$3
  if [[ $TEST_MODE == true ]]; then
    install -m "$mode" "$source" "$destination"
  else
    install -o root -g root -m "$mode" "$source" "$destination"
  fi
}

verify_private_file() {
  local path=$1 mode=$2 owner
  if [[ $TEST_MODE == true ]]; then
    [[ $(stat -c '%a' "$path") == "$mode" ]]
  else
    owner=$(stat -c '%a:%u' "$path")
    [[ $owner == "$mode:0" ]]
  fi
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
  verify_private_file "$COMPOSE_STOPPED_FILE" 600 ||
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
        --command="SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid()") ||
      fail 'could not verify source PostgreSQL client connections'
    [[ $connections =~ ^[0-9]+$ ]] || fail 'source PostgreSQL client connection count is invalid'
    [[ $connections == 0 ]] && return 0
    sleep 1
  done
  fail 'source PostgreSQL still has client connections after the Compose write fence'
}

fence_source_writes() {
  local running_ids container_id service container_name
  ensure_private_dir "$EVIDENCE_DIR"
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
    [[ $service == postgres || $service == rustfs ]] && continue
    container_name=$(incus exec "$SOURCE_INSTANCE" -- docker inspect --format '{{.Name}}' "$container_id" | sed 's#^/##') ||
      fail 'could not read a running teagram Compose container name'
    [[ $container_name =~ ^[[:alnum:]_.-]+$ ]] || fail 'a running teagram Compose container has an invalid name'
    record_stopped_compose_container "$container_name"
    incus exec "$SOURCE_INSTANCE" -- docker stop --time 120 "$container_name" >/dev/null ||
      fail 'could not stop every running Compose writer before the PostgreSQL snapshot'
  done <<< "$running_ids"

  check_source_container teagram-postgres-1
  check_source_container teagram-rustfs-1
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

capture_compose_environment() {
  incus exec "$SOURCE_INSTANCE" -- docker inspect \
    --format '{{range .Config.Env}}{{println .}}{{end}}' teagram-teagramd-1
}

capture_admin_token_hash() {
  capture_compose_environment |
    awk -F= '$1 == "TG_ADMIN_TOKEN_HASH" { print substr($0, index($0, "=") + 1) }'
}

capture_admin_origin() {
  capture_compose_environment |
    awk -F= '$1 == "TG_ADMIN_ORIGIN" { print substr($0, index($0, "=") + 1) }'
}

validate_source_blob_backend() {
  local source_environment
  source_environment=$(capture_compose_environment) ||
    fail 'could not inspect the old teagramd blob configuration'
  if ! awk -F= '
    $1 == "TG_BLOB_S3_ENDPOINT" { endpoint = substr($0, index($0, "=") + 1); endpoint_count++ }
    $1 == "TG_BLOB_S3_BUCKET" { bucket = substr($0, index($0, "=") + 1); bucket_count++ }
    $1 == "TG_BLOB_S3_PREFIX" { prefix = substr($0, index($0, "=") + 1); prefix_count++ }
    $1 == "TG_BLOB_S3_ACCESS_KEY_ID" { access_key = substr($0, index($0, "=") + 1); access_key_count++ }
    $1 == "TG_BLOB_S3_SECRET_ACCESS_KEY_FILE" { secret_file = substr($0, index($0, "=") + 1); secret_file_count++ }
    $1 == "TG_MEDIA_KEY_CURRENT" { media_key_current = substr($0, index($0, "=") + 1); media_key_current_count++ }
    END {
      exit !(endpoint_count == 1 && endpoint == "http://rustfs:9000" &&
        bucket_count == 1 && bucket == "teagram" && prefix_count == 1 && prefix == "teagramd/" &&
        access_key_count == 1 && length(access_key) == 20 && access_key !~ /[^0-9a-f]/ &&
        secret_file_count == 1 && secret_file ~ /^\// &&
        media_key_current_count == 1 && media_key_current ~ /^[1-9][0-9]*$/)
    }
  ' <<< "$source_environment"; then
    fail 'source teagramd must use encrypted S3 at http://rustfs:9000 with bucket teagram and prefix teagramd/'
  fi
}

source_container_revision() {
  incus exec "$SOURCE_INSTANCE" -- docker inspect \
    --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}' teagram-teagramd-1
}

prod_image_digest() {
  local image_name=$1 digest
  digest=$(awk -v name="$image_name" '
    $1 == "-" && $2 == "name:" { in_image = ($3 == name); next }
    in_image && $1 == "digest:" { print $2; exit }
  ' "$REPO_ROOT/deploy/k8s/prod/kustomization.yaml")
  [[ $digest =~ ^sha256:[0-9a-f]{64}$ ]] || fail "production image pin is invalid: $image_name"
  printf '%s' "$digest"
}

prod_literal() {
  local generator=$1 literal_prefix=$2 value
  value=$(awk -v generator="$generator" -v prefix="$literal_prefix" '
    $1 == "-" && $2 == "name:" { in_generator = ($3 == generator); next }
    in_generator && $1 == "-" && index($2, prefix) == 1 {
      sub("^" prefix, "", $2)
      print $2
      exit
    }
  ' "$REPO_ROOT/deploy/k8s/prod/kustomization.yaml")
  [[ -n $value ]] || fail "production $generator pin is missing"
  printf '%s' "$value"
}

source_docker_helper_enabled() {
  [[ $TEST_MODE == false || ${CUTOVER_TEST_SOURCE_DOCKER_HELPER:-} == true ]]
}

resolve_image_revision() {
  local image=$1 revision
  if source_docker_helper_enabled; then
    incus exec "$SOURCE_INSTANCE" -- docker pull --quiet --platform linux/amd64 "$image" >/dev/null 2>&1 ||
      fail "could not resolve pinned image $image"
  fi
  revision=$(incus exec "$SOURCE_INSTANCE" -- docker image inspect "$image" \
    --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}') ||
    fail "could not read pinned image revision: $image"
  [[ $revision =~ ^[0-9a-f]{40}$ ]] || fail "pinned image has an invalid source revision: $image"
  printf '%s' "$revision"
}

image_latest_migration() {
  local image=$1 version
  if [[ $TEST_MODE == true && -n ${CUTOVER_TEST_MIGRATION_VERSION:-} ]] &&
    ! source_docker_helper_enabled; then
    version=$CUTOVER_TEST_MIGRATION_VERSION
  else
    version=$(incus exec "$SOURCE_INSTANCE" -- docker run --rm --network none \
      --entrypoint /bin/sh "$image" -ec \
      'ls -1 /migrations | sed -nE "s/^([0-9]{14})_.+\\.sql$/\\1/p" | sort | tail -n 1' \
      2>/dev/null) || fail "could not inspect the migration version in $image"
  fi
  [[ $version =~ ^[0-9]{14}$ ]] || fail 'pinned migration image has no valid newest migration version'
  printf '%s' "$version"
}

validate_release_for_source() {
  local expected_revision live_revision app_digest migrations_digest helper_digest
  local app_image migrations_image helper_image app_revision migrations_revision helper_revision
  local migrations_validation_image helper_validation_image
  local required_version latest_version migration_job expected_job_suffix
  expected_revision=$(<"$REPO_ROOT/deploy/k8s/prod/source-revision")
  if [[ $TEST_MODE == true && -n ${CUTOVER_TEST_PINNED_REVISION:-} ]]; then
    expected_revision=$CUTOVER_TEST_PINNED_REVISION
  fi
  [[ $expected_revision =~ ^[0-9a-f]{40}$ ]] || fail 'pinned production source revision is invalid'
  live_revision=$(source_container_revision) || fail 'could not read the live teagramd image revision'
  [[ $live_revision =~ ^[0-9a-f]{40}$ ]] || fail "live teagramd image has an invalid source revision: ${live_revision:-<empty>}"
  [[ $live_revision == "$expected_revision" ]] ||
    fail "live teagramd revision $live_revision does not match pinned production revision $expected_revision"
  LIVE_SOURCE_REVISION=$live_revision
  PINNED_SOURCE_REVISION=$expected_revision

  app_digest=$(prod_image_digest ghcr.io/teagramhq/teagramd)
  migrations_digest=$(prod_image_digest ghcr.io/teagramhq/teagram-migrations)
  helper_digest=$(prod_image_digest ghcr.io/teagramhq/teagramd-mc-init)
  app_image="ghcr.io/teagramhq/teagramd@$app_digest"
  migrations_image="ghcr.io/teagramhq/teagram-migrations@$migrations_digest"
  helper_image="ghcr.io/teagramhq/teagramd-mc-init@$helper_digest"
  migrations_validation_image=$migrations_image
  helper_validation_image=$helper_image
  if [[ $TEST_MODE == true ]]; then
    app_image=${CUTOVER_TEST_TEAGRAMD_IMAGE:-$app_image}
    migrations_validation_image=${CUTOVER_TEST_MIGRATIONS_IMAGE:-$migrations_image}
    helper_validation_image=${CUTOVER_TEST_MC_IMAGE:-$helper_image}
  fi
  app_revision=$(resolve_image_revision "$app_image")
  migrations_revision=$(resolve_image_revision "$migrations_validation_image")
  helper_revision=$(resolve_image_revision "$helper_validation_image")
  [[ $app_revision == "$expected_revision" ]] ||
    fail "pinned teagramd image revision $app_revision does not match production revision $expected_revision"
  [[ $migrations_revision == "$expected_revision" ]] ||
    fail "pinned migration image revision $migrations_revision does not match production revision $expected_revision"
  [[ $helper_revision == "$expected_revision" ]] ||
    fail "pinned S3 client image revision $helper_revision does not match production revision $expected_revision"

  required_version=$(prod_literal required-migration-version migration-version=)
  migration_job=$(prod_literal db-migrations-job-revision job-name=)
  expected_job_suffix=${migrations_digest#sha256:}
  if [[ $TEST_MODE == true && -n ${CUTOVER_TEST_MIGRATIONS_JOB_NAME:-} ]]; then
    [[ $CUTOVER_TEST_MIGRATIONS_JOB_NAME == "$migration_job" ]] ||
      fail 'CI migration Job name does not match the committed production Job'
  fi
  [[ $migration_job == "db-migrations-${expected_job_suffix:0:16}" ]] ||
    fail 'production migration Job name does not match its pinned migration image digest'
  latest_version=$(image_latest_migration "$migrations_validation_image")
  [[ $required_version == "$latest_version" ]] ||
    fail "pinned migration version $required_version does not match pinned image newest migration $latest_version"

  if [[ $TEST_MODE == false ]]; then
    REQUIRED_MIGRATION_VERSION=$required_version
    APP_IMAGE=$app_image
    MC_IMAGE=$helper_image
    MIGRATIONS_JOB_NAME=$migration_job
  else
    REQUIRED_MIGRATION_VERSION=$latest_version
    APP_IMAGE=$app_image
    MC_IMAGE=$helper_validation_image
    MIGRATIONS_JOB_NAME=$migration_job
  fi
}

check_source_migration_version() {
  local expected_version=$1 live_version
  live_version=$(incus exec "$SOURCE_INSTANCE" -- docker exec teagram-postgres-1 \
    psql --username=postgres --dbname=teagram --tuples-only --no-align \
      --command="SELECT COALESCE(MAX(version)::text, '') FROM atlas_schema_revisions.atlas_schema_revisions") ||
    fail 'could not read the live database migration version'
  [[ $live_version == "$expected_version" ]] ||
    fail "live database migration version $live_version does not match pinned migration version $expected_version"
}

check_source_blob_authority() {
  incus exec "$SOURCE_INSTANCE" -- docker cp \
    teagram-teagramd-1:/run/teagramd/blob-mode/. - |
    python3 -c '
import json
import pathlib
import re
import sys
import tarfile

files = {}
def strict_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON field")
        result[key] = value
    return result

try:
    with tarfile.open(fileobj=sys.stdin.buffer, mode="r|*") as archive:
        for member in archive:
            path = pathlib.PurePosixPath(member.name)
            parts = tuple(part for part in path.parts if part not in (".", ""))
            if path.is_absolute() or any(part == ".." for part in parts):
                raise ValueError("unsafe path")
            if member.isdir():
                continue
            if not member.isfile() or not parts:
                raise ValueError("unsupported entry")
            if parts == ("mode.json",):
                pass
            elif len(parts) == 2 and parts[0] == "journal" and re.fullmatch(r"[0-9]{10}\.json", parts[1]):
                pass
            else:
                raise ValueError("unsupported entry")
            if member.size <= 0 or member.size > 4096:
                raise ValueError("invalid record size")
            name = "/".join(parts)
            if name in files:
                raise ValueError("duplicate authority file")
            stream = archive.extractfile(member)
            if stream is None:
                raise ValueError("unreadable record")
            content = stream.read()
            if len(content) != member.size:
                raise ValueError("short authority record")
            files[name] = content
    mode = files.get("mode.json")
    journals = sorted(name for name in files if name.startswith("journal/"))
    if mode is None or not journals or len(journals) > 1000:
        raise ValueError("authority records are incomplete")
    records = []
    for index, name in enumerate(journals, 1):
        if name != f"journal/{index:010d}.json":
            raise ValueError("journal sequence is invalid")
        records.append(json.loads(files[name], object_pairs_hook=strict_object))
    current = json.loads(mode, object_pairs_hook=strict_object)
    if not isinstance(current, dict) or any(not isinstance(record, dict) for record in records):
        raise ValueError("authority record is not an object")
    if mode != files[journals[-1]] or current != records[-1]:
        raise ValueError("authority head is stale")
    if (current.get("outcome") != "encrypted" or
            current.get("backend") != {"kind": "s3", "endpoint": "http://rustfs:9000", "bucket": "teagram", "prefix": "teagramd/"}):
        raise ValueError("authority does not match encrypted S3")
except (tarfile.TarError, OSError, ValueError, TypeError, json.JSONDecodeError):
    raise SystemExit("k3s cutover: source blob-mode authority is unavailable or does not describe encrypted S3")
print("encrypted S3 authority verified")
' || fail 'could not verify the live encrypted S3 blob-mode authority'
}

check_source_media_keys() {
  local source_keys target_keys current_version
  current_version=$(capture_compose_value TG_MEDIA_KEY_CURRENT)
  [[ $current_version =~ ^[1-9][0-9]*$ ]] || fail 'source media encryption key version is invalid'
  source_keys=$(incus exec "$SOURCE_INSTANCE" -- docker cp \
    teagram-teagramd-1:/run/teagramd/media-keys/. - |
    python3 -c '
import hashlib
import re
import sys
import tarfile

keys = {}
try:
    with tarfile.open(fileobj=sys.stdin.buffer, mode="r|*") as archive:
        for member in archive:
            name = member.name.rsplit("/", 1)[-1]
            if not member.isfile() or not re.fullmatch(r"[0-9]+\.key", name):
                continue
            if member.size < 1 or member.size > 65536 or name in keys:
                raise ValueError("invalid key file")
            stream = archive.extractfile(member)
            if stream is None:
                raise ValueError("unreadable key file")
            keys[name] = hashlib.sha256(stream.read()).hexdigest()
    if "1.key" not in keys:
        raise ValueError("current key is missing")
except (tarfile.TarError, OSError, ValueError):
    raise SystemExit("k3s cutover: source encrypted-media keys are unavailable or invalid")
for name in sorted(keys, key=lambda entry: int(entry.split(".", 1)[0])):
    print(f"{name}\t{keys[name]}")
') || fail 'could not inventory source encrypted-media keys'
  awk -F '\t' -v current="$current_version.key" '$1 == current { found = 1 } END { exit !found }' \
    <<< "$source_keys" || fail 'source media encryption key version is missing from the mounted key set'
  target_keys=$(kubectl get secret teagramd-media-keys --namespace "$NAMESPACE" --output json |
    python3 -c '
import base64
import hashlib
import json
import re
import sys

try:
    secret = json.load(sys.stdin)
    data = secret.get("data")
    if not isinstance(data, dict) or "1.key" not in data:
        raise ValueError("current key is missing")
    keys = {}
    for name, encoded in data.items():
        if not re.fullmatch(r"[0-9]+\.key", name):
            raise ValueError("invalid key name")
        value = base64.b64decode(encoded, validate=True)
        if not value or len(value) > 65536:
            raise ValueError("invalid key size")
        keys[name] = hashlib.sha256(value).hexdigest()
except (ValueError, TypeError, json.JSONDecodeError):
    raise SystemExit("k3s cutover: target encrypted-media Secret is invalid")
for name in sorted(keys, key=lambda entry: int(entry.split(".", 1)[0])):
    print(f"{name}\t{keys[name]}")
') || fail 'could not inventory target encrypted-media keys'
  [[ $source_keys == "$target_keys" ]] || fail 'target encrypted-media keys do not match the live Compose key set'
}

check_source_blob_credentials() {
  local source_access_key source_secret_file source_secret source_digest target_digest
  source_access_key=$(capture_compose_value TG_BLOB_S3_ACCESS_KEY_ID)
  source_secret_file=$(capture_compose_value TG_BLOB_S3_SECRET_ACCESS_KEY_FILE)
  [[ $source_access_key =~ ^[0-9a-f]{20}$ && $source_secret_file == /* ]] ||
    fail 'source S3 credentials are not available in the expected form'
  source_secret=$(capture_source_blob_secret "$source_secret_file") || fail 'could not read the source S3 secret'
  source_digest=$(printf '%s\0%s' "$source_access_key" "$source_secret" | sha256sum | awk '{print $1}')
  target_digest=$(kubectl get secret teagramd-blob-credentials --namespace "$NAMESPACE" --output json |
    python3 -c '
import base64
import hashlib
import json
import re
import sys

try:
    secret = json.load(sys.stdin)
    data = secret.get("data")
    if not isinstance(data, dict):
        raise ValueError("missing Secret data")
    access_key = base64.b64decode(data["TG_BLOB_S3_ACCESS_KEY_ID"], validate=True)
    secret_key = base64.b64decode(data["TG_BLOB_S3_SECRET_ACCESS_KEY"], validate=True)
    if (re.fullmatch(rb"[0-9a-f]{20}", access_key) is None or
            re.fullmatch(rb"[0-9a-f]{64}", secret_key) is None):
        raise ValueError("invalid S3 credential format")
except (KeyError, TypeError, ValueError, json.JSONDecodeError):
    raise SystemExit("k3s cutover: target S3 credentials are invalid")
sys.stdout.write(hashlib.sha256(access_key + b"\0" + secret_key).hexdigest())
') || fail 'could not inventory target S3 credentials'
  source_secret=''
  [[ $source_digest == "$target_digest" ]] || fail 'target S3 credentials do not match the live Compose credentials'
}

preserve_media_key_version() {
  local current_version configured_version
  current_version=$(capture_compose_value TG_MEDIA_KEY_CURRENT)
  [[ $current_version =~ ^[1-9][0-9]*$ ]] || fail 'source media encryption key version is invalid'
  kubectl patch configmap teagramd-runtime --namespace "$NAMESPACE" --type=merge \
    --patch "{\"data\":{\"TG_MEDIA_KEY_CURRENT\":\"$current_version\"}}" >/dev/null ||
    fail 'could not preserve the live media encryption key version in the target runtime ConfigMap'
  configured_version=$(kubectl get configmap teagramd-runtime --namespace "$NAMESPACE" \
    --output 'jsonpath={.data.TG_MEDIA_KEY_CURRENT}') || fail 'could not verify the target media encryption key version'
  [[ $configured_version == "$current_version" ]] ||
    fail 'target media encryption key version does not match the live Compose version'
}

capture_compose_value() {
  local name=$1
  capture_compose_environment |
    awk -F= -v name="$name" '$1 == name { print substr($0, index($0, "=") + 1) }'
}

capture_source_blob_secret() {
  local path=$1
  incus exec "$SOURCE_INSTANCE" -- docker cp "teagram-teagramd-1:$path" - |
    python3 -c '
import sys
import tarfile

value = None
try:
    with tarfile.open(fileobj=sys.stdin.buffer, mode="r|*") as archive:
        for member in archive:
            if not member.isfile():
                continue
            if value is not None or member.size != 64:
                raise ValueError("invalid source secret")
            stream = archive.extractfile(member)
            if stream is None:
                raise ValueError("unreadable source secret")
            value = stream.read().decode("ascii")
    if value is None:
        raise ValueError("invalid source secret")
    if len(value) != 64 or any(char not in "0123456789abcdef" for char in value):
        raise ValueError("invalid source secret")
except (tarfile.TarError, OSError, ValueError, UnicodeDecodeError):
    raise SystemExit("k3s cutover: source S3 secret is unavailable or invalid")
sys.stdout.write(value)
'
}

source_compose_networks() {
  local container=$1
  # shellcheck disable=SC2016 # Docker expands this Go template remotely.
  incus exec "$SOURCE_INSTANCE" -- docker inspect \
    --format '{{range $name, $network := .NetworkSettings.Networks}}{{println $name}}{{end}}' "$container"
}

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
ExecStart=/usr/local/bin/k3s kubectl port-forward --namespace $NAMESPACE --address $target_lxc_ip $target $ports
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

discover_target_lxc_ip() {
  # shellcheck disable=SC2016 # This command is interpreted by the target LXC's shell.
  target_lxc_ip=$(incus exec "$TARGET_INSTANCE" -- sh -ec \
    'ip -o -4 addr show dev eth0 scope global | awk "NR == 1 {split(\$4, a, \"/\"); print a[1]}"')
  [[ $target_lxc_ip =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] ||
    fail 'could not identify the private teagram-k3s LXC address'
}

wait_for_tcp_forward() {
  local ip=$1 port=$2 attempt
  for ((attempt = 1; attempt <= 30; attempt++)); do
    if python3 - "$ip" "$port" <<'PY'
import socket
import sys

try:
    with socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=1):
        pass
except OSError:
    raise SystemExit(1)
PY
    then
      return 0
    fi
    sleep 2
  done
  return 1
}

mc_client_exec() {
  if [[ -n $MC_HELPER_CONTAINER ]]; then
    incus exec "$SOURCE_INSTANCE" -- docker exec --interactive "$MC_HELPER_CONTAINER" mc "$@"
  else
    kubectl exec --namespace "$SOURCE_INSTANCE" --stdin pod/cutover-s3-client -- mc "$@"
  fi
}

setup_mc_client() {
  local target_endpoint=$1 access_key secret_key secret_path target_listing
  local app_network_output rustfs_network_output network app_network common_network=''
  local -a app_networks=() rustfs_networks=()
  access_key=$(capture_compose_value TG_BLOB_S3_ACCESS_KEY_ID)
  secret_path=$(capture_compose_value TG_BLOB_S3_SECRET_ACCESS_KEY_FILE)
  [[ $access_key =~ ^[0-9a-f]{20}$ && $secret_path == /* ]] ||
    fail 'source S3 credentials are not available in the expected form'

  if [[ $TEST_MODE == true ]]; then
    target_endpoint=${CUTOVER_TEST_TARGET_S3_ENDPOINT:-$target_endpoint}
    kubectl apply --filename "$SCRIPT_DIR/test-cutover-s3-client.yaml" >/dev/null ||
      fail 'could not create the disposable S3 client pod'
    MC_CLIENT_POD_CREATED=true
    kubectl wait --for=condition=Ready pod/cutover-s3-client --namespace "$SOURCE_INSTANCE" --timeout=180s >/dev/null ||
      fail 'the disposable S3 client pod is unavailable'
  fi

  if source_docker_helper_enabled; then
    secret_key=$(capture_source_blob_secret "$secret_path") || fail 'could not read the source S3 secret'
    app_network_output=$(source_compose_networks teagram-teagramd-1) ||
      fail 'could not inspect the source Compose networks'
    rustfs_network_output=$(source_compose_networks teagram-rustfs-1) ||
      fail 'could not inspect the source RustFS network'
    [[ -n $app_network_output && -n $rustfs_network_output ]] ||
      fail 'source Compose and RustFS networks are unavailable'
    mapfile -t app_networks < <(printf '%s' "$app_network_output")
    mapfile -t rustfs_networks < <(printf '%s' "$rustfs_network_output")
    ((${#app_networks[@]} >= 2 && ${#rustfs_networks[@]} == 1)) ||
      fail 'source teagramd must share its RustFS network and a separate target route'
    for network in "${rustfs_networks[@]}"; do
      [[ $network =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*$ ]] || fail 'source RustFS network name is invalid'
      for app_network in "${app_networks[@]}"; do
        if [[ $network == "$app_network" ]]; then
          [[ -z $common_network ]] || fail 'source teagramd shares multiple networks with RustFS'
          common_network=$network
        fi
      done
    done
    [[ -n $common_network ]] || fail 'source teagramd is not attached to the live RustFS network'
    MC_HELPER_CONTAINER="teagram-cutover-mc-$$"
    incus exec "$SOURCE_INSTANCE" -- docker run --detach --name "$MC_HELPER_CONTAINER" \
      --network "$common_network" --env HOME=/tmp --env MC_CONFIG_DIR=/tmp/mc \
      --entrypoint /bin/sh "$MC_IMAGE" -ec 'mkdir -p /tmp/mc; exec sleep 21600' >/dev/null ||
      fail 'could not start the temporary S3 client on the source Compose network'
    for network in "${app_networks[@]}"; do
      [[ $network == "$common_network" ]] && continue
      [[ $network =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*$ ]] || fail 'source Compose network name is invalid'
      incus exec "$SOURCE_INSTANCE" -- docker network connect "$network" "$MC_HELPER_CONTAINER" >/dev/null ||
        fail 'could not connect the temporary S3 client to the target route network'
    done
    incus exec "$SOURCE_INSTANCE" -- docker exec "$MC_HELPER_CONTAINER" mc alias set \
      source http://rustfs:9000 "$access_key" "$secret_key" --api S3v4 >/dev/null 2>&1 ||
      fail 'could not configure the source S3 client'
    incus exec "$SOURCE_INSTANCE" -- docker exec "$MC_HELPER_CONTAINER" mc alias set \
      target "$target_endpoint" "$access_key" "$secret_key" --api S3v4 >/dev/null 2>&1 ||
      fail 'could not configure the target S3 client'
    secret_key=''
  else
    # shellcheck disable=SC2016 # The client pod's shell expands these variables.
    kubectl exec --namespace "$SOURCE_INSTANCE" pod/cutover-s3-client -- /bin/sh -ec \
      'mkdir -p "$MC_CONFIG_DIR"; mc alias set source http://rustfs:9000 "$TG_BLOB_S3_ACCESS_KEY_ID" "$TG_BLOB_S3_SECRET_ACCESS_KEY" --api S3v4 >/dev/null 2>&1; mc alias set target "$1" "$TG_BLOB_S3_ACCESS_KEY_ID" "$TG_BLOB_S3_SECRET_ACCESS_KEY" --api S3v4 >/dev/null 2>&1' \
      -- "$target_endpoint" || fail 'could not configure the disposable S3 client'
  fi
  source_s3_listing source >/dev/null
  target_listing=$(source_s3_listing target)
  [[ -z $target_listing ]] || fail 'target RustFS already contains objects under the encrypted S3 prefix'
}

stop_mc_client() {
  if [[ -n $MC_HELPER_CONTAINER ]]; then
    incus exec "$SOURCE_INSTANCE" -- docker rm --force "$MC_HELPER_CONTAINER" >/dev/null 2>&1 ||
      fail 'could not remove the temporary S3 client container'
    MC_HELPER_CONTAINER=''
  fi
  if [[ $TEST_MODE == true && $MC_CLIENT_POD_CREATED == true ]]; then
    kubectl delete pod/cutover-s3-client --namespace "$SOURCE_INSTANCE" \
      --ignore-not-found --wait=true --timeout=180s >/dev/null ||
      fail 'could not remove the disposable S3 client pod'
    MC_CLIENT_POD_CREATED=false
  fi
}

source_s3_listing() {
  local alias=$1 output
  output=$(mc_client_exec --json ls --recursive "$alias/teagram/teagramd/" 2>/dev/null) ||
    fail "could not list the $alias encrypted S3 object prefix"
  python3 -c '
import json
import re
import sys

prefix = "teagramd/"
objects = {}
try:
    for line in sys.stdin:
        if not line.strip():
            continue
        item = json.loads(line)
        if item.get("status") != "success":
            raise ValueError("listing returned an error")
        if item.get("type") == "folder":
            continue
        if item.get("type") != "file":
            raise ValueError("listing returned an unsupported entry")
        key = item.get("key")
        size = item.get("size")
        if not isinstance(key, str) or not isinstance(size, int) or size < 0:
            raise ValueError("listing entry is incomplete")
        if key.startswith("teagram/"):
            key = key[len("teagram/"):]
        if not key.startswith(prefix):
            key = prefix + key
        parts = key.split("/")
        if (not key.startswith(prefix) or not re.fullmatch(r"[A-Za-z0-9._/-]+", key) or
                any(part in ("", ".", "..") for part in parts) or "\t" in key or "\n" in key):
            raise ValueError("listing key is invalid")
        if key in objects:
            raise ValueError("listing contains a duplicate object")
        objects[key] = size
except (json.JSONDecodeError, ValueError, TypeError):
    raise SystemExit("k3s cutover: S3 inventory is invalid or incomplete")
for key in sorted(objects):
    print(f"{key}\t{objects[key]}")
' <<< "$output" || fail "the $alias encrypted S3 object inventory is invalid"
}

record_source_s3_size() {
  local listing
  listing=$(source_s3_listing source)
  if [[ -z $listing ]]; then
    S3_OBJECTS=0
    S3_BYTES=0
    return
  fi
  S3_OBJECTS=$(wc -l <<< "$listing")
  S3_BYTES=$(awk -F '\t' '{ total += $2 } END { print total + 0 }' <<< "$listing")
  [[ $S3_OBJECTS =~ ^[0-9]+$ && $S3_BYTES =~ ^[0-9]+$ ]] || fail 'source S3 object totals are invalid'
  (( S3_BYTES <= MAX_ENCRYPTED_BLOB_BYTES )) ||
    fail 'source encrypted S3 prefix exceeds the 18 GiB cutover budget for the 20 GiB RustFS PVC'
}

s3_object_hashes() {
  local alias=$1 listing_file=$2
  local -a keys=()
  mapfile -t keys < <(awk -F '\t' 'NF == 2 { print $1 }' "$listing_file")
  ((${#keys[@]} == 0)) && return 0
  if [[ $TEST_MODE == true && -z $MC_HELPER_CONTAINER ]]; then
    # shellcheck disable=SC2016 # The disposable client's shell expands the variables.
    printf '%s\n' "${keys[@]}" | kubectl exec --stdin --namespace "$SOURCE_INSTANCE" \
      pod/cutover-s3-client -- /bin/sh -ec '
        set -o pipefail
        alias_name=$1
        while IFS= read -r key; do
          digest=$(mc cat "$alias_name/teagram/$key" | sha256sum)
          digest=${digest%% *}
          printf "%s\\t%s\\n" "$key" "$digest"
        done
      ' -- "$alias" 2>/dev/null || fail "could not checksum the $alias S3 objects"
  else
    # shellcheck disable=SC2016 # The helper container's shell expands the variables.
    printf '%s\n' "${keys[@]}" | incus exec "$SOURCE_INSTANCE" -- docker exec --interactive \
      "$MC_HELPER_CONTAINER" /bin/sh -ec '
        set -o pipefail
        alias_name=$1
        while IFS= read -r key; do
          digest=$(mc cat "$alias_name/teagram/$key" | sha256sum)
          digest=${digest%% *}
          printf "%s\\t%s\\n" "$key" "$digest"
        done
      ' -- "$alias" 2>/dev/null || fail "could not checksum the $alias S3 objects"
  fi
}

inventory_s3_prefix() {
  local alias=$1 listing=$2 scratch listing_file hashes_file inventory_hash status=0
  scratch=$(mktemp -d "$EVIDENCE_DIR/s3-inventory.XXXXXX")
  S3_INVENTORY_SCRATCH=$scratch
  listing_file=$scratch/listing
  hashes_file=$scratch/hashes
  printf '%s\n' "$listing" > "$listing_file"
  s3_object_hashes "$alias" "$listing_file" > "$hashes_file" || status=$?
  if (( status == 0 )); then
    inventory_hash=$(python3 -c '
import re
import sys

sizes = {}
with open(sys.argv[1], encoding="utf-8") as listing:
    for line in listing:
        if not line.strip():
            continue
        key, size = line.rstrip("\n").split("\t", 1)
        sizes[key] = int(size)
hashes = {}
with open(sys.argv[2], encoding="utf-8") as inventory:
    for line in inventory:
        key, digest = line.rstrip("\n").split("\t", 1)
        if key in hashes or not re.fullmatch(r"[0-9a-f]{64}", digest):
            raise SystemExit("k3s cutover: S3 checksum inventory is invalid")
        hashes[key] = digest
if set(sizes) != set(hashes):
    raise SystemExit("k3s cutover: S3 checksum inventory is incomplete")
for key in sorted(sizes):
    print(f"{key}\t{sizes[key]}\t{hashes[key]}")
' "$listing_file" "$hashes_file" | sha256sum | awk '{print $1}') || status=$?
  fi
  rm -f -- "$listing_file" "$hashes_file"
  rmdir -- "$scratch"
  S3_INVENTORY_SCRATCH=''
  (( status == 0 )) || fail "could not inventory the $alias S3 object checksums"
  [[ $inventory_hash =~ ^[0-9a-f]{64}$ ]] || fail "the $alias S3 checksum inventory is invalid"
  printf '%s' "$inventory_hash"
}

mirror_encrypted_s3_prefix() {
  local source_listing target_listing source_inventory target_inventory
  local source_total target_total source_objects target_objects
  source_listing=$(source_s3_listing source)
  target_listing=$(source_s3_listing target)
  [[ -z $target_listing ]] || fail 'target RustFS already contains objects under the encrypted S3 prefix'
  source_inventory=$(inventory_s3_prefix source "$source_listing")
  if [[ -z $source_listing ]]; then source_objects=0; else source_objects=$(wc -l <<< "$source_listing"); fi
  source_total=$(awk -F '\t' '{ total += $2 } END { print total + 0 }' <<< "$source_listing")
  [[ $source_total =~ ^[0-9]+$ && $source_objects =~ ^[0-9]+$ ]] || fail 'source S3 object totals are invalid'
  [[ $source_objects == "$S3_OBJECTS" && $source_total == "$S3_BYTES" ]] ||
    fail 'source encrypted S3 inventory changed after the write fence'
  (( source_total <= MAX_ENCRYPTED_BLOB_BYTES )) ||
    fail 'source encrypted S3 prefix exceeds the 18 GiB cutover budget for the 20 GiB RustFS PVC'

  if ! mc_client_exec mirror --overwrite "source/teagram/teagramd/" "target/teagram/teagramd/" \
    >/dev/null 2>&1; then
    fail 'could not mirror encrypted S3 objects to target RustFS'
  fi
  if [[ ${CUTOVER_TEST_CORRUPT_S3_OBJECT:-} == true && $TEST_MODE == true && -n $source_listing ]]; then
    first_key=$(awk -F '\t' 'NR == 1 { print $1 }' <<< "$source_listing")
    first_size=$(awk -F '\t' 'NR == 1 { print $2 }' <<< "$source_listing")
    [[ $first_size =~ ^[1-9][0-9]*$ ]] || fail 'could not determine the disposable S3 object size for checksum injection'
    python3 -c '
import sys

remaining = int(sys.argv[1])
chunk = b"x" * min(1024 * 1024, remaining)
while remaining:
    part = chunk[:remaining]
    sys.stdout.buffer.write(part)
    remaining -= len(part)
' "$first_size" |
      mc_client_exec pipe "target/teagram/$first_key" >/dev/null 2>&1 ||
      fail 'could not inject a same-size disposable S3 checksum mismatch'
  fi
  target_listing=$(source_s3_listing target)
  target_inventory=$(inventory_s3_prefix target "$target_listing")
  if [[ -z $target_listing ]]; then target_objects=0; else target_objects=$(wc -l <<< "$target_listing"); fi
  target_total=$(awk -F '\t' '{ total += $2 } END { print total + 0 }' <<< "$target_listing")
  [[ $source_objects == "$target_objects" && $source_total == "$target_total" ]] ||
    fail 'source and target S3 object counts or byte totals do not match'
  [[ $source_inventory == "$target_inventory" ]] ||
    fail 'source and target S3 per-object SHA-256 checksums do not match'
  S3_OBJECTS=$source_objects
  S3_BYTES=$source_total
  stop_mc_client
}

blob_mode_tar_hash() {
  python3 -c '
import hashlib
import pathlib
import sys
import tarfile

files = {}
try:
    with tarfile.open(fileobj=sys.stdin.buffer, mode="r|*") as archive:
        for member in archive:
            path = pathlib.PurePosixPath(member.name)
            parts = tuple(part for part in path.parts if part not in (".", ""))
            if path.is_absolute() or any(part == ".." for part in parts):
                raise ValueError("unsafe path")
            if member.isdir():
                continue
            if not member.isfile() or not parts:
                raise ValueError("unsupported entry")
            stream = archive.extractfile(member)
            if stream is None:
                raise ValueError("unreadable file")
            content = stream.read()
            if len(content) != member.size:
                raise ValueError("short file")
            files["/".join(parts)] = content
    digest = hashlib.sha256()
    for name in sorted(files):
        content = files[name]
        digest.update(name.encode("utf-8") + b"\\0" + str(len(content)).encode("ascii") + b"\\0")
        digest.update(hashlib.sha256(content).digest())
        digest.update(b"\\n")
except (tarfile.TarError, OSError, ValueError):
    raise SystemExit("k3s cutover: blob-mode state archive is invalid")
print(digest.hexdigest())
'
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
    --from-file=TG_ADMIN_ORIGIN="$admin_origin_file" >/dev/null
}

serve_status_file() {
  local destination=$1
  incus exec "$SOURCE_INSTANCE" -- tailscale serve status --json > "$destination" || return 1
  chmod 0600 "$destination"
}

check_serve_status() {
  local status_file=$1 expected_root=$2 websocket_base=$3 admin_base=$4 expected_tcp=$5
  local expected_bot_api_tcp=$6 allow_old_routes=${7:-false}
  python3 -c '
import json
import pathlib
import sys

status = json.loads(pathlib.Path(sys.argv[1]).read_text(encoding="utf-8"))
expected_root = sys.argv[2]
websocket_base = sys.argv[3]
admin_base = sys.argv[4]
expected_tcp = sys.argv[5]
expected_bot_api_tcp = sys.argv[6]
allow_old_routes = sys.argv[7] == "true"
expected = {
    "/": expected_root,
    "/apiws": websocket_base + "/apiws",
    "/admin": admin_base + "/admin",
    "/.well-known/teagramd/client": websocket_base + "/.well-known/teagramd/client",
    "/.well-known/telegramd/client": websocket_base + "/.well-known/telegramd/client",
}
old = {
    "/": "http://127.0.0.1:8081",
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
    allowed = {target, old[path]} if allow_old_routes else {target}
    if not isinstance(handler, dict) or handler.get("Proxy") not in allowed:
        raise SystemExit("k3s cutover: Tailscale Serve handler target changed: " + path)
tcp = status.get("TCP")
if not isinstance(tcp, dict) or set(tcp) != {"443", "2443", "2447"}:
    raise SystemExit("k3s cutover: an advertised MTProto or Bot API TCP listener is missing or changed")
if tcp["443"] != {"HTTPS": True}:
    raise SystemExit("k3s cutover: the HTTPS listener is missing or changed")
tcp_handler = tcp["2443"]
allowed_tcp = {expected_tcp, "127.0.0.1:2443"} if allow_old_routes else {expected_tcp}
if not isinstance(tcp_handler, dict) or tcp_handler.get("TCPForward") not in allowed_tcp:
    raise SystemExit("k3s cutover: the advertised MTProto TCP target changed")
bot_api_handler = tcp["2447"]
allowed_bot_api_tcp = {expected_bot_api_tcp, "127.0.0.1:2447"} if allow_old_routes else {expected_bot_api_tcp}
if (not isinstance(bot_api_handler, dict) or
        bot_api_handler.get("TCPForward") not in allowed_bot_api_tcp or
        bot_api_handler.get("ProxyProtocol") != 1):
    raise SystemExit("k3s cutover: the Bot API TCP target or PROXY protocol changed")
funnel = status.get("AllowFunnel", {})
if not isinstance(funnel, dict):
    raise SystemExit("k3s cutover: Tailscale Funnel state is missing or invalid")
if any(value is not False for value in funnel.values()):
    raise SystemExit("k3s cutover: Tailscale Funnel is enabled or its state is unknown")
' "$status_file" "$expected_root" "$websocket_base" "$admin_base" "$expected_tcp" "$expected_bot_api_tcp" "$allow_old_routes"
}

set_serve_routes() {
  local root=$1 websocket_base=$2 admin_base=$3 tcp_target=$4 bot_api_tcp_target=$5 skip_root=${6:-false}
  if [[ $skip_root != true ]]; then
    incus exec "$SOURCE_INSTANCE" -- tailscale serve --bg --https=443 --set-path=/ "$root" >/dev/null
  fi
  incus exec "$SOURCE_INSTANCE" -- tailscale serve --bg --https=443 --set-path=/apiws \
    "$websocket_base/apiws" >/dev/null
  incus exec "$SOURCE_INSTANCE" -- tailscale serve --bg --https=443 --set-path=/admin \
    "$admin_base/admin" >/dev/null
  incus exec "$SOURCE_INSTANCE" -- tailscale serve --bg --https=443 \
    --set-path=/.well-known/teagramd/client "$websocket_base/.well-known/teagramd/client" >/dev/null
  incus exec "$SOURCE_INSTANCE" -- tailscale serve --bg --https=443 \
    --set-path=/.well-known/telegramd/client "$websocket_base/.well-known/telegramd/client" >/dev/null
  incus exec "$SOURCE_INSTANCE" -- tailscale serve --bg --tcp="$MTPROTO_PORT" \
    "tcp://$tcp_target" >/dev/null
  incus exec "$SOURCE_INSTANCE" -- tailscale serve --yes --bg --tcp="$BOT_API_PORT" \
    --proxy-protocol=1 "tcp://$bot_api_tcp_target" >/dev/null
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

wait_for_job() {
  local job=$1 timeout_seconds=$2 elapsed=0 condition
  while (( elapsed < timeout_seconds )); do
    if ! condition=$(kubectl get "$job" --namespace "$NAMESPACE" \
      --output 'jsonpath={range .status.conditions[?(@.status=="True")]}{.type}{"\n"}{end}'); then
      printf 'k3s cutover: could not read Job status: %s\n' "$job" >&2
      return 1
    fi
    if [[ $condition == *Failed* ]]; then
      printf 'k3s cutover: Job failed: %s\n' "$job" >&2
      kubectl describe "$job" --namespace "$NAMESPACE" >&2 || true
      kubectl logs "$job" --namespace "$NAMESPACE" --all-containers --tail=200 >&2 || true
      return 1
    fi
    [[ $condition == *Complete* ]] && return 0
    sleep 5
    elapsed=$((elapsed + 5))
  done
  printf 'k3s cutover: timed out waiting for Job %s after %ss\n' "$job" "$timeout_seconds" >&2
  kubectl describe "$job" --namespace "$NAMESPACE" >&2 || true
  kubectl logs "$job" --namespace "$NAMESPACE" --all-containers --tail=200 >&2 || true
  return 1
}

kubectl_resource_exists() {
  local resource=$1 result
  result=$(kubectl get "$resource" --namespace "$NAMESPACE" --ignore-not-found --output name) || return 2
  [[ -n $result ]]
}

stop_k8s_resource() {
  local resource=$1 selector=$2 failure_message=$3 lookup_status
  if kubectl_resource_exists "$resource"; then
    kubectl scale "$resource" --replicas=0 --namespace "$NAMESPACE" >/dev/null ||
      fail "could not scale down $resource"
  else
    lookup_status=$?
    [[ $lookup_status == 1 ]] || fail "could not determine whether $resource exists"
  fi
  wait_for_no_pods "$selector" 180 || fail "$failure_message"
}

stop_k8s_connector() {
  stop_k8s_resource deployment/cloudflared app.kubernetes.io/name=cloudflared \
    'k8s cloudflared pods did not stop; refusing to change the route or start the old connector'
}

stop_k8s_application() {
  stop_k8s_resource statefulset/teagramd app.kubernetes.io/name=teagramd \
    'k8s teagramd pods did not stop; refusing to restart the old Compose writers'
}

confirm() {
  local expected=$1 prompt=$2 response
  printf '%s\n' "$prompt" >&2
  printf 'Type %s to continue: ' "$expected" >&2
  if [[ $TEST_MODE == true ]]; then
    IFS= read -r response <&3
  else
    IFS= read -r response
  fi
  [[ $response == "$expected" ]] || fail 'operator did not confirm the exact route state'
}

print_rollback_command() {
  printf 'Rollback command: sudo bash %q --rollback\n' "$SCRIPT_PATH"
}

stop_port_forward_units() {
  local unit load_state failed=false
  for unit in "$LINKSELECTOR_PORT_FORWARD_UNIT" "$TEAGRAMD_PORT_FORWARD_UNIT" "$RUSTFS_PORT_FORWARD_UNIT"; do
    if ! load_state=$(incus exec "$TARGET_INSTANCE" -- systemctl show -p LoadState --value "$unit"); then
      failed=true
      continue
    fi
    if [[ $load_state != not-found ]]; then
      if [[ $load_state != loaded ]] || ! incus exec "$TARGET_INSTANCE" -- systemctl disable --now "$unit" >/dev/null; then
        failed=true
      fi
    fi
  done
  [[ $failed == false ]]
}

stop_rustfs_copy_forward() {
  [[ $TEST_MODE == false ]] || return 0
  incus exec "$TARGET_INSTANCE" -- systemctl disable --now "$RUSTFS_PORT_FORWARD_UNIT" >/dev/null ||
    fail 'could not stop the temporary RustFS copy port-forward'
  incus exec "$TARGET_INSTANCE" -- rm -f "/etc/systemd/system/$RUSTFS_PORT_FORWARD_UNIT"
  incus exec "$TARGET_INSTANCE" -- systemctl daemon-reload
}

recover_before_edge_switch() {
  local phase=${1:-} recovery_failed=false workloads_stopped=true lookup_status
  case "$phase" in
    serve-switch-started|serve-switched|route-change-requested|route-switched|k8s-connector-started|complete)
      recover_after_serve_switch
      return 0
      ;;
  esac
  if kubectl_resource_exists deployment/cloudflared; then
    if ! kubectl scale deployment/cloudflared --replicas=0 --namespace "$NAMESPACE" >/dev/null; then
      printf 'k3s cutover: failed to scale down the k8s Cloudflare connector during recovery\n' >&2
      recovery_failed=true
      workloads_stopped=false
    fi
  else
    lookup_status=$?
    if [[ $lookup_status != 1 ]]; then
      printf 'k3s cutover: could not determine whether the k8s Cloudflare connector exists during recovery\n' >&2
      recovery_failed=true
      workloads_stopped=false
    fi
  fi
  if kubectl_resource_exists statefulset/teagramd; then
    if ! kubectl scale statefulset/teagramd --replicas=0 --namespace "$NAMESPACE" >/dev/null; then
      printf 'k3s cutover: failed to scale down k8s teagramd during recovery\n' >&2
      recovery_failed=true
      workloads_stopped=false
    fi
  else
    lookup_status=$?
    if [[ $lookup_status != 1 ]]; then
      printf 'k3s cutover: could not determine whether k8s teagramd exists during recovery\n' >&2
      recovery_failed=true
      workloads_stopped=false
    fi
  fi
  if ! wait_for_no_pods app.kubernetes.io/name=cloudflared 180; then
    printf 'k3s cutover: failed to verify no k8s Cloudflare connector pods remain during recovery\n' >&2
    recovery_failed=true
    workloads_stopped=false
  fi
  if ! wait_for_no_pods app.kubernetes.io/name=teagramd 180; then
    printf 'k3s cutover: failed to verify no k8s teagramd pods remain during recovery\n' >&2
    recovery_failed=true
    workloads_stopped=false
  fi

  if ! stop_port_forward_units; then
    printf 'k3s cutover: failed to stop target port-forward units during recovery\n' >&2
    recovery_failed=true
  fi
  if [[ -f $COMPOSE_STOPPED_FILE && $workloads_stopped == true ]]; then
    if ! restart_stopped_compose_services; then
      printf 'k3s cutover: one or more old Compose services need manual restart\n' >&2
      recovery_failed=true
    fi
  fi
  if [[ $recovery_failed == true ]]; then
    printf 'k3s cutover: recovery was incomplete; inspect the phase and stopped-service evidence before proceeding\n' >&2
  fi
}

unit_file=''
admin_hash_file=''
admin_origin_file=''
cutover_manifest=''
dump_spool=''
handle_exit() {
  local status=$? phase='' current_serve=''
  trap - EXIT
  [[ -z ${unit_file:-} ]] || rm -f -- "$unit_file"
  [[ -z ${admin_hash_file:-} ]] || rm -f -- "$admin_hash_file"
  [[ -z ${admin_origin_file:-} ]] || rm -f -- "$admin_origin_file"
  [[ -z ${cutover_manifest:-} ]] || rm -f -- "$cutover_manifest"
  [[ -z ${dump_spool:-} ]] || rm -f -- "$dump_spool"
  if [[ -n ${S3_INVENTORY_SCRATCH:-} ]]; then
    rm -f -- "$S3_INVENTORY_SCRATCH/listing" "$S3_INVENTORY_SCRATCH/hashes"
    rmdir -- "$S3_INVENTORY_SCRATCH" 2>/dev/null || true
    S3_INVENTORY_SCRATCH=''
  fi
  if [[ -n ${MC_HELPER_CONTAINER:-} ]]; then
    incus exec "$SOURCE_INSTANCE" -- docker rm --force "$MC_HELPER_CONTAINER" >/dev/null 2>&1 ||
      printf 'k3s cutover: could not remove the temporary S3 client container\n' >&2
    MC_HELPER_CONTAINER=''
  fi
  if [[ ${MC_CLIENT_POD_CREATED:-false} == true ]]; then
    kubectl delete pod/cutover-s3-client --namespace "$SOURCE_INSTANCE" \
      --ignore-not-found --wait=true --timeout=180s >/dev/null 2>&1 ||
      printf 'k3s cutover: could not remove the disposable S3 client pod\n' >&2
    MC_CLIENT_POD_CREATED=false
  fi
  if (( status != 0 )) && [[ -f $PHASE_FILE ]]; then
    phase=$(<"$PHASE_FILE")
    case "$phase" in
      prepared) ;;
      *) kubectl delete pod/cutover-volume-importer --namespace "$NAMESPACE" --ignore-not-found --wait=true --timeout=300s >/dev/null ||
           printf 'k3s cutover: could not remove the temporary volume importer; manual cleanup is required\n' >&2 ;;
    esac
    if [[ $phase != complete && $phase != rolled-back ]]; then
      case "$phase" in
        serve-switch-started)
          current_serve=$(mktemp)
          if serve_status_file "$current_serve" && check_serve_status "$current_serve" http://127.0.0.1:8081 \
            http://127.0.0.1:2444 http://127.0.0.1:2445 "127.0.0.1:$MTPROTO_PORT" \
            "127.0.0.1:$BOT_API_PORT"; then
            recover_before_edge_switch ready-for-edge
          else
            recover_after_serve_switch
          fi
          rm -f -- "$current_serve"
          ;;
        serve-switched|route-change-requested|route-switched|k8s-connector-started)
          recover_after_serve_switch
          ;;
        *)
          recover_before_edge_switch "$phase"
          ;;
      esac
      print_rollback_command >&2
    fi
  fi
  exit "$status"
}

recover_after_serve_switch() {
  printf 'k3s cutover: a live Serve route may have accepted k3s writes; automatic recovery is disabled to avoid losing those writes. Explicit rollback is required and does not carry k3s writes back.\n' >&2
}
trap handle_exit EXIT

rollback() {
  local current_serve target_root target_websocket target_admin target_tcp target_bot_api_tcp target_ip serve_after
  local current_phase cleanup_failed=false
  current_phase=$(<"$PHASE_FILE")
  case "$current_phase" in
    serve-switch-started|serve-switched|route-change-requested|route-switched|k8s-connector-started|complete)
      confirm K3S-WRITES-LOST 'WARNING: this rollback does not copy k3s writes back to Compose. All writes accepted since the first live Serve route will be lost from the restored source. Continue only after accepting this data loss.'
      ;;
  esac
  current_serve=$(mktemp)
  serve_status_file "$current_serve"
  if ! check_serve_status "$current_serve" http://127.0.0.1:8081 \
    http://127.0.0.1:2444 http://127.0.0.1:2445 "127.0.0.1:$MTPROTO_PORT" \
    "127.0.0.1:$BOT_API_PORT"; then
    [[ -f $EVIDENCE_DIR/serve-target && ! -L $EVIDENCE_DIR/serve-target ]] ||
      fail 'Tailscale Serve is not on the old routes, and no verified cutover routes were recorded'
    mapfile -t target_routes < "$EVIDENCE_DIR/serve-target"
    [[ ${#target_routes[@]} -eq 5 ]] || fail 'recorded Tailscale Serve cutover routes are incomplete'
    target_root=${target_routes[0]}
    target_websocket=${target_routes[1]}
    target_admin=${target_routes[2]}
    target_tcp=${target_routes[3]}
    target_bot_api_tcp=${target_routes[4]}
    [[ $target_root =~ ^http://(([0-9]{1,3}\.){3}[0-9]{1,3}):$LINKSELECTOR_PORT$ ]] ||
      fail 'recorded Tailscale Serve cutover root is invalid'
    target_ip=${BASH_REMATCH[1]}
    [[ $target_websocket == "http://$target_ip:$WEBSOCKET_PORT" &&
      $target_admin == "http://$target_ip:$ADMIN_PORT" &&
      $target_tcp == "$target_ip:$MTPROTO_PORT" &&
      $target_bot_api_tcp == "$target_ip:$BOT_API_PORT" ]] ||
      fail 'recorded Tailscale Serve cutover service addresses are invalid'
    check_serve_status "$current_serve" "$target_root" "$target_websocket" "$target_admin" \
      "$target_tcp" "$target_bot_api_tcp" true ||
      fail 'Tailscale Serve contains an unrecognized route; refusing to overwrite it during rollback'
  fi
  rm -f -- "$current_serve"

  stop_k8s_connector
  stop_k8s_application
  set_serve_routes http://127.0.0.1:8081 http://127.0.0.1:2444 http://127.0.0.1:2445 \
    "127.0.0.1:$MTPROTO_PORT" "127.0.0.1:$BOT_API_PORT"
  serve_after=$(mktemp)
  serve_status_file "$serve_after"
  check_serve_status "$serve_after" http://127.0.0.1:8081 http://127.0.0.1:2444 \
    http://127.0.0.1:2445 "127.0.0.1:$MTPROTO_PORT" "127.0.0.1:$BOT_API_PORT"
  rm -f -- "$serve_after"

  printf 'Cloudflare dashboard route: %s -> %s\n' "$CLOUDFLARE_AFTER" "$CLOUDFLARE_BEFORE" >&2
  confirm ROUTE-RESTORED 'If the route is already on the before value, leave it there. Otherwise restore the before value now.'
  if [[ -f $COMPOSE_STOPPED_FILE ]]; then
    restart_stopped_compose_services || fail 'one or more Compose services need manual restart; rollback incomplete'
  fi
  check_source_container teagram-teagramd-1
  check_source_container teagram-postgres-1
  check_source_container teagram-web-cloudflared
  record_phase rolled-back
  if ! stop_port_forward_units; then
    printf 'k3s cutover: could not stop all target port-forwards; manual cleanup is required\n' >&2
    cleanup_failed=true
  fi
  if ! kubectl delete pod/cutover-volume-importer --namespace "$NAMESPACE" --ignore-not-found --wait=true --timeout=300s >/dev/null; then
    printf 'k3s cutover: could not remove the temporary volume importer; manual cleanup is required\n' >&2
    cleanup_failed=true
  fi
  [[ $cleanup_failed == false ]] || fail 'Serve routes and Compose services were restored; manual cleanup is required'
  printf 'Rollback complete: all five HTTPS handlers, the old MTProto and Bot API TCP routes, the old connector, and stopped Compose services are restored. k3s writes since the switch are not carried back.\n'
}

if [[ $ACTION == --rollback ]]; then
  [[ -f $PHASE_FILE && ! -L $PHASE_FILE ]] || fail 'no recorded cutover phase exists to roll back'
  if kubectl_resource_exists "namespace/$NAMESPACE"; then :; else
    lookup_status=$?
    [[ $lookup_status == 1 ]] && fail 'teagram namespace is missing; cannot verify the rollback target'
    fail 'could not determine whether the teagram namespace exists'
  fi
  check_source_container teagram-postgres-1
  trap - EXIT
  rollback
  exit 0
fi

[[ ! -e $PHASE_FILE ]] || fail 'a prior cutover phase exists; inspect the evidence, use --rollback if needed, then follow "Retry after a pre-switch failure" in deploy/k8s/README.md before another run'
for resource in \
  statefulset/postgres statefulset/teagramd statefulset/rustfs \
  deployment/cloudflared deployment/linkselector deployment/linklanding deployment/teagram-web \
  service/postgres service/postgres-headless service/rustfs service/rustfs-headless \
  service/teagramd service/teagramd-headless service/linkselector service/linklanding service/web service/teagram-web \
  service/cloudflared-metrics pod/cutover-volume-importer; do
  if kubectl_resource_exists "$resource"; then
    fail "target production workload already exists: $resource"
  else
    lookup_status=$?
    [[ $lookup_status == 1 ]] || fail "could not determine whether $resource exists"
  fi
done
for selector in app.kubernetes.io/name=db-migrations app.kubernetes.io/name=rustfs-init; do
  existing_jobs=$(kubectl get jobs --namespace "$NAMESPACE" --selector="$selector" --output name)
  [[ -z $existing_jobs ]] || fail "target production Job already exists: $selector"
done
for claim in data-postgres-0 data-rustfs-0 key-data-teagramd-0 legacy-blobs-teagramd-0 blob-mode-teagramd-0; do
  if kubectl_resource_exists "pvc/$claim"; then
    fail "target cutover PVC already exists: $claim"
  else
    lookup_status=$?
    [[ $lookup_status == 1 ]] || fail "could not determine whether pvc/$claim exists"
  fi
done
if kubectl_resource_exists "namespace/$NAMESPACE"; then :; else
  lookup_status=$?
  [[ $lookup_status == 1 ]] && fail 'the bootstrap teagram namespace is missing'
  fail 'could not determine whether the bootstrap teagram namespace exists'
fi
for secret in postgres-credentials rustfs-credentials teagramd-blob-credentials teagramd-media-keys cloudflared-token; do
  if kubectl_resource_exists "secret/$secret"; then :; else
    lookup_status=$?
    [[ $lookup_status == 1 ]] && fail "required namespace Secret is missing: $secret"
    fail "could not determine whether Secret $secret exists"
  fi
done
for configmap in teagramd-runtime link-edge-runtime; do
  if kubectl_resource_exists "configmap/$configmap"; then :; else
    lookup_status=$?
    [[ $lookup_status == 1 ]] && fail "required namespace ConfigMap is missing: $configmap"
    fail "could not determine whether ConfigMap $configmap exists"
  fi
done

check_source_container teagram-teagramd-1
validate_source_blob_backend
check_source_container teagram-postgres-1
validate_release_for_source
check_source_migration_version "$REQUIRED_MIGRATION_VERSION"
check_source_blob_authority
check_source_media_keys
check_source_blob_credentials
check_source_container teagram-link-edge-selector-1
check_source_container teagram-web-cloudflared
check_source_key() {
  local path=$1
  incus exec "$SOURCE_INSTANCE" -- docker cp "teagram-teagramd-1:$path" - |
    python3 -c '
import pathlib, sys, tarfile
name = pathlib.PurePosixPath(sys.argv[1]).name
with tarfile.open(fileobj=sys.stdin.buffer, mode="r|*") as archive:
    members = [member for member in archive if member.name == name]
    if len(members) != 1 or not members[0].isfile() or members[0].size <= 0:
        raise SystemExit("source identity file is missing or empty")
while sys.stdin.buffer.read(1024 * 1024):
    pass
' "$path" || fail 'could not inspect the source identity file'
}
check_source_key /var/lib/teagramd/server_key.pem
check_source_key /var/lib/teagramd/enc_key.hex

SERVE_BEFORE=$(mktemp)
serve_status_file "$SERVE_BEFORE"
check_serve_status "$SERVE_BEFORE" http://127.0.0.1:8081 http://127.0.0.1:2444 \
  http://127.0.0.1:2445 "127.0.0.1:$MTPROTO_PORT" "127.0.0.1:$BOT_API_PORT"

record_phase prepared
ensure_private_dir "$EVIDENCE_DIR"
install_private_file 0600 "$SERVE_BEFORE" "$EVIDENCE_DIR/serve-before.json"
rm -f -- "$SERVE_BEFORE"
if kubectl_resource_exists secret/teagramd-admin; then
  fail 'target teagramd-admin Secret already exists; refusing to overwrite the cutover identity'
else
  lookup_status=$?
  [[ $lookup_status == 1 ]] || fail 'could not determine whether the teagramd-admin Secret exists'
fi
admin_hash_file=$(mktemp)
admin_origin_file=$(mktemp)
install_admin_secret "$admin_hash_file" "$admin_origin_file"
rm -f -- "$admin_hash_file" "$admin_origin_file"
admin_hash_file=''
admin_origin_file=''
if [[ $TEST_MODE == true ]]; then
  cutover_manifest=$(mktemp "$RUNNER_TEMP/cutover-alpha.XXXXXX.yaml")
  kubectl kustomize "$REPO_ROOT/deploy/k8s/prod-cutover" |
    sed \
      -e "s/^\( *\)namespace: teagram$/\1namespace: $NAMESPACE/" \
      -e "s|ghcr.io/teagramhq/teagramd@sha256:[0-9a-f]*|$APP_IMAGE|g" \
      -e "s|name: db-migrations-[0-9a-f]*|name: $MIGRATIONS_JOB_NAME|g" \
      -e "s|job-name: db-migrations-[0-9a-f]*|job-name: $MIGRATIONS_JOB_NAME|g" \
      -e "s|migration-version: \"[0-9]*\"|migration-version: \"$REQUIRED_MIGRATION_VERSION\"|g" \
      > "$cutover_manifest"
  kubectl apply --dry-run=server --filename "$cutover_manifest" >/dev/null
  record_phase applying
  kubectl apply --filename "$cutover_manifest" >/dev/null
  kubectl patch deployment/cloudflared --namespace "$NAMESPACE" --type=strategic \
    --patch-file "$SCRIPT_DIR/test-cutover-cloudflared-patch.yaml" >/dev/null
  rm -f -- "$cutover_manifest"
  cutover_manifest=''
else
  kubectl apply --dry-run=server --kustomize "$REPO_ROOT/deploy/k8s/prod-cutover" >/dev/null
  record_phase applying
  kubectl apply --kustomize "$REPO_ROOT/deploy/k8s/prod-cutover" >/dev/null
fi

kubectl rollout status statefulset/postgres --namespace "$NAMESPACE" --timeout=600s >/dev/null
kubectl rollout status statefulset/rustfs --namespace "$NAMESPACE" --timeout=600s >/dev/null
kubectl rollout status deployment/linklanding --namespace "$NAMESPACE" --timeout=300s >/dev/null
kubectl rollout status deployment/linkselector --namespace "$NAMESPACE" --timeout=300s >/dev/null
kubectl rollout status deployment/teagram-web --namespace "$NAMESPACE" --timeout=300s >/dev/null
kubectl wait --for=condition=complete jobs \
  --selector=app.kubernetes.io/name=rustfs-init --namespace "$NAMESPACE" --timeout=600s >/dev/null
[[ $(kubectl get deployment cloudflared --namespace "$NAMESPACE" -o jsonpath='{.spec.replicas}') == 0 ]] ||
  fail 'k8s cloudflared must remain at zero replicas until the edge switch'

discover_target_lxc_ip
if [[ $TEST_MODE == false ]]; then
  install_port_forward_unit "$RUSTFS_PORT_FORWARD_UNIT" \
    'Source-only encrypted S3 copy port-forward' service/rustfs "$RUSTFS_PORT:9000"
  wait_for_tcp_forward "$target_lxc_ip" "$RUSTFS_PORT" ||
    fail 'target RustFS port-forward did not become ready'
fi
target_s3_endpoint="http://$target_lxc_ip:$RUSTFS_PORT"
setup_mc_client "$target_s3_endpoint"

fence_source_writes
fenced_source_revision=$(source_container_revision) || fail 'could not recheck the source teagramd revision after the write fence'
[[ $fenced_source_revision == "$LIVE_SOURCE_REVISION" ]] ||
  fail "live teagramd revision changed from $LIVE_SOURCE_REVISION to $fenced_source_revision during the write fence; pinned production revision is $PINNED_SOURCE_REVISION"
check_source_migration_version "$REQUIRED_MIGRATION_VERSION"
check_source_blob_authority
check_source_media_keys
check_source_blob_credentials
if [[ $TEST_MODE == true && ${CUTOVER_TEST_FAIL_AFTER_SOURCE_FENCE:-} == true ]]; then
  fail 'injected disposable-cluster failure after the source write fence'
fi

blob_sizes=$(incus exec "$SOURCE_INSTANCE" -- docker cp \
  teagram-teagramd-1:/var/lib/teagramd-blobs/. - |
  python3 -c '
import sys
import tarfile
from pathlib import PurePosixPath

total = 0
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
    while sys.stdin.buffer.read(1024 * 1024):
        pass
except (tarfile.TarError, OSError, ValueError):
    raise SystemExit("k3s cutover: legacy blob tree contains an unsupported entry")
print(total)
') || fail 'could not inspect the source blob volume safely after the write fence'
[[ $blob_sizes =~ ^[0-9]+$ ]] || fail 'source blob size is unavailable'
blob_bytes=$blob_sizes
(( blob_bytes <= MAX_LEGACY_BLOB_BYTES )) || fail 'source blob volume exceeds the 20 GiB retained PVC'
record_source_s3_size

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
# kubectl exec --stdin can end a large upload early without reporting an error.
# Send the private spool through a loopback port-forward and compare SHA-256
# before restoring it.
# The spool is removed once the restore has finished.
dump_spool="$EVIDENCE_DIR/source-$dump_stamp.dump.partial"
: > "$dump_spool"
chmod 0600 "$dump_spool"
incus exec "$SOURCE_INSTANCE" -- docker exec teagram-postgres-1 \
  pg_dump --format=custom --no-owner --no-acl --username=postgres --dbname=teagram > "$dump_spool"
spool_hash=$(sha256sum "$dump_spool" | awk '{print $1}')
[[ $spool_hash =~ ^[0-9a-f]{64}$ ]] || fail 'could not hash the PostgreSQL cutover dump'
dump_uploaded=false
for upload_attempt in 1 2 3 4 5; do
  if ! bash "$SCRIPT_DIR/cutover-dump-upload.sh" "$NAMESPACE" postgres-0 "$dump_spool" "$dump_path"; then
    printf 'k3s cutover: PostgreSQL dump upload attempt %s failed; retrying\n' "$upload_attempt" >&2
    continue
  fi
  if ! uploaded_hash=$(kubectl exec --namespace "$NAMESPACE" postgres-0 -c postgres -- \
    sha256sum "$dump_path" | awk '{print $1}'); then
    printf 'k3s cutover: could not hash PostgreSQL dump upload attempt %s; retrying\n' "$upload_attempt" >&2
    continue
  fi
  if [[ $uploaded_hash == "$spool_hash" ]]; then
    dump_uploaded=true
    break
  fi
  printf 'k3s cutover: PostgreSQL dump upload attempt %s did not match its SHA-256; retrying\n' "$upload_attempt" >&2
done
[[ $dump_uploaded == true ]] || fail 'the PostgreSQL dump could not be uploaded intact to the k3s PostgreSQL PVC'
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
rm -f -- "$dump_spool"
dump_spool=''
printf '%s\n' "$dump_path" > "$EVIDENCE_DIR/postgres-dump-path"
chmod 0600 "$EVIDENCE_DIR/postgres-dump-path"
record_phase database-restored
target_available_bytes=$(kubectl exec --namespace "$NAMESPACE" postgres-0 -c postgres -- \
  df -Pk /var/lib/postgresql | awk 'END { print $4 * 1024 }')
[[ $target_available_bytes =~ ^[0-9]+$ ]] || fail 'could not inspect target disk space before importing blobs'
(( blob_bytes + S3_BYTES + MIN_STORAGE_RESERVE_BYTES <= target_available_bytes )) ||
  fail 'target disk cannot hold both the retained local blobs and encrypted RustFS copy with 1 GiB free'

for claim in key-data-teagramd-0 legacy-blobs-teagramd-0 blob-mode-teagramd-0; do
  kubectl wait --for=jsonpath='{.status.phase}'=Bound "pvc/$claim" \
    --namespace "$NAMESPACE" --timeout=600s >/dev/null
done
kubectl wait --for=condition=Ready pod/cutover-volume-importer --namespace "$NAMESPACE" --timeout=300s >/dev/null

# New local-path claims can start with permissions that deny writes to the
# importer. Make the private destination root traversable to its restricted
# root process before extracting, and keep archive owners from replacing it.
kubectl exec --namespace "$NAMESPACE" pod/cutover-volume-importer -c importer -- \
  sh -ec 'chown 0:0 /target/legacy-blobs /target/blob-mode; chmod 0700 /target/legacy-blobs /target/blob-mode' >/dev/null

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
  # The restricted importer must own the private file until it verifies it.
  incus exec "$SOURCE_INSTANCE" -- docker cp "teagram-teagramd-1:$source_path" - |
    drain_after kubectl exec --namespace "$NAMESPACE" --stdin pod/cutover-volume-importer -c importer -- \
      tar -xopf - -C /target/key-data
  destination_hash=$(kubectl exec --namespace "$NAMESPACE" pod/cutover-volume-importer -c importer -- \
    sha256sum "/target/key-data/$file_name" | awk '{print $1}')
  [[ $destination_hash == "$source_hash" ]] || fail "copied identity file did not match its source: $file_name"
}

copy_secret_file /var/lib/teagramd/server_key.pem server_key.pem
copy_secret_file /var/lib/teagramd/enc_key.hex enc_key.hex
incus exec "$SOURCE_INSTANCE" -- docker cp teagram-teagramd-1:/var/lib/teagramd-blobs/. - |
  drain_after kubectl exec --namespace "$NAMESPACE" --stdin pod/cutover-volume-importer -c importer -- \
    tar -xopf - -C /target/legacy-blobs
source_blob_mode_hash=$(incus exec "$SOURCE_INSTANCE" -- docker cp \
  teagram-teagramd-1:/run/teagramd/blob-mode/. - | drain_after blob_mode_tar_hash) ||
  fail 'could not inventory the source blob-mode authority'
incus exec "$SOURCE_INSTANCE" -- docker cp teagram-teagramd-1:/run/teagramd/blob-mode/. - |
  drain_after kubectl exec --namespace "$NAMESPACE" --stdin pod/cutover-volume-importer -c importer -- \
    tar -xopf - -C /target/blob-mode
target_blob_mode_hash=$(kubectl exec --namespace "$NAMESPACE" pod/cutover-volume-importer -c importer -- \
  tar -cf - -C /target/blob-mode . | blob_mode_tar_hash) || fail 'could not inventory the copied blob-mode authority'
[[ $source_blob_mode_hash == "$target_blob_mode_hash" ]] ||
  fail 'copied blob-mode authority does not match the live Compose state'
kubectl exec --namespace "$NAMESPACE" pod/cutover-volume-importer -c importer -- \
  sh -ec 'chmod 0600 /target/key-data/server_key.pem /target/key-data/enc_key.hex; chown -R 65532:65532 /target/key-data /target/legacy-blobs; chown -R 0:0 /target/blob-mode; chmod 0755 /target/blob-mode' >/dev/null
kubectl delete pod/cutover-volume-importer --namespace "$NAMESPACE" --wait=true --timeout=300s >/dev/null

migration_jobs=()
while IFS= read -r job; do
  [[ -z $job ]] || migration_jobs+=("$job")
done < <(kubectl get jobs --namespace "$NAMESPACE" \
  --selector=app.kubernetes.io/name=db-migrations --output name)
[[ ${#migration_jobs[@]} -eq 1 ]] || fail 'expected exactly one pinned Atlas migration Job'
kubectl patch "${migration_jobs[0]}" --namespace "$NAMESPACE" --type=merge \
  --patch '{"spec":{"suspend":false}}' >/dev/null
wait_for_job "${migration_jobs[0]}" 900
mirror_encrypted_s3_prefix
stop_rustfs_copy_forward
preserve_media_key_version

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
ExecStart=/usr/local/bin/k3s kubectl port-forward --namespace $NAMESPACE --address $linkselector_ip $target $ports
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
  'Tailnet-only teagramd MTProto, WebSocket, admin, and Bot API port-forward' service/teagramd \
  "$MTPROTO_PORT:$MTPROTO_PORT $WEBSOCKET_PORT:2444 $ADMIN_PORT:2445 $BOT_API_PORT:$BOT_API_PORT"
wait_for_forward() {
  local port=$1 attempt
  for ((attempt = 1; attempt <= 30; attempt++)); do
    if python3 - "$linkselector_ip" "$port" <<'PY'
import socket
import sys

try:
    with socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=1):
        pass
except OSError:
    raise SystemExit(1)
PY
    then
      return 0
    fi
    sleep 2
  done
  printf 'k3s cutover: target port-forward did not become ready on port %s\n' "$port" >&2
  return 1
}
wait_for_http_forward() {
  local url=$1 attempt
  shift
  for ((attempt = 1; attempt <= 30; attempt++)); do
    if incus exec "$TARGET_INSTANCE" -- curl --silent --output /dev/null --max-time 3 "$@" "$url"; then
      return 0
    fi
    sleep 2
  done
  return 1
}
wait_for_http_forward "http://$linkselector_ip:$LINKSELECTOR_PORT/+k3s-smoke" --fail ||
  fail 'selector port-forward readiness timed out'
wait_for_http_forward "http://$linkselector_ip:$LINKSELECTOR_PORT/" --fail ||
  fail 'web app is not reachable through the k3s selector'
wait_for_http_forward "http://$linkselector_ip:$WEBSOCKET_PORT/apiws" ||
  fail 'WebSocket port-forward readiness timed out'
wait_for_http_forward "http://$linkselector_ip:$ADMIN_PORT/admin" ||
  fail 'admin port-forward readiness timed out'
wait_for_forward "$MTPROTO_PORT" || fail 'MTProto port-forward readiness timed out'
wait_for_forward "$BOT_API_PORT" || fail 'Bot API port-forward readiness timed out'
incus exec "$TARGET_INSTANCE" -- curl --fail --silent --show-error --max-time 10 \
  "http://$linkselector_ip:$LINKSELECTOR_PORT/+k3s-smoke" --output /dev/null
web_status=$(incus exec "$TARGET_INSTANCE" -- curl --silent --show-error --output /dev/null \
  --write-out '%{http_code}' --max-time 10 "http://$linkselector_ip:$LINKSELECTOR_PORT/") ||
  fail 'the new web Service is not reachable through the k3s selector'
[[ $web_status == 200 ]] || fail "the web app returned unexpected HTTP status $web_status"
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
[[ $admin_status =~ ^(200|301|302|303|307|308|401|403)$ ]] || fail "the new admin Service returned unexpected HTTP status $admin_status"
python3 - "$linkselector_ip" "$BOT_API_PORT" <<'PY' || fail 'the Bot API request through the k3s relay did not receive an HTTP response'
import socket
import sys

host, port = sys.argv[1], int(sys.argv[2])
request = (
    b"PROXY TCP4 100.64.1.2 100.64.0.1 1234 2447\r\n"
    b"GET /bot0:cutover-smoke-invalid/getMe HTTP/1.1\r\n"
    b"Host: teagram.invalid\r\n"
    b"Connection: close\r\n\r\n"
)
with socket.create_connection((host, port), timeout=5) as connection:
    connection.settimeout(5)
    connection.sendall(request)
    response = bytearray()
    while len(response) < 65536:
        chunk = connection.recv(4096)
        if not chunk:
            break
        response.extend(chunk)
if not response.startswith(b"HTTP/1.1 401 ") or b'"description":"Unauthorized"' not in response:
    raise SystemExit("unexpected Bot API response")
PY
record_phase ready-for-edge
printf '%s\n%s\n%s\n%s\n%s\n' "http://$linkselector_ip:$LINKSELECTOR_PORT" \
  "http://$linkselector_ip:$WEBSOCKET_PORT" "http://$linkselector_ip:$ADMIN_PORT" \
  "$linkselector_ip:$MTPROTO_PORT" "$linkselector_ip:$BOT_API_PORT" > "$EVIDENCE_DIR/serve-target"
chmod 0600 "$EVIDENCE_DIR/serve-target"
printf 'Cloudflare dashboard route: %s -> %s\n' "$CLOUDFLARE_BEFORE" "$CLOUDFLARE_AFTER" >&2
record_phase serve-switch-started
incus exec "$SOURCE_INSTANCE" -- tailscale serve --bg --https=443 --set-path=/ \
  "http://$linkselector_ip:$LINKSELECTOR_PORT" >/dev/null
record_phase serve-switched
set_serve_routes "http://$linkselector_ip:$LINKSELECTOR_PORT" \
  "http://$linkselector_ip:$WEBSOCKET_PORT" "http://$linkselector_ip:$ADMIN_PORT" \
  "$linkselector_ip:$MTPROTO_PORT" "$linkselector_ip:$BOT_API_PORT" true
SERVE_AFTER=$(mktemp)
serve_status_file "$SERVE_AFTER"
check_serve_status "$SERVE_AFTER" "http://$linkselector_ip:$LINKSELECTOR_PORT" \
  "http://$linkselector_ip:$WEBSOCKET_PORT" "http://$linkselector_ip:$ADMIN_PORT" \
  "$linkselector_ip:$MTPROTO_PORT" "$linkselector_ip:$BOT_API_PORT"
install_private_file 0600 "$SERVE_AFTER" "$EVIDENCE_DIR/serve-after.json"
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
printf 'k3s cutover complete: postgres_dump=%s legacy_blob_bytes=%s encrypted_blob_bytes=%s s3_objects=%s evidence=%s\n' \
  "$dump_path" "$blob_bytes" "$S3_BYTES" "$S3_OBJECTS" "$EVIDENCE_DIR"
print_rollback_command
