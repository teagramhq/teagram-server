#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
TMP=$(mktemp -d /tmp/teagram-media-ops-test.XXXXXXXX)
FIXTURE_TMP=$TMP
export FIXTURE_TMP
cleanup() {
  local status=$?
  trap - EXIT
  case "$TMP" in
    /tmp/teagram-media-ops-test.????????)
      [[ -d "$TMP" && ! -L "$TMP" ]] && /bin/rm -rf -- "$TMP" || status=1
      ;;
    *) printf '%s\n' 'refusing to remove an unexpected fixture path' >&2; status=1 ;;
  esac
  exit "$status"
}
trap cleanup EXIT
report_failure() {
  local status=$? line=$1 command=$2
  printf 'FAIL media ops fixture line %s: %s (exit %s)\n' "$line" "$command" "$status" >&2
  [[ -z "${output:-}" ]] || printf 'last operation output: %s\n' "$output" >&2
  exit "$status"
}
trap 'report_failure "$LINENO" "$BASH_COMMAND"' ERR

mkdir -p "$TMP/bin"
FIXTURE_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
FIXTURE_REVISION=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
FIXTURE_IMAGE=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
export FIXTURE_SHA FIXTURE_REVISION FIXTURE_IMAGE

cat > "$TMP/bin/id" <<'ID'
#!/usr/bin/env bash
[[ "$#" == 1 && "$1" == -u ]] || exit 99
printf '0\n'
ID

cat > "$TMP/bin/stat" <<'STAT'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$#" == 3 && "$1" == -c ]]; then
  format=$2
  path=$3
elif [[ "$#" == 4 && "$1" == -c && "$3" == -- ]]; then
  format=$2
  path=$4
else
  exec /usr/bin/stat "$@"
fi
if [[ "$format" == '%u:%a:%g' && "$path" == "$FIXTURE_KEY_DIR" ]]; then
  printf '65532:700:65532\n'
  exit 0
fi
if [[ "$format" == '%u:%a:%g' && "$path" == "$FIXTURE_KEY_DIR/1.key" ]]; then
  printf '65532:600:65532\n'
  exit 0
fi
if [[ "$format" == '%u:%a' && "$path" == /tmp/teagram-media-op.* ]]; then
  printf '0:700\n'
  exit 0
fi
case "$format" in
  %u) printf '0\n'; exit 0 ;;
  %u:%a) printf '0:%s\n' "$(/usr/bin/stat -c %a -- "$path")"; exit 0 ;;
esac
exec /usr/bin/stat "$@"
STAT

cat > "$TMP/bin/git" <<'GIT'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == -C && "$2" == "$FIXTURE_REPO" ]] || { echo "unexpected fixture git call: $*" >&2; exit 99; }
shift 2
case "$1" in
  branch) [[ "$2" == --show-current ]] && printf 'main\n' ;;
  rev-parse)
    case "$2" in
      --verify) printf '%s\n' "$FIXTURE_REVISION" ;;
      HEAD) printf '%s\n' "$FIXTURE_REVISION" ;;
      origin/main) printf '%s\n' "$FIXTURE_REVISION" ;;
      *) exit 99 ;;
    esac
    ;;
  fetch) exit 0 ;;
  merge-base) exit 0 ;;
  show)
    case "$2" in
      "$FIXTURE_SHA:deploy/teagram/ops/blob-encryption-common.inc")
        sed -e "s|/opt/teagram|$FIXTURE_REPO|g" \
          -e "s|/root|$FIXTURE_EVIDENCE|g" \
          -e "s|/etc/teagramd/media-keys|$FIXTURE_KEY_DIR|g" \
          -e "s|/tmp/teagram-deploy.lock|$FIXTURE_TMP/deploy.lock|g" "$FIXTURE_COMMON"
        ;;
      "$FIXTURE_SHA:deploy/teagramd/rollout-runner/blob-transition-runner.py"|\
      "$FIXTURE_SHA:deploy/teagramd/rollout-runner/blob-mode-state.py")
        printf '# fixture helper\n'
        ;;
      *) echo "unexpected fixture git show: $2" >&2; exit 99 ;;
    esac
    ;;
  *) echo "unexpected fixture git command: $*" >&2; exit 99 ;;
