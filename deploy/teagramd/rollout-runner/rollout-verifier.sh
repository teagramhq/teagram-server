#!/usr/bin/env bash
set -euo pipefail
umask 077

ROLLOUT_MAX_READY_SECONDS=${ROLLOUT_READY_SECONDS:-120}
ROLLOUT_REQUIRED_GRACE=2m0s
ROLLOUT_ADMIN_URL=https://teagram.tailaa4918.ts.net/admin/
ROLLOUT_TCP_HOST=teagram.tailaa4918.ts.net
ROLLOUT_TCP_PORT=2443
ROLLOUT_CHECKOUT_PATH=${ROLLOUT_CHECKOUT_PATH:-/opt/teagram}
ROLLOUT_VERIFIER_PATH=$(cd "$(dirname "$0")" && pwd)/$(basename "$0")

if ! declare -p ROLLOUT_VERIFIER_SOURCE_ONLY >/dev/null 2>&1; then
  ROLLOUT_VERIFIER_SOURCE_ONLY=0
fi

sha256_text() {
  printf '%s' "$1" | sha256sum | awk '{print $1}'
}

approved_logging_transition() {
  case "$1:$2" in
    syslog-v1:syslog-v1|default:syslog-v1) return 0 ;;
    *) return 1 ;;
  esac
}

approved_login_code_logging_transition() {
  case "$1:$2" in
    true:false|false:false|unset:false|not-captured:false) return 0 ;;
    *) return 1 ;;
  esac
}

