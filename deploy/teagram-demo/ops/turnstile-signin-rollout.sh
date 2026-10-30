#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

readonly REPO=/opt/teagram-demo
readonly ENV_FILE=$REPO/deploy/teagram-demo/.env
readonly BACKUP_DIR=/var/backups/teagram-demo-turnstile-signin
readonly FLAG=TG_TURNSTILE_SIGNIN_ENABLED
readonly TARGET=${1:-}
readonly OPS_SHA=${OPS_SHA:-}
readonly OPS_NAME=${OPS_NAME:-}
TMP_DIR=
ENV_BACKUP=
ROLLOUT_COMMITTED=0
ROLLBACK_CONFIRMED=0

fail() {
  printf 'demo Turnstile rollout: %s\n' "$1" >&2
  if [[ -n "$ENV_BACKUP" ]]; then
    printf 'environment_backup=%s\n' "$ENV_BACKUP" >&2
  fi
  exit 1
}

[[ "$EUID" -eq 0 ]] || fail 'operation must run as root'
[[ "$TARGET" == true || "$TARGET" == false ]] || fail 'target is invalid'
case "$OPS_NAME:$TARGET" in
  turnstile-signin-on:true|turnstile-signin-off:false) ;;
  *) fail 'operation name does not match the requested flag' ;;
esac
[[ "$OPS_SHA" =~ ^[0-9a-f]{40}$ ]] || fail 'reviewed SHA is invalid'
[[ -d "$REPO/.git" && ! -L "$REPO" ]] || fail 'demo checkout is unavailable'
[[ "$(git -C "$REPO" rev-parse HEAD)" == "$OPS_SHA" ]] || fail 'demo checkout differs from the reviewed SHA'
git -C "$REPO" merge-base --is-ancestor "$OPS_SHA" refs/remotes/origin/main ||
  fail 'reviewed SHA is not on origin/main'

for tracked in \
  deploy/teagram-demo/ops/turnstile-signin-rollout.sh \
  deploy/teagram-demo/ops/turnstile-signin-rollback.sh \
  deploy/teagram-demo/update_turnstile_signin.py \
  deploy/teagram-demo/compose.yaml \
  deploy/teagram-demo/validate-env.py \
  deploy/teagram-demo/probe_tailnet_isolation.py \
  deploy/teagram-demo/demo-common.sh \
  deploy/teagram-demo/deploy-main.sh; do
  expected=$(git -C "$REPO" rev-parse "$OPS_SHA:$tracked") || fail "reviewed source is missing: $tracked"
  actual=$(git -C "$REPO" hash-object "$REPO/$tracked") || fail "live source is missing: $tracked"
  [[ "$actual" == "$expected" ]] || fail "live source differs from the reviewed SHA: $tracked"
done
git --no-optional-locks -C "$REPO" diff --quiet HEAD -- || fail 'demo checkout has tracked changes'

[[ -d "$REPO/.state" && ! -L "$REPO/.state" && -d "$REPO/.state/teagram-demo" && ! -L "$REPO/.state/teagram-demo" ]] ||
  fail 'demo state path is unavailable or unsafe'
readonly LOCK_PATH=$REPO/.state/deploy.lock
[[ ! -L "$LOCK_PATH" ]] || fail 'demo deployment lock is a symlink'
exec 9> "$LOCK_PATH"
flock -x 9

# shellcheck source=deploy/teagram-demo/demo-common.sh
source "$REPO/deploy/teagram-demo/demo-common.sh"
# shellcheck source=deploy/teagram-demo/ops/turnstile-signin-rollback.sh
source "$REPO/deploy/teagram-demo/ops/turnstile-signin-rollback.sh"
install_turnstile_traps
python3 "$REPO/deploy/teagram-demo/validate-env.py" "$ENV_FILE" || fail 'current demo environment is invalid'

