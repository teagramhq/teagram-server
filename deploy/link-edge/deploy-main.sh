#!/usr/bin/env bash
# Refreshes the isolated link selector to the tested teagram-server commit and
# verifies the production discovery routes. The landing service and Serve
# handler table are not changed.
set -euo pipefail

fail() {
  printf 'link-edge deploy: %s\n' "$1" >&2
  exit 1
}

[[ $# == 1 ]] || fail 'usage: deploy-main.sh <full-commit-sha>'
SHA=$1
case "$SHA" in *[!0-9a-f]*|"") fail 'commit SHA must be a full 40-hex value' ;; esac
[[ ${#SHA} == 40 ]] || fail 'commit SHA must be a full 40-hex value'
: "${TG_INVITE_WEB_SIGNUP_URL:?link-edge deploy: set TG_INVITE_WEB_SIGNUP_URL to the public HTTPS sign-up URL}"

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
REPO_ROOT=$(cd "$SCRIPT_DIR/../.." && pwd -P)
COMPOSE_FILE="$REPO_ROOT/deploy/link-edge/compose.yaml"
IMAGE_SOURCE=https://github.com/teagramhq/teagram-server
PUBLIC_BASE_URL=https://teagram.tailaa4918.ts.net
ROLLBACK_IMAGE=teagram-linkselector:rollback
COMPOSE=(
  docker compose
  --env-file /dev/null
  --project-directory "$REPO_ROOT/deploy/link-edge"
  --file "$COMPOSE_FILE"
  --project-name teagram-link-edge
)

compose() {
  "${COMPOSE[@]}" "$@"
}

check_container() {
  local container=$1 service=$2 config project actual_service health
  config=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project.config_files"}}' "$container") || return 1
  project=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}' "$container") || return 1
  actual_service=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.service"}}' "$container") || return 1
  health=$(docker inspect --format '{{.State.Health.Status}}' "$container") || return 1
  [[ $config == "$COMPOSE_FILE" && $project == teagram-link-edge && $actual_service == "$service" && $health == healthy ]]
}

check_status() {
  local url=$1 expected=$2 status
  if ! status=$(curl -sS --max-time 20 -o /dev/null -w '%{http_code}' "$url"); then
    printf 'link-edge deploy: request failed: %s\n' "$url" >&2
    return 1
  fi
  if [[ $status != "$expected" ]]; then
    printf 'link-edge deploy: %s returned %s, expected %s\n' "$url" "$status" "$expected" >&2
    return 1
  fi
  return 0
}

check_production_routes() {
  local client_json client_response client_status content_type
  check_status http://127.0.0.1:8081/.well-known/telegram-web/version.txt 404 || return 1
  check_status "$PUBLIC_BASE_URL/.well-known/telegram-web/version.txt" 404 || return 1
  check_status "$PUBLIC_BASE_URL/.well-known/telegramd/client" 404 || return 1

  if ! client_response=$(curl -q -sS --max-time 20 -w '\n%{http_code}' "$PUBLIC_BASE_URL/.well-known/teagramd/client"); then
    echo 'link-edge deploy: teagramd discovery document request failed' >&2
    return 1
  fi
  client_status=${client_response##*$'\n'}
  client_json=${client_response%$'\n'*}
  if [[ $client_status != 200 ]]; then
    printf 'link-edge deploy: teagramd discovery document returned %s, expected 200\n' "$client_status" >&2
    return 1
  fi
  if ! python3 "$SCRIPT_DIR/validate-discovery.py" <<<"$client_json"; then
    echo 'link-edge deploy: teagramd discovery document does not match the version 1 contract' >&2
    return 1
  fi

  if ! content_type=$(curl -sS --max-time 20 -o /dev/null -w '%{content_type}' "$PUBLIC_BASE_URL/.well-known/teagram-web/version.txt"); then
    echo 'link-edge deploy: teagram-web version request failed' >&2
    return 1
  fi
  if [[ $content_type != text/plain* ]]; then
    printf 'link-edge deploy: teagram-web version content type was %s, expected text/plain\n' "$content_type" >&2
    return 1
  fi
  check_status "$PUBLIC_BASE_URL/.well-known/teagram-web/version.txt" 200 || return 1
}

rollback_selector() {
  echo 'link-edge deploy: restoring the previous selector image' >&2
  docker image tag "$ROLLBACK_IMAGE" teagram-linkselector:local || return 1
  compose up -d --no-deps --wait --wait-timeout 60 selector || return 1
  local selector_id restored_image restored_revision
  selector_id=$(compose ps --quiet selector) || return 1
  [[ -n $selector_id ]] || return 1
  check_container "$selector_id" selector || return 1
  restored_image=$(docker inspect --format '{{.Image}}' "$selector_id") || return 1
  restored_revision=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$restored_image") || return 1
  [[ $restored_revision == "$current_revision" ]]
}

compose config --quiet || fail 'link-edge Compose configuration is invalid'

selector_id=$(compose ps --quiet selector)
landing_id=$(compose ps --quiet linklanding)
[[ -n $selector_id && -n $landing_id ]] || fail 'the existing selector and landing containers must be present'
check_container "$selector_id" selector || fail 'the live selector does not match the approved Compose project or is unhealthy'
check_container "$landing_id" linklanding || fail 'the live landing service does not match the approved Compose project or is unhealthy'

selector_image=$(docker inspect --format '{{.Image}}' "$selector_id")
current_source=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.source"}}' "$selector_image")
current_revision=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$selector_image")
if [[ $current_source != "$IMAGE_SOURCE" || $current_revision != "$SHA" ]]; then
  docker image tag "$selector_image" "$ROLLBACK_IMAGE" || fail 'cannot preserve the current selector image for rollback'
  if ! EDGE_IMAGE_SOURCE="$IMAGE_SOURCE" EDGE_IMAGE_REVISION="$SHA" compose build selector; then
    fail 'selector image build failed; the running selector was not changed'
  fi
  built_source=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.source"}}' teagram-linkselector:local)
  built_revision=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' teagram-linkselector:local)
  [[ $built_source == "$IMAGE_SOURCE" && $built_revision == "$SHA" ]] ||
    fail 'built selector image provenance does not match the tested commit'

  if ! compose up -d --no-deps --wait --wait-timeout 60 selector; then
    if ! rollback_selector; then
      fail 'selector rollout failed and the previous image could not be restored'
    fi
    fail 'selector rollout failed; the previous image was restored'
  fi

  selector_id=$(compose ps --quiet selector) || {
    rollback_selector || fail 'cannot inspect the new selector or restore the previous image'
    fail 'cannot inspect the new selector; the previous image was restored'
  }
  if [[ -z $selector_id ]] || ! check_container "$selector_id" selector; then
    rollback_selector || fail 'new selector validation failed and the previous image could not be restored'
    fail 'new selector validation failed; the previous image was restored'
  fi
  if ! selector_image=$(docker inspect --format '{{.Image}}' "$selector_id"); then
    rollback_selector || fail 'cannot inspect the new selector or restore the previous image'
    fail 'cannot inspect the new selector; the previous image was restored'
  fi
  if ! deployed_revision=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$selector_image"); then
    rollback_selector || fail 'cannot inspect new selector provenance or restore the previous image'
    fail 'cannot inspect new selector provenance; the previous image was restored'
  fi
  if [[ $deployed_revision != "$SHA" ]]; then
    rollback_selector || fail 'new selector provenance failed and the previous image could not be restored'
    fail 'new selector provenance does not match the tested commit; the previous image was restored'
  fi

  if ! check_production_routes; then
    rollback_selector || fail 'post-deploy checks failed and the previous selector image could not be restored'
    fail 'post-deploy checks failed; the previous selector image was restored'
  fi
else
  check_production_routes || fail 'post-deploy checks failed for the already deployed selector'
fi

printf 'link-edge selector verified at %s; retired path=404; discovery routes=valid\n' "$SHA"
