#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/teagram-demo/demo-common.sh
source "$SCRIPT_DIR/demo-common.sh"

TMP_DIR=
cleanup() {
  if [[ -n "$TMP_DIR" && -d "$TMP_DIR" ]]; then
    rm -rf -- "$TMP_DIR"
  fi
}
trap cleanup EXIT

demo_require_root
demo_require_env
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
TARGET_INVENTORY="$TMP_DIR/target-inventory.json"
MODE_HELPER="$DEMO_SCRIPT_DIR/../teagramd/rollout-runner/blob-mode-state.py"

demo_render_config "$TARGET_RENDER"
demo_check_blob_mount "$TARGET_RENDER"
BLOB_PATH=$(jq -er '.volumes.tgblobs.driver_opts.device' "$TARGET_RENDER")
python3 "$MODE_HELPER" compose --checkout "$DEMO_REPO_ROOT" \
  < "$TARGET_RENDER" > "$TARGET_INVENTORY"

MODE_FILE="$DEMO_AUTHORITY_DIR/mode.json"
JOURNAL_DIR="$DEMO_AUTHORITY_DIR/journal"
if [[ -e "$MODE_FILE" || -L "$MODE_FILE" ]]; then
  # Resume a valid, already-published first-install authority after an
  # interrupted switch from the unguarded bootstrap container.
  mapfile -t CONTAINER_IDS < <(demo_bootstrap_compose ps -q)
  if ((${#CONTAINER_IDS[@]})); then
    docker inspect "${CONTAINER_IDS[@]}" > "$TMP_DIR/containers.raw.json"
  else
    printf '[]\n' > "$TMP_DIR/containers.raw.json"
  fi
  python3 "$MODE_HELPER" containers \
    --checkout "$DEMO_REPO_ROOT" < "$TMP_DIR/containers.raw.json" > "$TMP_DIR/containers.json"
  cp "$TARGET_INVENTORY" "$TMP_DIR/target-preflight.json"
  python3 "$MODE_HELPER" validate \
    --state-dir "$DEMO_AUTHORITY_DIR" \
    --report-root "$DEMO_STATE_DIR/reports" \
    --containers "$TMP_DIR/containers.json" \
    --compose "$TARGET_INVENTORY" \
    --override "$SCRIPT_DIR/compose.empty-overrides.yaml" \
    --checkout "$DEMO_REPO_ROOT" \
    --allow-empty-containers \
    --allow-unguarded-initial-local \
    --allow-unguarded-initial-local-containers
  demo_compose up -d teagramd edge
  python3 "$SCRIPT_DIR/smoke.py" "$DEMO_STATE_DIR/client.json"
  echo "demo install resumed at $SHA"
  exit 0
fi
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

LOCK_PATH="$DEMO_REPO_ROOT/.state/deploy.lock"
mkdir -p "$(dirname "$LOCK_PATH")"
exec 9> "$LOCK_PATH"
flock -x 9

export TEAGRAMD_SOURCE_REVISION="$SHA"
demo_compose build teagramd
IMAGE_REVISION=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' teagramd:local)
[[ "$IMAGE_REVISION" == "$SHA" ]] || demo_fail 'built teagramd image does not identify the selected main commit'

demo_compose run --rm --no-deps teagramd bootstrap-identity > "$TMP_DIR/bootstrap.stdout" 2> "$TMP_DIR/bootstrap.stderr"
FINGERPRINT=$(sed -n 's/.*fingerprint=\(-\{0,1\}[0-9][0-9]*\).*/\1/p' "$TMP_DIR/bootstrap.stdout" | head -n 1)
[[ "$FINGERPRINT" =~ ^-?[0-9]+$ ]] || demo_fail 'identity bootstrap did not report an RSA fingerprint'

demo_compose run --rm --no-deps teagramd client-config > "$TMP_DIR/client.json"
jq -e '
  .version == 1
  and .mtproto.endpoint == "demo.teagram.io:443"
  and (.mtproto.rsa_spki | type == "string" and length > 0)
  and .websocket.endpoint == "wss://demo.teagram.io/apiws"
' "$TMP_DIR/client.json" >/dev/null || demo_fail 'client-config lacks the approved demo WebSocket binding; wait for MAIN-2196'
install -o root -g root -m 0644 "$TMP_DIR/client.json" "$DEMO_STATE_DIR/client.json.next"
mv -f -- "$DEMO_STATE_DIR/client.json.next" "$DEMO_STATE_DIR/client.json"

demo_bootstrap_compose up -d migrate
mapfile -t MIGRATE_IDS < <(demo_bootstrap_compose ps -a -q migrate)
((${#MIGRATE_IDS[@]} == 1)) || demo_fail 'bootstrap migration container is missing or ambiguous'
MIGRATE_STATUS=$(docker inspect --format '{{.State.Status}} {{.State.ExitCode}}' "${MIGRATE_IDS[0]}")
[[ "$MIGRATE_STATUS" == 'exited 0' ]] || demo_fail "bootstrap migrations did not complete successfully ($MIGRATE_STATUS)"
demo_bootstrap_compose up -d teagramd

mapfile -t CONTAINER_IDS < <(demo_bootstrap_compose ps -q)
((${#CONTAINER_IDS[@]} >= 2)) || demo_fail 'bootstrap server and database containers are not both running'
docker inspect "${CONTAINER_IDS[@]}" > "$TMP_DIR/baseline-containers.raw.json"
python3 "$MODE_HELPER" containers --checkout "$DEMO_REPO_ROOT" \
  < "$TMP_DIR/baseline-containers.raw.json" > "$TMP_DIR/baseline-containers.json"
docker inspect "${CONTAINER_IDS[@]}" > "$TMP_DIR/current-containers.raw.json"
python3 "$MODE_HELPER" containers --checkout "$DEMO_REPO_ROOT" \
  < "$TMP_DIR/current-containers.raw.json" > "$TMP_DIR/current-containers.json"
python3 "$MODE_HELPER" compose --checkout "$DEMO_REPO_ROOT" \
  < "$TARGET_RENDER" > "$TARGET_INVENTORY"
cp "$TARGET_INVENTORY" "$TMP_DIR/target-preflight.json"
TARGET_ARTIFACT_SHA=$(sha256sum "$SCRIPT_DIR/compose.yaml")
TARGET_ARTIFACT_SHA=${TARGET_ARTIFACT_SHA%% *}
LOCK_PATH=$(realpath -m "$LOCK_PATH")

python3 "$MODE_HELPER" initialize-local \
  --state-dir "$DEMO_AUTHORITY_DIR" \
  --report-root "$DEMO_STATE_DIR/reports" \
  --baseline-containers "$TMP_DIR/baseline-containers.json" \
  --current-containers "$TMP_DIR/current-containers.json" \
  --preflight-target-compose "$TMP_DIR/target-preflight.json" \
  --target-compose "$TARGET_INVENTORY" \
  --target-artifact-sha256 "$TARGET_ARTIFACT_SHA" \
  --override "$SCRIPT_DIR/compose.empty-overrides.yaml" \
  --checkout "$DEMO_REPO_ROOT" \
  --target-sha "$SHA" \
  --baseline-sha "$SHA" \
  --lock-path "$LOCK_PATH"

demo_compose up -d teagramd edge
python3 "$SCRIPT_DIR/smoke.py" "$DEMO_STATE_DIR/client.json"
echo "demo install ready sha=$SHA rsa_fingerprint=$FINGERPRINT"
