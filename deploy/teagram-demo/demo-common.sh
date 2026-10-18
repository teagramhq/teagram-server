#!/usr/bin/env bash

DEMO_SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
DEMO_REPO_ROOT=$(cd -P "$DEMO_SCRIPT_DIR/../.." && pwd)
DEMO_ENV_FILE="$DEMO_SCRIPT_DIR/.env"
DEMO_STATE_DIR="$DEMO_REPO_ROOT/.state/teagram-demo"
DEMO_AUTHORITY_DIR="$DEMO_REPO_ROOT/.state/blob-mode"
DEMO_COMPOSE_ARGS=(
  --env-file "$DEMO_ENV_FILE"
  --project-directory "$DEMO_REPO_ROOT"
  --project-name teagram-demo
  --file "$DEMO_SCRIPT_DIR/compose.yaml"
)

demo_fail() {
  printf 'teagram-demo: %s\n' "$1" >&2
  return 1
}

demo_require_root() {
  [[ "$EUID" -eq 0 ]] || demo_fail 'must run as root on the demo LXC'
}

demo_require_env() {
  [[ -f "$DEMO_ENV_FILE" && ! -L "$DEMO_ENV_FILE" ]] || demo_fail 'deploy/teagram-demo/.env is missing or not a regular file'
  python3 "$DEMO_SCRIPT_DIR/validate-env.py" "$DEMO_ENV_FILE"
}

demo_compose() {
  env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
    docker compose "${DEMO_COMPOSE_ARGS[@]}" "$@" </dev/null
}

demo_compose_input() {
  env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
    docker compose "${DEMO_COMPOSE_ARGS[@]}" "$@"
}

demo_bootstrap_compose() {
  env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
    docker compose "${DEMO_COMPOSE_ARGS[@]}" \
      --file "$DEMO_SCRIPT_DIR/compose.bootstrap.yaml" "$@" </dev/null
}

demo_check_blob_mount() {
  local rendered_json="$1" blob_path
  blob_path=$(jq -er '.volumes.tgblobs.driver_opts.device' "$rendered_json") ||
    demo_fail 'resolved tgblobs volume has no device path'
  python3 "$DEMO_SCRIPT_DIR/check-blob-mount.py" "$blob_path"
}

demo_render_config() {
  local destination="$1"
  demo_compose config --format json > "$destination"
  chmod 0600 "$destination"
  jq -e '
    (.services.teagramd.ports // [] | length) == 0
    and (.services.edge.ports | length) == 1
    and .services.edge.ports[0].host_ip == "127.0.0.1"
    and ((.services.edge.ports[0].target | tostring) == "8080")
    and ((.services | to_entries | map(.value.ports // []) | add | map(.target | tostring) | index("2443")) == null)
    and .services.teagramd.environment.TG_REGISTRATION == "open"
    and .services.teagramd.environment.TG_LOG_LOGIN_CODES == "false"
    and .services.teagramd.environment.TG_MAX_FILE_BYTES == "16777216"
    and .services.teagramd.environment.TG_MAX_USER_STORAGE_BYTES == "268435456"
    and .services.teagramd.environment.TG_WEBSOCKET_LISTEN_ADDR == ":2446"
    and (.services.teagramd.networks | keys) == ["backend"]
    and .networks.backend.internal == true
    and (.services.edge.networks | keys | sort) == ["backend", "edge"]
    and .volumes.tgblobs.driver == "local"
    and .volumes.tgblobs.driver_opts.type == "none"
    and .volumes.tgblobs.driver_opts.o == "bind"
    and (.volumes.tgblobs.driver_opts.device | startswith("/"))
  ' "$destination" >/dev/null || demo_fail 'resolved Compose render violates the demo exposure or storage contract'
}
