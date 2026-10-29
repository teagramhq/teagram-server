#!/usr/bin/env bash

DEMO_SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
DEMO_REPO_ROOT=$(cd -P "$DEMO_SCRIPT_DIR/../.." && pwd)
DEMO_ENV_FILE="$DEMO_SCRIPT_DIR/.env"
# These variables are shared with scripts that source this file.
# shellcheck disable=SC2034
DEMO_STATE_DIR="$DEMO_REPO_ROOT/.state/teagram-demo"
# shellcheck disable=SC2034
DEMO_CLIENT_CONFIG_DIR="$DEMO_STATE_DIR/public-client"
# shellcheck disable=SC2034
DEMO_CLIENT_CONFIG="$DEMO_CLIENT_CONFIG_DIR/client"
# shellcheck disable=SC2034
DEMO_AUTHORITY_DIR="$DEMO_REPO_ROOT/.state/blob-mode"
# shellcheck disable=SC2034
DEMO_SEED_SECRET_DIR="/var/lib/teagram-demo/secrets"
# shellcheck disable=SC2034
DEMO_SEED_PASSWORD_FILE="$DEMO_SEED_SECRET_DIR/seed-owner-password"
# shellcheck disable=SC2034
DEMO_SEED_ACCOUNT_DIR="$DEMO_SEED_SECRET_DIR/demo-accounts"
# shellcheck disable=SC2034
DEMO_SEED_ACCOUNT_PASSWORD_FILE="$DEMO_SEED_ACCOUNT_DIR/inventory.json"
# shellcheck disable=SC2034
DEMO_SEED_OWNER_MARKER="$DEMO_SEED_SECRET_DIR/seed-owner-created"
# shellcheck disable=SC2034
DEMO_SEED_OWNER_USERNAME="demo_seed_owner"
# shellcheck disable=SC2034
DEMO_IMAGE_REPOSITORY="ghcr.io/teagramhq/teagram-server-demo"
DEMO_ACTIVE_RELEASE_FILE="$DEMO_STATE_DIR/active-release.json"
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

[[ -f "$DEMO_SCRIPT_DIR/haproxy.cfg" && -f "$DEMO_SCRIPT_DIR/render-haproxy-config.py" &&
  -f "$DEMO_SCRIPT_DIR/nginx.conf" ]] || {
  demo_fail 'proxy configuration source is missing'
  return 1
}
TEAGRAM_DEMO_HAPROXY_CONFIG_SHA256=$(
  sha256sum -- "$DEMO_SCRIPT_DIR/haproxy.cfg" "$DEMO_SCRIPT_DIR/render-haproxy-config.py" |
    awk '{print $1}' | sha256sum | awk '{print $1}'
)
TEAGRAM_DEMO_EDGE_CONFIG_SHA256=$(sha256sum -- "$DEMO_SCRIPT_DIR/nginx.conf" | awk '{print $1}')
export TEAGRAM_DEMO_HAPROXY_CONFIG_SHA256 TEAGRAM_DEMO_EDGE_CONFIG_SHA256

demo_require_proxy_configuration_current() {
  local service="$1" expected_source_hash="$2" source_label target_hash live_hash live_source_hash
  local -a proxy_ids=()

  case "$service" in
    haproxy) source_label='com.teagram.demo.haproxy-config-sha256' ;;
    edge) source_label='com.teagram.demo.edge-config-sha256' ;;
    *) demo_fail "unsupported proxy service: $service"; return 1 ;;
  esac

  [[ "$expected_source_hash" =~ ^[0-9a-f]{64}$ ]] || {
    demo_fail "$service source configuration hash is invalid"
    return 1
  }
  mapfile -t proxy_ids < <(demo_compose ps -q "$service")
  ((${#proxy_ids[@]} == 1)) || {
    demo_fail "running $service proxy is missing or ambiguous"
    return 1
  }
  target_hash=$(demo_compose config --hash "$service" | awk -v service="$service" '$1 == service {print $2}') || {
    demo_fail "$service target Compose hash could not be read"
    return 1
  }
  live_hash=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.config-hash"}}' "${proxy_ids[0]}") || {
    demo_fail "$service live Compose hash could not be read"
    return 1
  }
  live_source_hash=$(docker inspect --format "{{index .Config.Labels \"$source_label\"}}" "${proxy_ids[0]}") || {
    demo_fail "$service live source configuration hash could not be read"
    return 1
  }
  [[ "$target_hash" =~ ^[0-9a-f]{64}$ ]] || {
    demo_fail "$service target Compose hash is invalid"
    return 1
  }
  [[ "$live_hash" == "$target_hash" && "$live_source_hash" == "$expected_source_hash" ]] ||
    {
      demo_fail "$service proxy image or configuration changed; apply proxy changes during maintenance before deploying"
      return 1
    }
}

