#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROLLOUT_VERIFIER_SOURCE_ONLY=1
source "$SCRIPT_DIR/rollout-verifier.sh"

TMP=$(mktemp -d)
trap 'rm -rf -- "$TMP"' EXIT
chmod 700 "$TMP"
ROLLOUT_CHECKOUT_PATH="$TMP/checkout"
mkdir -m 700 "$ROLLOUT_CHECKOUT_PATH"
ENV_FILE="$TMP/.env"
OVERRIDE_FILE="$TMP/docker-compose.override.yml"
printf 'TG_NEW_FLAG=on\n' > "$ENV_FILE"
: > "$OVERRIDE_FILE"
chmod 600 "$ENV_FILE" "$OVERRIDE_FILE"
ROTATION_MEDIA_SOURCE="$TMP/rotation-media-keys"
mkdir -m 700 "$ROTATION_MEDIA_SOURCE"
printf '%064d\n' 0 > "$ROTATION_MEDIA_SOURCE/1.key"
chmod 600 "$ROTATION_MEDIA_SOURCE/1.key"
if [ "$(id -u)" = 0 ]; then
  chown 65532:65532 "$ROTATION_MEDIA_SOURCE" "$ROTATION_MEDIA_SOURCE/1.key"
fi

REAL_JQ=$(command -v jq)
JQ_SECRET_ARG_MARKER="$TMP/jq-secret-arg"
export REAL_JQ JQ_SECRET_ARG_MARKER
mkdir -m 700 "$TMP/bin"
cat > "$TMP/bin/jq" <<'SH'
#!/usr/bin/env bash
set -Eeuo pipefail
real_jq=$REAL_JQ
for arg in "$@"; do
  if [[ "$arg" == *'fixture-secret-postgres-password'* || "$arg" == *'postgres://fixture:dsn-secret@db/teagram'* ]]; then
    : > "$JQ_SECRET_ARG_MARKER"
  fi
done
unset REAL_JQ JQ_SECRET_ARG_MARKER
exec "$real_jq" "$@"
SH
chmod 700 "$TMP/bin/jq"
PATH="$TMP/bin:$PATH"
export PATH

COMPOSE_JSON='{"services":{"teagramd":{"stop_grace_period":"2m0s","ports":[],"volumes":[],"network_mode":"","networks":{},"environment":{"TG_REPLICA_COUNT":"1","TG_CLIENT_ADDR_TRUST":"socket","TG_LOG_LOGIN_CODES":"false","POSTGRES_PASSWORD":"fixture-secret-postgres-password","DATABASE_URL":"postgres://fixture:dsn-secret@db/teagram"}}}}'

inspect_json() {
  local extra=$1 log_config=${INSPECT_LOG_CONFIG:-'{"Type":"json-file","Config":{}}'}
  jq -cn --arg extra "$extra" --argjson log_config "$log_config" '
    ["TG_REPLICA_COUNT=1","TG_CLIENT_ADDR_TRUST=socket","TG_LOG_LOGIN_CODES=false"] as $base
    | {
        Id:"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
        Image:"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
        Config:{Env:($base + (if $extra == "" then [] else [$extra] end)),StopTimeout:120},
        State:{Status:"running",ExitCode:0,StartedAt:"2026-10-16T12:00:00Z",FinishedAt:"0001-01-01T00:00:00Z"},
        HostConfig:{LogConfig:$log_config,PortBindings:{}},Mounts:[]
      }
  '
}

rotation_inspect_json() {
  jq -c --arg media_source "$ROTATION_MEDIA_SOURCE" '
    .Config.Env += [
      "TG_MEDIA_KEY_DIR=/run/teagramd/media-keys",
      "TG_MEDIA_KEY_CURRENT=1"
    ]
    | .Mounts += [{
        Type:"bind",Source:$media_source,Destination:"/run/teagramd/media-keys",
        Mode:"ro",RW:false,Propagation:""
      }]
  ' <<< "$(inspect_json '')"
}

target_snapshot() {
  local actual_env=$1 env_assignment=${2:-TG_NEW_FLAG=on} resolved_env_value=${3:-on} inspected compose_json
  printf '%s\n' "$env_assignment" > "$ENV_FILE"
  inspected=$(inspect_json "$actual_env")
  compose_json=$(jq -c --arg value "$resolved_env_value" \
    '.services.teagramd.environment.TG_NEW_FLAG=$value' <<< "$COMPOSE_JSON")
  ROLLOUT_ADDED_ENV=TG_NEW_FLAG snapshot_from_json \
    "$inspected" "$compose_json" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target
}

rollback_snapshot() {
  local actual_env=$1 resolved_env_value=${2:-on} inspected compose_json
  inspected=$(inspect_json "$actual_env")
  compose_json=$(jq -c --arg value "$resolved_env_value" \
    '.services.teagramd.environment.TG_NEW_FLAG=$value' <<< "$COMPOSE_JSON")
  ROLLOUT_ADDED_ENV=TG_NEW_FLAG snapshot_from_json \
    "$inspected" "$compose_json" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render rollback
}

registration_snapshot() {
  local actual=$1 env_value=$2 compose_value=$3 name=$4 inspected compose_json
  printf 'TG_REGISTRATION=%s\n' "$env_value" > "$ENV_FILE"
  inspected=$(inspect_json "$actual")
  compose_json=$(jq -c --arg value "$compose_value" \
    '.services.teagramd.environment.TG_REGISTRATION=$value' <<< "$COMPOSE_JSON")
  ROLLOUT_CHANGED_ENV=TG_REGISTRATION snapshot_from_json \
    "$inspected" "$compose_json" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render "$name"
}

public_setting_snapshot() {
  local setting=$1 value=$2 actual=$3 name=$4 added=${5:-} changed=${6:-} inspected compose_json
  printf '%s=%s\n' "$setting" "$value" > "$ENV_FILE"
  inspected=$(inspect_json "$actual")
  compose_json=$(jq -c --arg setting "$setting" --arg value "$value" \
    '.services.teagramd.environment[$setting]=$value' <<< "$COMPOSE_JSON")
  ROLLOUT_ADDED_ENV="$added" ROLLOUT_CHANGED_ENV="$changed" snapshot_from_json \
    "$inspected" "$compose_json" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render "$name"
}

exchange_budget_snapshot() {
  local name=$1 value=$2 added=${3:-} env_content=${4:-} actual_value actual inspected compose_json
  actual_value=${5:-$value}
  if [ -n "$env_content" ]; then
    printf '%s\n' "$env_content" > "$ENV_FILE"
  else
    : > "$ENV_FILE"
  fi
  actual=
  if [ -n "$actual_value" ]; then
    actual="TG_RATE_LIMIT_KEY_EXCHANGE_IP=$actual_value"
  fi
  inspected=$(inspect_json "$actual")
  inspected=$(jq -c --arg value "$value" '
    .Config.Env += [
      "TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET=",
      "TG_RATE_LIMIT_KEY_EXCHANGE_IP_WINDOW=",
      "TG_KEY_EXCHANGE_PER_CONNECTION=",
      "TG_KEY_EXCHANGE_GLOBAL_RATE=",
      "TG_KEY_EXCHANGE_GLOBAL_BURST="
    ] + (if $value == "" then ["TG_RATE_LIMIT_KEY_EXCHANGE_IP="] else [] end)
  ' <<< "$inspected")
  compose_json=$(jq -c --arg value "$value" '
    .services.teagramd.environment += {
      TG_RATE_LIMIT_KEY_EXCHANGE_IP:$value,
      TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET:"",
      TG_RATE_LIMIT_KEY_EXCHANGE_IP_WINDOW:"",
      TG_KEY_EXCHANGE_PER_CONNECTION:"",
      TG_KEY_EXCHANGE_GLOBAL_RATE:"",
      TG_KEY_EXCHANGE_GLOBAL_BURST:""
    }
  ' <<< "$COMPOSE_JSON")
  ROLLOUT_ADDED_ENV="$added" snapshot_from_json \
    "$inspected" "$compose_json" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render "$name"
}

tailnet_limit_snapshot() {
  local name=$1 actual_value=$2 env_content=$3 env_mode=$4 compose_value=$5 inspected compose_json added= changed= actual
  printf '%s\n' "$env_content" > "$ENV_FILE"
  actual=
  if [ -n "$actual_value" ]; then
    actual="TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET=$actual_value"
  fi
  inspected=$(inspect_json "$actual")
  compose_json=$(jq -c --arg value "$compose_value" \
    '.services.teagramd.environment.TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET=$value' <<< "$COMPOSE_JSON")
  case "$env_mode" in
    added) added=TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET ;;
    changed) changed=TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET ;;
    retained) ;;
    *) printf 'invalid tailnet fixture mode: %s\n' "$env_mode" >&2; return 1 ;;
  esac
  ROLLOUT_ADDED_ENV="$added" ROLLOUT_CHANGED_ENV="$changed" snapshot_from_json \
    "$inspected" "$compose_json" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render "$name"
}

invite_quota_snapshot() {
  local name=$1 actual_value=$2 env_content=$3 env_mode=$4 compose_value=$5 inspected compose_json added=
  printf '%s\n' "$env_content" > "$ENV_FILE"
  inspected=$(inspect_json "TG_USER_INVITE_QUOTA=$actual_value")
  compose_json=$(jq -c --arg value "$compose_value" \
    '.services.teagramd.environment.TG_USER_INVITE_QUOTA=$value' <<< "$COMPOSE_JSON")
  case "$env_mode" in
    added) added=TG_USER_INVITE_QUOTA ;;
    retained) ;;
    *) printf 'invalid invite quota fixture mode: %s\n' "$env_mode" >&2; return 1 ;;
  esac
  ROLLOUT_ADDED_ENV="$added" snapshot_from_json \
    "$inspected" "$compose_json" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render "$name"
}

: > "$ENV_FILE"
default_compose_json=$COMPOSE_JSON
COMPOSE_JSON=$(jq -c '.services.teagramd.logging={driver:"syslog",options:{"syslog-address":"unixgram:///dev/log","syslog-facility":"daemon","syslog-format":"rfc5424",tag:"teagramd"}}' <<< "$COMPOSE_JSON")
INSPECT_LOG_CONFIG='{"Type":"syslog","Config":{"syslog-address":"unixgram:///dev/log","syslog-facility":"daemon","syslog-format":"rfc5424","tag":"teagramd"}}'
empty_env=$(snapshot_from_json \
  "$(inspect_json '')" "$COMPOSE_JSON" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render baseline)
