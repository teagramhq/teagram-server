#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -P "$SCRIPT_DIR/../.." && pwd)
unset TG_UPLOADS_DISABLED
# shellcheck source=deploy/teagram-demo/demo-common.sh
source "$SCRIPT_DIR/demo-common.sh"
TMP_DIR=$(mktemp -d)
trap 'rm -rf -- "$TMP_DIR"' EXIT

cat > "$TMP_DIR/demo.env" <<'ENV'
POSTGRES_PASSWORD=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
TG_AUTHKEY_ENC_KEY=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
TG_PUBLIC_LINK_PREFIX=https://demo.teagram.io/
TG_ADVERTISE_ADDR=demo.teagram.io:443
TG_ADVERTISE_DEMO_GUEST_SIGN_UP=true
TG_WEBSOCKET_LISTEN_ADDR=:2446
TG_WEBSOCKET_ALLOWED_ORIGINS=https://web.teagram.io,https://demo.teagram.io
TG_LOG_LOGIN_CODES=false
TG_REGISTRATION=open
TG_MAX_FILE_BYTES=16777216
TG_MAX_USER_STORAGE_BYTES=268435456
TG_UPLOADS_DISABLED=true
TEAGRAM_DEMO_BLOB_PATH=/srv/teagram-demo-blobs/data
TEAGRAM_DEMO_TURNSTILE_SITE_KEY=fixture-site-key
ENV
secret_file=/var/lib/teagram-demo/secrets/turnstile-secret-key
printf 'TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE=%s\n' "$secret_file" >> "$TMP_DIR/demo.env"
chmod 0600 "$TMP_DIR/demo.env"

compose=(
  docker compose
  --env-file "$TMP_DIR/demo.env"
  --project-directory "$REPO_ROOT"
  --project-name teagram-demo
  --file "$SCRIPT_DIR/compose.yaml"
)
TG_UPLOADS_DISABLED=false env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
  -u TG_UPLOAD_MAX_BYTES -u TG_RATE_LIMIT_UPLOAD_BYTES \
  -u TG_TURNSTILE_SIGNIN_ENABLED \
  -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE \
  "${compose[@]}" config --format json > "$TMP_DIR/target.json"
