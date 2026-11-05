#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

readonly REPO=/opt/teagram-demo
readonly OPS_SHA=${OPS_SHA:-}
readonly OPS_NAME=${OPS_NAME:-}
readonly OP_NAME=rotate-turnstile-qa-bypass-token
readonly SOURCE_FILES="deploy/teagram-demo/demo-common.sh
deploy/teagram-demo/compose.yaml
deploy/teagram-demo/haproxy.cfg
deploy/teagram-demo/render-haproxy-config.py
deploy/teagram-demo/nginx-config/nginx.conf
deploy/teagram-demo/validate-env.py
deploy/teagram-demo/haproxy-control.py"
TMP_DIR=
ACTIVE_SERVER=
INACTIVE_SERVER=
ACTIVE_CONTAINER_ID=
ROTATION_INSTALLED=0
ROTATION_VERIFIED=0
ROTATION_MARKER=

fail() {
  printf 'demo QA bypass rotation: %s\n' "$1" >&2
  exit 1
}

[[ "$EUID" -eq 0 ]] || fail 'operation must run as root'
[[ "$OPS_NAME" == "$OP_NAME" ]] || fail 'unexpected operation name'
[[ "$OPS_SHA" =~ ^[0-9a-f]{40}$ ]] || fail 'reviewed SHA is invalid'
[[ -d "$REPO/.git" && ! -L "$REPO" ]] || fail 'demo checkout is unavailable'
git -C "$REPO" merge-base --is-ancestor "$OPS_SHA" refs/remotes/origin/main ||
  fail 'reviewed SHA is not on origin/main'
git --no-optional-locks -C "$REPO" diff --quiet HEAD -- ||
  fail 'demo checkout has tracked changes'

while IFS= read -r tracked; do
  expected=$(git -C "$REPO" rev-parse "$OPS_SHA:$tracked") ||
    fail "reviewed source is missing: $tracked"
  actual=$(git -C "$REPO" hash-object "$REPO/$tracked") ||
    fail "live source is missing: $tracked"
  [[ "$actual" == "$expected" ]] || fail "live source differs from the reviewed SHA: $tracked"
done <<<"$SOURCE_FILES"

[[ -d "$REPO/.state" && ! -L "$REPO/.state" &&
  -d "$REPO/.state/teagram-demo" && ! -L "$REPO/.state/teagram-demo" ]] ||
  fail 'demo state path is unavailable or unsafe'
readonly LOCK_PATH=$REPO/.state/deploy.lock
[[ ! -L "$LOCK_PATH" ]] || fail 'demo deployment lock is a symlink'
exec 9> "$LOCK_PATH"
flock -x 9

# shellcheck disable=SC1091 # Loaded from the pinned demo checkout at runtime.
source "$REPO/deploy/teagram-demo/demo-common.sh"
ACTIVE_SERVER=$(demo_active_server) || fail 'active server slot is unavailable'
INACTIVE_SERVER=$(demo_inactive_server) || fail 'inactive server slot is unavailable'

fail_closed_and_cleanup() {
  local status=$?
  local container_id
  local running_ids
  local server_stopped=0
  trap - EXIT HUP INT TERM
  if [[ -n "$ROTATION_MARKER" && -f "$ROTATION_MARKER" && ! -L "$ROTATION_MARKER" ]]; then
    ROTATION_INSTALLED=1
  fi
  if (( ROTATION_INSTALLED == 1 && ROTATION_VERIFIED == 0 )); then
    if demo_compose stop --timeout 10 "$ACTIVE_SERVER" >/dev/null; then
      server_stopped=1
    elif running_ids=$(docker ps -q \
        --filter label=com.docker.compose.project=teagram-demo \
        --filter label=com.docker.compose.service="$ACTIVE_SERVER"); then
      server_stopped=1
      while IFS= read -r container_id; do
        [[ -n "$container_id" ]] || continue
        if ! docker stop "$container_id" >/dev/null 2>&1; then
          if ! docker kill "$container_id" >/dev/null 2>&1; then
            printf 'demo QA bypass rotation: failed to stop or kill server container %s\n' \
              "$container_id" >&2
            server_stopped=0
            status=1
          fi
        fi
      done <<<"$running_ids"
    else
      printf '%s\n' 'demo QA bypass rotation: could not inspect server containers during fail-closed cleanup' >&2
      status=1
    fi
    if (( server_stopped == 1 )); then
      printf '%s\n' 'demo QA bypass rotation: server stopped; rerun the reviewed operation to install a fresh token' >&2
    elif demo_haproxy_control drain "$ACTIVE_SERVER" >/dev/null; then
      printf '%s\n' 'demo QA bypass rotation: server drained from HAProxy; rerun the reviewed operation to install a fresh token' >&2
    else
      printf '%s\n' 'demo QA bypass rotation: server stop and HAProxy drain could not be confirmed; do not restore the old token' >&2
      status=1
    fi
  fi
  if [[ -n "$TMP_DIR" ]]; then
    case "$TMP_DIR" in
      /tmp/teagram-demo-qa-bypass-rotation.????????) rm -rf -- "$TMP_DIR" || status=1 ;;
      *) printf '%s\n' 'demo QA bypass rotation: refusing unexpected temporary path' >&2; status=1 ;;
    esac
  fi
  exit "$status"
}