rotation_base_compose=$(jq -c --arg media_source "$ROTATION_MEDIA_SOURCE" '
  .services.teagramd.image="teagramd:local"
  | .services.teagramd.environment += {
      TG_POSTGRES_DSN:"fixture-dsn",
      TG_AUTHKEY_ENC_KEY:"",
      TG_AUTHKEY_ENC_KEY_FILE:"/var/lib/teagramd/enc_key.hex",
      TG_BLOB_DIR:"/var/lib/teagramd-blobs",
      TG_BLOB_S3_ENDPOINT:"http://rustfs:9000",
      TG_BLOB_S3_BUCKET:"teagram",
      TG_BLOB_S3_PREFIX:"teagramd/",
      TG_BLOB_S3_REGION:"us-east-1",
      TG_BLOB_S3_ACCESS_KEY_ID:"",
      TG_BLOB_S3_SECRET_ACCESS_KEY_FILE:"/run/secrets/teagramd-blob-secret-key",
      TG_BLOB_S3_ALLOW_INSECURE_HTTP:"true",
      TG_MEDIA_KEY_DIR:"/run/teagramd/media-keys",
      TG_MEDIA_KEY_CURRENT:"1"
    }
  | .services.teagramd.volumes += [{
      type:"bind",source:$media_source,target:"/run/teagramd/media-keys",
      read_only:true,bind:{create_host_path:false}
    }]
' <<< "$COMPOSE_JSON")
rotation_migration_compose=$(jq -c --arg mode_source "$(canonical_blob_mode_source)" --arg media_source "$ROTATION_MEDIA_SOURCE" '
  .services["blob-key-rotate"] = {
    image:"teagramd:local",
    entrypoint:["/usr/local/bin/blob-key-rotate"],
    command:null,
    user:"0:0",
    read_only:true,
    profiles:["migration"],
    environment:(.services.teagramd.environment | {
      TG_POSTGRES_DSN,
      TG_AUTHKEY_ENC_KEY,
      TG_AUTHKEY_ENC_KEY_FILE,
      TG_BLOB_DIR,
      TG_BLOB_S3_ENDPOINT,
      TG_BLOB_S3_BUCKET,
      TG_BLOB_S3_PREFIX,
      TG_BLOB_S3_REGION,
      TG_BLOB_S3_ACCESS_KEY_ID,
      TG_BLOB_S3_SECRET_ACCESS_KEY_FILE,
      TG_BLOB_S3_ALLOW_INSECURE_HTTP,
      TG_MEDIA_KEY_DIR,
      TG_MEDIA_KEY_CURRENT
    }),
    depends_on:{
      postgres:{condition:"service_healthy",required:true},
      "rustfs-init":{condition:"service_completed_successfully",required:true}
    },
    volumes:[
      {type:"volume",source:"tgkey",target:"/var/lib/teagramd",read_only:true,volume:{}},
      {type:"bind",source:$mode_source,target:"/run/teagramd/blob-mode",read_only:true,bind:{create_host_path:false}},
      {type:"volume",source:"blob_key_rotate_reports",target:"/run/teagramd/reports",volume:{}},
      {type:"bind",source:$media_source,target:"/run/teagramd/media-keys",read_only:true,bind:{create_host_path:false}}
    ],
    networks:{default:null,rustfs_private:null},
    secrets:[{source:"teagramd_blob_secret_key",target:"teagramd-blob-secret-key"}]
  }
  | .volumes.blob_key_rotate_reports={name:"teagram_blob_key_rotate_reports",driver:"local"}
' <<< "$rotation_base_compose")
rotation_inspect=$(rotation_inspect_json)
rotation_base_snapshot=$(snapshot_from_json \
  "$rotation_inspect" "$rotation_base_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render baseline)
rotation_migration_snapshot=$(snapshot_from_json \
  "$rotation_inspect" "$rotation_migration_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render preflight)
printf '%s\n' "$rotation_base_snapshot" > "$TMP/migration-baseline.snapshot.json"
printf '%s\n' "$rotation_migration_snapshot" > "$TMP/migration-target.snapshot.json"
if ! compare_snapshots \
  "$TMP/migration-baseline.snapshot.json" "$TMP/migration-target.snapshot.json" \
  sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
  "$TMP/migration-comparisons.tsv"; then
  printf '%s\n' 'FAIL inactive key-rotation migration profile changed the serving Compose comparison' >&2
  cat "$TMP/migration-comparisons.tsv" >&2
  exit 1
fi
printf '%s\n' 'PASS inactive key-rotation migration profile is excluded from serving Compose drift'
unsafe_rotation_migration_compose=$(jq -c '
  .services["blob-key-rotate"].volumes[2] = {
    type:"bind",source:"/root",target:"/run/teagramd/reports",bind:{create_host_path:false}
  }
' <<< "$rotation_migration_compose")
if snapshot_from_json \
  "$rotation_inspect" "$unsafe_rotation_migration_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render preflight \
  >/dev/null 2>"$TMP/unsafe-rotation-profile.err"; then
  printf '%s\n' 'FAIL key-rotation migration profile with a host bind was accepted' >&2
  exit 1
fi
printf '%s\n' 'PASS unsafe key-rotation migration mounts are rejected'
empty_invite_quota_mapping=$(invite_quota_snapshot baseline '' '' retained '')
[ "$(jq -r '.container_env_sha256' <<< "$empty_env")" = \
  "$(jq -r '.container_env_sha256' <<< "$empty_invite_quota_mapping")" ]
[ "$(jq -r '.config_sha256' <<< "$empty_env")" = \
  "$(jq -r '.config_sha256' <<< "$empty_invite_quota_mapping")" ]
empty_invite_quota_added_baseline=$(invite_quota_snapshot baseline '' TG_USER_INVITE_QUOTA= added '')
if [ "$(jq -r '.container_env_added' <<< "$empty_invite_quota_added_baseline")" != none ]; then
  printf '%s\n' 'FAIL exact-empty invite quota mapping was treated as an existing named environment addition' >&2
  exit 1
fi
printf '%s\n' 'PASS empty invite quota mapping remains equivalent to an absent value'
empty_exchange_env=$(exchange_budget_snapshot target '' '')
[ "$(jq -r '.container_env_sha256' <<< "$empty_env")" = \
  "$(jq -r '.container_env_sha256' <<< "$empty_exchange_env")" ]
[ "$(jq -r '.container_env_added' <<< "$empty_exchange_env")" = none ]
empty_exchange_added_baseline=$(exchange_budget_snapshot baseline '' TG_RATE_LIMIT_KEY_EXCHANGE_IP '')
if [ "$(jq -r '.container_env_added' <<< "$empty_exchange_added_baseline")" != none ]; then
  printf '%s\n' 'FAIL exact-empty exchange mapping was treated as an existing named environment addition' >&2
  exit 1
fi
empty_tailnet_added_baseline=$(exchange_budget_snapshot baseline '' TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET '')
if [ "$(jq -r '.container_env_added' <<< "$empty_tailnet_added_baseline")" != none ]; then
  printf '%s\n' 'FAIL exact-empty tailnet mapping was treated as an existing named environment addition' >&2
  exit 1
fi
printf '%s\n' "$empty_env" > "$TMP/key-exchange-baseline.snapshot.json"
printf '%s\n' "$empty_exchange_env" > "$TMP/key-exchange-empty-target.snapshot.json"
if ! compare_snapshots \
  "$TMP/key-exchange-baseline.snapshot.json" "$TMP/key-exchange-empty-target.snapshot.json" \
  sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
  "$TMP/key-exchange-empty-comparisons.tsv"; then
  printf '%s\n' 'FAIL full baseline-to-target comparison rejected empty key-exchange mappings' >&2
  cat "$TMP/key-exchange-empty-comparisons.tsv" >&2
  exit 1
fi
image_env_inspect=$(jq -c \
  --arg image_id sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc \
  --arg container_id cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc '
  .Image=$image_id
  | .Id=$container_id
  | .Config.Env += ["TG_STICKER_CATALOG_BUNDLE=/usr/share/teagram/sticker-bundle"]
' <<< "$(inspect_json '')")
image_env_json='["TG_STICKER_CATALOG_BUNDLE=/usr/share/teagram/sticker-bundle"]'
image_env_snapshot=$(snapshot_from_json \
  "$image_env_inspect" "$COMPOSE_JSON" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target '' "$image_env_json")
if [ "$(jq -r '.container_env_sha256' <<< "$empty_env")" != \
     "$(jq -r '.container_env_sha256' <<< "$image_env_snapshot")" ]; then
  printf '%s\n' 'FAIL image-provided environment changed the operator environment digest' >&2
  exit 1
fi
live_image_env_snapshot=$(snapshot_from_json \
  "$image_env_inspect" "$COMPOSE_JSON" "$ENV_FILE" "$OVERRIDE_FILE" live-container-inspection baseline '' "$image_env_json")
if [ "$(jq -r '.container_env_sha256' <<< "$empty_env")" != \
     "$(jq -r '.container_env_sha256' <<< "$live_image_env_snapshot")" ]; then
  printf '%s\n' 'FAIL image-provided environment changed the live-inspection operator digest' >&2
  exit 1
fi
printf '%s\n' "$image_env_snapshot" > "$TMP/image-env-target.snapshot.json"
if ! compare_snapshots \
  "$TMP/key-exchange-baseline.snapshot.json" "$TMP/image-env-target.snapshot.json" \
  sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc \
  "$TMP/image-env-comparisons.tsv"; then
  printf '%s\n' 'FAIL full baseline-to-target comparison rejected image-provided environment' >&2
  cat "$TMP/image-env-comparisons.tsv" >&2
  exit 1
fi
printf '%s\n' 'PASS image-provided environment does not change operator environment equivalence'

blob_dir_compose=$(jq -c \
  '.services.teagramd.environment.TG_BLOB_DIR="/var/lib/telegramd-blobs"' <<< "$COMPOSE_JSON")
blob_dir_inspect=$(jq -c \
  '.Config.Env += ["TG_BLOB_DIR=/var/lib/telegramd-blobs"]' <<< "$(inspect_json '')")
blob_dir_baseline=$(snapshot_from_json \
  "$blob_dir_inspect" "$blob_dir_compose" "$ENV_FILE" "$OVERRIDE_FILE" \
  selected-compose-render baseline '' '["TG_BLOB_DIR=/var/lib/telegramd-blobs"]')
blob_dir_target=$(snapshot_from_json \
  "$blob_dir_inspect" "$blob_dir_compose" "$ENV_FILE" "$OVERRIDE_FILE" \
  selected-compose-render target '' '["TG_BLOB_DIR=/image-default-blobs"]')
if [ "$(jq -r '.container_env_sha256' <<< "$blob_dir_baseline")" != \
     "$(jq -r '.container_env_sha256' <<< "$blob_dir_target")" ]; then
  printf '%s\n' 'FAIL explicit Compose environment matching the baseline image default changed the target environment digest' >&2
  exit 1
fi
if [ "$(jq -r '.container_env_sha256' <<< "$empty_env")" = \
     "$(jq -r '.container_env_sha256' <<< "$blob_dir_baseline")" ]; then
  printf '%s\n' 'FAIL explicit Compose environment matching the image default was omitted from the environment digest' >&2
  exit 1
fi
printf '%s\n' 'PASS explicit Compose values matching a baseline image default remain pinned across image default changes'

unexpected_runtime_inspect=$(jq -c '.Config.Env += ["TG_UNDECLARED_RUNTIME_FLAG=on"]' <<< "$image_env_inspect")
unexpected_runtime_snapshot=$(snapshot_from_json \
  "$unexpected_runtime_inspect" "$COMPOSE_JSON" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target '' "$image_env_json")
if [ "$(jq -r '.container_env_sha256' <<< "$empty_env")" = \
     "$(jq -r '.container_env_sha256' <<< "$unexpected_runtime_snapshot")" ]; then
  printf '%s\n' 'FAIL unmodeled runtime environment addition was ignored with image defaults' >&2
  exit 1
fi
printf '%s\n' 'PASS unmodeled runtime environment additions remain pinned'

operator_env_baseline_compose=$(jq -c \
  '.services.teagramd.environment.TG_OPERATOR_FLAG="baseline"' <<< "$COMPOSE_JSON")
operator_env_target_compose=$(jq -c \
  '.services.teagramd.environment.TG_OPERATOR_FLAG="target"' <<< "$COMPOSE_JSON")
operator_env_baseline=$(snapshot_from_json \
  "$(inspect_json 'TG_OPERATOR_FLAG=baseline')" "$operator_env_baseline_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render baseline)
operator_env_target=$(snapshot_from_json \
  "$(inspect_json 'TG_OPERATOR_FLAG=target')" "$operator_env_target_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target)
if [ "$(jq -r '.container_env_sha256' <<< "$operator_env_baseline")" = \
     "$(jq -r '.container_env_sha256' <<< "$operator_env_target")" ]; then
  printf '%s\n' 'FAIL Compose-provided environment did not change the operator environment digest' >&2
  exit 1
fi
printf '%s\n' 'PASS Compose-provided environment remains in operator environment equivalence'
COMPOSE_JSON=$default_compose_json
unset INSPECT_LOG_CONFIG
printf '%s\n' 'PASS full baseline-to-target comparison accepts empty key-exchange mappings'
printf '%s\n' 'PASS empty key-exchange settings retain the binary-default environment baseline'

added_exchange_env=$(exchange_budget_snapshot target 0 TG_RATE_LIMIT_KEY_EXCHANGE_IP \
  'TG_RATE_LIMIT_KEY_EXCHANGE_IP=0')
[ "$(jq -r '.container_env_sha256' <<< "$empty_env")" = \
  "$(jq -r '.container_env_sha256' <<< "$added_exchange_env")" ]
[ "$(jq -r '.container_env_added' <<< "$added_exchange_env")" = TG_RATE_LIMIT_KEY_EXCHANGE_IP ]
[ "$(jq -r '.config_sha256' <<< "$empty_env")" != \
  "$(jq -r '.config_sha256' <<< "$added_exchange_env")" ]
if exchange_budget_snapshot target 0 TG_RATE_LIMIT_KEY_EXCHANGE_IP \
  'TG_RATE_LIMIT_KEY_EXCHANGE_IP=0' 1 >/dev/null 2>"$TMP/key-exchange-mismatch.err"; then
  printf '%s\n' 'FAIL target with a mismatched configured key-exchange value was accepted' >&2
  exit 1
fi
printf '%s\n' 'PASS configured key-exchange values remain pinned and must match the target container'
printf '%s\n' 'PASS named key-exchange override is verified without changing the remaining environment baseline'

COMPOSE_JSON=$(jq -c '.services.teagramd.logging={driver:"syslog",options:{"syslog-address":"unixgram:///dev/log","syslog-facility":"daemon","syslog-format":"rfc5424",tag:"teagramd"}}' <<< "$COMPOSE_JSON")
INSPECT_LOG_CONFIG='{"Type":"syslog","Config":{"syslog-address":"unixgram:///dev/log","syslog-facility":"daemon","syslog-format":"rfc5424","tag":"teagramd"}}'
tailnet_added_baseline=$(tailnet_limit_snapshot baseline '' 'TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET=0' added 0)
tailnet_added_target=$(tailnet_limit_snapshot target 0 'TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET=0' added 0)
[ "$(jq -r '.container_env_added' <<< "$tailnet_added_baseline")" = none ]
[ "$(jq -r '.container_env_added' <<< "$tailnet_added_target")" = TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET ]
[ "$(jq -r '.container_env_sha256' <<< "$tailnet_added_baseline")" = \
  "$(jq -r '.container_env_sha256' <<< "$tailnet_added_target")" ]
[ "$(jq -r '.config_sha256' <<< "$tailnet_added_baseline")" = \
  "$(jq -r '.config_sha256' <<< "$tailnet_added_target")" ]
printf '%s\n' "$tailnet_added_baseline" > "$TMP/tailnet-add-baseline.snapshot.json"
printf '%s\n' "$tailnet_added_target" > "$TMP/tailnet-add-target.snapshot.json"
if ! compare_snapshots \
  "$TMP/tailnet-add-baseline.snapshot.json" "$TMP/tailnet-add-target.snapshot.json" \
  sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
  "$TMP/tailnet-add-comparisons.tsv"; then
  printf '%s\n' 'FAIL named tailnet addition changed the baseline-to-target comparison' >&2
  cat "$TMP/tailnet-add-comparisons.tsv" >&2
  exit 1
fi
printf '%s\n' 'PASS named tailnet addition is value-checked and passes baseline-to-target comparison'

invite_added_baseline=$(invite_quota_snapshot baseline '' TG_USER_INVITE_QUOTA= added '')
invite_added_target=$(invite_quota_snapshot target 5 TG_USER_INVITE_QUOTA=5 added 5)
[ "$(jq -r '.container_env_added' <<< "$invite_added_baseline")" = none ]
[ "$(jq -r '.container_env_added' <<< "$invite_added_target")" = TG_USER_INVITE_QUOTA ]
[ "$(jq -r '.container_env_sha256' <<< "$invite_added_baseline")" = \
  "$(jq -r '.container_env_sha256' <<< "$invite_added_target")" ]
[ "$(jq -r '.config_sha256' <<< "$invite_added_baseline")" = \
  "$(jq -r '.config_sha256' <<< "$invite_added_target")" ]
if invite_quota_snapshot target 5 TG_USER_INVITE_QUOTA=5 added 4 \
  >/dev/null 2>"$TMP/invite-quota-dotenv-compose-mismatch.err"; then
  printf '%s\n' 'FAIL mismatched invite quota .env and resolved Compose values were accepted' >&2
  exit 1
fi
if invite_quota_snapshot target -1 TG_USER_INVITE_QUOTA=-1 added -1 \
  >/dev/null 2>"$TMP/invite-quota-negative.err"; then
  printf '%s\n' 'FAIL negative invite quota was accepted' >&2
  exit 1
fi
invite_retained=$(invite_quota_snapshot baseline 5 TG_USER_INVITE_QUOTA=5 retained 5)
[ "$(jq -r '.container_env_sha256' <<< "$invite_retained")" != \
  "$(jq -r '.container_env_sha256' <<< "$empty_env")" ]
printf '%s\n' 'PASS named invite quota is value-checked, comparable, and pinned after addition'

: > "$ENV_FILE"
empty_invite_quota_rollback_inspect=$(jq -c '.Config.Env += ["TG_USER_INVITE_QUOTA="]' \
  <<< "$(inspect_json '')")
empty_invite_quota_rollback_compose=$(jq -c \
  '.services.teagramd.environment.TG_USER_INVITE_QUOTA=""' \
  <<< "$COMPOSE_JSON")
empty_invite_quota_rollback=$(ROLLOUT_ADDED_ENV=TG_USER_INVITE_QUOTA \
  snapshot_from_json "$empty_invite_quota_rollback_inspect" "$empty_invite_quota_rollback_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render rollback)
[ "$(jq -r '.container_env_added' <<< "$empty_invite_quota_rollback")" = none ]
printf '%s\n' 'PASS empty retained invite quota mapping does not block rollback of a named addition'

if tailnet_limit_snapshot target 20 'TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET=0' added 20 \
  >/dev/null 2>"$TMP/tailnet-dotenv-compose-mismatch.err"; then
  printf '%s\n' 'FAIL mismatched tailnet .env and resolved Compose values were accepted' >&2
  exit 1
fi
printf '%s\n' 'PASS tailnet .env addition must match the resolved Compose value'

: > "$ENV_FILE"
empty_tailnet_rollback_inspect=$(jq -c '.Config.Env += ["TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET="]' \
  <<< "$(inspect_json '')")
empty_tailnet_rollback_compose=$(jq -c \
  '.services.teagramd.environment.TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET=""' \
  <<< "$COMPOSE_JSON")
empty_tailnet_rollback=$(ROLLOUT_ADDED_ENV=TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET \
  snapshot_from_json "$empty_tailnet_rollback_inspect" "$empty_tailnet_rollback_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render rollback)
[ "$(jq -r '.container_env_added' <<< "$empty_tailnet_rollback")" = none ]
printf '%s\n' 'PASS empty retained tailnet mapping does not block rollback of a named addition'

if tailnet_limit_snapshot target 5 'TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET=0' added 0 \
  >/dev/null 2>"$TMP/tailnet-mismatch.err"; then
  printf '%s\n' 'FAIL mismatched tailnet addition was accepted' >&2
  exit 1
fi
tailnet_retained_zero=$(tailnet_limit_snapshot baseline 0 'TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET=0' retained 0)
tailnet_retained_twelve=$(tailnet_limit_snapshot baseline 12 'TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET=12' retained 12)
[ "$(jq -r '.config_sha256' <<< "$tailnet_retained_zero")" != \
  "$(jq -r '.config_sha256' <<< "$tailnet_retained_twelve")" ]
[ "$(jq -r '.container_env_sha256' <<< "$tailnet_retained_zero")" != \
  "$(jq -r '.container_env_sha256' <<< "$tailnet_retained_twelve")" ]
printf '%s\n' 'PASS retained tailnet values remain pinned in Compose and container digests'

tailnet_changed_baseline=$(tailnet_limit_snapshot baseline 12 'TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET=0' changed 0)
tailnet_changed_target=$(tailnet_limit_snapshot target 0 'TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET=0' changed 0)
[ "$(jq -r '.container_env_changed' <<< "$tailnet_changed_target")" = TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET ]
[ "$(jq -r '.container_env_sha256' <<< "$tailnet_changed_baseline")" = \
  "$(jq -r '.container_env_sha256' <<< "$tailnet_changed_target")" ]
[ "$(jq -r '.config_sha256' <<< "$tailnet_changed_baseline")" = \
  "$(jq -r '.config_sha256' <<< "$tailnet_changed_target")" ]
printf '%s\n' "$tailnet_changed_baseline" > "$TMP/tailnet-change-baseline.snapshot.json"
printf '%s\n' "$tailnet_changed_target" > "$TMP/tailnet-change-target.snapshot.json"
if ! compare_snapshots \
  "$TMP/tailnet-change-baseline.snapshot.json" "$TMP/tailnet-change-target.snapshot.json" \
  sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
  "$TMP/tailnet-change-comparisons.tsv"; then
  printf '%s\n' 'FAIL named tailnet value change changed the baseline-to-target comparison' >&2
  cat "$TMP/tailnet-change-comparisons.tsv" >&2
  exit 1
fi
printf '%s\n' 'PASS named tailnet value changes stay separately pinned from general config drift'

if tailnet_limit_snapshot target -1 'TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET=-1' added -1 \
  >/dev/null 2>"$TMP/tailnet-negative.err"; then
  printf '%s\n' 'FAIL negative tailnet limit was accepted' >&2
  exit 1
fi
printf '%s\n' 'PASS negative tailnet limit is rejected before rollout'

COMPOSE_JSON=$default_compose_json
unset INSPECT_LOG_CONFIG

snapshot=$(target_snapshot 'TG_NEW_FLAG=on')
[ "$(jq -r '.container_env_added' <<< "$snapshot")" = TG_NEW_FLAG ]
if [ -e "$JQ_SECRET_ARG_MARKER" ]; then
  printf '%s\n' 'FAIL resolved Compose secrets were passed in subprocess arguments' >&2
  exit 1
fi
printf '%s\n' 'PASS resolved Compose secrets stay out of subprocess arguments'
printf '%s\n' 'PASS target container receives the named .env value'

snapshot=$(target_snapshot 'TG_NEW_FLAG=quoted value' 'TG_NEW_FLAG="quoted value"' 'quoted value')
[ "$(jq -r '.container_env_added' <<< "$snapshot")" = TG_NEW_FLAG ]
printf '%s\n' 'PASS quoted .env value matches the resolved Compose environment'

snapshot=$(target_snapshot 'TG_NEW_FLAG=on-suffix' 'TG_NEW_FLAG=on' 'on-suffix')
[ "$(jq -r '.container_env_added' <<< "$snapshot")" = TG_NEW_FLAG ]
printf '%s\n' 'PASS interpolated Compose value matches the target container'

printf 'TG_NEW_FLAG=on\n' > "$ENV_FILE"
inspected=$(inspect_json 'TG_NEW_FLAG=on')
if ROLLOUT_ADDED_ENV=TG_NEW_FLAG snapshot_from_json \
  "$inspected" "$COMPOSE_JSON" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
  >/dev/null 2>"$TMP/unresolved-compose.err"; then
  printf '%s\n' 'FAIL addition absent from resolved Compose environment was accepted' >&2
  exit 1
fi
! grep -q 'TG_NEW_FLAG=on' "$TMP/unresolved-compose.err"
printf '%s\n' 'PASS addition absent from the resolved Compose environment is rejected'

if target_snapshot '' >/dev/null 2>"$TMP/missing.err"; then
  printf '%s\n' 'FAIL missing named environment addition was accepted' >&2
  exit 1
fi
! grep -q 'TG_NEW_FLAG=on' "$TMP/missing.err"
printf '%s\n' 'PASS target container missing the named .env value is rejected'

if target_snapshot 'TG_NEW_FLAG=off' >/dev/null 2>"$TMP/mismatch.err"; then
  printf '%s\n' 'FAIL mismatched named environment addition was accepted' >&2
  exit 1
fi
! grep -Eq 'TG_NEW_FLAG=(on|off)' "$TMP/mismatch.err"
printf '%s\n' 'PASS target container with a mismatched named value is rejected'

snapshot=$(rollback_snapshot '')
[ "$(jq -r '.container_env_added' <<< "$snapshot")" = none ]
printf '%s\n' 'PASS rollback snapshot permits the previous release to omit the added variable'

snapshot=$(rollback_snapshot 'TG_NEW_FLAG=on')
[ "$(jq -r '.container_env_added' <<< "$snapshot")" = TG_NEW_FLAG ]
printf '%s\n' 'PASS rollback snapshot accepts a retained addition matching resolved Compose'

if rollback_snapshot 'TG_NEW_FLAG=off' >/dev/null 2>"$TMP/rollback-mismatch.err"; then
  printf '%s\n' 'FAIL rollback snapshot accepted a retained addition that mismatches resolved Compose' >&2
  exit 1
fi
! grep -Eq 'TG_NEW_FLAG=(on|off)' "$TMP/rollback-mismatch.err"
printf '%s\n' 'PASS rollback snapshot rejects a retained addition mismatching resolved Compose'

baseline=$(registration_snapshot 'TG_REGISTRATION=closed' open open baseline)
target=$(registration_snapshot 'TG_REGISTRATION=open' open open target)
[ "$(jq -r '.container_env_changed' <<< "$baseline")" = none ]
[ "$(jq -r '.container_env_changed' <<< "$target")" = TG_REGISTRATION ]
[ "$(jq -r '.container_env_sha256' <<< "$baseline")" = "$(jq -r '.container_env_sha256' <<< "$target")" ]
printf '%s\n' 'PASS target records the named registration value while preserving other environment equivalence'

rollback=$(registration_snapshot 'TG_REGISTRATION=open' open open rollback)
[ "$(jq -r '.container_env_changed' <<< "$rollback")" = TG_REGISTRATION ]
printf '%s\n' 'PASS rollback snapshot verifies and records a retained registration value'

if registration_snapshot 'TG_REGISTRATION=closed' open open target \
  >/dev/null 2>"$TMP/registration-container-mismatch.err"; then
  printf '%s\n' 'FAIL target accepted a registration value that differs from the requested .env' >&2
  exit 1
fi
! grep -Eq 'TG_REGISTRATION=(open|closed)' "$TMP/registration-container-mismatch.err"
printf '%s\n' 'PASS target rejects a registration value that differs from the requested .env'

if registration_snapshot 'TG_REGISTRATION=open' open closed target \
  >/dev/null 2>"$TMP/registration-compose-mismatch.err"; then
  printf '%s\n' 'FAIL target accepted a registration value that differs from resolved Compose' >&2
  exit 1
fi
! grep -Eq 'TG_REGISTRATION=(open|closed)' "$TMP/registration-compose-mismatch.err"
printf '%s\n' 'PASS target rejects a registration value that differs from resolved Compose'

legacy_inspect=$(inspect_json '')
legacy_snapshot=$(snapshot_from_json \
  "$legacy_inspect" "$COMPOSE_JSON" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render baseline)
public_ws_compose=$(jq -c '
  .services.teagramd.environment += {
    TG_PUBLIC_WEBSOCKET_LISTEN_ADDR:"",
    TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR:"",
    TG_PUBLIC_REGISTRATION:"closed",
    TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS:"",
    TG_WELCOME_CHANNEL:""
  }
' <<< "$COMPOSE_JSON")
public_ws_inspect=$(printf '%s' "$legacy_inspect" | jq -c '
  .Config.Env += [
    "TG_PUBLIC_WEBSOCKET_LISTEN_ADDR=",
    "TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR=",
    "TG_PUBLIC_REGISTRATION=closed",
    "TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS=",
    "TG_WELCOME_CHANNEL="
  ]
')
public_ws_snapshot=$(snapshot_from_json \
  "$public_ws_inspect" "$public_ws_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target)
[ "$(jq -r '.config_sha256' <<< "$legacy_snapshot")" = "$(jq -r '.config_sha256' <<< "$public_ws_snapshot")" ]
[ "$(jq -r '.container_env_sha256' <<< "$legacy_snapshot")" = "$(jq -r '.container_env_sha256' <<< "$public_ws_snapshot")" ]
printf '%s\n' 'PASS safe public defaults preserve resolved config and container environment equivalence'

syslog_logging='{"driver":"syslog","options":{"syslog-address":"unixgram:///dev/log","syslog-facility":"daemon","syslog-format":"rfc5424","tag":"teagramd"}}'
legacy_login_codes_compose=$(jq -c \
  '.services.teagramd.environment.TG_LOG_LOGIN_CODES="true"' <<< "$COMPOSE_JSON")
syslog_compose=$(jq -c --argjson logging "$syslog_logging" \
  '.services.teagramd.environment.TG_LOG_LOGIN_CODES="false" | .services.teagramd.logging=$logging' <<< "$COMPOSE_JSON")
syslog_options=$(jq -c '.options' <<< "$syslog_logging")
legacy_login_codes_inspect=$(printf '%s' "$legacy_inspect" | jq -c \
  '.Config.Env |= map(select(startswith("TG_LOG_LOGIN_CODES=") | not)) + ["TG_LOG_LOGIN_CODES=true"]')
syslog_inspect=$(printf '%s' "$legacy_inspect" | jq -c --argjson options "$syslog_options" \
  '.Id="cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc" | .Config.Env |= map(select(startswith("TG_LOG_LOGIN_CODES=") | not)) + ["TG_LOG_LOGIN_CODES=false"] | .HostConfig.LogConfig={Type:"syslog",Config:$options}')
default_logging_snapshot=$(snapshot_from_json \
  "$legacy_login_codes_inspect" "$legacy_login_codes_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render baseline)
syslog_logging_snapshot=$(snapshot_from_json \
  "$syslog_inspect" "$syslog_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target)
if [ "$(jq -r '.config_sha256' <<< "$default_logging_snapshot")" != \
       "$(jq -r '.config_sha256' <<< "$syslog_logging_snapshot")" ] || \
   [ "$(jq -r '.container_env_sha256' <<< "$default_logging_snapshot")" != \
       "$(jq -r '.container_env_sha256' <<< "$syslog_logging_snapshot")" ]; then
  printf '%s\n' 'FAIL reviewed previous-to-current profile changed unrelated Compose or container-environment digests' >&2
  exit 1
fi
[ "$(jq -r '.compose_logging_policy' <<< "$default_logging_snapshot")" = default ]
[ "$(jq -r '.compose_logging_policy' <<< "$syslog_logging_snapshot")" = syslog-v1 ]
if [ "$(jq -r '.compose_log_login_codes' <<< "$default_logging_snapshot")" != true ] || \
   [ "$(jq -r '.container_log_login_codes' <<< "$default_logging_snapshot")" != true ] || \
   [ "$(jq -r '.compose_log_login_codes' <<< "$syslog_logging_snapshot")" != false ] || \
   [ "$(jq -r '.container_log_login_codes' <<< "$syslog_logging_snapshot")" != false ]; then
  printf '%s\n' 'FAIL previous-to-current profile did not capture the exact login-code logging transition' >&2
  exit 1
fi
printf '%s\n' "$default_logging_snapshot" > "$TMP/baseline.snapshot.json"
printf '%s\n' "$syslog_logging_snapshot" > "$TMP/syslog-target.snapshot.json"
if ! compare_snapshots "$TMP/baseline.snapshot.json" "$TMP/syslog-target.snapshot.json" \
  sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
  "$TMP/profile-comparisons.tsv"; then
  printf '%s\n' 'FAIL the complete previous-to-current Compose profile was rejected' >&2
  cat "$TMP/profile-comparisons.tsv" >&2
  exit 1
fi
printf '%s\n' 'PASS complete previous-to-current Compose profile accepts default-to-syslog and true-to-false login-code logging'
if compare_snapshots "$TMP/baseline.snapshot.json" "$TMP/baseline.snapshot.json" \
  sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
  "$TMP/unmigrated-profile-comparisons.tsv" >/dev/null 2>&1; then
  printf '%s\n' 'FAIL unchanged default logging with login codes was accepted' >&2
  exit 1
fi
if ! grep -q $'compose_logging_policy\tdefault\tdefault\tfail' "$TMP/unmigrated-profile-comparisons.tsv" || \
   ! grep -q $'compose_log_login_codes\ttrue\ttrue\tfail' "$TMP/unmigrated-profile-comparisons.tsv"; then
  printf '%s\n' 'FAIL unchanged default logging did not fail both profile comparisons' >&2
  cat "$TMP/unmigrated-profile-comparisons.tsv" >&2
  exit 1
fi
printf '%s\n' 'PASS unchanged default logging with login codes is rejected by target verification'
if snapshot_from_json "$legacy_login_codes_inspect" "$syslog_compose" "$ENV_FILE" "$OVERRIDE_FILE" \
  selected-compose-render target >/dev/null 2>&1; then
  printf '%s\n' 'FAIL syslog target accepted a container with a different active logger or login-code setting' >&2
  exit 1
fi
syslog_enabled_codes_compose=$(jq -c --argjson logging "$syslog_logging" \
  '.services.teagramd.environment.TG_LOG_LOGIN_CODES="true" | .services.teagramd.logging=$logging' <<< "$COMPOSE_JSON")
syslog_enabled_codes_inspect=$(printf '%s' "$legacy_login_codes_inspect" | jq -c --argjson options "$syslog_options" \
  '.HostConfig.LogConfig={Type:"syslog",Config:$options}')
if snapshot_from_json "$syslog_enabled_codes_inspect" "$syslog_enabled_codes_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target >/dev/null 2>&1; then
  printf '%s\n' 'FAIL syslog target accepted login-code logging enabled' >&2
  exit 1
fi
unapproved_logging_compose=$(jq -c '.services.teagramd.logging.options.tag="other"' <<< "$syslog_compose")
if snapshot_from_json "$syslog_inspect" "$unapproved_logging_compose" "$ENV_FILE" "$OVERRIDE_FILE" \
  selected-compose-render preflight >/dev/null 2>&1; then
  printf '%s\n' 'FAIL unreviewed syslog options were accepted' >&2
  exit 1
fi
rollback_logger_mismatch_inspect=$(printf '%s' "$legacy_login_codes_inspect" | jq -c --argjson options "$syslog_options" \
  '.HostConfig.LogConfig={Type:"syslog",Config:$options}')
rollback_logger_mismatch=$(snapshot_from_json "$rollback_logger_mismatch_inspect" "$legacy_login_codes_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render rollback)
printf '%s\n' "$rollback_logger_mismatch" > "$TMP/mismatched-rollback.snapshot.json"
if compare_snapshots "$TMP/baseline.snapshot.json" "$TMP/mismatched-rollback.snapshot.json" \
  sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
  "$TMP/mismatched-rollback-comparisons.tsv" >/dev/null 2>&1; then
  printf '%s\n' 'FAIL rollback accepted a different inspected logging driver' >&2
  exit 1
fi
if ! grep -q $'container_logging_sha256\t.*\tfail' "$TMP/mismatched-rollback-comparisons.tsv"; then
  printf '%s\n' 'FAIL mismatched rollback did not fail the inspected logging comparison' >&2
  cat "$TMP/mismatched-rollback-comparisons.tsv" >&2
  exit 1
fi
printf '%s\n' 'PASS rollback equivalence rejects a changed inspected logging driver'
printf '%s\n' 'PASS reviewed syslog policy is isolated, exact, and verified against the target container'

for setting in TG_PUBLIC_WEBSOCKET_LISTEN_ADDR TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR TG_PUBLIC_REGISTRATION TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS TG_WELCOME_CHANNEL; do
  case "$setting" in
    TG_PUBLIC_REGISTRATION) unsafe_value=invite ;;
    TG_WELCOME_CHANNEL) unsafe_value=welcome ;;
    TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR) unsafe_value=server.example.com:443 ;;
    *) unsafe_value=https://public.example ;;
  esac
  unsafe_compose=$(jq -c --arg setting "$setting" --arg value "$unsafe_value" \
    '.services.teagramd.environment[$setting]=$value' <<< "$public_ws_compose")
  if snapshot_from_json "$public_ws_inspect" "$unsafe_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
    >/dev/null 2>"$TMP/$setting-compose.err"; then
    printf 'FAIL unsafe resolved Compose setting was accepted: %s\n' "$setting" >&2
    exit 1
  fi
  printf 'PASS unsafe resolved Compose setting is rejected: %s\n' "$setting"

  unsafe_inspect=$(printf '%s' "$public_ws_inspect" | jq -c --arg setting "$setting" --arg value "$unsafe_value" '
    .Config.Env |= (map(select((startswith($setting + "=")) | not)) + [$setting + "=" + $value])
  ')
  if snapshot_from_json "$unsafe_inspect" "$public_ws_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
    >/dev/null 2>"$TMP/$setting-container.err"; then
    printf 'FAIL unsafe container setting was accepted: %s\n' "$setting" >&2
    exit 1
  fi
  printf 'PASS unsafe container setting is rejected: %s\n' "$setting"
done

for setting_value in \
  TG_PUBLIC_WEBSOCKET_LISTEN_ADDR=0.0.0.0:2444 \
  TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR=server.example.com:443 \
  TG_PUBLIC_REGISTRATION=invite \
  TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS=https://web.teagram.io; do
  setting=${setting_value%%=*}
  value=${setting_value#*=}
  named_public_snapshot=$(public_setting_snapshot "$setting" "$value" "$setting=$value" target "$setting")
  [ "$(jq -r '.container_env_added' <<< "$named_public_snapshot")" = "$setting" ]
  printf 'PASS named public setting reaches the resolved Compose and target container: %s\n' "$setting"
done

setting=TG_PUBLIC_WEBSOCKET_LISTEN_ADDR
value=0.0.0.0:2444
printf '%s=%s\n' "$setting" "$value" > "$ENV_FILE"
named_public_compose=$(jq -c --arg setting "$setting" --arg value "$value" \
  '.services.teagramd.environment[$setting]=$value' <<< "$public_ws_compose")
named_public_preflight=$(ROLLOUT_ADDED_ENV="$setting" snapshot_from_json \
  "$legacy_inspect" "$named_public_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render preflight)
[ "$(jq -r '.container_env_added' <<< "$named_public_preflight")" = none ]
printf '%s\n' 'PASS named public setting is admitted during preflight before replacement'

printf 'TG_PUBLIC_WEBSOCKET_LISTEN_ADDR="0.0.0.0:2444"\n' > "$ENV_FILE"
public_listen_quoted_compose=$(jq -c '.services.teagramd.environment.TG_PUBLIC_WEBSOCKET_LISTEN_ADDR="0.0.0.0:2444"' <<< "$public_ws_compose")
public_listen_quoted_inspect=$(printf '%s' "$legacy_inspect" | jq -c \
  '.Config.Env += ["TG_PUBLIC_WEBSOCKET_LISTEN_ADDR=0.0.0.0:2444"]')
named_public_quoted_target=$(ROLLOUT_ADDED_ENV=TG_PUBLIC_WEBSOCKET_LISTEN_ADDR snapshot_from_json \
  "$public_listen_quoted_inspect" "$public_listen_quoted_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target)
[ "$(jq -r '.container_env_added' <<< "$named_public_quoted_target")" = TG_PUBLIC_WEBSOCKET_LISTEN_ADDR ]
printf '%s\n' 'PASS quoted public .env value reaches the exact resolved Compose and target value'

printf 'TG_PUBLIC_WEBSOCKET_LISTEN_ADDR=0.0.0.0:2444\n' > "$ENV_FILE"
public_listen_mismatched_compose=$(jq -c '.services.teagramd.environment.TG_PUBLIC_WEBSOCKET_LISTEN_ADDR="127.0.0.1:2444"' <<< "$public_ws_compose")
public_listen_mismatched_inspect=$(printf '%s' "$legacy_inspect" | jq -c \
  '.Config.Env += ["TG_PUBLIC_WEBSOCKET_LISTEN_ADDR=127.0.0.1:2444"]')
if ROLLOUT_ADDED_ENV=TG_PUBLIC_WEBSOCKET_LISTEN_ADDR snapshot_from_json \
  "$public_listen_mismatched_inspect" "$public_listen_mismatched_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
  >/dev/null 2>"$TMP/public-listen-mismatched-source.err"; then
  printf '%s\n' 'FAIL public setting whose resolved value differs from .env was accepted' >&2
  exit 1
fi
printf '%s\n' 'PASS public addition rejects a resolved value that differs from its .env assignment'

printf 'TG_WELCOME_CHANNEL=welcome\n' > "$ENV_FILE"
welcome_active_compose=$(jq -c '.services.teagramd.environment.TG_WELCOME_CHANNEL="welcome"' <<< "$public_ws_compose")
welcome_active_inspect=$(printf '%s' "$public_ws_inspect" | jq -c '
  .Config.Env |= (map(select(startswith("TG_WELCOME_CHANNEL=") | not)) + ["TG_WELCOME_CHANNEL=welcome"])
')
if ROLLOUT_ADDED_ENV=TG_WELCOME_CHANNEL snapshot_from_json \
  "$welcome_active_inspect" "$welcome_active_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
  >/dev/null 2>"$TMP/welcome-active-named.err"; then
  printf '%s\n' 'FAIL named TG_WELCOME_CHANNEL activation was accepted' >&2
  exit 1
fi
printf '%s\n' 'PASS TG_WELCOME_CHANNEL remains fail-closed when named'

printf 'TG_PUBLIC_REGISTRATION=open\n' > "$ENV_FILE"
public_registration_open_compose=$(jq -c '.services.teagramd.environment.TG_PUBLIC_REGISTRATION="open"' <<< "$public_ws_compose")
public_registration_open_inspect=$(printf '%s' "$public_ws_inspect" | jq -c '
  .Config.Env |= (map(select(startswith("TG_PUBLIC_REGISTRATION=") | not)) + ["TG_PUBLIC_REGISTRATION=open"])
')
if ROLLOUT_ADDED_ENV=TG_PUBLIC_REGISTRATION snapshot_from_json \
  "$public_registration_open_inspect" "$public_registration_open_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
  >/dev/null 2>"$TMP/public-registration-open.err"; then
  printf '%s\n' 'FAIL named TG_PUBLIC_REGISTRATION=open was accepted' >&2
  exit 1
fi
printf '%s\n' 'PASS TG_PUBLIC_REGISTRATION=open is rejected even when named'

printf 'TG_PUBLIC_WEBSOCKET_LISTEN_ADDR=127.0.0.1:2444\n' > "$ENV_FILE"
public_listen_baseline_inspect=$(printf '%s' "$legacy_inspect" | jq -c '.Config.Env += ["TG_PUBLIC_WEBSOCKET_LISTEN_ADDR=0.0.0.0:2444"]')
public_listen_changed_compose=$(jq -c '.services.teagramd.environment.TG_PUBLIC_WEBSOCKET_LISTEN_ADDR="127.0.0.1:2444"' <<< "$public_ws_compose")
if snapshot_from_json "$public_listen_baseline_inspect" "$public_listen_changed_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render preflight \
  >/dev/null 2>"$TMP/public-listen-changed-unnamed.err"; then
  printf '%s\n' 'FAIL changed public WebSocket setting was accepted without the change path' >&2
  exit 1
fi
public_listen_change_preflight=$(ROLLOUT_CHANGED_ENV=TG_PUBLIC_WEBSOCKET_LISTEN_ADDR snapshot_from_json \
  "$public_listen_baseline_inspect" "$public_listen_changed_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render preflight)
public_listen_change_target_inspect=$(printf '%s' "$legacy_inspect" | jq -c \
  '.Config.Env += ["TG_PUBLIC_WEBSOCKET_LISTEN_ADDR=127.0.0.1:2444"]')
public_listen_change_target=$(ROLLOUT_CHANGED_ENV=TG_PUBLIC_WEBSOCKET_LISTEN_ADDR snapshot_from_json \
  "$public_listen_change_target_inspect" "$public_listen_changed_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target)
[ "$(jq -r '.container_env_changed' <<< "$public_listen_change_preflight")" = none ]
[ "$(jq -r '.container_env_changed' <<< "$public_listen_change_target")" = TG_PUBLIC_WEBSOCKET_LISTEN_ADDR ]
printf '%s\n' 'PASS changed public WebSocket value requires and accepts the named change path'

printf 'TG_PUBLIC_WEBSOCKET_LISTEN_ADDR="127.0.0.1:2444"\n' > "$ENV_FILE"
public_listen_quoted_change_target=$(ROLLOUT_CHANGED_ENV=TG_PUBLIC_WEBSOCKET_LISTEN_ADDR snapshot_from_json \
  "$public_listen_change_target_inspect" "$public_listen_changed_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target)
[ "$(jq -r '.container_env_changed' <<< "$public_listen_quoted_change_target")" = TG_PUBLIC_WEBSOCKET_LISTEN_ADDR ]
printf '%s\n' 'PASS quoted changed public WebSocket value matches the target container'

printf 'TG_PUBLIC_REGISTRATION="closed"\n' > "$ENV_FILE"
public_registration_closed_compose=$(jq -c '.services.teagramd.environment.TG_PUBLIC_REGISTRATION="closed"' <<< "$public_ws_compose")
public_registration_closed_inspect=$(printf '%s' "$legacy_inspect" | jq -c \
  '.Config.Env += ["TG_PUBLIC_REGISTRATION=closed"]')
public_registration_quoted_change_target=$(ROLLOUT_CHANGED_ENV=TG_PUBLIC_REGISTRATION snapshot_from_json \
  "$public_registration_closed_inspect" "$public_registration_closed_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target)
[ "$(jq -r '.container_env_changed' <<< "$public_registration_quoted_change_target")" = TG_PUBLIC_REGISTRATION ]
printf '%s\n' 'PASS quoted TG_PUBLIC_REGISTRATION change matches the target container'

websocket_origins='https://teagram.tailaa4918.ts.net,https://web.teagram.io'
printf 'TG_WEBSOCKET_ALLOWED_ORIGINS=%s\n' "$websocket_origins" > "$ENV_FILE"
websocket_origins_compose=$(jq -c --arg value "$websocket_origins" \
  '.services.teagramd.environment.TG_WEBSOCKET_ALLOWED_ORIGINS=$value' <<< "$public_ws_compose")
websocket_origins_inspect=$(printf '%s' "$legacy_inspect" | jq -c --arg value "$websocket_origins" \
  '.Config.Env += ["TG_WEBSOCKET_ALLOWED_ORIGINS=" + $value]')
websocket_origins_target=$(ROLLOUT_CHANGED_ENV=TG_WEBSOCKET_ALLOWED_ORIGINS snapshot_from_json \
  "$websocket_origins_inspect" "$websocket_origins_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target)
[ "$(jq -r '.container_env_changed' <<< "$websocket_origins_target")" = TG_WEBSOCKET_ALLOWED_ORIGINS ]
printf '%s\n' 'PASS changed TG_WEBSOCKET_ALLOWED_ORIGINS matches the resolved Compose and target container'

websocket_origin_before='https://teagram.tailaa4918.ts.net'
websocket_origin_after='https://teagram.tailaa4918.ts.net,https://web.teagram.io'
printf 'services:\n  teagramd:\n    environment:\n      TG_WEBSOCKET_ALLOWED_ORIGINS: %s\n' \
  "$websocket_origin_after" > "$OVERRIDE_FILE"
printf 'TG_NEW_FLAG=on\n' > "$ENV_FILE"
override_origins_compose=$(jq -c --arg value "$websocket_origin_after" \
  '.services.teagramd.environment.TG_WEBSOCKET_ALLOWED_ORIGINS=$value' <<< "$public_ws_compose")
override_origins_baseline_inspect=$(printf '%s' "$legacy_inspect" | jq -c --arg value "$websocket_origin_before" \
  '.Config.Env += ["TG_WEBSOCKET_ALLOWED_ORIGINS=" + $value]')
override_origins_target_inspect=$(printf '%s' "$legacy_inspect" | jq -c --arg value "$websocket_origin_after" \
  '.Config.Env += ["TG_WEBSOCKET_ALLOWED_ORIGINS=" + $value]')
override_origins_baseline=$(ROLLOUT_CHANGED_ENV=TG_WEBSOCKET_ALLOWED_ORIGINS \
  ROLLOUT_CHANGED_ENV_SOURCE=override \
  ROLLOUT_OVERRIDE_ORIGIN_VALUE="$websocket_origin_after" \
  ROLLOUT_PREVIOUS_OVERRIDE_ORIGIN_VALUE="$websocket_origin_before" snapshot_from_json \
  "$override_origins_baseline_inspect" "$override_origins_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render baseline)
override_origins_target=$(ROLLOUT_CHANGED_ENV=TG_WEBSOCKET_ALLOWED_ORIGINS \
  ROLLOUT_CHANGED_ENV_SOURCE=override \
  ROLLOUT_OVERRIDE_ORIGIN_VALUE="$websocket_origin_after" \
  ROLLOUT_PREVIOUS_OVERRIDE_ORIGIN_VALUE="$websocket_origin_before" snapshot_from_json \
  "$override_origins_target_inspect" "$override_origins_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target)
[ "$(jq -r '.container_env_changed' <<< "$override_origins_baseline")" = none ]
[ "$(jq -r '.container_env_changed' <<< "$override_origins_target")" = TG_WEBSOCKET_ALLOWED_ORIGINS ]
printf '%s\n' 'PASS override-only WebSocket origin baseline and target are checked against their effective source'

printf '%s\n' \
  'TG_PUBLIC_WEBSOCKET_LISTEN_ADDR=0.0.0.0:2444' \
  'TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR=server.example.com:443' \
  'TG_PUBLIC_REGISTRATION=invite' \
  'TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS=https://web.teagram.io' > "$ENV_FILE"
public_retained_compose=$(jq -c '
  .services.teagramd.environment += {
    TG_PUBLIC_WEBSOCKET_LISTEN_ADDR:"0.0.0.0:2444",
    TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR:"server.example.com:443",
    TG_PUBLIC_REGISTRATION:"invite",
    TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS:"https://web.teagram.io"
  }
' <<< "$public_ws_compose")
public_retained_inspect=$(printf '%s' "$legacy_inspect" | jq -c '
  .Config.Env += [
    "TG_PUBLIC_WEBSOCKET_LISTEN_ADDR=0.0.0.0:2444",
    "TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR=server.example.com:443",
    "TG_PUBLIC_REGISTRATION=invite",
    "TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS=https://web.teagram.io"
  ]
')
public_retained_baseline=$(snapshot_from_json \
  "$public_retained_inspect" "$public_retained_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render baseline)
public_retained_target=$(snapshot_from_json \
  "$public_retained_inspect" "$public_retained_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target)
[ "$(jq -r '.container_env_sha256' <<< "$public_retained_baseline")" = "$(jq -r '.container_env_sha256' <<< "$public_retained_target")" ]
printf '%s\n' 'PASS previously added public settings are retained on a later unnamed rollout'

turnstile_legacy_inspect=$(inspect_json '')
turnstile_legacy_snapshot=$(snapshot_from_json \
  "$turnstile_legacy_inspect" "$COMPOSE_JSON" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render baseline)
turnstile_disabled_compose=$(jq -c '
  .services.teagramd.environment += {
    TG_TURNSTILE_SITE_KEY:"",
    TG_TURNSTILE_SECRET_KEY_FILE:"",
    TG_TURNSTILE_SIGNIN_ENABLED:"false"
  }
  | .services.teagramd.secrets = [{
      source:"teagramd_turnstile",
      target:"teagramd-turnstile"
    }]
  | .secrets.teagramd_turnstile = {file:"/dev/null"}
' <<< "$COMPOSE_JSON")
turnstile_disabled_inspect=$(printf '%s' "$turnstile_legacy_inspect" | jq -c '
  .Config.Env += [
    "TG_TURNSTILE_SITE_KEY=",
    "TG_TURNSTILE_SECRET_KEY_FILE=",
    "TG_TURNSTILE_SIGNIN_ENABLED=false"
  ]
  | .Mounts += [{
      Type:"bind",
      Source:"/dev/null",
      Destination:"/run/secrets/teagramd-turnstile",
      Mode:"ro",
      RW:false,
      Propagation:"rprivate"
    }]
')
turnstile_disabled_snapshot=$(snapshot_from_json \
  "$turnstile_disabled_inspect" "$turnstile_disabled_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target)
[ "$(jq -r '.config_sha256' <<< "$turnstile_legacy_snapshot")" = "$(jq -r '.config_sha256' <<< "$turnstile_disabled_snapshot")" ]
[ "$(jq -r '.mounts_sha256' <<< "$turnstile_legacy_snapshot")" = "$(jq -r '.mounts_sha256' <<< "$turnstile_disabled_snapshot")" ]
[ "$(jq -r '.container_env_sha256' <<< "$turnstile_legacy_snapshot")" = "$(jq -r '.container_env_sha256' <<< "$turnstile_disabled_snapshot")" ]
printf '%s\n' 'PASS disabled Turnstile defaults and its exact read-only secret mount preserve rollout equivalence'

turnstile_local_compose=$(jq -c '
  .services.teagramd.environment += {
    TG_TURNSTILE_SITE_KEY:"",
    TG_TURNSTILE_SECRET_KEY_FILE:"",
    TG_TURNSTILE_SIGNIN_ENABLED:"false"
  }
  | .services.teagramd.secrets = []
  | .secrets |= del(.teagramd_turnstile)
  | if (.secrets | length) == 0 then del(.secrets) else . end
' <<< "$turnstile_disabled_compose")
turnstile_local_inspect=$(printf '%s' "$turnstile_legacy_inspect" | jq -c '
  .Config.Env += [
    "TG_TURNSTILE_SITE_KEY=",
    "TG_TURNSTILE_SECRET_KEY_FILE=",
    "TG_TURNSTILE_SIGNIN_ENABLED=false"
  ]
')
turnstile_local_snapshot=$(snapshot_from_json \
  "$turnstile_local_inspect" "$turnstile_local_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target)
[ "$(jq -r '.config_sha256' <<< "$turnstile_legacy_snapshot")" = "$(jq -r '.config_sha256' <<< "$turnstile_local_snapshot")" ]
[ "$(jq -r '.mounts_sha256' <<< "$turnstile_legacy_snapshot")" = "$(jq -r '.mounts_sha256' <<< "$turnstile_local_snapshot")" ]
[ "$(jq -r '.container_env_sha256' <<< "$turnstile_legacy_snapshot")" = "$(jq -r '.container_env_sha256' <<< "$turnstile_local_snapshot")" ]
printf '%s\n' 'PASS disabled Turnstile defaults tolerate the local blob overlay resetting service secrets'

busybox_readlink() {
  [ "$1" = -f ] || { printf 'readlink: unrecognized option: %s\n' "$1" >&2; return 1; }
  command readlink -f "$2"
}
readlink() { busybox_readlink "$@"; }
verify_turnstile_compose_configuration "$turnstile_disabled_compose"
unset -f readlink busybox_readlink
printf '%s\n' 'PASS Turnstile path validation supports BusyBox readlink'

turnstile_secret_dir="$ROLLOUT_CHECKOUT_PATH/.secrets"
mkdir -m 700 "$turnstile_secret_dir"
turnstile_secret_source="$turnstile_secret_dir/teagramd-turnstile"
: > "$turnstile_secret_source"
chmod 444 "$turnstile_secret_source"
turnstile_compose=$(jq -c --arg source "$turnstile_secret_source" '
  .services.teagramd.environment += {
    TG_TURNSTILE_SITE_KEY:"fixture-site-key",
    TG_TURNSTILE_SECRET_KEY_FILE:"/run/secrets/teagramd-turnstile",
    TG_TURNSTILE_SIGNIN_ENABLED:"false"
  }
  | .services.teagramd.secrets = [{
      source:"teagramd_turnstile",
      target:"teagramd-turnstile"
    }]
  | .secrets.teagramd_turnstile = {file:$source}
' <<< "$COMPOSE_JSON")
turnstile_enabled_local_compose=$(jq -c '
  .services.teagramd.secrets = []
  | .secrets |= del(.teagramd_turnstile)
  | if (.secrets | length) == 0 then del(.secrets) else . end
' <<< "$turnstile_compose")
if verify_turnstile_compose_configuration "$turnstile_enabled_local_compose" \
  >/dev/null 2>"$TMP/turnstile-enabled-local.err"; then
  printf '%s\n' 'FAIL local blob overlay hid an enabled Turnstile secret' >&2
  exit 1
fi
printf '%s\n' 'PASS local blob overlay cannot hide an enabled Turnstile secret'

turnstile_inspect=$(printf '%s' "$turnstile_legacy_inspect" | jq -c --arg source "$turnstile_secret_source" '
  .Config.Env += [
    "TG_TURNSTILE_SITE_KEY=fixture-site-key",
    "TG_TURNSTILE_SECRET_KEY_FILE=/run/secrets/teagramd-turnstile",
    "TG_TURNSTILE_SIGNIN_ENABLED=false"
  ]
  | .Mounts += [{
      Type:"bind",
      Source:$source,
      Destination:"/run/secrets/teagramd-turnstile",
      Mode:"ro",
      RW:false,
      Propagation:"rprivate"
    }]
')
turnstile_enabled_snapshot=$(snapshot_from_json \
  "$turnstile_inspect" "$turnstile_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target)
[ "$(jq -r '.config_sha256' <<< "$turnstile_legacy_snapshot")" = "$(jq -r '.config_sha256' <<< "$turnstile_enabled_snapshot")" ]
[ "$(jq -r '.mounts_sha256' <<< "$turnstile_legacy_snapshot")" = "$(jq -r '.mounts_sha256' <<< "$turnstile_enabled_snapshot")" ]
printf '%s\n' 'PASS mapped Turnstile credentials use a read-only file mount without changing rollout equivalence'

turnstile_writable_inspect=$(printf '%s' "$turnstile_inspect" | jq -c '.Mounts[0].RW=true')
if snapshot_from_json "$turnstile_writable_inspect" "$turnstile_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
  >/dev/null 2>"$TMP/turnstile-writable-mount.err"; then
  printf '%s\n' 'FAIL writable Turnstile secret mount was accepted' >&2
  exit 1
fi
printf '%s\n' 'PASS writable Turnstile secret mount is rejected'

turnstile_unsafe_source_compose=$(printf '%s' "$turnstile_compose" | jq -c '.secrets.teagramd_turnstile.file="/etc/passwd"')
turnstile_unsafe_source_inspect=$(printf '%s' "$turnstile_inspect" | jq -c '.Mounts[0].Source="/etc/passwd"')
if snapshot_from_json "$turnstile_unsafe_source_inspect" "$turnstile_unsafe_source_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
  >/dev/null 2>"$TMP/turnstile-unsafe-source.err"; then
  printf '%s\n' 'FAIL unapproved Turnstile secret source was accepted' >&2
  exit 1
fi
printf '%s\n' 'PASS unapproved Turnstile secret source is rejected'

turnstile_secret_env_inspect=$(printf '%s' "$turnstile_inspect" | jq -c '.Config.Env += ["TG_TURNSTILE_SECRET_KEY=fixture-secret"]')
if snapshot_from_json "$turnstile_secret_env_inspect" "$turnstile_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
  >/dev/null 2>"$TMP/turnstile-secret-env.err"; then
  printf '%s\n' 'FAIL Turnstile secret in container environment was accepted' >&2
  exit 1
fi
printf '%s\n' 'PASS Turnstile secret in container environment is rejected'

printf 'TEAGRAM_TURNSTILE_SITE_KEY=fixture-site-key\nTEAGRAM_TURNSTILE_SECRET_KEY_FILE=%s\n' \
  "$turnstile_secret_source" > "$ENV_FILE"
turnstile_alias_compose=$(jq -c --arg source "$turnstile_secret_source" '
  .services.teagramd.environment += {
    TG_TURNSTILE_SITE_KEY:"fixture-site-key",
    TG_TURNSTILE_SECRET_KEY_FILE:"/run/secrets/teagramd-turnstile",
    TG_TURNSTILE_SIGNIN_ENABLED:"false"
  }
  | .services.teagramd.secrets = [{source:"teagramd_turnstile",target:"teagramd-turnstile"}]
  | .secrets.teagramd_turnstile = {file:$source}
' <<< "$COMPOSE_JSON")
turnstile_alias_inspect=$(printf '%s' "$turnstile_legacy_inspect" | jq -c --arg source "$turnstile_secret_source" '
  .Config.Env += [
    "TG_TURNSTILE_SITE_KEY=fixture-site-key",
    "TG_TURNSTILE_SECRET_KEY_FILE=/run/secrets/teagramd-turnstile",
    "TG_TURNSTILE_SIGNIN_ENABLED=false"
  ]
  | .Mounts += [{
      Type:"bind",
      Source:$source,
      Destination:"/run/secrets/teagramd-turnstile",
      Mode:"ro",
      RW:false,
      Propagation:"rprivate"
    }]
')
turnstile_alias_snapshot=$(ROLLOUT_ADDED_ENV=TEAGRAM_TURNSTILE_SITE_KEY,TEAGRAM_TURNSTILE_SECRET_KEY_FILE \
  snapshot_from_json "$turnstile_alias_inspect" "$turnstile_alias_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target)
[ "$(jq -r '.config_sha256' <<< "$turnstile_legacy_snapshot")" = "$(jq -r '.config_sha256' <<< "$turnstile_alias_snapshot")" ]
[ "$(jq -r '.mounts_sha256' <<< "$turnstile_legacy_snapshot")" = "$(jq -r '.mounts_sha256' <<< "$turnstile_alias_snapshot")" ]
[ "$(jq -r '.container_env_sha256' <<< "$turnstile_legacy_snapshot")" = "$(jq -r '.container_env_sha256' <<< "$turnstile_alias_snapshot")" ]
[ "$(jq -r '.container_env_added' <<< "$turnstile_alias_snapshot")" = "TEAGRAM_TURNSTILE_SECRET_KEY_FILE,TEAGRAM_TURNSTILE_SITE_KEY" ]
printf '%s\n' 'PASS named TEAGRAM host additions verify their mapped TG container settings'

turnstile_signin_snapshot() {
  local value=$1 name=$2 inspected compose
  printf 'TEAGRAM_TURNSTILE_SITE_KEY=fixture-site-key\nTEAGRAM_TURNSTILE_SECRET_KEY_FILE=%s\nTG_TURNSTILE_SIGNIN_ENABLED=%s\n' \
    "$turnstile_secret_source" "$value" > "$ENV_FILE"
  inspected=$(printf '%s' "$turnstile_alias_inspect" | jq -c --arg value "$value" '
    .Config.Env |= map(if startswith("TG_TURNSTILE_SIGNIN_ENABLED=") then "TG_TURNSTILE_SIGNIN_ENABLED=" + $value else . end)
  ')
  compose=$(printf '%s' "$turnstile_alias_compose" | jq -c --arg value "$value" \
    '.services.teagramd.environment.TG_TURNSTILE_SIGNIN_ENABLED=$value')
  ROLLOUT_CHANGED_ENV=TG_TURNSTILE_SIGNIN_ENABLED \
    snapshot_from_json "$inspected" "$compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render "$name"
}

turnstile_signin_off=$(turnstile_signin_snapshot false baseline)
turnstile_signin_on=$(turnstile_signin_snapshot true target)
[ "$(jq -r '.container_env_changed' <<< "$turnstile_signin_off")" = none ]
[ "$(jq -r '.container_env_changed' <<< "$turnstile_signin_on")" = TG_TURNSTILE_SIGNIN_ENABLED ]
[ "$(jq -r '.container_env_sha256' <<< "$turnstile_signin_off")" = "$(jq -r '.container_env_sha256' <<< "$turnstile_signin_on")" ]
printf '%s\n' 'PASS named Turnstile sign-in changes accept only boolean values and preserve other environment equivalence'

printf 'TG_TURNSTILE_SIGNIN_ENABLED=false\n' > "$ENV_FILE"
turnstile_signin_old_inspect=$(printf '%s' "$turnstile_alias_inspect" | jq -c '
  .Config.Env |= map(if startswith("TG_TURNSTILE_SIGNIN_ENABLED=") then "TG_TURNSTILE_SIGNIN_ENABLED=true" else . end)
')
ROLLOUT_CHANGED_ENV=TG_TURNSTILE_SIGNIN_ENABLED \
  snapshot_from_json "$turnstile_signin_old_inspect" "$turnstile_alias_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render baseline >/dev/null
if ROLLOUT_CHANGED_ENV=TG_TURNSTILE_SIGNIN_ENABLED \
  snapshot_from_json "$turnstile_signin_old_inspect" "$turnstile_alias_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
  >/dev/null 2>"$TMP/turnstile-signin-old-target.err"; then
  printf '%s\n' 'FAIL target kept the previous Turnstile sign-in value' >&2
  exit 1
fi
if snapshot_from_json "$turnstile_signin_old_inspect" "$turnstile_alias_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render baseline \
  >/dev/null 2>"$TMP/turnstile-signin-unnamed.err"; then
  printf '%s\n' 'FAIL baseline accepted an unnamed Turnstile sign-in difference' >&2
  exit 1
fi
printf '%s\n' 'PASS baseline may hold the previous value of a named Turnstile sign-in change; target and unnamed differences may not'

printf 'TEAGRAM_TURNSTILE_SITE_KEY=fixture-site-key\nTEAGRAM_TURNSTILE_SECRET_KEY_FILE=%s\n' \
  "$turnstile_secret_source" > "$ENV_FILE"
turnstile_enable_baseline=$(ROLLOUT_ADDED_ENV=TEAGRAM_TURNSTILE_SITE_KEY,TEAGRAM_TURNSTILE_SECRET_KEY_FILE \
  snapshot_from_json "$turnstile_disabled_inspect" "$turnstile_alias_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render baseline)
[ "$(jq -r '.container_env_added' <<< "$turnstile_enable_baseline")" = none ]
[ "$(jq -r '.container_env_sha256' <<< "$turnstile_enable_baseline")" = "$(jq -r '.container_env_sha256' <<< "$turnstile_alias_snapshot")" ]
if ROLLOUT_ADDED_ENV=TEAGRAM_TURNSTILE_SITE_KEY,TEAGRAM_TURNSTILE_SECRET_KEY_FILE \
  snapshot_from_json "$turnstile_disabled_inspect" "$turnstile_alias_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
  >/dev/null 2>"$TMP/turnstile-enable-target.err"; then
  printf '%s\n' 'FAIL target kept the disabled Turnstile defaults' >&2
  exit 1
fi
printf 'TG_TURNSTILE_SIGNIN_ENABLED=false\n' > "$ENV_FILE"
turnstile_signin_add_baseline=$(ROLLOUT_ADDED_ENV=TG_TURNSTILE_SIGNIN_ENABLED \
  snapshot_from_json "$turnstile_disabled_inspect" "$turnstile_disabled_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render baseline)
[ "$(jq -r '.container_env_added' <<< "$turnstile_signin_add_baseline")" = none ]
printf '%s\n' 'PASS live disabled Turnstile defaults count as unset when the deploy adds Turnstile settings'

media_key_source="$TMP/media-keys"
mkdir -m 700 "$media_key_source"
printf '%064d' 0 > "$media_key_source/1.key"
chmod 600 "$media_key_source/1.key"
if [ "$(id -u)" = 0 ]; then
  chown 65532:65532 "$media_key_source" "$media_key_source/1.key"
fi
media_key_compose=$(jq -c --arg source "$media_key_source" '
  .["x-media-keys"]={environment:{TG_MEDIA_KEY_DIR:"/run/teagramd/media-keys",TG_MEDIA_KEY_CURRENT:"1"}}
  | .services.teagramd.environment.TG_MEDIA_KEY_DIR="/run/teagramd/media-keys"
  | .services.teagramd.environment.TG_MEDIA_KEY_CURRENT="1"
  | .services.teagramd.volumes += [{
      type:"bind",source:$source,target:"/run/teagramd/media-keys",read_only:true,
      bind:{create_host_path:false}
    }]
' <<< "$syslog_compose")
media_key_inspect=$(printf '%s' "$syslog_inspect" | jq -c --arg source "$media_key_source" '
  .Config.Env += [
    "TG_MEDIA_KEY_DIR=/run/teagramd/media-keys",
    "TG_MEDIA_KEY_CURRENT=1"
  ]
  | .Mounts += [{
      Type:"bind",Source:$source,Destination:"/run/teagramd/media-keys",
      Mode:"ro",RW:false,Propagation:"rprivate"
    }]
')
media_key_baseline=$(snapshot_from_json \
  "$syslog_inspect" "$syslog_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render baseline)
media_key_target=$(snapshot_from_json \
  "$media_key_inspect" "$media_key_compose" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target)
[ "$(jq -r '.media_key_source' <<< "$media_key_baseline")" = unset ]
[ "$(jq -r '.media_key_current' <<< "$media_key_baseline")" = unset ]
[ "$(jq -r '.media_key_source' <<< "$media_key_target")" = "$media_key_source" ]
[ "$(jq -r '.media_key_current' <<< "$media_key_target")" = 1 ]
[ "$(jq -r '.config_sha256' <<< "$media_key_baseline")" = "$(jq -r '.config_sha256' <<< "$media_key_target")" ]
printf '%s\n' 'PASS Compose extension anchors do not change the resolved runtime digest'
[ "$(jq -r '.mounts_sha256' <<< "$media_key_baseline")" = "$(jq -r '.mounts_sha256' <<< "$media_key_target")" ]
[ "$(jq -r '.container_env_sha256' <<< "$media_key_baseline")" = "$(jq -r '.container_env_sha256' <<< "$media_key_target")" ]
media_key_compose_empty_bind=$(jq -c '.services.teagramd.volumes[-1].bind={}' <<< "$media_key_compose")
printf '%s\n' "$media_key_compose_empty_bind" > "$TMP/media-key-empty-bind-render.json"
docker() {
  local compose_file=
  [ "${1:-}" = compose ] || return 127
  shift
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --project-directory|--file) [ "$#" -ge 2 ] || return 64; [ "$1" = --file ] && compose_file=$2; shift 2 ;;
      --format) [ "$#" -ge 2 ] || return 64; shift 2 ;;
      *) shift ;;
    esac
  done
  case "$compose_file" in
    "$TMP/media-key-explicit-false.yml"|"$TMP/media-key-default-create-host-path.yml"|"$TMP/media-key-empty-bind-default.yml"|"$TMP/media-key-interpolated-base.yml") cat "$TMP/media-key-empty-bind-render.json" ;;
    "$TMP/media-key-interpolated-override.yml")
      [ "${TG_MEDIA_KEY_TARGET:-}" = /run/teagramd/media-keys ] || return 1
      cat "$TMP/media-key-empty-bind-render.json"
      ;;
    *) return 1 ;;
  esac
}
cat > "$TMP/media-key-explicit-false.yml" <<'YAML'
services:
  teagramd:
    volumes:
      - type: bind
        source: /etc/teagramd/media-keys
        target: /run/teagramd/media-keys
        read_only: true
        bind:
          create_host_path: false
YAML
media_key_empty_bind_target=$(snapshot_from_json \
  "$media_key_inspect" "$media_key_compose_empty_bind" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
  "$TMP/media-key-explicit-false.yml")
[ "$(jq -r '.media_key_source' <<< "$media_key_empty_bind_target")" = "$media_key_source" ]
[ "$(jq -r '.config_sha256' <<< "$media_key_target")" = "$(jq -r '.config_sha256' <<< "$media_key_empty_bind_target")" ]
printf '%s\n' 'PASS source-proven bind:{} normalizes with explicit create_host_path:false'
cat > "$TMP/media-key-default-create-host-path.yml" <<'YAML'
services:
  teagramd:
    volumes:
      - type: bind
        source: /etc/teagramd/media-keys
        target: /run/teagramd/media-keys
        read_only: true
YAML
cat > "$TMP/media-key-empty-bind-default.yml" <<'YAML'
services:
  teagramd:
    volumes:
      - type: bind
        source: /etc/teagramd/media-keys
        target: /run/teagramd/media-keys
        read_only: true
        bind: {}
YAML
if snapshot_from_json \
  "$media_key_inspect" "$media_key_compose_empty_bind" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
  "$TMP/media-key-default-create-host-path.yml" >/dev/null 2>&1; then
  printf '%s\n' 'FAIL source Compose default create_host_path:true was accepted' >&2
  exit 1
fi
if snapshot_from_json \
  "$media_key_inspect" "$media_key_compose_empty_bind" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
  "$TMP/media-key-empty-bind-default.yml" >/dev/null 2>&1; then
  printf '%s\n' 'FAIL source Compose bind:{} default was treated as create_host_path:false' >&2
  exit 1
fi
printf '%s\n' 'PASS source Compose default and empty bind reject create_host_path:true'
cat > "$TMP/media-key-interpolated-base.yml" <<'YAML'
services:
  teagramd:
    volumes:
      - type: bind
        source: /etc/teagramd/media-keys
        target: /run/teagramd/media-keys
        read_only: true
        bind:
          create_host_path: false
YAML
cat > "$TMP/media-key-interpolated-override.yml" <<'YAML'
services:
  teagramd:
    volumes:
      - type: bind
        source: /etc/teagramd/media-keys
        target: ${TG_MEDIA_KEY_TARGET}
        read_only: true
        bind:
          create_host_path: true
YAML
TG_MEDIA_KEY_TARGET=/run/teagramd/media-keys
export TG_MEDIA_KEY_TARGET
if snapshot_from_json \
  "$media_key_inspect" "$media_key_compose_empty_bind" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
  "$TMP/media-key-interpolated-base.yml:$TMP/media-key-interpolated-override.yml" \
  >/dev/null 2>&1; then
  printf '%s\n' 'FAIL base media-key bind proof hid an interpolated override with create_host_path:true' >&2
  exit 1
fi
printf '%s\n' 'PASS interpolated override cannot inherit the base media-key bind proof'
unset -f docker
printf '%064s' '' | tr ' ' A > "$media_key_source/1.key"
snapshot_from_json "$media_key_inspect" "$media_key_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target >/dev/null
printf '%064d\r\n' 0 > "$media_key_source/1.key"
snapshot_from_json "$media_key_inspect" "$media_key_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target >/dev/null
printf '%s\n' 'PASS media-key accepts no newline, uppercase hex, and CRLF'
printf '%064d' 0 > "$media_key_source/1.key"
printf '%s\n' "$media_key_baseline" > "$TMP/media-key-baseline.snapshot.json"
printf '%s\n' "$media_key_target" > "$TMP/media-key-target.snapshot.json"
compare_snapshots "$TMP/media-key-baseline.snapshot.json" "$TMP/media-key-target.snapshot.json" \
  sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
  "$TMP/media-key-comparisons.tsv" >/dev/null
printf '%s\n' 'PASS read-only media-key mount addition preserves rollout equivalence'

media_key_changed_source=$(printf '%s' "$media_key_target" | jq -c --arg source "$TMP/other-media-keys" '.media_key_source=$source')
printf '%s\n' "$media_key_changed_source" > "$TMP/media-key-changed-source.snapshot.json"
if compare_snapshots "$TMP/media-key-target.snapshot.json" "$TMP/media-key-changed-source.snapshot.json" \
  sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
  "$TMP/media-key-changed-source-comparisons.tsv" >/dev/null; then
  printf '%s\n' 'FAIL media-key source drift was accepted after the initial bind addition' >&2
  exit 1
fi
printf '%s\n' 'PASS media-key source path is pinned after initial deployment'

media_key_changed_version=$(printf '%s' "$media_key_target" | jq -c '.media_key_current="2"')
printf '%s\n' "$media_key_changed_version" > "$TMP/media-key-changed-version.snapshot.json"
if compare_snapshots "$TMP/media-key-target.snapshot.json" "$TMP/media-key-changed-version.snapshot.json" \
  sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
  "$TMP/media-key-changed-version-comparisons.tsv" >/dev/null; then
  printf '%s\n' 'FAIL media-key version drift was accepted after the initial deployment' >&2
  exit 1
fi
printf '%s\n' 'PASS media-key version is pinned after initial deployment'

media_key_writable_compose=$(jq -c '
  .services.teagramd.volumes[-1].read_only=false
' <<< "$media_key_compose")
media_key_writable_inspect=$(printf '%s' "$media_key_inspect" | jq -c '
  .Mounts[-1].Mode="rw" | .Mounts[-1].RW=true
')
if snapshot_from_json "$media_key_writable_inspect" "$media_key_writable_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
  >/dev/null 2>"$TMP/media-key-writable.err"; then
  printf '%s\n' 'FAIL writable media-key mount was accepted' >&2
  exit 1
fi
printf '%s\n' 'PASS writable media-key mount is rejected'

media_key_create_host_path_compose=$(jq -c '
  .services.teagramd.volumes[-1].bind.create_host_path=true
' <<< "$media_key_compose")
if snapshot_from_json "$media_key_inspect" "$media_key_create_host_path_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
  >/dev/null 2>"$TMP/media-key-create-host-path.err"; then
  printf '%s\n' 'FAIL media-key host path creation was accepted' >&2
  exit 1
fi
printf '%s\n' 'PASS media-key host path creation is rejected'

media_key_extra_option_compose=$(jq -c '
  .services.teagramd.volumes[-1].bind.propagation="rprivate"
' <<< "$media_key_compose")
if snapshot_from_json "$media_key_inspect" "$media_key_extra_option_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
  >/dev/null 2>"$TMP/media-key-extra-option.err"; then
  printf '%s\n' 'FAIL media-key mount with extra bind options was accepted' >&2
  exit 1
fi
printf '%s\n' 'PASS media-key mount rejects unreviewed bind options'

media_key_wrong_version_compose=$(jq -c '.services.teagramd.environment.TG_MEDIA_KEY_CURRENT="2"' <<< "$media_key_compose")
media_key_wrong_version_inspect=$(printf '%s' "$media_key_inspect" | jq -c '
  .Config.Env |= map(if startswith("TG_MEDIA_KEY_CURRENT=") then "TG_MEDIA_KEY_CURRENT=2" else . end)
')
if snapshot_from_json "$media_key_wrong_version_inspect" "$media_key_wrong_version_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
  >/dev/null 2>"$TMP/media-key-version.err"; then
  printf '%s\n' 'FAIL unsupported media-key version was accepted' >&2
  exit 1
fi
printf '%s\n' 'PASS unsupported media-key version is rejected'

media_key_inside_checkout="$ROLLOUT_CHECKOUT_PATH/media-keys"
mkdir -m 700 "$media_key_inside_checkout"
printf '%064d\n' 0 > "$media_key_inside_checkout/1.key"
chmod 600 "$media_key_inside_checkout/1.key"
if [ "$(id -u)" = 0 ]; then
  chown 65532:65532 "$media_key_inside_checkout" "$media_key_inside_checkout/1.key"
fi
media_key_inside_compose=$(jq -c --arg source "$media_key_inside_checkout" \
  '.services.teagramd.volumes[-1].source=$source' <<< "$media_key_compose")
media_key_inside_inspect=$(printf '%s' "$media_key_inspect" | jq -c --arg source "$media_key_inside_checkout" \
  '.Mounts[-1].Source=$source')
if snapshot_from_json "$media_key_inside_inspect" "$media_key_inside_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
  >/dev/null 2>"$TMP/media-key-inside-checkout.err"; then
  printf '%s\n' 'FAIL media-key directory inside the application checkout was accepted' >&2
  exit 1
fi
printf '%s\n' 'PASS media-key directory must be outside the application checkout'

if [ "$(id -u)" = 0 ]; then
  chown 0:0 "$media_key_source/1.key"
  if snapshot_from_json "$media_key_inspect" "$media_key_compose" \
    "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
    >/dev/null 2>"$TMP/media-key-owner.err"; then
    chown 65532:65532 "$media_key_source/1.key"
    printf '%s\n' 'FAIL media-key file with an unexpected owner was accepted' >&2
    exit 1
  fi
  chown 65532:65532 "$media_key_source/1.key"
  printf '%s\n' 'PASS media-key file owner is validated when rollout runs as root'
fi

media_key_wrong_path=$(printf '%s' "$media_key_inspect" | jq -c '
  .Mounts[-1].Source="/tmp/other-media-keys"
')
if snapshot_from_json "$media_key_wrong_path" "$media_key_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render target \
  >/dev/null 2>"$TMP/media-key-source-mismatch.err"; then
  printf '%s\n' 'FAIL media-key source mismatch was accepted' >&2
  exit 1
fi
printf '%s\n' 'PASS media-key container mount must match the resolved Compose source'

media_key_preflight=$(snapshot_from_json \
  "$media_key_inspect" "$media_key_compose" \
  "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render preflight)
media_key_initial_local_drift=$(snapshot_from_json \
  "$legacy_inspect" "$COMPOSE_JSON" "$ENV_FILE" "$OVERRIDE_FILE" selected-compose-render rollback)
[ "$(jq -r '.config_sha256' <<< "$media_key_preflight")" = \
  "$(jq -r '.config_sha256' <<< "$media_key_initial_local_drift")" ]
verifier_test_env_file=$ENV_FILE
verifier_test_override_file=$OVERRIDE_FILE
ROLLOUT_RUNNER_SOURCE_ONLY=1
source "$SCRIPT_DIR/rollout-runner.sh"
ENV_FILE=$verifier_test_env_file
OVERRIDE_FILE=$verifier_test_override_file
write_immutable() { : > "$1"; }
append_evidence_row() { printf '%s\t%s\t%s\t%s\t%s\n' "$2" "$3" "$4" "$5" "$6" >> "$1"; }
fail() { printf '%s\n' "$1" >&2; return 1; }
copy_immutable() { cp -- "$1" "$2" && chmod 600 -- "$2"; }
media_key_initial_local_baseline=$(jq -c '
  .provenance.compose_source="live-container-inspection"
  | .provenance.stop_grace_period_source="docker-inspect-config-stop-timeout"
  | .compose_replica_count="not-captured"
  | .compose_client_addr_trust="not-captured"
' <<< "$media_key_baseline")
INITIALIZE_LOCAL=1
BASELINE_DIR="$TMP/media-key-initial-local.baseline"
ROLLBACK_DIR="$TMP/media-key-initial-local.rollback"
mkdir -m 700 "$BASELINE_DIR" "$ROLLBACK_DIR"
printf '%s\n' "$media_key_initial_local_baseline" > "$BASELINE_DIR/baseline.snapshot.json"
printf '%s\n' "$media_key_target" > "$BASELINE_DIR/initial-local-target-render.snapshot.json"
printf '%s\n' "$media_key_target" > "$ROLLBACK_DIR/rollback.snapshot.json"
if ! compare_rollback_to_baseline; then
  printf '%s\n' 'FAIL initial-local rollback rejected the pinned media-key configuration' >&2
  cat "$ROLLBACK_DIR/rollback-equivalence.tsv" >&2
  exit 1
fi
printf '%s\n' "$media_key_initial_local_drift" > "$ROLLBACK_DIR/rollback.snapshot.json"
if compare_rollback_to_baseline >/dev/null 2>&1; then
  printf '%s\n' 'FAIL initial-local rollback accepted media-key drift with normalized digests' >&2
  exit 1
fi
if ! grep -q $'rollback\tselected_render_media_key_source\t.*\tfail' \
    "$ROLLBACK_DIR/rollback-equivalence.tsv"; then
  printf '%s\n' 'FAIL initial-local rollback did not record media-key render mismatch' >&2
  cat "$ROLLBACK_DIR/rollback-equivalence.tsv" >&2
  exit 1
fi
printf '%s\n' 'PASS initial-local rollback compares the pinned media-key render'

blob_dir_initial_compose=$(jq -c \
  '.services.teagramd.environment.TG_BLOB_DIR="/var/lib/telegramd-blobs"' \
  <<< "$syslog_compose")
blob_dir_initial_inspect=$(printf '%s' "$syslog_inspect" | jq -c '
  .Config.Env |= (map(select(startswith("TG_BLOB_DIR=") | not)) +
    ["TG_BLOB_DIR=/var/lib/telegramd-blobs"])
')
blob_dir_initial_target_inspect=$(printf '%s' "$blob_dir_initial_inspect" | jq -c \
  --arg image_id sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc \
  --arg container_id dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd \
  '.Image=$image_id | .Id=$container_id')
blob_dir_baseline_image_env='["TG_BLOB_DIR=/var/lib/telegramd-blobs"]'
blob_dir_target_image_env='["TG_BLOB_DIR=/image-default-blobs"]'
blob_dir_initial_live_baseline=$(snapshot_from_json \
  "$blob_dir_initial_inspect" '{}' "$ENV_FILE" "$OVERRIDE_FILE" \
  live-container-inspection baseline '' "$blob_dir_baseline_image_env")
blob_dir_initial_render=$(snapshot_from_json \
  "$blob_dir_initial_inspect" "$blob_dir_initial_compose" "$ENV_FILE" "$OVERRIDE_FILE" \
  selected-compose-render initial-local-target-render '' "$blob_dir_baseline_image_env")
blob_dir_initial_preflight=$(snapshot_from_json \
  "$blob_dir_initial_inspect" "$blob_dir_initial_compose" "$ENV_FILE" "$OVERRIDE_FILE" \
  selected-compose-render preflight '' "$blob_dir_baseline_image_env")
blob_dir_initial_target=$(snapshot_from_json \
  "$blob_dir_initial_target_inspect" "$blob_dir_initial_compose" "$ENV_FILE" "$OVERRIDE_FILE" \
  selected-compose-render target '' "$blob_dir_target_image_env")
blob_dir_initial_rollback=$(snapshot_from_json \
  "$blob_dir_initial_inspect" "$blob_dir_initial_compose" "$ENV_FILE" "$OVERRIDE_FILE" \
  selected-compose-render rollback '' "$blob_dir_baseline_image_env")
blob_dir_initial_baseline_sha=$(jq -r '.container_env_sha256' <<< "$blob_dir_initial_live_baseline")
blob_dir_initial_render_sha=$(jq -r '.container_env_sha256' <<< "$blob_dir_initial_render")
blob_dir_initial_target_sha=$(jq -r '.container_env_sha256' <<< "$blob_dir_initial_target")
if [ "$blob_dir_initial_baseline_sha" = "$blob_dir_initial_render_sha" ] || \
   [ "$blob_dir_initial_render_sha" != "$blob_dir_initial_target_sha" ]; then
  printf '%s\n' 'FAIL mixed live/selected Compose snapshots did not expose matching explicit TG_BLOB_DIR values' >&2
  exit 1
fi

INITIALIZE_LOCAL=1
BUILT_IMAGE_ID=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
BASELINE_DIR="$TMP/blob-dir-initial-local.baseline"
TARGET_DIR="$TMP/blob-dir-initial-local.target"
ROLLBACK_DIR="$TMP/blob-dir-initial-local.rollback"
mkdir -m 700 "$BASELINE_DIR" "$TARGET_DIR" "$ROLLBACK_DIR"
printf '%s\n' "$blob_dir_initial_live_baseline" > "$BASELINE_DIR/baseline.snapshot.json"
printf '%s\n' "$blob_dir_initial_render" > "$BASELINE_DIR/initial-local-target-render.snapshot.json"
printf '%s\n' "$blob_dir_initial_preflight" > "$TARGET_DIR/preflight.snapshot.json"
printf '%s\n' "$blob_dir_initial_target" > "$TARGET_DIR/target.snapshot.json"
printf '%s\n' "$blob_dir_initial_rollback" > "$ROLLBACK_DIR/rollback.snapshot.json"
chmod 600 "$BASELINE_DIR"/*.json "$TARGET_DIR"/*.json "$ROLLBACK_DIR"/*.json
if ! compare_resolved_preflight; then
  printf '%s\n' 'FAIL initial-local preflight rejected matching live baseline and selected Compose environment' >&2
  cat "$TARGET_DIR/preflight-comparisons.tsv" >&2
  exit 1
fi
if ! compare_initial_local_target; then
  printf '%s\n' 'FAIL initial-local target rejected a TG_BLOB_DIR value pinned by Compose across image defaults' >&2
  cat "$TARGET_DIR/target-comparisons.tsv" >&2
  exit 1
fi
if ! compare_rollback_to_baseline; then
  printf '%s\n' 'FAIL initial-local rollback rejected a TG_BLOB_DIR value pinned by Compose across image defaults' >&2
  cat "$ROLLBACK_DIR/rollback-equivalence.tsv" >&2
  exit 1
fi
printf '%s\n' 'PASS initial-local preflight, target, and rollback normalize live and selected TG_BLOB_DIR environment'
live_preflight_fixture="$SCRIPT_DIR/testdata/key-exchange-preflight/live-b5d8471-to-35dd1457.json"
if ! jq -e '
  .source.live_revision == "b5d84718a4a2c0c059acb07d3875e3b212fa06d4" and
  .source.candidate_head == "35dd14574b0fc94bde1048120629c48cfb97c639" and
  .source.tailnet_env == "absent" and
  .source.container_env_added == "none" and
  .source.comparison_rows == 12 and
  .baseline.provenance.compose_source == "selected-compose-render" and
  .preflight.provenance.compose_source == "selected-compose-render"
' "$live_preflight_fixture" >/dev/null; then
  printf '%s\n' 'FAIL live key-exchange preflight fixture metadata is invalid' >&2
  exit 1
fi
INITIALIZE_LOCAL=0
BASELINE_DIR="$TMP/live-preflight.baseline"
TARGET_DIR="$TMP/live-preflight.target"
mkdir -m 700 "$BASELINE_DIR" "$TARGET_DIR"
jq -c '.baseline' "$live_preflight_fixture" > "$BASELINE_DIR/baseline.snapshot.json"
jq -c '.preflight' "$live_preflight_fixture" > "$TARGET_DIR/preflight.snapshot.json"
if ! compare_resolved_preflight; then
  printf '%s\n' 'FAIL live baseline-to-target preflight fixture was rejected' >&2
  cat "$TARGET_DIR/preflight-comparisons.tsv" >&2
  exit 1
fi
live_preflight_rows=$(grep -vc '^$' "$TARGET_DIR/preflight-comparisons.tsv")
[ "$live_preflight_rows" -eq 12 ] || {
  printf 'FAIL live baseline-to-target comparison recorded %s rows instead of 12\n' "$live_preflight_rows" >&2
  cat "$TARGET_DIR/preflight-comparisons.tsv" >&2
  exit 1
}
if awk -F '\t' 'NF > 0 && $5 != "pass" { found=1 } END { exit !found }' \
    "$TARGET_DIR/preflight-comparisons.tsv"; then
  printf '%s\n' 'FAIL live baseline-to-target comparison contains a failing row' >&2
  cat "$TARGET_DIR/preflight-comparisons.tsv" >&2
  exit 1
fi
printf '%s\n' 'PASS live b5d8471-to-35dd1457 preflight fixture passes all 12 comparisons'