jq -e \
  --arg secretFile "$secret_file" \
  --arg haproxyConfigHash "$TEAGRAM_DEMO_HAPROXY_CONFIG_SHA256" '
  .name == "teagram-demo"
  and (.services | keys | sort) == ["edge", "haproxy", "migrate", "postgres", "teagramd", "teagramd-next", "turnstile-egress"]
  and ((.services.teagramd.ports // []) | length) == 0
  and ((.services.edge.ports | length) == 1)
  and .services.edge.ports[0].host_ip == "127.0.0.1"
  and (.services.edge.ports[0].published | tostring) == "8080"
  and (.services | to_entries | map(.value.ports // []) | add | map(.published | tostring) | index("2443")) == null
  and ((.services.haproxy.ports // []) | length) == 0
  and .services.haproxy.platform == "linux/amd64"
  and .services.haproxy.image == "mirror.gcr.io/library/haproxy:3.2-alpine@sha256:51f173c6825753d9e3a888b1004a1c2cb730936cc9178730a5c555246bd161a2"
  and .services.haproxy.labels["com.teagram.demo.haproxy-config-sha256"] == $haproxyConfigHash
  and any(.services.haproxy.volumes[]; (.source | endswith("/.state/teagram-demo/haproxy-config")) and .target == "/usr/local/etc/haproxy" and .read_only)
  and .services.haproxy.networks.backend.ipv4_address == "198.18.130.2"
  and .services.haproxy.healthcheck.test[-1] == "http://127.0.0.1:8404/healthz"
  and .services.edge.depends_on.haproxy.condition == "service_healthy"
  and .services.edge.entrypoint == ["nginx", "-c", "/etc/nginx/demo/nginx.conf", "-g", "daemon off;"]
  and (.services.edge.labels // {}) == {}
  and any(.services.edge.volumes[]; (.source | endswith("/deploy/teagram-demo/nginx-config")) and .target == "/etc/nginx/demo" and .read_only)
  and any(.services.edge.volumes[]; (.source | endswith("/.state/teagram-demo/public-client")) and .target == "/usr/share/nginx/html/.well-known/teagramd" and .read_only)
  and (.services.teagramd.networks | keys) == ["backend"]
  and .services.teagramd.networks.backend.ipv4_address == "198.18.130.253"
  and .services["teagramd-next"].networks.backend.ipv4_address == "198.18.130.251"
  and .services.teagramd.environment.HTTPS_PROXY == "http://turnstile-egress:3128"
  and .services.teagramd.environment.HTTP_PROXY == "http://turnstile-egress:3128"
  and .services.teagramd.environment.NO_PROXY == "localhost,127.0.0.1,postgres,teagramd,teagramd-next"
  and .services.teagramd.depends_on["turnstile-egress"].condition == "service_healthy"
  and .services["turnstile-egress"].platform == "linux/amd64"
  and .services["turnstile-egress"].image == "mirror.gcr.io/ubuntu/squid:7.2-26.04_edge@sha256:f6843088c0eab9e63ecb8a1c215d939cd97d0370611bb2bdb65c4a8e48b3c3eb"
  and (.services["turnstile-egress"].ports // [] | length) == 0
  and .services["turnstile-egress"].networks.backend.ipv4_address == "198.18.130.252"
  and .services["turnstile-egress"].networks.egress.gw_priority == 1
  and (.services["turnstile-egress"].networks | keys | sort) == ["backend", "egress"]
  and .services["turnstile-egress"].healthcheck.test == ["CMD", "/usr/bin/pebble", "health"]
  and any(.services["turnstile-egress"].volumes[]; .target == "/var/lib/pebble/default/layers/002-demo-health.yaml" and .read_only)
  and .networks.egress.internal != true
  and .services["turnstile-egress"].logging.options["max-size"] == "10m"
  and .services["turnstile-egress"].logging.options["max-file"] == "3"
  and (.services.teagramd.networks | keys) == ["backend"]
  and .networks.backend.internal == true
  and .networks.backend.ipam.config[0].subnet == "198.18.130.0/24"
  and .networks.backend.ipam.config[0].ip_range == "198.18.130.128/26"
  and (.services.edge.networks | keys | sort) == ["backend", "edge"]
  and .services.teagramd.environment.TG_REGISTRATION == "open"
  and .services.teagramd.environment.TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET == "20"
  and .services["teagramd-next"].environment.TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET == "20"
  and .services.teagramd.environment.TG_TURNSTILE_SIGNIN_ENABLED == "false"
  and .services["teagramd-next"].environment.TG_TURNSTILE_SIGNIN_ENABLED == "false"
  and .services.teagramd.environment.TG_STICKER_IMPORT == "off"
  and .services["teagramd-next"].environment.TG_STICKER_IMPORT == "off"
  and .services.teagramd.environment.TG_ADVERTISE_DEMO_GUEST_SIGN_UP == "true"
  and .services["teagramd-next"].environment.TG_ADVERTISE_DEMO_GUEST_SIGN_UP == "true"
  and .services.teagramd.environment.GOMAXPROCS == "1"
  and .services.teagramd.environment.GOMEMLIMIT == "320MiB"
  and (.services.teagramd.mem_limit | tostring) == "469762048"
  and (.services["teagramd-next"].mem_limit | tostring) == "469762048"
  and .services.teagramd.environment.TG_LOG_LOGIN_CODES == "false"
  and .services.teagramd.environment.TG_LOG_AUTH_KEY_UNBINDS == "true"
  and .services["teagramd-next"].environment.TG_LOG_AUTH_KEY_UNBINDS == "true"
  and .services.teagramd.environment.TG_MAX_FILE_BYTES == "16777216"
  and .services.teagramd.environment.TG_MAX_USER_STORAGE_BYTES == "268435456"
  and .services.teagramd.environment.TG_UPLOAD_MAX_BYTES == "512000"
  and .services.teagramd.environment.TG_RATE_LIMIT_UPLOAD_BYTES == "20971520"
  and .services.teagramd.environment.TG_UPLOADS_DISABLED == "true"
  and .services["teagramd-next"].environment.TG_UPLOAD_MAX_BYTES == "512000"
  and .services["teagramd-next"].environment.TG_RATE_LIMIT_UPLOAD_BYTES == "20971520"
  and .services["teagramd-next"].environment.TG_UPLOADS_DISABLED == "true"
  and .services.teagramd.environment.TG_WEBSOCKET_LISTEN_ADDR == ":2446"
  and .services.teagramd.environment.TG_WEBSOCKET_ALLOWED_ORIGINS == "https://web.teagram.io,https://demo.teagram.io"
  and .services["teagramd-next"].environment.TG_WEBSOCKET_ALLOWED_ORIGINS == "https://web.teagram.io,https://demo.teagram.io"
  and .services.teagramd.environment.TG_CLIENT_ADDR_TRUST == "proxy-v2"
  and .services.teagramd.environment.TG_CLIENT_ADDR_PROXY_CIDRS == "198.18.130.2/32"
  and .services.teagramd.environment.TG_TURNSTILE_SITE_KEY == "fixture-site-key"
  and .services.teagramd.environment.TG_TURNSTILE_SECRET_KEY_FILE == "/run/secrets/teagram_demo_turnstile"
  and .services.teagramd.environment.TG_TURNSTILE_TEST_MODE == "false"
  and .services.teagramd.environment.TG_TURNSTILE_HOSTNAMES == "web.teagram.io,demo.teagram.io"
  and .services.teagramd.environment.TG_TURNSTILE_QA_BYPASS_TOKEN_FILE == "/run/secrets/teagram_demo_turnstile_qa_bypass"
  and .services["teagramd-next"].environment.TG_TURNSTILE_TEST_MODE == "false"
  and .services["teagramd-next"].environment.TG_TURNSTILE_HOSTNAMES == "web.teagram.io,demo.teagram.io"
  and .services["teagramd-next"].environment.TG_TURNSTILE_QA_BYPASS_TOKEN_FILE == "/run/secrets/teagram_demo_turnstile_qa_bypass"
  and any(.services.teagramd.secrets[]; .source == "teagram_demo_turnstile_qa_bypass" and .target == "/run/secrets/teagram_demo_turnstile_qa_bypass")
  and any(.services["teagramd-next"].secrets[]; .source == "teagram_demo_turnstile_qa_bypass" and .target == "/run/secrets/teagram_demo_turnstile_qa_bypass")
  and .secrets.teagram_demo_turnstile.file == $secretFile
  and .secrets.teagram_demo_turnstile_qa_bypass.file == "/var/lib/teagram-demo/secrets/turnstile-qa-bypass-token"
  and .services.postgres.environment.PGDATA == "/var/lib/postgresql/18/docker"
  and .services.postgres.platform == "linux/amd64"
  and .services.postgres.networks.backend.ipv4_address == "198.18.130.3"
  and (.services.postgres.mem_limit | tostring) == "335544320"
  and any(.services.postgres.volumes[]; (.source | endswith("/deploy/teagram-demo/postgresql.conf")) and .target == "/etc/postgresql/postgresql.conf" and .read_only)
  and .services.teagramd.image == "teagramd:local"
  and .services["teagramd-next"].image == "teagramd:local"
  and .services.teagramd.build == null
  and .services["teagramd-next"].build == null
  and (.services.migrate.mem_limit | tostring) == "268435456"
  and (.services.postgres.image | endswith("@sha256:d8703cd7fba306b9fec9268ecedfa8a966846c053036a60e3635791957eb2f66"))
  and .services.migrate.platform == "linux/amd64"
  and (.services.migrate.image | endswith("@sha256:5abea953e8e048d5b27b1b3e2fc83e17fdc1f7cd0d908a8432ed0c7e6394c58f"))
  and any(.services.postgres.volumes[]; .target == "/var/lib/postgresql")
  and .volumes.tgblobs.driver == "local"
  and .volumes.tgblobs.driver_opts.type == "none"
  and .volumes.tgblobs.driver_opts.o == "bind"
  and .volumes.tgblobs.driver_opts.device == "/srv/teagram-demo-blobs/data"
' "$TMP_DIR/target.json" >/dev/null

EDGE_CONFIG_FIXTURE="$TMP_DIR/restrictive-checkout/deploy/teagram-demo"
(
  umask 077
  mkdir -p "$EDGE_CONFIG_FIXTURE/nginx-config"
  printf '%s\n' 'server {}' > "$EDGE_CONFIG_FIXTURE/nginx-config/nginx.conf"
)
[[ "$(stat -c '%a' "$EDGE_CONFIG_FIXTURE/nginx-config")" == 700 ]]
[[ "$(stat -c '%a' "$EDGE_CONFIG_FIXTURE/nginx-config/nginx.conf")" == 600 ]]
DEMO_SCRIPT_DIR="$EDGE_CONFIG_FIXTURE" demo_prepare_edge_config_permissions
[[ "$(stat -c '%a' "$EDGE_CONFIG_FIXTURE/nginx-config")" == 755 ]]
[[ "$(stat -c '%a' "$EDGE_CONFIG_FIXTURE/nginx-config/nginx.conf")" == 644 ]]

cp -- "$TMP_DIR/demo.env" "$TMP_DIR/demo-signin.env"
printf '%s\n' 'TG_TURNSTILE_SIGNIN_ENABLED=true' >> "$TMP_DIR/demo-signin.env"
env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
  -u TG_TURNSTILE_SIGNIN_ENABLED \
  -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE \
  docker compose --env-file "$TMP_DIR/demo-signin.env" \
    --project-directory "$REPO_ROOT" --project-name teagram-demo \
    --file "$SCRIPT_DIR/compose.yaml" config --format json > "$TMP_DIR/signin.json"
jq -e '
  .services.teagramd.environment.TG_TURNSTILE_SIGNIN_ENABLED == "true"
  and .services["teagramd-next"].environment.TG_TURNSTILE_SIGNIN_ENABLED == "true"
  and .services.teagramd.environment.TG_ADVERTISE_DEMO_GUEST_SIGN_UP == "true"
  and .services["teagramd-next"].environment.TG_ADVERTISE_DEMO_GUEST_SIGN_UP == "true"
' "$TMP_DIR/signin.json" >/dev/null

(
  demo_compose() {
    cat "$TMP_DIR/target.json"
  }
  demo_compose_with_file() {
    docker compose --project-directory "$REPO_ROOT" --project-name teagram-demo \
      --file "$1" "${@:2}"
  }
  LEGACY_SOURCE_HASH=$(printf 'legacy nginx config fixture\n' | sha256sum | awk '{print $1}')
  LEGACY_HASH=$(demo_edge_legacy_compose_hash "$LEGACY_SOURCE_HASH")
  [[ "$LEGACY_HASH" =~ ^[0-9a-f]{64}$ ]] || {
    printf 'normalized legacy edge Compose file did not produce a valid hash\n' >&2
    exit 1
  }
)

(
  MOCK_TARGET_HASH=$(printf 'target config' | sha256sum | awk '{print $1}')
  MOCK_OTHER_HASH=$(printf 'changed config' | sha256sum | awk '{print $1}')
  MOCK_LIVE_HASH=$MOCK_TARGET_HASH
  MOCK_HAPROXY_SOURCE_HASH=$TEAGRAM_DEMO_HAPROXY_CONFIG_SHA256
  MOCK_PREVIOUS_SHA=1111111111111111111111111111111111111111
  MOCK_LEGACY_SOURCE_HASH=$(printf 'legacy nginx config fixture\n' | sha256sum | awk '{print $1}')
  MOCK_LIVE_EDGE_SOURCE_HASH=$MOCK_LEGACY_SOURCE_HASH
  MOCK_MOUNT_LAYOUT=directory
  MOCK_EDGE_DRIFT=none
  MOCK_CURRENT_RENDER="$TMP_DIR/current-edge-render.json"

  git() {
    if [[ "$1" == -C && "$2" == "$DEMO_REPO_ROOT" && "$3" == show &&
      "$4" == "$MOCK_PREVIOUS_SHA:deploy/teagram-demo/nginx.conf" ]]; then
      printf 'legacy nginx config fixture\n'
    else
      return 1
    fi
  }

  demo_compose() {
    if [[ "$1" == ps && "$2" == -q ]]; then
      printf 'proxy-container\n'
    elif [[ "$1" == config && "$2" == --hash && "$3" == "$MOCK_SERVICE" ]]; then
      printf '%s %s\n' "$3" "$MOCK_TARGET_HASH"
    elif [[ "$1" == config && "$2" == --format && "$3" == json ]]; then
      if [[ "$MOCK_EDGE_DRIFT" == none ]]; then
        cat "$TMP_DIR/target.json" | tee "$MOCK_CURRENT_RENDER"
      else
        jq --arg drift "$MOCK_EDGE_DRIFT" '
          if $drift == "image" then .services.edge.image += "-drift"
          elif $drift == "port" then .services.edge.ports[0].published = "18080"
          elif $drift == "network" then .services.edge.networks.edge.aliases = ["drift"]
          elif $drift == "resources" then .services.edge.mem_limit = ((.services.edge.mem_limit | tonumber) + 1048576)
          else error("unknown edge drift fixture") end
        ' "$TMP_DIR/target.json" | tee "$MOCK_CURRENT_RENDER"
      fi
    else
      return 1
    fi
  }
  demo_compose_with_file() {
    docker compose --file "$1" "${@:2}"
  }
  docker() {
    if [[ "$1" == compose ]]; then
      local config_file=
      local -a args=("$@")
      local index
      for ((index = 1; index < ${#args[@]}; index++)); do
        if [[ "${args[index]}" == --file ]]; then
          config_file="${args[index + 1]}"
          break
        fi
      done
      [[ -f "$config_file" && "${args[-3]} ${args[-2]} ${args[-1]}" == 'config --hash edge' ]] || return 1
      jq -e \
        --slurpfile current "$MOCK_CURRENT_RENDER" \
        --arg legacyFile "$DEMO_SCRIPT_DIR/nginx.conf" \
        --arg discoveryDir "$DEMO_STATE_DIR/public-client" \
        --arg legacySourceHash "$MOCK_LEGACY_SOURCE_HASH" '
          .services.edge as $normalized
          | $current[0].services.edge as $desired
          | $normalized.entrypoint == ["nginx", "-g", "daemon off;"]
            and ($normalized.labels == (($desired.labels // {}) + {
              "com.teagram.demo.edge-config-sha256": $legacySourceHash
            }))
            and $normalized.image == $desired.image
            and $normalized.ports == $desired.ports
            and $normalized.networks == $desired.networks
            and $normalized.mem_limit == $desired.mem_limit
            and $normalized.memswap_limit == $desired.memswap_limit
            and $normalized.cpus == $desired.cpus
            and $normalized.pids_limit == $desired.pids_limit
            and any($normalized.volumes[]; .type == "bind" and .source == $legacyFile and
              .target == "/etc/nginx/nginx.conf" and .read_only)
            and all($normalized.volumes[]; .target != "/etc/nginx/demo")
            and any($normalized.volumes[]; .source == $discoveryDir and
              .target == "/usr/share/nginx/html/.well-known/teagramd" and .read_only)
        ' "$config_file" >/dev/null || return 1
      jq -c '.services.edge' "$config_file" | sha256sum | awk '{print "edge", $1}'
      return
    fi
    [[ "$1" == inspect && "$2" == --format ]] || return 1
    case "$3" in
      *com.docker.compose.config-hash*) printf '%s\n' "$MOCK_LIVE_HASH" ;;
      *com.teagram.demo.haproxy-config-sha256*) printf '%s\n' "$MOCK_HAPROXY_SOURCE_HASH" ;;
      *com.teagram.demo.edge-config-sha256*) printf '%s\n' "$MOCK_LIVE_EDGE_SOURCE_HASH" ;;
      *'.Mounts'*)
        if [[ "$MOCK_MOUNT_LAYOUT" == directory ]]; then
          printf 'bind|%s/nginx-config|/etc/nginx/demo\n' "$DEMO_SCRIPT_DIR"
        elif [[ "$MOCK_MOUNT_LAYOUT" == legacy ]]; then
          printf 'bind|%s/nginx.conf|/etc/nginx/nginx.conf\n' "$DEMO_SCRIPT_DIR"
        else
          printf 'bind|/unexpected/path|/etc/nginx/other\n'
        fi
        ;;
      *) return 1 ;;
    esac
  }

  MOCK_SERVICE=haproxy
  demo_require_proxy_configuration_current haproxy "$TEAGRAM_DEMO_HAPROXY_CONFIG_SHA256"
  MOCK_LIVE_HASH=$MOCK_OTHER_HASH
  if demo_require_proxy_configuration_current haproxy "$TEAGRAM_DEMO_HAPROXY_CONFIG_SHA256" >/dev/null 2>&1; then
    printf '%s\n' 'haproxy accepted a stale Compose hash' >&2
    exit 1
  fi
  MOCK_LIVE_HASH=$MOCK_TARGET_HASH
  MOCK_HAPROXY_SOURCE_HASH=$MOCK_OTHER_HASH
  if demo_require_proxy_configuration_current haproxy "$TEAGRAM_DEMO_HAPROXY_CONFIG_SHA256" >/dev/null 2>&1; then
    printf '%s\n' 'haproxy accepted a stale source configuration hash' >&2
    exit 1
  fi

  MOCK_SERVICE=edge
  MOCK_HAPROXY_SOURCE_HASH=$TEAGRAM_DEMO_HAPROXY_CONFIG_SHA256
  demo_require_proxy_configuration_current edge
  MOCK_LIVE_HASH=$MOCK_OTHER_HASH
  if demo_require_proxy_configuration_current edge >/dev/null 2>&1; then
    printf '%s\n' 'edge accepted a stale Compose hash with the new mount layout' >&2
    exit 1
  fi
  MOCK_LIVE_HASH=$MOCK_OTHER_HASH
  MOCK_MOUNT_LAYOUT=legacy
  if demo_require_proxy_configuration_current edge >/dev/null 2>&1; then
    printf '%s\n' 'edge accepted a legacy mount without deployment migration permission' >&2
    exit 1
  fi
  MOCK_LEGACY_HASH=$(demo_edge_legacy_compose_hash "$MOCK_LEGACY_SOURCE_HASH")
  MOCK_LIVE_HASH=$MOCK_LEGACY_HASH
  demo_require_proxy_configuration_current edge "" 1 "$MOCK_PREVIOUS_SHA"
  MOCK_LIVE_EDGE_SOURCE_HASH=$MOCK_OTHER_HASH
  if demo_require_proxy_configuration_current edge "" 1 "$MOCK_PREVIOUS_SHA" >/dev/null 2>&1; then
    printf '%s\n' 'edge accepted a mismatched legacy source configuration hash' >&2
    exit 1
  fi
  MOCK_LIVE_EDGE_SOURCE_HASH=$MOCK_LEGACY_SOURCE_HASH
  MOCK_LIVE_HASH=$MOCK_OTHER_HASH
  if demo_require_proxy_configuration_current edge "" 1 "$MOCK_PREVIOUS_SHA" >/dev/null 2>&1; then
    printf '%s\n' 'edge accepted unrelated Compose drift through legacy migration' >&2
    exit 1
  fi
  MOCK_LIVE_HASH=$MOCK_LEGACY_HASH
  for MOCK_EDGE_DRIFT in image port network resources; do
    if demo_require_proxy_configuration_current edge "" 1 "$MOCK_PREVIOUS_SHA" >/dev/null 2>&1; then
      printf 'edge accepted %s drift through legacy migration\n' "$MOCK_EDGE_DRIFT" >&2
      exit 1
    fi
  done
)

(
  MOCK_MOUNT_LAYOUT=legacy
  MOCK_EDGE_RECREATE_COUNT=0
  MOCK_EDGE_PRECHECKED=0
  MOCK_EDGE_CONFIG_TESTED=0
  MOCK_EDGE_RELOADED=0

  demo_compose() {
    if [[ "$*" == 'ps -q edge' ]]; then
      if [[ "$MOCK_MOUNT_LAYOUT" == directory ]]; then
        printf 'refreshed-edge\n'
      else
        printf 'stale-edge\n'
      fi
    elif [[ "$*" == 'up -d --no-deps --force-recreate edge' ]]; then
      MOCK_MOUNT_LAYOUT=directory
      ((MOCK_EDGE_RECREATE_COUNT += 1))
    elif [[ "$*" == 'run --rm -T --no-deps --entrypoint nginx edge -c /etc/nginx/demo/nginx.conf -t' ]]; then
      MOCK_EDGE_PRECHECKED=1
    else
      printf 'unexpected demo_compose invocation: %s\n' "$*" >&2
      return 1
    fi
  }
  docker() {
    if [[ "$1" == inspect && "$2" == --format && "$3" == *'.Mounts'* ]]; then
      if [[ "$MOCK_MOUNT_LAYOUT" == directory ]]; then
        printf 'bind|%s/nginx-config|/etc/nginx/demo\n' "$DEMO_SCRIPT_DIR"
      elif [[ "$MOCK_MOUNT_LAYOUT" == legacy ]]; then
        printf 'bind|%s/nginx.conf|/etc/nginx/nginx.conf\n' "$DEMO_SCRIPT_DIR"
      else
        printf 'bind|/unexpected/path|/etc/nginx/other\n'
      fi
    elif [[ "$1" == exec && "$2" == refreshed-edge && "$3" == nginx && "$4" == -c &&
      "$5" == /etc/nginx/demo/nginx.conf && "$6" == -t ]]; then
      MOCK_EDGE_CONFIG_TESTED=1
    elif [[ "$1" == exec && "$2" == refreshed-edge && "$3" == nginx && "$4" == -c &&
      "$5" == /etc/nginx/demo/nginx.conf && "$6" == -s && "$7" == reload ]]; then
      MOCK_EDGE_RELOADED=1
    else
      printf 'unexpected docker invocation: %s\n' "$*" >&2
      return 1
    fi
  }

  EDGE_CONTAINER_ID=
  demo_reload_edge
  [[ "$EDGE_CONTAINER_ID" == refreshed-edge && "$MOCK_MOUNT_LAYOUT" == directory &&
    "$MOCK_EDGE_PRECHECKED" == 1 && "$MOCK_EDGE_RECREATE_COUNT" == 1 && "$MOCK_EDGE_CONFIG_TESTED" == 1 &&
    "$MOCK_EDGE_RELOADED" == 1 ]] || {
    printf 'edge reload did not migrate the legacy single-file mount before validating it\n' >&2
    exit 1
  }
  demo_reload_edge
  [[ "$MOCK_EDGE_RECREATE_COUNT" == 1 ]] || {
    printf 'edge reload recreated a directory-mounted edge instead of reloading it\n' >&2
    exit 1
  }
  MOCK_MOUNT_LAYOUT=unsupported
  if demo_reload_edge >/dev/null 2>&1; then
    printf 'edge reload recreated an unsupported mount layout\n' >&2
    exit 1
  fi
  [[ "$MOCK_EDGE_RECREATE_COUNT" == 1 ]] || {
    printf 'edge reload recreated a container for a non-migration config change\n' >&2
    exit 1
  }
)

python3 - "$SCRIPT_DIR/render-haproxy-config.py" "$SCRIPT_DIR/haproxy.cfg" "$TMP_DIR" <<'PY'
from pathlib import Path
import subprocess
import sys


renderer = Path(sys.argv[1])
template = Path(sys.argv[2])
output_dir = Path(sys.argv[3])
markers = ("@TEAGRAMD_DISABLED@", "@TEAGRAMD_NEXT_DISABLED@")

for active in ("teagramd", "teagramd-next"):
    destination = output_dir / f"haproxy-{active}.cfg"
    subprocess.run(
        [sys.executable, str(renderer), active, str(template), str(destination)],
        check=True,
    )
    rendered = destination.read_text(encoding="utf-8")
    if any(marker in rendered for marker in markers):
        raise SystemExit(f"HAProxy renderer left an unresolved marker for {active}")

    for slot in ("teagramd", "teagramd-next"):
        slot_lines = [
            line.strip()
            for line in rendered.splitlines()
            if line.strip().startswith(f"server {slot} ")
            or line.strip().startswith(f"server {slot}-mtproto ")
        ]
        if len(slot_lines) != 2:
            raise SystemExit(f"HAProxy renderer did not render both {slot} backends")
        should_be_disabled = slot != active
        if any(line.endswith(" disabled") != should_be_disabled for line in slot_lines):
            raise SystemExit(f"HAProxy renderer selected the wrong {slot} backends for {active}")
PY

(
  demo_prepare_client_config_dir() { :; }
  demo_prepare_haproxy_runtime() { :; }
  demo_compose() { cat "$TMP_DIR/target.json"; }
  demo_render_config "$TMP_DIR/production-validated.json"
)

env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
  -u TG_TURNSTILE_SIGNIN_ENABLED \
  -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE \
  TEAGRAM_DEMO_EDGE_BIND=127.0.0.1:18080 \
  "${compose[@]}" config --format json > "$TMP_DIR/validation.json"
jq -e '
  .services.edge.ports | length == 1
  and .[0].host_ip == "127.0.0.1"
  and (.[0].published | tostring) == "18080"
' "$TMP_DIR/validation.json" >/dev/null

env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
  -u TG_TURNSTILE_SIGNIN_ENABLED \
  -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE \
  "${compose[@]}" --file "$SCRIPT_DIR/compose.bootstrap.yaml" \
  --file "$SCRIPT_DIR/compose.seed.yaml" config --format json > "$TMP_DIR/bootstrap.json"
jq -e '
  (.services.teagramd.volumes | map(.target) | sort) == ["/var/lib/teagramd", "/var/lib/teagramd-blobs"]
  and .services.teagramd.environment.TG_BLOB_DIR == "/var/lib/teagramd-blobs"
  and .services.teagramd.environment.HTTP_PROXY == ""
  and .services.teagramd.environment.HTTPS_PROXY == ""
  and (.services.teagramd.depends_on | keys) == ["migrate"]
  and .services.teagramd.environment.TG_CLIENT_ADDR_TRUST == "socket"
  and .services.teagramd.environment.TG_UPLOADS_DISABLED == "false"
  and .services["teagramd-next"].environment.TG_UPLOADS_DISABLED == "false"
  and .services.teagramd.environment.TG_STICKER_IMPORT == "off"
  and .services["teagramd-next"].environment.TG_STICKER_IMPORT == "off"
  and .services.teagramd.environment.TG_CLIENT_ADDR_PROXY_CIDRS == ""
  and .services.teagramd.environment.TG_TURNSTILE_SITE_KEY == ""
  and .services.teagramd.environment.TG_TURNSTILE_SECRET_KEY_FILE == ""
  and .services.teagramd.environment.TG_TURNSTILE_QA_BYPASS_TOKEN_FILE == ""
  and .services["teagramd-next"].environment.TG_TURNSTILE_QA_BYPASS_TOKEN_FILE == ""
  and .services.teagramd.environment.TG_TURNSTILE_SIGNIN_ENABLED == "false"
  and .services["teagramd-next"].environment.TG_TURNSTILE_SIGNIN_ENABLED == "false"
  and .services.teagramd.environment.TG_RATE_LIMIT_SIGN_UP_IP == "32"
  and .services.teagramd.environment.TG_RATE_LIMIT_KEY_EXCHANGE_IP == "256"
  and .services.teagramd.environment.TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET == "256"
  and .services.teagramd.environment.TG_RATE_LIMIT_KEY_EXCHANGE_IP_WINDOW == "1h"
  and .services["teagramd-next"].environment.TG_RATE_LIMIT_KEY_EXCHANGE_IP == "256"
  and .services["teagramd-next"].environment.TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET == "256"
  and .services["teagramd-next"].environment.TG_RATE_LIMIT_KEY_EXCHANGE_IP_WINDOW == "1h"
  and .volumes.tgblobs.driver_opts.device == "/srv/teagram-demo-blobs/data"
' "$TMP_DIR/bootstrap.json" >/dev/null

env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
  -u TG_TURNSTILE_SIGNIN_ENABLED \
  -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE \
  "${compose[@]}" --file "$SCRIPT_DIR/compose.seed.yaml" config --format json > "$TMP_DIR/seed.json"
jq -e '
  .services.teagramd.environment.TG_CLIENT_ADDR_TRUST == "socket"
  and .services.teagramd.environment.TG_UPLOADS_DISABLED == "false"
  and .services["teagramd-next"].environment.TG_UPLOADS_DISABLED == "false"
  and .services.teagramd.environment.TG_STICKER_IMPORT == "off"
  and .services["teagramd-next"].environment.TG_STICKER_IMPORT == "off"
  and .services.teagramd.environment.TG_CLIENT_ADDR_PROXY_CIDRS == ""
  and .services.teagramd.environment.HTTP_PROXY == ""
  and .services.teagramd.environment.HTTPS_PROXY == ""
  and (.services.teagramd.depends_on | keys) == ["migrate"]
  and .services.teagramd.environment.TG_TURNSTILE_SITE_KEY == ""
  and .services.teagramd.environment.TG_TURNSTILE_SECRET_KEY_FILE == ""
  and .services.teagramd.environment.TG_TURNSTILE_QA_BYPASS_TOKEN_FILE == ""
  and .services["teagramd-next"].environment.TG_TURNSTILE_QA_BYPASS_TOKEN_FILE == ""
  and .services.teagramd.environment.TG_TURNSTILE_SIGNIN_ENABLED == "false"
  and .services["teagramd-next"].environment.TG_TURNSTILE_SIGNIN_ENABLED == "false"
  and .services.teagramd.environment.TG_RATE_LIMIT_SIGN_UP_IP == "32"
  and .services.teagramd.environment.TG_RATE_LIMIT_SIGN_UP_IP_WINDOW == "1h"
  and .services.teagramd.environment.TG_RATE_LIMIT_SEND_CODE_IP == "256"
  and .services.teagramd.environment.TG_RATE_LIMIT_SEND_CODE_IP_PHONES == "32"
  and .services.teagramd.environment.TG_RATE_LIMIT_GET_PASSWORD_IP == "256"
  and .services.teagramd.environment.TG_RATE_LIMIT_KEY_EXCHANGE_IP == "256"
  and .services.teagramd.environment.TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET == "256"
  and .services.teagramd.environment.TG_RATE_LIMIT_KEY_EXCHANGE_IP_WINDOW == "1h"
  and .services["teagramd-next"].environment.TG_RATE_LIMIT_KEY_EXCHANGE_IP == "256"
  and .services["teagramd-next"].environment.TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET == "256"
  and .services["teagramd-next"].environment.TG_RATE_LIMIT_KEY_EXCHANGE_IP_WINDOW == "1h"
  and .services.teagramd.environment.TG_RATE_LIMIT_SEND == "0"
' "$TMP_DIR/seed.json" >/dev/null

env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
  -u TG_TURNSTILE_SIGNIN_ENABLED \
  -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE \
  "${compose[@]}" --profile seed --file "$SCRIPT_DIR/compose.seed.yaml" \
  config --format json > "$TMP_DIR/seed-profile.json"
jq -e '
  .services["demo-seed"].profiles == ["seed"]
  and .services["demo-seed"].image == "teagramd:local"
  and .services["demo-seed"].entrypoint == ["/usr/local/bin/demo-seed"]
  and .services["demo-seed"].user == "0:0"
  and .services["demo-seed"].read_only == true
  and .services["demo-seed"].cap_drop == ["ALL"]
  and .services["demo-seed"].security_opt == ["no-new-privileges:true"]
  and (.services["demo-seed"].networks | keys) == ["backend"]
  and (.services["demo-seed"].ports // [] | length) == 0
  and .services["demo-seed"].environment.GOMAXPROCS == "1"
  and .services["demo-seed"].environment.GOMEMLIMIT == "192MiB"
  and (.services["demo-seed"].secrets // [] | length) == 0
  and any(.services["demo-seed"].volumes[]; .target == "/run/teagram-demo/client.json" and .read_only)
  and any(.services["demo-seed"].volumes[]; .target == "/run/secrets/seed-owner-password" and .read_only)
  and any(.services["demo-seed"].volumes[]; .target == "/run/secrets/demo-accounts" and (.read_only // false) == false)
  and .services["demo-probe"].read_only == true
  and (.services["demo-probe"].volumes | length) == 1
  and .services["demo-probe"].volumes[0].target == "/run/teagram-demo/client.json"
  and (.services["demo-probe"].volumes[0].read_only == true)
  and .services["demo-probe"].environment.GOMAXPROCS == "1"
  and .services["demo-probe"].environment.GOMEMLIMIT == "192MiB"
  and (.services["demo-probe"].networks | keys) == ["backend"]
' "$TMP_DIR/seed-profile.json" >/dev/null

env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
  -u TG_TURNSTILE_SIGNIN_ENABLED \
  -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE \
  "${compose[@]}" --profile activity config --format json > "$TMP_DIR/activity-profile.json"
jq -e '
  .services["demo-activity"].profiles == ["activity"]
  and .services["demo-activity"].image == "teagramd:local"
  and .services["demo-activity"].entrypoint == ["/usr/local/bin/demo-seed"]
  and .services["demo-activity"].user == "0:0"
  and .services["demo-activity"].read_only == true
  and .services["demo-activity"].cap_drop == ["ALL"]
  and .services["demo-activity"].security_opt == ["no-new-privileges:true"]
  and (.services["demo-activity"].networks | keys) == ["backend"]
  and (.services["demo-activity"].ports // [] | length) == 0
  and .services["demo-activity"].environment.GOMAXPROCS == "1"
  and .services["demo-activity"].environment.GOMEMLIMIT == "192MiB"
  and (.services["demo-activity"].secrets // [] | length) == 0
  and any(.services["demo-activity"].volumes[]; .target == "/run/teagram-demo/client.json" and .read_only)
  and any(.services["demo-activity"].volumes[]; .target == "/var/lib/teagram-demo-activity" and (.read_only // false) == false)
' "$TMP_DIR/activity-profile.json" >/dev/null

python3 - "$SCRIPT_DIR/teagram-demo-activity.service" \
  "$SCRIPT_DIR/teagram-demo-activity.timer" "$SCRIPT_DIR/install-activity.sh" \
  "$SCRIPT_DIR/run-activity.sh" "$SCRIPT_DIR/wipe-weekly.sh" \
  "$SCRIPT_DIR/teagram-demo-weekly-wipe.service" \
  "$SCRIPT_DIR/activity-credential.sh" \
  "$SCRIPT_DIR/test-activity-credential-handoff.sh" <<'PY'
from pathlib import Path
import sys


service = Path(sys.argv[1]).read_text(encoding="utf-8")
timer = Path(sys.argv[2]).read_text(encoding="utf-8")
installer = Path(sys.argv[3]).read_text(encoding="utf-8")
runner = Path(sys.argv[4]).read_text(encoding="utf-8")
wipe = Path(sys.argv[5]).read_text(encoding="utf-8")
wipe_service = Path(sys.argv[6]).read_text(encoding="utf-8")
credential_helper = Path(sys.argv[7]).read_text(encoding="utf-8")
credential_handoff_test = Path(sys.argv[8]).read_text(encoding="utf-8")


def assert_text_order(text: str, name: str, fragments: list[str]) -> None:
    positions = [text.find(fragment) for fragment in fragments]
    if any(position < 0 for position in positions) or positions != sorted(positions):
        raise SystemExit(f"{name}: safety steps are missing or out of order")


for name, text, fragments in [
    ("activity service", service, [
        "LoadCredential=inventory.json:/var/lib/teagram-demo/secrets/demo-accounts/inventory.json",
        "RuntimeDirectory=teagram-demo-activity-credential",
        "RuntimeDirectoryMode=0700",
        "RuntimeDirectoryPreserve=no",
        "ReadWritePaths=/var/lib/teagram-demo-activity /run/teagram-demo-activity-credential /run/docker.sock /var/run/docker.sock",
        "StateDirectory=teagram-demo-activity",
        "StateDirectoryMode=0700",
        "TimeoutStartSec=infinity",
        "StartLimitBurst=3",
        "StartLimitIntervalSec=1h",
        "ProtectSystem=strict",
    ]),
    ("activity timer", timer, ["OnBootSec=8m", "OnUnitActiveSec=10m", "Persistent=true", "RandomizedDelaySec=5m"]),
    ("activity installer", installer, ["enable --now teagram-demo-activity.timer"]),
    ("activity runner", runner, ["--volume", "demo-activity", "--activity", "--target", "--max-actions-per-hour", "--min-interval"]),
]:
    if any(fragment not in text for fragment in fragments):
        raise SystemExit(f"{name}: missing required activity scheduling or credential setting")
if "PASSWORD" in runner or "CREDENTIALS_DIRECTORY" in runner:
    raise SystemExit("activity runner: credentials must be staged without passing their value or private path")
for fragment in [
    'source "$SCRIPT_DIR/activity-credential.sh"',
    'ACTIVITY_CREDENTIAL_FILE=$(demo_stage_activity_credential)',
    'trap \'demo_cleanup_activity_credential "$ACTIVITY_CREDENTIAL_FILE"\' EXIT',
    '--volume "$ACTIVITY_CREDENTIAL_FILE:/run/secrets/demo-activity-inventory.json:ro"',
]:
    if fragment not in runner:
        raise SystemExit("activity runner: daemon-visible credential staging and cleanup are required")
for fragment in [
    "CREDENTIALS_DIRECTORY",
    "RUNTIME_DIRECTORY",
    "install -o root -g root -m 0400",
    "demo_cleanup_activity_credential",
]:
    if fragment not in credential_helper:
        raise SystemExit("activity credential helper: protected staging and cleanup are required")
for fragment in [
    "--property=ProtectSystem=strict",
    'RuntimeDirectory=$RUNTIME_DIRECTORY_NAME',
    "docker run --rm --network none",
    'source=$ACTIVITY_CREDENTIAL_FILE,target=/inventory,readonly',
    "credential handoff verified",
]:
    if fragment not in credential_handoff_test:
        raise SystemExit("activity credential integration test: real systemd and Docker handoff is required")
if "TimeoutStartSec=60min" not in wipe_service:
    raise SystemExit("weekly wipe service: timeout must cover stopping scheduled activity and reseeding")
assert_text_order(wipe, "wipe-weekly.sh activity gate", [
    "systemctl stop teagram-demo-activity.timer",
    "flock -x -w 20m",
    "dropdb -U postgres --if-exists teagram",
    "systemctl reset-failed teagram-demo-activity.service",
    "systemctl start teagram-demo-activity.timer",
])
PY

python3 "$SCRIPT_DIR/../teagramd/rollout-runner/blob-mode-state.py" compose \
  --checkout "$REPO_ROOT" < "$TMP_DIR/seed.json" > "$TMP_DIR/seed-inventory.json"
python3 "$SCRIPT_DIR/../teagramd/rollout-runner/blob-mode-state.py" compose \
  --checkout "$REPO_ROOT" < "$TMP_DIR/bootstrap.json" > "$TMP_DIR/bootstrap-inventory.json"
jq -e --slurpfile bootstrap "$TMP_DIR/bootstrap-inventory.json" '
  (.services | map(.name)) == ["teagramd", "teagramd-next"]
  and all(.services[]; .initial_local_guard_environment == {
    "TG_REPLICA_COUNT": "1",
    "TG_CLIENT_ADDR_TRUST": "socket"
  })
  and all(.services[]; . as $seed | any($bootstrap[0].services[];
    .name == $seed.name
    and .tg_environment_sha256 == $seed.tg_environment_sha256
    and .network == $seed.network
  ))
' "$TMP_DIR/seed-inventory.json" >/dev/null

grep -Fq 'location = /apiws {' "$SCRIPT_DIR/nginx-config/nginx.conf"
grep -Fq 'root /usr/share/nginx/html/.well-known/teagramd/web/current;' "$SCRIPT_DIR/nginx-config/nginx.conf"
grep -Fq 'location = /teagram-config.json {' "$SCRIPT_DIR/nginx-config/nginx.conf"
grep -Fq 'connect-src '\''self'\'' wss://demo.teagram.io https://challenges.cloudflare.com;' "$SCRIPT_DIR/nginx-config/nginx.conf"
# These patterns match literal Nginx variables.
# shellcheck disable=SC2016
grep -Fq 'set $teagramd http://haproxy:2446;' "$SCRIPT_DIR/nginx-config/nginx.conf"
# shellcheck disable=SC2016
grep -Fq 'proxy_set_header CF-Connecting-IP $http_cf_connecting_ip;' "$SCRIPT_DIR/nginx-config/nginx.conf"
grep -Fq '"CF-Connecting-IP": "192.0.2.1"' "$SCRIPT_DIR/smoke.py"
grep -Fq 'return 200 "Teagram demo server";' "$SCRIPT_DIR/nginx-config/nginx.conf"
grep -Fq 'args.bootstrap' "$SCRIPT_DIR/smoke.py"
grep -Fq 'https://foreign.example", 403' "$SCRIPT_DIR/smoke.py"
grep -Fq 'args.web_source_commit' "$SCRIPT_DIR/smoke.py"
grep -Fq 'base_url + "/"' "$SCRIPT_DIR/smoke.py"
grep -Fq 'location = /admin { return 404; }' "$SCRIPT_DIR/nginx-config/nginx.conf"
grep -Fq 'location ^~ /admin/ { return 404; }' "$SCRIPT_DIR/nginx-config/nginx.conf"
grep -Fq 'fastcgi_temp_path /tmp/fastcgi_temp;' "$SCRIPT_DIR/nginx-config/nginx.conf"
grep -Fq 'uwsgi_temp_path /tmp/uwsgi_temp;' "$SCRIPT_DIR/nginx-config/nginx.conf"
grep -Fq 'scgi_temp_path /tmp/scgi_temp;' "$SCRIPT_DIR/nginx-config/nginx.conf"
grep -Fq 'image: mirror.gcr.io/library/nginx:1.28.0-alpine@sha256:' "$SCRIPT_DIR/compose.yaml"
grep -Fq 'http-request set-src req.hdr_ip(CF-Connecting-IP)' "$SCRIPT_DIR/haproxy.cfg"
grep -Fq 'req.hdr_cnt(CF-Connecting-IP) eq 1' "$SCRIPT_DIR/haproxy.cfg"
grep -Fq 'server teagramd teagramd:2446 check send-proxy-v2' "$SCRIPT_DIR/haproxy.cfg"
grep -Fq 'server teagramd-next teagramd-next:2446 check send-proxy-v2' "$SCRIPT_DIR/haproxy.cfg"
grep -Fq 'frontend mtproto' "$SCRIPT_DIR/haproxy.cfg"
grep -Fq 'bind :2443' "$SCRIPT_DIR/haproxy.cfg"
grep -Fq 'default_backend teagramd_mtproto' "$SCRIPT_DIR/haproxy.cfg"
grep -Fq 'backend teagramd_mtproto' "$SCRIPT_DIR/haproxy.cfg"
grep -Fq 'server teagramd-mtproto teagramd:2443 check send-proxy-v2' "$SCRIPT_DIR/haproxy.cfg"
grep -Fq 'stats socket /run/haproxy/admin.sock mode 600 level admin' "$SCRIPT_DIR/haproxy.cfg"
grep -Fq '@TEAGRAMD_DISABLED@' "$SCRIPT_DIR/haproxy.cfg"
grep -Fq '@TEAGRAMD_NEXT_DISABLED@' "$SCRIPT_DIR/haproxy.cfg"
grep -Fq 'http-check send meth CONNECT uri challenges.cloudflare.com:443' "$SCRIPT_DIR/haproxy.cfg"
grep -Fq 'monitor fail if { nbsrv(turnstile_egress) eq 0 }' "$SCRIPT_DIR/haproxy.cfg"
grep -Fq 'bind :8404' "$SCRIPT_DIR/haproxy.cfg"
grep -Fq 'http-reuse never' "$SCRIPT_DIR/haproxy.cfg"
grep -Fq 'acl allowed_client src 198.18.130.2/32 198.18.130.251/32 198.18.130.253/32' "$SCRIPT_DIR/turnstile-egress.conf"
grep -Fq 'acl siteverify_host dstdomain challenges.cloudflare.com' "$SCRIPT_DIR/turnstile-egress.conf"
grep -Fq 'http_access allow allowed_client CONNECT CONNECT_PORT siteverify_host' "$SCRIPT_DIR/turnstile-egress.conf"
grep -Fq 'http_access deny all' "$SCRIPT_DIR/turnstile-egress.conf"
grep -Fq 'cache deny all' "$SCRIPT_DIR/turnstile-egress.conf"
grep -Fq 'level: ready' "$SCRIPT_DIR/pebble-health.yaml"
grep -Fq 'host: 127.0.0.1' "$SCRIPT_DIR/pebble-health.yaml"
grep -Fq 'port: 3128' "$SCRIPT_DIR/pebble-health.yaml"
grep -Fq 'location = /healthz {' "$SCRIPT_DIR/nginx-config/nginx.conf"
grep -Fq 'https://demo.teagram.io/healthz' "$SCRIPT_DIR/gatus.yaml"
grep -Fq 'base_url + "/healthz"' "$SCRIPT_DIR/smoke.py"
cmp -s "$SCRIPT_DIR/nginx.conf" "$SCRIPT_DIR/nginx-config/nginx.conf" || {
  printf 'legacy ops fingerprint config copy does not match the mounted Nginx config\n' >&2
  exit 1
}

python3 - "$SCRIPT_DIR/wipe-weekly.sh" "$SCRIPT_DIR/init-demo.sh" \
  "$SCRIPT_DIR/deploy-main.sh" "$SCRIPT_DIR/demo-common.sh" \
  "$SCRIPT_DIR/forced-command.sh" "$SCRIPT_DIR/test-seed-compose.sh" <<'PY'
from pathlib import Path
import sys


def assert_text_order(text: str, name: str, fragments: list[str], start: int = 0) -> None:
    positions = [text.find(fragment, start) for fragment in fragments]
    if any(position < 0 for position in positions) or positions != sorted(positions):
        raise SystemExit(f"{name}: safety steps are missing or out of order")


def assert_order(path: Path, fragments: list[str], start: int = 0) -> None:
    assert_text_order(path.read_text(encoding="utf-8"), path.name, fragments, start)


rate_limit_reset_command = "demo_compose exec -T postgres psql -v ON_ERROR_STOP=1 -U postgres -d teagram"
rate_limit_reset_sql = "-c 'DELETE FROM rate_limits; DELETE FROM send_code_ip_calls; DELETE FROM send_code_ip_phones; DELETE FROM public_send_code_ip_calls; DELETE FROM public_send_code_ip_phones;'"
rate_limit_reset = rate_limit_reset_command + " \\\n  " + rate_limit_reset_sql
assert_order(Path(sys.argv[1]), [
    'demo_compose stop edge haproxy "$ACTIVE_SERVER" turnstile-egress',
    "demo_prune_deploy_backups\n",
    "dropdb -U postgres --if-exists teagram",
    'demo_seed_compose up -d "$ACTIVE_SERVER"',
    "demo_prepare_seed_owner \"$TMP_DIR\"",
    '"$SEED_SCRIPT" --seed',
    '"$SEED_SCRIPT" --verify-seed --endpoint "$ACTIVE_ENDPOINT"',
    '"$SCRIPT_DIR/test-seed-compose.sh" "$BEFORE_GENERATION"',
    rate_limit_reset,
    'demo_compose up -d --wait --wait-timeout 120 "$ACTIVE_SERVER" haproxy',
    '"$SEED_SCRIPT" --verify-turnstile --endpoint haproxy:2443',
    "demo_compose up -d edge",
])
seed_test = Path(sys.argv[6])
assert_order(seed_test, [
    'before=$(capture_seed_report)',
    '"$SEED_SCRIPT" --seed --endpoint "$ACTIVE_ENDPOINT"',
    'after=$(capture_seed_report)',
    '[[ "$before" == "$after" ]]',
])
assert_order(seed_test, [
    'before_generation=$(capture_wipe_generation)',
    'after_generation=$(capture_wipe_generation)',
    '[[ "$after_generation" == "$before_generation" ]]',
    'demo_seed_compose up -d --force-recreate --wait --wait-timeout 120 "$ACTIVE_SERVER"',
    'after_restart_generation=$(capture_wipe_generation)',
    '[[ "$after_restart_generation" == "$before_generation" ]]',
])
seed_test_text = seed_test.read_text(encoding="utf-8")
if "markers=133" not in seed_test_text or "search_account=demo_searcher" not in seed_test_text:
    raise SystemExit("test-seed-compose.sh: API convergence check must assert stable marker counts and search account")
deploy_path = Path(sys.argv[3])
deploy = deploy_path.read_text(encoding="utf-8")
cleanup = deploy.split("demo_cleanup() {", 1)[1].split("\n}", 1)[0]
if 'cat-file -e \\\n      "$PREV_SHA:deploy/teagram-demo/nginx-config/nginx.conf"' not in cleanup:
    raise SystemExit("deploy-main.sh: rollback must distinguish old directory and file mounts")
if "demo_restore_legacy_edge_after_rollback || edge_rollback_rc=1" not in cleanup:
    raise SystemExit("deploy-main.sh: legacy edge restore must run before the wrapper returns")
if "demo_reload_edge" not in cleanup:
    raise SystemExit("deploy-main.sh: directory-mounted edge rollback must reload gracefully")
forced_command = Path(sys.argv[5]).read_text(encoding="utf-8")
if "demo_restore_legacy_edge_after_rollback" in forced_command:
    raise SystemExit("forced-command.sh: first migration recovery must run before the old wrapper resets checkout")
if 'bash "$REPO/deploy/teagram-demo/deploy-main.sh"' not in forced_command:
    raise SystemExit("forced-command.sh: server deployment must invoke the checked deployment rollback")
init = Path(sys.argv[2]).read_text(encoding="utf-8")
assert_text_order(init, "init-demo.sh image release", [
    'IMAGE_REF=$(demo_pull_image_by_sha "$SHA" "$IMAGE_DIGEST")',
    'export TEAGRAM_DEMO_IMAGE_REF="$IMAGE_REF"',
    'demo_write_active_release "$ACTIVE_SERVER" "$IMAGE_REF"',
    'demo_compose run --rm --no-deps teagramd bootstrap-identity',
])
if '[[ "$#" -ne 1 || ! "${1:-}" =~ ^sha256:[0-9a-f]{64}$ ]]' not in init:
    raise SystemExit("init-demo.sh: first install must require the digest copied from the selected main CI run")
deploy_preflight = deploy.split('TARGET_RENDER="$TMP_DIR/target-render.json"', 1)[1].split(
    'docker inspect "${CONTAINER_IDS[@]}"', 1
)[0]
assert_text_order(deploy_preflight, "deploy-main.sh proxy preflight", [
    'mapfile -t CONTAINER_IDS < <(demo_compose ps -q)',
    'demo_require_proxy_configuration_current haproxy "$TEAGRAM_DEMO_HAPROXY_CONFIG_SHA256"',
    'demo_require_proxy_configuration_current edge "" 1',
    'demo_reload_edge',
    'demo_render_config "$TARGET_RENDER"',
])
deploy_edge_refresh = deploy.split('EDGE_CONFIG_RELOAD_ATTEMPTED=1\ndemo_reload_edge\n', 1)[1]
assert_text_order(deploy_edge_refresh, "deploy-main.sh reloaded edge inventory", [
    'mapfile -t CONTAINER_IDS < <(demo_compose ps -q)',
    'demo_prepare_client_config_dir',
    'docker inspect "${CONTAINER_IDS[@]}"',
])
deploy_steps = [
    'CANDIDATE_IMAGE_REF=$(demo_pull_image_by_sha "$SHA" "$IMAGE_DIGEST")',
    "demo_backup_database\n",
    'export TEAGRAM_DEMO_IMAGE_REF="$CANDIDATE_IMAGE_REF"',
    'demo_compose run --rm --no-deps -e TG_ADVERTISE_DEMO_GUEST_SIGN_UP=false "$CANDIDATE_SERVER" client-config > "$TMP_DIR/client-before-migration.json"',
    'CANDIDATE_SPKI=$(jq -er',
    '[[ "$CANDIDATE_SPKI" == "$OLD_SPKI" ]]',
    "demo_compose up -d migrate",
    "demo_wait_migration demo_compose 'deployment migrations'",
    'demo_compose run --rm --no-deps "$CANDIDATE_SERVER" client-config > "$TMP_DIR/client.json"',
    'demo_seed_compose up -d "$CANDIDATE_SERVER"',
    '"$SCRIPT_DIR/seed-demo.sh" --verify-seed --endpoint "$CANDIDATE_ENDPOINT"',
    'demo_compose up -d --wait --wait-timeout 120 "$CANDIDATE_SERVER"',
    'demo_haproxy_control enable "$CANDIDATE_SERVER"',
    'demo_haproxy_control wait-healthy "$CANDIDATE_SERVER" --timeout 60',
    'demo_haproxy_control drain "$ACTIVE_SERVER"',
    'install -o root -g root -m 0644 "$TMP_DIR/client.json" "$DEMO_CLIENT_CONFIG.next"',
    '--web-artifact-digest "sha256:${PREV_WEB_RELEASE#* }"',
    '"$SCRIPT_DIR/seed-demo.sh" --verify-turnstile --endpoint haproxy:2443',
    'demo_write_active_release "$CANDIDATE_SERVER" "$CANDIDATE_IMAGE_REF"',
    'demo_compose stop "$ACTIVE_SERVER"',
]
deploy_start = deploy.find(deploy_steps[0])
deploy_positions = [deploy.find(step, deploy_start) for step in deploy_steps]
if any(position < 0 for position in deploy_positions) or deploy_positions != sorted(deploy_positions):
    raise SystemExit("deploy-main.sh: candidate health and traffic cutover must precede stopping the old server")
if "PUBLIC_ACTIVATION_STARTED=1" not in deploy or "migrated database preserved" not in deploy:
    raise SystemExit("deploy-main.sh: failed cutover must retain the migrated database and dump")
if 'demo_compose stop edge' in deploy or 'demo_compose stop haproxy' in deploy:
    raise SystemExit("deploy-main.sh: normal deploy must keep the public edge and HAProxy running")
if 'python3 "$SCRIPT_DIR/smoke.py" --bootstrap "$DEMO_CLIENT_CONFIG"' not in deploy:
    raise SystemExit("deploy-main.sh: server-only handoff must smoke the bootstrap status only when no web artifact exists")
backup_function = deploy.split("demo_backup_database() {", 1)[1].split("\n}", 1)[0]
assert_text_order(backup_function, "deploy-main.sh backup function", [
    'DB_DUMP="$BACKUP_DIR/postgres.dump"',
    "demo_compose exec -T postgres pg_dump -U postgres -Fc teagram",
    "demo_compose_input exec -T postgres pg_restore --list",
    'BACKUP_READY=1',
])
assert_order(deploy_path, [
    'CANDIDATE_IMAGE_REF=$(demo_pull_image_by_sha "$SHA" "$IMAGE_DIGEST")',
    "demo_backup_database\n",
    'export TEAGRAM_DEMO_IMAGE_REF="$CANDIDATE_IMAGE_REF"',
    "demo_compose up -d migrate",
    "demo_wait_migration demo_compose 'deployment migrations'",
    'demo_seed_compose up -d "$CANDIDATE_SERVER"',
    '"$SCRIPT_DIR/seed-demo.sh" --verify-seed --endpoint "$CANDIDATE_ENDPOINT"',
    'demo_compose up -d --wait --wait-timeout 120 "$CANDIDATE_SERVER"',
    'demo_compose stop "$ACTIVE_SERVER"',
])
if 'docker image tag "$PREV_IMAGE_ID" teagramd:local' in deploy:
    raise SystemExit("deploy-main.sh: rollback must use the persisted previous image digest, not a mutable tag")
if 'demo_write_active_release "$ACTIVE_SERVER" "$PREV_IMAGE_REF"' not in deploy:
    raise SystemExit("deploy-main.sh: confirmed rollback must restore the previous slot and image digest")
if "MIGRATE_STATUS=$(docker inspect" in deploy:
    raise SystemExit("deploy-main.sh: detached migration status must be checked after waiting")
if "PREV_IMAGE_TAG" in deploy or "teagramd:rollback-" in deploy:
    raise SystemExit("deploy-main.sh: rollback image must not get a persistent unique tag")
if 'demo_pull_image_by_sha "$SHA" "$IMAGE_DIGEST"' not in deploy or 'previous-image-ref.txt' not in deploy:
    raise SystemExit("deploy-main.sh: deployment must pull the trusted candidate digest while recording the previous image reference")
if '[[ "$IMAGE_DIGEST" =~ ^sha256:[0-9a-f]{64}$ ]]' not in deploy:
    raise SystemExit("deploy-main.sh: deployment must validate the trusted CI image digest")
if "compose build" in deploy or "compose build" in init:
    raise SystemExit("demo install and deploy must never build the server image in the LXC")
waiter = Path(sys.argv[4]).read_text(encoding="utf-8")
if "demo_wait_migration()" not in waiter or 'docker wait "${migration_ids[0]}"' not in waiter:
    raise SystemExit("demo-common.sh: migration helper must wait for the container exit code")
if 'install -d -o 99 -g 99 -m 0700 "$DEMO_STATE_DIR/haproxy-run"' not in waiter:
    raise SystemExit("demo-common.sh: HAProxy runtime socket directory must be writable only by the image's haproxy user")
forced_command = Path(sys.argv[5]).read_text(encoding="utf-8")
wipe = Path(sys.argv[1]).read_text(encoding="utf-8")
if 'LOCK_PATH="$DEMO_REPO_ROOT/.state/deploy.lock"' not in wipe:
    raise SystemExit("wipe-weekly.sh: deploy and wipe must share the deploy lock")
assert_text_order(wipe, "wipe-weekly.sh wipe generation publication", [
    'BEFORE_GENERATION=$(jq -er',
    'dropdb -U postgres --if-exists teagram',
    "demo_wait_migration demo_compose 'post-reset migrations'",
    'client-config > "$TMP_DIR/client-after-wipe.json"',
    'AFTER_GENERATION=$(jq -er',
    '[[ "$AFTER_GENERATION" != "$BEFORE_GENERATION" ]]',
    'install -o root -g root -m 0644 "$TMP_DIR/client-after-wipe.json" "$DEMO_CLIENT_CONFIG.next"',
    'mv -f -- "$DEMO_CLIENT_CONFIG.next" "$DEMO_CLIENT_CONFIG"',
    'demo_compose up -d edge',
])
assert_text_order(init, "init-demo.sh resume client config after migrations", [
    "demo_wait_migration demo_bootstrap_compose 'resume migrations'",
    'client-config > "$TMP_DIR/client.json"',
    'mv -f -- "$DEMO_CLIENT_CONFIG.next" "$DEMO_CLIENT_CONFIG"',
])
bootstrap_wait = init.index("demo_wait_migration demo_bootstrap_compose 'bootstrap migrations'")
assert_text_order(init, "init-demo.sh fresh client config after migrations", [
    "demo_wait_migration demo_bootstrap_compose 'bootstrap migrations'",
    'client-config > "$TMP_DIR/client.json"',
    'mv -f -- "$DEMO_CLIENT_CONFIG.next" "$DEMO_CLIENT_CONFIG"',
], start=bootstrap_wait)
assert_text_order(deploy, "deploy-main.sh client config after migrations", [
    "demo_wait_migration demo_compose 'deployment migrations'",
    'client-config > "$TMP_DIR/client.json"',
    'and .demo.guest_sign_up == true',
    'NEW_GENERATION=$(jq -er',
    'OLD_GENERATION=$(jq -er',
])
if 'capture_wipe_generation()' not in seed_test.read_text(encoding="utf-8"):
    raise SystemExit("test-seed-compose.sh: must verify the stored wipe generation remains stable during seed convergence")
assert_text_order(forced_command, "forced-command.sh deploy lock", [
    "unset DEMO_DEPLOY_LOCK_HELD",
    'readonly IMAGE_DIGEST=${BASH_REMATCH[2]}',
    'LOCK_PATH="$REPO/.state/deploy.lock"',
    'exec 9> "$LOCK_PATH"',
    "flock -x 9",
    'git -C "$REPO" fetch',
    'git -C "$REPO" reset --hard "$SHA"',
    'DEMO_DEPLOY_LOCK_HELD=1 DEPLOY_SHA="$SHA" DEPLOY_IMAGE_DIGEST="$IMAGE_DIGEST" PREVIOUS_SHA="$previous"',
    'git -C "$REPO" reset --hard "$previous"',
])
assert_order(deploy_path, [
    'LOCK_PATH="$DEMO_REPO_ROOT/.state/deploy.lock"',
    'if [[ "${DEMO_DEPLOY_LOCK_HELD:-0}" != 1 ]]; then',
    "flock -x 9",
    'git -C "$DEMO_REPO_ROOT" fetch',
])
init = Path(sys.argv[2]).read_text(encoding="utf-8")
cleanup = init.split("cleanup() {", 1)[1].split("\n}", 1)[0]
if "rc != 0" not in cleanup or "PUBLIC_SERVICES_MAY_BE_RUNNING == 1" not in cleanup:
    raise SystemExit("init-demo.sh: failed install must close a possibly public stack")
if "demo_compose stop edge haproxy" not in cleanup:
    raise SystemExit("init-demo.sh: failed install cleanup must stop edge and HAProxy")
assert_text_order(init, "init-demo.sh resume lock", [
    "demo_lock_init() {",
    'LOCK_PATH="$DEMO_REPO_ROOT/.state/deploy.lock"',
    "flock -x 9",
    "demo_lock_init\n",
    'MODE_FILE="$DEMO_AUTHORITY_DIR/mode.json"',
    'if [[ -e "$MODE_FILE" || -L "$MODE_FILE" ]]; then',
])
if '  --lock-path "$LOCK_PATH"' not in init:
    raise SystemExit("init-demo.sh: fresh install must pass its acquired lock path to authority initialization")
assert_order(Path(sys.argv[2]), [
    "PUBLIC_SERVICES_MAY_BE_RUNNING=1",
    'demo_compose up -d --wait --wait-timeout 120 "$ACTIVE_SERVER" haproxy',
    '"$SEED_SCRIPT" --verify-turnstile --endpoint haproxy:2443',
    "demo_compose up -d edge",
    'python3 "$SCRIPT_DIR/smoke.py" --bootstrap "$DEMO_CLIENT_CONFIG"',
    "exit 0",
])
assert_order(Path(sys.argv[2]), [
    "PUBLIC_SERVICES_MAY_BE_RUNNING=1",
    'demo_compose up -d --wait --wait-timeout 120 "$ACTIVE_SERVER" haproxy',
    '"$SEED_SCRIPT" --verify-turnstile --endpoint haproxy:2443',
    "demo_compose up -d edge",
    'python3 "$SCRIPT_DIR/smoke.py" --bootstrap "$DEMO_CLIENT_CONFIG"',
], start=init.index("exit 0") + len("exit 0"))
assert_order(Path(sys.argv[2]), [
    'demo_compose stop edge haproxy turnstile-egress',
    'python3 "$MODE_HELPER" validate',
    'IMAGE_REF=$(demo_pull_image_by_sha "$SHA" "$IMAGE_DIGEST")',
    'demo_compose down --remove-orphans',
    'demo_bootstrap_compose up -d migrate',
    "demo_wait_migration demo_bootstrap_compose 'resume migrations'",
    'demo_write_active_release "$ACTIVE_SERVER" "$IMAGE_REF"',
    'demo_seed_compose up -d "$ACTIVE_SERVER"',
    '"$SEED_SCRIPT" --seed',
    '"$SEED_SCRIPT" --verify-seed --endpoint "$ACTIVE_ENDPOINT"',
    '"$SCRIPT_DIR/test-seed-compose.sh"',
    'demo_compose up -d --wait --wait-timeout 120 "$ACTIVE_SERVER" haproxy',
    '"$SEED_SCRIPT" --verify-turnstile --endpoint haproxy:2443',
    "demo_compose up -d edge",
])
resume_path = init.split('if [[ -e "$MODE_FILE" || -L "$MODE_FILE" ]]; then\n', 1)[1].split(
    '\nfi\n[[ "$ACTIVE_SERVER"', 1
)[0]
assert_text_order(resume_path, "init-demo.sh resume rate limit reset", [
    '"$SCRIPT_DIR/test-seed-compose.sh"',
    rate_limit_reset_command,
    rate_limit_reset_sql,
    '"$SEED_SCRIPT" --verify-turnstile --endpoint haproxy:2443',
])
fresh_path = init.split('[[ "$ACTIVE_SERVER" == teagramd ]] || demo_fail', 1)[1]
assert_text_order(fresh_path, "init-demo.sh fresh rate limit reset", [
    '"$SCRIPT_DIR/test-seed-compose.sh"',
    rate_limit_reset_command,
    rate_limit_reset_sql,
    '"$SEED_SCRIPT" --verify-turnstile --endpoint haproxy:2443',
])
if 'demo_compose down --volumes' in resume_path or 'demo_compose down -v' in resume_path:
    raise SystemExit("init-demo.sh: resume network recreation must preserve named volumes")
if "demo_wait_migration demo_bootstrap_compose 'bootstrap migrations'" not in init:
    raise SystemExit("init-demo.sh: bootstrap must wait for migration completion")
if "MIGRATE_STATUS=$(docker inspect" in init:
    raise SystemExit("init-demo.sh: detached bootstrap migration status must be checked after waiting")
if "demo_wait_migration demo_compose 'post-reset migrations'" not in Path(sys.argv[1]).read_text(encoding="utf-8"):
    raise SystemExit("wipe-weekly.sh: reset must wait for migration completion")
if "demo_prune_deploy_backups()" not in Path(sys.argv[1]).read_text(encoding="utf-8"):
    raise SystemExit("wipe-weekly.sh: weekly reset must remove old deploy snapshots")
assert_text_order(wipe, "wipe-weekly.sh persistent identity check", [
    'demo_compose up -d edge',
    'python3 "$SCRIPT_DIR/smoke.py" "$CLIENT_CONFIG"',
    'demo_compose exec -T "$ACTIVE_SERVER" /usr/local/bin/teagramd client-config > "$TMP_DIR/client-active-after-wipe.json"',
    "ACTIVE_SPKI=$(jq -er '.mtproto.rsa_spki' \"$TMP_DIR/client-active-after-wipe.json\")",
    '[[ "$ACTIVE_SPKI" == "$BEFORE_SPKI" ]]',
    "ACTIVE_GENERATION=$(jq -er '.demo.wipe_generation' \"$TMP_DIR/client-active-after-wipe.json\")",
    '[[ "$ACTIVE_GENERATION" == "$AFTER_GENERATION" ]]',
])
PY

INIT_LOCK_FUNCTION="$TMP_DIR/init-lock-function.sh"
sed -n '/^demo_lock_init() {$/,/^}/p' "$SCRIPT_DIR/init-demo.sh" > "$INIT_LOCK_FUNCTION"
cat > "$TMP_DIR/init-resume-lock-regression.sh" <<'BASH'
set -Eeuo pipefail
source "$INIT_LOCK_FUNCTION"
DEMO_REPO_ROOT="$TMP_DIR/resume-lock-repo"
LOCK_PATH="$DEMO_REPO_ROOT/.state/deploy.lock"
mkdir -p "$(dirname "$LOCK_PATH")"
exec 8> "$LOCK_PATH"
flock -x 8
(
  exec 8>&-
  : > "$TMP_DIR/resume-attempted"
  demo_lock_init
  : > "$TMP_DIR/resume-continued"
) &
resume_pid=$!
for attempt in {1..100}; do
  [[ -e "$TMP_DIR/resume-attempted" ]] && break
  sleep 0.01
done
[[ -e "$TMP_DIR/resume-attempted" ]] || {
  printf '%s\n' 'resume lock test did not start its child' >&2
  kill "$resume_pid" 2>/dev/null || true
  exit 1
}
sleep 0.1
[[ ! -e "$TMP_DIR/resume-continued" ]] || {
  printf '%s\n' 'resume continued while the wipe lock was held' >&2
  exit 1
}
flock -u 8
wait "$resume_pid"
[[ -e "$TMP_DIR/resume-continued" ]]
BASH
env \
  INIT_LOCK_FUNCTION="$INIT_LOCK_FUNCTION" \
  TMP_DIR="$TMP_DIR" \
  bash "$TMP_DIR/init-resume-lock-regression.sh"

AUTHORITY_INIT_BLOCK="$TMP_DIR/init-authority-publication.sh"
# The sed range intentionally matches literal shell variables.
# shellcheck disable=SC2016
sed -n '/^LOCK_PATH=$(realpath -m "$LOCK_PATH")$/,/^  --lock-path "$LOCK_PATH"$/p' \
  "$SCRIPT_DIR/init-demo.sh" > "$AUTHORITY_INIT_BLOCK"
cat > "$TMP_DIR/init-fresh-authority-regression.sh" <<'BASH'
set -Eeuo pipefail
source "$INIT_LOCK_FUNCTION"
DEMO_REPO_ROOT="$TMP_DIR/fresh-install-repo"
unset LOCK_PATH
demo_lock_init

MODE_HELPER="$TMP_DIR/blob-mode-state.py"
DEMO_AUTHORITY_DIR="$TMP_DIR/fresh-authority"
DEMO_STATE_DIR="$TMP_DIR/fresh-state"
TMP_DIR="$TMP_DIR/fresh-install-tmp"
INITIAL_INVENTORY="$TMP_DIR/target-inventory.json"
TARGET_ARTIFACT_SHA=fixture-artifact-sha
SCRIPT_DIR="$TMP_DIR/teagram-demo"
SHA=0123456789abcdef0123456789abcdef01234567
AUTHORITY_CALLS="$TMP_DIR/authority-calls"
mkdir -p "$TMP_DIR"

python3() {
  printf '%s\n' "$*" >> "$AUTHORITY_CALLS"
}
source "$AUTHORITY_INIT_BLOCK"

EXPECTED_LOCK_PATH=$(realpath -m "$DEMO_REPO_ROOT/.state/deploy.lock")
grep -Fq 'initialize-local' "$AUTHORITY_CALLS"
grep -Fq -- "--lock-path $EXPECTED_LOCK_PATH" "$AUTHORITY_CALLS"
[[ "$LOCK_PATH" == "$EXPECTED_LOCK_PATH" ]]
BASH
env INIT_LOCK_FUNCTION="$INIT_LOCK_FUNCTION" AUTHORITY_INIT_BLOCK="$AUTHORITY_INIT_BLOCK" TMP_DIR="$TMP_DIR" \
  bash "$TMP_DIR/init-fresh-authority-regression.sh"

PRUNE_FUNCTION="$TMP_DIR/demo-prune-backups.sh"
sed -n '/^demo_prune_deploy_backups() {$/,/^}/p' "$SCRIPT_DIR/wipe-weekly.sh" > "$PRUNE_FUNCTION"
cat > "$TMP_DIR/backup-prune-regression.sh" <<'BASH'
set -Eeuo pipefail
source "$PRUNE_FUNCTION"
BACKUP_ROOT="$DEMO_STATE_DIR/backups"
SENTINEL_DIR="$TMP_DIR/retention-outside"
mkdir -p "$BACKUP_ROOT/deploy-old" "$SENTINEL_DIR"
printf 'old user records\n' > "$BACKUP_ROOT/deploy-old/postgres.dump"
printf 'outside state\n' > "$SENTINEL_DIR/keep"
ln -s "$SENTINEL_DIR" "$BACKUP_ROOT/external-link"
demo_fail() { return 1; }
chown() { :; }
chmod() { :; }
demo_prune_deploy_backups
[[ -d "$BACKUP_ROOT" ]]
[[ -z "$(find "$BACKUP_ROOT" -mindepth 1 -maxdepth 1 -print -quit)" ]]
[[ -f "$SENTINEL_DIR/keep" ]]
rm -rf -- "$BACKUP_ROOT"
ln -s "$SENTINEL_DIR" "$BACKUP_ROOT"
if demo_prune_deploy_backups; then
  printf '%s\n' 'backup prune test followed a symlinked backup root' >&2
  exit 1
fi
[[ -f "$SENTINEL_DIR/keep" ]]
BASH
env \
  DEMO_STATE_DIR="$TMP_DIR/retention-state" \
  TMP_DIR="$TMP_DIR" \
  PRUNE_FUNCTION="$PRUNE_FUNCTION" \
  bash "$TMP_DIR/backup-prune-regression.sh"

IMAGE_CLEANUP_FUNCTION="$TMP_DIR/demo-remove-previous-image.sh"
sed -n \
  -e '/^demo_cleanup() {$/,/^}/p' \
  -e '/^demo_remove_previous_image() {$/,/^}/p' \
  "$SCRIPT_DIR/deploy-main.sh" > "$IMAGE_CLEANUP_FUNCTION"
cat > "$TMP_DIR/deploy-image-cleanup-regression.sh" <<'BASH'
set -Eeuo pipefail
source "$IMAGE_CLEANUP_FUNCTION"
DOCKER_CALLS="$TMP_DIR/docker-image-calls"
docker() { printf '%s\n' "$*" >> "$DOCKER_CALLS"; }
demo_remove_previous_image sha256:previous sha256:active
[[ "$(cat "$DOCKER_CALLS")" == 'image rm sha256:previous' ]]
: > "$DOCKER_CALLS"
demo_remove_previous_image sha256:active sha256:active
[[ ! -s "$DOCKER_CALLS" ]]
BASH
env \
  IMAGE_CLEANUP_FUNCTION="$IMAGE_CLEANUP_FUNCTION" \
  TMP_DIR="$TMP_DIR" \
  bash "$TMP_DIR/deploy-image-cleanup-regression.sh"

LEGACY_EDGE_ROLLBACK_FUNCTION="$TMP_DIR/demo-restore-legacy-edge.sh"
sed -n '/^demo_restore_legacy_edge_after_rollback() {$/,/^}/p' \
  "$SCRIPT_DIR/deploy-main.sh" > "$LEGACY_EDGE_ROLLBACK_FUNCTION"
cat > "$TMP_DIR/demo-restore-legacy-edge-regression.sh" <<'BASH'
set -Eeuo pipefail
source "$DEMO_COMMON"
REPO="$TEST_REPO"
SCRIPT_DIR="$REPO/deploy/teagram-demo"
DEMO_SCRIPT_DIR="$SCRIPT_DIR"
DEMO_REPO_ROOT="$REPO"
DEMO_ENV_FILE="$SCRIPT_DIR/.env"
TMP_DIR="$TEST_ROOT/tmp"
PREV_SHA=1111111111111111111111111111111111111111
MOCK_EDGE_STATE="$TEST_ROOT/edge-state"
MOCK_RECREATE_COUNT_FILE="$TEST_ROOT/recreate-count"
export MOCK_EDGE_STATE MOCK_RECREATE_COUNT_FILE
mkdir -p "$TMP_DIR" "$SCRIPT_DIR/nginx-config"
printf '%s\n' 'legacy nginx config fixture' > "$SCRIPT_DIR/nginx.conf"
printf '%s\n' 'global' > "$SCRIPT_DIR/haproxy.cfg"
printf '%s\n' 'print("fixture")' > "$SCRIPT_DIR/render-haproxy-config.py"
printf '%s\n' 'POSTGRES_PASSWORD=fixture' > "$DEMO_ENV_FILE"
printf '%s\n' directory > "$MOCK_EDGE_STATE"
printf '0\n' > "$MOCK_RECREATE_COUNT_FILE"

source "$LEGACY_EDGE_ROLLBACK_FUNCTION"
demo_active_image_ref() { printf '%s\n' 'teagramd:previous'; }
demo_compose() {
  [[ "$*" == 'ps -a -q edge' ]] || return 1
  printf 'old-edge\n'
}
demo_edge_uses_legacy_file_mount() {
  local mounts
  mounts=$(docker inspect --format ignored "$1") || return 1
  grep -Fqx -- "bind|$DEMO_SCRIPT_DIR/nginx.conf|/etc/nginx/nginx.conf" <<< "$mounts"
}
chown() { return 0; }
git() {
  if [[ "$1" == -C && "$2" == "$DEMO_REPO_ROOT" && "$3" == cat-file ]]; then
    return 1
  fi
  if [[ "$1" == -C && "$2" == "$DEMO_REPO_ROOT" && "$3" == show ]]; then
    case "$4" in
      "$PREV_SHA:deploy/teagram-demo/compose.yaml")
        printf '%s\n' 'source: ./deploy/teagram-demo/nginx.conf' \
          'target: /etc/nginx/nginx.conf'
        ;;
      "$PREV_SHA:deploy/teagram-demo/nginx.conf")
        printf '%s\n' 'legacy nginx config fixture'
        ;;
      "$PREV_SHA:deploy/teagram-demo/haproxy.cfg")
        cat "$SCRIPT_DIR/haproxy.cfg"
        ;;
      "$PREV_SHA:deploy/teagram-demo/render-haproxy-config.py")
        cat "$SCRIPT_DIR/render-haproxy-config.py"
        ;;
      *) return 1 ;;
    esac
    return
  fi
  return 1
}
docker() {
  if [[ "$1" == ps && "$2" == -q ]]; then
    printf 'restored-edge\n'
  elif [[ "$1" == inspect && "$2" == --format ]]; then
    if [[ "$(<"$MOCK_EDGE_STATE")" == directory ]]; then
      printf 'bind|%s/nginx-config|/etc/nginx/demo\n' "$DEMO_SCRIPT_DIR"
    else
      printf 'bind|%s/nginx.conf|/etc/nginx/nginx.conf\n' "$DEMO_SCRIPT_DIR"
    fi
  elif [[ "$1" == exec && ( "$2" == old-edge || "$2" == restored-edge ) &&
    "$3" == nginx && "$4" == -t ]]; then
    printf 'tested\n' >> "$TEST_ROOT/nginx-test"
  else
    printf 'unexpected docker invocation: %s\n' "$*" >&2
    return 1
  fi
}
demo_restore_legacy_edge_after_rollback
[[ "$(<"$MOCK_EDGE_STATE")" == legacy ]]
[[ "$(<"$MOCK_RECREATE_COUNT_FILE")" == 1 ]]
[[ "$(<"$TEST_ROOT/nginx-test")" == tested ]]
printf '%s\n' 'new checkout config' > "$SCRIPT_DIR/nginx.conf"
demo_restore_legacy_edge_after_rollback
[[ "$(<"$MOCK_RECREATE_COUNT_FILE")" == 2 ]]
demo_restore_legacy_edge_after_rollback
[[ "$(<"$MOCK_RECREATE_COUNT_FILE")" == 2 ]]
BASH
mkdir -p "$TMP_DIR/legacy-edge-bin"
cat > "$TMP_DIR/legacy-edge-bin/docker" <<'BASH'
#!/usr/bin/env bash
set -Eeuo pipefail
if [[ "$1" == compose && "$*" == *' up -d --no-deps --force-recreate edge' ]]; then
  [[ "$TEAGRAM_DEMO_EDGE_CONFIG_SHA256" =~ ^[0-9a-f]{64}$ ]]
  [[ "$TEAGRAM_DEMO_HAPROXY_CONFIG_SHA256" =~ ^[0-9a-f]{64}$ ]]
  [[ "$TEAGRAM_DEMO_IMAGE_REF" == 'teagramd:previous' ]]
  printf '%s\n' legacy > "$MOCK_EDGE_STATE"
  count=$(<"$MOCK_RECREATE_COUNT_FILE")
  printf '%s\n' "$((count + 1))" > "$MOCK_RECREATE_COUNT_FILE"
elif [[ "$1" == ps ]]; then
  printf 'restored-edge\n'
else
  printf 'unexpected docker executable invocation: %s\n' "$*" >&2
  exit 1
fi
BASH
chmod +x "$TMP_DIR/legacy-edge-bin/docker"
env \
  DEMO_COMMON="$SCRIPT_DIR/demo-common.sh" \
  LEGACY_EDGE_ROLLBACK_FUNCTION="$LEGACY_EDGE_ROLLBACK_FUNCTION" \
  TEST_REPO="$TMP_DIR/legacy-demo-checkout" \
  TEST_ROOT="$TMP_DIR/legacy-edge-test" \
  PATH="$TMP_DIR/legacy-edge-bin:$PATH" \
  bash "$TMP_DIR/demo-restore-legacy-edge-regression.sh"


cat > "$TMP_DIR/deploy-rollback-failure-regression.sh" <<'BASH'
set -Eeuo pipefail
source "$CLEANUP_FUNCTION"
ACTIVE_SERVER=teagramd
CANDIDATE_SERVER=teagramd-next
BACKUP_READY=1
BACKUP_DIR="$TEST_ROOT/backup"
CANDIDATE_STARTED=1
CLIENT_CONFIG_UPDATED=0
CUTOVER_COMMITTED=0
PUBLIC_ACTIVATION_STARTED=1
PREV_IMAGE_ID=sha256:previous
SHA=0123456789abcdef0123456789abcdef01234567
RESTORE_DB=
RESTORE_DB_CREATED=0
TMP_DIR="$TEST_ROOT/tmp"
mkdir -p "$TMP_DIR"
trap demo_cleanup EXIT

demo_haproxy_control() {
  printf 'haproxy %s\n' "$*" >> "$CALLS"
  case "$*" in
    'enable teagramd'|'drain teagramd-next') return 0 ;;
    'wait-healthy teagramd --timeout 60') return 1 ;;
    *) return 1 ;;
  esac
}
demo_compose() { printf 'compose %s\n' "$*" >> "$CALLS"; }
demo_write_active_release() { printf 'active %s %s\n' "$1" "$2" >> "$CALLS"; }
demo_prepare_haproxy_runtime() { printf 'prepare runtime\n' >> "$CALLS"; }
docker() { printf 'docker %s\n' "$*" >> "$CALLS"; }

false
BASH
if env \
  CLEANUP_FUNCTION="$IMAGE_CLEANUP_FUNCTION" \
  TEST_ROOT="$TMP_DIR/deploy-rollback-failure" \
  CALLS="$TMP_DIR/deploy-rollback-failure-calls" \
  bash "$TMP_DIR/deploy-rollback-failure-regression.sh"; then
  printf '%s\n' 'rollback-failure regression unexpectedly succeeded' >&2
  exit 1
fi
if ! grep -Fxq 'haproxy wait-healthy teagramd --timeout 60' "$TMP_DIR/deploy-rollback-failure-calls"; then
  printf '%s\n' 'rollback-failure regression did not exercise the old-slot health-check failure' >&2
  exit 1
fi
if grep -Fxq 'compose stop teagramd-next' "$TMP_DIR/deploy-rollback-failure-calls"; then
  printf '%s\n' 'deploy rollback stopped the candidate before the old slot was restored' >&2
  exit 1
fi

cat > "$TMP_DIR/deploy-image-cleanup-failure-regression.sh" <<'BASH'
set -Eeuo pipefail
source "$IMAGE_CLEANUP_FUNCTION"
PUBLIC_ACTIVATION_STARTED=1
BACKUP_READY=1
SERVICES_STOPPED=1
RESTORE_DB=
RESTORE_DB_CREATED=0
TEST_TMP_DIR="$TMP_DIR"
TMP_DIR=
DEPLOY_CONTINUED="$TEST_TMP_DIR/deploy-continued"
PRESERVE_CALLED="$TEST_TMP_DIR/preserve-called"
demo_preserve_after_activation_failure() { : > "$PRESERVE_CALLED"; }
docker() {
  [[ "$*" == 'image rm sha256:previous' ]] || return 0
  return 1
}
trap demo_cleanup EXIT
demo_remove_previous_image sha256:previous sha256:active
BACKUP_READY=0
: > "$DEPLOY_CONTINUED"
BASH
if ! env \
  IMAGE_CLEANUP_FUNCTION="$IMAGE_CLEANUP_FUNCTION" \
  TMP_DIR="$TMP_DIR" \
  bash "$TMP_DIR/deploy-image-cleanup-failure-regression.sh"; then
  printf '%s\n' 'deploy failed after previous-image cleanup failed' >&2
  exit 1
fi
[[ -e "$TMP_DIR/deploy-continued" ]]
[[ ! -e "$TMP_DIR/preserve-called" ]]

BACKUP_FUNCTIONS="$TMP_DIR/deploy-backup-functions.sh"
sed -n '/^demo_backup_database() {$/,/^}/p' "$SCRIPT_DIR/deploy-main.sh" > "$BACKUP_FUNCTIONS"
cat > "$TMP_DIR/deploy-backup-regression.sh" <<'BASH'
set -Eeuo pipefail
source "$BACKUP_FUNCTIONS"
mkdir -p "$BACKUP_DIR" "$TMP_DIR"
INGRESS_OPEN=1
SERVER_RUNNING=1
demo_fail() { return 1; }
chown() { :; }
chmod() { :; }
demo_compose() {
  if [[ "$*" == *'pg_dump -U postgres -Fc teagram'* ]]; then
    [[ "$INGRESS_OPEN" == 1 && "$SERVER_RUNNING" == 1 ]] || return 1
    printf 'live-database-snapshot\n'
  elif [[ "$*" == *"to_regclass('public.demo_wipe_generation')"* ]]; then
    printf 'f\n'
  elif [[ "$*" == *'psql -U postgres -Atqc'* ]]; then
    : # The isolated restore database does not exist yet.
  elif [[ "$*" == *'createdb -U postgres'* || "$*" == *'dropdb -U postgres'* ]]; then
    :
  else
    printf 'unexpected demo_compose call: %s\n' "$*" >&2
    return 1
  fi
}
demo_compose_input() {
  if [[ "$*" == *'pg_restore --list'* ]]; then
    printf 'TABLE DATA\n'
  elif [[ "$*" == *'-d teagram_restore_'* ]]; then
    cat >/dev/null
  else
    printf 'unexpected demo_compose_input call: %s\n' "$*" >&2
    return 1
  fi
}
demo_backup_database
[[ "$INGRESS_OPEN" == 1 && "$SERVER_RUNNING" == 1 ]]
grep -Fxq live-database-snapshot "$DB_DUMP"
[[ -s "$BACKUP_DIR/postgres.dump.sha256" ]]
[[ "$BACKUP_READY" == 1 ]]
BASH
env \
  BACKUP_DIR="$TMP_DIR/deploy-backup" \
  TMP_DIR="$TMP_DIR/deploy-backup-tmp" \
  DB_DUMP='' \
  RESTORE_DB='' \
  RESTORE_DB_CREATED=0 \
  BACKUP_READY=0 \
  SHA=0123456789abcdef0123456789abcdef01234567 \
  BACKUP_FUNCTIONS="$BACKUP_FUNCTIONS" \
  bash "$TMP_DIR/deploy-backup-regression.sh"

MIGRATION_FUNCTION="$TMP_DIR/demo-wait-migration.sh"
sed -n '/^demo_wait_migration() {$/,/^}/p' "$SCRIPT_DIR/demo-common.sh" > "$MIGRATION_FUNCTION"
cat > "$TMP_DIR/migration-wait-regression.sh" <<'BASH'
set -Eeuo pipefail
source "$MIGRATION_FUNCTION"
WAIT_COMPLETE="$TMP_DIR/migration-wait-complete"
MOCK_EXIT_CODE=0
demo_fail() { return 1; }
demo_compose() {
  [[ "$*" == 'ps -a -q migrate' ]]
  printf 'migration-container\n'
}
docker() {
  [[ "$1" == wait && "$2" == migration-container ]]
  sleep 0.1
  : > "$WAIT_COMPLETE"
  printf '%s\n' "$MOCK_EXIT_CODE"
}
demo_wait_migration demo_compose 'fixture migrations'
[[ -f "$WAIT_COMPLETE" ]]
MOCK_EXIT_CODE=17
if demo_wait_migration demo_compose 'fixture migrations'; then
  printf '%s\n' 'migration wait test accepted a failed migration' >&2
  exit 1
fi
BASH
env TMP_DIR="$TMP_DIR" MIGRATION_FUNCTION="$MIGRATION_FUNCTION" \
  bash "$TMP_DIR/migration-wait-regression.sh"

CLEANUP_FUNCTION="$TMP_DIR/init-demo-cleanup.sh"
COMPOSE_CALLS="$TMP_DIR/init-demo-compose-calls"
TMP_WORK_DIR="$TMP_DIR/init-demo-work"
mkdir -p "$TMP_WORK_DIR"
sed -n '/^cleanup() {$/,/^}/p' "$SCRIPT_DIR/init-demo.sh" > "$CLEANUP_FUNCTION"
cat > "$TMP_DIR/init-demo-failure.sh" <<'BASH'
set -Eeuo pipefail
source "$CLEANUP_FUNCTION"
demo_compose() {
  printf '%s\n' "$*" >> "$COMPOSE_CALLS"
}
TMP_DIR="$TMP_WORK_DIR"
PUBLIC_SERVICES_MAY_BE_RUNNING=1
trap cleanup EXIT
false # simulate smoke failure after the public stack starts
BASH
if CLEANUP_FUNCTION="$CLEANUP_FUNCTION" COMPOSE_CALLS="$COMPOSE_CALLS" \
  TMP_WORK_DIR="$TMP_WORK_DIR" bash "$TMP_DIR/init-demo-failure.sh"; then
  printf '%s\n' 'init-demo.sh cleanup test accepted a simulated smoke failure' >&2
  exit 1
fi
grep -Fxq 'stop edge haproxy' "$COMPOSE_CALLS"
[[ ! -e "$TMP_WORK_DIR" ]]

EDGE_CONFIG_PERMISSION_DIR="$TMP_DIR/edge-config-permissions"
mkdir -p "$EDGE_CONFIG_PERMISSION_DIR"
EDGE_CONFIG_PERMISSION_FILE="$EDGE_CONFIG_PERMISSION_DIR/nginx.conf"
printf '%s\n' 'worker_processes 1;' > "$EDGE_CONFIG_PERMISSION_FILE"
chmod 0700 "$EDGE_CONFIG_PERMISSION_DIR"
chmod 0600 "$EDGE_CONFIG_PERMISSION_FILE"
demo_prepare_edge_config_permissions "$EDGE_CONFIG_PERMISSION_DIR" "$EDGE_CONFIG_PERMISSION_FILE"
[[ "$(stat -c '%a' "$EDGE_CONFIG_PERMISSION_DIR")" == 755 ]]
[[ "$(stat -c '%a' "$EDGE_CONFIG_PERMISSION_FILE")" == 644 ]]

python3 "$SCRIPT_DIR/test-validate-env.py"
python3 "$SCRIPT_DIR/test_update_turnstile_signin.py"
bash "$SCRIPT_DIR/test-turnstile-signin-rollout.sh"
python3 "$SCRIPT_DIR/test-probe-tailnet-isolation.py"
if systemctl --system show-environment >/dev/null 2>&1; then
  if [[ "$EUID" -eq 0 ]]; then
    "$SCRIPT_DIR/test-activity-credential-handoff.sh"
  elif command -v sudo >/dev/null 2>&1 && sudo -n true >/dev/null 2>&1; then
    sudo -n env "PATH=$PATH" "$SCRIPT_DIR/test-activity-credential-handoff.sh"
  else
    printf 'systemd-to-container credential integration skipped (root access is unavailable)\n'
  fi
else
  printf 'systemd-to-container credential integration skipped (active system manager is unavailable)\n'
fi
printf 'demo Compose contract holds\n'
