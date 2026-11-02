#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

readonly REPO=/opt/teagram
readonly POSTGRES_CONTAINER=teagram-postgres-1
readonly SERVER_CONTAINER=teagram-teagramd-1
readonly EXPECTED_COMPOSE_FILES="$REPO/docker-compose.yml,$REPO/docker-compose.override.yml"
readonly EXPECTED_POSTGRES_SERVICE=postgres
readonly EXPECTED_SERVER_SERVICE=teagramd

fail() {
  printf 'server start rejected: %s\n' "$1" >&2
  exit 1
}

if [[ "$#" != 0 ]]; then
  printf '%s\n' 'usage: start-server.sh' >&2
  exit 64
fi
[[ "$(id -u)" == 0 ]] || fail 'root is required'
[[ "${OPS_SHA:-}" =~ ^[0-9a-f]{40}$ ]] || fail 'reviewed source SHA is invalid'

exec 9>/tmp/teagram-deploy.lock || fail 'cannot open the deployment lock'
flock -x 9 || fail 'cannot acquire the deployment lock'

TEMP_DIR=$(mktemp -d /tmp/teagram-start-server.XXXXXXXX) || fail 'cannot create the private helper directory'
[[ -d "$TEMP_DIR" && ! -L "$TEMP_DIR" && "$(stat -c '%a' -- "$TEMP_DIR")" == 700 ]] || {
  fail 'private helper directory failed validation'
}
cleanup() {
  local status=$?
  trap - EXIT
  case "${TEMP_DIR:-}" in
    /tmp/teagram-start-server.????????)
      if [[ -d "$TEMP_DIR" && ! -L "$TEMP_DIR" ]]; then
        rm -rf -- "$TEMP_DIR" || status=1
      else
        status=1
      fi
      ;;
    *)
      printf '%s\n' 'refusing to remove an unexpected helper path' >&2
      status=1
      ;;
  esac
  exit "$status"
}
trap cleanup EXIT

container_state=
container_project=
container_service=
container_compose_files=
inspect_container() {
  local name=$1 inspection
  inspection=$(docker inspect --format '{{.State.Status}}|{{index .Config.Labels "com.docker.compose.project"}}|{{index .Config.Labels "com.docker.compose.service"}}|{{index .Config.Labels "com.docker.compose.project.config_files"}}' "$name") || {
    fail "cannot inspect existing $name container"
  }
  IFS='|' read -r container_state container_project container_service container_compose_files <<< "$inspection"
  [[ -n "$container_state" && -n "$container_project" && -n "$container_service" && -n "$container_compose_files" ]] || {
    fail "existing $name container has incomplete Compose identity"
  }
  case "$container_state" in
    running|exited|created) ;;
    *) fail "existing $name container state is not restartable" ;;
  esac
}

inspect_container "$SERVER_CONTAINER"
[[ "$container_project" == teagram && "$container_service" == "$EXPECTED_SERVER_SERVICE" ]] || {
  fail 'teagramd container is not in the teagram Compose project'
}
LIVE_COMPOSE_FILES=$container_compose_files
[[ "$LIVE_COMPOSE_FILES" == "$EXPECTED_COMPOSE_FILES" ]] || {
  fail 'live Compose selection is not the reviewed teagram S3 stack'
}
for compose_file in "$REPO/docker-compose.yml" "$REPO/docker-compose.override.yml"; do
  [[ -f "$compose_file" && ! -L "$compose_file" ]] || fail 'live Compose input is missing or symlinked'
done

inspect_container "$POSTGRES_CONTAINER"
[[ "$container_project" == teagram && "$container_service" == "$EXPECTED_POSTGRES_SERVICE" ]] || {
  fail 'Postgres container is not in the teagram Compose project'
}
[[ "$container_compose_files" == "$LIVE_COMPOSE_FILES" ]] || {
  fail 'server containers do not share the inspected Compose selection'
}
inspect_container "$SERVER_CONTAINER"
[[ "$container_compose_files" == "$LIVE_COMPOSE_FILES" ]] || {
  fail 'teagramd Compose selection changed during preflight'
}

git -C "$REPO" show "$OPS_SHA:deploy/teagramd/rollout-runner/blob-transition-runner.py" \
  > "$TEMP_DIR/blob-transition-runner.py" || fail 'cannot stage the reviewed blob authority validator'
git -C "$REPO" show "$OPS_SHA:deploy/teagramd/rollout-runner/blob-mode-state.py" \
  > "$TEMP_DIR/blob-mode-state.py" || fail 'cannot stage the reviewed blob state validator'