esac
GIT

cat > "$TMP/bin/python3" <<'PYTHON'
#!/usr/bin/env bash
set -euo pipefail
case "${1##*/}" in
  blob-transition-runner.py)
    [[ "$2" == deployment-env ]] || exit 99
    jq -cn --arg repo "$FIXTURE_REPO" \
      '{outcome:"s3-accepted",env_files:[$repo + "/.env",$repo + "/.env.rustfs-parked"]}'
    ;;
  blob-mode-state.py)
    [[ "$2" == authority ]] || exit 99
    jq -cn --arg outcome "$FIXTURE_AUTHORITY" '{outcome:$outcome}'
    ;;
  *) echo "unexpected fixture Python helper: $1" >&2; exit 99 ;;
esac
PYTHON

cat > "$TMP/bin/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  ps)
    if [[ "$*" == *"{{.Names}}|{{.Label"* ]]; then
      printf 'teagram-teagramd-1|teagramd\n'
    else
      printf 'teagram-teagramd-1\n'
    fi
    ;;
  inspect)
    if [[ "$*" == *'--type container'* ]]; then exit 1; fi
    format=$3
    container=$4
    case "$container" in
      teagram-postgres-1) service=postgres; state_file=$FIXTURE_POSTGRES_STATE ;;
      teagram-teagramd-1) service=teagramd; state_file=$FIXTURE_SERVER_STATE ;;
      teagram-rustfs-1) service=rustfs; state_file=$FIXTURE_RUSTFS_STATE ;;
      *) echo "unexpected fixture container inspect: $container" >&2; exit 99 ;;
    esac
    state=$(cat "$state_file")
    case "$format" in
      *'{{.State.Status}}|{{index .Config.Labels'* )
        printf '%s|teagram|%s|%s|%s|%s|healthy\n' \
          "$state" "$service" "$FIXTURE_COMPOSE_FILES" "$FIXTURE_IMAGE" "$FIXTURE_REVISION"
        ;;
      '{{.State.Status}}') printf '%s\n' "$state" ;;
      '{{.State.Health.Status}}') printf 'healthy\n' ;;
      *) echo "unexpected fixture inspect format: $format" >&2; exit 99 ;;
    esac
    ;;
  stop)
    [[ "$2" == --time && "$4" == teagram-teagramd-1 ]] || { echo "unexpected stop: $*" >&2; exit 99; }
    printf 'stop:server\n' >> "$FIXTURE_EVENTS"
    printf 'exited\n' > "$FIXTURE_SERVER_STATE"
    ;;
  start)
    case "$2" in
      teagram-teagramd-1) printf 'start:server\n' >> "$FIXTURE_EVENTS"; printf 'running\n' > "$FIXTURE_SERVER_STATE" ;;
      teagram-rustfs-1) printf 'start:rustfs\n' >> "$FIXTURE_EVENTS"; printf 'running\n' > "$FIXTURE_RUSTFS_STATE" ;;
      *) echo "unexpected fixture start: $*" >&2; exit 99 ;;
    esac
    ;;
  restart)
    [[ "$2" == --time && "$4" == teagram-rustfs-1 ]] || { echo "unexpected restart: $*" >&2; exit 99; }
    printf 'restart:rustfs\n' >> "$FIXTURE_EVENTS"
    printf 'exited\n' > "$FIXTURE_RUSTFS_STATE"
    [[ "$FIXTURE_SCENARIO" != restart-recovery ]] || exit 42
    printf 'running\n' > "$FIXTURE_RUSTFS_STATE"
    ;;
  exec)
    [[ "$2" == -i && "$3" == teagram-postgres-1 && "$4" == pg_dump ]] || {
      echo "unexpected fixture exec: $*" >&2
      exit 99
    }
    printf 'pg_dump\n' >> "$FIXTURE_EVENTS"
    [[ "$FIXTURE_SCENARIO" != backup-failure ]] || { echo 'fixture pg_dump failure' >&2; exit 42; }
    printf 'fixture archive\n'
    ;;
  compose)
    if [[ "$*" == *' config --format json' ]]; then
      jq -cn --arg key "$FIXTURE_KEY_DIR" --arg evidence "$FIXTURE_EVIDENCE" \
        --arg state "$FIXTURE_REPO/.state/blob-mode" '
        {services:{
          "blob-encrypt-migrate":{
            image:"teagramd:local",entrypoint:["/usr/local/bin/blob-encrypt-migrate"],
            user:"0:0",read_only:true,profiles:["migration"],
            volumes:[
              {source:$key,target:"/run/teagramd/media-keys",read_only:true},
              {source:$evidence,target:"/run/teagramd/reports"},
              {source:$state,target:"/run/teagramd/blob-mode"}
            ],networks:{default:{},rustfs_private:{}}},
          teagramd:{image:"teagramd:local",
            environment:{TG_MEDIA_KEY_CURRENT:"1",TG_MEDIA_KEY_DIR:"/run/teagramd/media-keys"},
            volumes:[{source:$key,target:"/run/teagramd/media-keys",read_only:true}]}
        }}'
      exit
    fi
    if [[ "$*" == *' blob-encrypt-migrate '* ]]; then
      printf 'migration\n' >> "$FIXTURE_EVENTS"
      if [[ "$FIXTURE_SCENARIO" == interrupted-migration ]]; then
        echo 'fixture interrupted migration' >&2
        exit 42
      fi
      if [[ "$FIXTURE_SCENARIO" == cleanup-gating ]]; then
        printf '{"mode":"encrypt","authority":"encrypted","writes":"stopped","objects":1,"bytes":1,"unaccounted":[{"kind":"file","id":"999","key":"files/999"}]}\n'
      else
        printf '{"mode":"encrypt","authority":"encrypted","writes":"stopped","objects":1,"bytes":1}\n'
      fi
      exit
    fi
    echo "unexpected fixture Compose command: $*" >&2
    exit 99
    ;;
  *) echo "unexpected fixture Docker command: $*" >&2; exit 99 ;;
