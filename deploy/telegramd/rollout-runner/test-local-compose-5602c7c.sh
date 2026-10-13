#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(git -C "$SCRIPT_DIR" rev-parse --show-toplevel)
readonly ARTIFACT_NAME=.rollout-compose.local-5602c7c.yml
readonly ARTIFACT_SHA=05fc5a3e7dbdc2feac58fce870796baebc055b617bc81f6d6b206c5a2c184cc5
readonly CONTENT_SHA=577dd9885aa9babd3370a97417d1653884f0567c6f50e7feb208ca9d11ca15bd
readonly APP_TARGET_SHA=5602c7c13eb1571278f43129345a4b52f0630377
TMP=$(mktemp -d "${TMPDIR:-/tmp}/telegramd-local-compose-5602c7c.XXXXXXXX")
chmod 700 "$TMP"
readonly WORKTREE="$TMP/application"
readonly DOCKER_CONFIG="$TMP/docker-config"
readonly DOCKER_HOME="$TMP/home"
readonly ORIGINAL_HOME=${HOME:-}
DOCKER_ENDPOINT=${DOCKER_HOST:-}
WORKTREE_ADDED=0
SUPPLIED_IMAGE=
UNSET_IMAGE=

cleanup() {
  local status=$?
  trap - EXIT
  for image_name in "$SUPPLIED_IMAGE" "$UNSET_IMAGE"; do
    [ -n "$image_name" ] || continue
    if env -i PATH="$PATH" HOME="$DOCKER_HOME" DOCKER_CONFIG="$DOCKER_CONFIG" DOCKER_HOST="$DOCKER_ENDPOINT" \
      docker image inspect "$image_name" >/dev/null 2>&1; then
      env -i PATH="$PATH" HOME="$DOCKER_HOME" DOCKER_CONFIG="$DOCKER_CONFIG" DOCKER_HOST="$DOCKER_ENDPOINT" \
        docker image rm "$image_name" >/dev/null || status=$?
    fi
  done
  if [ "$WORKTREE_ADDED" = 1 ]; then
    git -C "$REPO_ROOT" worktree remove --force "$WORKTREE" || status=$?
  fi
  rm -rf -- "$TMP" || status=$?
  exit "$status"
}
trap cleanup EXIT

if [ -z "$DOCKER_ENDPOINT" ]; then
  active_context=$(docker context show)
  DOCKER_ENDPOINT=$(docker context inspect "$active_context" --format '{{.Endpoints.docker.Host}}')
fi
readonly DOCKER_ENDPOINT

mkdir -m 700 "$DOCKER_CONFIG" "$DOCKER_HOME"
mkdir -m 700 "$DOCKER_CONFIG/cli-plugins"
docker_compose_plugin=
for plugin_path in \
  "$ORIGINAL_HOME/.docker/cli-plugins/docker-compose" \
  /usr/local/lib/docker/cli-plugins/docker-compose \
  /usr/local/libexec/docker/cli-plugins/docker-compose \
  /usr/lib/docker/cli-plugins/docker-compose \
  /usr/libexec/docker/cli-plugins/docker-compose; do
  if [ -x "$plugin_path" ]; then
    docker_compose_plugin=$plugin_path
    break
  fi
done
[ -n "$docker_compose_plugin" ] || {
  printf '%s\n' 'Docker Compose CLI plugin was not found in the supported plugin directories' >&2
  exit 1
}
ln -s -- "$docker_compose_plugin" "$DOCKER_CONFIG/cli-plugins/docker-compose"
docker_buildx_plugin=
for plugin_path in \
  "$ORIGINAL_HOME/.docker/cli-plugins/docker-buildx" \
  /usr/local/lib/docker/cli-plugins/docker-buildx \
  /usr/local/libexec/docker/cli-plugins/docker-buildx \
  /usr/lib/docker/cli-plugins/docker-buildx \
  /usr/libexec/docker/cli-plugins/docker-buildx; do
  if [ -x "$plugin_path" ]; then
    docker_buildx_plugin=$plugin_path
    break
  fi