TMP_DIR=$(mktemp -d /tmp/teagram-demo-turnstile.XXXXXXXX) || fail 'could not create a temporary rollout directory'
chmod 0700 "$TMP_DIR"
BEFORE_RENDER=$TMP_DIR/before-render.json
TARGET_RENDER=$TMP_DIR/target-render.json
demo_render_config "$BEFORE_RENDER" || fail 'current demo Compose configuration is not ready'

expected_flag=$(jq -er --arg flag "$FLAG" '.services.teagramd.environment[$flag]' "$BEFORE_RENDER") ||
  fail 'current Compose signin flag is unavailable'
[[ "$expected_flag" == false || "$expected_flag" == true ]] || fail 'current Compose signin flag is invalid'
active_server=$(demo_active_server) || fail 'active server slot is unavailable'
mapfile -t active_ids < <(demo_compose ps -q "$active_server")
(( ${#active_ids[@]} == 1 )) || fail 'active server container is missing or ambiguous'
container_flag=$(docker inspect --format '{{json .Config.Env}}' "${active_ids[0]}" | \
  jq -er --arg flag "$FLAG" '[.[] | select(startswith($flag + "=")) | split("=")[1]] | if length == 1 then .[0] else error("missing or duplicate flag") end') ||
  fail 'active server signin flag is unavailable'
[[ "$container_flag" == "$expected_flag" ]] || fail 'active server flag differs from Compose'

run_rollout() {
  local image_digest=${1:?image digest is required}
  DEMO_DEPLOY_LOCK_HELD=1 DEPLOY_SHA="$OPS_SHA" DEPLOY_IMAGE_DIGEST="$image_digest" \
    PREVIOUS_SHA="$OPS_SHA" bash "$REPO/deploy/teagram-demo/deploy-main.sh"
}

active_flag() {
  local server
  local -a ids=()
  server=$(demo_active_server) || return 1
  mapfile -t ids < <(demo_compose ps -q "$server")
  (( ${#ids[@]} == 1 )) || return 1
  docker inspect --format '{{json .Config.Env}}' "${ids[0]}" | \
    jq -er --arg flag "$FLAG" '[.[] | select(startswith($flag + "=")) | split("=")[1]] | if length == 1 then .[0] else error("missing or duplicate flag") end'
}

backup_name="env.before-turnstile-${OPS_SHA}-${BASHPID}-${RANDOM}"
backup_candidate="$BACKUP_DIR/$backup_name"
[[ ! -e "$backup_candidate" && ! -L "$backup_candidate" ]] || fail 'environment backup path already exists'
ENV_BACKUP=$backup_candidate
if update_output=$(ROLLOUT_RUNNER_ADDED_ENV="$FLAG" ROLLOUT_RUNNER_CHANGED_ENV="$FLAG" \
  python3 "$REPO/deploy/teagram-demo/update_turnstile_signin.py" \
    --env-file "$ENV_FILE" --backup-dir "$BACKUP_DIR" --backup-path "$ENV_BACKUP" --target "$TARGET"); then
  :
else
  printf '%s\n' "$update_output" >&2
  fail 'approved environment update failed'
fi
printf '%s\n' "$update_output"
changed=$(sed -n 's/.*changed=\([^ ]*\).*/\1/p' <<< "$update_output")
if [[ "$changed" == false ]]; then
  ENV_BACKUP=
  [[ "$expected_flag" == "$TARGET" ]] || fail 'no-op environment update does not match the active flag'
  printf 'rollout=already-effective effective_flag=%s active_server=%s\n' "$TARGET" "$active_server"
  exit 0
fi
reported_backup=$(sed -n 's/.*backup=\([^ ]*\).*/\1/p' <<< "$update_output")
[[ "$reported_backup" == "$ENV_BACKUP" ]] || fail 'environment backup record differs from the prepared path'
[[ "$changed" == true && -f "$ENV_BACKUP" && ! -L "$ENV_BACKUP" ]] || fail 'environment backup record is invalid'
[[ "$(stat -c '%u:%a' "$ENV_BACKUP")" == 0:600 ]] || fail 'environment backup is not root-only'

if ! python3 "$REPO/deploy/teagram-demo/validate-env.py" "$ENV_FILE"; then
  restore_env || fail 'environment validation failed and the previous environment could not be restored'
  fail 'updated demo environment is invalid'
fi
demo_render_config "$TARGET_RENDER" || {
  restore_env || fail 'Compose validation failed and the previous environment could not be restored'
  fail 'updated demo Compose configuration is invalid'
}
jq -e --arg flag "$FLAG" --arg target "$TARGET" '
  .services.teagramd.environment[$flag] == $target
  and .services["teagramd-next"].environment[$flag] == $target
  and .services.teagramd.environment.TG_ADVERTISE_DEMO_GUEST_SIGN_UP == "true"
  and .services["teagramd-next"].environment.TG_ADVERTISE_DEMO_GUEST_SIGN_UP == "true"
  and .services.teagramd.environment.TG_TURNSTILE_SITE_KEY != ""
  and .services.teagramd.environment.TG_TURNSTILE_SECRET_KEY_FILE == "/run/secrets/teagram_demo_turnstile"
  and .services["teagramd-next"].environment.TG_TURNSTILE_SITE_KEY != ""
  and .services["teagramd-next"].environment.TG_TURNSTILE_SECRET_KEY_FILE == "/run/secrets/teagram_demo_turnstile"
  and .services.edge.ports[0].host_ip == "127.0.0.1"
  and (.services.edge.ports[0].published | tostring) == "8080"
  and ([.services[] | (.ports // [])[]? | .published | tostring] | index("2443")) == null
' "$TARGET_RENDER" >/dev/null || {
  restore_env || fail 'target Compose verification failed and the previous environment could not be restored'
  fail 'target config changed signup, Turnstile credentials, or listener exposure'
}

image_ref=$(demo_active_image_ref) || {
  restore_env || fail 'active image lookup failed and the previous environment could not be restored'
  fail 'active image reference is unavailable'
}
digest=${image_ref##*@}
[[ "$image_ref" == "$DEMO_IMAGE_REPOSITORY@$digest" && "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || {
  restore_env || fail 'active image validation failed and the previous environment could not be restored'
  fail 'active image is not a pinned demo artifact'
}
image_id=$(docker image inspect --format '{{.Id}}' "$image_ref") || {
  restore_env || fail 'active image inspection failed and the previous environment could not be restored'
  fail 'active image identity is unavailable'
}
image_revision=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$image_id") || {
  restore_env || fail 'active image revision lookup failed and the previous environment could not be restored'
  fail 'active image revision is unavailable'
}
[[ "$image_revision" == "$OPS_SHA" ]] || {
  restore_env || fail 'active image mismatch and the previous environment could not be restored'
  fail 'active image revision differs from the reviewed checkout'
}

if run_rollout "$digest"; then
  :
else
  rollout_status=$?
  restore_previous_runtime "$expected_flag"
  exit "$rollout_status"
fi

active_after=$(demo_active_server) || {
  restore_previous_runtime "$expected_flag"
  fail 'deployment finished but the active server slot is unavailable'
}
flag_after=$(active_flag) || {
  restore_previous_runtime "$expected_flag"
  fail 'deployment finished but the effective flag could not be read'
}
if [[ "$flag_after" != "$TARGET" ]]; then
  restore_previous_runtime "$expected_flag"
  fail 'post-deploy flag did not match the requested target'
fi
if ! python3 "$REPO/deploy/teagram-demo/probe_tailnet_isolation.py"; then
  restore_previous_runtime "$expected_flag"
  fail 'tailnet isolation probe failed; the previous signin flag was restored'
fi
ROLLOUT_COMMITTED=1
printf 'rollout=pass target_revision=%s active_server=%s readiness=pass effective_flag=%s environment_backup=%s\n' \
  "$OPS_SHA" "$active_after" "$flag_after" "$ENV_BACKUP"