esac
DOCKER

cat > "$TMP/bin/sleep" <<'SLEEP'
#!/usr/bin/env bash
[[ "$#" == 1 ]] || exit 99
SLEEP
chmod 755 "$TMP/bin/"*

FIXTURE_COMMON=$SCRIPT_DIR/ops/blob-encryption-common.inc
export FIXTURE_COMMON

setup_case() {
  local scenario=$1 authority=$2 case_dir
  case_dir=$TMP/$scenario
  FIXTURE_REPO=$case_dir/repo
  FIXTURE_EVIDENCE=$case_dir/root
  FIXTURE_OPERATION_EVIDENCE=$FIXTURE_EVIDENCE/teagram-media-encryption
  FIXTURE_KEY_DIR=$case_dir/keys
  FIXTURE_POSTGRES_STATE=$case_dir/postgres-state
  FIXTURE_SERVER_STATE=$case_dir/server-state
  FIXTURE_RUSTFS_STATE=$case_dir/rustfs-state
  FIXTURE_EVENTS=$case_dir/events
  FIXTURE_SCENARIO=$scenario
  FIXTURE_AUTHORITY=$authority
  FIXTURE_COMPOSE_FILES=$FIXTURE_REPO/docker-compose.yml,$FIXTURE_REPO/docker-compose.override.yml
  export FIXTURE_REPO FIXTURE_EVIDENCE FIXTURE_OPERATION_EVIDENCE FIXTURE_KEY_DIR FIXTURE_POSTGRES_STATE \
    FIXTURE_SERVER_STATE FIXTURE_RUSTFS_STATE FIXTURE_EVENTS FIXTURE_SCENARIO \
    FIXTURE_AUTHORITY FIXTURE_COMPOSE_FILES

  mkdir -p "$FIXTURE_REPO/.state/blob-mode" "$FIXTURE_EVIDENCE" "$FIXTURE_KEY_DIR"
  mkdir -m 700 "$FIXTURE_OPERATION_EVIDENCE"
  chmod 700 "$FIXTURE_EVIDENCE" "$FIXTURE_KEY_DIR"
  printf 'fixture env\n' > "$FIXTURE_REPO/.env"
  printf 'fixture parked env\n' > "$FIXTURE_REPO/.env.rustfs-parked"
  printf 'fixture compose\n' > "$FIXTURE_REPO/docker-compose.yml"
  printf 'fixture override\n' > "$FIXTURE_REPO/docker-compose.override.yml"
  chmod 600 "$FIXTURE_REPO/.env" "$FIXTURE_REPO/.env.rustfs-parked"
  printf '%064d\n' 0 > "$FIXTURE_KEY_DIR/1.key"
  cp "$FIXTURE_KEY_DIR/1.key" "$FIXTURE_EVIDENCE/teagram-media-key-v1-20261024T192020Z.key"
  chmod 600 "$FIXTURE_KEY_DIR/1.key" "$FIXTURE_EVIDENCE/teagram-media-key-v1-20261024T192020Z.key"
  printf 'running\n' > "$FIXTURE_POSTGRES_STATE"
  printf 'running\n' > "$FIXTURE_SERVER_STATE"
  printf 'running\n' > "$FIXTURE_RUSTFS_STATE"
  : > "$FIXTURE_EVENTS"

  sed -e "s|/opt/teagram|$FIXTURE_REPO|g" \
    -e "s|/root|$FIXTURE_EVIDENCE|g" \
    -e "s|/etc/teagramd/media-keys|$FIXTURE_KEY_DIR|g" \
    -e "s|/tmp/teagram-deploy.lock|$case_dir/deploy.lock|g" \
    "$SCRIPT_DIR/ops/encrypt-media.sh" > "$case_dir/encrypt-media.sh"
  sed -e "s|/opt/teagram|$FIXTURE_REPO|g" \
    -e "s|/root|$FIXTURE_EVIDENCE|g" \
    -e "s|/etc/teagramd/media-keys|$FIXTURE_KEY_DIR|g" \
    -e "s|/tmp/teagram-deploy.lock|$case_dir/deploy.lock|g" \
    "$SCRIPT_DIR/ops/cleanup-media.sh" > "$case_dir/cleanup-media.sh"
  chmod 755 "$case_dir/encrypt-media.sh" "$case_dir/cleanup-media.sh"

  if [[ "$authority" == encrypted ]]; then
    setup_encrypted_authority
  fi
}

