#!/usr/bin/env bash

DEMO_SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
DEMO_REPO_ROOT=$(cd -P "$DEMO_SCRIPT_DIR/../.." && pwd)
DEMO_DEPLOY_TARGET=${DEMO_DEPLOY_TARGET:-${TEAGRAM_DEMO_VARIANT:-charlie}}
if [[ "$DEMO_DEPLOY_TARGET" == lima ]]; then
  DEMO_ENV_FILE=${TEAGRAM_DEMO_LIMA_ENV_FILE:-/var/lib/teagram-demo/lima.env}
  DEMO_COMPOSE_FILE="$DEMO_SCRIPT_DIR/lima/compose.yaml"
  DEMO_COMPOSE_ARGS=(
    --env-file "$DEMO_ENV_FILE"
    --project-directory "$DEMO_REPO_ROOT"
    --project-name teagram-demo
    --file "$DEMO_COMPOSE_FILE"
  )
else
  DEMO_ENV_FILE="$DEMO_SCRIPT_DIR/.env"
  DEMO_COMPOSE_FILE="$DEMO_REPO_ROOT/docker-compose.yml"
  DEMO_COMPOSE_ARGS=(
    --env-file "$DEMO_ENV_FILE"
    --project-directory "$DEMO_REPO_ROOT"
    --project-name teagram-demo
    --file "$DEMO_COMPOSE_FILE"
    --profile demo
  )
fi
# These variables are shared with scripts that source this file.
# shellcheck disable=SC2034
DEMO_STATE_DIR="$DEMO_REPO_ROOT/.state/teagram-demo"
# shellcheck disable=SC2034
DEMO_CLIENT_CONFIG_DIR="$DEMO_STATE_DIR/public-client"
# shellcheck disable=SC2034
DEMO_WEB_ROOT="$DEMO_CLIENT_CONFIG_DIR/web"
# shellcheck disable=SC2034
DEMO_WEB_RELEASES_DIR="$DEMO_WEB_ROOT/releases"
# shellcheck disable=SC2034
DEMO_WEB_CURRENT_LINK="$DEMO_WEB_ROOT/current"
# shellcheck disable=SC2034
DEMO_CLIENT_CONFIG="$DEMO_CLIENT_CONFIG_DIR/client"
# shellcheck disable=SC2034
DEMO_TAILNET_CLIENT_CONFIG="$DEMO_CLIENT_CONFIG_DIR/client-tailnet"
# shellcheck disable=SC2034
DEMO_AUTHORITY_DIR="$DEMO_REPO_ROOT/.state/blob-mode"
# shellcheck disable=SC2034
DEMO_SEED_SECRET_DIR="/var/lib/teagram-demo/secrets"
# shellcheck disable=SC2034
DEMO_TURNSTILE_QA_BYPASS_FILE="$DEMO_SEED_SECRET_DIR/turnstile-qa-bypass-token"
# shellcheck disable=SC2034
DEMO_SEED_PASSWORD_FILE="$DEMO_SEED_SECRET_DIR/seed-owner-password"
# shellcheck disable=SC2034
DEMO_SEED_OWNER_SESSION_FILE="$DEMO_SEED_SECRET_DIR/seed-owner.session"
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
DEMO_TAILNET_ENABLED_FILE="$DEMO_STATE_DIR/tailnet-enabled"
DEMO_TAILNET_COMPOSE_FILE="$DEMO_SCRIPT_DIR/teagram-dev-tailnet.compose.yaml"
DEMO_TAILNET_ORIGIN="https://teagram-dev.tailaa4918.ts.net"
demo_fail() {
  printf 'teagram-demo: %s\n' "$1" >&2
  return 1
}

demo_tailnet_enabled() {
  local enabled
  if [[ "${TEAGRAM_DEV_TAILNET_ENABLED+x}" == x ]]; then
    enabled=$TEAGRAM_DEV_TAILNET_ENABLED
  elif [[ -e "$DEMO_TAILNET_ENABLED_FILE" || -L "$DEMO_TAILNET_ENABLED_FILE" ]]; then
    [[ -f "$DEMO_TAILNET_ENABLED_FILE" && ! -L "$DEMO_TAILNET_ENABLED_FILE" &&
      "$(stat -c '%a' "$DEMO_TAILNET_ENABLED_FILE")" == 600 ]] || {
      demo_fail 'persisted teagram-dev tailnet setting is missing, unsafe, or has invalid permissions'
      return 1
    }
    enabled=$(<"$DEMO_TAILNET_ENABLED_FILE")
  else
    enabled=false
  fi
  case "$enabled" in
    ''|false)
      printf '%s\n' false
      ;;
    true)
      [[ "$DEMO_DEPLOY_TARGET" != lima ]] || {
        demo_fail 'teagram-dev tailnet listener is not available in the lima variant'
        return 1
      }
      printf '%s\n' true
      ;;
    *)
      demo_fail 'teagram-dev tailnet setting must be true or false'
      return 1
      ;;
  esac
}

demo_persist_tailnet_enabled() {
  local enabled="${1:-}" temporary
  case "$enabled" in
    true|false) ;;
    *)
      demo_fail 'tailnet setting to persist must be true or false'
      return 1
      ;;
  esac
  [[ ! -L "$DEMO_STATE_DIR" && ! -L "$DEMO_TAILNET_ENABLED_FILE" ]] || {
    demo_fail 'tailnet setting state path must not be a symlink'
    return 1
  }
  mkdir -p "$DEMO_STATE_DIR" || {
    demo_fail 'tailnet setting state directory could not be created'
    return 1
  }
  if ! temporary=$(mktemp "$DEMO_STATE_DIR/.tailnet-enabled.XXXXXXXX"); then
    demo_fail 'tailnet setting could not be staged'
    return 1
  fi
  if ! printf '%s\n' "$enabled" > "$temporary"; then
    rm -f -- "$temporary"
    demo_fail 'tailnet setting could not be written'
    return 1
  fi
  if ! chmod 0600 "$temporary"; then
    rm -f -- "$temporary"
    demo_fail 'tailnet setting permissions could not be set'
    return 1
  fi
  if ! mv -f -- "$temporary" "$DEMO_TAILNET_ENABLED_FILE"; then
    rm -f -- "$temporary"
    demo_fail 'tailnet setting could not be installed'
    return 1
  fi
}

demo_valid_signup_username() {
  [[ "${1:-}" =~ ^[A-Za-z][A-Za-z0-9_]{1,31}$ ]]
}

