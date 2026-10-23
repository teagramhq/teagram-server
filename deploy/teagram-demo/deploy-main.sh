#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/teagram-demo/demo-common.sh
source "$SCRIPT_DIR/demo-common.sh"

readonly SHA=${DEPLOY_SHA:-}
readonly PREV_SHA=${PREVIOUS_SHA:-}
readonly MODE_HELPER="$DEMO_SCRIPT_DIR/../teagramd/rollout-runner/blob-mode-state.py"
ACTIVE_SERVER=$(demo_active_server)
CANDIDATE_SERVER=$(demo_inactive_server)
readonly ACTIVE_SERVER CANDIDATE_SERVER
readonly CANDIDATE_ENDPOINT="$CANDIDATE_SERVER:2443"
TS=$(date -u +%Y%m%dT%H%M%SZ)
readonly TS
TMP_DIR=
BACKUP_DIR=
PREV_IMAGE_ID=
DB_DUMP=
RESTORE_DB=
RESTORE_DB_CREATED=0
BACKUP_READY=0
CANDIDATE_STARTED=0
PUBLIC_ACTIVATION_STARTED=0
CUTOVER_COMMITTED=0
CLIENT_CONFIG_UPDATED=0

demo_remove_previous_image() {
  local previous_image_id="$1" active_image_id="$2"
  [[ "$previous_image_id" != "$active_image_id" ]] || return 0
  docker image rm "$previous_image_id" ||
    printf 'could not remove previous image %s\n' "$previous_image_id" >&2
}

demo_cleanup() {
  local rc=$?
  local rollback_rc=0 rollback_confirmed=1
  trap - EXIT
  if (( rc != 0 && BACKUP_READY == 1 )); then
    if (( CUTOVER_COMMITTED == 1 )); then
      printf 'demo rollout failed after cutover; active server=%s and database preserved sha=%s backup=%s\n' \
        "$CANDIDATE_SERVER" "$SHA" "$BACKUP_DIR" >&2
    else
      if (( PUBLIC_ACTIVATION_STARTED == 1 )); then
        rollback_confirmed=0
        if demo_haproxy_control enable "$ACTIVE_SERVER" &&
          demo_haproxy_control wait-healthy "$ACTIVE_SERVER" --timeout 60 &&
          demo_haproxy_control drain "$CANDIDATE_SERVER"; then
          if (( CLIENT_CONFIG_UPDATED == 1 )); then
            cp -- "$BACKUP_DIR/client.json" "$DEMO_CLIENT_CONFIG.next" || rollback_rc=1
            chown root:root "$DEMO_CLIENT_CONFIG.next" || rollback_rc=1
            chmod 0644 "$DEMO_CLIENT_CONFIG.next" || rollback_rc=1
            mv -f -- "$DEMO_CLIENT_CONFIG.next" "$DEMO_CLIENT_CONFIG" || rollback_rc=1
          fi
          demo_write_active_server "$ACTIVE_SERVER" || rollback_rc=1
          demo_prepare_haproxy_runtime || rollback_rc=1
          if (( rollback_rc == 0 )); then
            rollback_confirmed=1
            printf 'demo rollout reverted to server=%s; migrated database preserved backup=%s\n' \
              "$ACTIVE_SERVER" "$BACKUP_DIR" >&2
          else
            printf 'demo rollout could not persist rollback to server=%s; candidate remains running; backup=%s\n' \
              "$ACTIVE_SERVER" "$BACKUP_DIR" >&2
            rc=1
          fi
        else
          printf 'demo rollout could not confirm server=%s healthy and restored; candidate remains running and backup=%s\n' \
            "$ACTIVE_SERVER" "$BACKUP_DIR" >&2
          rc=1
        fi
      fi
      if (( CANDIDATE_STARTED == 1 )); then
        if (( rollback_confirmed == 1 )); then
          demo_compose stop "$CANDIDATE_SERVER" || rc=1
        else
          printf 'demo rollout left candidate server=%s running because rollback was not confirmed\n' \
            "$CANDIDATE_SERVER" >&2
        fi
      fi
      if [[ -n "$PREV_IMAGE_ID" ]]; then
        docker image tag "$PREV_IMAGE_ID" teagramd:local || rc=1
      fi
      printf 'demo rollout retained previous server=%s database state and backup=%s\n' \
        "$ACTIVE_SERVER" "$BACKUP_DIR" >&2
    fi
  fi
  if [[ -n "$RESTORE_DB" && "$RESTORE_DB_CREATED" == 1 ]]; then
    demo_compose exec -T postgres dropdb -U postgres --if-exists "$RESTORE_DB" </dev/null ||
      printf 'could not remove isolated restore database %s\n' "$RESTORE_DB" >&2
  fi
  if [[ -n "$TMP_DIR" && -d "$TMP_DIR" ]]; then
    rm -rf -- "$TMP_DIR"
  fi
  exit "$rc"
}
trap demo_cleanup EXIT

