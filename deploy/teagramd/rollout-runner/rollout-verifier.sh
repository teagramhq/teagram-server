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
    .[0] as $inspect
    | .[1] as $compose
    | ($inspect | item | .Config.Env // []) as $container_env
    | ($compose.services.teagramd.environment // null) as $compose_env
    | if ($container_env | type) != "array" or ($compose_env | type) != "object" then false else
        ($env_content | split("\n")) as $env_lines
        | all($added[];
            . as $name
            | ($name | runtime_name($name)) as $runtime_name
            | [ $env_lines[] as $line
                | ($line | index("=")) as $separator
                | select($separator != null and $line[:$separator] == $name)
              ] as $assignments
            | [ $container_env[]
                | select(type == "string" and startswith($runtime_name + "="))
                | .[($runtime_name | length) + 1:]
              ] as $actual
            | ($assignments | length) == 1 and
              ($compose_env | has($runtime_name)) and
              ($compose_env[$runtime_name] | type) == "string" and
              ($actual | length) == 1 and
              $actual[0] == $compose_env[$runtime_name]
          )
      end
  ' >/dev/null; then
    printf '%s\n' 'named environment additions do not match the resolved Compose environment' >&2
    return 1
  fi
  return 0
}

verify_changed_container_environment() {
  local inspect_json=$1 compose_json=$2 env_file=$3 changed_env_names=$4
  [ "$changed_env_names" != "[]" ] || return 0
  if ! {
    printf '%s\n' "$inspect_json" "$compose_json"
  } | jq -se --argjson changed "$changed_env_names" --rawfile env_content "$env_file" '
    def item: if type == "array" then .[0] else . end;
    .[0] as $inspect
    | .[1] as $compose
    | ($inspect | item | .Config.Env // []) as $container_env
    | ($compose.services.teagramd.environment // null) as $compose_env
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
            | (($name == "TG_REGISTRATION" and ($assignments[0] | IN("open","closed"))) or
               ($name == "TG_TURNSTILE_SIGNIN_ENABLED" and ($assignments[0] | IN("true","false")))) and
              ($assignments | length) == 1 and
              ($compose_env | has($name)) and
              ($compose_env[$name] == $assignments[0]) and
              ($actual | length) == 1 and
              ($actual[0] == $assignments[0])
          )
      end
  ' >/dev/null; then
    printf '%s\n' 'named environment value changes do not match the resolved Compose environment' >&2
    return 1
  fi
  return 0
}

verify_safe_public_container_environment() {
  local inspect_json=$1
  if ! printf '%s\n' "$inspect_json" | jq -e '
    def item: if type == "array" then .[0] else . end;
    def safe_setting($env; $name; $allowed):
      [$env[]
       | select(type == "string" and startswith($name + "="))
       | .[($name | length) + 1:]] as $values
      | ($values | length) <= 1 and
        all($values[]; . as $value | any($allowed[]; . == $value));
    (item.Config.Env // null) as $env
    | if ($env | type) != "array" then false else
        safe_setting($env; "TG_PUBLIC_WEBSOCKET_LISTEN_ADDR"; [""]) and
        safe_setting($env; "TG_PUBLIC_REGISTRATION"; ["closed"]) and
        safe_setting($env; "TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS"; [""]) and
        safe_setting($env; "TG_WELCOME_CHANNEL"; [""])
      end
  ' >/dev/null; then
    printf '%s\n' 'container has unsafe public environment settings' >&2
    return 1
  fi
  return 0
}

verify_safe_public_compose_environment() {
  local compose_json=$1
  if ! printf '%s\n' "$compose_json" | jq -e '
    def safe_setting($env; $name; $allowed):
      if ($env | has($name) | not) then true
      elif ($env[$name] | type) != "string" then false
      else any($allowed[]; . == $env[$name]) end;
    .services.teagramd.environment as $env
    | if ($env | type) != "object" then false else
        safe_setting($env; "TG_PUBLIC_WEBSOCKET_LISTEN_ADDR"; [""]) and
        safe_setting($env; "TG_PUBLIC_REGISTRATION"; ["closed"]) and
        safe_setting($env; "TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS"; [""]) and
        safe_setting($env; "TG_WELCOME_CHANNEL"; [""])
      end
  ' >/dev/null; then
    printf '%s\n' 'resolved Compose has unsafe public environment settings' >&2
    return 1
  fi
  return 0
}

verify_turnstile_compose_configuration() {
  local compose_json=$1 checkout_root secret_source file_source secret_dir runtime_uid
  checkout_root=$(readlink -f "$ROLLOUT_CHECKOUT_PATH") || return 1
  secret_source="$checkout_root/.secrets/teagramd-turnstile"
  if ! printf '%s\n' "$compose_json" | jq -e --arg secret_source "$secret_source" '
    (.services.teagramd.environment // {}) as $env
    | (.services.teagramd.secrets // []) as $service_secrets
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
    | ($compose.services.teagramd.environment // {}) as $compose_env
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

snapshot_from_json() {
  local inspect_json=$1
  local compose_json=$2
  local env_file=$3
  local override_file=$4
  local compose_source=${5:-selected-compose-render}
  local snapshot_name=${6:-}
  local mounts exposure resolved_config canonical_compose container_env container_id image_id state exit_code started_at finished_at grace stop_timeout mode_source
  local mounts_sha exposure_sha config_sha env_sha override_sha container_env_sha mount_count
  local compose_replica_count compose_client_addr_trust container_replica_count container_client_addr_trust
  local added_env_names changed_env_names container_env_added container_env_changed retained_added_env_names

  if [ ! -f "$env_file" ] || [ "$(stat -c %a "$env_file")" != 600 ]; then
    printf '%s\n' 'environment file must be a regular mode-0600 file' >&2
    return 1
  fi

  # Variables a deploy adds to .env are named by the runner; they are left out
  # of the environment digest and recorded by name so every other entry stays pinned.
  added_env_names=$(jq -cn --arg names "${ROLLOUT_ADDED_ENV:-}" '$names | split(",") | map(select(length > 0))')
  changed_env_names=$(jq -cn --arg names "${ROLLOUT_CHANGED_ENV:-}" '$names | split(",") | map(select(length > 0))')
  verify_safe_public_container_environment "$inspect_json" || return 1
  if [ "$compose_source" = selected-compose-render ]; then
    verify_safe_public_compose_environment "$compose_json" || return 1
    verify_turnstile_compose_configuration "$compose_json" || return 1
    verify_turnstile_container_configuration \
      "$inspect_json" "$compose_json" "$snapshot_name" "$added_env_names" "$changed_env_names" || return 1
    verify_turnstile_secret_mount "$inspect_json" "$compose_json" "$snapshot_name" || return 1
  fi

  mode_source=$(canonical_blob_mode_source) || return 1

  mounts=$(printf '%s' "$inspect_json" | jq -ce --arg mode_source "$mode_source" --arg compose_source "$compose_source" '
    def item: if type == "array" then .[0] else . end;
    (item.Mounts // [])
    | map({type:(.Type // ""), name:(.Name // ""), source:(.Source // ""),
           destination:(.Destination // ""), mode:(.Mode // ""),
           rw:(.RW // false), propagation:(.Propagation // "")})
    | map(select((.destination == "/run/secrets/teagramd-turnstile" and
                  $compose_source == "selected-compose-render") | not))
    | map(select((.type == "bind" and .source == $mode_source and
                  .destination == "/run/teagramd/blob-mode" and .rw == false) | not))
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
    verify_changed_container_environment "$inspect_json" "$compose_json" "$env_file" "$changed_env_names" || return 1
  elif [ "$compose_source" = selected-compose-render ] && [ "$snapshot_name" = rollback ]; then
    retained_added_env_names=$(printf '%s' "$inspect_json" | jq -c --argjson added "$added_env_names" '
      def item: if type == "array" then .[0] else . end;
      def runtime_name($name):
        if $name == "TEAGRAM_TURNSTILE_SITE_KEY" then "TG_TURNSTILE_SITE_KEY"
        elif $name == "TEAGRAM_TURNSTILE_SECRET_KEY_FILE" then "TG_TURNSTILE_SECRET_KEY_FILE"
        else $name end;
      [(item.Config.Env // [])[] | select(type == "string")
       | select(IN("TG_TURNSTILE_SITE_KEY=", "TG_TURNSTILE_SECRET_KEY_FILE=", "TG_TURNSTILE_SIGNIN_ENABLED=false") | not)
       | split("=")[0]] as $keys
      | [$added[] | select(. as $name | ($name | runtime_name($name)) as $runtime | any($keys[]; . == $runtime))]
      | unique
    ') || return 1
    verify_added_container_environment \
      "$inspect_json" "$compose_json" "$env_file" "$retained_added_env_names" || return 1
    verify_changed_container_environment "$inspect_json" "$compose_json" "$env_file" "$changed_env_names" || return 1
  fi
  container_env=$(printf '%s' "$inspect_json" | jq -ce --argjson added "$added_env_names" --argjson changed "$changed_env_names" '
    def item: if type == "array" then .[0] else . end;
    def runtime_name($name):
      if $name == "TEAGRAM_TURNSTILE_SITE_KEY" then "TG_TURNSTILE_SITE_KEY"
      elif $name == "TEAGRAM_TURNSTILE_SECRET_KEY_FILE" then "TG_TURNSTILE_SECRET_KEY_FILE"
      else $name end;
    (item.Config.Env // [])
    | if type != "array" then error("container environment must be an array") else
        map(select(type != "string" or (startswith("TG_REPLICA_COUNT=") | not)))
        | map(select(type != "string" or (startswith("TG_CLIENT_ADDR_TRUST=") | not)))
        | map(select(type != "string" or ((split("=")[0]) as $key | (["TG_PUBLIC_WEBSOCKET_LISTEN_ADDR", "TG_PUBLIC_REGISTRATION", "TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS", "TG_WELCOME_CHANNEL"] | index($key) == null))))
        | map(select(type != "string" or ((split("=")[0]) as $key | any($added[]; runtime_name(.) == $key) | not)))
        | map(select(type != "string" or ((split("=")[0]) as $key | any($changed[]; . == $key) | not)))
        | map(select(type != "string" or (
            (. == "TG_TURNSTILE_SITE_KEY=" or
             . == "TG_TURNSTILE_SECRET_KEY_FILE=" or
             . == "TG_TURNSTILE_SIGNIN_ENABLED=false") | not
          )))
        | sort
      end
  ')
  # The disabled Turnstile defaults count as unset in a container that predates
  # this deploy, so enabling Turnstile can add them; the target must carry them.
  container_env_added=$(printf '%s' "$inspect_json" | jq -er --argjson added "$added_env_names" --arg snapshot_name "$snapshot_name" '
    def item: if type == "array" then .[0] else . end;
    def runtime_name($name):
      if $name == "TEAGRAM_TURNSTILE_SITE_KEY" then "TG_TURNSTILE_SITE_KEY"
      elif $name == "TEAGRAM_TURNSTILE_SECRET_KEY_FILE" then "TG_TURNSTILE_SECRET_KEY_FILE"
      else $name end;
    [(item.Config.Env // [])[] | select(type == "string")
     | select($snapshot_name == "target" or
              (IN("TG_TURNSTILE_SITE_KEY=", "TG_TURNSTILE_SECRET_KEY_FILE=", "TG_TURNSTILE_SIGNIN_ENABLED=false") | not))
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
    resolved_config=$(printf '%s' "$compose_json" | jq -ce --arg mode_source "$mode_source" '
      .services.teagramd as $s
      | {
          stop_grace_period:($s.stop_grace_period // ""),
          ports:(($s.ports // [])
            | map({target:(.target // 0 | tostring), published:(.published // "" | tostring),
                   host_ip:(.host_ip // ""), protocol:(.protocol // "tcp"), mode:(.mode // "")})
            | sort_by(.host_ip, .published, .target, .protocol, .mode)),
          volumes:(($s.volumes // [])
            | map(select((.type == "bind" and .source == $mode_source and
                          .target == "/run/teagramd/blob-mode" and .read_only == true) | not))
            | map({type:(.type // ""), source:(.source // ""), target:(.target // ""),
                   read_only:(.read_only // false)})
            | sort_by(.type, .source, .target, .read_only)),
          network_mode:($s.network_mode // ""),
          networks:(($s.networks // {}) | if type == "object" then keys else . end | sort)
        }
    ')
    canonical_compose=$(printf '%s' "$compose_json" | jq -ceS --arg mode_source "$mode_source" '
      if (.services.teagramd.environment | type) != "object" then error("teagramd environment must be an object") else
        .services.teagramd.environment |= del(
          .TG_REPLICA_COUNT, .TG_CLIENT_ADDR_TRUST,
          .TG_PUBLIC_WEBSOCKET_LISTEN_ADDR, .TG_PUBLIC_REGISTRATION,
          .TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS, .TG_WELCOME_CHANNEL,
          .TG_TURNSTILE_SITE_KEY, .TG_TURNSTILE_SECRET_KEY_FILE,
          .TG_TURNSTILE_SIGNIN_ENABLED)
      end
      | if (.services.teagramd.secrets | type) == "array" then
          .services.teagramd.secrets |= map(select(.source != "teagramd_turnstile"))
        else . end
      | if (.services.teagramd.secrets | type) == "array" and (.services.teagramd.secrets | length) == 0 then
          del(.services.teagramd.secrets)
        else . end
      | if (.secrets | type) == "object" then .secrets |= del(.teagramd_turnstile) else . end
      | if (.secrets | type) == "object" and (.secrets | length) == 0 then del(.secrets) else . end
      | .services |= with_entries(
          if (.key | startswith("teagramd")) and (.value.volumes | type) == "array" then
            .value.volumes |= map(select((.type == "bind" and .source == $mode_source and
                                          .target == "/run/teagramd/blob-mode" and .read_only == true) | not))
          else . end)
    ')
    compose_replica_count=$(printf '%s' "$compose_json" | jq -er '
      .services.teagramd.environment as $env
      | if ($env | type) != "object" then error("teagramd environment must be an object")
        elif ($env | has("TG_REPLICA_COUNT")) then
          if ($env.TG_REPLICA_COUNT | type) == "string" and ($env.TG_REPLICA_COUNT | test("^[0-9]{1,3}$")) then $env.TG_REPLICA_COUNT else "invalid" end
        else "unset" end
    ')
    compose_client_addr_trust=$(printf '%s' "$compose_json" | jq -er '
      .services.teagramd.environment as $env
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
  exposure_sha=$(sha256_text "$exposure")
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
    --arg mount_count "$mount_count" \
    --arg mounts_sha "$mounts_sha" \
    --arg exposure_sha "$exposure_sha" \
    --arg config_sha "$config_sha" \
    --arg env_sha "$env_sha" \
    --arg override_sha "$override_sha" \
    --arg container_env_sha "$container_env_sha" \
    --arg container_env_added "$container_env_added" \
    --arg container_env_changed "$container_env_changed" \
    --arg compose_source "$compose_source" \
    --arg compose_selection "${COMPOSE_FILE:-unset}" \
    '{container_id:$container_id,image_id:$image_id,state:$state,exit_code:$exit_code,
      started_at:$started_at,finished_at:$finished_at,stop_grace_period:$grace,
      container_stop_timeout:$stop_timeout,
      compose_replica_count:$compose_replica_count,
      compose_client_addr_trust:$compose_client_addr_trust,
      container_replica_count:$container_replica_count,
      container_client_addr_trust:$container_client_addr_trust,
      mount_count:($mount_count|tonumber),mounts_sha256:$mounts_sha,
      exposure_sha256:$exposure_sha,config_sha256:$config_sha,
      env_sha256:$env_sha,override_sha256:$override_sha,
      container_env_sha256:$container_env_sha,
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
  local field before_value after_value before_image after_image result
  local fields=(mounts_sha256 stop_grace_period container_stop_timeout config_sha256 env_sha256 override_sha256 exposure_sha256 container_env_sha256)
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
      (.container_replica_count | type == "string" and test("^(unset|invalid|duplicate|[0-9]{1,3})$")) and
      (.container_client_addr_trust | type == "string" and IN("unset","invalid","duplicate","socket")) and
      (.mount_count | type == "number" and . >= 0) and
      (.mounts_sha256 | sha) and (.exposure_sha256 | sha) and
      (.config_sha256 | sha) and (.env_sha256 | sha) and (.override_sha256 | sha) and
      (.container_env_sha256 | sha)
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
      (.container_replica_count | type == "string" and test("^(unset|invalid|duplicate|[0-9]{1,3})$")) and
      (.container_client_addr_trust | type == "string" and IN("unset","invalid","duplicate","socket")) and
      (.mount_count | type == "number" and . >= 0) and
      (.mounts_sha256 | sha) and (.exposure_sha256 | sha) and
      (.config_sha256 | sha) and (.env_sha256 | sha) and (.override_sha256 | sha) and
      (.container_env_sha256 | sha)
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
  for field in "${fields[@]}"; do
    if ! before_value=$(jq -er --arg field "$field" '.[$field]' "$before_file"); then return 1; fi
    if ! after_value=$(jq -er --arg field "$field" '.[$field]' "$after_file"); then return 1; fi
    if [ "$before_value" = "$after_value" ]; then result=pass; else result=fail; fi
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
  local inspect_json compose_json snapshot output
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
  snapshot=$(snapshot_from_json "$inspect_json" "$compose_json" "$env_file" "$override_file" "$compose_source" "$name") || { printf '%s\n' 'allowlisted snapshot generation failed' >&2; return 1; }
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

main() {
  [ "$#" -ge 1 ] || { printf '%s\n' 'commands: snapshot, compare, ready, ready-pair' >&2; return 64; }
  local command=$1
  shift
  case "$command" in
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
      printf '%s\n' 'commands: snapshot, compare, ready, ready-pair' >&2
      return 64 ;;
  esac
}

if [ "$ROLLOUT_VERIFIER_SOURCE_ONLY" != 1 ]; then
  main "$@"
fi