trap fail_closed_and_cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
TMP_DIR=$(mktemp -d /tmp/teagram-demo-qa-bypass-rotation.XXXXXXXX) ||
  fail 'could not create the private rotation work directory'
chmod 0700 "$TMP_DIR"
ROTATION_MARKER=$TMP_DIR/token-rotation-started
ROTATOR=$TMP_DIR/rotate-qa-bypass-token.py
git -C "$REPO" show "$OPS_SHA:deploy/teagram-demo/rotate-qa-bypass-token.py" > "$ROTATOR" ||
  fail 'reviewed token rotation helper is missing'
expected_rotator=$(git -C "$REPO" rev-parse "$OPS_SHA:deploy/teagram-demo/rotate-qa-bypass-token.py") ||
  fail 'reviewed token rotation helper identity is unavailable'
actual_rotator=$(git -C "$REPO" hash-object "$ROTATOR") ||
  fail 'extracted token rotation helper identity is unavailable'
[[ "$actual_rotator" == "$expected_rotator" ]] || fail 'extracted token rotation helper differs from the reviewed SHA'

python3 "$REPO/deploy/teagram-demo/validate-env.py" "$DEMO_ENV_FILE" ||
  fail 'current demo environment is invalid'
demo_render_config "$TMP_DIR/compose.json" || fail 'current demo Compose configuration is invalid'
jq -e '
  .services.teagramd.environment.TG_TURNSTILE_QA_BYPASS_TOKEN_FILE == "/run/secrets/teagram_demo_turnstile_qa_bypass"
  and .services["teagramd-next"].environment.TG_TURNSTILE_QA_BYPASS_TOKEN_FILE == "/run/secrets/teagram_demo_turnstile_qa_bypass"
  and .secrets.teagram_demo_turnstile_qa_bypass.file == "/var/lib/teagram-demo/secrets/turnstile-qa-bypass-token"
' "$TMP_DIR/compose.json" >/dev/null || fail 'demo bypass secret mount differs from the reviewed contract'

active_ids=$(demo_compose ps -q "$ACTIVE_SERVER")
[[ "$active_ids" != *$'\n'* ]] || fail 'active server container is ambiguous'
if [[ -n "$active_ids" ]]; then
  ACTIVE_CONTAINER_ID=$active_ids
  active_image_ref=$(demo_active_image_ref) || fail 'active release image is unavailable'
  expected_image_id=$(docker image inspect --format '{{.Id}}' "$active_image_ref") ||
    fail 'pinned active image could not be inspected'
  running_image_id=$(docker inspect --format '{{.Image}}' "$ACTIVE_CONTAINER_ID") ||
    fail 'active server image could not be inspected'
  [[ "$running_image_id" == "$expected_image_id" ]] ||
    fail 'active server image differs from its pinned release'
  container_env=$(docker inspect --format '{{json .Config.Env}}' "$ACTIVE_CONTAINER_ID") ||
    fail 'active server environment could not be inspected'
  jq -e 'index("TG_TURNSTILE_QA_BYPASS_TOKEN_FILE=/run/secrets/teagram_demo_turnstile_qa_bypass") != null' \
    <<<"$container_env" >/dev/null || fail 'active server is not using the reviewed bypass mount'
fi
inactive_ids=$(demo_compose ps -q "$INACTIVE_SERVER")
[[ -z "$inactive_ids" ]] || fail 'inactive server slot is unexpectedly serving'

if rotation_result=$(python3 "$ROTATOR" "$ROTATION_MARKER"); then
  ROTATION_INSTALLED=1
else
  [[ ! -e "$ROTATION_MARKER" && ! -L "$ROTATION_MARKER" ]] || ROTATION_INSTALLED=1
  fail 'the root-only token rotation and verified snapshot failed'
fi
[[ "$rotation_result" == rotation=pass\ token_length=64\ snapshot=/var/backups/teagram-demo-turnstile-qa-bypass/* ]] ||
  fail 'rotation helper returned an invalid result'
ROTATION_INSTALLED=1

demo_compose up -d --no-deps --force-recreate --wait --wait-timeout 120 "$ACTIVE_SERVER" ||
  fail 'server did not become healthy with the rotated token'
active_after_ids=$(demo_compose ps --status running -q "$ACTIVE_SERVER")
[[ -n "$active_after_ids" && "$active_after_ids" != *$'\n'* ]] ||
  fail 'rotated server is missing or ambiguous'
new_container_id=$active_after_ids
[[ -z "$ACTIVE_CONTAINER_ID" || "$new_container_id" != "$ACTIVE_CONTAINER_ID" ]] ||
  fail 'server container was not recreated after token rotation'
demo_haproxy_control wait-healthy "$ACTIVE_SERVER" --timeout 60 --allow-drained ||
  fail 'rotated server did not pass the HAProxy readiness check'
demo_haproxy_control enable "$ACTIVE_SERVER" ||
  fail 'rotated server could not be enabled in HAProxy'
inactive_after_ids=$(demo_compose ps -q "$INACTIVE_SERVER")
[[ -z "$inactive_after_ids" ]] || fail 'inactive server slot became active during rotation'

ROTATION_VERIFIED=1
printf '%s\n' "$rotation_result"
printf 'rotation=pass active_server=%s readiness=haproxy-healthy old_container_replaced=true\n' \
  "$ACTIVE_SERVER"