demo_wait_migration() {
  local compose_command="$1" description="$2" exit_code
  local -a migration_ids=()
  mapfile -t migration_ids < <("$compose_command" ps -a -q migrate)
  if ((${#migration_ids[@]} != 1)); then
    demo_fail "$description container is missing or ambiguous"
    return 1
  fi
  if ! exit_code=$(docker wait "${migration_ids[0]}"); then
    demo_fail "$description container wait failed"
    return 1
  fi
  if [[ ! "$exit_code" =~ ^[0-9]+$ ]]; then
    demo_fail "$description returned an invalid exit code"
    return 1
  fi
  if [[ "$exit_code" != 0 ]]; then
    demo_fail "$description failed (exit code $exit_code)"
    return 1
  fi
}

demo_require_root() {
  [[ "$EUID" -eq 0 ]] || demo_fail 'must run as root on the demo LXC'
}

demo_prepare_turnstile_secret_access() {
  local secret_file="${1:-$DEMO_SEED_SECRET_DIR/turnstile-secret-key}"
  local secret_dir state
  secret_dir=$(dirname -- "$secret_file")

  [[ "$EUID" -eq 0 ]] || {
    demo_fail 'must run as root to prepare the Turnstile secret mount'
    return 1
  }
  [[ -d "$secret_dir" && ! -L "$secret_dir" &&
    "$(stat -c '%u:%g:%a' "$secret_dir")" == '0:0:700' ]] || {
    demo_fail 'Turnstile secret directory must be root-owned mode 0700'
    return 1
  }
  [[ -f "$secret_file" && ! -L "$secret_file" && "$(stat -c '%u' "$secret_file")" == 0 ]] || {
    demo_fail 'Turnstile secret must be a regular root-owned file'
    return 1
  }
  state=$(stat -c '%u:%g:%a' "$secret_file") || {
    demo_fail 'Turnstile secret metadata could not be read'
    return 1
  }
  case "$state" in
    0:0:600)
      chgrp 65532 -- "$secret_file" || {
        demo_fail 'Turnstile secret runtime group could not be set'
        return 1
      }
      ;;
    0:65532:600|0:65532:440) ;;
    *)
      demo_fail 'Turnstile secret must be root-owned and inaccessible outside runtime gid 65532'
      return 1
      ;;
  esac
  chmod 0440 -- "$secret_file" || {
    demo_fail 'Turnstile secret runtime read mode could not be set'
    return 1
  }
}

demo_require_env() {
  [[ -f "$DEMO_ENV_FILE" && ! -L "$DEMO_ENV_FILE" ]] || {
    demo_fail 'deploy/teagram-demo/.env is missing or not a regular file'
    return 1
  }
  demo_prepare_turnstile_secret_access || return 1
  python3 "$DEMO_SCRIPT_DIR/validate-env.py" "$DEMO_ENV_FILE" || return 1
}

