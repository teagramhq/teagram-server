#!/usr/bin/env bash

DEMO_SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
DEMO_REPO_ROOT=$(cd -P "$DEMO_SCRIPT_DIR/../.." && pwd)
DEMO_ENV_FILE="$DEMO_SCRIPT_DIR/.env"
# These variables are shared with scripts that source this file.
# shellcheck disable=SC2034
DEMO_STATE_DIR="$DEMO_REPO_ROOT/.state/teagram-demo"
# shellcheck disable=SC2034
DEMO_AUTHORITY_DIR="$DEMO_REPO_ROOT/.state/blob-mode"
# shellcheck disable=SC2034
DEMO_SEED_SECRET_DIR="/var/lib/teagram-demo/secrets"
# shellcheck disable=SC2034
DEMO_SEED_PASSWORD_FILE="$DEMO_SEED_SECRET_DIR/seed-owner-password"
# shellcheck disable=SC2034
DEMO_SEED_OWNER_MARKER="$DEMO_SEED_SECRET_DIR/seed-owner-created"
# shellcheck disable=SC2034
DEMO_SEED_OWNER_USERNAME="demo_seed_owner"
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
    -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE \
    docker compose "${DEMO_COMPOSE_ARGS[@]}" "$@" </dev/null
}

demo_compose_input() {
  env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
    -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE \
    docker compose "${DEMO_COMPOSE_ARGS[@]}" "$@"
}

demo_bootstrap_compose() {
  env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
    -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE \
    docker compose "${DEMO_COMPOSE_ARGS[@]}" \
      --file "$DEMO_SCRIPT_DIR/compose.bootstrap.yaml" \
      --file "$DEMO_SCRIPT_DIR/compose.seed.yaml" "$@" </dev/null
}

demo_seed_compose() {
  env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
    -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE \
    docker compose "${DEMO_COMPOSE_ARGS[@]}" \
      --file "$DEMO_SCRIPT_DIR/compose.seed.yaml" "$@" </dev/null
}

