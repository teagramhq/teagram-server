#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/teagram-demo/demo-common.sh
source "$SCRIPT_DIR/demo-common.sh"

readonly SHA=${DEPLOY_SHA:-}
readonly PREV_SHA=${PREVIOUS_SHA:-}
readonly MODE_HELPER="$DEMO_SCRIPT_DIR/../teagramd/rollout-runner/blob-mode-state.py"
TS=$(date -u +%Y%m%dT%H%M%SZ)
readonly TS
TMP_DIR=
BACKUP_DIR=
PREV_IMAGE_ID=
PREV_IMAGE_TAG=
DB_DUMP=
RESTORE_DB=
RESTORE_DB_CREATED=0
BACKUP_READY=0
DATABASE_MUTATION=0
SERVICES_STOPPED=0
PUBLIC_ACTIVATION_STARTED=0

demo_cleanup() {
  local rc=$?
  trap - EXIT
  if [[ -n "$RESTORE_DB" && "$RESTORE_DB_CREATED" == 1 ]]; then
    demo_compose exec -T postgres dropdb -U postgres --if-exists "$RESTORE_DB" </dev/null ||
      printf 'could not remove isolated restore database %s\n' "$RESTORE_DB" >&2
  fi
  if (( rc != 0 && BACKUP_READY == 1 )); then
    if (( PUBLIC_ACTIVATION_STARTED == 1 )); then
      demo_preserve_after_activation_failure ||
        printf 'could not confirm public edge stopped; inspect deployment backup %s\n' "$BACKUP_DIR" >&2
    else
      demo_rollback || printf 'demo rollback failed; keep services stopped and inspect %s\n' "$BACKUP_DIR" >&2
    fi
  fi
  if [[ -n "$TMP_DIR" && -d "$TMP_DIR" ]]; then
    rm -rf -- "$TMP_DIR"
  fi
  exit "$rc"
}
trap demo_cleanup EXIT

demo_rollback_stop() {
  if demo_compose config --services | grep -Fxq haproxy; then
    if demo_compose ps -q turnstile-egress | grep -q .; then
      demo_compose stop edge haproxy teagramd turnstile-egress
    else
      demo_compose stop edge haproxy teagramd
    fi
  else
    demo_compose stop edge teagramd
  fi
}

demo_preserve_after_activation_failure() {
  local stop_rc=0
  set +e
  demo_compose stop edge haproxy >/dev/null 2>&1 || stop_rc=1
  printf 'demo activation failed; public edge stopped and deployed database preserved sha=%s backup=%s\n' \
    "$SHA" "$DB_DUMP" >&2
  return "$stop_rc"
}

demo_rollback() {
  local rollback_rc=0 migrate_ids migrate_status
  set +e
  if (( SERVICES_STOPPED == 1 || DATABASE_MUTATION == 1 )); then
    demo_rollback_stop >/dev/null 2>&1 || rollback_rc=1
  fi
  if (( DATABASE_MUTATION == 1 )); then
    demo_compose exec -T postgres dropdb -U postgres --if-exists teagram </dev/null || rollback_rc=1
    demo_compose exec -T postgres createdb -U postgres teagram </dev/null || rollback_rc=1
    demo_compose_input exec -T postgres pg_restore -U postgres --no-owner --no-privileges --exit-on-error -d teagram \
      < "$DB_DUMP" || rollback_rc=1
  fi
  git -C "$DEMO_REPO_ROOT" reset --hard "$PREV_SHA" || rollback_rc=1
  docker image tag "$PREV_IMAGE_ID" teagramd:local || rollback_rc=1
  cp -- "$BACKUP_DIR/client.json" "$DEMO_STATE_DIR/client.json.next" || rollback_rc=1
  chown root:root "$DEMO_STATE_DIR/client.json.next" || rollback_rc=1
  chmod 0644 "$DEMO_STATE_DIR/client.json.next" || rollback_rc=1
  mv -f -- "$DEMO_STATE_DIR/client.json.next" "$DEMO_STATE_DIR/client.json" || rollback_rc=1
  if (( SERVICES_STOPPED == 1 && rollback_rc == 0 )); then
    demo_compose rm -sf migrate >/dev/null 2>&1 || rollback_rc=1
    if (( rollback_rc == 0 )); then
      demo_compose up -d migrate || rollback_rc=1
    fi
    if (( rollback_rc == 0 )); then
      migrate_ids=$(demo_compose ps -a -q migrate)
      if [[ -z "$migrate_ids" ]]; then
        rollback_rc=1
      else
        migrate_status=$(docker inspect --format '{{.State.Status}} {{.State.ExitCode}}' "$migrate_ids" 2>/dev/null)
        [[ "$migrate_status" == 'exited 0' ]] || rollback_rc=1
      fi
    fi
    if (( rollback_rc == 0 )); then
      demo_compose up -d teagramd edge || rollback_rc=1
    fi
    if (( rollback_rc == 0 )); then
      python3 "$SCRIPT_DIR/smoke.py" "$DEMO_STATE_DIR/client.json" || rollback_rc=1
    fi
  fi
  if (( rollback_rc != 0 && SERVICES_STOPPED == 1 )); then
    demo_rollback_stop >/dev/null 2>&1 || true
  fi
  if (( rollback_rc == 0 )); then
    printf 'demo rollback=verified previous_sha=%s dump=%s\n' "$PREV_SHA" "$DB_DUMP" >&2
    return 0
  fi
  printf 'demo rollback=failed previous_sha=%s dump=%s\n' "$PREV_SHA" "$DB_DUMP" >&2
  return 1
}