setup_encrypted_authority() {
  local transition_id=11111111-1111-4111-8111-111111111111 inventory dump_sha inventory_sha
  inventory="$FIXTURE_EVIDENCE/teagramd-blob-encryption-inventory-$transition_id.json"
  jq -cn '{schema:"teagram.blob-encryption-inventory/v1",
    encrypted:{classes:{valid_envelope:{count:1},no_valid_envelope:{count:0},missing:{count:0},unaccounted:{count:0,ids:[]}}},
    plaintext:{classes:{no_valid_envelope:{count:1},missing:{count:0},unaccounted:{count:0,ids:[]}}}}' > "$inventory"
  chmod 600 "$inventory"
  dump="$FIXTURE_OPERATION_EVIDENCE/pgdump-pre-migration-fixture.dump"
  printf 'fixture database archive\n' > "$dump"
  chmod 600 "$dump"
  dump_sha=$(sha256sum "$dump")
  dump_sha=${dump_sha%% *}
  jq -cn --arg dump "$dump" --arg sha "$dump_sha" \
    '{result:"isolated-restore-pass",dump_path:$dump,dump_sha256:$sha}' \
    > "$FIXTURE_OPERATION_EVIDENCE/pgdump-pre-migration-fixture.manifest"
  chmod 600 "$FIXTURE_OPERATION_EVIDENCE/pgdump-pre-migration-fixture.manifest"
  printf '{"type":"header","schema":"teagram.blob-encryption-progress/v1","writes":"stopped"}\n' \
    > "$FIXTURE_EVIDENCE/blob-encryption-progress.jsonl"
  chmod 600 "$FIXTURE_EVIDENCE/blob-encryption-progress.jsonl"
  jq -cn --arg sha "$dump_sha" \
    '{transition_id:"11111111-1111-4111-8111-111111111111",evidence:{encrypted_inventory_sha256:$sha,object_count:1}}' \
    > "$FIXTURE_REPO/.state/blob-mode/mode.json"
  # Replace the inventory digest with the digest of the inventory report.
  inventory_sha=$(sha256sum "$inventory")
  inventory_sha=${inventory_sha%% *}
  jq --arg sha "$inventory_sha" '.evidence.encrypted_inventory_sha256=$sha' \
    "$FIXTURE_REPO/.state/blob-mode/mode.json" > "$TMP/mode.json"
  mv "$TMP/mode.json" "$FIXTURE_REPO/.state/blob-mode/mode.json"
  chmod 600 "$FIXTURE_REPO/.state/blob-mode/mode.json"
}

