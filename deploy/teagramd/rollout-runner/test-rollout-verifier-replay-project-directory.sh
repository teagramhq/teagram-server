#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
if [ "$(id -u)" != 0 ]; then
  printf '%s\n' 'run the verifier replay project-directory fixture as root' >&2
  exit 77
fi

TMP=$(mktemp -d /tmp/main2612-replay.XXXXXXXX)
mkdir -m 700 -- "$TMP/baseline" "$TMP/preflight" "$TMP/checkout" "$TMP/project" "$TMP/bin"
cleanup() {
  rm -f -- \
    "$TMP/baseline/replay.snapshot.json" \
    "$TMP/checkout/.env" "$TMP/checkout/.env.rustfs-parked" "$TMP/checkout/docker-compose.override.yml" \
    "$TMP/bin/docker" "$TMP/docker-args" "$TMP/compose-file" "$TMP/inspect-json"
  rmdir -- "$TMP/baseline" "$TMP/preflight" "$TMP/checkout" "$TMP/project" "$TMP/bin" "$TMP"
}
trap cleanup EXIT

printf 'FIXTURE_VALUE=non-secret\n' > "$TMP/checkout/.env"
printf 'FIXTURE_EXTRA=non-secret\n' > "$TMP/checkout/.env.rustfs-parked"
: > "$TMP/checkout/docker-compose.override.yml"
chmod 600 -- "$TMP/checkout/.env" "$TMP/checkout/.env.rustfs-parked" "$TMP/checkout/docker-compose.override.yml"
cat > "$TMP/bin/docker" <<'DOCKER'
#!/usr/bin/env bash
set -Eeuo pipefail
if [ "$1" = inspect ]; then
  printf '[]\n' > "$INSPECT_JSON"
  printf '[]\n'
elif [ "$1" = compose ]; then
  printf '%s\0' "$@" > "$DOCKER_ARGS"
  printf '%s\n' "$COMPOSE_FILE" > "$COMPOSE_FILE_CAPTURE"
  printf '{}\n'
else
  printf 'unexpected docker command: %s\n' "$1" >&2
  exit 99
fi
DOCKER
chmod 700 -- "$TMP/bin/docker"
export INSPECT_JSON="$TMP/inspect-json" DOCKER_ARGS="$TMP/docker-args" COMPOSE_FILE_CAPTURE="$TMP/compose-file"
PATH="$TMP/bin:$PATH"
export PATH

ROLLOUT_VERIFIER_SOURCE_ONLY=1
source "$SCRIPT_DIR/rollout-verifier.sh"
initialize_verifier_config
ROLLOUT_CHECKOUT_PATH="$TMP/checkout"
ROLLOUT_COMPOSE_PROJECT_DIRECTORY="$TMP/project"
ROLLOUT_COMPOSE_ENV_FILES="$TMP/checkout/.env,$TMP/checkout/.env.rustfs-parked"
COMPOSE_FILE="$TMP/checkout/docker-compose.yml:$TMP/checkout/docker-compose.override.yml"
export ROLLOUT_CHECKOUT_PATH ROLLOUT_COMPOSE_PROJECT_DIRECTORY ROLLOUT_COMPOSE_ENV_FILES COMPOSE_FILE

snapshot_from_json() {
  printf '%s\n' '{"fixture":true}'
}

cd "$ROLLOUT_CHECKOUT_PATH"
snapshot_command aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
  "$TMP/baseline" replay "$TMP/checkout/.env" "$TMP/checkout/docker-compose.override.yml" selected-compose-render >/dev/null
mapfile -d '' -t docker_args < "$DOCKER_ARGS"
[ "${docker_args[0]}" = compose ]
[ "${docker_args[1]}" = --project-directory ]
[ "${docker_args[2]}" = "$TMP/project" ]
[ "${docker_args[3]}" = --env-file ]
[ "${docker_args[4]}" = "$TMP/checkout/.env" ]
[ "${docker_args[5]}" = --env-file ]
[ "${docker_args[6]}" = "$TMP/checkout/.env.rustfs-parked" ]
[ "${docker_args[7]}" = config ]
[ "${docker_args[8]}" = --format ]
[ "${docker_args[9]}" = json ]
[ "$(cat "$COMPOSE_FILE_CAPTURE")" = "$COMPOSE_FILE" ]
[ -f "$TMP/baseline/replay.snapshot.json" ]
[ "$(stat -c %a -- "$TMP/baseline/replay.snapshot.json")" = 600 ]
printf '%s\n' 'PASS verifier replay keeps the source project directory and guarded environment file'