demo_backup_database() {
  DB_DUMP="$BACKUP_DIR/postgres.dump"
  demo_compose exec -T postgres pg_dump -U postgres -Fc teagram </dev/null > "$DB_DUMP"
  chown root:root "$DB_DUMP"
  chmod 0600 "$DB_DUMP"
  [[ -s "$DB_DUMP" ]] || demo_fail 'database backup is empty'
  demo_compose_input exec -T postgres pg_restore --list < "$DB_DUMP" > "$TMP_DIR/restore.list"
  grep -q 'TABLE DATA' "$TMP_DIR/restore.list" || demo_fail 'database backup failed its restore-list check'
  RESTORE_DB="teagram_restore_${SHA:0:12}"
  RESTORE_DB_EXISTS=$(demo_compose exec -T postgres psql -U postgres -Atqc \
    "SELECT 1 FROM pg_database WHERE datname = '$RESTORE_DB'" </dev/null)
  [[ -z "$RESTORE_DB_EXISTS" ]] || demo_fail 'isolated restore database name is already in use'
  demo_compose exec -T postgres createdb -U postgres "$RESTORE_DB" </dev/null
  RESTORE_DB_CREATED=1
  demo_compose_input exec -T postgres pg_restore -U postgres --no-owner --no-privileges --exit-on-error \
    -d "$RESTORE_DB" < "$DB_DUMP"
  demo_compose exec -T postgres dropdb -U postgres "$RESTORE_DB" </dev/null
  RESTORE_DB_CREATED=0
  sha256sum "$DB_DUMP" > "$BACKUP_DIR/postgres.dump.sha256"
  chown root:root "$BACKUP_DIR/postgres.dump.sha256"
  chmod 0600 "$BACKUP_DIR/postgres.dump.sha256"
  BACKUP_READY=1
}

demo_require_root
demo_require_env
[[ "$SHA" =~ ^[0-9a-f]{40}$ ]] || demo_fail 'DEPLOY_SHA must be a full 40-hex commit SHA'
[[ "$PREV_SHA" =~ ^[0-9a-f]{40}$ ]] || demo_fail 'PREVIOUS_SHA must be a full 40-hex commit SHA'
LOCK_PATH="$DEMO_REPO_ROOT/.state/deploy.lock"
if [[ "${DEMO_DEPLOY_LOCK_HELD:-0}" != 1 ]]; then
  mkdir -p "$(dirname "$LOCK_PATH")"
  exec 9> "$LOCK_PATH"
  flock -x 9
fi
[[ "$(git -C "$DEMO_REPO_ROOT" rev-parse HEAD)" == "$SHA" ]] || demo_fail 'checkout is not the requested deployment SHA'
git -C "$DEMO_REPO_ROOT" fetch -q origin +refs/heads/main:refs/remotes/origin/main
git -C "$DEMO_REPO_ROOT" merge-base --is-ancestor "$SHA" origin/main || demo_fail 'deployment SHA is no longer on origin/main'
git -C "$DEMO_REPO_ROOT" diff --quiet HEAD -- || demo_fail 'checkout has tracked changes'