run_encrypt() {
  local status=0 output
  output=$(env OPS_SHA="$FIXTURE_SHA" PATH="$TMP/bin:$PATH" \
    bash "$TMP/$(basename "$(dirname "$FIXTURE_REPO")")/encrypt-media.sh" 2>&1) || status=$?
  printf '%s\n' "$status" > "$TMP/operation-status"
  printf '%s\n' "$output"
}

run_cleanup() {
  local status=0 output
  output=$(env OPS_SHA="$FIXTURE_SHA" PATH="$TMP/bin:$PATH" \
    bash "$TMP/$(basename "$(dirname "$FIXTURE_REPO")")/cleanup-media.sh" "$@" 2>&1) || status=$?
  printf '%s\n' "$status" > "$TMP/operation-status"
  printf '%s\n' "$output"
}

setup_case backup-failure s3-accepted
output=$(run_encrypt)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'pg_dump failed'* ]]
[[ "$(cat "$FIXTURE_SERVER_STATE")" == running ]]
[[ "$(cat "$FIXTURE_EVENTS")" == $'stop:server\npg_dump\nstart:server' ]]
printf '%s\n' 'PASS failed pre-migration backup restores teagramd without starting migration'

setup_case interrupted-migration encrypted
output=$(run_encrypt)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'blob-encrypt-migrate encrypt failed'* ]]
[[ "$(cat "$FIXTURE_SERVER_STATE")" == running ]]
[[ "$(cat "$FIXTURE_EVENTS")" == $'stop:server\nmigration\nstart:server' ]]
printf '%s\n' 'PASS interrupted encrypted migration restores the server under encrypted authority'

setup_case cleanup-gating encrypted
output=$(run_cleanup)
[[ "$(cat "$TMP/operation-status")" == 64 ]]
[[ "$output" == *'usage: cleanup-media.sh --acceptance-passed'* ]]
[[ ! -s "$FIXTURE_EVENTS" ]]
output=$(run_cleanup --acceptance-passed)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'unaccounted objects'* ]]
[[ "$(cat "$FIXTURE_SERVER_STATE")" == running ]]
[[ "$(cat "$FIXTURE_EVENTS")" == $'stop:server\nmigration\nstart:server' ]]
printf '%s\n' 'PASS cleanup requires acceptance and refuses unaccounted objects before deleting plaintext'

setup_case restart-recovery encrypted
output=$(run_encrypt)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'RustFS restart failed'* ]]
[[ "$(cat "$FIXTURE_RUSTFS_STATE")" == running ]]
[[ "$(cat "$FIXTURE_SERVER_STATE")" == running ]]
[[ "$(cat "$FIXTURE_EVENTS")" == $'stop:server\nmigration\nrestart:rustfs\nstart:rustfs\nstart:server' ]]
printf '%s\n' 'PASS restart failure recovers RustFS and teagramd before returning'