done
if [ -n "$docker_buildx_plugin" ]; then
  ln -s -- "$docker_buildx_plugin" "$DOCKER_CONFIG/cli-plugins/docker-buildx"
fi
suffix=${TMP##*.}
suffix=${suffix,,}
SUPPLIED_IMAGE="telegramd:revision-gate-$suffix-supplied"
UNSET_IMAGE="telegramd:revision-gate-$suffix-unset"

actual_sha=$(sha256sum "$SCRIPT_DIR/local-compose-5602c7c.yml")
actual_sha=${actual_sha%% *}
[ "$actual_sha" = "$ARTIFACT_SHA" ] || {
  printf '%s\n' '5602c7c target-local Compose artifact digest differs from the reviewed pin' >&2
  exit 1
}
actual_content_sha=$(awk '!/^[[:space:]]*#/' "$SCRIPT_DIR/local-compose-5602c7c.yml" | sha256sum)
actual_content_sha=${actual_content_sha%% *}
[ "$actual_content_sha" = "$CONTENT_SHA" ] || {
  printf '%s\n' '5602c7c target-local Compose body digest differs from the reviewed pin' >&2
  exit 1
}
grep -Fq "SOURCE_LOCAL_COMPOSE_TARGET_SHA=$APP_TARGET_SHA" "$SCRIPT_DIR/rollout-runner.sh" || {
  printf '%s\n' 'runner target pin differs from the 5602c7c application target' >&2
  exit 1
}
grep -Fq "SOURCE_LOCAL_COMPOSE_SHA=$ARTIFACT_SHA" "$SCRIPT_DIR/rollout-runner.sh" || {
  printf '%s\n' 'runner artifact digest differs from the target-local Compose artifact' >&2
  exit 1
}
grep -Fq "SOURCE_LOCAL_COMPOSE_CONTENT_SHA=$CONTENT_SHA" "$SCRIPT_DIR/rollout-runner.sh" || {
  printf '%s\n' 'runner body digest differs from the target-local Compose artifact' >&2
  exit 1
}
printf '%s\n' 'PASS artifact and comment-independent body match the reviewed target pins'

if ! git -C "$REPO_ROOT" cat-file -e "$APP_TARGET_SHA^{commit}" 2>/dev/null; then
  git -C "$REPO_ROOT" fetch -q --no-tags origin "$APP_TARGET_SHA"
fi
git -C "$REPO_ROOT" worktree add --quiet --detach "$WORKTREE" "$APP_TARGET_SHA"
WORKTREE_ADDED=1
[ "$(git -C "$WORKTREE" rev-parse HEAD)" = "$APP_TARGET_SHA" ] || {
  printf '%s\n' 'isolated application checkout does not match the pinned target' >&2
  exit 1
}
grep -Fq 'LABEL org.opencontainers.image.revision="${TELEGRAMD_SOURCE_REVISION}"' "$WORKTREE/Dockerfile" || {
  printf '%s\n' 'pinned application target does not label images from TELEGRAMD_SOURCE_REVISION' >&2
  exit 1
}
cp -- "$SCRIPT_DIR/local-compose-5602c7c.yml" "$WORKTREE/$ARTIFACT_NAME"

write_override() {
  local image_name=$1 path=$2
  cat > "$path" <<YAML
services:
  telegramd:
    image: $image_name
    ports:
      - "127.0.0.1:2444:2444"
      - "100.64.0.10:2445:2445"
YAML
  chmod 600 "$path"
}

compose_config() {
  local override=$1
  shift
  local -a compose_env=()
  while [ "$#" -gt 0 ] && [[ "$1" == *=* ]]; do
    compose_env+=("$1")
    shift
  done
  env -i PATH="$PATH" HOME="$DOCKER_HOME" DOCKER_CONFIG="$DOCKER_CONFIG" DOCKER_HOST="$DOCKER_ENDPOINT" \
    POSTGRES_PASSWORD=fixture TG_PUBLIC_LINK_PREFIX=https://links.invalid "${compose_env[@]}" \
    docker compose --env-file /dev/null --project-directory "$WORKTREE" \
      --file "$WORKTREE/$ARTIFACT_NAME" --file "$override" "$@"
}

compose_build() {
  local override=$1
  shift
  local -a compose_env=()
  while [ "$#" -gt 0 ] && [[ "$1" == *=* ]]; do
    compose_env+=("$1")
    shift
  done
  timeout --signal=TERM --kill-after=15s 360s \
    env -i PATH="$PATH" HOME="$DOCKER_HOME" DOCKER_CONFIG="$DOCKER_CONFIG" DOCKER_HOST="$DOCKER_ENDPOINT" \
      POSTGRES_PASSWORD=fixture TG_PUBLIC_LINK_PREFIX=https://links.invalid "${compose_env[@]}" \
      docker compose --env-file /dev/null --project-directory "$WORKTREE" \
        --file "$WORKTREE/$ARTIFACT_NAME" --file "$override" build -q telegramd
}

verify_render() {
  local override=$1 output=$2 expected_revision=$3 expected_image=$4
  shift 4
  compose_config "$override" "$@" config --quiet
  compose_config "$override" "$@" config --format json > "$output"
  jq -e --arg revision "$expected_revision" --arg image "$expected_image" --arg root "$WORKTREE" '
    (.services | keys) == ["migrate", "postgres", "telegramd"]
    and .services.telegramd.build.context == $root
    and .services.telegramd.build.args.TELEGRAMD_SOURCE_REVISION == $revision
    and .services.telegramd.image == $image
    and .services.telegramd.environment.TG_BLOB_DIR == "/var/lib/telegramd-blobs"
    and .services.telegramd.environment.TG_PUBLIC_LINK_PREFIX == "https://links.invalid"
    and (.services.telegramd.environment | keys | sort) == ["TG_AUTHKEY_ENC_KEY", "TG_AUTHKEY_ENC_KEY_FILE", "TG_BLOB_DIR", "TG_CLIENT_ADDR_TRUST", "TG_LOG_LOGIN_CODES", "TG_POSTGRES_DSN", "TG_PUBLIC_LINK_PREFIX", "TG_REGISTRATION", "TG_REPLICA_COUNT", "TG_REPLICA_ID", "TG_RSA_KEY_FINGERPRINT"]
    and .services.telegramd.environment.TG_AUTHKEY_ENC_KEY == ""
    and .services.telegramd.environment.TG_AUTHKEY_ENC_KEY_FILE == "/var/lib/telegramd/enc_key.hex"
    and .services.telegramd.environment.TG_LOG_LOGIN_CODES == "true"
    and .services.telegramd.environment.TG_REGISTRATION == "closed"
    and .services.telegramd.environment.TG_REPLICA_ID == ""
    and .services.telegramd.environment.TG_REPLICA_COUNT == "1"
    and .services.telegramd.environment.TG_CLIENT_ADDR_TRUST == "socket"
    and .services.telegramd.environment.TG_RSA_KEY_FINGERPRINT == ""
    and .services.telegramd.environment.TG_POSTGRES_DSN == "postgres://postgres:fixture@postgres:5432/telegram?sslmode=disable"
    and .services.postgres.environment.POSTGRES_PASSWORD == "fixture"
    and .services.postgres.environment.POSTGRES_DB == "telegram"
    and .services.postgres.image == "postgres:16-alpine"
    and .services.migrate.image == "arigaio/atlas:1.2.0-alpine"
    and ((.services.postgres.ports // []) | length) == 0
    and .services.telegramd.read_only == true
    and ([.services.telegramd.environment | keys[] | select(startswith("TG_BLOB_S3_"))] | length) == 0
    and ([.services.telegramd.environment | keys[] | select(startswith("RUSTFS_"))] | length) == 0
    and ([.services.telegramd.depends_on | keys[]] == ["migrate"])
    and .services.migrate.depends_on.postgres.condition == "service_healthy"
    and .services.telegramd.depends_on.migrate.condition == "service_completed_successfully"
    and ((.services.telegramd.secrets // []) | length) == 0
    and ((.secrets // {}) | length) == 0
    and (.services.telegramd.stop_grace_period == "120s" or .services.telegramd.stop_grace_period == "2m0s")
    and any(.services.telegramd.volumes[]; .target == "/var/lib/telegramd-blobs" and .type == "volume" and .source == "tgblobs" and (.read_only // false) == false)
    and any(.services.telegramd.volumes[]; .target == "/var/lib/telegramd" and .type == "volume" and .source == "tgkey")
    and any(.services.telegramd.volumes[]; .target == "/run/telegramd/blob-mode" and .read_only == true)
    and (.volumes.pgdata.name == "telegram-server_pgdata")
    and (.volumes.tgkey.name == "telegram-server_tgkey")
    and (.volumes.tgblobs.name == "telegram-server_tgblobs")
    and any(.services.telegramd.ports[]; .target == 2443 and .host_ip == "127.0.0.1")
    and any(.services.telegramd.ports[]; .target == 2444 and .host_ip == "127.0.0.1")
    and any(.services.telegramd.ports[]; .target == 2445 and .host_ip == "100.64.0.10")
  ' "$output" >/dev/null || {
    printf '%s\n' 'rendered target-local Compose contract differs from the reviewed local rollout' >&2
    return 1
  }
}

build_and_inspect() {
  local mode=$1 override=$2 expected_revision=$3 image_name=$4 actual_revision
  shift 4
  compose_build "$override" "$@"
  actual_revision=$(env -i PATH="$PATH" HOME="$DOCKER_HOME" DOCKER_CONFIG="$DOCKER_CONFIG" DOCKER_HOST="$DOCKER_ENDPOINT" \
    docker image inspect "$image_name" --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}')
  [ "$actual_revision" = "$expected_revision" ] || {
    printf 'image revision label mismatch for %s (%s): expected <%s>, got <%s>\n' \
      "$mode" "$image_name" "$expected_revision" "$actual_revision" >&2
    return 1
  }
  printf 'PASS %s build label equals <%s> from isolated source %s\n' \
    "$mode" "$expected_revision" "$APP_TARGET_SHA"
}

supplied_override="$TMP/override-supplied.yml"
unset_override="$TMP/override-unset.yml"
write_override "$SUPPLIED_IMAGE" "$supplied_override"
write_override "$UNSET_IMAGE" "$unset_override"

verify_render "$supplied_override" "$TMP/render-supplied.json" "$APP_TARGET_SHA" "$SUPPLIED_IMAGE" \
  TELEGRAMD_SOURCE_REVISION="$APP_TARGET_SHA"
printf '%s\n' 'PASS supplied revision renders as the full build argument and preserves synthetic override bindings'
build_and_inspect supplied "$supplied_override" "$APP_TARGET_SHA" "$SUPPLIED_IMAGE" \
  TELEGRAMD_SOURCE_REVISION="$APP_TARGET_SHA"

verify_render "$unset_override" "$TMP/render-unset.json" '' "$UNSET_IMAGE"
printf '%s\n' 'PASS unset revision renders as an empty build argument'
build_and_inspect unset "$unset_override" '' "$UNSET_IMAGE"

[ ! -e "$WORKTREE/.env" ] && [ ! -e "$WORKTREE/.secrets" ] || {
  printf '%s\n' 'credential-free revision fixture unexpectedly created credential files' >&2
  exit 1
}
printf '%s\n' 'PASS supplied and unset images were inspected without provisioning credentials or secrets'