mkdir -p "$DEMO_STATE_DIR/backups" "$DEMO_STATE_DIR/reports"
chown root:root "$DEMO_STATE_DIR" "$DEMO_STATE_DIR/backups" "$DEMO_STATE_DIR/reports"
chmod 0755 "$DEMO_STATE_DIR"
chmod 0700 "$DEMO_STATE_DIR/backups" "$DEMO_STATE_DIR/reports"
TMP_DIR=$(mktemp -d "$DEMO_STATE_DIR/deploy.XXXXXXXX")
chmod 0700 "$TMP_DIR"
BACKUP_DIR="$DEMO_STATE_DIR/backups/deploy-${SHA:0:12}-$TS"
mkdir -m 0700 -- "$BACKUP_DIR"
chown root:root "$BACKUP_DIR"

TARGET_RENDER="$TMP_DIR/target-render.json"
TARGET_INVENTORY="$TMP_DIR/target-inventory.json"
demo_render_config "$TARGET_RENDER"
demo_check_blob_mount "$TARGET_RENDER"
mapfile -t CONTAINER_IDS < <(demo_compose ps -q)
(("${#CONTAINER_IDS[@]}" >= 4)) || demo_fail 'demo database, server, HAProxy, or edge container is not running'
[[ -z "$(demo_compose ps --status running -q "$CANDIDATE_SERVER")" ]] ||
  demo_fail 'candidate server slot is already running; reconcile the rollout before deploying'
docker inspect "${CONTAINER_IDS[@]}" > "$TMP_DIR/containers.raw.json"
mapfile -t POSTGRES_IDS < <(demo_compose ps -q postgres)
(("${#POSTGRES_IDS[@]}" == 1)) || demo_fail 'running demo database is missing or ambiguous'
TARGET_POSTGRES_HASH=$(demo_compose config --hash postgres | awk '$1 == "postgres" {print $2}')
LIVE_POSTGRES_HASH=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.config-hash"}}' "${POSTGRES_IDS[0]}")
[[ "$TARGET_POSTGRES_HASH" =~ ^[0-9a-f]{64}$ && "$LIVE_POSTGRES_HASH" == "$TARGET_POSTGRES_HASH" ]] ||
  demo_fail 'PostgreSQL Compose settings changed; handle database image or volume migrations separately'
python3 "$MODE_HELPER" containers --checkout "$DEMO_REPO_ROOT" \
  < "$TMP_DIR/containers.raw.json" > "$TMP_DIR/containers.json"
python3 "$MODE_HELPER" compose --checkout "$DEMO_REPO_ROOT" \
  < "$TARGET_RENDER" > "$TARGET_INVENTORY"
python3 "$MODE_HELPER" validate \
  --state-dir "$DEMO_AUTHORITY_DIR" \
  --report-root "$DEMO_STATE_DIR/reports" \
  --containers "$TMP_DIR/containers.json" \
  --compose "$TARGET_INVENTORY" \
  --override "$SCRIPT_DIR/compose.empty-overrides.yaml" \
  --checkout "$DEMO_REPO_ROOT"

mapfile -t SERVER_IDS < <(demo_compose ps -q "$ACTIVE_SERVER")
(("${#SERVER_IDS[@]}" == 1)) || demo_fail 'active demo server is missing or ambiguous'
PREV_IMAGE_ID=$(docker inspect --format '{{.Image}}' "${SERVER_IDS[0]}")
[[ "$PREV_IMAGE_ID" =~ ^sha256:[0-9a-f]{64}$ ]] || demo_fail 'running demo image ID is invalid'
printf '%s\n' "$PREV_IMAGE_ID" > "$BACKUP_DIR/previous-image-id.txt"
printf '%s\n' "$ACTIVE_SERVER" > "$BACKUP_DIR/active-server.txt"
cp -- "$DEMO_CLIENT_CONFIG" "$BACKUP_DIR/client.json"
chown root:root "$BACKUP_DIR/previous-image-id.txt" "$BACKUP_DIR/active-server.txt" "$BACKUP_DIR/client.json"
chmod 0600 "$BACKUP_DIR/previous-image-id.txt" "$BACKUP_DIR/active-server.txt" "$BACKUP_DIR/client.json"

