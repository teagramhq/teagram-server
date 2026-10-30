#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/teagram-demo/demo-common.sh
source "$SCRIPT_DIR/demo-common.sh"

readonly SHA=${DEPLOY_SHA:-}
readonly IMAGE_DIGEST=${DEPLOY_IMAGE_DIGEST:-}
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
PREV_IMAGE_REF=
CANDIDATE_IMAGE_REF=
DB_DUMP=
RESTORE_DB=
RESTORE_DB_CREATED=0
BACKUP_GENERATION=
BACKUP_READY=0
CANDIDATE_STARTED=0
PUBLIC_ACTIVATION_STARTED=0
CUTOVER_COMMITTED=0
CLIENT_CONFIG_UPDATED=0
EDGE_CONFIG_RELOAD_ATTEMPTED=0
EDGE_CONTAINER_ID=
PREV_WEB_RELEASE=
PREV_WEB_LINK=

demo_remove_previous_image() {
  local previous_image_id="$1" active_image_id="$2"
  [[ "$previous_image_id" != "$active_image_id" ]] || return 0
  docker image rm "$previous_image_id" ||
    printf 'could not remove previous image %s\n' "$previous_image_id" >&2
}

demo_restore_legacy_edge_after_rollback() {
  local legacy_compose="$TMP_DIR/legacy-edge-compose.yaml"
  local legacy_nginx="$TMP_DIR/legacy-edge-nginx.conf"
  local legacy_haproxy="$TMP_DIR/legacy-edge-haproxy.cfg"
  local legacy_haproxy_renderer="$TMP_DIR/legacy-edge-render-haproxy-config.py"
  local legacy_edge_hash legacy_haproxy_hash legacy_image_ref mounts legacy_nginx_changed=0
  local -a edge_ids=()

  if git -C "$DEMO_REPO_ROOT" cat-file -e \
    "$PREV_SHA:deploy/teagram-demo/nginx-config/nginx.conf" 2>/dev/null; then
    return 0
  fi
  [[ -d "$TMP_DIR" ]] || {
    demo_fail 'temporary directory is unavailable for legacy edge rollback'
    return 1
  }
  if ! git -C "$DEMO_REPO_ROOT" show "$PREV_SHA:deploy/teagram-demo/compose.yaml" > "$legacy_compose" ||
    ! git -C "$DEMO_REPO_ROOT" show "$PREV_SHA:deploy/teagram-demo/nginx.conf" > "$legacy_nginx" ||
    ! git -C "$DEMO_REPO_ROOT" show "$PREV_SHA:deploy/teagram-demo/haproxy.cfg" > "$legacy_haproxy" ||
    ! git -C "$DEMO_REPO_ROOT" show "$PREV_SHA:deploy/teagram-demo/render-haproxy-config.py" > "$legacy_haproxy_renderer"; then
    demo_fail 'previous demo proxy configuration could not be restored'
    return 1
  fi
  if ! grep -Fq 'source: ./deploy/teagram-demo/nginx.conf' "$legacy_compose" ||
    ! grep -Fq 'target: /etc/nginx/nginx.conf' "$legacy_compose"; then
    demo_fail 'previous demo Compose file does not define the supported legacy edge mount'
    return 1
  fi

  if cmp -s "$legacy_nginx" "$SCRIPT_DIR/nginx.conf"; then
    rm -f -- "$legacy_nginx"
  else
    if ! chown root:root "$legacy_nginx" || ! chmod 0644 "$legacy_nginx" ||
      ! mv -f -- "$legacy_nginx" "$SCRIPT_DIR/nginx.conf"; then
      demo_fail 'previous Nginx config could not be restored at its stable file path'
      return 1
    fi
    legacy_nginx_changed=1
  fi
  legacy_edge_hash=$(sha256sum -- "$SCRIPT_DIR/nginx.conf" | awk '{print $1}') || return 1
  legacy_haproxy_hash=$(
    sha256sum -- "$legacy_haproxy" "$legacy_haproxy_renderer" |
      awk '{print $1}' | sha256sum | awk '{print $1}'
  ) || return 1
  [[ "$legacy_edge_hash" =~ ^[0-9a-f]{64}$ && "$legacy_haproxy_hash" =~ ^[0-9a-f]{64}$ ]] || {
    demo_fail 'previous proxy configuration hash is invalid'
    return 1
  }
  if ! legacy_image_ref=$(demo_active_image_ref); then
    return 1
  fi
  mapfile -t edge_ids < <(demo_compose ps -a -q edge)
  ((${#edge_ids[@]} <= 1)) || {
    demo_fail 'demo edge is ambiguous during legacy mount rollback'
    return 1
  }
  if ((${#edge_ids[@]} == 1)); then
    if ! mounts=$(docker inspect --format \
      '{{range .Mounts}}{{printf "%s|%s|%s\n" .Type .Source .Destination}}{{end}}' "${edge_ids[0]}"); then
      demo_fail 'demo edge mounts could not be read during legacy mount rollback'
      return 1
    fi
    if grep -Fqx -- "bind|$SCRIPT_DIR/nginx.conf|/etc/nginx/nginx.conf" <<< "$mounts"; then
      if (( legacy_nginx_changed == 0 )); then
        EDGE_CONTAINER_ID=${edge_ids[0]}
        docker exec "$EDGE_CONTAINER_ID" nginx -t >/dev/null || {
          demo_fail 'restored legacy edge configuration failed its syntax check'
          return 1
        }
        return 0
      fi
    elif ! grep -Fqx -- "bind|$SCRIPT_DIR/nginx-config|/etc/nginx/demo" <<< "$mounts"; then
      demo_fail 'demo edge has an unsupported mount during legacy rollback'
      return 1
    fi
  fi
  env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
    -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE \
    -u TEAGRAM_DEMO_EDGE_BIND \
    "TEAGRAM_DEMO_IMAGE_REF=$legacy_image_ref" \
    "TEAGRAM_DEMO_EDGE_CONFIG_SHA256=$legacy_edge_hash" \
    "TEAGRAM_DEMO_HAPROXY_CONFIG_SHA256=$legacy_haproxy_hash" \
    docker compose --env-file "$DEMO_ENV_FILE" --project-directory "$DEMO_REPO_ROOT" \
      --project-name teagram-demo --file "$legacy_compose" \
      up -d --no-deps --force-recreate edge </dev/null || {
        demo_fail 'edge could not be restored from the previous file-mount Compose config'
        return 1
      }
  mapfile -t edge_ids < <(docker ps -q \
    --filter label=com.docker.compose.project=teagram-demo \
    --filter label=com.docker.compose.service=edge)
  ((${#edge_ids[@]} == 1)) || {
    demo_fail 'restored legacy demo edge is missing or ambiguous'
    return 1
  }
  EDGE_CONTAINER_ID=${edge_ids[0]}
  demo_edge_uses_legacy_file_mount "$EDGE_CONTAINER_ID" || {
    demo_fail 'restored demo edge does not use the previous single-file config mount'
    return 1
  }
  docker exec "$EDGE_CONTAINER_ID" nginx -t >/dev/null || {
    demo_fail 'restored legacy edge configuration failed its syntax check'
    return 1
  }
  printf 'demo edge restored to the legacy file mount for checkout %s\n' "$PREV_SHA" >&2
}

demo_cleanup() {
  local rc=$?
  local rollback_rc=0 rollback_confirmed=1 edge_rollback_rc nginx_rollback_file
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
          export TEAGRAM_DEMO_IMAGE_REF="$PREV_IMAGE_REF"
          if [[ -n "${PREV_WEB_RELEASE:-}" ]]; then
            demo_write_active_release "$ACTIVE_SERVER" "$PREV_IMAGE_REF" \
              "${PREV_WEB_RELEASE%% *}" "sha256:${PREV_WEB_RELEASE#* }" || rollback_rc=1
          else
            demo_write_active_release "$ACTIVE_SERVER" "$PREV_IMAGE_REF" || rollback_rc=1
          fi
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
      printf 'demo rollout retained previous server=%s database state and backup=%s\n' \
        "$ACTIVE_SERVER" "$BACKUP_DIR" >&2
    fi
  fi
  if (( rc != 0 && ${CUTOVER_COMMITTED:-0} == 0 && ${EDGE_CONFIG_RELOAD_ATTEMPTED:-0} == 1 )); then
    edge_rollback_rc=0
    if git -C "$DEMO_REPO_ROOT" cat-file -e \
      "$PREV_SHA:deploy/teagram-demo/nginx-config/nginx.conf" 2>/dev/null; then
      nginx_rollback_file="$SCRIPT_DIR/nginx-config/.nginx.conf.rollback.$$"
      if ! git -C "$DEMO_REPO_ROOT" show \
        "$PREV_SHA:deploy/teagram-demo/nginx-config/nginx.conf" > "$nginx_rollback_file" ||
        ! chown root:root "$nginx_rollback_file" || ! chmod 0644 "$nginx_rollback_file" ||
        ! mv -f -- "$nginx_rollback_file" "$SCRIPT_DIR/nginx-config/nginx.conf" ||
        ! demo_reload_edge; then
        rm -f -- "$nginx_rollback_file"
        edge_rollback_rc=1
      fi
    else
      demo_restore_legacy_edge_after_rollback || edge_rollback_rc=1
    fi
    if (( edge_rollback_rc == 0 )); then
      printf 'demo edge configuration rolled back to %s\n' "$PREV_SHA" >&2
    else
      printf 'demo edge configuration rollback could not be confirmed; inspect the public edge\n' >&2
      rc=1
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
  BACKUP_HAS_GENERATION=$(demo_compose exec -T postgres psql -U postgres -d "$RESTORE_DB" -Atqc \
    "SELECT to_regclass('public.demo_wipe_generation') IS NOT NULL" </dev/null)
  [[ "$BACKUP_HAS_GENERATION" == t || "$BACKUP_HAS_GENERATION" == f ]] ||
    demo_fail 'could not inspect the database backup for its wipe generation'
  if [[ "$BACKUP_HAS_GENERATION" == t ]]; then
    BACKUP_GENERATION=$(demo_compose exec -T postgres psql -U postgres -d "$RESTORE_DB" -Atqc \
      'SELECT generation FROM demo_wipe_generation WHERE singleton = 1' </dev/null)
    [[ "$BACKUP_GENERATION" =~ ^[0-9a-f]{64}$ ]] || demo_fail 'database backup has an invalid or missing wipe generation'
  fi
  demo_compose exec -T postgres dropdb -U postgres "$RESTORE_DB" </dev/null
  RESTORE_DB_CREATED=0
  sha256sum "$DB_DUMP" > "$BACKUP_DIR/postgres.dump.sha256"
  chown root:root "$BACKUP_DIR/postgres.dump.sha256"
  chmod 0600 "$BACKUP_DIR/postgres.dump.sha256"
  BACKUP_READY=1
}

demo_require_root
demo_prepare_edge_config_permissions
[[ "$SHA" =~ ^[0-9a-f]{40}$ ]] || demo_fail 'DEPLOY_SHA must be a full 40-hex commit SHA'
[[ "$IMAGE_DIGEST" =~ ^sha256:[0-9a-f]{64}$ ]] || demo_fail 'DEPLOY_IMAGE_DIGEST must be a trusted sha256 digest'
[[ "$PREV_SHA" =~ ^[0-9a-f]{40}$ ]] || demo_fail 'PREVIOUS_SHA must be a full 40-hex commit SHA'
LOCK_PATH="$DEMO_REPO_ROOT/.state/deploy.lock"
if [[ "${DEMO_DEPLOY_LOCK_HELD:-0}" != 1 ]]; then
  mkdir -p "$(dirname "$LOCK_PATH")"
  exec 9> "$LOCK_PATH"
  flock -x 9
fi
demo_require_env
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
mapfile -t CONTAINER_IDS < <(demo_compose ps -q)
(("${#CONTAINER_IDS[@]}" >= 4)) || demo_fail 'demo database, server, HAProxy, or edge container is not running'
[[ -z "$(demo_compose ps --status running -q "$CANDIDATE_SERVER")" ]] ||
  demo_fail 'candidate server slot is already running; reconcile the rollout before deploying'
demo_require_proxy_configuration_current haproxy "$TEAGRAM_DEMO_HAPROXY_CONFIG_SHA256"
demo_require_proxy_configuration_current edge "" 1 "$PREV_SHA"
mapfile -t EDGE_IDS < <(demo_compose ps -q edge)
((${#EDGE_IDS[@]} == 1)) || demo_fail 'running edge proxy is missing or ambiguous'
EDGE_CONTAINER_ID=${EDGE_IDS[0]}
EDGE_CONFIG_RELOAD_ATTEMPTED=1
demo_reload_edge
mapfile -t CONTAINER_IDS < <(demo_compose ps -q)
((${#CONTAINER_IDS[@]} >= 4)) || demo_fail 'demo database, server, HAProxy, or refreshed edge container is not running'
demo_prepare_client_config_dir
PREV_WEB_RELEASE=$(demo_reconcile_web_release)
PREV_WEB_LINK=$PREV_WEB_RELEASE
demo_render_config "$TARGET_RENDER"
demo_check_blob_mount "$TARGET_RENDER"
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
PREV_IMAGE_REF=$(demo_active_image_ref)
PREV_IMAGE_REF_ID=$(docker image inspect --format '{{.Id}}' "$PREV_IMAGE_REF")
[[ "$PREV_IMAGE_REF_ID" == "$PREV_IMAGE_ID" ]] ||
  demo_fail 'active release image does not match the running server container'
CANDIDATE_IMAGE_REF=$(demo_pull_image_by_sha "$SHA" "$IMAGE_DIGEST")
printf '%s\n' "$PREV_IMAGE_ID" > "$BACKUP_DIR/previous-image-id.txt"
printf '%s\n' "$ACTIVE_SERVER" > "$BACKUP_DIR/active-server.txt"
printf '%s\n' "$PREV_IMAGE_REF" > "$BACKUP_DIR/previous-image-ref.txt"
cp -- "$DEMO_CLIENT_CONFIG" "$BACKUP_DIR/client.json"
printf '%s\n' "${PREV_WEB_LINK:-none}" > "$BACKUP_DIR/previous-web-release.txt"
chown root:root "$BACKUP_DIR/previous-image-id.txt" "$BACKUP_DIR/active-server.txt" \
  "$BACKUP_DIR/previous-image-ref.txt" "$BACKUP_DIR/client.json" "$BACKUP_DIR/previous-web-release.txt"
chmod 0600 "$BACKUP_DIR/previous-image-id.txt" "$BACKUP_DIR/active-server.txt" \
  "$BACKUP_DIR/previous-image-ref.txt" "$BACKUP_DIR/client.json" "$BACKUP_DIR/previous-web-release.txt"

# The snapshot is taken online. Existing clients keep using the old server while
# the tested image is pulled, migrations apply, and the candidate is checked.
demo_backup_database

export TEAGRAM_DEMO_IMAGE_REF="$CANDIDATE_IMAGE_REF"
# Check candidate RSA identity before applying migrations. The candidate's
# discovery generation is intentionally disabled for this probe because the
# pre-migration database may not have the generation table yet.
demo_compose run --rm --no-deps -e TG_ADVERTISE_DEMO_GUEST_SIGN_UP=false "$CANDIDATE_SERVER" client-config > "$TMP_DIR/client-before-migration.json"
jq -e '
  .version == 1
  and .mtproto.endpoint == "demo.teagram.io:443"
  and (.mtproto.rsa_spki | type == "string" and length > 0)
  and .websocket.endpoint == "wss://demo.teagram.io/apiws"
  and .demo == null
' "$TMP_DIR/client-before-migration.json" >/dev/null || demo_fail 'candidate pre-migration client config is invalid'
OLD_SPKI=$(jq -er '.mtproto.rsa_spki' "$BACKUP_DIR/client.json")
CANDIDATE_SPKI=$(jq -er '.mtproto.rsa_spki' "$TMP_DIR/client-before-migration.json")
[[ "$CANDIDATE_SPKI" == "$OLD_SPKI" ]] || demo_fail 'candidate changed the persistent RSA identity'

demo_compose rm -sf migrate >/dev/null
demo_compose up -d migrate
demo_wait_migration demo_compose 'deployment migrations'
demo_compose run --rm --no-deps "$CANDIDATE_SERVER" client-config > "$TMP_DIR/client.json"
jq -e '
  .version == 1
  and .mtproto.endpoint == "demo.teagram.io:443"
  and (.mtproto.rsa_spki | type == "string" and length > 0)
  and .websocket.endpoint == "wss://demo.teagram.io/apiws"
  and .demo.guest_sign_up == true
  and (.demo.wipe_generation | type == "string" and test("^[0-9a-f]{64}$"))
' "$TMP_DIR/client.json" >/dev/null || demo_fail 'client-config lacks the approved demo WebSocket, guest signup, or wipe generation binding'
OLD_SPKI=$(jq -er '.mtproto.rsa_spki' "$BACKUP_DIR/client.json")
NEW_SPKI=$(jq -er '.mtproto.rsa_spki' "$TMP_DIR/client.json")
[[ "$NEW_SPKI" == "$OLD_SPKI" ]] || demo_fail 'deployment changed the persistent RSA identity'
NEW_GENERATION=$(jq -er '.demo.wipe_generation' "$TMP_DIR/client.json")
if jq -e '(.demo | type == "object") and (.demo | has("wipe_generation"))' "$BACKUP_DIR/client.json" >/dev/null; then
  OLD_GENERATION=$(jq -er '.demo.wipe_generation | select(type == "string" and test("^[0-9a-f]{64}$"))' "$BACKUP_DIR/client.json")
  [[ "$OLD_GENERATION" == "$BACKUP_GENERATION" ]] || demo_fail 'published client generation differs from the database backup'
else
  jq -e '(.demo == null) or ((.demo | type == "object") and (.demo | has("wipe_generation") | not))' \
    "$BACKUP_DIR/client.json" >/dev/null || demo_fail 'backup client config has an invalid wipe generation field'
fi
if [[ -n "$BACKUP_GENERATION" ]]; then
  [[ "$NEW_GENERATION" == "$BACKUP_GENERATION" ]] || demo_fail 'deployment changed the stored wipe generation'
fi
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
if [[ -n "$PREV_WEB_RELEASE" ]]; then
  python3 "$SCRIPT_DIR/smoke.py" \
    --web-source-commit "${PREV_WEB_RELEASE%% *}" \
    --web-artifact-digest "sha256:${PREV_WEB_RELEASE#* }" \
    "$DEMO_CLIENT_CONFIG"
else
  python3 "$SCRIPT_DIR/smoke.py" --bootstrap "$DEMO_CLIENT_CONFIG"
fi
"$SCRIPT_DIR/seed-demo.sh" --verify-turnstile --endpoint haproxy:2443
if [[ -n "$PREV_WEB_RELEASE" ]]; then
  demo_write_active_release "$CANDIDATE_SERVER" "$CANDIDATE_IMAGE_REF" \
    "${PREV_WEB_RELEASE%% *}" "sha256:${PREV_WEB_RELEASE#* }"
else
  demo_write_active_release "$CANDIDATE_SERVER" "$CANDIDATE_IMAGE_REF"
fi
demo_prepare_haproxy_runtime

# New sessions route to the candidate while HAProxy drains the old server's
# existing connections before its graceful stop signal.
demo_compose stop "$ACTIVE_SERVER"
CUTOVER_COMMITTED=1
demo_remove_previous_image "$PREV_IMAGE_ID" "$CANDIDATE_IMAGE_ID"
BACKUP_READY=0
printf 'demo deployed sha=%s previous_sha=%s active_server=%s backup=%s\n' \
  "$SHA" "$PREV_SHA" "$CANDIDATE_SERVER" "$BACKUP_DIR"