verify_added_container_environment() {
  local inspect_json=$1 compose_json=$2 env_file=$3 added_env_names=$4
  [ "$added_env_names" != "[]" ] || return 0
  if ! {
    printf '%s\n' "$inspect_json" "$compose_json"
  } | jq -se --argjson added "$added_env_names" --rawfile env_content "$env_file" '
    def item: if type == "array" then .[0] else . end;
    def runtime_name($name):
      if $name == "TEAGRAM_TURNSTILE_SITE_KEY" then "TG_TURNSTILE_SITE_KEY"
      elif $name == "TEAGRAM_TURNSTILE_SECRET_KEY_FILE" then "TG_TURNSTILE_SECRET_KEY_FILE"
      else $name end;
    def dotenv_value($value):
      if ($value | length) >= 2 and
         ((($value | startswith("\"")) and ($value | endswith("\""))) or
          (($value | startswith("\u0027")) and ($value | endswith("\u0027")))) then $value[1:-1]
      else $value end;
    .[0] as $inspect
    | .[1] as $compose
    | ($inspect | item | .Config.Env // []) as $container_env
    | ($compose.services[env.ROLLOUT_COMPOSE_SERVICE // "teagramd"].environment // null) as $compose_env
    | if ($container_env | type) != "array" or ($compose_env | type) != "object" then false else
        ($env_content | split("\n")) as $env_lines
        | all($added[];
            . as $name
            | ($name | runtime_name($name)) as $runtime_name
            | [ $env_lines[] as $line
                | ($line | index("=")) as $separator
                | select($separator != null and $line[:$separator] == $name)
                | $line[($separator + 1):]
              ] as $assignments
            | [ $container_env[]
                | select(type == "string" and startswith($runtime_name + "="))
                | .[($runtime_name | length) + 1:]
              ] as $actual
            | ($assignments | length) == 1 and
              ($compose_env | has($runtime_name)) and
              ($compose_env[$runtime_name] | type) == "string" and
              ($actual | length) == 1 and
              $actual[0] == $compose_env[$runtime_name] and
              ($name != "TG_USER_INVITE_QUOTA" or
               (dotenv_value($assignments[0]) | test("^[0-9]+$"))) and
              ($name != "TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET" or
               (dotenv_value($assignments[0]) | test("^[0-9]+$"))) and
              (($name | IN(
                "TG_USER_INVITE_QUOTA",
                "TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET",
                "TG_PUBLIC_WEBSOCKET_LISTEN_ADDR", "TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR",
                "TG_PUBLIC_REGISTRATION", "TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS"
              )) | not or dotenv_value($assignments[0]) == $compose_env[$runtime_name])
          )
      end
  ' >/dev/null; then
    printf '%s\n' 'named environment additions do not match the resolved Compose environment' >&2
    return 1
  fi
  return 0
}

verify_changed_container_environment() {
  local inspect_json=$1 compose_json=$2 env_file=$3 changed_env_names=$4 snapshot_name=$5
  [ "$changed_env_names" != "[]" ] || return 0
  if ! {
    printf '%s\n' "$inspect_json" "$compose_json"
  } | jq -se --argjson changed "$changed_env_names" --arg snapshot_name "$snapshot_name" \
      --arg source "${ROLLOUT_CHANGED_ENV_SOURCE:-dotenv}" \
      --arg override_value "${ROLLOUT_OVERRIDE_ORIGIN_VALUE:-}" \
      --arg previous_override_value "${ROLLOUT_PREVIOUS_OVERRIDE_ORIGIN_VALUE:-}" \
      --rawfile env_content "$env_file" '
    def item: if type == "array" then .[0] else . end;
    def dotenv_value($value):
      if ($value | length) >= 2 and
         ((($value | startswith("\"")) and ($value | endswith("\""))) or
          (($value | startswith("\u0027")) and ($value | endswith("\u0027")))) then $value[1:-1]
      else $value end;
    .[0] as $inspect
    | .[1] as $compose
    | ($inspect | item | .Config.Env // []) as $container_env
    | ($compose.services[env.ROLLOUT_COMPOSE_SERVICE // "teagramd"].environment // null) as $compose_env
    | if ($container_env | type) != "array" or ($compose_env | type) != "object" then false else
        ($env_content | split("\n")) as $env_lines
        | all($changed[];
            . as $name
            | [ $env_lines[] as $line
                | ($line | index("=")) as $separator
                | select($separator != null and $line[:$separator] == $name)
                | $line[($separator + 1):]
              ] as $assignments
            | [ $container_env[]
                | select(type == "string" and startswith($name + "="))
                | .[($name | length) + 1:]
              ] as $actual
            | if $source == "override" then
                $name == "TG_WEBSOCKET_ALLOWED_ORIGINS" and
                ($assignments | length) == 0 and
                ($compose_env | has($name)) and
                ($compose_env[$name] | type) == "string" and
                $compose_env[$name] == $override_value and
                ($actual | length) == 1 and
                (if $snapshot_name == "baseline" or $snapshot_name == "preflight"
                 then $actual[0] == $previous_override_value
                 else $actual[0] == $compose_env[$name] end)
              else
                (($name == "TG_REGISTRATION" and (dotenv_value($assignments[0]) | IN("open","closed"))) or
                 ($name == "TG_TURNSTILE_SIGNIN_ENABLED" and ($assignments[0] | IN("true","false"))) or
                 ($name == "TG_PUBLIC_REGISTRATION" and (dotenv_value($assignments[0]) | IN("invite","closed"))) or
                ($name == "TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET" and (dotenv_value($assignments[0]) | test("^[0-9]+$"))) or
                 ($name | IN("TG_WEBSOCKET_ALLOWED_ORIGINS", "TG_PUBLIC_WEBSOCKET_LISTEN_ADDR", "TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR", "TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS"))) and
                ($assignments | length) == 1 and
                ($compose_env | has($name)) and
                ($compose_env[$name] == dotenv_value($assignments[0])) and
                ($actual | length) == 1 and
                ($actual[0] == dotenv_value($assignments[0]))
              end
          )
      end
  ' >/dev/null; then
    printf '%s\n' 'named environment value changes do not match the resolved Compose environment and source' >&2
    return 1
  fi
  return 0
}

verify_safe_public_environment() {
  local inspect_json=$1 compose_json=$2 env_file=$3 added_env_names=$4 changed_env_names=$5
  local snapshot_name=$6 compose_source=$7
  if ! printf '%s\n' "$inspect_json" "$compose_json" | jq -se \
    --argjson added "$added_env_names" --argjson changed "$changed_env_names" \
    --arg snapshot_name "$snapshot_name" --arg compose_source "$compose_source" \
    --rawfile env_content "$env_file" '
    def item: if type == "array" then .[0] else . end;
    def values($env; $name):
      [$env[]
       | select(type == "string" and startswith($name + "="))
       | .[($name | length) + 1:]];
    def assignments($lines; $name):
      [$lines[] as $line
       | ($line | index("=")) as $separator
       | select($separator != null and $line[:$separator] == $name)
       | $line[($separator + 1):]];
    def dotenv_value($value):
      if ($value | length) >= 2 and
         ((($value | startswith("\"")) and ($value | endswith("\""))) or
          (($value | startswith("\u0027")) and ($value | endswith("\u0027")))) then $value[1:-1]
      else $value end;
    def default_value($name):
      if $name == "TG_PUBLIC_REGISTRATION" then "closed" else "" end;
    def is_default($name; $value): $value == default_value($name);
    def valid_value($name; $value):
      if $name == "TG_PUBLIC_REGISTRATION" then $value == "invite" or $value == "closed"
      else true end;
    .[0] as $inspect
    | .[1] as $compose
    | ($inspect | item | .Config.Env // null) as $container_env
    | ($compose.services[env.ROLLOUT_COMPOSE_SERVICE // "teagramd"].environment // null) as $compose_env
    | ($compose.services["teagramd-public-1"].environment // null) as $public_env
    | ($env_content | split("\n")) as $env_lines
    | if ($container_env | type) != "array" then false
      elif $compose_source == "selected-compose-render" and ($compose_env | type) != "object" then false
      else
        all([
          "TG_PUBLIC_WEBSOCKET_LISTEN_ADDR",
          "TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR",
          "TG_PUBLIC_REGISTRATION",
          "TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS",
          "TG_WELCOME_CHANNEL"
        ][];
          . as $name
          | default_value($name) as $default
          | values($container_env; $name) as $container_values
          | (($compose_source == "selected-compose-render") and ($compose_env | has($name))) as $compose_has
          | (if $compose_has then $compose_env[$name] else $default end) as $compose_value
          | (($public_env | type) == "object" and
             ($name | IN("TG_PUBLIC_WEBSOCKET_LISTEN_ADDR", "TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR")) and
             ($public_env | has($name))) as $route_moved
          | (if $route_moved then $public_env[$name] else $compose_value end) as $active_value
          | (if ($container_values | length) == 0 then $default else $container_values[0] end) as $container_value
          | assignments($env_lines; $name) as $env_assignments
          | if ($container_values | length) > 1 or
               ($compose_has and ($compose_value | type) != "string") then false
            elif $compose_source == "live-container-inspection" then
              is_default($name; $container_value)
            elif $route_moved then
              ($compose_value == $default) and
              ($active_value | type) == "string" and
              (valid_value($name; $active_value)) and
              ($env_assignments | length) == 1 and
              (dotenv_value($env_assignments[0]) == $active_value) and
              (if $snapshot_name == "target" then $container_value == $compose_value
               else $container_value == $active_value end)
            elif is_default($name; $compose_value) and is_default($name; $container_value) then true
            elif $name == "TG_WELCOME_CHANNEL" or
                 (valid_value($name; $compose_value) | not) or
                 (valid_value($name; $container_value) | not) or
                 ($env_assignments | length) != 1 then false
            elif dotenv_value($env_assignments[0]) != $compose_value then false
            elif any($added[]; . == $name) then
              if $snapshot_name == "target" then $container_value == $compose_value
              elif $snapshot_name == "baseline" or $snapshot_name == "preflight" then
                is_default($name; $container_value) or $container_value == $compose_value
              else false end
            elif any($changed[]; . == $name) then
              if $snapshot_name == "baseline" or $snapshot_name == "preflight" then true
              else $container_value == $compose_value end
            else $container_value == $compose_value
            end
        )
      end
  ' >/dev/null; then
    if [ "$compose_source" = selected-compose-render ]; then
      printf '%s\n' 'resolved Compose has unsafe public environment settings or does not match its container' >&2
    else
      printf '%s\n' 'container has unsafe public environment settings' >&2
    fi
    return 1
  fi
  return 0
}

verify_turnstile_compose_configuration() {
  local compose_json=$1 checkout_root secret_source file_source secret_dir runtime_uid
  checkout_root=$(readlink -f "$ROLLOUT_CHECKOUT_PATH") || return 1
  secret_source="$checkout_root/.secrets/teagramd-turnstile"
  if ! printf '%s\n' "$compose_json" | jq -e --arg secret_source "$secret_source" '
    (.services[env.ROLLOUT_COMPOSE_SERVICE // "teagramd"].environment // {}) as $env
    | (.services[env.ROLLOUT_COMPOSE_SERVICE // "teagramd"].secrets // []) as $service_secrets
    | (.secrets // {}) as $secrets
    | ($secrets.teagramd_turnstile.file // "") as $file_source
    | (
        ($env | has("TG_TURNSTILE_SITE_KEY")) or
        ($env | has("TG_TURNSTILE_SECRET_KEY_FILE")) or
        ($env | has("TG_TURNSTILE_SIGNIN_ENABLED")) or
        any($service_secrets[]?; .source == "teagramd_turnstile") or
        ($secrets | has("teagramd_turnstile"))
      ) as $configured
    | if ($configured | not) then true else
        ($env.TG_TURNSTILE_SITE_KEY | type == "string") and
        ($env.TG_TURNSTILE_SECRET_KEY_FILE | type == "string") and
        ($env.TG_TURNSTILE_SIGNIN_ENABLED | IN("false", "true")) and
        ($env | has("TG_TURNSTILE_SECRET_KEY") | not) and
        (
          if $env.TG_TURNSTILE_SITE_KEY == "" then
            ($file_source == "/dev/null" and
             ([$service_secrets[]? | select(.source == "teagramd_turnstile" and .target == "teagramd-turnstile")] | length) == 1) or
            ($file_source == "" and
             ([$service_secrets[]? | select(.source == "teagramd_turnstile")] | length) == 0)
          else
            $file_source == $secret_source and
            ([$service_secrets[]? | select(.source == "teagramd_turnstile" and .target == "teagramd-turnstile")] | length) == 1
          end
        ) and
        (
          if $env.TG_TURNSTILE_SITE_KEY == "" then
            $env.TG_TURNSTILE_SECRET_KEY_FILE == "" and
            $env.TG_TURNSTILE_SIGNIN_ENABLED == "false" and
            ($file_source == "/dev/null" or $file_source == "")
          else
            ($env.TG_TURNSTILE_SITE_KEY | test("^[A-Za-z0-9_-]{1,256}$")) and
            $env.TG_TURNSTILE_SECRET_KEY_FILE == "/run/secrets/teagramd-turnstile" and
            $file_source == $secret_source
          end
        ) and
        ($env.TG_TURNSTILE_SIGNIN_ENABLED != "true" or $env.TG_TURNSTILE_SITE_KEY != "")
      end
  ' >/dev/null; then
    printf '%s\n' 'resolved Compose Turnstile mapping is incomplete or unsafe' >&2
    return 1
  fi
  file_source=$(printf '%s\n' "$compose_json" | jq -er '.secrets.teagramd_turnstile.file // ""') || return 1
  if [ -n "$file_source" ] && [ "$file_source" != "/dev/null" ]; then
    secret_dir=$(dirname -- "$file_source")
    runtime_uid=$(id -u)
    [ "$file_source" = "$secret_source" ] && [ -f "$file_source" ] && [ ! -L "$file_source" ] && \
      [ ! -L "$secret_dir" ] && [ "$(stat -c %a -- "$file_source")" = 444 ] && \
      [ "$(stat -c %u -- "$file_source")" = "$runtime_uid" ] && \
      [ "$(stat -c %a -- "$secret_dir")" = 700 ] && \
      [ "$(stat -c %u -- "$secret_dir")" = "$runtime_uid" ] || {
      printf '%s\n' 'Turnstile secret source must be a root-owned mode-0444 file in a root-only mode-0700 directory' >&2
      return 1
    }
  fi
  return 0
}

verify_turnstile_container_configuration() {
  local inspect_json=$1 compose_json=$2 snapshot_name=$3 added_env_names=$4 changed_env_names=$5
  if ! printf '%s\n' "$inspect_json" "$compose_json" | jq -se --arg snapshot_name "$snapshot_name" \
    --argjson added "$added_env_names" --argjson changed "$changed_env_names" '
    def item: if type == "array" then .[0] else . end;
    def runtime_name($name):
      if $name == "TEAGRAM_TURNSTILE_SITE_KEY" then "TG_TURNSTILE_SITE_KEY"
      elif $name == "TEAGRAM_TURNSTILE_SECRET_KEY_FILE" then "TG_TURNSTILE_SECRET_KEY_FILE"
      else $name end;
    .[0] as $inspect
    | .[1] as $compose
    | ($compose.services[env.ROLLOUT_COMPOSE_SERVICE // "teagramd"].environment // {}) as $compose_env
    | ($inspect | item | .Config.Env // []) as $container_env
    | if ($container_env | type) != "array" then false else
        [
          $container_env[] | select(type == "string") |
          select(
            startswith("TG_TURNSTILE_SITE_KEY=") or
            startswith("TG_TURNSTILE_SECRET_KEY_FILE=") or
            startswith("TG_TURNSTILE_SIGNIN_ENABLED=") or
            startswith("TG_TURNSTILE_SECRET_KEY=")
          )
        ] as $turnstile_entries
        | if ($compose_env | has("TG_TURNSTILE_SITE_KEY") | not) then
            ($turnstile_entries | length) == 0
          else
            ([$turnstile_entries[] | select(startswith("TG_TURNSTILE_SECRET_KEY="))] | length) == 0 and
            (
              if $snapshot_name == "target" then
                ($turnstile_entries | length) == 3
              else
                ($turnstile_entries | length) == 0 or ($turnstile_entries | length) == 3
              end
            ) and
            (
              if ($turnstile_entries | length) == 0 then true else
                # The live container predates this deploy, so settings it
                # adds or changes may still hold their previous values.
                [$added[] | runtime_name(.)] as $added_runtime
                | all(["TG_TURNSTILE_SITE_KEY", "TG_TURNSTILE_SECRET_KEY_FILE", "TG_TURNSTILE_SIGNIN_ENABLED"][];
                    . as $name
                    | ([$turnstile_entries[] | select(startswith($name + "="))] | length) == 1 and
                      (
                        ($snapshot_name != "target" and
                         (any($added_runtime[]; . == $name) or any($changed[]; . == $name))) or
                        ($turnstile_entries | index($name + "=" + $compose_env[$name])) != null
                      ))
              end
            )
          end
      end
  ' >/dev/null; then
    printf '%s\n' 'container Turnstile environment does not match the resolved Compose mapping' >&2
    return 1
  fi
  return 0
}

verify_turnstile_secret_mount() {
  local inspect_json=$1 compose_json=$2 snapshot_name=$3
  if ! printf '%s\n' "$inspect_json" "$compose_json" | jq -se --arg snapshot_name "$snapshot_name" '
    def item: if type == "array" then .[0] else . end;
    .[0] as $inspect
    | .[1] as $compose
    | ($compose.secrets.teagramd_turnstile.file // null) as $expected_source
    | [
        (($inspect | item | .Mounts // [])[])
        | select(.Destination == "/run/secrets/teagramd-turnstile")
      ] as $mounts
    | if $expected_source == null then
        ($mounts | length) == 0
      else
        if ($mounts | length) == 0 then $snapshot_name != "target"
        elif ($mounts | length) != 1 then false
        else
          $mounts[0].Type == "bind" and
          ($mounts[0].Source == $expected_source or
           ($snapshot_name != "target" and $mounts[0].Source == "/dev/null")) and
          $mounts[0].RW == false
        end
      end
  ' >/dev/null; then
    printf '%s\n' 'container Turnstile secret mount does not match the resolved Compose mapping' >&2
    return 1
  fi
  return 0
}

MEDIA_KEY_MOUNT_CONFIGURED=false
MEDIA_KEY_SOURCE_PATH=
MEDIA_KEY_VERSION=

verify_media_key_mount() {
  local inspect_json=$1 compose_json=$2 snapshot_name=$3 compose_files_value=${4:-${COMPOSE_FILE:-}}
  local record media_source media_version container_present source_bind_false=false service_name
  service_name=${ROLLOUT_COMPOSE_SERVICE:-teagramd}
  local resolved_source checkout_root directory_mode key_file key_mode key_owner
  # Compose omits create_host_path: false from the resolved config.
  if printf '%s' "$compose_json" | jq -e --arg service_name "$service_name" '
      [.services[$service_name].volumes[]? | select(.target == "/run/teagramd/media-keys" and .bind == {})]
      | length > 0
    ' >/dev/null; then
    verify_media_key_source_bind_false "$compose_files_value" "$service_name" || {
      printf '%s\n' 'source Compose does not explicitly disable media-key host-path creation' >&2
      return 1
    }
    source_bind_false=true
  fi
  if ! record=$(printf '%s\n' "$inspect_json" "$compose_json" | jq -ser \
    --arg snapshot_name "$snapshot_name" --arg service_name "$service_name" \
    --argjson source_bind_false "$source_bind_false" '
    def item: if type == "array" then .[0] else . end;
    def env_values($env; $name):
      [$env[] | select(type == "string" and startswith($name + "=")) | .[($name | length) + 1:]];
    def approved_media_key_bind:
      . == {create_host_path:false} or ($source_bind_false and . == {});
    .[0] as $inspect
    | .[1] as $compose
    | ($compose.services[$service_name] // null) as $service
    | ($service.environment // {}) as $compose_env
    | ($service.volumes // []) as $compose_volumes
    | ($inspect | item | .Config.Env // []) as $container_env
    | ($inspect | item | .Mounts // []) as $container_mounts
    | (if ($container_env | type) == "array" then $container_env else error("container environment is invalid") end) as $checked_env
    | (if ($compose_env | type) == "object" and ($compose_volumes | type) == "array" then true else error("Compose media-key configuration is invalid") end)
    | (env_values($checked_env; "TG_MEDIA_KEY_DIR")) as $container_dirs
    | (env_values($checked_env; "TG_MEDIA_KEY_CURRENT")) as $container_versions
    | [ $compose_volumes[] | select(type == "object" and .target == "/run/teagramd/media-keys") ] as $compose_mounts
    | [ $container_mounts[] | select(type == "object" and .Destination == "/run/teagramd/media-keys") ] as $live_mounts
    | (($compose_env | has("TG_MEDIA_KEY_DIR")) or ($compose_env | has("TG_MEDIA_KEY_CURRENT"))) as $configured
    | if ($configured | not) and ($compose_mounts | length) == 0 and
         ($container_dirs | length) == 0 and ($container_versions | length) == 0 and
         ($live_mounts | length) == 0 then "disabled"
      elif ($configured | not) or
           ($compose_env.TG_MEDIA_KEY_DIR != "/run/teagramd/media-keys") or
           (($compose_env.TG_MEDIA_KEY_CURRENT | type) != "string") or
           (($compose_env.TG_MEDIA_KEY_CURRENT | test("^[1-9][0-9]*$") | not)) or
           ($compose_mounts | length) != 1 then error("Compose media-key configuration is incomplete")
      else
        $compose_mounts[0] as $mount
        | ($mount.source // "") as $source
        | if ($mount | keys) != ["bind", "read_only", "source", "target", "type"] then
             error("Compose media-key mount has unexpected fields")
          elif ($mount.bind | type) != "object" or ($mount.bind | approved_media_key_bind | not) then
             error("Compose media-key bind options are invalid")
          elif $mount.type != "bind" or $mount.read_only != true or
               ($source | startswith("/") | not) or
               (($source | split("/")) | index("..") != null) or
               ($source | contains("\t")) or
               ($source | contains("\n")) then error("Compose media-key mount is not an exact read-only bind")
          else
            (if $snapshot_name == "target" or ($container_dirs | length) > 0 or
                ($container_versions | length) > 0 or ($live_mounts | length) > 0 then
               ($container_dirs | length) == 1 and
               $container_dirs[0] == "/run/teagramd/media-keys" and
               ($container_versions | length) == 1 and
               $container_versions[0] == $compose_env.TG_MEDIA_KEY_CURRENT and
               ($live_mounts | length) == 1 and
               $live_mounts[0].Type == "bind" and
               $live_mounts[0].Source == $source and
               $live_mounts[0].RW == false
             else $snapshot_name != "target" end) as $container_matches
            | if ($container_matches | not) then error("container media-key configuration does not match Compose")
              else [$source, $compose_env.TG_MEDIA_KEY_CURRENT, (($live_mounts | length) == 1 | tostring)] | @tsv end
          end
      end
  '); then
    printf '%s\n' 'resolved media-key configuration is incomplete or does not match the inspected container' >&2
    return 1
  fi
  if [ "$record" = disabled ]; then
    MEDIA_KEY_MOUNT_CONFIGURED=false
    MEDIA_KEY_SOURCE_PATH=
    MEDIA_KEY_VERSION=
    return 0
  fi
  IFS=$'\t' read -r media_source media_version container_present <<< "$record"
  [[ "$media_source" = /* && "$media_version" = 1 && "$container_present" =~ ^(true|false)$ ]] || {
    printf '%s\n' 'resolved media-key source metadata is invalid' >&2
    return 1
  }
  [ -d "$media_source" ] && [ ! -L "$media_source" ] || {
    printf '%s\n' 'media-key directory is missing or symlinked' >&2
    return 1
  }
  resolved_source=$(readlink -f "$media_source") || return 1
  [ "$resolved_source" = "$media_source" ] || {
    printf '%s\n' 'media-key directory has a symlinked path' >&2
    return 1
  }
  checkout_root=$(readlink -f "$ROLLOUT_CHECKOUT_PATH") || return 1
  case "$resolved_source/" in
    "$checkout_root/"*)
      printf '%s\n' 'media-key directory must be outside the application checkout' >&2
      return 1
      ;;
  esac
  directory_mode=$(stat -c %a "$media_source") || return 1
  [ "$directory_mode" = 700 ] || {
    printf '%s\n' 'media-key directory permissions are not private' >&2
    return 1
  }
  key_file="$media_source/$media_version.key"
  [ -f "$key_file" ] && [ ! -L "$key_file" ] || {
    printf '%s\n' 'selected media-key file is missing or symlinked' >&2
    return 1
  }
  key_mode=$(stat -c %a "$key_file") || return 1
  [ "$key_mode" = 600 ] && python3 -c '
import re
import sys

try:
    with open(sys.argv[1], encoding="utf-8") as key_file:
        value = key_file.read().strip()
except (OSError, UnicodeDecodeError):
    raise SystemExit(1)

raise SystemExit(re.fullmatch(r"[0-9a-fA-F]{64}", value) is None)
' "$key_file" 2>/dev/null || {
    printf '%s\n' 'selected media-key file has invalid permissions or format' >&2
    return 1
  }
  if [ "$(id -u)" = 0 ]; then
    key_owner=$(stat -c '%u:%g' "$key_file") || return 1
    [ "$(stat -c '%u:%g' "$media_source")" = 65532:65532 ] && [ "$key_owner" = 65532:65532 ] || {
      printf '%s\n' 'media-key directory or file has an unexpected owner' >&2
      return 1
    }
  fi
  MEDIA_KEY_MOUNT_CONFIGURED=true
  MEDIA_KEY_SOURCE_PATH=$media_source
  MEDIA_KEY_VERSION=$media_version
}

canonical_blob_mode_source() {
  local checkout_path state_dir mode_source
  checkout_path=$(readlink -f "$ROLLOUT_CHECKOUT_PATH") || return 1
  state_dir="$checkout_path/.state"
  if [ -e "$state_dir" ] || [ -L "$state_dir" ]; then
    state_dir=$(readlink -f "$state_dir") || return 1
  fi
  mode_source="$state_dir/blob-mode"
  if [ -e "$mode_source" ] || [ -L "$mode_source" ]; then
    readlink -f "$mode_source"
  else
    printf '%s\n' "$mode_source"
  fi
}

verify_media_key_source_bind_false() {
  local compose_files_value=${1:-} service_name=${2:-${ROLLOUT_COMPOSE_SERVICE:-teagramd}}
  local compose_file resolved_source_compose raw_bind_flags source_bind_flags source_result source_count source_approved total_matches=0
  local -a compose_files
  [ -n "$compose_files_value" ] || return 1
  IFS=: read -r -a compose_files <<< "$compose_files_value"
  [ "${#compose_files[@]}" -gt 0 ] || return 1
  for compose_file in "${compose_files[@]}"; do
    [ -n "$compose_file" ] && [ -f "$compose_file" ] && [ ! -L "$compose_file" ] || return 1
    resolved_source_compose=$(COMPOSE_FILE="$compose_file" docker compose \
      --project-directory "$ROLLOUT_CHECKOUT_PATH" --file "$compose_file" \
      config --no-consistency --format json </dev/null 2>/dev/null) || return 1
    raw_bind_flags=$(awk -v service_name="$service_name" '
      function trim(value) {
        sub(/^[[:space:]]+/, "", value)
        sub(/[[:space:]]+$/, "", value)
        return value
      }
      function close_volume() {
        if (in_volume) {
          print (!volume_invalid && bind_count == 1 && create_host_path_count == 1 &&
                 create_host_path_value == "false") ? "true" : "false"
        }
        in_volume = 0
        item_indent = -1
        volume_invalid = 0
        bind_count = 0
        bind_indent = -1
        bind_child_indent = -1
        create_host_path_count = 0
        create_host_path_value = ""
      }
      function parse_entry(text, key_indent, key, value, compact) {
        if (!match(text, /^[A-Za-z_][A-Za-z0-9_-]*:[[:space:]]*/)) return
        key = substr(text, 1, index(text, ":") - 1)
        value = trim(substr(text, index(text, ":") + 1))
        if (key == "bind") {
          bind_count++
          bind_indent = key_indent
          bind_child_indent = -1
          if (value != "") {
            compact = value
            gsub(/[[:space:]]/, "", compact)
            if (compact == "{create_host_path:false}") {
              create_host_path_count++
              create_host_path_value = "false"
            } else if (compact == "{create_host_path:true}") {
              create_host_path_count++
              create_host_path_value = "true"
            }
          }
          return
        }
        if (bind_indent >= 0) {
          if (key_indent > bind_indent) {
            if (bind_child_indent < 0) bind_child_indent = key_indent
            if (key_indent != bind_child_indent || key != "create_host_path") {
              volume_invalid = 1
            } else {
              create_host_path_count++
              create_host_path_value = trim(value)
            }
            return
          }
          if (key_indent > item_indent) {
            bind_indent = -1
            bind_child_indent = -1
          }
        }
      }
      {
        line = $0
        if (index(line, sprintf("%c", 9)) > 0) {
          if (in_volume) volume_invalid = 1
          next
        }
        sub(/\r$/, "", line)
        sub(/[[:space:]]+#.*/, "", line)
        if (line ~ /^[[:space:]]*$/) next
        match(line, /[^ ]/)
        indent = RSTART - 1
        text = substr(line, RSTART)

        if (in_teagramd && indent <= service_indent) {
          close_volume()
          in_volumes = 0
          in_teagramd = 0
        }
        if (in_services && !in_teagramd && indent <= services_indent &&
            text !~ /^services:[[:space:]]*$/) in_services = 0
        if (indent == 0 && text ~ /^services:[[:space:]]*$/) {
          close_volume()
          in_services = 1
          services_indent = indent
          in_teagramd = 0
          in_volumes = 0
          next
        }
        if (in_services && !in_teagramd && indent > services_indent &&
            trim(text) == service_name ":") {
          in_teagramd = 1
          service_indent = indent
          next
        }
        if (in_teagramd && in_volumes && indent <= volumes_indent) {
          close_volume()
          in_volumes = 0
        }
        if (in_teagramd && !in_volumes && indent > service_indent &&
            text ~ /^volumes:[[:space:]]*$/) {
          in_volumes = 1
          volumes_indent = indent
          next
        }
        if (in_volumes && indent > volumes_indent && text ~ /^-[[:space:]]*/) {
          if (in_volume) close_volume()
          in_volume = 1
          item_indent = indent
          rest = text
          sub(/^-[[:space:]]*/, "", rest)
          if (rest != "") parse_entry(rest, indent + 2)
          next
        }
        if (in_volume) {
          if (indent <= item_indent) {
            close_volume()
          } else {
            parse_entry(text, indent)
            next
          }
        }
      }
      END { close_volume() }
    ' "$compose_file") || return 1
    if [ -n "$raw_bind_flags" ]; then
      source_bind_flags=$(printf '%s\n' "$raw_bind_flags" | jq -Rsc \
        'split("\n") | map(select(length > 0) | . == "true")') || return 1
    else
      source_bind_flags='[]'
    fi
    source_result=$(printf '%s' "$resolved_source_compose" | jq -c \
      --arg service_name "$service_name" --argjson source_bind_flags "$source_bind_flags" '
      (.services[$service_name].volumes // []) as $volumes
      | if ($volumes | type) != "array" or ($volumes | length) != ($source_bind_flags | length) then
          error("resolved Compose volume order does not match its source")
        else
          [$volumes | to_entries[] | select(.value.target == "/run/teagramd/media-keys")] as $matching
          | {count:($matching | length), source_bind_false:all($matching[]; $source_bind_flags[.key] == true)}
        end
    ') || return 1
    source_count=$(printf '%s' "$source_result" | jq -er '.count') || return 1
    source_approved=$(printf '%s' "$source_result" | jq -er '.source_bind_false') || return 1
    [ "$source_approved" = true ] || return 1
    total_matches=$((total_matches + source_count))
  done
  [ "$total_matches" -gt 0 ]
}

verify_blob_rotation_migration_profile() {
  local compose_json=$1 mode_source=$2 media_source=$3
  if ! printf '%s' "$compose_json" | jq -e \
    --arg mode_source "$mode_source" --arg media_source "$media_source" '
    def safe_bind($source; $target):
      type == "object" and
      (keys | sort) == ["bind", "read_only", "source", "target", "type"] and
      .type == "bind" and .source == $source and .target == $target and .read_only == true and
      .bind == {create_host_path:false};
    def empty_volume($source; $target):
      type == "object" and
      (keys | sort) == ["read_only", "source", "target", "type", "volume"] and
      .type == "volume" and .source == $source and .target == $target and
      .read_only == true and .volume == {};
    . as $compose
    | ($compose.services["blob-key-rotate"] // null) as $rotation
    | if $rotation == null then true else
        ($compose.services.teagramd // {}) as $teagramd
        | ($rotation.environment // {}) as $rotation_env
        | ($teagramd.environment // {}) as $teagramd_env
        | ($rotation.volumes // []) as $volumes
        | ($rotation.depends_on // {}) as $depends_on
        | ($rotation.networks // {}) as $networks
        | ($rotation.secrets // []) as $secrets
        | ($teagramd.build.context // "") as $build_context
        | (if ($build_context | type) == "string" and ($build_context | startswith("/"))
           then ($build_context + "/.state/blob-mode") else $mode_source end) as $target_mode_source
        | ($rotation | keys | sort) == [
            "command", "depends_on", "entrypoint", "environment", "image", "networks",
            "profiles", "read_only", "secrets", "user", "volumes"
          ] and
          $rotation.image == $teagramd.image and
          $rotation.entrypoint == ["/usr/local/bin/blob-key-rotate"] and
          $rotation.command == null and
          $rotation.user == "0:0" and
          $rotation.read_only == true and
          $rotation.profiles == ["migration"] and
          ($rotation_env | keys | sort) == [
            "TG_AUTHKEY_ENC_KEY", "TG_AUTHKEY_ENC_KEY_FILE", "TG_BLOB_DIR",
            "TG_BLOB_S3_ACCESS_KEY_ID", "TG_BLOB_S3_ALLOW_INSECURE_HTTP", "TG_BLOB_S3_BUCKET",
            "TG_BLOB_S3_ENDPOINT", "TG_BLOB_S3_PREFIX", "TG_BLOB_S3_REGION",
            "TG_BLOB_S3_SECRET_ACCESS_KEY_FILE", "TG_MEDIA_KEY_CURRENT", "TG_MEDIA_KEY_DIR",
            "TG_POSTGRES_DSN"
          ] and
          all($rotation_env[]; type == "string") and
          all([
            "TG_AUTHKEY_ENC_KEY", "TG_AUTHKEY_ENC_KEY_FILE", "TG_BLOB_DIR",
            "TG_BLOB_S3_ACCESS_KEY_ID", "TG_BLOB_S3_ALLOW_INSECURE_HTTP", "TG_BLOB_S3_BUCKET",
            "TG_BLOB_S3_ENDPOINT", "TG_BLOB_S3_PREFIX", "TG_BLOB_S3_REGION",
            "TG_BLOB_S3_SECRET_ACCESS_KEY_FILE", "TG_MEDIA_KEY_CURRENT", "TG_MEDIA_KEY_DIR",
            "TG_POSTGRES_DSN"
          ][]; . as $name | $rotation_env[$name] == $teagramd_env[$name]) and
          $rotation_env.TG_BLOB_DIR == "/var/lib/teagramd-blobs" and
          $rotation_env.TG_MEDIA_KEY_DIR == "/run/teagramd/media-keys" and
          ($media_source | type == "string" and startswith("/")) and
          ($secrets == [{source:"teagramd_blob_secret_key",target:"teagramd-blob-secret-key"}]) and
          ($networks | keys | sort) == ["default", "rustfs_private"] and
          all($networks[]; . == null or . == {}) and
          ($depends_on | keys | sort) == ["postgres", "rustfs-init"] and
          ($depends_on.postgres.condition == "service_healthy") and
          (($depends_on.postgres | keys - ["condition", "required"]) | length) == 0 and
          (($depends_on.postgres.required // true) == true) and
          ($depends_on["rustfs-init"].condition == "service_completed_successfully") and
          (($depends_on["rustfs-init"] | keys - ["condition", "required"]) | length) == 0 and
          (($depends_on["rustfs-init"].required // true) == true) and
          ($volumes | length) == 4 and
          ([$volumes[] | select(empty_volume("tgkey"; "/var/lib/teagramd"))] | length) == 1 and
          ([$volumes[] | select(
            safe_bind($mode_source; "/run/teagramd/blob-mode") or
            safe_bind($target_mode_source; "/run/teagramd/blob-mode")
          )] | length) == 1 and
          ([$volumes[] | select(
            type == "object" and (keys | sort) == ["source", "target", "type", "volume"] and
            .type == "volume" and .source == "blob_key_rotate_reports" and
            .target == "/run/teagramd/reports" and .volume == {}
          )] | length) == 1 and
          ([$volumes[] | select(safe_bind($media_source; "/run/teagramd/media-keys"))] | length) == 1 and
          $compose.volumes.blob_key_rotate_reports == {name:"teagram_blob_key_rotate_reports",driver:"local"}
      end
  ' >/dev/null; then
    printf '%s\n' 'key-rotation migration profile is not the approved isolated operator configuration' >&2
    return 1
  fi
  return 0
}

snapshot_from_json() {
  local inspect_json=$1
  local compose_json=$2
  local env_file=$3
  local override_file=$4
  local compose_source=${5:-selected-compose-render}
  local snapshot_name=${6:-}
  local compose_files_value=${7:-${COMPOSE_FILE:-}}
  local image_env_json=${8:-'[]'}
  local mounts exposure exposure_for_hash resolved_config canonical_compose container_env container_logging_config container_id image_id state exit_code started_at finished_at grace stop_timeout mode_source mode_mount_target compose_logging_policy compose_log_login_codes container_log_login_codes compose_env_names
  local mounts_sha exposure_sha config_sha env_sha override_sha container_env_sha container_logging_sha mount_count
  local compose_replica_count compose_client_addr_trust container_replica_count container_client_addr_trust
  local compose_public_ws_2446 exposure_public_ws_2446
  local media_key_source media_key_current
  local added_env_names changed_env_names container_env_added container_env_changed retained_added_env_names
  MEDIA_KEY_MOUNT_CONFIGURED=false
  MEDIA_KEY_SOURCE_PATH=
  MEDIA_KEY_VERSION=

  if [ ! -f "$env_file" ] || [ "$(stat -c %a "$env_file")" != 600 ]; then
    printf '%s\n' 'environment file must be a regular mode-0600 file' >&2
    return 1
  fi

  # Variables a deploy adds to .env are named by the runner; they are left out
  # of the environment digest and recorded by name so every other entry stays pinned.
  added_env_names=$(jq -cn --arg names "${ROLLOUT_ADDED_ENV:-}" '$names | split(",") | map(select(length > 0))')
  changed_env_names=$(jq -cn --arg names "${ROLLOUT_CHANGED_ENV:-}" '$names | split(",") | map(select(length > 0))')
  if ! jq -e 'type == "array" and all(.[]; type == "string")' <<< "$image_env_json" >/dev/null; then
    printf '%s\n' 'image environment is invalid' >&2
    return 1
  fi
  verify_safe_public_environment \
    "$inspect_json" "$compose_json" "$env_file" "$added_env_names" "$changed_env_names" \
    "$snapshot_name" "$compose_source" || return 1
  compose_logging_policy=not-captured
  compose_log_login_codes=not-captured
  if [ "$compose_source" = selected-compose-render ]; then
    compose_logging_policy=$(printf '%s' "$compose_json" | jq -er '
      .services[env.ROLLOUT_COMPOSE_SERVICE // "teagramd"].logging as $logging
      | if $logging == null then "default"
        elif $logging == {
          driver:"syslog",
          options:{
            "syslog-address":"unixgram:///dev/log",
            "syslog-facility":"daemon",
            "syslog-format":"rfc5424",
            tag:"teagramd"
          }
        } then "syslog-v1"
        else error("unapproved teagramd logging configuration")
        end
    ' 2>/dev/null) || {
      printf '%s\n' 'resolved Compose logging configuration is not approved' >&2
      return 1
    }
    compose_log_login_codes=$(printf '%s' "$compose_json" | jq -er '
      .services[env.ROLLOUT_COMPOSE_SERVICE // "teagramd"].environment as $env
      | if ($env | type) != "object" then error("teagramd environment must be an object")
        elif ($env | has("TG_LOG_LOGIN_CODES") | not) then "unset"
        elif $env.TG_LOG_LOGIN_CODES == "true" or $env.TG_LOG_LOGIN_CODES == true then "true"
        elif $env.TG_LOG_LOGIN_CODES == "false" or $env.TG_LOG_LOGIN_CODES == false then "false"
        else "invalid" end
    ' 2>/dev/null) || {
      printf '%s\n' 'resolved Compose login-code logging configuration is not approved' >&2
      return 1
    }
    case "$compose_log_login_codes" in
      true|false|unset) ;;
      *) printf '%s\n' 'resolved Compose login-code logging configuration is not approved' >&2; return 1 ;;
    esac
    if [ "$compose_logging_policy" = syslog-v1 ] && [ "$compose_log_login_codes" != false ]; then
      printf '%s\n' 'syslog retention requires TG_LOG_LOGIN_CODES=false' >&2
      return 1
    fi
    if [ "$compose_logging_policy" = syslog-v1 ] && \
       { [ "$snapshot_name" = baseline ] || [ "$snapshot_name" = target ] || [ "$snapshot_name" = rollback ]; } && \
       ! printf '%s' "$inspect_json" | jq -e '
         def item: if type == "array" then .[0] else . end;
         (item.HostConfig.LogConfig // {}) as $log
         | $log.Type == "syslog" and
           ($log.Config // {}) == {
             "syslog-address":"unixgram:///dev/log",
             "syslog-facility":"daemon",
             "syslog-format":"rfc5424",
             tag:"teagramd"
           }
       ' >/dev/null; then
      printf '%s\n' 'container syslog logging configuration does not match the reviewed Compose policy' >&2
      return 1
    fi
  fi
  container_log_login_codes=$(printf '%s' "$inspect_json" | jq -er '
    def item: if type == "array" then .[0] else . end;
    (item.Config.Env // []) as $env
    | if ($env | type) != "array" then error("container environment must be an array") else
        [$env[] | select(type == "string" and startswith("TG_LOG_LOGIN_CODES="))
          | .[("TG_LOG_LOGIN_CODES=" | length):]] as $values
        | if ($values | length) == 0 then "unset"
          elif ($values | length) > 1 then "invalid"
          elif $values[0] == "true" or $values[0] == "false" then $values[0]
          else "invalid" end
      end
  ' 2>/dev/null) || {
    printf '%s\n' 'container login-code logging configuration is invalid' >&2
    return 1
  }
  case "$container_log_login_codes" in
    true|false|unset) ;;
    *) printf '%s\n' 'container login-code logging configuration is invalid' >&2; return 1 ;;
  esac
  if [ "$compose_source" = selected-compose-render ] && \
     { [ "$snapshot_name" = baseline ] || [ "$snapshot_name" = target ] || [ "$snapshot_name" = rollback ]; } && \
     [ "$compose_log_login_codes" != "$container_log_login_codes" ]; then
    printf '%s\n' 'resolved Compose login-code logging setting does not match the inspected container' >&2
    return 1
  fi
  if [ "$compose_source" = selected-compose-render ]; then
    compose_public_ws_2446=$(printf '%s' "$compose_json" | jq -r '
      def approved_public_ws_port:
        type == "object" and
        ((.target | tostring) == "2446") and
        ((.published | tostring) == "2446") and
        .host_ip == "127.0.0.1" and
        ((.protocol // "tcp") == "tcp") and
        ((.mode // "") == "" or (.mode // "") == "ingress") and
        ((keys - ["target", "published", "host_ip", "protocol", "mode"]) | length) == 0;
      (.services[env.ROLLOUT_COMPOSE_SERVICE // "teagramd"].ports // []) as $ports
      | if ($ports | type) != "array" then error("resolved service ports must be an array") else
          [$ports[] | select(approved_public_ws_port)] as $approved
          | if ($approved | length) > 1 then error("approved public WebSocket port is duplicated")
            else (($approved | length) == 1) end
        end
    ') || return 1
  else
    compose_public_ws_2446=not-captured
  fi
  if [ "$compose_source" = selected-compose-render ]; then
    verify_turnstile_compose_configuration "$compose_json" || return 1
    verify_turnstile_container_configuration \
      "$inspect_json" "$compose_json" "$snapshot_name" "$added_env_names" "$changed_env_names" || return 1
    verify_turnstile_secret_mount "$inspect_json" "$compose_json" "$snapshot_name" || return 1
    verify_media_key_mount "$inspect_json" "$compose_json" "$snapshot_name" "$compose_files_value" || return 1
  fi
  media_key_source=${MEDIA_KEY_SOURCE_PATH:-unset}
  media_key_current=${MEDIA_KEY_VERSION:-unset}

  mode_source=$(canonical_blob_mode_source) || return 1
  mode_mount_target=/run/teagramd/blob-mode
  [ "${ROLLOUT_COMPOSE_SERVICE:-teagramd}" != telegramd ] || mode_mount_target=/run/telegramd/blob-mode
  if [ "$compose_source" = selected-compose-render ]; then
    verify_blob_rotation_migration_profile "$compose_json" "$mode_source" "$MEDIA_KEY_SOURCE_PATH" || return 1
  fi

  mounts=$(printf '%s' "$inspect_json" | jq -ce --arg mode_source "$mode_source" --arg mode_target "$mode_mount_target" --arg compose_source "$compose_source" \
    --arg media_key_source "$MEDIA_KEY_SOURCE_PATH" --argjson media_key_enabled "$MEDIA_KEY_MOUNT_CONFIGURED" '
    def item: if type == "array" then .[0] else . end;
    (item.Mounts // [])
    | map({type:(.Type // ""), name:(.Name // ""), source:(.Source // ""),
           destination:(.Destination // ""), mode:(.Mode // ""),
           rw:(.RW // false), propagation:(.Propagation // "")})
    | map(select((.destination == "/run/secrets/teagramd-turnstile" and
                  $compose_source == "selected-compose-render") | not))
    | map(select(($media_key_enabled and .type == "bind" and .source == $media_key_source and
                  .destination == "/run/teagramd/media-keys" and .rw == false) | not))
    | map(select((.type == "bind" and .source == $mode_source and
                  .destination == $mode_target and .rw == false) | not))
    | sort_by(.type, .name, .source, .destination, .mode, .rw, .propagation)
  ')
  exposure=$(printf '%s' "$inspect_json" | jq -ce '
    def item: if type == "array" then .[0] else . end;
    (item.HostConfig.PortBindings // {})
    | to_entries
    | map({container_port:.key,
           bindings:((.value // [])
             | map({host_ip:(.HostIp // ""), host_port:(.HostPort // "")})
             | sort_by(.host_ip, .host_port))})
    | sort_by(.container_port)
  ')
  exposure_public_ws_2446=$(printf '%s' "$exposure" | jq -r '
    [.[] | select(.container_port == "2446/tcp") | .bindings[]
      | select(.host_ip == "127.0.0.1" and .host_port == "2446")] as $approved
    | if ($approved | length) > 1 then error("approved public WebSocket exposure is duplicated")
      else (($approved | length) == 1) end
  ') || return 1
  if [ "$compose_source" = selected-compose-render ] && \
     { [ "$snapshot_name" = baseline ] || [ "$snapshot_name" = target ] || [ "$snapshot_name" = rollback ]; } && \
     [ "$compose_public_ws_2446" != "$exposure_public_ws_2446" ]; then
    if ! { [ "${ROLLOUT_PUBLIC_WEBSOCKET:-0}" = 1 ] && [ "$snapshot_name" = preflight ] && \
           [ "$compose_public_ws_2446" = false ] && [ "$exposure_public_ws_2446" = true ]; }; then
      printf '%s\n' 'resolved Compose public WebSocket port does not match the inspected container' >&2
      return 1
    fi
  fi
  exposure_for_hash=$(printf '%s' "$exposure" | jq -ce '
    map(
      if .container_port == "2446/tcp" then
        .bindings |= map(select(.host_ip != "127.0.0.1" or .host_port != "2446"))
        | select((.bindings | length) > 0)
      else . end
    )
  ') || return 1
  container_replica_count=$(printf '%s' "$inspect_json" | jq -er '
    def item: if type == "array" then .[0] else . end;
    (item.Config.Env // []) as $env
    | if ($env | type) != "array" then error("container environment must be an array") else
        [$env[] | select(type == "string" and startswith("TG_REPLICA_COUNT=")) | .[("TG_REPLICA_COUNT=" | length):]] as $values
        | if ($values | length) == 0 then "unset"
          elif ($values | length) > 1 then "duplicate"
          elif ($values[0] | test("^[0-9]{1,3}$")) then $values[0]
          else "invalid" end
      end
  ')
  container_client_addr_trust=$(printf '%s' "$inspect_json" | jq -er '
    def item: if type == "array" then .[0] else . end;
    (item.Config.Env // []) as $env
    | if ($env | type) != "array" then error("container environment must be an array") else
        [$env[] | select(type == "string" and startswith("TG_CLIENT_ADDR_TRUST=")) | .[("TG_CLIENT_ADDR_TRUST=" | length):]] as $values
        | if ($values | length) == 0 then "unset"
          elif ($values | length) > 1 then "duplicate"
          elif $values[0] == "socket" then "socket"
          else "invalid" end
      end
  ')
  if [ "$compose_source" = selected-compose-render ] && [ "$snapshot_name" = target ]; then
    verify_added_container_environment "$inspect_json" "$compose_json" "$env_file" "$added_env_names" || return 1
    verify_changed_container_environment "$inspect_json" "$compose_json" "$env_file" "$changed_env_names" "$snapshot_name" || return 1
  elif [ "$compose_source" = selected-compose-render ] && [ "$snapshot_name" = rollback ]; then
    retained_added_env_names=$(printf '%s' "$inspect_json" | jq -c --argjson added "$added_env_names" '
      def item: if type == "array" then .[0] else . end;
      def runtime_name($name):
        if $name == "TEAGRAM_TURNSTILE_SITE_KEY" then "TG_TURNSTILE_SITE_KEY"
        elif $name == "TEAGRAM_TURNSTILE_SECRET_KEY_FILE" then "TG_TURNSTILE_SECRET_KEY_FILE"
        else $name end;
      [(item.Config.Env // [])[] | select(type == "string")
       | select(. != "TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET=")
       | select(. != "TG_USER_INVITE_QUOTA=")
       | select(IN(
           "TG_TURNSTILE_SITE_KEY=", "TG_TURNSTILE_SECRET_KEY_FILE=", "TG_TURNSTILE_SIGNIN_ENABLED=false",
           "TG_PUBLIC_WEBSOCKET_LISTEN_ADDR=", "TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR=",
           "TG_PUBLIC_REGISTRATION=closed", "TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS="
         ) | not)
       | split("=")[0]] as $keys
      | [$added[] | select(. as $name | ($name | runtime_name($name)) as $runtime | any($keys[]; . == $runtime))]
      | unique
    ') || return 1
    verify_added_container_environment \
      "$inspect_json" "$compose_json" "$env_file" "$retained_added_env_names" || return 1
    verify_changed_container_environment "$inspect_json" "$compose_json" "$env_file" "$changed_env_names" "$snapshot_name" || return 1
  elif [ "$compose_source" = selected-compose-render ] && \
       [ "${ROLLOUT_CHANGED_ENV_SOURCE:-dotenv}" = override ] && [ "$changed_env_names" != "[]" ]; then
    verify_changed_container_environment "$inspect_json" "$compose_json" "$env_file" "$changed_env_names" "$snapshot_name" || return 1
  fi
  container_logging_config=$(printf '%s' "$inspect_json" | jq -ceS '
    def item: if type == "array" then .[0] else . end;
    (item.HostConfig.LogConfig // {}) as $log
    | if ($log.Type | type) != "string" or $log.Type == "" or
         (($log.Config // {}) | type) != "object" then
        error("inspected container logging configuration is invalid")
      else {type:$log.Type,config:($log.Config // {})} end
  ') || {
    printf '%s\n' 'inspected container logging configuration is invalid' >&2
    return 1
  }
  container_logging_sha=$(sha256_text "$container_logging_config")
  # Compose's resolved environment includes operator values from .env. Docker's
  # Config.Env also includes image ENV defaults, which are covered by image identity.
  # Keep configured Compose keys in the digest even when their value matches an
  # image default; a later image may change that default while Compose pins its value.
  # Empty mapped key-exchange values use the binary defaults, so they stay
  # equivalent to an absent variable; configured values remain pinned.
  compose_env_names='[]'
  if [ "$compose_source" = selected-compose-render ]; then
    compose_env_names=$(printf '%s' "$compose_json" | jq -ce '
      (.services[env.ROLLOUT_COMPOSE_SERVICE // "teagramd"].environment // {}) as $env
      | if ($env | type) == "object" then ($env | keys)
        else error("resolved Compose environment must be an object") end
    ') || return 1
  fi
  container_env=$(printf '%s' "$inspect_json" | jq -ce --argjson image_env "$image_env_json" --argjson compose_env_names "$compose_env_names" --argjson added "$added_env_names" --argjson changed "$changed_env_names" \
    --argjson media_key_enabled "$MEDIA_KEY_MOUNT_CONFIGURED" '
    def item: if type == "array" then .[0] else . end;
    def runtime_name($name):
      if $name == "TEAGRAM_TURNSTILE_SITE_KEY" then "TG_TURNSTILE_SITE_KEY"
      elif $name == "TEAGRAM_TURNSTILE_SECRET_KEY_FILE" then "TG_TURNSTILE_SECRET_KEY_FILE"
      else $name end;
    ($compose_env_names | map(runtime_name(.))) as $configured_env_names
    | (item | .Config.Env // []) as $raw_env
    | if ($raw_env | type) != "array" or any($raw_env[]; type != "string") then error("container environment must be an array of strings")
      else $raw_env
        | map(. as $entry
          | select(($image_env | index($entry)) == null or
                  ($configured_env_names | index($entry | split("=")[0])) != null))
        | map(select(type != "string" or (startswith("TG_REPLICA_COUNT=") | not)))
        | map(select(type != "string" or (startswith("TG_CLIENT_ADDR_TRUST=") | not)))
        | map(select(type != "string" or (startswith("TG_LOG_LOGIN_CODES=") | not)))
        | map(select(. != "TG_USER_INVITE_QUOTA="))
        | map(select(type != "string" or ((split("=")[0]) as $key | (["TG_PUBLIC_WEBSOCKET_LISTEN_ADDR", "TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR", "TG_PUBLIC_REGISTRATION", "TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS", "TG_WELCOME_CHANNEL"] | index($key) == null))))
        | map(select(type != "string" or ((split("=")[0]) as $key | any($added[]; runtime_name(.) == $key) | not)))
        | map(select(type != "string" or ((split("=")[0]) as $key | any($changed[]; . == $key) | not)))
        | map(select(type != "string" or (IN(
            "TG_RATE_LIMIT_KEY_EXCHANGE_IP=",
            "TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET=",
            "TG_RATE_LIMIT_KEY_EXCHANGE_IP_WINDOW=",
            "TG_KEY_EXCHANGE_PER_CONNECTION=",
            "TG_KEY_EXCHANGE_GLOBAL_RATE=",
            "TG_KEY_EXCHANGE_GLOBAL_BURST="
          ) | not)))
        | map(select(type != "string" or (((split("="))[0]) as $key | ($media_key_enabled and ($key | IN("TG_MEDIA_KEY_DIR", "TG_MEDIA_KEY_CURRENT"))) | not)))
        | map(select(type != "string" or (
            (. == "TG_TURNSTILE_SITE_KEY=" or
             . == "TG_TURNSTILE_SECRET_KEY_FILE=" or
             . == "TG_TURNSTILE_SIGNIN_ENABLED=false") | not
          )))
        | sort
      end
  ')
  # Disabled Turnstile and public listener defaults count as unset before the
  # target, so a named addition can introduce them and the target must carry it.
  container_env_added=$(printf '%s' "$inspect_json" | jq -er --argjson added "$added_env_names" --arg snapshot_name "$snapshot_name" '
    def item: if type == "array" then .[0] else . end;
    def runtime_name($name):
      if $name == "TEAGRAM_TURNSTILE_SITE_KEY" then "TG_TURNSTILE_SITE_KEY"
      elif $name == "TEAGRAM_TURNSTILE_SECRET_KEY_FILE" then "TG_TURNSTILE_SECRET_KEY_FILE"
      else $name end;
    [(item.Config.Env // [])[] | select(type == "string")
     | select(. != "TG_USER_INVITE_QUOTA=")
     | select(IN(
         "TG_RATE_LIMIT_KEY_EXCHANGE_IP=",
         "TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET=",
         "TG_RATE_LIMIT_KEY_EXCHANGE_IP_WINDOW=",
         "TG_KEY_EXCHANGE_PER_CONNECTION=",
         "TG_KEY_EXCHANGE_GLOBAL_RATE=",
         "TG_KEY_EXCHANGE_GLOBAL_BURST="
       ) | not)
     | select($snapshot_name == "target" or
              (IN(
                "TG_TURNSTILE_SITE_KEY=", "TG_TURNSTILE_SECRET_KEY_FILE=", "TG_TURNSTILE_SIGNIN_ENABLED=false",
                "TG_PUBLIC_WEBSOCKET_LISTEN_ADDR=", "TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR=",
                "TG_PUBLIC_REGISTRATION=closed", "TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS=",
                "TG_REPLICA_COUNT=1"
              ) | not))
     | split("=")[0]] as $keys
    | [$added[] | select(. as $name | ($name | runtime_name($name)) as $runtime | any($keys[]; . == $runtime))]
    | unique | sort | if length == 0 then "none" else join(",") end
  ')
  container_env_changed=none
  if { [ "$snapshot_name" = target ] || [ "$snapshot_name" = rollback ]; } && [ "$changed_env_names" != "[]" ]; then
    container_env_changed=$(printf '%s' "$changed_env_names" | jq -er 'if length == 0 then "none" else sort | join(",") end')
  fi

  container_id=$(printf '%s' "$inspect_json" | jq -er 'if type == "array" then .[0].Id else .Id end')
  image_id=$(printf '%s' "$inspect_json" | jq -er 'if type == "array" then .[0].Image else .Image end')
  state=$(printf '%s' "$inspect_json" | jq -er 'if type == "array" then .[0].State.Status else .State.Status end')
  exit_code=$(printf '%s' "$inspect_json" | jq -er 'if type == "array" then .[0].State.ExitCode else .State.ExitCode end')
  started_at=$(printf '%s' "$inspect_json" | jq -er 'if type == "array" then .[0].State.StartedAt else .State.StartedAt end')
  finished_at=$(printf '%s' "$inspect_json" | jq -er 'if type == "array" then .[0].State.FinishedAt else .State.FinishedAt end')
  stop_timeout=$(printf '%s' "$inspect_json" | jq -er '
    (if type == "array" then .[0].Config.StopTimeout else .Config.StopTimeout end)
    | select(type == "number" and . == floor)
  ')
  if [ "$compose_source" = live-container-inspection ]; then
    # The selected Compose file is the desired target during initialize-local.
    # Derive the running baseline's grace from Docker's inspected stop timeout
    # and leave render-only fields explicitly uncaptured.
    if [ "$stop_timeout" -ge 60 ]; then
      grace="$((stop_timeout / 60))m$((stop_timeout % 60))s"
    else
      grace="${stop_timeout}s"
    fi
    compose_replica_count=not-captured
    compose_client_addr_trust=not-captured
    config_sha=not-captured
    env_sha=not-captured
    override_sha=not-captured
  else
    [ "$compose_source" = selected-compose-render ] || {
      printf '%s\n' 'invalid Compose provenance for snapshot' >&2
      return 1
    }
    resolved_config=$(printf '%s' "$compose_json" | jq -ce --arg mode_source "$mode_source" --arg mode_target "$mode_mount_target" '
      .services[env.ROLLOUT_COMPOSE_SERVICE // "teagramd"] as $s
      | {
          stop_grace_period:($s.stop_grace_period // ""),
      ports:(($s.ports // [])
            | map({target:(.target // 0 | tostring), published:(.published // "" | tostring),
                   host_ip:(.host_ip // ""), protocol:(.protocol // "tcp"), mode:(.mode // "")})
            | map(select(.target != "2446" or .published != "2446" or .host_ip != "127.0.0.1" or
                         .protocol != "tcp" or (.mode != "" and .mode != "ingress")))
            | sort_by(.host_ip, .published, .target, .protocol, .mode)),
          volumes:(($s.volumes // [])
            | map(select((.type == "bind" and .source == $mode_source and
                          .target == $mode_target and .read_only == true) | not))
            | map({type:(.type // ""), source:(.source // ""), target:(.target // ""),
                   read_only:(.read_only // false)})
            | sort_by(.type, .source, .target, .read_only)),
          network_mode:($s.network_mode // ""),
          networks:(($s.networks // {}) | if type == "object" then keys else . end | sort)
        }
    ')
    canonical_compose=$(printf '%s' "$compose_json" | jq -ceS --arg mode_source "$mode_source" --arg mode_target "$mode_mount_target" \
      --arg media_key_source "$MEDIA_KEY_SOURCE_PATH" --argjson media_key_enabled "$MEDIA_KEY_MOUNT_CONFIGURED" \
      --argjson added "$added_env_names" --argjson changed "$changed_env_names" '
      def approved_public_ws_port:
        type == "object" and
        ((.target | tostring) == "2446") and
        ((.published | tostring) == "2446") and
        .host_ip == "127.0.0.1" and
        ((.protocol // "tcp") == "tcp") and
        ((.mode // "") == "" or (.mode // "") == "ingress") and
        ((keys - ["target", "published", "host_ip", "protocol", "mode"]) | length) == 0;
      def empty_key_exchange_default:
        (.key | IN(
          "TG_RATE_LIMIT_KEY_EXCHANGE_IP",
          "TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET",
          "TG_RATE_LIMIT_KEY_EXCHANGE_IP_WINDOW",
          "TG_KEY_EXCHANGE_PER_CONNECTION",
          "TG_KEY_EXCHANGE_GLOBAL_RATE",
          "TG_KEY_EXCHANGE_GLOBAL_BURST"
        )) and .value == "";
      def empty_user_invite_quota_default:
        .key == "TG_USER_INVITE_QUOTA" and .value == "";
      # Compose includes extension anchors in its rendered output. Their
      # expanded service fields remain in the digest; the anchors do not.
      with_entries(select((.key | startswith("x-")) | not))
      |
      if (.services[env.ROLLOUT_COMPOSE_SERVICE // "teagramd"].environment | type) != "object" then error("teagramd environment must be an object") else
        .services[env.ROLLOUT_COMPOSE_SERVICE // "teagramd"].environment |= del(
          .TG_REPLICA_COUNT, .TG_CLIENT_ADDR_TRUST,
          .TG_WEBSOCKET_ALLOWED_ORIGINS,
          .TG_PUBLIC_WEBSOCKET_LISTEN_ADDR, .TG_PUBLIC_REGISTRATION,
          .TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR,
          .TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS, .TG_WELCOME_CHANNEL,
          .TG_LOG_LOGIN_CODES,
          .TG_MEDIA_KEY_DIR, .TG_MEDIA_KEY_CURRENT,
          .TG_TURNSTILE_SITE_KEY, .TG_TURNSTILE_SECRET_KEY_FILE,
          .TG_TURNSTILE_SIGNIN_ENABLED)
      end
      # Empty key-exchange mappings use binary defaults; active values remain
      # subject to the Compose and container environment checks.
      | .services[env.ROLLOUT_COMPOSE_SERVICE // "teagramd"].environment |= with_entries(select((empty_key_exchange_default or empty_user_invite_quota_default) | not))
      # Named tailnet changes are verified against .env, resolved Compose, and
      # the inspected container; omit only that named value from config drift.
      | if (any($added[]; . == "TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET") or
            any($changed[]; . == "TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET")) then
          del(.services[env.ROLLOUT_COMPOSE_SERVICE // "teagramd"].environment.TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET)
        else . end
      # The first quota is checked against .env, Compose, and the container;
      # omit it from the unrelated config digest during that named addition.
      | if any($added[]; . == "TG_USER_INVITE_QUOTA") then
          del(.services[env.ROLLOUT_COMPOSE_SERVICE // "teagramd"].environment.TG_USER_INVITE_QUOTA)
        else . end
      # Reviewed logger and login-code transitions are checked separately.
      # Keep both out of the unrelated config digest.
      | .services[env.ROLLOUT_COMPOSE_SERVICE // "teagramd"] |= del(.logging)
      | .services[env.ROLLOUT_COMPOSE_SERVICE // "teagramd"].ports = ((.services[env.ROLLOUT_COMPOSE_SERVICE // "teagramd"].ports // []) | map(select(approved_public_ws_port | not)))
      | if (.services[env.ROLLOUT_COMPOSE_SERVICE // "teagramd"].ports | length) == 0 then del(.services[env.ROLLOUT_COMPOSE_SERVICE // "teagramd"].ports) else . end
      | if $media_key_enabled then
          .services[env.ROLLOUT_COMPOSE_SERVICE // "teagramd"].volumes |= map(select((.type == "bind" and .source == $media_key_source and
                                                      .target == "/run/teagramd/media-keys" and .read_only == true) | not))
        else . end
      | if (.services[env.ROLLOUT_COMPOSE_SERVICE // "teagramd"].secrets | type) == "array" then
          .services[env.ROLLOUT_COMPOSE_SERVICE // "teagramd"].secrets |= map(select(.source != "teagramd_turnstile"))
        else . end
      | if (.services[env.ROLLOUT_COMPOSE_SERVICE // "teagramd"].secrets | type) == "array" and (.services[env.ROLLOUT_COMPOSE_SERVICE // "teagramd"].secrets | length) == 0 then
          del(.services[env.ROLLOUT_COMPOSE_SERVICE // "teagramd"].secrets)
        else . end
      | if (.secrets | type) == "object" then .secrets |= del(.teagramd_turnstile) else . end
      | if (.secrets | type) == "object" and (.secrets | length) == 0 then del(.secrets) else . end
      | .services |= with_entries(
          if .key == (env.ROLLOUT_COMPOSE_SERVICE // "teagramd") and (.value.volumes | type) == "array" then
            .value.volumes |= map(select((.type == "bind" and .source == $mode_source and
                                          .target == $mode_target and .read_only == true) | not))
          else . end)
      | .services |= with_entries(select(.key | IN("public-haproxy", "teagramd-public-1", "teagramd-public-2") | not))
      | if (.networks | type) == "object" then
          .networks |= with_entries(select(.key | IN("public-ingress", "public-backend") | not))
        else . end
      # The migration-only operator is validated as an isolated, read-only
      # profile above. It cannot affect the serving container while inactive.
      | if .services["blob-key-rotate"] != null then
          .services |= del(."blob-key-rotate")
          | .volumes |= del(.blob_key_rotate_reports)
          | if (.volumes | length) == 0 then del(.volumes) else . end
        else . end
    ')
    compose_replica_count=$(printf '%s' "$compose_json" | jq -er '
      .services[env.ROLLOUT_COMPOSE_SERVICE // "teagramd"].environment as $env
      | if ($env | type) != "object" then error("teagramd environment must be an object")
        elif ($env | has("TG_REPLICA_COUNT")) then
          if ($env.TG_REPLICA_COUNT | type) == "string" and ($env.TG_REPLICA_COUNT | test("^[0-9]{1,3}$")) then $env.TG_REPLICA_COUNT else "invalid" end
        else "unset" end
    ')
    compose_client_addr_trust=$(printf '%s' "$compose_json" | jq -er '
      .services[env.ROLLOUT_COMPOSE_SERVICE // "teagramd"].environment as $env
      | if ($env | type) != "object" then error("teagramd environment must be an object")
        elif ($env | has("TG_CLIENT_ADDR_TRUST")) then
          if ($env.TG_CLIENT_ADDR_TRUST | type) == "string" and $env.TG_CLIENT_ADDR_TRUST == "socket" then "socket" else "invalid" end
        else "unset" end
    ')
    grace=$(printf '%s' "$resolved_config" | jq -er '.stop_grace_period')
    config_sha=$(sha256_text "$canonical_compose")
    env_sha=$(sha256sum "$env_file" | awk '{print $1}')
    override_sha=$(sha256sum "$override_file" | awk '{print $1}')
  fi
  mount_count=$(printf '%s' "$mounts" | jq -er 'length')
  mounts_sha=$(sha256_text "$mounts")
  exposure_sha=$(sha256_text "$exposure_for_hash")
  container_env_sha=$(sha256_text "$container_env")

  jq -cnS \
    --arg container_id "$container_id" \
    --arg image_id "$image_id" \
    --arg state "$state" \
    --arg exit_code "$exit_code" \
    --arg started_at "$started_at" \
    --arg finished_at "$finished_at" \
    --arg grace "$grace" \
    --argjson stop_timeout "$stop_timeout" \
    --arg compose_replica_count "$compose_replica_count" \
    --arg compose_client_addr_trust "$compose_client_addr_trust" \
    --arg container_replica_count "$container_replica_count" \
    --arg container_client_addr_trust "$container_client_addr_trust" \
    --arg compose_public_ws_2446 "$compose_public_ws_2446" \
    --arg compose_logging_policy "$compose_logging_policy" \
    --arg compose_log_login_codes "$compose_log_login_codes" \
    --arg container_log_login_codes "$container_log_login_codes" \
    --arg media_key_source "$media_key_source" \
    --arg media_key_current "$media_key_current" \
    --arg exposure_public_ws_2446 "$exposure_public_ws_2446" \
    --arg mount_count "$mount_count" \
    --arg mounts_sha "$mounts_sha" \
    --arg exposure_sha "$exposure_sha" \
    --arg config_sha "$config_sha" \
    --arg env_sha "$env_sha" \
    --arg override_sha "$override_sha" \
    --arg container_env_sha "$container_env_sha" \
    --arg container_logging_sha "$container_logging_sha" \
    --arg container_env_added "$container_env_added" \
    --arg container_env_changed "$container_env_changed" \
    --arg compose_source "$compose_source" \
    --arg compose_selection "${COMPOSE_FILE:-unset}" \
    '{container_id:$container_id,image_id:$image_id,state:$state,exit_code:$exit_code,
      started_at:$started_at,finished_at:$finished_at,stop_grace_period:$grace,
      container_stop_timeout:$stop_timeout,
      compose_replica_count:$compose_replica_count,
      compose_client_addr_trust:$compose_client_addr_trust,
      compose_public_ws_2446:$compose_public_ws_2446,
      compose_logging_policy:$compose_logging_policy,
      compose_log_login_codes:$compose_log_login_codes,
      container_log_login_codes:$container_log_login_codes,
      media_key_source:$media_key_source,media_key_current:$media_key_current,
      exposure_public_ws_2446:$exposure_public_ws_2446,
      container_replica_count:$container_replica_count,
      container_client_addr_trust:$container_client_addr_trust,
      mount_count:($mount_count|tonumber),mounts_sha256:$mounts_sha,
      exposure_sha256:$exposure_sha,config_sha256:$config_sha,
      env_sha256:$env_sha,override_sha256:$override_sha,
      container_env_sha256:$container_env_sha,
      container_logging_sha256:$container_logging_sha,
      container_env_added:$container_env_added,
      container_env_changed:$container_env_changed,
      provenance:{container_source:"docker-inspect",compose_source:$compose_source,
        compose_selection:(if $compose_source == "selected-compose-render" then $compose_selection else "not-captured" end),
        stop_grace_period_source:(if $compose_source == "live-container-inspection" then "docker-inspect-config-stop-timeout" else "docker-compose-config" end)}}'
}

secure_evidence_dir() {
  local dir=$1
  if [ "$(id -u)" != 0 ]; then
    printf '%s\n' 'evidence operations require uid 0' >&2
    return 1
  fi
  case "$dir" in
    /root/*) ;;
    *) printf '%s\n' 'evidence directory must be below /root' >&2; return 1 ;;
  esac
  case "$dir" in
    /root/*/*) printf '%s\n' 'evidence directory must be a direct child of /root' >&2; return 1 ;;
  esac
  case "$dir" in
    *'/../'*|*/..|*'/./'*) printf '%s\n' 'evidence directory must be canonical' >&2; return 1 ;;
  esac
  if [ -L "$dir" ]; then
    printf '%s\n' 'evidence directory cannot be a symlink' >&2
    return 1
  fi
  if [ -e "$dir" ]; then
    if [ ! -d "$dir" ] || [ "$(stat -c %a "$dir")" != 700 ] || [ "$(stat -c %u "$dir")" != 0 ]; then
      printf '%s\n' 'existing evidence directory is not root-only mode 0700' >&2
      return 1
    fi
  else
    mkdir -m 700 "$dir"
  fi
  if [ "$(stat -c %a "$dir")" != 700 ] || [ "$(stat -c %u "$dir")" != 0 ]; then
    printf '%s\n' 'evidence directory is not root-only' >&2
    return 1
  fi
}

validate_container_id() {
  if ! [[ "$1" =~ ^[0-9a-f]{64}$ ]]; then
    printf '%s\n' 'container id must be a full Docker ID' >&2
    return 1
  fi
}

require_private_input() {
  local path=$1 dir=$2
  case "$path" in
    "$dir"/*) ;;
    *) printf '%s\n' 'snapshot input must be directly inside the evidence directory' >&2; return 1 ;;
  esac
  case "$path" in
    "$dir"/*/*) printf '%s\n' 'snapshot input path must be canonical' >&2; return 1 ;;
  esac
  if [ ! -f "$path" ] || [ -L "$path" ] || [ "$(stat -c %a "$path")" != 600 ] || [ "$(stat -c %u "$path")" != 0 ]; then
    printf '%s\n' 'snapshot input must be a root-owned mode-0600 regular file' >&2
    return 1
  fi
}

write_private_file() {
  local path=$1
  local contents=$2
  local temp="$path.tmp.$$"
  if [ -e "$path" ] || [ -L "$path" ]; then
    printf '%s\n' 'refusing to overwrite evidence' >&2
    return 1
  fi
  (umask 077; printf '%s\n' "$contents" > "$temp")
  chmod 600 "$temp"
  mv "$temp" "$path"
  chmod 600 "$path"
}

append_private_line() {
  local path=$1
  shift
  if [ ! -e "$path" ]; then
    (umask 077; : > "$path")
  fi
  chmod 600 "$path"
  printf '%s\n' "$*" >> "$path"
}

protect_comparison_file() {
  local output=$1 mode
  if ! chmod 600 "$output"; then
    return 1
  fi
  if ! mode=$(stat -c %a "$output"); then
    return 1
  fi
  [ "$mode" = 600 ]
}

record_comparison() {
  local output=$1
  local field=$2
  local before=$3
  local after=$4
  local result=$5
  local timestamp
  if ! timestamp=$(date -u +%Y-%m-%dT%H:%M:%SZ); then
    return 1
  fi
  if ! printf '%s\t%s\t%s\t%s\t%s\n' \
    "$timestamp" "$field" "$before" "$after" "$result" | tee -a "$output" >/dev/null; then
    return 1
  fi
  if ! protect_comparison_file "$output"; then
    return 1
  fi
  if [ "$result" != pass ]; then
    ROLLOUT_COMPARE_FAILURES=$((ROLLOUT_COMPARE_FAILURES + 1))
  fi
  return 0
}

record_ordinary_setting() {
  local output=$1 field=$2 before=$3 after=$4 expected=$5 result
  if { [ "$before" = unset ] || [ "$before" = "$expected" ]; } && [ "$after" = "$expected" ]; then
    result=pass
  else
    result=fail
  fi
  if ! record_comparison "$output" "$field" "$before" "$after" "$result"; then
    printf '%s\n' "failed to persist comparison result: $field" >&2
    return 1
  fi
  return 0
}

compare_snapshots() {
  local before_file=$1
  local after_file=$2
  local expected_image=$3
  local result_file=$4
  local field before_value after_value before_image after_image result before_logging_policy after_logging_policy
  local fields=(mounts_sha256 stop_grace_period container_stop_timeout config_sha256 env_sha256 override_sha256 exposure_sha256 container_env_sha256 container_logging_sha256 compose_logging_policy compose_log_login_codes container_log_login_codes media_key_source media_key_current)
  ROLLOUT_COMPARE_FAILURES=0
  if [ -e "$result_file" ] || [ -L "$result_file" ]; then
    printf '%s\n' 'comparison evidence already exists' >&2
    return 64
  fi
  if ! jq -e '
      def sha: test("^[0-9a-f]{64}$");
      type == "object" and
      (.container_id | test("^[0-9a-f]{64}$")) and
      (.image_id | test("^sha256:[0-9a-f]{64}$")) and
      (.state | IN("created","running","paused","restarting","removing","exited","dead")) and
      (.exit_code | test("^[0-9]+$")) and
      (.started_at | test("^[0-9TZ:.+-]+$")) and
      (.finished_at | test("^[0-9TZ:.+-]+$")) and
      (.stop_grace_period | test("^[0-9]+(m[0-9]+s|s)$")) and
      (.container_stop_timeout | type == "number" and . >= 0 and . == floor) and
      (.compose_replica_count | type == "string" and test("^(unset|invalid|[0-9]{1,3})$")) and
      (.compose_client_addr_trust | type == "string" and IN("unset","invalid","socket")) and
      (.compose_public_ws_2446 | type == "string" and IN("true","false","not-captured")) and
      (.compose_logging_policy | type == "string" and IN("default","syslog-v1","not-captured")) and
      (.compose_log_login_codes | type == "string" and IN("true","false","unset","not-captured")) and
      (.container_log_login_codes | type == "string" and IN("true","false","unset")) and
      (.media_key_source | type == "string" and (. == "unset" or startswith("/"))) and
      (.media_key_current | type == "string" and (. == "unset" or test("^[1-9][0-9]*$"))) and
      (.exposure_public_ws_2446 | type == "string" and IN("true","false")) and
      (.container_replica_count | type == "string" and test("^(unset|invalid|duplicate|[0-9]{1,3})$")) and
      (.container_client_addr_trust | type == "string" and IN("unset","invalid","duplicate","socket")) and
      (.mount_count | type == "number" and . >= 0) and
      (.mounts_sha256 | sha) and (.exposure_sha256 | sha) and
      (.config_sha256 | sha) and (.env_sha256 | sha) and (.override_sha256 | sha) and
      (.container_env_sha256 | sha) and (.container_logging_sha256 | sha)
    ' "$before_file" >/dev/null \
    || ! jq -e '
      def sha: test("^[0-9a-f]{64}$");
      type == "object" and
      (.container_id | test("^[0-9a-f]{64}$")) and
      (.image_id | test("^sha256:[0-9a-f]{64}$")) and
      (.state | IN("created","running","paused","restarting","removing","exited","dead")) and
      (.exit_code | test("^[0-9]+$")) and
      (.started_at | test("^[0-9TZ:.+-]+$")) and
      (.finished_at | test("^[0-9TZ:.+-]+$")) and
      (.stop_grace_period | test("^[0-9]+(m[0-9]+s|s)$")) and
      (.container_stop_timeout | type == "number" and . >= 0 and . == floor) and
      (.compose_replica_count | type == "string" and test("^(unset|invalid|[0-9]{1,3})$")) and
      (.compose_client_addr_trust | type == "string" and IN("unset","invalid","socket")) and
      (.compose_public_ws_2446 | type == "string" and IN("true","false","not-captured")) and
      (.compose_logging_policy | type == "string" and IN("default","syslog-v1","not-captured")) and
      (.compose_log_login_codes | type == "string" and IN("true","false","unset","not-captured")) and
      (.container_log_login_codes | type == "string" and IN("true","false","unset")) and
      (.media_key_source | type == "string" and (. == "unset" or startswith("/"))) and
      (.media_key_current | type == "string" and (. == "unset" or test("^[1-9][0-9]*$"))) and
      (.exposure_public_ws_2446 | type == "string" and IN("true","false")) and
      (.container_replica_count | type == "string" and test("^(unset|invalid|duplicate|[0-9]{1,3})$")) and
      (.container_client_addr_trust | type == "string" and IN("unset","invalid","duplicate","socket")) and
      (.mount_count | type == "number" and . >= 0) and
      (.mounts_sha256 | sha) and (.exposure_sha256 | sha) and
      (.config_sha256 | sha) and (.env_sha256 | sha) and (.override_sha256 | sha) and
      (.container_env_sha256 | sha) and (.container_logging_sha256 | sha)
    ' "$after_file" >/dev/null \
    || ! [[ "$expected_image" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    printf '%s\n' 'invalid snapshot schema' >&2
    return 65
  fi
  if ! (umask 077; : > "$result_file"); then
    printf '%s\n' 'comparison evidence initialization failed' >&2
    return 1
  fi
  if ! protect_comparison_file "$result_file"; then
    printf '%s\n' 'comparison evidence permission setup failed' >&2
    return 1
  fi
  before_logging_policy=$(jq -er '.compose_logging_policy' "$before_file") || return 1
  after_logging_policy=$(jq -er '.compose_logging_policy' "$after_file") || return 1
  for field in "${fields[@]}"; do
    if ! before_value=$(jq -er --arg field "$field" '.[$field]' "$before_file"); then return 1; fi
    if ! after_value=$(jq -er --arg field "$field" '.[$field]' "$after_file"); then return 1; fi
    if [ "$field" = compose_logging_policy ]; then
      if approved_logging_transition "$before_value" "$after_value"; then result=pass; else result=fail; fi
    elif [ "$field" = compose_log_login_codes ] || [ "$field" = container_log_login_codes ]; then
      if approved_login_code_logging_transition "$before_value" "$after_value"; then result=pass; else result=fail; fi
    elif [ "$field" = media_key_source ] && [ "$before_value" = unset ] && [[ "$after_value" = /* ]]; then
      result=pass
    elif [ "$field" = media_key_current ] && [ "$before_value" = unset ] && [ "$after_value" = 1 ]; then
      result=pass
    elif [ "$field" = container_logging_sha256 ] && \
         [ "$before_logging_policy" = default ] && [ "$after_logging_policy" = syslog-v1 ]; then
      result=pass
    elif [ "$before_value" = "$after_value" ]; then result=pass; else result=fail; fi
    if ! record_comparison "$result_file" "$field" "$before_value" "$after_value" "$result"; then
      printf '%s\n' "failed to persist comparison result: $field" >&2
      return 1
    fi
  done
  for field in compose_public_ws_2446 exposure_public_ws_2446; do
    if ! before_value=$(jq -er --arg field "$field" '.[$field]' "$before_file"); then return 1; fi
    if ! after_value=$(jq -er --arg field "$field" '.[$field]' "$after_file"); then return 1; fi
    if [ "$before_value" = "$after_value" ] || \
       { [ "$before_value" = false ] && [ "$after_value" = true ]; } || \
       { [ "${ROLLOUT_PUBLIC_WEBSOCKET:-0}" = 1 ] && [ "$before_value" = true ] && [ "$after_value" = false ]; }; then
      result=pass
    else
      result=fail
    fi
    if ! record_comparison "$result_file" "$field" "$before_value" "$after_value" "$result"; then
      printf '%s\n' "failed to persist comparison result: $field" >&2
      return 1
    fi
  done
  if ! before_value=$(jq -er '.compose_replica_count' "$before_file"); then return 1; fi
  if ! after_value=$(jq -er '.compose_replica_count' "$after_file"); then return 1; fi
  if ! record_ordinary_setting "$result_file" resolved_TG_REPLICA_COUNT "$before_value" "$after_value" 1; then return 1; fi
  if ! before_value=$(jq -er '.compose_client_addr_trust' "$before_file"); then return 1; fi
  if ! after_value=$(jq -er '.compose_client_addr_trust' "$after_file"); then return 1; fi
  if ! record_ordinary_setting "$result_file" resolved_TG_CLIENT_ADDR_TRUST "$before_value" "$after_value" socket; then return 1; fi
  if ! before_value=$(jq -er '.container_replica_count' "$before_file"); then return 1; fi
  if ! after_value=$(jq -er '.container_replica_count' "$after_file"); then return 1; fi
  if ! record_ordinary_setting "$result_file" running_TG_REPLICA_COUNT "$before_value" "$after_value" 1; then return 1; fi
  if ! before_value=$(jq -er '.container_client_addr_trust' "$before_file"); then return 1; fi
  if ! after_value=$(jq -er '.container_client_addr_trust' "$after_file"); then return 1; fi
  if ! record_ordinary_setting "$result_file" running_TG_CLIENT_ADDR_TRUST "$before_value" "$after_value" socket; then return 1; fi
  if ! before_value=$(jq -er '.container_id' "$before_file"); then return 1; fi
  if ! after_value=$(jq -er '.container_id' "$after_file"); then return 1; fi
  if ! before_image=$(jq -er '.image_id' "$before_file"); then return 1; fi
  if ! after_image=$(jq -er '.image_id' "$after_file"); then return 1; fi
  if [ "$after_value" = "$before_value" ]; then
    if [ "$before_image" = "$expected_image" ]; then result=pass; else result=fail; fi
    if ! record_comparison "$result_file" baseline_image_matches_built "$before_image" "$expected_image" "$result"; then return 1; fi
    if [ "$before_image" = "$expected_image" ] && [ "$after_image" = "$expected_image" ]; then result=pass; else result=fail; fi
    if ! record_comparison "$result_file" container_reused "$before_value" "$after_value" "$result"; then return 1; fi
  else
    if ! record_comparison "$result_file" container_replaced "$before_value" "$after_value" pass; then return 1; fi
  fi
  if [ "$after_image" = "$expected_image" ]; then result=pass; else result=fail; fi
  if ! record_comparison "$result_file" target_image_matches_built "$expected_image" "$after_image" "$result"; then return 1; fi
  if ! after_value=$(jq -er '.state' "$after_file"); then return 1; fi
  if [ "$after_value" = running ]; then result=pass; else result=fail; fi
  if ! record_comparison "$result_file" target_state "$after_value" "$after_value" "$result"; then return 1; fi
  if ! before_value=$(jq -er '.stop_grace_period' "$before_file"); then return 1; fi
  if [ "$before_value" = "$ROLLOUT_REQUIRED_GRACE" ]; then result=pass; else result=fail; fi
  if ! record_comparison "$result_file" baseline_grace_policy "$ROLLOUT_REQUIRED_GRACE" "$before_value" "$result"; then return 1; fi
  if ! after_value=$(jq -er '.stop_grace_period' "$after_file"); then return 1; fi
  if [ "$after_value" = "$ROLLOUT_REQUIRED_GRACE" ]; then result=pass; else result=fail; fi
  if ! record_comparison "$result_file" target_grace_policy "$ROLLOUT_REQUIRED_GRACE" "$after_value" "$result"; then return 1; fi
  if ! before_value=$(jq -er '.container_stop_timeout' "$before_file"); then return 1; fi
  if [ "$before_value" = 120 ]; then result=pass; else result=fail; fi
  if ! record_comparison "$result_file" baseline_inspected_stop_timeout 120 "$before_value" "$result"; then return 1; fi
  if ! after_value=$(jq -er '.container_stop_timeout' "$after_file"); then return 1; fi
  if [ "$after_value" = 120 ]; then result=pass; else result=fail; fi
  if ! record_comparison "$result_file" target_inspected_stop_timeout 120 "$after_value" "$result"; then return 1; fi
  if ! sync; then
    printf '%s\n' 'comparison evidence sync failed' >&2
    return 1
  fi
  if [ "$ROLLOUT_COMPARE_FAILURES" -eq 0 ]; then return 0; fi
  return 1
}

readiness_sample_ok() {
  local sample=$1
  printf '%s' "$sample" | jq -e '
    .container_state == "running" and
    (.advertise_count | type == "number" and . > 0) and
    (.error_count | type == "number" and . == 0) and
    .migrate_state == "exited" and
    (.migrate_exit | tonumber) == 0 and
    .postgres_health == "healthy" and
    .tcp == "connected" and
    .tls == "verified"
  ' >/dev/null
}

readiness_sample_terminal_failure() {
  local sample=$1
  printf '%s' "$sample" | jq -e '
    (.container_state == "exited" or .container_state == "dead") or
    (.error_count | type == "number" and . > 0)
  ' >/dev/null
}

append_readiness_sample() {
  local output=$1 role=$2 container_id=$3 elapsed=$4 sample=$5
  printf '%s\trole=%s\telapsed=%s\tcontainer_id=%s\tstate=%s\texit=%s\tadvertise=%s\terrors=%s\tmigrate=%s/%s\tpostgres=%s\ttcp=%s\ttls=%s\ttls_http=%s\ttls_verify=%s\n' \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$role" "$elapsed" "$container_id" \
    "$(printf '%s' "$sample" | jq -r '.container_state')" \
    "$(printf '%s' "$sample" | jq -r '.container_exit')" \
    "$(printf '%s' "$sample" | jq -r '.advertise_count')" \
    "$(printf '%s' "$sample" | jq -r '.error_count')" \
    "$(printf '%s' "$sample" | jq -r '.migrate_state')" \
    "$(printf '%s' "$sample" | jq -r '.migrate_exit')" \
    "$(printf '%s' "$sample" | jq -r '.postgres_health')" \
    "$(printf '%s' "$sample" | jq -r '.tcp')" \
    "$(printf '%s' "$sample" | jq -r '.tls')" \
    "$(printf '%s' "$sample" | jq -r '.tls_http_status')" \
    "$(printf '%s' "$sample" | jq -r '.tls_verify')" >> "$output"
  chmod 600 "$output"
}

wait_readiness() {
  local role=$1 container_id=$2 output=$3 max_seconds=$4 interval=$5 probe_fn=$6
  local start=$SECONDS elapsed remaining sample
  if [ "$role" != target ] && [ "$role" != rollback ]; then
    printf '%s\n' 'readiness role must be target or rollback' >&2
    return 64
  fi
  if ! [[ "$max_seconds" =~ ^[0-9]+$ ]] || [ "$max_seconds" -lt 1 ] || [ "$max_seconds" -gt "$ROLLOUT_MAX_READY_SECONDS" ]; then
    printf '%s\n' 'readiness bound must be between 1 and 120 seconds' >&2
    return 64
  fi
  if ! [[ "$interval" =~ ^[0-9]+$ ]] || [ "$interval" -lt 1 ] || [ "$interval" -gt 5 ]; then
    printf '%s\n' 'readiness interval must be between 1 and 5 seconds' >&2
    return 64
  fi
  if [ -e "$output" ] || [ -L "$output" ]; then
    printf '%s\n' 'readiness evidence already exists' >&2
    return 64
  fi
  (umask 077; : > "$output")
  chmod 600 "$output"
  while [ $((SECONDS - start)) -le "$max_seconds" ]; do
    elapsed=$((SECONDS - start))
    if ! sample=$("$probe_fn" "$container_id" "$role"); then
      sample='{"container_state":"unknown","container_exit":"unknown","advertise_count":"unknown","error_count":"unknown","migrate_state":"unknown","migrate_exit":"unknown","postgres_health":"unknown","tcp":"failed","tls":"failed","tls_http_status":"000","tls_verify":"unknown"}'
    fi
    append_readiness_sample "$output" "$role" "$container_id" "$elapsed" "$sample"
    if readiness_sample_ok "$sample"; then
      append_private_line "$output.result" "role=$role result=ready elapsed=$elapsed"
      sync
      return 0
    fi
    if readiness_sample_terminal_failure "$sample"; then
      append_private_line "$output.result" "role=$role result=failed elapsed=$elapsed"
      sync
      return 1
    fi
    elapsed=$((SECONDS - start))
    remaining=$((max_seconds - elapsed))
    if [ "$remaining" -le 0 ]; then break; fi
    if [ "$remaining" -lt "$interval" ]; then sleep "$remaining"; else sleep "$interval"; fi
  done
  append_private_line "$output.result" "role=$role result=timeout max_seconds=$max_seconds elapsed=$((SECONDS - start))"
  sync
  return 2
}

probe_live() {
  local container_id=$1 role=$2 container_line state exit_code logs advertise_count error_count
  local migrate_id migrate_line migrate_state migrate_exit postgres_id postgres_health tcp tls_out tls tls_http_status tls_verify
  container_line=$(timeout 3 docker inspect "$container_id" --format '{{.State.Status}}|{{.State.ExitCode}}' 2>/dev/null || printf 'unknown|unknown')
  IFS='|' read -r state exit_code <<< "$container_line"
  if logs=$(timeout 5 docker logs "$container_id" 2>&1 | awk '/advertise=teagram\.tailaa4918\.ts\.net:2443/{advertise++} /level=ERROR/{errors++} END{printf "%d %d", advertise+0, errors+0}'); then
    read -r advertise_count error_count <<< "$logs"
  else
    advertise_count=unknown
    error_count=unknown
  fi
  migrate_id=$(timeout 3 docker compose ps -aq migrate </dev/null 2>/dev/null | head -n 1 || true)
  if [ -n "$migrate_id" ]; then
    migrate_line=$(timeout 3 docker inspect "$migrate_id" --format '{{.State.Status}}|{{.State.ExitCode}}' 2>/dev/null || printf 'unknown|unknown')
    IFS='|' read -r migrate_state migrate_exit <<< "$migrate_line"
  else
    migrate_state=missing
    migrate_exit=unknown
  fi
  postgres_id=$(timeout 3 docker compose ps -q postgres </dev/null 2>/dev/null | head -n 1 || true)
  if [ -n "$postgres_id" ]; then
    postgres_health=$(timeout 3 docker inspect "$postgres_id" --format '{{.State.Health.Status}}' 2>/dev/null || printf 'unknown')
  else
    postgres_health=missing
  fi
  if timeout 2 nc -z -w1 "$ROLLOUT_TCP_HOST" "$ROLLOUT_TCP_PORT" >/dev/null 2>&1; then tcp=connected; else tcp=failed; fi
  tls_out=$(timeout 7 curl -q --connect-timeout 3 --max-time 5 -sS -o /dev/null -w '%{http_code} %{ssl_verify_result}' "$ROLLOUT_ADMIN_URL" 2>/dev/null || true)
  read -r tls_http_status tls_verify <<< "$tls_out"
  tls_http_status=${tls_http_status:-000}
  tls_verify=${tls_verify:-unknown}
  if [[ "$tls_out" =~ ^(2[0-9][0-9]|3[0-9][0-9]|401|403)[[:space:]]0$ ]]; then tls=verified; else tls=failed; fi
  jq -cnS \
    --arg container_state "$state" --arg container_exit "$exit_code" \
    --arg advertise_count "$advertise_count" --arg error_count "$error_count" \
    --arg migrate_state "$migrate_state" --arg migrate_exit "$migrate_exit" \
    --arg postgres_health "$postgres_health" --arg tcp "$tcp" --arg tls "$tls" \
    --arg tls_http_status "$tls_http_status" --arg tls_verify "$tls_verify" \
    --arg role "$role" \
    '{role:$role,container_state:$container_state,container_exit:$container_exit,
      advertise_count:(if ($advertise_count | test("^[0-9]+$")) then ($advertise_count|tonumber) else $advertise_count end),
      error_count:(if ($error_count | test("^[0-9]+$")) then ($error_count|tonumber) else $error_count end),
      migrate_state:$migrate_state,
      migrate_exit:$migrate_exit,postgres_health:$postgres_health,tcp:$tcp,tls:$tls,
      tls_http_status:$tls_http_status,tls_verify:$tls_verify}'
}

live_ready_command() {
  local role=$1 container_id=$2 evidence_dir=$3 output result_file rc max_seconds=${ROLLOUT_READY_SECONDS:-120}
  if [ "$PWD" != "$ROLLOUT_CHECKOUT_PATH" ]; then
    printf '%s\n' 'readiness must run from the teagram-server checkout' >&2
    return 64
  fi
  if ! [[ "$max_seconds" =~ ^[0-9]+$ ]] || [ "$max_seconds" -lt 1 ] || [ "$max_seconds" -gt 120 ]; then
    printf '%s\n' 'readiness bound must be between 1 and 120 seconds' >&2
    return 64
  fi
  validate_container_id "$container_id"
  secure_evidence_dir "$evidence_dir"
  output="$evidence_dir/readiness-$role.tsv"
  result_file="$evidence_dir/readiness-$role.result"
  if [ -e "$output" ] || [ -e "$result_file" ]; then
    printf '%s\n' 'readiness evidence already exists' >&2
    return 64
  fi
  # The inner loop stops probing at max_seconds; the small outer margin lets it
  # fsync its bounded-timeout evidence before the safety timeout can terminate it.
  if timeout "$((max_seconds + 2))" bash "$ROLLOUT_VERIFIER_PATH" __ready-loop "$role" "$container_id" "$output" >/dev/null 2>&1; then
    rc=0
  else
    rc=$?
  fi
  case "$rc" in
    0) result="role=$role result=ready max_seconds=$max_seconds" ;;
    2|124) result="role=$role result=bounded_timeout max_seconds=$max_seconds" ;;
    *) result="role=$role result=failed exit=$rc max_seconds=$max_seconds" ;;
  esac
  write_private_file "$result_file" "$result"
  printf '%s\n' "$result"
  [ "$rc" -eq 0 ]
}

snapshot_command() {
  local container_id=$1 evidence_dir=$2 name=$3 env_file=$4 override_file=$5
  local compose_source=${6:-selected-compose-render}
  local inspect_json compose_json image_env_json image_id snapshot output
  secure_evidence_dir "$evidence_dir"
  validate_container_id "$container_id"
  if [ "$PWD" != "$ROLLOUT_CHECKOUT_PATH" ] || [ "$env_file" != "$ROLLOUT_CHECKOUT_PATH/.env" ] || [ "$override_file" != "$ROLLOUT_CHECKOUT_PATH/docker-compose.override.yml" ]; then
    printf '%s\n' 'snapshot must run from the teagram-server checkout with its configured secret and override paths' >&2
    return 64
  fi
  case "$name" in *[!A-Za-z0-9._-]*|'') printf '%s\n' 'invalid evidence name' >&2; return 64 ;; esac
  case "$compose_source" in
    selected-compose-render|live-container-inspection) ;;
    *) printf '%s\n' 'invalid snapshot source' >&2; return 64 ;;
  esac
  output="$evidence_dir/$name.snapshot.json"
  inspect_json=$(docker inspect "$container_id" 2>/dev/null) || { printf '%s\n' 'container snapshot unavailable' >&2; return 1; }
  if [ "$compose_source" = selected-compose-render ]; then
    compose_json=$(docker compose config --format json </dev/null 2>/dev/null) || { printf '%s\n' 'resolved compose configuration unavailable' >&2; return 1; }
  else
    compose_json='{}'
  fi
  image_id=$(printf '%s' "$inspect_json" | jq -er 'def item: if type == "array" then .[0] else . end; item.Image | select(type == "string" and length > 0)') || {
    printf '%s\n' 'container image identifier unavailable' >&2
    return 1
  }
  image_env_json=$(docker image inspect --format '{{json .Config.Env}}' "$image_id" 2>/dev/null) || {
    printf '%s\n' 'container image environment unavailable' >&2
    return 1
  }
  image_env_json=$(jq -ce 'if . == null then [] elif type == "array" and all(.[]; type == "string") then . else error("invalid image environment") end' <<< "$image_env_json") || {
    printf '%s\n' 'container image environment is invalid' >&2
    return 1
  }
  snapshot=$(snapshot_from_json "$inspect_json" "$compose_json" "$env_file" "$override_file" "$compose_source" "$name" "${COMPOSE_FILE:-}" "$image_env_json") || { printf '%s\n' 'allowlisted snapshot generation failed' >&2; return 1; }
  write_private_file "$output" "$snapshot"
  printf 'snapshot_written=%s container_id=%s image_id=%s\n' "$output" \
    "$(printf '%s' "$snapshot" | jq -r '.container_id')" \
    "$(printf '%s' "$snapshot" | jq -r '.image_id')"
}

compare_command() {
  local before=$1 after=$2 expected_image=$3 evidence_dir=$4 name=$5 results
  local rc
  secure_evidence_dir "$evidence_dir"
  case "$name" in *[!A-Za-z0-9._-]*|'') printf '%s\n' 'invalid evidence name' >&2; return 64 ;; esac
  require_private_input "$before" "$evidence_dir"
  require_private_input "$after" "$evidence_dir"
  results="$evidence_dir/$name-comparisons.tsv"
  if compare_snapshots "$before" "$after" "$expected_image" "$results"; then
    printf 'comparison_result=pass checks=%s evidence=%s\n' "$(wc -l < "$results" | tr -d ' ')" "$results"
  else
    rc=$?
    printf 'comparison_result=fail checks=%s evidence=%s\n' "$(wc -l < "$results" | tr -d ' ')" "$results"
    return "$rc"
  fi
}

secure_replay_evidence_dir() {
  local dir=$1 canonical owner=0
  if [ "${ROLLOUT_PREFLIGHT_TEST_MODE:-0}" = 1 ]; then
    owner=$(id -u) || return 1
  else
    [ "$(id -u)" = 0 ] || { printf '%s\n' 'preflight replay requires uid 0' >&2; return 1; }
  fi
  case "$dir" in
    /tmp/*) ;;
    *) printf '%s\n' 'preflight replay evidence must be directly below /tmp' >&2; return 1 ;;
  esac
  case "$dir" in
    /tmp/*/*) printf '%s\n' 'preflight replay evidence must be directly below /tmp' >&2; return 1 ;;
  esac
  case "$dir" in
    *'/../'*|*/..|*'/./'*) printf '%s\n' 'preflight replay evidence path must be canonical' >&2; return 1 ;;
  esac
  [ -d "$dir" ] && [ ! -L "$dir" ] || { printf '%s\n' 'preflight replay evidence directory is unavailable' >&2; return 1; }
  canonical=$(readlink -f -- "$dir") || return 1
  [ "$canonical" = "$dir" ] && [ "$(stat -c %u:%a -- "$dir")" = "$owner:700" ] || {
    printf '%s\n' 'preflight replay evidence has an invalid owner or mode' >&2
    return 1
  }
}

preflight_snapshot_command() {
  local container_id=$1 project_dir=$2 evidence_dir=$3 name=$4 project_name=$5 selection=$6
  local env_file override_file compose_files_value='' compose_json inspect_json snapshot output entry file canonical owner=0
  local -a compose_entries=() compose_args=()
  secure_replay_evidence_dir "$evidence_dir" || return 1
  if [ "${ROLLOUT_PREFLIGHT_TEST_MODE:-0}" = 1 ]; then owner=$(id -u) || return 1; fi
  case "$project_dir" in
    "$evidence_dir"/*) ;;
    *) printf '%s\n' 'preflight replay checkout must be inside its evidence directory' >&2; return 64 ;;
  esac
  case "$project_dir" in
    *'/../'*|*/..|*'/./'*) printf '%s\n' 'preflight replay checkout path must be canonical' >&2; return 64 ;;
  esac
  [ -d "$project_dir" ] && [ ! -L "$project_dir" ] || { printf '%s\n' 'preflight replay checkout is unavailable' >&2; return 1; }
  canonical=$(readlink -f -- "$project_dir") || return 1
  [ "$canonical" = "$project_dir" ] || { printf '%s\n' 'preflight replay checkout path must be canonical' >&2; return 64; }
  [ "$(stat -c %u:%a -- "$project_dir")" = "$owner:700" ] || {
    printf '%s\n' 'preflight replay checkout has an invalid owner or mode' >&2
    return 1
  }
  validate_container_id "$container_id" || return 64
  case "$name" in
    baseline|preflight) ;;
    *) printf '%s\n' 'preflight replay snapshot name is invalid' >&2; return 64 ;;
  esac
  [[ "$project_name" =~ ^[a-z0-9][a-z0-9_-]*$ ]] || {
    printf '%s\n' 'preflight replay project name is invalid' >&2
    return 64
  }
  [ -n "$selection" ] || { printf '%s\n' 'preflight replay Compose selection is empty' >&2; return 64; }
  IFS=, read -r -a compose_entries <<< "$selection"
  [ "${#compose_entries[@]}" -gt 0 ] || { printf '%s\n' 'preflight replay Compose selection is empty' >&2; return 64; }
  for entry in "${compose_entries[@]}"; do
    [[ "$entry" =~ ^[A-Za-z0-9._/-]+$ ]] && [ "$entry" != .. ] && \
      [[ "$entry" != ../* && "$entry" != */../* && "$entry" != */.. && "$entry" != *//* ]] || {
      printf '%s\n' 'preflight replay Compose selection is invalid' >&2
      return 64
    }
    file="$project_dir/$entry"
    [ -f "$file" ] && [ ! -L "$file" ] || { printf '%s\n' 'selected preflight Compose file is unavailable' >&2; return 1; }
    canonical=$(readlink -f -- "$file") || return 1
    [ "$canonical" = "$file" ] || { printf '%s\n' 'selected preflight Compose file is symlinked' >&2; return 1; }
    if [ -n "$compose_files_value" ]; then compose_files_value+=:; fi
    compose_files_value+="$file"
    compose_args+=(--file "$file")
  done
  env_file="$project_dir/.env"
  override_file="$project_dir/docker-compose.override.yml"
  for file in "$env_file" "$override_file"; do
    [ -f "$file" ] && [ ! -L "$file" ] || { printf '%s\n' 'live Compose input is unavailable' >&2; return 1; }
    canonical=$(readlink -f -- "$file") || return 1
    [ "$canonical" = "$file" ] || { printf '%s\n' 'live Compose input is symlinked' >&2; return 1; }
  done
  [ "$(stat -c %u:%a -- "$env_file")" = "$owner:600" ] || {
    printf '%s\n' 'live environment file has an invalid owner or mode' >&2
    return 1
  }
  [ ! -e "$evidence_dir/$name.snapshot.json" ] && [ ! -L "$evidence_dir/$name.snapshot.json" ] || {
    printf '%s\n' 'preflight replay snapshot already exists' >&2
    return 64
  }

  unset COMPOSE_PROFILES COMPOSE_ENV_FILES
  COMPOSE_FILE=$compose_files_value
  COMPOSE_PROJECT_NAME=$project_name
  COMPOSE_EXPERIMENTAL_GIT_REMOTE=false
  COMPOSE_EXPERIMENTAL_OCI_REMOTE=false
  XDG_CACHE_HOME="$evidence_dir/compose-cache"
  ROLLOUT_CHECKOUT_PATH=$project_dir
  ROLLOUT_COMPOSE_SERVICE=teagramd
  ROLLOUT_ADDED_ENV=
  ROLLOUT_CHANGED_ENV=
  export COMPOSE_FILE COMPOSE_PROJECT_NAME COMPOSE_EXPERIMENTAL_GIT_REMOTE
  export COMPOSE_EXPERIMENTAL_OCI_REMOTE XDG_CACHE_HOME
  export ROLLOUT_CHECKOUT_PATH ROLLOUT_COMPOSE_SERVICE
  export ROLLOUT_ADDED_ENV ROLLOUT_CHANGED_ENV
  inspect_json=$(docker inspect "$container_id" 2>/dev/null) || {
    printf '%s\n' 'cannot inspect the live teagramd container' >&2
    return 1
  }
  compose_json=$(docker compose --project-directory "$project_dir" --project-name "$project_name" \
    "${compose_args[@]}" config --format json </dev/null 2>/dev/null) || {
    printf '%s\n' 'cannot render the selected Compose files' >&2
    return 1
  }
  snapshot=$(snapshot_from_json "$inspect_json" "$compose_json" "$env_file" "$override_file" \
    selected-compose-render "$name" "$compose_files_value") || {
    printf '%s\n' 'preflight replay snapshot validation failed' >&2
    return 1
  }
  output="$evidence_dir/$name.snapshot.json"
  write_private_file "$output" "$snapshot" || {
    printf '%s\n' 'cannot write private preflight replay snapshot' >&2
    return 1
  }
}

main() {
  [ "$#" -ge 1 ] || { printf '%s\n' 'commands: preflight-snapshot, snapshot, compare, ready, ready-pair' >&2; return 64; }
  local command=$1
  shift
  case "$command" in
    preflight-snapshot)
      [ "$#" -eq 6 ] || { printf '%s\n' 'usage: rollout-verifier.sh preflight-snapshot CONTAINER_ID CHECKOUT EVIDENCE_DIR baseline|preflight PROJECT_NAME COMPOSE_SELECTION' >&2; return 64; }
      preflight_snapshot_command "$@" ;;
    snapshot)
      [ "$#" -ge 5 ] && [ "$#" -le 6 ] || { printf '%s\n' 'usage: rollout-verifier.sh snapshot CONTAINER_ID EVIDENCE_DIR NAME ENV_FILE OVERRIDE_FILE [selected-compose-render|live-container-inspection]' >&2; return 64; }
      snapshot_command "$@" ;;
    compare)
      [ "$#" -eq 5 ] || { printf '%s\n' 'usage: rollout-verifier.sh compare BEFORE AFTER EXPECTED_IMAGE_ID EVIDENCE_DIR NAME' >&2; return 64; }
      compare_command "$@" ;;
    ready)
      [ "$#" -eq 3 ] || { printf '%s\n' 'usage: rollout-verifier.sh ready target|rollback CONTAINER_ID EVIDENCE_DIR' >&2; return 64; }
      live_ready_command "$@" ;;
    ready-pair)
      [ "$#" -eq 3 ] || { printf '%s\n' 'usage: rollout-verifier.sh ready-pair TARGET_CONTAINER_ID ROLLBACK_CONTAINER_ID EVIDENCE_DIR' >&2; return 64; }
      local target_rc=0 rollback_rc=0
      if live_ready_command target "$1" "$3"; then :; else target_rc=$?; fi
      if live_ready_command rollback "$2" "$3"; then :; else rollback_rc=$?; fi
      if [ "$target_rc" -ne 0 ] || [ "$rollback_rc" -ne 0 ]; then return 1; fi ;;
    __ready-loop)
      [ "$#" -eq 3 ] || return 64
      if [ "$(id -u)" != 0 ]; then return 64; fi
      secure_evidence_dir "$(dirname "$3")"
      wait_readiness "$1" "$2" "$3" "${ROLLOUT_READY_SECONDS:-120}" 5 probe_live ;;
    *)
      printf '%s\n' 'commands: preflight-snapshot, snapshot, compare, ready, ready-pair' >&2
      return 64 ;;
  esac
}

if [ "$ROLLOUT_VERIFIER_SOURCE_ONLY" != 1 ]; then
  main "$@"
fi
