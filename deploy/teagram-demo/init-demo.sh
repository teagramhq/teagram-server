#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
DEMO_REPO_ROOT=$(cd -P "$SCRIPT_DIR/../.." && pwd)

demo_lock_init() {
  LOCK_PATH="$DEMO_REPO_ROOT/.state/deploy.lock"
  mkdir -p "$(dirname "$LOCK_PATH")"
  exec 9> "$LOCK_PATH"
  flock -x 9
}

if [[ "$EUID" -ne 0 ]]; then
  echo 'teagram-demo: must run as root on the demo LXC' >&2
  exit 1
fi
if [[ "$#" -ne 1 || ! "${1:-}" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  echo 'teagram-demo: usage: init-demo.sh <trusted-ci-sha256-digest>' >&2
  exit 1
fi
readonly IMAGE_DIGEST=$1
demo_lock_init

# shellcheck source=deploy/teagram-demo/demo-common.sh
source "$SCRIPT_DIR/demo-common.sh"

TMP_DIR=
PUBLIC_SERVICES_MAY_BE_RUNNING=0
cleanup() {
  local rc=$?
  trap - EXIT
  if (( rc != 0 && PUBLIC_SERVICES_MAY_BE_RUNNING == 1 )); then
    if ! demo_compose stop edge haproxy; then
      printf 'teagram-demo: failed to stop public ingress during install cleanup\n' >&2
    fi
  fi
  if [[ -n "$TMP_DIR" && -d "$TMP_DIR" ]]; then
    if ! rm -rf -- "$TMP_DIR"; then
      printf 'teagram-demo: failed to remove temporary install state\n' >&2
      (( rc != 0 )) || rc=1
    fi
  fi
  exit "$rc"
}
trap cleanup EXIT

demo_require_env
ACTIVE_SERVER=$(demo_active_server)
ACTIVE_ENDPOINT="$ACTIVE_SERVER:2443"
SEED_SCRIPT="$SCRIPT_DIR/seed-demo.sh"
git -C "$DEMO_REPO_ROOT" ls-files --error-unmatch deploy/teagram-demo/seed-demo.sh >/dev/null 2>&1 ||
  demo_fail 'initial install is disabled until the tracked API seed-demo.sh is supplied'
[[ -f "$SEED_SCRIPT" && ! -L "$SEED_SCRIPT" && -x "$SEED_SCRIPT" ]] ||
  demo_fail 'tracked API seed-demo.sh must be executable'
"$SEED_SCRIPT" --check

git -C "$DEMO_REPO_ROOT" diff --quiet HEAD -- || demo_fail 'checkout has tracked changes'
[[ "$(git -C "$DEMO_REPO_ROOT" branch --show-current)" == main ]] || demo_fail 'checkout must be on main'
git -C "$DEMO_REPO_ROOT" fetch -q origin main
SHA=$(git -C "$DEMO_REPO_ROOT" rev-parse HEAD)
git -C "$DEMO_REPO_ROOT" merge-base --is-ancestor "$SHA" origin/main || demo_fail 'checkout is not on origin/main'

mkdir -p "$DEMO_STATE_DIR"
chown root:root "$DEMO_STATE_DIR"
chmod 0755 "$DEMO_STATE_DIR"
TMP_DIR=$(mktemp -d "$DEMO_REPO_ROOT/.state/teagram-demo/init.XXXXXXXX")
chmod 0700 "$TMP_DIR"
TARGET_RENDER="$TMP_DIR/target-render.json"
INITIAL_RENDER="$TMP_DIR/initial-render.json"
INITIAL_INVENTORY="$TMP_DIR/initial-inventory.json"
MODE_HELPER="$DEMO_SCRIPT_DIR/../teagramd/rollout-runner/blob-mode-state.py"

demo_render_config "$TARGET_RENDER"
demo_check_blob_mount "$TARGET_RENDER"
BLOB_PATH=$(jq -er '.volumes.tgblobs.driver_opts.device' "$TARGET_RENDER")
demo_seed_compose config --format json > "$INITIAL_RENDER"
chmod 0600 "$INITIAL_RENDER"
demo_check_blob_mount "$INITIAL_RENDER"
[[ "$(jq -er '.volumes.tgblobs.driver_opts.device' "$INITIAL_RENDER")" == "$BLOB_PATH" ]] ||
  demo_fail 'seed and production Compose renders use different blob paths'
python3 "$MODE_HELPER" compose --checkout "$DEMO_REPO_ROOT" \
  < "$INITIAL_RENDER" > "$INITIAL_INVENTORY"

MODE_FILE="$DEMO_AUTHORITY_DIR/mode.json"
JOURNAL_DIR="$DEMO_AUTHORITY_DIR/journal"
if [[ -e "$MODE_FILE" || -L "$MODE_FILE" ]]; then
  # Resume a valid, already-published first-install authority after an
  # interrupted switch from the unguarded bootstrap container.
  demo_compose stop edge haproxy turnstile-egress
  mapfile -t CONTAINER_IDS < <(demo_bootstrap_compose ps -q)
  if ((${#CONTAINER_IDS[@]})); then
    docker inspect "${CONTAINER_IDS[@]}" > "$TMP_DIR/containers.raw.json"
  else
    printf '[]\n' > "$TMP_DIR/containers.raw.json"
  fi
  python3 "$MODE_HELPER" containers \
    --checkout "$DEMO_REPO_ROOT" < "$TMP_DIR/containers.raw.json" > "$TMP_DIR/containers.json"
  cp "$INITIAL_INVENTORY" "$TMP_DIR/target-preflight.json"
  python3 "$MODE_HELPER" validate \
    --state-dir "$DEMO_AUTHORITY_DIR" \
    --report-root "$DEMO_STATE_DIR/reports" \
    --containers "$TMP_DIR/containers.json" \
    --compose "$INITIAL_INVENTORY" \
    --override "$SCRIPT_DIR/compose.empty-overrides.yaml" \
    --checkout "$DEMO_REPO_ROOT" \
    --allow-empty-containers \
    --allow-unguarded-initial-local \
    --allow-unguarded-initial-local-containers

  IMAGE_REF=$(demo_pull_image_by_sha "$SHA" "$IMAGE_DIGEST")
  export TEAGRAM_DEMO_IMAGE_REF="$IMAGE_REF"
  # Recreate the project networks from the current IPAM config, including the
  # backend dynamic-address pool. Compose down leaves named volumes intact.
  demo_compose down --remove-orphans
  demo_bootstrap_compose up -d migrate
  demo_wait_migration demo_bootstrap_compose 'resume migrations'
  demo_compose run --rm --no-deps teagramd-maintenance client-config > "$TMP_DIR/client.json"
  jq -e '
    .version == 1
    and .mtproto.endpoint == "demo.teagram.io:443"
    and (.mtproto.rsa_spki | type == "string" and length > 0)
    and .websocket.endpoint == "wss://demo.teagram.io/apiws"
    and .demo.guest_sign_up == true
    and (.demo.wipe_generation | type == "string" and test("^[0-9a-f]{64}$"))
  ' "$TMP_DIR/client.json" >/dev/null || demo_fail 'client-config lacks the approved demo wipe generation binding'
  install -o root -g root -m 0644 "$TMP_DIR/client.json" "$DEMO_CLIENT_CONFIG.next"
  mv -f -- "$DEMO_CLIENT_CONFIG.next" "$DEMO_CLIENT_CONFIG"
  demo_write_active_release "$ACTIVE_SERVER" "$IMAGE_REF"
  demo_seed_compose up -d "$ACTIVE_SERVER"
  demo_prepare_seed_owner "$TMP_DIR"
  "$SEED_SCRIPT" --seed \
    --endpoint "$ACTIVE_ENDPOINT" \
    --owner-username "$DEMO_SEED_OWNER_USERNAME" \
    --owner-password-file "$DEMO_SEED_PASSWORD_FILE"
  "$SEED_SCRIPT" --verify-seed --endpoint "$ACTIVE_ENDPOINT"
  "$SCRIPT_DIR/test-seed-compose.sh"

  # The seed and convergence checks exercise auth flows with relaxed rate
  # limits. Remove only the limiter state they created before the private
  # production-config Turnstile probe runs.
  demo_compose exec -T postgres psql -v ON_ERROR_STOP=1 -U postgres -d teagram \
    -c 'DELETE FROM rate_limits; DELETE FROM send_code_ip_calls; DELETE FROM send_code_ip_phones; DELETE FROM public_send_code_ip_calls; DELETE FROM public_send_code_ip_phones;'

  PUBLIC_SERVICES_MAY_BE_RUNNING=1
  demo_compose up -d --wait --wait-timeout 120 "$ACTIVE_SERVER" haproxy
  "$SEED_SCRIPT" --verify-turnstile --endpoint haproxy:2443
  demo_compose up -d edge
  python3 "$SCRIPT_DIR/smoke.py" --bootstrap "$DEMO_CLIENT_CONFIG"
  echo "demo install resumed and seeded at $SHA"
  exit 0
fi
[[ "$ACTIVE_SERVER" == teagramd ]] || demo_fail 'fresh installs must begin in the teagramd slot'
if [[ -e "$JOURNAL_DIR" || -L "$JOURNAL_DIR" ]]; then
  demo_fail 'blob-mode journal exists without mode.json; preserve state and reconcile it before retrying'
fi

for volume in teagram-demo_pgdata teagram-demo_tgkey teagram-demo_tgblobs; do
  if docker volume inspect "$volume" >/dev/null 2>&1; then
    demo_fail "persistent volume $volume already exists without a published mode record"
  fi
done
python3 - "$BLOB_PATH" <<'PY'
import os
import sys

try:
    entries = list(os.scandir(sys.argv[1]))
except OSError as error:
    raise SystemExit(f"demo blob path cannot be listed: {error}")
if entries:
    raise SystemExit("demo blob path is not empty; refusing fresh installation")
PY

IMAGE_REF=$(demo_pull_image_by_sha "$SHA" "$IMAGE_DIGEST")
export TEAGRAM_DEMO_IMAGE_REF="$IMAGE_REF"
demo_write_active_release "$ACTIVE_SERVER" "$IMAGE_REF"
"$SEED_SCRIPT" --check

demo_compose run --rm --no-deps teagramd-maintenance bootstrap-identity > "$TMP_DIR/bootstrap.stdout" 2> "$TMP_DIR/bootstrap.stderr"
FINGERPRINT=$(sed -n 's/.*fingerprint=\(-\{0,1\}[0-9][0-9]*\).*/\1/p' "$TMP_DIR/bootstrap.stdout" | head -n 1)
[[ "$FINGERPRINT" =~ ^-?[0-9]+$ ]] || demo_fail 'identity bootstrap did not report an RSA fingerprint'

demo_bootstrap_compose up -d migrate
demo_wait_migration demo_bootstrap_compose 'bootstrap migrations'
demo_compose run --rm --no-deps teagramd-maintenance client-config > "$TMP_DIR/client.json"
jq -e '
  .version == 1
  and .mtproto.endpoint == "demo.teagram.io:443"
  and (.mtproto.rsa_spki | type == "string" and length > 0)
  and .websocket.endpoint == "wss://demo.teagram.io/apiws"
  and .demo.guest_sign_up == true
  and (.demo.wipe_generation | type == "string" and test("^[0-9a-f]{64}$"))
' "$TMP_DIR/client.json" >/dev/null || demo_fail 'client-config lacks the approved demo wipe generation binding; wait for MAIN-2196'
install -o root -g root -m 0644 "$TMP_DIR/client.json" "$DEMO_CLIENT_CONFIG.next"
mv -f -- "$DEMO_CLIENT_CONFIG.next" "$DEMO_CLIENT_CONFIG"
demo_bootstrap_compose up -d teagramd

mapfile -t CONTAINER_IDS < <(demo_bootstrap_compose ps -q)
((${#CONTAINER_IDS[@]} >= 2)) || demo_fail 'bootstrap server and database containers are not both running'
docker inspect "${CONTAINER_IDS[@]}" > "$TMP_DIR/baseline-containers.raw.json"
python3 "$MODE_HELPER" containers --checkout "$DEMO_REPO_ROOT" \
  < "$TMP_DIR/baseline-containers.raw.json" > "$TMP_DIR/baseline-containers.json"
docker inspect "${CONTAINER_IDS[@]}" > "$TMP_DIR/current-containers.raw.json"
python3 "$MODE_HELPER" containers --checkout "$DEMO_REPO_ROOT" \
  < "$TMP_DIR/current-containers.raw.json" > "$TMP_DIR/current-containers.json"
cp "$INITIAL_INVENTORY" "$TMP_DIR/target-preflight.json"
TARGET_ARTIFACT_SHA=$(sha256sum "$SCRIPT_DIR/compose.yaml")
TARGET_ARTIFACT_SHA=${TARGET_ARTIFACT_SHA%% *}
LOCK_PATH=$(realpath -m "$LOCK_PATH")

python3 "$MODE_HELPER" initialize-local \
  --state-dir "$DEMO_AUTHORITY_DIR" \
  --report-root "$DEMO_STATE_DIR/reports" \
  --baseline-containers "$TMP_DIR/baseline-containers.json" \
  --current-containers "$TMP_DIR/current-containers.json" \
  --preflight-target-compose "$TMP_DIR/target-preflight.json" \
  --target-compose "$INITIAL_INVENTORY" \
  --target-artifact-sha256 "$TARGET_ARTIFACT_SHA" \
  --override "$SCRIPT_DIR/compose.empty-overrides.yaml" \
  --checkout "$DEMO_REPO_ROOT" \
  --target-sha "$SHA" \
  --baseline-sha "$SHA" \
  --lock-path "$LOCK_PATH"

demo_reset_seed_owner_marker
demo_seed_compose up -d teagramd
demo_prepare_seed_owner "$TMP_DIR"
"$SEED_SCRIPT" --seed \
  --endpoint teagramd:2443 \
  --owner-username "$DEMO_SEED_OWNER_USERNAME" \
  --owner-password-file "$DEMO_SEED_PASSWORD_FILE"
"$SEED_SCRIPT" --verify-seed --endpoint teagramd:2443
"$SCRIPT_DIR/test-seed-compose.sh"

# The seed and convergence checks exercise auth flows with relaxed rate
# limits. Remove only the limiter state they created before the private
# production-config Turnstile probe runs.
demo_compose exec -T postgres psql -v ON_ERROR_STOP=1 -U postgres -d teagram \
  -c 'DELETE FROM rate_limits; DELETE FROM send_code_ip_calls; DELETE FROM send_code_ip_phones; DELETE FROM public_send_code_ip_calls; DELETE FROM public_send_code_ip_phones;'

PUBLIC_SERVICES_MAY_BE_RUNNING=1
demo_compose up -d --wait --wait-timeout 120 "$ACTIVE_SERVER" haproxy
"$SEED_SCRIPT" --verify-turnstile --endpoint haproxy:2443
demo_compose up -d edge
python3 "$SCRIPT_DIR/smoke.py" --bootstrap "$DEMO_CLIENT_CONFIG"
echo "demo install ready sha=$SHA rsa_fingerprint=$FINGERPRINT seeded=true"