demo_turnstile_signin_enabled() {
  local -a values=()
  mapfile -t values < <(sed -n 's/^TG_TURNSTILE_SIGNIN_ENABLED=//p' "$DEMO_ENV_FILE")
  if ((${#values[@]} == 0)); then
    printf '%s\n' false
    return 0
  fi
  if ((${#values[@]} != 1)) || [[ "${values[0]}" != true && "${values[0]}" != false ]]; then
    demo_fail 'sign-in Turnstile setting is missing or invalid'
    return 1
  fi
  printf '%s\n' "${values[0]}"
}

demo_prepare_runtime_tmp_dirs() {
  local runtime_tmp_dir="$DEMO_STATE_DIR/tmp"
  local turnstile_tmp_dir="$runtime_tmp_dir/turnstile-egress"
  local haproxy_tmp_dir="$runtime_tmp_dir/haproxy"
  local edge_tmp_dir="$runtime_tmp_dir/edge"
  local edge_tailnet_tmp_dir="$runtime_tmp_dir/edge-tailnet"

  [[ ! -L "$DEMO_REPO_ROOT/.state" && ! -L "$DEMO_STATE_DIR" &&
    ! -L "$runtime_tmp_dir" && ! -L "$turnstile_tmp_dir" &&
    ! -L "$haproxy_tmp_dir" && ! -L "$edge_tmp_dir" &&
    ! -L "$edge_tailnet_tmp_dir" ]] || {
    demo_fail 'demo /tmp state path must not contain symlinks'
    return 1
  }
  install -d -o root -g root -m 0700 "$runtime_tmp_dir" || {
    demo_fail 'demo /tmp state directory could not be created'
    return 1
  }
  install -d -o 584792 -g 584792 -m 1777 "$turnstile_tmp_dir" || {
    demo_fail 'Turnstile egress /tmp directory could not be created'
    return 1
  }
  install -d -o root -g root -m 1777 "$haproxy_tmp_dir" "$edge_tmp_dir" "$edge_tailnet_tmp_dir" || {
    demo_fail 'proxy /tmp directories could not be created'
    return 1
  }
  [[ "$(stat -c '%u:%g:%a' "$turnstile_tmp_dir")" == '584792:584792:1777' &&
    "$(stat -c '%u:%g:%a' "$haproxy_tmp_dir")" == '0:0:1777' &&
    "$(stat -c '%u:%g:%a' "$edge_tmp_dir")" == '0:0:1777' &&
    "$(stat -c '%u:%g:%a' "$edge_tailnet_tmp_dir")" == '0:0:1777' ]] || {
    demo_fail 'demo /tmp directory ownership or mode could not be verified'
    return 1
  }
}

demo_prepare_runtime_tmp_dirs_for_up() {
  [[ "${1:-}" == up ]] || return 0
  demo_prepare_runtime_tmp_dirs
}

demo_server_client_config() {
  local service="$1" compose_fn=demo_compose server_ids_output
  local -a server_ids=()
  case "${2:-demo}" in
    demo) compose_fn=demo_compose ;;
    seed) compose_fn=demo_seed_compose ;;
    *) demo_fail 'client-config Compose context must be demo or seed'; return 1 ;;
  esac
  server_ids_output=$("$compose_fn" ps --status running -q "$service") || return 1
  if [[ -n "$server_ids_output" ]]; then
    mapfile -t server_ids <<< "$server_ids_output"
  fi
  ((${#server_ids[@]} == 1)) || {
    demo_fail "client-config requires exactly one running $service container"
    return 1
  }
  "$compose_fn" exec -T "$service" /usr/local/bin/teagramd client-config
}

[[ -f "$DEMO_SCRIPT_DIR/haproxy.cfg" && -f "$DEMO_SCRIPT_DIR/render-haproxy-config.py" &&
  -f "$DEMO_SCRIPT_DIR/nginx-config/nginx.conf" &&
  -f "$DEMO_SCRIPT_DIR/nginx-config/common-locations.conf" ]] || {
  demo_fail 'proxy configuration source is missing'
  return 1
}

# shellcheck disable=SC2120 # Tests pass alternate paths; normal callers use defaults.
demo_prepare_edge_config_permissions() {
  local config_dir="${1:-$DEMO_SCRIPT_DIR/nginx-config}"
  local config_file="${2:-$config_dir/nginx.conf}"
  local locations_file="$config_dir/common-locations.conf"
  [[ -d "$config_dir" && ! -L "$config_dir" ]] || {
    demo_fail 'edge configuration directory is missing or unsafe'
    return 1
  }
  [[ -f "$config_file" && ! -L "$config_file" ]] || {
    demo_fail 'edge configuration file is missing or unsafe'
    return 1
  }
  [[ -f "$locations_file" && ! -L "$locations_file" ]] || {
    demo_fail 'shared edge locations file is missing or unsafe'
    return 1
  }
  chmod 0755 -- "$config_dir" || {
    demo_fail 'edge configuration directory permissions could not be prepared'
    return 1
  }
  [[ "$(stat -c '%a' "$config_dir")" == 755 ]] || {
    demo_fail 'edge configuration directory permissions could not be verified'
    return 1
  }
  chmod 0644 -- "$config_file" || {
    demo_fail 'edge configuration file permissions could not be prepared'
    return 1
  }
  chmod 0644 -- "$locations_file" || {
    demo_fail 'shared edge locations permissions could not be prepared'
    return 1
  }
  [[ "$(stat -c '%a' "$config_file")" == 644 ]] || {
    demo_fail 'edge configuration file permissions could not be verified'
    return 1
  }
  [[ "$(stat -c '%a' "$locations_file")" == 644 ]] || {
    demo_fail 'shared edge locations permissions could not be verified'
    return 1
  }
}

demo_prepare_tailnet_edge_config_permissions() {
  local config_file="$DEMO_SCRIPT_DIR/nginx-tailnet.conf"
  demo_prepare_edge_config_permissions || return 1
  [[ -f "$config_file" && ! -L "$config_file" ]] || {
    demo_fail 'tailnet edge configuration file is missing or unsafe'
    return 1
  }
  chmod 0644 -- "$config_file" || {
    demo_fail 'tailnet edge configuration permissions could not be prepared'
    return 1
  }
  [[ "$(stat -c '%a' "$config_file")" == 644 ]] || {
    demo_fail 'tailnet edge configuration permissions could not be verified'
    return 1
  }
}

TEAGRAM_DEMO_HAPROXY_CONFIG_SHA256=$(
  sha256sum -- "$DEMO_SCRIPT_DIR/haproxy.cfg" "$DEMO_SCRIPT_DIR/render-haproxy-config.py" |
    awk '{print $1}' | sha256sum | awk '{print $1}'
)
export TEAGRAM_DEMO_HAPROXY_CONFIG_SHA256

demo_migrate_proxy_tmpfs() {
  local previous_sha="$1" active_server="$2" service target_hash legacy_hash live_hash
  local mounts tmpfs previous_file edge_source_hash live_source_hash legacy_edge_bind_hash=
  local -a proxy_ids=() migrate_services=()

  [[ "$previous_sha" =~ ^[0-9a-f]{40}$ && -d "$TMP_DIR" ]] || {
    demo_fail 'proxy /tmp migration requires a previous revision and temporary directory'
    return 1
  }
  DEMO_PROXY_TMPFS_LEGACY_COMPOSE="$TMP_DIR/proxy-tmpfs-legacy.json"
  if ! demo_compose config --format json | jq -e --arg tmp_root "$DEMO_STATE_DIR/tmp" '
    . as $config
    | if all(["turnstile-egress", "haproxy", "edge"][]; . as $service |
        [$config.services[$service].volumes[]? | select(.target == "/tmp")] as $mounts
        | ($mounts | length == 1)
          and $mounts[0].type == "bind"
          and $mounts[0].source == ($tmp_root + "/" + $service)
          and ($mounts[0].read_only // false) == false
          and $mounts[0].bind.create_host_path == false
          and ([$config.services[$service].tmpfs[]? | select(startswith("/tmp"))] | length == 0)
      ) then
        .services["turnstile-egress"].volumes |= map(select(.target != "/tmp"))
        | .services["turnstile-egress"].tmpfs |=
            (.[0:1] + ["/tmp:rw,noexec,nosuid,nodev,size=16m,uid=584792,gid=584792,mode=1777"] + .[1:])
        | .services.haproxy.volumes |= map(select(.target != "/tmp"))
        | .services.haproxy.tmpfs = ["/tmp:size=16m,mode=1777"]
        | .services.edge.volumes |= map(select(.target != "/tmp"))
        | .services.edge.tmpfs = ["/tmp:size=16m,mode=1777"]
      else error("unsupported desired proxy /tmp mounts") end
  ' > "$DEMO_PROXY_TMPFS_LEGACY_COMPOSE"; then
    demo_fail 'previous proxy /tmp configuration could not be reconstructed'
    return 1
  fi
  chmod 0600 "$DEMO_PROXY_TMPFS_LEGACY_COMPOSE" || return 1

  mapfile -t proxy_ids < <(demo_compose ps -q edge)
  ((${#proxy_ids[@]} == 1)) || { demo_fail 'running edge proxy is missing or ambiguous'; return 1; }
  if demo_edge_uses_legacy_file_mount "${proxy_ids[0]}"; then
    previous_file="$TMP_DIR/previous-edge-nginx.conf"
    git -C "$DEMO_REPO_ROOT" show "$previous_sha:deploy/teagram-demo/nginx.conf" > "$previous_file" || {
      demo_fail 'previous legacy edge configuration could not be read'
      return 1
    }
    edge_source_hash=$(sha256sum "$previous_file" | awk '{print $1}') || return 1
    live_source_hash=$(docker inspect --format '{{index .Config.Labels "com.teagram.demo.edge-config-sha256"}}' "${proxy_ids[0]}") || return 1
    [[ "$live_source_hash" == "$edge_source_hash" ]] || {
      demo_fail 'legacy edge source configuration differs from the previous release'
      return 1
    }
    legacy_edge_bind_hash=$(demo_edge_legacy_compose_hash "$edge_source_hash") || return 1
    jq -e --arg config_dir "$DEMO_SCRIPT_DIR/nginx-config" --arg legacy_file "$DEMO_SCRIPT_DIR/nginx.conf" \
      --arg source_hash "$edge_source_hash" '
      if .services.edge.entrypoint == ["nginx", "-c", "/etc/nginx/demo/nginx.conf", "-g", "daemon off;"]
        and ([.services.edge.volumes[] | select(.source == $config_dir and .target == "/etc/nginx/demo" and .read_only)] | length == 1)
      then
        .services.edge.entrypoint = ["nginx", "-g", "daemon off;"]
        | .services.edge.labels = ((.services.edge.labels // {}) + {"com.teagram.demo.edge-config-sha256": $source_hash})
        | .services.edge.volumes |= map(if .source == $config_dir and .target == "/etc/nginx/demo"
            then .source = $legacy_file | .target = "/etc/nginx/nginx.conf" else . end)
      else error("unsupported legacy edge configuration") end
    ' "$DEMO_PROXY_TMPFS_LEGACY_COMPOSE" > "$TMP_DIR/proxy-tmpfs-legacy-edge.json" || return 1
    mv -f -- "$TMP_DIR/proxy-tmpfs-legacy-edge.json" "$DEMO_PROXY_TMPFS_LEGACY_COMPOSE" || return 1
  elif ! demo_edge_uses_directory_mount "${proxy_ids[0]}"; then
    demo_fail 'edge proxy config mount is unsupported during /tmp migration'
    return 1
  fi

  for service in turnstile-egress haproxy edge; do
    mapfile -t proxy_ids < <(demo_compose ps -q "$service")
    ((${#proxy_ids[@]} == 1)) || { demo_fail "running $service proxy is missing or ambiguous"; return 1; }
    target_hash=$(demo_compose config --hash "$service" | awk -v s="$service" '$1 == s {print $2}') || return 1
    legacy_hash=$(demo_compose_with_file "$DEMO_PROXY_TMPFS_LEGACY_COMPOSE" config --hash "$service" |
      awk -v s="$service" '$1 == s {print $2}') || return 1
    live_hash=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.config-hash"}}' "${proxy_ids[0]}") || return 1
    [[ "$target_hash" =~ ^[0-9a-f]{64}$ && "$legacy_hash" =~ ^[0-9a-f]{64}$ &&
      "$target_hash" != "$legacy_hash" ]] || { demo_fail "$service proxy hashes are invalid"; return 1; }
    if [[ "$service" == haproxy ]]; then
      live_source_hash=$(docker inspect --format '{{index .Config.Labels "com.teagram.demo.haproxy-config-sha256"}}' "${proxy_ids[0]}") || return 1
      [[ "$live_source_hash" == "$TEAGRAM_DEMO_HAPROXY_CONFIG_SHA256" ]] || {
        demo_fail 'haproxy source configuration changed before /tmp migration'
        return 1
      }
    fi
    tmpfs=$(docker inspect --format '{{json .HostConfig.Tmpfs}}' "${proxy_ids[0]}") || return 1
    mounts=$(docker inspect --format '{{json .Mounts}}' "${proxy_ids[0]}") || return 1
    if [[ "$live_hash" == "$legacy_hash" ]]; then
      if ! jq -e '(. // {}) | has("/tmp")' <<< "$tmpfs" >/dev/null ||
        ! jq -e 'all(.[]; .Destination != "/tmp" or .Type == "tmpfs")' <<< "$mounts" >/dev/null; then
        demo_fail "$service does not have the expected previous /tmp tmpfs"
        return 1
      fi
      migrate_services+=("$service")
    elif [[ "$live_hash" == "$target_hash" || ( "$service" == edge && -n "$legacy_edge_bind_hash" && "$live_hash" == "$legacy_edge_bind_hash" ) ]]; then
      if ! jq -e '(. // {}) | has("/tmp") | not' <<< "$tmpfs" >/dev/null ||
        ! jq -e --arg source "$DEMO_STATE_DIR/tmp/$service" \
          '[.[] | select(.Destination == "/tmp")] | length == 1 and .[0].Type == "bind" and .[0].Source == $source and .[0].RW == true' <<< "$mounts" >/dev/null; then
        demo_fail "$service does not have the expected /tmp bind mount"
        return 1
      fi
    else
      demo_fail "$service has unrelated configuration drift; refusing /tmp migration"
      return 1
    fi
  done

  ((${#migrate_services[@]} > 0)) || return 0
  for service in "${migrate_services[@]}"; do
    if [[ "$service" == turnstile-egress ]]; then
      for previous_file in turnstile-egress.conf pebble-health.yaml; do
        if ! git -C "$DEMO_REPO_ROOT" show "$previous_sha:deploy/teagram-demo/$previous_file" > "$TMP_DIR/previous-$previous_file" ||
          ! cmp -s -- "$TMP_DIR/previous-$previous_file" "$DEMO_SCRIPT_DIR/$previous_file"; then
          demo_fail 'turnstile egress configuration changed before /tmp migration'
          return 1
        fi
      done
    fi
  done
  demo_prepare_runtime_tmp_dirs || return 1
  if [[ " ${migrate_services[*]} " == *' haproxy '* ]]; then
    demo_haproxy_control wait-healthy "$active_server" --timeout 60 || return 1
    demo_prepare_haproxy_runtime || return 1
    mapfile -t proxy_ids < <(demo_compose ps -q haproxy)
    ((${#proxy_ids[@]} == 1)) || return 1
    docker exec "${proxy_ids[0]}" haproxy -c -f /usr/local/etc/haproxy/haproxy.cfg >/dev/null || {
      demo_fail 'HAProxy configuration failed its pre-migration syntax check'
      return 1
    }
  fi
  if [[ " ${migrate_services[*]} " == *' edge '* ]]; then
    demo_compose run --rm -T --no-deps --entrypoint nginx edge -c /etc/nginx/demo/nginx.conf -t >/dev/null || {
      demo_fail 'edge configuration failed its pre-migration syntax check'
      return 1
    }
  fi
  for service in "${migrate_services[@]}"; do
    if [[ "$service" == edge ]]; then
      # The deploy cleanup uses this when a recreated edge must be rolled back.
      # shellcheck disable=SC2034
      EDGE_CONFIG_RELOAD_ATTEMPTED=1
    fi
    DEMO_PROXY_TMPFS_ATTEMPTED+=("$service")
    demo_compose up -d --no-deps --force-recreate --wait --wait-timeout 120 "$service" || {
      demo_fail "$service could not be recreated with its /tmp bind mount"
      return 1
    }
    mapfile -t proxy_ids < <(demo_compose ps -q "$service")
    ((${#proxy_ids[@]} == 1)) || { demo_fail "recreated $service proxy is missing or ambiguous"; return 1; }
    target_hash=$(demo_compose config --hash "$service" | awk -v s="$service" '$1 == s {print $2}') || return 1
    live_hash=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.config-hash"}}' "${proxy_ids[0]}") || return 1
    tmpfs=$(docker inspect --format '{{json .HostConfig.Tmpfs}}' "${proxy_ids[0]}") || return 1
    mounts=$(docker inspect --format '{{json .Mounts}}' "${proxy_ids[0]}") || return 1
    if [[ ! "$target_hash" =~ ^[0-9a-f]{64}$ || "$live_hash" != "$target_hash" ]] ||
      ! jq -e '(. // {}) | has("/tmp") | not' <<< "$tmpfs" >/dev/null ||
      ! jq -e --arg source "$DEMO_STATE_DIR/tmp/$service" \
        '[.[] | select(.Destination == "/tmp")] | length == 1 and .[0].Type == "bind" and .[0].Source == $source and .[0].RW == true' <<< "$mounts" >/dev/null; then
      demo_fail "recreated $service proxy does not have the expected /tmp bind mount"
      return 1
    fi
    if [[ "$service" == haproxy ]]; then
      demo_haproxy_control wait-healthy "$active_server" --timeout 60 || return 1
    elif [[ "$service" == edge ]]; then
      docker exec "${proxy_ids[0]}" nginx -c /etc/nginx/demo/nginx.conf -t >/dev/null || return 1
    fi
  done
}

demo_restore_proxy_tmpfs() {
  local service
  for service in "${DEMO_PROXY_TMPFS_ATTEMPTED[@]}"; do
    demo_compose_with_file "$DEMO_PROXY_TMPFS_LEGACY_COMPOSE" \
      up -d --no-deps --force-recreate --wait --wait-timeout 120 "$service" || {
      demo_fail "$service could not be restored to its previous /tmp configuration"
      return 1
    }
  done
  DEMO_PROXY_TMPFS_ATTEMPTED=()
}

demo_require_proxy_configuration_current() {
  local service="$1" expected_source_hash="${2:-}" allow_legacy_edge_migration="${3:-0}"
  local legacy_source_revision="${4:-}" source_label target_hash live_hash live_source_hash legacy_source_hash expected_legacy_hash
  local -a proxy_ids=()

  case "$service" in
    haproxy) source_label='com.teagram.demo.haproxy-config-sha256' ;;
    edge) source_label='' ;;
    *) demo_fail "unsupported proxy service: $service"; return 1 ;;
  esac

  if [[ "$service" == haproxy && ! "$expected_source_hash" =~ ^[0-9a-f]{64}$ ]]; then
    demo_fail 'haproxy source configuration hash is invalid'
    return 1
  fi
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
  [[ "$target_hash" =~ ^[0-9a-f]{64}$ ]] || {
    demo_fail "$service target Compose hash is invalid"
    return 1
  }
  if [[ "$service" == edge ]]; then
    if [[ "$live_hash" == "$target_hash" ]] && demo_edge_uses_directory_mount "${proxy_ids[0]}"; then
      return 0
    fi
    if [[ "$allow_legacy_edge_migration" == 1 ]] &&
      demo_edge_uses_legacy_file_mount "${proxy_ids[0]}"; then
      [[ "$legacy_source_revision" =~ ^[0-9a-f]{40}$ ]] || {
        demo_fail 'legacy edge source revision is invalid'
        return 1
      }
      if ! legacy_source_hash=$(git -C "$DEMO_REPO_ROOT" show \
        "$legacy_source_revision:deploy/teagram-demo/nginx.conf" | sha256sum | awk '{print $1}'); then
        demo_fail 'legacy edge source configuration hash could not be derived'
        return 1
      fi
      [[ "$legacy_source_hash" =~ ^[0-9a-f]{64}$ ]] || {
        demo_fail 'legacy edge source configuration hash is invalid'
        return 1
      }
      live_source_hash=$(docker inspect --format \
        '{{index .Config.Labels "com.teagram.demo.edge-config-sha256"}}' "${proxy_ids[0]}") || {
        demo_fail 'legacy edge source configuration hash could not be read'
        return 1
      }
      [[ "$live_source_hash" == "$legacy_source_hash" ]] || {
        demo_fail 'legacy edge source configuration differs from the previous release'
        return 1
      }
      expected_legacy_hash=$(demo_edge_legacy_compose_hash "$legacy_source_hash") || return 1
      [[ "$live_hash" == "$expected_legacy_hash" ]] && return 0
      demo_fail 'legacy edge proxy has unrelated image, Compose settings, or config drift; refusing the one-time mount migration'
      return 1
    fi
    demo_fail 'edge proxy image, Compose settings, or config mount changed; only the single-file to directory mount migration is automatic'
    return 1
  fi

  live_source_hash=$(docker inspect --format "{{index .Config.Labels \"$source_label\"}}" "${proxy_ids[0]}") || {
    demo_fail "$service live source configuration hash could not be read"
    return 1
  }
  [[ "$live_hash" == "$target_hash" && "$live_source_hash" == "$expected_source_hash" ]] && return 0
  demo_fail "$service proxy image or configuration changed; apply proxy changes during maintenance before deploying"
  return 1
}

demo_edge_uses_directory_mount() {
  local edge_id="$1" mounts
  mounts=$(docker inspect --format '{{range .Mounts}}{{printf "%s|%s|%s\n" .Type .Source .Destination}}{{end}}' "$edge_id") || return 1
  grep -Fqx -- "bind|$DEMO_SCRIPT_DIR/nginx-config|/etc/nginx/demo" <<< "$mounts"
}

demo_edge_uses_legacy_file_mount() {
  local edge_id="$1" mounts
  mounts=$(docker inspect --format '{{range .Mounts}}{{printf "%s|%s|%s\n" .Type .Source .Destination}}{{end}}' "$edge_id") || return 1
  grep -Fqx -- "bind|$DEMO_SCRIPT_DIR/nginx.conf|/etc/nginx/nginx.conf" <<< "$mounts"
}

demo_edge_legacy_compose_hash() {
  local legacy_source_hash="$1" normalized_file hash_output expected_hash
  [[ "$legacy_source_hash" =~ ^[0-9a-f]{64}$ ]] || {
    demo_fail 'legacy edge source configuration hash is invalid'
    return 1
  }
  [[ -n "${TMP_DIR:-}" && -d "$TMP_DIR" ]] || {
    demo_fail 'temporary directory is unavailable for legacy edge validation'
    return 1
  }
  normalized_file=$(mktemp "$TMP_DIR/edge-legacy-compose.XXXXXXXX.json") || {
    demo_fail 'legacy edge Compose config could not be created'
    return 1
  }
  if ! demo_compose config --format json | jq -e \
    --arg config_dir "$DEMO_SCRIPT_DIR/nginx-config" \
    --arg legacy_file "$DEMO_SCRIPT_DIR/nginx.conf" \
    --arg legacy_source_hash "$legacy_source_hash" '
      if
        .services.edge.entrypoint == ["nginx", "-c", "/etc/nginx/demo/nginx.conf", "-g", "daemon off;"]
        and (((.services.edge.labels // {}) | has("com.teagram.demo.edge-config-sha256")) | not)
        and ([((.services.edge.volumes // [])[]) | select(
          .type == "bind" and .source == $config_dir and .target == "/etc/nginx/demo" and .read_only == true
        )] | length == 1)
        and ([((.services.edge.volumes // [])[]) | select(.target == "/etc/nginx/nginx.conf")] | length == 0)
        and ($legacy_source_hash | test("^[0-9a-f]{64}$"))
      then
        .services.edge.entrypoint = ["nginx", "-g", "daemon off;"]
        | .services.edge.labels = ((.services.edge.labels // {}) + {
            "com.teagram.demo.edge-config-sha256": $legacy_source_hash
          })
        | .services.edge.volumes |= map(
            if .type == "bind" and .source == $config_dir and .target == "/etc/nginx/demo" then
              .source = $legacy_file | .target = "/etc/nginx/nginx.conf"
            else . end
          )
      else
        error("current edge Compose config does not match the supported mount migration")
      end
    ' > "$normalized_file"; then
    rm -f -- "$normalized_file"
    demo_fail 'current edge Compose config could not be normalized for the legacy mount migration'
    return 1
  fi
  if ! hash_output=$(demo_compose_with_file "$normalized_file" config --hash edge); then
    rm -f -- "$normalized_file"
    demo_fail 'normalized legacy edge Compose hash could not be read'
    return 1
  fi
  rm -f -- "$normalized_file"
  expected_hash=$(awk '$1 == "edge" {print $2}' <<< "$hash_output")
  [[ "$expected_hash" =~ ^[0-9a-f]{64}$ ]] || {
    demo_fail 'normalized legacy edge Compose hash is invalid'
    return 1
  }
  printf '%s\n' "$expected_hash"
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

demo_prepare_runtime_secret_access() {
  local secret_file="${1:-$DEMO_SEED_SECRET_DIR/turnstile-secret-key}"
  local secret_dir state
  secret_dir=$(dirname -- "$secret_file")

  [[ "$EUID" -eq 0 ]] || {
    demo_fail 'must run as root to prepare a runtime secret mount'
    return 1
  }
  [[ -d "$secret_dir" && ! -L "$secret_dir" &&
    "$(stat -c '%u:%g:%a' "$secret_dir")" == '0:0:700' ]] || {
    demo_fail 'runtime secret directory must be root-owned mode 0700'
    return 1
  }
  [[ -f "$secret_file" && ! -L "$secret_file" && "$(stat -c '%u' "$secret_file")" == 0 ]] || {
    demo_fail 'runtime secret must be a regular root-owned file'
    return 1
  }
  state=$(stat -c '%u:%g:%a' "$secret_file") || {
    demo_fail 'runtime secret metadata could not be read'
    return 1
  }
  case "$state" in
    0:0:600)
      chgrp 65532 -- "$secret_file" || {
        demo_fail 'runtime secret group could not be set'
        return 1
      }
      ;;
    0:65532:600|0:65532:440) ;;
    *)
      demo_fail 'runtime secret must be root-owned and inaccessible outside runtime gid 65532'
      return 1
      ;;
  esac
  chmod 0440 -- "$secret_file" || {
    demo_fail 'runtime secret read mode could not be set'
    return 1
  }
}

demo_ensure_qa_bypass_token() {
  local temporary
  [[ "$EUID" -eq 0 ]] || {
    demo_fail 'must run as root to provision the demo QA bypass token'
    return 1
  }
  [[ -d "$DEMO_SEED_SECRET_DIR" && ! -L "$DEMO_SEED_SECRET_DIR" &&
    "$(stat -c '%u:%g:%a' "$DEMO_SEED_SECRET_DIR")" == '0:0:700' ]] || {
    demo_fail 'demo secret directory must be root-owned mode 0700 before QA token provisioning'
    return 1
  }
  if [[ -e "$DEMO_TURNSTILE_QA_BYPASS_FILE" || -L "$DEMO_TURNSTILE_QA_BYPASS_FILE" ]]; then
    demo_prepare_runtime_secret_access "$DEMO_TURNSTILE_QA_BYPASS_FILE"
    return $?
  fi

  temporary=$(mktemp "$DEMO_SEED_SECRET_DIR/.turnstile-qa-bypass-token.XXXXXXXX") || {
    demo_fail 'could not create a temporary demo QA bypass token file'
    return 1
  }
  if ! openssl rand -hex 32 > "$temporary"; then
    rm -f -- "$temporary"
    demo_fail 'could not generate the demo QA bypass token'
    return 1
  fi
  if ! chown root:65532 "$temporary" || ! chmod 0440 "$temporary"; then
    rm -f -- "$temporary"
    demo_fail 'could not secure the demo QA bypass token file'
    return 1
  fi
  if ! ln -- "$temporary" "$DEMO_TURNSTILE_QA_BYPASS_FILE"; then
    rm -f -- "$temporary"
    if [[ -e "$DEMO_TURNSTILE_QA_BYPASS_FILE" || -L "$DEMO_TURNSTILE_QA_BYPASS_FILE" ]]; then
      demo_prepare_runtime_secret_access "$DEMO_TURNSTILE_QA_BYPASS_FILE"
      return $?
    fi
    demo_fail 'could not install the demo QA bypass token'
    return 1
  fi
  rm -f -- "$temporary"
}

demo_require_env() {
  if [[ "$DEMO_DEPLOY_TARGET" == lima ]]; then
    [[ -f "$DEMO_ENV_FILE" && ! -L "$DEMO_ENV_FILE" ]] || {
      demo_fail 'lima environment file is missing or not a regular file'
      return 1
    }
    python3 "$DEMO_SCRIPT_DIR/validate-env.py" --lima "$DEMO_ENV_FILE"
    return $?
  fi
  [[ -f "$DEMO_ENV_FILE" && ! -L "$DEMO_ENV_FILE" ]] || {
    demo_fail 'deploy/teagram-demo/.env is missing or not a regular file'
    return 1
  }
  python3 "$DEMO_SCRIPT_DIR/update-websocket-origins.py" "$DEMO_ENV_FILE" || return 1
  demo_prepare_runtime_secret_access || return 1
  demo_ensure_qa_bypass_token || return 1
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

demo_resolve_seed_endpoint() {
  local mode="$1" explicit="$2" endpoint="$3"
  if [[ "$mode" == --moderate-delete-message || "$mode" == --moderate-ban-user ||
    "$mode" == --verify-general-moderation-smoke ]]; then
    if [[ "$explicit" == 1 && "$endpoint" != haproxy:2443 ]]; then
      demo_fail 'moderation commands must use the internal haproxy:2443 listener'
      return 1
    fi
    printf '%s\n' haproxy:2443
    return 0
  fi
  if [[ "$mode" == --cleanup-general-moderation-smoke ]]; then
    if [[ "$explicit" != 1 ]]; then
      printf '%s\n' haproxy:2443
      return 0
    fi
    case "$endpoint" in
      haproxy:2443|teagramd:2443|teagramd-next:2443)
        printf '%s\n' "$endpoint"
        return 0
        ;;
      *)
        demo_fail 'General moderation smoke cleanup requires an internal endpoint'
        return 1
        ;;
    esac
  fi
  if [[ "$mode" == --prepare-general-moderation-smoke && "$endpoint" == haproxy:2443 ]]; then
    demo_fail 'General moderation smoke preparation must use a private teagramd endpoint'
    return 1
  fi
  printf '%s\n' "$endpoint"
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
  local active="$1" image_ref="$2" web_sha="${3:-}" web_digest="${4:-}" temporary
  if [[ "$active" != teagramd && "$active" != teagramd-next ]]; then
    demo_fail 'invalid active server selection'
    return 1
  fi
  if ! demo_valid_image_ref "$image_ref"; then
    demo_fail 'invalid active release image reference'
    return 1
  fi
  if [[ -n "$web_sha" || -n "$web_digest" ]]; then
    [[ "$web_sha" =~ ^[0-9a-f]{40}$ && "$web_digest" =~ ^sha256:[0-9a-f]{64}$ ]] || {
      demo_fail 'invalid active web artifact identity'
      return 1
    }
  fi
  if [[ -L "$DEMO_STATE_DIR" ]]; then
    demo_fail 'demo state directory must not be a symlink'
    return 1
  fi
  install -d -o root -g root -m 0755 "$DEMO_STATE_DIR"
  temporary=$(mktemp "$DEMO_STATE_DIR/active-release.XXXXXXXX")
  jq -n --arg server "$active" --arg image_ref "$image_ref" \
    --arg web_sha "$web_sha" --arg web_digest "$web_digest" '
    {server: $server, image_ref: $image_ref}
    + if $web_sha != "" and $web_digest != "" then
        {web: {source_commit: $web_sha, artifact_digest: $web_digest}}
      else {} end
  ' > "$temporary"
  chown root:root "$temporary"
  chmod 0600 "$temporary"
  mv -f -- "$temporary" "$DEMO_ACTIVE_RELEASE_FILE"
}

demo_active_web_release() {
  local web_sha web_digest
  if [[ ! -e "$DEMO_ACTIVE_RELEASE_FILE" && ! -L "$DEMO_ACTIVE_RELEASE_FILE" ]]; then
    printf '\n'
    return 0
  fi
  if [[ ! -f "$DEMO_ACTIVE_RELEASE_FILE" || -L "$DEMO_ACTIVE_RELEASE_FILE" ||
    "$(stat -c '%u:%a' "$DEMO_ACTIVE_RELEASE_FILE")" != '0:600' ]]; then
    demo_fail 'active release file must be a root-owned mode 0600 file'
    return 1
  fi
  if ! web_sha=$(jq -er '.web.source_commit // ""' "$DEMO_ACTIVE_RELEASE_FILE") ||
    ! web_digest=$(jq -er '.web.artifact_digest // ""' "$DEMO_ACTIVE_RELEASE_FILE"); then
    demo_fail 'active web artifact identity is invalid'
    return 1
  fi
  if [[ -z "$web_sha" && -z "$web_digest" ]]; then
    printf '\n'
    return 0
  fi
  [[ "$web_sha" =~ ^[0-9a-f]{40}$ && "$web_digest" =~ ^sha256:[0-9a-f]{64}$ ]] || {
    demo_fail 'active web artifact identity is invalid'
    return 1
  }
  printf '%s %s\n' "$web_sha" "${web_digest#sha256:}"
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
  local config_file="$DEMO_CLIENT_CONFIG_DIR/teagram-config.json" temporary
  local expected_config='{"server":"demo.teagram.io","autoGuest":true}'
  [[ ! -L "$DEMO_STATE_DIR" ]] || demo_fail 'demo state directory must not be a symlink'
  [[ ! -L "$DEMO_CLIENT_CONFIG_DIR" ]] || demo_fail 'public client config directory must not be a symlink'
  [[ ! -L "$DEMO_WEB_ROOT" ]] || demo_fail 'public web root must not be a symlink'
  [[ ! -L "$DEMO_WEB_RELEASES_DIR" ]] || demo_fail 'public web release directory must not be a symlink'
  install -d -o root -g root -m 0755 "$DEMO_CLIENT_CONFIG_DIR"
  install -d -o root -g root -m 0755 "$DEMO_WEB_RELEASES_DIR"
  if [[ -e "$config_file" || -L "$config_file" ]]; then
    [[ -f "$config_file" && ! -L "$config_file" &&
      "$(stat -c '%u:%a' "$config_file")" == '0:644' &&
      "$(stat -c '%s' "$config_file")" == "${#expected_config}" &&
      "$(<"$config_file")" == "$expected_config" ]] || {
      demo_fail 'demo web config file is invalid'
      return 1
    }
    return 0
  fi
  temporary=$(mktemp "$DEMO_CLIENT_CONFIG_DIR/.teagram-config.XXXXXXXX")
  printf '%s' "$expected_config" > "$temporary"
  chown root:root "$temporary"
  chmod 0644 "$temporary"
  mv -f -- "$temporary" "$config_file"
}

demo_prepare_tailnet_client_config() {
  local temporary
  [[ -f "$DEMO_CLIENT_CONFIG" && ! -L "$DEMO_CLIENT_CONFIG" ]] || {
    demo_fail 'public client config is missing or unsafe for tailnet discovery'
    return 1
  }
  [[ ! -L "$DEMO_TAILNET_CLIENT_CONFIG" ]] || {
    demo_fail 'tailnet client config must not be a symlink'
    return 1
  }
  jq -e '
    type == "object"
    and (.websocket | type == "object")
    and .websocket.endpoint == "wss://demo.teagram.io/apiws"
  ' "$DEMO_CLIENT_CONFIG" >/dev/null || {
    demo_fail 'public client config has an invalid WebSocket endpoint for tailnet discovery'
    return 1
  }
  if ! temporary=$(mktemp "$DEMO_CLIENT_CONFIG_DIR/.client-tailnet.XXXXXXXX"); then
    demo_fail 'could not create a temporary tailnet client config file'
    return 1
  fi
  if ! jq '.websocket.endpoint = "wss://teagram-dev.tailaa4918.ts.net/apiws"' \
    "$DEMO_CLIENT_CONFIG" > "$temporary"; then
    rm -f -- "$temporary"
    demo_fail 'tailnet client config could not be generated'
    return 1
  fi
  if ! chmod 0644 "$temporary"; then
    rm -f -- "$temporary"
    demo_fail 'tailnet client config permissions could not be set'
    return 1
  fi
  if ! mv -f -- "$temporary" "$DEMO_TAILNET_CLIENT_CONFIG"; then
    rm -f -- "$temporary"
    demo_fail 'tailnet client config could not be installed'
    return 1
  fi
  demo_prepare_tailnet_bootstrap_config
}

demo_prepare_tailnet_bootstrap_config() {
  local public_config="$DEMO_CLIENT_CONFIG_DIR/teagram-config.json"
  local tailnet_config="$DEMO_CLIENT_CONFIG_DIR/teagram-config-tailnet.json" temporary
  [[ -f "$public_config" && ! -L "$public_config" ]] || {
    demo_fail 'public Web bootstrap config is missing or unsafe for tailnet setup'
    return 1
  }
  [[ ! -L "$tailnet_config" ]] || {
    demo_fail 'tailnet Web bootstrap config must not be a symlink'
    return 1
  }
  jq -e 'type == "object" and .server == "demo.teagram.io" and .autoGuest == true' \
    "$public_config" >/dev/null || {
    demo_fail 'public Web bootstrap config is invalid for tailnet setup'
    return 1
  }
  if ! temporary=$(mktemp "$DEMO_CLIENT_CONFIG_DIR/.teagram-config-tailnet.XXXXXXXX"); then
    demo_fail 'tailnet Web bootstrap config could not be staged'
    return 1
  fi
  if ! jq '.server = "teagram-dev.tailaa4918.ts.net"' "$public_config" > "$temporary"; then
    rm -f -- "$temporary"
    demo_fail 'tailnet Web bootstrap config could not be generated'
    return 1
  fi
  if ! chmod 0644 "$temporary"; then
    rm -f -- "$temporary"
    demo_fail 'tailnet Web bootstrap config permissions could not be set'
    return 1
  fi
  if ! mv -f -- "$temporary" "$tailnet_config"; then
    rm -f -- "$temporary"
    demo_fail 'tailnet Web bootstrap config could not be installed'
    return 1
  fi
}

demo_reload_edge() {
  local -a edge_ids=()
  demo_prepare_edge_config_permissions || return 1
  mapfile -t edge_ids < <(demo_compose ps -q edge)
  ((${#edge_ids[@]} == 1)) || {
    demo_fail 'running edge proxy is missing or ambiguous'
    return 1
  }
  EDGE_CONTAINER_ID=${edge_ids[0]}
  if ! demo_edge_uses_directory_mount "$EDGE_CONTAINER_ID"; then
    demo_edge_uses_legacy_file_mount "$EDGE_CONTAINER_ID" || {
      demo_fail 'edge proxy config mount is neither the supported directory layout nor the legacy single-file layout'
      return 1
    }
    demo_compose run --rm -T --no-deps --entrypoint nginx edge \
      -c /etc/nginx/demo/nginx.conf -t >/dev/null || {
      demo_fail 'new edge configuration failed its pre-migration syntax check'
      return 1
    }
    # This is the one-time transition from the legacy single-file bind mount.
    # Future config updates are visible through the directory mount and reload
    # Nginx gracefully without stopping the public edge container.
    demo_compose up -d --no-deps --force-recreate edge || {
      demo_fail 'edge proxy could not be recreated with the current configuration'
      return 1
    }
    mapfile -t edge_ids < <(demo_compose ps -q edge)
    ((${#edge_ids[@]} == 1)) || {
      demo_fail 'refreshed edge proxy is missing or ambiguous'
      return 1
    }
    EDGE_CONTAINER_ID=${edge_ids[0]}
    demo_edge_uses_directory_mount "$EDGE_CONTAINER_ID" || {
      demo_fail 'recreated edge proxy does not have the directory config mount'
      return 1
    }
  fi
  docker exec "$EDGE_CONTAINER_ID" nginx -c /etc/nginx/demo/nginx.conf -t >/dev/null || {
    demo_fail 'edge nginx configuration failed its syntax check'
    return 1
  }
  docker exec "$EDGE_CONTAINER_ID" nginx -c /etc/nginx/demo/nginx.conf -s reload >/dev/null || {
    demo_fail 'edge nginx graceful reload failed'
    return 1
  }
  demo_reload_tailnet_edge
}

demo_reload_tailnet_edge() {
  local enabled tailnet_ids_output
  local -a tailnet_ids=()
  enabled=$(demo_tailnet_enabled) || return 1
  if [[ "$enabled" != true ]]; then
    tailnet_ids_output=$(demo_tailnet_compose ps -a -q edge-tailnet) || return 1
    if [[ -n "$tailnet_ids_output" ]]; then
      demo_tailnet_compose stop edge-tailnet || return 1
      demo_tailnet_compose rm -f edge-tailnet || return 1
    fi
    demo_persist_tailnet_enabled false || return 1
    return 0
  fi

  demo_prepare_tailnet_client_config || return 1
  demo_prepare_tailnet_edge_config_permissions || return 1
  demo_tailnet_compose run --rm -T --no-deps --entrypoint nginx edge-tailnet \
    -c /etc/nginx/tailnet/nginx.conf -t >/dev/null || {
    demo_fail 'tailnet edge configuration failed its pre-start syntax check'
    return 1
  }
  demo_tailnet_compose up -d --no-deps edge-tailnet || {
    demo_fail 'tailnet edge proxy could not be started with the current configuration'
    return 1
  }
  tailnet_ids_output=$(demo_tailnet_compose ps --status running -q edge-tailnet) || return 1
  if [[ -n "$tailnet_ids_output" ]]; then
    mapfile -t tailnet_ids <<< "$tailnet_ids_output"
  fi
  ((${#tailnet_ids[@]} == 1)) || {
    demo_fail 'running tailnet edge proxy is missing or ambiguous'
    return 1
  }
  docker exec "${tailnet_ids[0]}" nginx -c /etc/nginx/tailnet/nginx.conf -t >/dev/null || {
    demo_fail 'tailnet edge nginx configuration failed its syntax check'
    return 1
  }
  docker exec "${tailnet_ids[0]}" nginx -c /etc/nginx/tailnet/nginx.conf -s reload >/dev/null || {
    demo_fail 'tailnet edge nginx graceful reload failed'
    return 1
  }
  demo_persist_tailnet_enabled true
}

demo_current_web_release() {
  local target
  if [[ ! -e "$DEMO_WEB_CURRENT_LINK" && ! -L "$DEMO_WEB_CURRENT_LINK" ]]; then
    printf '\n'
    return 0
  fi
  if [[ ! -L "$DEMO_WEB_CURRENT_LINK" ]]; then
    demo_fail 'current public web release must be a symlink'
    return 1
  fi
  target=$(readlink -- "$DEMO_WEB_CURRENT_LINK") || return 1
  [[ "$target" =~ ^releases/([0-9a-f]{40})-([0-9a-f]{64})$ ]] || {
    demo_fail 'current public web release target is invalid'
    return 1
  }
  [[ -d "$DEMO_WEB_ROOT/$target" && ! -L "$DEMO_WEB_ROOT/$target" ]] || {
    demo_fail 'current public web release directory is missing or unsafe'
    return 1
  }
  printf '%s %s\n' "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}"
}

demo_verify_web_release_manifest() {
  local directory="$1" source_commit="$2" digest_hex="$3"
  jq -e --arg source "$source_commit" --arg digest "sha256:$digest_hex" '
    type == "object"
    and (keys | sort) == ["artifactDigest", "endpoint", "fingerprint", "mode", "sourceCommit"]
    and .mode == "private"
    and .sourceCommit == $source
    and .artifactDigest == $digest
    and (.endpoint | type == "string" and startswith("wss://"))
    and (.fingerprint | type == "string" and test("^[0-9a-f]{16}$"))
  ' "$directory/mtproto-target.json" >/dev/null || {
    demo_fail 'public web release manifest does not match its release identity'
    return 1
  }
}

demo_reconcile_web_release() {
  local active_web current_web source_commit digest_hex image_ref server target temporary
  active_web=$(demo_active_web_release) || return 1
  current_web=$(demo_current_web_release) || return 1
  if [[ "$active_web" == "$current_web" ]]; then
    printf '%s\n' "$active_web"
    return 0
  fi

  if [[ -n "$current_web" ]]; then
    source_commit=${current_web%% *}
    digest_hex=${current_web#* }
    demo_verify_web_release_manifest "$DEMO_WEB_ROOT/releases/$source_commit-$digest_hex" \
      "$source_commit" "$digest_hex" || return 1
    server=$(demo_active_server) || return 1
    image_ref=$(demo_active_image_ref) || return 1
    demo_write_active_release "$server" "$image_ref" "$source_commit" "sha256:$digest_hex" || return 1
    printf '%s\n' "$current_web"
    return 0
  fi

  if [[ -n "$active_web" ]]; then
    source_commit=${active_web%% *}
    digest_hex=${active_web#* }
    target="releases/$source_commit-$digest_hex"
    demo_verify_web_release_manifest "$DEMO_WEB_ROOT/$target" "$source_commit" "$digest_hex" || return 1
    temporary="$DEMO_WEB_CURRENT_LINK.recover.$$"
    [[ ! -e "$temporary" && ! -L "$temporary" ]] || {
      demo_fail 'temporary web release recovery link already exists'
      return 1
    }
    if ! ln -s -- "$target" "$temporary" || ! mv -Tf -- "$temporary" "$DEMO_WEB_CURRENT_LINK"; then
      rm -f -- "$temporary"
      demo_fail 'current public web release could not be recovered'
      return 1
    fi
    printf '%s\n' "$active_web"
    return 0
  fi

  printf '\n'
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

demo_compose_with_file() {
  local compose_file="$1" image_ref enabled=false
  local -a compose_args=(
    --env-file "$DEMO_ENV_FILE"
    --project-directory "$DEMO_REPO_ROOT"
    --project-name teagram-demo
  )
  if [[ "$DEMO_DEPLOY_TARGET" == lima ]]; then
    compose_args+=(--file "$DEMO_COMPOSE_FILE")
  elif [[ "$compose_file" != "$DEMO_REPO_ROOT/docker-compose.yml" ]]; then
    compose_args+=(--file "$compose_file")
  else
    compose_args+=(--file "$DEMO_REPO_ROOT/docker-compose.yml")
  fi
  if [[ "$DEMO_DEPLOY_TARGET" != lima ]]; then
    if [[ "${DEMO_FORCE_TAILNET_COMPOSE:-false}" == true ]]; then
      enabled=true
    else
      enabled=$(demo_tailnet_enabled) || return 1
    fi
  fi
  if [[ "$compose_file" == "$DEMO_REPO_ROOT/docker-compose.yml" &&
    "$DEMO_DEPLOY_TARGET" != lima && "$enabled" == true ]]; then
    [[ -f "$DEMO_TAILNET_COMPOSE_FILE" && ! -L "$DEMO_TAILNET_COMPOSE_FILE" ]] || {
      demo_fail 'teagram-dev tailnet Compose overlay is missing or unsafe'
      return 1
    }
    compose_args+=(--file "$DEMO_TAILNET_COMPOSE_FILE")
  fi
  if [[ "$DEMO_DEPLOY_TARGET" != lima ]]; then
    compose_args+=(--profile demo)
  fi
  shift
  demo_prepare_runtime_tmp_dirs_for_up "$@" || return 1
  if ! image_ref=$(demo_compose_image_ref); then
    return 1
  fi
  env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
    -u TG_TURNSTILE_SIGNIN_ENABLED \
    -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE \
    -u TEAGRAM_DEMO_EDGE_BIND -u TEAGRAM_DEMO_EDGE_CONFIG_SHA256 \
    -u DEMO_FORCE_TAILNET_COMPOSE \
    "TEAGRAM_DEV_TAILNET_ENABLED=$enabled" \
    "TEAGRAM_DEMO_IMAGE_REF=$image_ref" \
    docker compose "${compose_args[@]}" "$@" </dev/null
}

demo_compose() {
  local arg server='' start_edge=false has_tailnet=false enabled
  local compose_file="$DEMO_REPO_ROOT/docker-compose.yml"
  if [[ "$DEMO_DEPLOY_TARGET" == lima ]]; then
    compose_file="$DEMO_COMPOSE_FILE"
  fi
  if [[ "${1:-}" == up ]]; then
    for arg in "$@"; do
      case "$arg" in
        teagramd|teagramd-next) server="$arg" ;;
        edge) start_edge=true ;;
        edge-tailnet) has_tailnet=true ;;
      esac
    done
    enabled=$(demo_tailnet_enabled) || return 1
    if [[ "$enabled" == true && "$start_edge" == true && "$has_tailnet" != true ]]; then
      set -- "$@" edge-tailnet
    fi
  fi
  if [[ -n "$server" ]]; then
    demo_seed_compose stop "$server" || return 1
    demo_seed_compose rm -f "$server" || return 1
  fi
  demo_compose_with_file "$compose_file" "$@"
}

demo_tailnet_compose() {
  DEMO_FORCE_TAILNET_COMPOSE=true \
    demo_compose_with_file "$DEMO_REPO_ROOT/docker-compose.yml" "$@"
}

demo_capture_tailnet_listener_state() {
  local running_ids_output
  local -a running_ids=()
  running_ids_output=$(demo_tailnet_compose ps --status running -q edge-tailnet) || return 1
  if [[ -n "$running_ids_output" ]]; then
    mapfile -t running_ids <<< "$running_ids_output"
  fi
  case "${#running_ids[@]}" in
    0) printf '%s\n' false ;;
    1) printf '%s\n' true ;;
    *)
      demo_fail 'running tailnet edge proxy is missing or ambiguous'
      return 1
      ;;
  esac
}

demo_restore_tailnet_listener_state() {
  case "${1:-}" in
    true|false) ;;
    *)
      demo_fail 'captured tailnet listener state is invalid'
      return 1
      ;;
  esac
  TEAGRAM_DEV_TAILNET_ENABLED="$1" demo_reload_tailnet_edge
}

demo_validation_compose() {
  local image_ref
  demo_prepare_runtime_tmp_dirs_for_up "$@" || return 1
  if ! image_ref=$(demo_compose_image_ref); then
    return 1
  fi
  env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
    -u TG_TURNSTILE_SIGNIN_ENABLED \
    -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE \
    "TEAGRAM_DEMO_IMAGE_REF=$image_ref" \
    TEAGRAM_DEMO_EDGE_BIND=127.0.0.1:18080 \
    docker compose "${DEMO_COMPOSE_ARGS[@]}" "$@" </dev/null
}

demo_compose_input() {
  local image_ref
  demo_prepare_runtime_tmp_dirs_for_up "$@" || return 1
  if ! image_ref=$(demo_compose_image_ref); then
    return 1
  fi
  env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
    -u TG_TURNSTILE_SIGNIN_ENABLED \
    -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE \
    -u TEAGRAM_DEMO_EDGE_BIND \
    "TEAGRAM_DEMO_IMAGE_REF=$image_ref" \
    docker compose "${DEMO_COMPOSE_ARGS[@]}" "$@"
}

demo_bootstrap_compose() {
  local image_ref arg
  local -a mapped_args=()
  for arg in "$@"; do
    case "$arg" in
      teagramd) mapped_args+=(teagramd-bootstrap) ;;
      *) mapped_args+=("$arg") ;;
    esac
  done
  demo_prepare_runtime_tmp_dirs_for_up "$@" || return 1
  if ! image_ref=$(demo_compose_image_ref); then
    return 1
  fi
  env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
    -u TG_TURNSTILE_SIGNIN_ENABLED \
    -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE \
    -u TEAGRAM_DEMO_EDGE_BIND \
    "TEAGRAM_DEMO_IMAGE_REF=$image_ref" \
    docker compose "${DEMO_COMPOSE_ARGS[@]}" --profile bootstrap \
      "${mapped_args[@]}" </dev/null
}

demo_seed_compose() {
  local image_ref arg server=''
  local -a mapped_args=()
  for arg in "$@"; do
    case "$arg" in
      teagramd) mapped_args+=(teagramd-seed); server="$arg" ;;
      teagramd-next) mapped_args+=(teagramd-next-seed); server="$arg" ;;
      *) mapped_args+=("$arg") ;;
    esac
  done
  if [[ "${1:-}" == up && -n "$server" ]]; then
    demo_bootstrap_compose stop "$server" || return 1
    demo_bootstrap_compose rm -f "$server" || return 1
  fi
  demo_prepare_runtime_tmp_dirs_for_up "$@" || return 1
  if ! image_ref=$(demo_compose_image_ref); then
    return 1
  fi
  env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
    -u TG_TURNSTILE_SIGNIN_ENABLED \
    -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE \
    -u TEAGRAM_DEMO_EDGE_BIND \
    "TEAGRAM_DEMO_IMAGE_REF=$image_ref" \
    docker compose "${DEMO_COMPOSE_ARGS[@]}" --profile seed \
      "${mapped_args[@]}" </dev/null
}

demo_seed_candidate_image_id() {
  local server="$1" expected_sha="$2" candidate_ids_output image_id revision
  local -a candidate_ids=()

  case "$server" in
    teagramd|teagramd-next) ;;
    *) demo_fail 'candidate server is not a known demo slot'; return 1 ;;
  esac
  candidate_ids_output=$(demo_seed_compose ps --status running -q "$server") || return 1
  if [[ -n "$candidate_ids_output" ]]; then
    mapfile -t candidate_ids <<< "$candidate_ids_output"
  fi
  ((${#candidate_ids[@]} == 1)) || {
    demo_fail 'running seed candidate is missing or ambiguous'
    return 1
  }
  image_id=$(docker inspect --format '{{.Image}}' "${candidate_ids[0]}") || {
    demo_fail 'running seed candidate image could not be inspected'
    return 1
  }
  revision=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$image_id") || {
    demo_fail 'running seed candidate revision could not be inspected'
    return 1
  }
  [[ "$revision" == "$expected_sha" ]] || {
    demo_fail 'running seed candidate image does not match the requested SHA'
    return 1
  }
  printf '%s\n' "$image_id"
}

demo_remove_candidate_services() {
  local server="$1" rc=0 production_running seed_running

  demo_compose stop "$server" || rc=1
  demo_seed_compose stop "$server" || rc=1
  demo_seed_compose rm -f "$server" || rc=1
  if ! production_running=$(demo_compose ps --status running -q "$server"); then
    rc=1
  elif [[ -n "$production_running" ]]; then
    demo_fail 'production candidate is still running after rollback'
    rc=1
  fi
  if ! seed_running=$(demo_seed_compose ps --status running -q "$server"); then
    rc=1
  elif [[ -n "$seed_running" ]]; then
    demo_fail 'seed candidate is still running after rollback'
    rc=1
  fi
  return "$rc"
}

demo_seed_compose_input() {
  local image_ref arg
  local -a mapped_args=()
  for arg in "$@"; do
    case "$arg" in
      teagramd) mapped_args+=(teagramd-seed) ;;
      teagramd-next) mapped_args+=(teagramd-next-seed) ;;
      *) mapped_args+=("$arg") ;;
    esac
  done
  demo_prepare_runtime_tmp_dirs_for_up "$@" || return 1
  if ! image_ref=$(demo_compose_image_ref); then
    return 1
  fi
  env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
    -u TG_TURNSTILE_SIGNIN_ENABLED \
    -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE \
    -u TEAGRAM_DEMO_EDGE_BIND \
    "TEAGRAM_DEMO_IMAGE_REF=$image_ref" \
    docker compose "${DEMO_COMPOSE_ARGS[@]}" --profile seed \
      "${mapped_args[@]}"
}

demo_check_blob_mount() {
  local rendered_json="$1" blob_path
  blob_path=$(jq -er '.volumes.tgblobs.driver_opts.device' "$rendered_json") ||
    demo_fail 'resolved tgblobs volume has no device path'
  python3 "$DEMO_SCRIPT_DIR/check-blob-mount.py" "$blob_path"
}

demo_render_config() {
  local destination="$1" tailnet_enabled
  tailnet_enabled=$(demo_tailnet_enabled) || return 1
  demo_prepare_client_config_dir
  demo_prepare_haproxy_runtime
  demo_compose config --format json > "$destination"
  chmod 0600 "$destination"
  jq -e \
    --arg haproxyConfigHash "$TEAGRAM_DEMO_HAPROXY_CONFIG_SHA256" \
    --arg tailnetEnabled "$tailnet_enabled" \
    --arg tailnetOrigin "$DEMO_TAILNET_ORIGIN" '
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
    and .services.teagramd.environment.TG_DEMO_GENERAL_CHAT_ENABLED == "true"
    and .services["teagramd-next"].environment.TG_DEMO_GENERAL_CHAT_ENABLED == "true"
    and (.services.teagramd.environment.TG_TURNSTILE_SIGNIN_ENABLED == "false" or .services.teagramd.environment.TG_TURNSTILE_SIGNIN_ENABLED == "true")
    and .services.teagramd.environment.TG_TURNSTILE_SIGNIN_ENABLED == .services["teagramd-next"].environment.TG_TURNSTILE_SIGNIN_ENABLED
    and .services.teagramd.environment.TG_LOG_LOGIN_CODES == "false"
    and .services.teagramd.environment.TG_LOG_AUTH_KEY_UNBINDS == "true"
    and .services["teagramd-next"].environment.TG_LOG_AUTH_KEY_UNBINDS == "true"
    and .services.teagramd.environment.TG_MAX_FILE_BYTES == "16777216"
    and .services.teagramd.environment.TG_MAX_USER_STORAGE_BYTES == "268435456"
    and .services.teagramd.environment.TG_WEBSOCKET_LISTEN_ADDR == ":2446"
    and .services.teagramd.environment.TG_WEBSOCKET_ALLOWED_ORIGINS == ("https://web.teagram.io,https://demo.teagram.io" + (if $tailnetEnabled == "true" then "," + $tailnetOrigin else "" end))
    and .services["teagramd-next"].environment.TG_WEBSOCKET_ALLOWED_ORIGINS == ("https://web.teagram.io,https://demo.teagram.io" + (if $tailnetEnabled == "true" then "," + $tailnetOrigin else "" end))
    and .services.teagramd.environment.TG_WEBSOCKET_BROWSER_ONLY == "true"
    and .services["teagramd-next"].environment.TG_WEBSOCKET_BROWSER_ONLY == "true"
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
    and .services.teagramd.environment.TG_TURNSTILE_TEST_MODE == "false"
    and .services.teagramd.environment.TG_TURNSTILE_HOSTNAMES == ("web.teagram.io,demo.teagram.io" + (if $tailnetEnabled == "true" then ",teagram-dev.tailaa4918.ts.net" else "" end))
    and .services["teagramd-next"].environment.TG_TURNSTILE_HOSTNAMES == ("web.teagram.io,demo.teagram.io" + (if $tailnetEnabled == "true" then ",teagram-dev.tailaa4918.ts.net" else "" end))
    and .services.teagramd.environment.TG_TURNSTILE_QA_BYPASS_TOKEN_FILE == "/run/secrets/teagram_demo_turnstile_qa_bypass"
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
    and any(.services.edge.volumes[]; (.source | endswith("/deploy/teagram-demo/nginx-config")) and .target == "/etc/nginx/demo" and .read_only)
    and (.services.edge.networks | keys | sort) == ["backend", "edge"]
    and (if $tailnetEnabled == "true" then
      .services["edge-tailnet"] != null
      and (.services["edge-tailnet"].ports | length) == 1
      and .services["edge-tailnet"].ports[0].host_ip == "127.0.0.1"
      and ((.services["edge-tailnet"].ports[0].target | tostring) == "8081")
      and ((.services["edge-tailnet"].ports[0].published | tostring) == "8081")
      and (.services["edge-tailnet"].networks | keys | sort) == ["backend", "teagram-dev-tailnet"]
      and (.networks["teagram-dev-tailnet"].internal // false) == false
      and .networks["teagram-dev-tailnet"].ipam.config[0].gateway == "198.18.134.1"
      and .networks["teagram-dev-tailnet"].ipam.config[0].subnet == "198.18.134.0/24"
      and .networks["teagram-dev-tailnet"].driver_opts["com.docker.network.bridge.enable_ip_masquerade"] == "false"
      and any(.services["edge-tailnet"].volumes[]; (.source | endswith("/deploy/teagram-demo/nginx-tailnet.conf")) and .target == "/etc/nginx/tailnet/nginx.conf" and .read_only)
      and any(.services["edge-tailnet"].volumes[]; (.source | endswith("/.state/teagram-demo/tmp/edge-tailnet")) and .target == "/tmp" and (.read_only // false) == false)
      else .services["edge-tailnet"] == null end)
    and .secrets.teagram_demo_turnstile.file == "/var/lib/teagram-demo/secrets/turnstile-secret-key"
    and .secrets.teagram_demo_turnstile_qa_bypass.file == "/var/lib/teagram-demo/secrets/turnstile-qa-bypass-token"
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
  if [[ -e "$DEMO_SEED_OWNER_SESSION_FILE" || -L "$DEMO_SEED_OWNER_SESSION_FILE" ]]; then
    [[ -f "$DEMO_SEED_OWNER_SESSION_FILE" && ! -L "$DEMO_SEED_OWNER_SESSION_FILE" &&
      "$(stat -c '%u:%a' "$DEMO_SEED_OWNER_SESSION_FILE")" == '0:600' ]] ||
      demo_fail 'seed-owner session file has unsafe type or permissions'
    rm -f -- "$DEMO_SEED_OWNER_SESSION_FILE"
  fi
  if [[ -e "$DEMO_SEED_ACCOUNT_DIR/moderation-smoke.session" || -L "$DEMO_SEED_ACCOUNT_DIR/moderation-smoke.session" ]]; then
    [[ -d "$DEMO_SEED_ACCOUNT_DIR" && ! -L "$DEMO_SEED_ACCOUNT_DIR" &&
      -f "$DEMO_SEED_ACCOUNT_DIR/moderation-smoke.session" && ! -L "$DEMO_SEED_ACCOUNT_DIR/moderation-smoke.session" &&
      "$(stat -c '%u:%a' "$DEMO_SEED_ACCOUNT_DIR/moderation-smoke.session")" == '0:600' ]] ||
      demo_fail 'General moderation smoke session has unsafe type or permissions'
    rm -f -- "$DEMO_SEED_ACCOUNT_DIR/moderation-smoke.session"
  fi
  rm -f -- "$DEMO_SEED_OWNER_MARKER"
}

demo_prepare_seed_owner_session() {
  [[ ! -L "$DEMO_SEED_SECRET_DIR" ]] || demo_fail 'seed secret directory must not be a symlink'
  install -d -o root -g root -m 0700 "$DEMO_SEED_SECRET_DIR"
  [[ "$(stat -c '%u:%a' "$DEMO_SEED_SECRET_DIR")" == '0:700' ]] ||
    demo_fail 'seed secret directory must be root-owned mode 0700'
  if [[ -e "$DEMO_SEED_OWNER_SESSION_FILE" || -L "$DEMO_SEED_OWNER_SESSION_FILE" ]]; then
    [[ -f "$DEMO_SEED_OWNER_SESSION_FILE" && ! -L "$DEMO_SEED_OWNER_SESSION_FILE" &&
      "$(stat -c '%u:%a' "$DEMO_SEED_OWNER_SESSION_FILE")" == '0:600' ]] ||
      demo_fail 'seed-owner session file has unsafe type or permissions'
  else
    install -o root -g root -m 0600 /dev/null "$DEMO_SEED_OWNER_SESSION_FILE" ||
      demo_fail 'seed-owner session file could not be created securely'
  fi
}

demo_seed_owner_admin_command() {
  local active_server="$1"
  shift
  demo_seed_compose_input exec -T --user 0:65532 "$active_server" \
    /usr/local/bin/teagramd admin "$@"
}

demo_prepare_seed_owner() {
  local work_dir="$1" temporary marker_temporary session_temporary active_server
  active_server=$(demo_active_server)

  [[ ! -L "$DEMO_SEED_SECRET_DIR" ]] || demo_fail 'seed secret directory must not be a symlink'
  install -d -o root -g root -m 0700 "$DEMO_SEED_SECRET_DIR"
  [[ "$(stat -c '%u:%a' "$DEMO_SEED_SECRET_DIR")" == '0:700' ]] ||
    demo_fail 'seed secret directory must be root-owned mode 0700'

  demo_prepare_seed_owner_session

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

  session_temporary=$(mktemp "$DEMO_SEED_SECRET_DIR/.seed-owner.session.XXXXXXXX")
  if ! {
    chmod 0600 "$session_temporary" &&
      chown root:root "$session_temporary" &&
      mv -f -- "$session_temporary" "$DEMO_SEED_OWNER_SESSION_FILE"
  }; then
    rm -f -- "$session_temporary"
    demo_fail 'seed-owner session file could not be reset for the new database'
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

  if ! demo_seed_owner_admin_command "$active_server" create-user \
    --username "$DEMO_SEED_OWNER_USERNAME" \
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