demo_require_root
demo_require_env
[[ "$SHA" =~ ^[0-9a-f]{40}$ ]] || demo_fail 'DEPLOY_SHA must be a full 40-hex commit SHA'
[[ "$PREV_SHA" =~ ^[0-9a-f]{40}$ ]] || demo_fail 'PREVIOUS_SHA must be a full 40-hex commit SHA'
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

LOCK_PATH="$DEMO_REPO_ROOT/.state/deploy.lock"
mkdir -p "$(dirname "$LOCK_PATH")"
exec 9> "$LOCK_PATH"
flock -x 9

TARGET_RENDER="$TMP_DIR/target-render.json"
TARGET_INVENTORY="$TMP_DIR/target-inventory.json"
demo_render_config "$TARGET_RENDER"
demo_check_blob_mount "$TARGET_RENDER"
mapfile -t CONTAINER_IDS < <(demo_compose ps -q)
((${#CONTAINER_IDS[@]} >= 4)) || demo_fail 'demo database, server, HAProxy, or edge container is not running'
docker inspect "${CONTAINER_IDS[@]}" > "$TMP_DIR/containers.raw.json"
mapfile -t POSTGRES_IDS < <(demo_compose ps -q postgres)
((${#POSTGRES_IDS[@]} == 1)) || demo_fail 'running demo database is missing or ambiguous'
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

mapfile -t SERVER_IDS < <(demo_compose ps -q teagramd)
((${#SERVER_IDS[@]} == 1)) || demo_fail 'running demo server is missing or ambiguous'
PREV_IMAGE_ID=$(docker inspect --format '{{.Image}}' "${SERVER_IDS[0]}")
[[ "$PREV_IMAGE_ID" =~ ^sha256:[0-9a-f]{64}$ ]] || demo_fail 'running demo image ID is invalid'
PREV_IMAGE_TAG="teagramd:rollback-${PREV_SHA:0:12}-$TS"
docker image tag "$PREV_IMAGE_ID" "$PREV_IMAGE_TAG"
printf '%s\n' "$PREV_IMAGE_ID" > "$BACKUP_DIR/previous-image-id.txt"
cp -- "$DEMO_STATE_DIR/client.json" "$BACKUP_DIR/client.json"
chown root:root "$BACKUP_DIR/previous-image-id.txt" "$BACKUP_DIR/client.json"
chmod 0600 "$BACKUP_DIR/previous-image-id.txt" "$BACKUP_DIR/client.json"

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
' "$TMP_DIR/client.json" >/dev/null || demo_fail 'client-config lacks the approved demo WebSocket binding'
OLD_SPKI=$(jq -er '.mtproto.rsa_spki' "$BACKUP_DIR/client.json")
NEW_SPKI=$(jq -er '.mtproto.rsa_spki' "$TMP_DIR/client.json")
[[ "$NEW_SPKI" == "$OLD_SPKI" ]] || demo_fail 'deployment changed the persistent RSA identity'

SERVICES_STOPPED=1
demo_compose stop edge haproxy teagramd turnstile-egress
DATABASE_MUTATION=1
demo_compose up -d migrate
mapfile -t MIGRATE_IDS < <(demo_compose ps -a -q migrate)
((${#MIGRATE_IDS[@]} == 1)) || demo_fail 'migration container is missing or ambiguous'
MIGRATE_STATUS=$(docker inspect --format '{{.State.Status}} {{.State.ExitCode}}' "${MIGRATE_IDS[0]}")
[[ "$MIGRATE_STATUS" == 'exited 0' ]] || demo_fail "database migration failed ($MIGRATE_STATUS)"
install -o root -g root -m 0644 "$TMP_DIR/client.json" "$DEMO_STATE_DIR/client.json.next"
mv -f -- "$DEMO_STATE_DIR/client.json.next" "$DEMO_STATE_DIR/client.json"
demo_compose up -d teagramd
demo_validation_compose up -d haproxy edge
python3 "$SCRIPT_DIR/smoke.py" "$DEMO_STATE_DIR/client.json" 18080
mapfile -t SERVER_IDS < <(demo_compose ps -q teagramd)
((${#SERVER_IDS[@]} == 1)) || demo_fail 'deployed server is missing or ambiguous'
IMAGE_ID=$(docker inspect --format '{{.Image}}' "${SERVER_IDS[0]}")
[[ "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$IMAGE_ID")" == "$SHA" ]] ||
  demo_fail 'running server image does not match the requested SHA'
demo_validation_compose stop edge
PUBLIC_ACTIVATION_STARTED=1
demo_compose up -d edge
BACKUP_READY=0
printf 'demo deployed sha=%s previous_sha=%s backup=%s\n' "$SHA" "$PREV_SHA" "$DB_DUMP"