demo_seed_compose_input() {
  env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
    -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE \
    docker compose "${DEMO_COMPOSE_ARGS[@]}" \
      --file "$DEMO_SCRIPT_DIR/compose.seed.yaml" "$@"
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
    and .services.teagramd.environment.TG_CLIENT_ADDR_TRUST == "proxy-v2"
    and .services.teagramd.environment.TG_CLIENT_ADDR_PROXY_CIDRS == "198.18.130.2/32"
    and .services.teagramd.environment.TG_TURNSTILE_SITE_KEY != ""
    and .services.teagramd.environment.TG_TURNSTILE_SECRET_KEY_FILE == "/run/secrets/teagram_demo_turnstile"
    and (.services.teagramd.networks | keys) == ["backend"]
    and .networks.backend.internal == true
    and .networks.backend.ipam.config[0].subnet == "198.18.130.0/24"
    and .services.haproxy.networks.backend.ipv4_address == "198.18.130.2"
    and (.services.haproxy.ports // [] | length) == 0
    and .services.haproxy.image == "mirror.gcr.io/library/haproxy:3.2-alpine@sha256:51f173c6825753d9e3a888b1004a1c2cb730936cc9178730a5c555246bd161a2"
    and .services.edge.depends_on.haproxy.condition == "service_healthy"
    and (.services.edge.networks | keys | sort) == ["backend", "edge"]
    and .secrets.teagram_demo_turnstile.file == "/var/lib/teagram-demo/secrets/turnstile-secret-key"
    and .volumes.tgblobs.driver == "local"
    and .volumes.tgblobs.driver_opts.type == "none"
    and .volumes.tgblobs.driver_opts.o == "bind"
    and (.volumes.tgblobs.driver_opts.device | startswith("/"))
  ' "$destination" >/dev/null || demo_fail 'resolved Compose render violates the demo exposure or storage contract'
}

demo_reset_seed_owner_marker() {
  if [[ -e "$DEMO_SEED_SECRET_DIR" || -L "$DEMO_SEED_SECRET_DIR" ]]; then
    [[ -d "$DEMO_SEED_SECRET_DIR" && ! -L "$DEMO_SEED_SECRET_DIR" &&
      "$(stat -c '%u:%a' "$DEMO_SEED_SECRET_DIR")" == '0:700' ]] ||
      demo_fail 'seed secret directory has unsafe type or permissions'
  fi
  [[ ! -L "$DEMO_SEED_OWNER_MARKER" ]] || demo_fail 'seed-owner marker must not be a symlink'
  rm -f -- "$DEMO_SEED_OWNER_MARKER"
}

demo_prepare_seed_owner() {
  local work_dir="$1" temporary marker_temporary

  [[ ! -L "$DEMO_SEED_SECRET_DIR" ]] || demo_fail 'seed secret directory must not be a symlink'
  install -d -o root -g root -m 0700 "$DEMO_SEED_SECRET_DIR"
  [[ "$(stat -c '%u:%a' "$DEMO_SEED_SECRET_DIR")" == '0:700' ]] ||
    demo_fail 'seed secret directory must be root-owned mode 0700'

  if [[ -e "$DEMO_SEED_PASSWORD_FILE" || -L "$DEMO_SEED_PASSWORD_FILE" ]]; then
    [[ -f "$DEMO_SEED_PASSWORD_FILE" && ! -L "$DEMO_SEED_PASSWORD_FILE" &&
      "$(stat -c '%u:%a' "$DEMO_SEED_PASSWORD_FILE")" == '0:600' ]] ||
      demo_fail 'seed-owner password file has unsafe type or permissions'
  fi

  if [[ -e "$DEMO_SEED_OWNER_MARKER" || -L "$DEMO_SEED_OWNER_MARKER" ]]; then
    [[ -f "$DEMO_SEED_OWNER_MARKER" && ! -L "$DEMO_SEED_OWNER_MARKER" &&
      "$(stat -c '%u:%a' "$DEMO_SEED_OWNER_MARKER")" == '0:600' ]] ||
      demo_fail 'seed-owner marker has unsafe type or permissions'
    [[ -f "$DEMO_SEED_PASSWORD_FILE" && ! -L "$DEMO_SEED_PASSWORD_FILE" &&
      "$(stat -c '%u:%a' "$DEMO_SEED_PASSWORD_FILE")" == '0:600' ]] ||
      demo_fail 'seed-owner password file has unsafe type or permissions'
    grep -Eq '^[0-9a-f]{64}$' "$DEMO_SEED_PASSWORD_FILE" ||
      demo_fail 'seed-owner password file has an invalid value'
    return 0
  fi

  temporary=$(mktemp "$DEMO_SEED_SECRET_DIR/seed-owner-password.XXXXXXXX")
  if ! {
    chmod 0600 "$temporary" &&
      chown root:root "$temporary" &&
      openssl rand -hex 32 > "$temporary" &&
      mv -f -- "$temporary" "$DEMO_SEED_PASSWORD_FILE"
  }; then
    rm -f -- "$temporary"
    demo_fail 'could not safely create the seed-owner password file'
  fi
  chmod 0600 "$DEMO_SEED_PASSWORD_FILE"
  chown root:root "$DEMO_SEED_PASSWORD_FILE"
  grep -Eq '^[0-9a-f]{64}$' "$DEMO_SEED_PASSWORD_FILE" ||
    demo_fail 'generated seed-owner password is invalid'

  if ! demo_seed_compose_input exec -T --user 0:65532 teagramd \
    /usr/local/bin/teagramd admin create-user --username "$DEMO_SEED_OWNER_USERNAME" \
    < "$DEMO_SEED_PASSWORD_FILE" \
    > "$work_dir/seed-owner-create.stdout" \
    2> "$work_dir/seed-owner-create.stderr"; then
    demo_fail 'could not create the non-admin seed owner and close admin election'
  fi
  [[ ! -s "$work_dir/seed-owner-create.stdout" ]] ||
    demo_fail 'seed-owner creation wrote unexpected stdout'
  grep -Eq "^User created: ${DEMO_SEED_OWNER_USERNAME} \\(user id: [0-9]+\\)$" \
    "$work_dir/seed-owner-create.stderr" ||
    demo_fail 'seed-owner creation confirmation was missing'

  marker_temporary=$(mktemp "$DEMO_SEED_SECRET_DIR/seed-owner-created.XXXXXXXX")
  chmod 0600 "$marker_temporary"
  chown root:root "$marker_temporary"
  printf 'created\n' > "$marker_temporary"
  mv -f -- "$marker_temporary" "$DEMO_SEED_OWNER_MARKER"
}