git -C "$REPO" show "$OPS_SHA:deploy/teagramd/rollout-runner/qualify-rustfs-transition.py" \
  > "$TEMP_DIR/qualify-rustfs-transition.py" || fail 'cannot stage the reviewed transition report validator'
git -C "$REPO" show "$OPS_SHA:deploy/teagramd/rollout-runner/rustfs-inert-surfaces.sql" \
  > "$TEMP_DIR/rustfs-inert-surfaces.sql" || fail 'cannot stage the reviewed 60-69 transition query'
git -C "$REPO" show "$OPS_SHA:deploy/teagramd/rollout-runner/rustfs-r70-inert-surfaces.sql" \
  > "$TEMP_DIR/rustfs-r70-inert-surfaces.sql" || fail 'cannot stage the reviewed 60-70 transition query'
chmod 600 "$TEMP_DIR/blob-transition-runner.py" "$TEMP_DIR/blob-mode-state.py" \
  "$TEMP_DIR/qualify-rustfs-transition.py" "$TEMP_DIR/rustfs-inert-surfaces.sql" \
  "$TEMP_DIR/rustfs-r70-inert-surfaces.sql" || {
  fail 'cannot protect the staged blob authority validators'
}

authority_json=$(cd "$REPO" && python3 "$TEMP_DIR/blob-transition-runner.py" deployment-env \
  --checkout "$REPO" --state-dir "$REPO/.state/blob-mode" \
  --report-root /root --lock-path /tmp/teagram-deploy.lock) || {
  fail 'live S3 authority or credential selection is not verified'
}
env_file_lines=$(jq -er '
  select(.outcome == "s3-accepted") | .env_files |
  if type == "array" and length == 2 and all(.[]; type == "string" and length > 0)
  then .[] else error("invalid environment file list") end
' <<< "$authority_json") || fail 'live S3 environment selection is invalid'
mapfile -t COMPOSE_ENV_FILE_PATHS <<< "$env_file_lines"
[[ "${#COMPOSE_ENV_FILE_PATHS[@]}" == 2 && "${COMPOSE_ENV_FILE_PATHS[0]}" == "$REPO/.env" ]] || {
  fail 'live S3 environment selection does not start with the verified .env'
}
legacy_parked="$(dirname "$REPO")/telegram-server/.env.rustfs-parked"
case "${COMPOSE_ENV_FILE_PATHS[1]}" in
  "$REPO/.env.rustfs-parked"|"$legacy_parked"|/root/.env.rustfs-parked) ;;
  *) fail 'live S3 environment selection includes an unapproved credential path' ;;
esac
for env_file in "${COMPOSE_ENV_FILE_PATHS[@]}"; do
  [[ -f "$env_file" && ! -L "$env_file" ]] || fail 'verified Compose environment file is unavailable'
done

start_existing_container() {
  local name=$1 expected_service=$2
  inspect_container "$name"
  [[ "$container_project" == teagram && "$container_service" == "$expected_service" ]] || {
    fail "$expected_service container is not in the teagram Compose project"
  }
  [[ "$container_compose_files" == "$LIVE_COMPOSE_FILES" ]] || {
    fail "$expected_service container no longer matches the inspected Compose selection"
  }
  if [[ "$container_state" != running ]]; then
    docker start "$name" >/dev/null || fail "could not start the existing $expected_service container"
  fi
  inspect_container "$name"
  [[ "$container_project" == teagram && "$container_service" == "$expected_service" &&
     "$container_compose_files" == "$LIVE_COMPOSE_FILES" ]] || {
    fail "$expected_service container identity changed during startup"
  }
  [[ "$container_state" == running ]] || fail "$expected_service did not enter the running state"
}

start_existing_container "$POSTGRES_CONTAINER" "$EXPECTED_POSTGRES_SERVICE"

sleep 5
postgres_ready=0
for attempt in {1..28}; do
  if docker exec "$POSTGRES_CONTAINER" pg_isready -U postgres -d teagram >/dev/null 2>&1; then
    postgres_ready=1
    break
  fi
  if [[ "$attempt" != 28 ]]; then
    sleep 2
  fi
done
[[ "$postgres_ready" == 1 ]] || fail 'Postgres did not become ready within 60 seconds'

start_existing_container "$SERVER_CONTAINER" "$EXPECTED_SERVER_SERVICE"

printf 'postgres=ready teagramd=running compose_selection=verified\n'