# The snapshot is taken online. Existing clients keep using the old server while
# the image builds, migrations apply, and the candidate is checked.
demo_backup_database

export TEAGRAMD_SOURCE_REVISION="$SHA"
demo_compose build teagramd
IMAGE_REVISION=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' teagramd:local)
[[ "$IMAGE_REVISION" == "$SHA" ]] || demo_fail 'built server image does not identify the requested SHA'
demo_compose run --rm --no-deps teagramd client-config > "$TMP_DIR/client.json"
jq -e '
  .version == 1
  and .mtproto.endpoint == "demo.teagram.io:443"
  and (.mtproto.rsa_spki | type == "string" and length > 0)
  and .websocket.endpoint == "wss://demo.teagram.io/apiws"
  and .demo.guest_sign_up == true
' "$TMP_DIR/client.json" >/dev/null || demo_fail 'client-config lacks the approved demo WebSocket or guest signup binding'
OLD_SPKI=$(jq -er '.mtproto.rsa_spki' "$BACKUP_DIR/client.json")
NEW_SPKI=$(jq -er '.mtproto.rsa_spki' "$TMP_DIR/client.json")
[[ "$NEW_SPKI" == "$OLD_SPKI" ]] || demo_fail 'deployment changed the persistent RSA identity'

demo_compose rm -sf migrate >/dev/null
demo_compose up -d migrate
demo_wait_migration demo_compose 'deployment migrations'
CANDIDATE_STARTED=1
demo_seed_compose up -d "$CANDIDATE_SERVER"
mapfile -t CANDIDATE_IDS < <(demo_compose ps -q "$CANDIDATE_SERVER")
(("${#CANDIDATE_IDS[@]}" == 1)) || demo_fail 'candidate server is missing or ambiguous'
CANDIDATE_IMAGE_ID=$(docker inspect --format '{{.Image}}' "${CANDIDATE_IDS[0]}")
CANDIDATE_IMAGE_REVISION=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$CANDIDATE_IMAGE_ID")
[[ "$CANDIDATE_IMAGE_REVISION" == "$SHA" ]] || demo_fail 'running candidate image does not match the requested SHA'
"$SCRIPT_DIR/seed-demo.sh" --verify-seed --endpoint "$CANDIDATE_ENDPOINT"
demo_compose up -d --wait --wait-timeout 120 "$CANDIDATE_SERVER"

# HAProxy checks both candidate listeners before routing to them. The previous
# server remains enabled until both candidate checks pass.
PUBLIC_ACTIVATION_STARTED=1
demo_haproxy_control enable "$CANDIDATE_SERVER"
demo_haproxy_control wait-healthy "$CANDIDATE_SERVER" --timeout 60
demo_haproxy_control drain "$ACTIVE_SERVER"
install -o root -g root -m 0644 "$TMP_DIR/client.json" "$DEMO_CLIENT_CONFIG.next"
mv -f -- "$DEMO_CLIENT_CONFIG.next" "$DEMO_CLIENT_CONFIG"
CLIENT_CONFIG_UPDATED=1
python3 "$SCRIPT_DIR/smoke.py" "$DEMO_CLIENT_CONFIG"
"$SCRIPT_DIR/seed-demo.sh" --verify-turnstile --endpoint haproxy:2443
demo_write_active_server "$CANDIDATE_SERVER"
demo_prepare_haproxy_runtime
CUTOVER_COMMITTED=1

# New sessions route to the candidate while HAProxy drains the old server's
# existing connections before its graceful stop signal.
demo_compose stop "$ACTIVE_SERVER"
demo_remove_previous_image "$PREV_IMAGE_ID" "$CANDIDATE_IMAGE_ID"
BACKUP_READY=0
printf 'demo deployed sha=%s previous_sha=%s active_server=%s backup=%s\n' \
  "$SHA" "$PREV_SHA" "$CANDIDATE_SERVER" "$BACKUP_DIR"
