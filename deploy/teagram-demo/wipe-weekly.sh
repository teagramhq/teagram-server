#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/teagram-demo/demo-common.sh
source "$SCRIPT_DIR/demo-common.sh"

demo_require_root
demo_require_env
SEED_SCRIPT="$SCRIPT_DIR/seed-demo.sh"
git -C "$DEMO_REPO_ROOT" ls-files --error-unmatch deploy/teagram-demo/seed-demo.sh >/dev/null 2>&1 ||
  demo_fail 'weekly wipe is disabled until the tracked API seed-demo.sh is supplied'
[[ -f "$SEED_SCRIPT" && ! -L "$SEED_SCRIPT" && -x "$SEED_SCRIPT" ]] ||
  demo_fail 'tracked API seed-demo.sh must be executable'

mkdir -p "$DEMO_STATE_DIR/reports"
chown root:root "$DEMO_STATE_DIR" "$DEMO_STATE_DIR/reports"
chmod 0755 "$DEMO_STATE_DIR"
chmod 0700 "$DEMO_STATE_DIR/reports"
LOCK_PATH="$DEMO_REPO_ROOT/.state/deploy.lock"
mkdir -p "$(dirname "$LOCK_PATH")"
exec 9> "$LOCK_PATH"
flock -x 9

TMP_DIR=$(mktemp -d "$DEMO_STATE_DIR/wipe.XXXXXXXX")
chmod 0700 "$TMP_DIR"
cleanup() {
  local rc=$?
  trap - EXIT
  rm -rf -- "$TMP_DIR"
  exit "$rc"
}
trap cleanup EXIT

TARGET_RENDER="$TMP_DIR/target-render.json"
TARGET_INVENTORY="$TMP_DIR/target-inventory.json"
demo_render_config "$TARGET_RENDER"
demo_check_blob_mount "$TARGET_RENDER"
mapfile -t CONTAINER_IDS < <(demo_compose ps -q)
((${#CONTAINER_IDS[@]} >= 4)) || demo_fail 'demo database, server, HAProxy, or edge container is not running'
docker inspect "${CONTAINER_IDS[@]}" > "$TMP_DIR/containers.raw.json"
python3 "$DEMO_SCRIPT_DIR/../teagramd/rollout-runner/blob-mode-state.py" containers \
  --checkout "$DEMO_REPO_ROOT" < "$TMP_DIR/containers.raw.json" > "$TMP_DIR/containers.json"
python3 "$DEMO_SCRIPT_DIR/../teagramd/rollout-runner/blob-mode-state.py" compose \
  --checkout "$DEMO_REPO_ROOT" < "$TARGET_RENDER" > "$TARGET_INVENTORY"
python3 "$DEMO_SCRIPT_DIR/../teagramd/rollout-runner/blob-mode-state.py" validate \
  --state-dir "$DEMO_AUTHORITY_DIR" \
  --report-root "$DEMO_STATE_DIR/reports" \
  --containers "$TMP_DIR/containers.json" \
  --compose "$TARGET_INVENTORY" \
  --override "$SCRIPT_DIR/compose.empty-overrides.yaml" \
  --checkout "$DEMO_REPO_ROOT"

CLIENT_CONFIG="$DEMO_STATE_DIR/client.json"
[[ -f "$CLIENT_CONFIG" && ! -L "$CLIENT_CONFIG" ]] || demo_fail 'current public client config is missing'
BEFORE_SPKI=$(jq -er '.mtproto.rsa_spki' "$CLIENT_CONFIG")
[[ "$(jq -er '.websocket.endpoint' "$CLIENT_CONFIG")" == 'wss://demo.teagram.io/apiws' ]] ||
  demo_fail 'current client config does not match the approved demo WebSocket endpoint'
"$SEED_SCRIPT" --check
curl --fail --silent --show-error --max-time 10 \
  https://demo.teagram.io/.well-known/telegramd/client > "$TMP_DIR/public-client.json"
cmp -s "$CLIENT_CONFIG" "$TMP_DIR/public-client.json" || demo_fail 'public client config differs from the current server identity'

# The seed check and public identity probe run before any destructive operation.
# The separate tgkey volume and .env are deliberately never removed or rewritten.
# Stop every public path before resetting data. The seed-only overlay never
# starts HAProxy or edge.
demo_compose stop edge haproxy teagramd
demo_compose exec -T postgres dropdb -U postgres --if-exists teagram </dev/null
demo_compose exec -T postgres createdb -U postgres teagram </dev/null
demo_reset_seed_owner_marker
BLOB_PATH=$(jq -er '.volumes.tgblobs.driver_opts.device' "$TARGET_RENDER")
find "$BLOB_PATH" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
demo_compose rm -sf migrate >/dev/null
demo_compose up -d migrate
mapfile -t MIGRATE_IDS < <(demo_compose ps -a -q migrate)
((${#MIGRATE_IDS[@]} == 1)) || demo_fail 'post-reset migrations container is missing or ambiguous'
MIGRATE_STATUS=$(docker inspect --format '{{.State.Status}} {{.State.ExitCode}}' "${MIGRATE_IDS[0]}")
[[ "$MIGRATE_STATUS" == 'exited 0' ]] || demo_fail "post-reset migrations failed ($MIGRATE_STATUS)"

demo_seed_compose up -d teagramd
demo_prepare_seed_owner "$TMP_DIR"
"$SEED_SCRIPT" --seed \
  --endpoint teagramd:2443 \
  --owner-username "$DEMO_SEED_OWNER_USERNAME" \
  --owner-password-file "$DEMO_SEED_PASSWORD_FILE"
"$SEED_SCRIPT" --verify-seed --endpoint teagramd:2443

# Recreate teagramd from the production config while all public paths are down.
# A bare API sign-up must prove the configured Turnstile gate is active before
# either HAProxy or edge is allowed to start.
demo_compose up -d teagramd
"$SEED_SCRIPT" --verify-turnstile --endpoint teagramd:2443
demo_compose up -d haproxy edge
python3 "$SCRIPT_DIR/smoke.py" "$CLIENT_CONFIG"
demo_compose run --rm --no-deps teagramd client-config > "$TMP_DIR/client-after-wipe.json"
AFTER_SPKI=$(jq -er '.mtproto.rsa_spki' "$TMP_DIR/client-after-wipe.json")
[[ "$AFTER_SPKI" == "$BEFORE_SPKI" ]] || demo_fail 'weekly wipe changed the persistent RSA identity'
printf 'weekly wipe=verified rsa_identity=preserved blob_mount=%s\n' "$BLOB_PATH"
