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
[[ ! -L "$DEMO_STATE_DIR" ]] || demo_fail 'demo state directory must not be a symlink'

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

demo_prune_deploy_backups() {
  local backup_root="$DEMO_STATE_DIR/backups"
  [[ ! -L "$backup_root" ]] || { demo_fail 'backup directory must not be a symlink'; return 1; }
  if [[ -e "$backup_root" && ! -d "$backup_root" ]]; then
    demo_fail 'backup path is not a directory'
    return 1
  fi
  mkdir -p "$backup_root"
  chown root:root "$backup_root"
  chmod 0700 "$backup_root"
  find "$backup_root" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
}

TARGET_RENDER="$TMP_DIR/target-render.json"
TARGET_INVENTORY="$TMP_DIR/target-inventory.json"
ACTIVE_SERVER=$(demo_active_server)
INACTIVE_SERVER=$(demo_inactive_server)
ACTIVE_ENDPOINT="$ACTIVE_SERVER:2443"
demo_render_config "$TARGET_RENDER"
demo_check_blob_mount "$TARGET_RENDER"
mapfile -t CONTAINER_IDS < <(demo_compose ps -q)
((${#CONTAINER_IDS[@]} >= 4)) || demo_fail 'demo database, server, HAProxy, or edge container is not running'
[[ -z "$(demo_compose ps --status running -q "$INACTIVE_SERVER")" ]] ||
  demo_fail 'inactive server slot is running; reconcile the rollout before wiping demo data'
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

CLIENT_CONFIG="$DEMO_CLIENT_CONFIG"
[[ -f "$CLIENT_CONFIG" && ! -L "$CLIENT_CONFIG" ]] || demo_fail 'current public client config is missing'
BEFORE_SPKI=$(jq -er '.mtproto.rsa_spki' "$CLIENT_CONFIG")
BEFORE_GENERATION=$(jq -er '.demo.wipe_generation | select(type == "string" and test("^[0-9a-f]{64}$"))' "$CLIENT_CONFIG")
[[ "$(jq -er '.websocket.endpoint' "$CLIENT_CONFIG")" == 'wss://demo.teagram.io/apiws' ]] ||
  demo_fail 'current client config does not match the approved demo WebSocket endpoint'
"$SEED_SCRIPT" --check
curl --fail --silent --show-error --max-time 10 \
  https://demo.teagram.io/.well-known/teagramd/client > "$TMP_DIR/public-client.json"
cmp -s "$CLIENT_CONFIG" "$TMP_DIR/public-client.json" || demo_fail 'public client config differs from the current server identity'

# The seed check and public identity probe run before any destructive operation.
# The separate tgkey volume and .env are deliberately never removed or rewritten.
# Pause scheduled activity and wait for any in-flight API call before the
# database is reset. Its session and rate state live outside the weekly wipe.
ACTIVITY_TIMER=/etc/systemd/system/teagram-demo-activity.timer
ACTIVITY_SERVICE=/etc/systemd/system/teagram-demo-activity.service
ACTIVITY_TIMER_WAS_ACTIVE=0
if [[ -f "$ACTIVITY_TIMER" ]]; then
  if systemctl is-active --quiet teagram-demo-activity.timer; then
    ACTIVITY_TIMER_WAS_ACTIVE=1
    systemctl stop teagram-demo-activity.timer
  fi
fi
if [[ -f "$ACTIVITY_SERVICE" ]]; then
  systemctl stop teagram-demo-activity.service
fi
ACTIVITY_LOCK=/var/lib/teagram-demo-activity/activity.lock
if [[ -d /var/lib/teagram-demo-activity && ! -L /var/lib/teagram-demo-activity && -e "$ACTIVITY_LOCK" ]]; then
  [[ -f "$ACTIVITY_LOCK" && ! -L "$ACTIVITY_LOCK" && "$(stat -c '%u:%a' "$ACTIVITY_LOCK")" == '0:600' ]] ||
    demo_fail 'activity lock file is unsafe'
  flock -x -w 20m "$ACTIVITY_LOCK" true || demo_fail 'timed out waiting for scheduled activity to stop'
fi
# Stop every public path before resetting data. The seed-only overlay never
# starts HAProxy or edge.
demo_compose stop edge haproxy "$ACTIVE_SERVER" turnstile-egress
demo_prune_deploy_backups
demo_compose exec -T postgres dropdb -U postgres --if-exists teagram </dev/null
demo_compose exec -T postgres createdb -U postgres teagram </dev/null
demo_reset_seed_owner_marker
BLOB_PATH=$(jq -er '.volumes.tgblobs.driver_opts.device' "$TARGET_RENDER")
find "$BLOB_PATH" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
demo_compose rm -sf migrate >/dev/null
demo_compose up -d migrate
demo_wait_migration demo_compose 'post-reset migrations'

demo_seed_compose up -d "$ACTIVE_SERVER"
demo_prepare_seed_owner "$TMP_DIR"
"$SEED_SCRIPT" --seed \
  --endpoint "$ACTIVE_ENDPOINT" \
  --owner-username "$DEMO_SEED_OWNER_USERNAME" \
  --owner-password-file "$DEMO_SEED_PASSWORD_FILE"
"$SEED_SCRIPT" --verify-seed --endpoint "$ACTIVE_ENDPOINT"

# Re-run the API seeder in the private seed lane and compare verified counts.
"$SCRIPT_DIR/test-seed-compose.sh" "$BEFORE_GENERATION"

# The seed and convergence checks deliberately exercise many auth flows while
# production rate limits are relaxed. Remove only the limiter state they
# created before the private production-config Turnstile probe runs.
demo_compose exec -T postgres psql -v ON_ERROR_STOP=1 -U postgres -d teagram \
  -c 'DELETE FROM rate_limits; DELETE FROM send_code_ip_calls; DELETE FROM send_code_ip_phones; DELETE FROM public_send_code_ip_calls; DELETE FROM public_send_code_ip_phones;'

# Recreate teagramd from the production config and start HAProxy's private
# MTProto path while edge stays down. The probe must prove Turnstile is active
# before the public edge starts.
demo_compose up -d --wait --wait-timeout 120 "$ACTIVE_SERVER" haproxy
"$SEED_SCRIPT" --verify-turnstile --endpoint haproxy:2443
demo_server_client_config "$ACTIVE_SERVER" > "$TMP_DIR/client-after-wipe.json"
jq -e '
  .version == 1
  and .mtproto.endpoint == "demo.teagram.io:443"
  and (.mtproto.rsa_spki | type == "string" and length > 0)
  and .websocket.endpoint == "wss://demo.teagram.io/apiws"
  and .demo.guest_sign_up == true
  and (.demo.wipe_generation | type == "string" and test("^[0-9a-f]{64}$"))
' "$TMP_DIR/client-after-wipe.json" >/dev/null || demo_fail 'client-config lacks the approved demo wipe generation binding'
AFTER_SPKI=$(jq -er '.mtproto.rsa_spki' "$TMP_DIR/client-after-wipe.json")
AFTER_GENERATION=$(jq -er '.demo.wipe_generation' "$TMP_DIR/client-after-wipe.json")
[[ "$AFTER_SPKI" == "$BEFORE_SPKI" ]] || demo_fail 'weekly wipe changed the persistent RSA identity'
[[ "$AFTER_GENERATION" != "$BEFORE_GENERATION" ]] || demo_fail 'weekly wipe reused the previous wipe generation'
install -o root -g root -m 0644 "$TMP_DIR/client-after-wipe.json" "$DEMO_CLIENT_CONFIG.next"
mv -f -- "$DEMO_CLIENT_CONFIG.next" "$DEMO_CLIENT_CONFIG"
demo_compose up -d edge
python3 "$SCRIPT_DIR/smoke.py" "$CLIENT_CONFIG"
demo_compose exec -T "$ACTIVE_SERVER" /usr/local/bin/teagramd client-config > "$TMP_DIR/client-active-after-wipe.json"
ACTIVE_SPKI=$(jq -er '.mtproto.rsa_spki' "$TMP_DIR/client-active-after-wipe.json")
[[ "$ACTIVE_SPKI" == "$BEFORE_SPKI" ]] || demo_fail 'weekly wipe changed the persistent RSA identity'
ACTIVE_GENERATION=$(jq -er '.demo.wipe_generation' "$TMP_DIR/client-active-after-wipe.json")
[[ "$ACTIVE_GENERATION" == "$AFTER_GENERATION" ]] || demo_fail 'active server advertises a different wipe generation'
if [[ -f "$ACTIVITY_SERVICE" ]]; then
  systemctl reset-failed teagram-demo-activity.service
  if ((ACTIVITY_TIMER_WAS_ACTIVE)); then
    systemctl start teagram-demo-activity.timer
  fi
fi
printf 'weekly wipe=verified rsa_identity=preserved blob_mount=%s\n' "$BLOB_PATH"