demo_valid_image_ref() {
  local image_ref="$1" digest
  if [[ "$image_ref" == teagramd:local ]]; then
    return 0
  fi
  [[ "$image_ref" == "$DEMO_IMAGE_REPOSITORY@"* ]] || return 1
  digest=${image_ref#"$DEMO_IMAGE_REPOSITORY@"}
  [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]
}

demo_active_server() {
  local active_file="$DEMO_STATE_DIR/active-server" release_file="$DEMO_ACTIVE_RELEASE_FILE" active
  if [[ -e "$release_file" || -L "$release_file" ]]; then
    if [[ ! -f "$release_file" || -L "$release_file" || "$(stat -c '%u:%a' "$release_file")" != '0:600' ]]; then
      demo_fail 'active release file must be a root-owned mode 0600 file'
      return 1
    fi
    if ! active=$(jq -er '.server | select(type == "string")' "$release_file"); then
      demo_fail 'active release file is invalid'
      return 1
    fi
  elif [[ ! -e "$active_file" && ! -L "$active_file" ]]; then
    active=teagramd
  else
    if [[ ! -f "$active_file" || -L "$active_file" || "$(stat -c '%u:%a' "$active_file")" != '0:600' ]]; then
      demo_fail 'active server marker must be a root-owned mode 0600 file'
      return 1
    fi
    active=$(<"$active_file")
  fi
  if [[ "$active" != teagramd && "$active" != teagramd-next ]]; then
    demo_fail 'active server selection is invalid'
    return 1
  fi
  printf '%s\n' "$active"
}

demo_active_image_ref() {
  local active_file="$DEMO_ACTIVE_RELEASE_FILE" image_ref
  if [[ -e "$active_file" || -L "$active_file" ]]; then
    if [[ ! -f "$active_file" || -L "$active_file" || "$(stat -c '%u:%a' "$active_file")" != '0:600' ]]; then
      demo_fail 'active release file must be a root-owned mode 0600 file'
      return 1
    fi
    if ! image_ref=$(jq -er '.image_ref | select(type == "string")' "$active_file"); then
      demo_fail 'active release image is invalid'
      return 1
    fi
  else
    image_ref=teagramd:local
  fi
  if ! demo_valid_image_ref "$image_ref"; then
    demo_fail 'active release image reference is invalid'
    return 1
  fi
  printf '%s\n' "$image_ref"
}

demo_compose_image_ref() {
  local image_ref="${TEAGRAM_DEMO_IMAGE_REF:-}"
  if [[ -z "$image_ref" ]]; then
    if ! image_ref=$(demo_active_image_ref); then
      return 1
    fi
  fi
  if ! demo_valid_image_ref "$image_ref"; then
    demo_fail 'Compose image reference is invalid'
    return 1
  fi
  printf '%s\n' "$image_ref"
}

demo_inactive_server() {
  local active
  if ! active=$(demo_active_server); then
    return 1
  fi
  if [[ "$active" == teagramd ]]; then
    printf 'teagramd-next\n'
  else
    printf 'teagramd\n'
  fi
}

demo_write_active_release() {
  local active="$1" image_ref="$2" temporary
  if [[ "$active" != teagramd && "$active" != teagramd-next ]]; then
    demo_fail 'invalid active server selection'
    return 1
  fi
  if ! demo_valid_image_ref "$image_ref"; then
    demo_fail 'invalid active release image reference'
    return 1
  fi
  if [[ -L "$DEMO_STATE_DIR" ]]; then
    demo_fail 'demo state directory must not be a symlink'
    return 1
  fi
  install -d -o root -g root -m 0755 "$DEMO_STATE_DIR"
  temporary=$(mktemp "$DEMO_STATE_DIR/active-release.XXXXXXXX")
  jq -n --arg server "$active" --arg image_ref "$image_ref" \
    '{server: $server, image_ref: $image_ref}' > "$temporary"
  chown root:root "$temporary"
  chmod 0600 "$temporary"
  mv -f -- "$temporary" "$DEMO_ACTIVE_RELEASE_FILE"
}

demo_pull_image_by_sha() {
  local sha="$1" digest="${2:-}" revision image_ref actual_image_ref
  if [[ ! "$sha" =~ ^[0-9a-f]{40}$ ]]; then
    demo_fail 'image source SHA must be a full 40-hex commit'
    return 1
  fi
  if [[ ! "$digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    demo_fail 'trusted CI image digest must be a sha256 digest'
    return 1
  fi
  image_ref="$DEMO_IMAGE_REPOSITORY@$digest"
  if ! docker pull "$image_ref" >&2; then
    demo_fail "could not pull demo image for $sha"
    return 1
  fi
  if ! revision=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$image_ref"); then
    demo_fail 'pulled demo image revision label is missing'
    return 1
  fi
  if [[ "$revision" != "$sha" ]]; then
    demo_fail 'pulled demo image does not identify the requested source SHA'
    return 1
  fi
  if ! actual_image_ref=$(docker image inspect --format '{{json .RepoDigests}}' "$image_ref" |
    jq -er --arg expected "$image_ref" --arg repository "$DEMO_IMAGE_REPOSITORY" \
      '[.[] | select(startswith($repository + "@"))] | unique | if length == 1 and .[0] == $expected then .[0] else error("missing, mismatched or ambiguous repository digest") end'); then
    demo_fail 'pulled demo image digest does not match the trusted CI digest'
    return 1
  fi
  if ! demo_valid_image_ref "$actual_image_ref"; then
    demo_fail 'pulled demo image digest is invalid'
    return 1
  fi
  printf '%s\n' "$actual_image_ref"
}

demo_prepare_client_config_dir() {
  [[ ! -L "$DEMO_STATE_DIR" ]] || demo_fail 'demo state directory must not be a symlink'
  [[ ! -L "$DEMO_CLIENT_CONFIG_DIR" ]] || demo_fail 'public client config directory must not be a symlink'
  install -d -o root -g root -m 0755 "$DEMO_CLIENT_CONFIG_DIR"
}

demo_prepare_haproxy_runtime() {
  local active
  active=$(demo_active_server)
  [[ ! -L "$DEMO_STATE_DIR" ]] || demo_fail 'demo state directory must not be a symlink'
  [[ ! -L "$DEMO_STATE_DIR/haproxy-config" ]] || demo_fail 'HAProxy config directory must not be a symlink'
  [[ ! -L "$DEMO_STATE_DIR/haproxy-run" ]] || demo_fail 'HAProxy runtime directory must not be a symlink'
  install -d -o root -g root -m 0755 "$DEMO_STATE_DIR/haproxy-config"
  install -d -o 99 -g 99 -m 0700 "$DEMO_STATE_DIR/haproxy-run"
  python3 "$DEMO_SCRIPT_DIR/render-haproxy-config.py" "$active" \
    "$DEMO_SCRIPT_DIR/haproxy.cfg" "$DEMO_STATE_DIR/haproxy-config/haproxy.cfg"
}

demo_haproxy_control() {
  python3 "$DEMO_SCRIPT_DIR/haproxy-control.py" \
    --socket "$DEMO_STATE_DIR/haproxy-run/admin.sock" "$@"
}

demo_compose() {
  local image_ref
  if ! image_ref=$(demo_compose_image_ref); then
    return 1
  fi
  env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
    -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE \
    -u TEAGRAM_DEMO_EDGE_BIND \
    "TEAGRAM_DEMO_IMAGE_REF=$image_ref" \
    docker compose "${DEMO_COMPOSE_ARGS[@]}" "$@" </dev/null
}

demo_validation_compose() {
  local image_ref
  if ! image_ref=$(demo_compose_image_ref); then
    return 1
  fi
  env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
    -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE \
    "TEAGRAM_DEMO_IMAGE_REF=$image_ref" \
    TEAGRAM_DEMO_EDGE_BIND=127.0.0.1:18080 \
    docker compose "${DEMO_COMPOSE_ARGS[@]}" "$@" </dev/null
}

demo_compose_input() {
  local image_ref
  if ! image_ref=$(demo_compose_image_ref); then
    return 1
  fi
  env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
    -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE \
    -u TEAGRAM_DEMO_EDGE_BIND \
    "TEAGRAM_DEMO_IMAGE_REF=$image_ref" \
    docker compose "${DEMO_COMPOSE_ARGS[@]}" "$@"
}

demo_bootstrap_compose() {
  local image_ref
  if ! image_ref=$(demo_compose_image_ref); then
    return 1
  fi
  env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
    -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE \
    -u TEAGRAM_DEMO_EDGE_BIND \
    "TEAGRAM_DEMO_IMAGE_REF=$image_ref" \
    docker compose "${DEMO_COMPOSE_ARGS[@]}" \
      --file "$DEMO_SCRIPT_DIR/compose.bootstrap.yaml" \
      --file "$DEMO_SCRIPT_DIR/compose.seed.yaml" "$@" </dev/null
}

demo_seed_compose() {
  local image_ref
  if ! image_ref=$(demo_compose_image_ref); then
    return 1
  fi
  env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
    -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE \
    -u TEAGRAM_DEMO_EDGE_BIND \
    "TEAGRAM_DEMO_IMAGE_REF=$image_ref" \
    docker compose "${DEMO_COMPOSE_ARGS[@]}" \
      --file "$DEMO_SCRIPT_DIR/compose.seed.yaml" "$@" </dev/null
}

demo_seed_compose_input() {
  local image_ref
  if ! image_ref=$(demo_compose_image_ref); then
    return 1
  fi
  env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
    -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE \
    -u TEAGRAM_DEMO_EDGE_BIND \
    "TEAGRAM_DEMO_IMAGE_REF=$image_ref" \
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
  demo_prepare_client_config_dir
  demo_prepare_haproxy_runtime
  demo_compose config --format json > "$destination"
  chmod 0600 "$destination"
  jq -e \
    --arg haproxyConfigHash "$TEAGRAM_DEMO_HAPROXY_CONFIG_SHA256" \
    --arg edgeConfigHash "$TEAGRAM_DEMO_EDGE_CONFIG_SHA256" '
    (.services.teagramd.ports // [] | length) == 0
    and (.services["teagramd-next"].ports // [] | length) == 0
    and (.services.teagramd.build == null)
    and (.services["teagramd-next"].build == null)
    and .services.teagramd.image == .services["teagramd-next"].image
    and (.services.edge.ports | length) == 1
    and .services.edge.ports[0].host_ip == "127.0.0.1"
    and ((.services.edge.ports[0].target | tostring) == "8080")
    and ((.services | to_entries | map(.value.ports // []) | add | map(.target | tostring) | index("2443")) == null)
    and .services.teagramd.environment.TG_REGISTRATION == "open"
    and .services.teagramd.environment.TG_ADVERTISE_DEMO_GUEST_SIGN_UP == "true"
    and .services["teagramd-next"].environment.TG_ADVERTISE_DEMO_GUEST_SIGN_UP == "true"
    and .services.teagramd.environment.TG_LOG_LOGIN_CODES == "false"
    and .services.teagramd.environment.TG_LOG_AUTH_KEY_UNBINDS == "true"
    and .services["teagramd-next"].environment.TG_LOG_AUTH_KEY_UNBINDS == "true"
    and .services.teagramd.environment.TG_MAX_FILE_BYTES == "16777216"
    and .services.teagramd.environment.TG_MAX_USER_STORAGE_BYTES == "268435456"
    and .services.teagramd.environment.TG_WEBSOCKET_LISTEN_ADDR == ":2446"
    and .services.teagramd.environment.GOMAXPROCS == "1"
    and .services.teagramd.environment.GOMEMLIMIT == "320MiB"
    and (.services.teagramd.mem_limit | tostring) == "469762048"
    and (.services["teagramd-next"].mem_limit | tostring) == "469762048"
    and (.services.postgres.mem_limit | tostring) == "335544320"
    and .services.teagramd.environment.TG_CLIENT_ADDR_TRUST == "proxy-v2"
    and .services.teagramd.environment.TG_CLIENT_ADDR_PROXY_CIDRS == "198.18.130.2/32"
    and .services.teagramd.environment.HTTP_PROXY == "http://turnstile-egress:3128"
    and .services.teagramd.environment.HTTPS_PROXY == "http://turnstile-egress:3128"
    and .services.teagramd.environment.NO_PROXY == "localhost,127.0.0.1,postgres,teagramd,teagramd-next"
    and .services.teagramd.environment.TG_TURNSTILE_SITE_KEY != ""
    and .services.teagramd.environment.TG_TURNSTILE_SECRET_KEY_FILE == "/run/secrets/teagram_demo_turnstile"
    and (.services.teagramd.networks | keys) == ["backend"]
    and .services.teagramd.networks.backend.ipv4_address == "198.18.130.253"
    and .services["teagramd-next"].networks.backend.ipv4_address == "198.18.130.251"
    and .networks.backend.internal == true
    and .networks.backend.ipam.config[0].subnet == "198.18.130.0/24"
    and .services["turnstile-egress"].networks.backend.ipv4_address == "198.18.130.252"
    and .services["turnstile-egress"].networks.egress.gw_priority == 1
    and (.services["turnstile-egress"].ports // [] | length) == 0
    and .services.teagramd.depends_on["turnstile-egress"].condition == "service_healthy"
    and .services.haproxy.networks.backend.ipv4_address == "198.18.130.2"
    and (.services.haproxy.ports // [] | length) == 0
    and .services.haproxy.image == "mirror.gcr.io/library/haproxy:3.2-alpine@sha256:51f173c6825753d9e3a888b1004a1c2cb730936cc9178730a5c555246bd161a2"
    and .services.haproxy.labels["com.teagram.demo.haproxy-config-sha256"] == $haproxyConfigHash
    and .services.edge.depends_on.haproxy.condition == "service_healthy"
    and .services.edge.labels["com.teagram.demo.edge-config-sha256"] == $edgeConfigHash
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
  local work_dir="$1" temporary marker_temporary active_server
  active_server=$(demo_active_server)

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

  if ! demo_seed_compose_input exec -T --user 0:65532 "$active_server" \
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
