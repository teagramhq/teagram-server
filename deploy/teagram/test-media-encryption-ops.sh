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
  if [[ -n "${FIXTURE_EVENTS:-}" && -f "$FIXTURE_EVENTS" ]]; then
    printf 'fixture events:\n' >&2
    cat "$FIXTURE_EVENTS" >&2 || true
  fi
  exit "$status"
}
trap 'report_failure "$LINENO" "$BASH_COMMAND"' ERR

mkdir -p "$TMP/bin"
FIXTURE_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
FIXTURE_REVISION=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
FIXTURE_ORIGIN_REVISION=eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
FIXTURE_IMAGE=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
FIXTURE_RETAGGED_IMAGE=sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
export FIXTURE_SHA FIXTURE_REVISION FIXTURE_ORIGIN_REVISION FIXTURE_IMAGE FIXTURE_RETAGGED_IMAGE

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
    case "$2:$3" in
      --verify:'HEAD^{commit}') printf '%s\n' "$FIXTURE_REVISION" ;;
      --verify:'origin/main^{commit}') printf '%s\n' "$FIXTURE_ORIGIN_REVISION" ;;
      --verify:*) exit 99 ;;
      HEAD) printf '%s\n' "$FIXTURE_REVISION" ;;
      origin/main) printf '%s\n' "$FIXTURE_ORIGIN_REVISION" ;;
      *) exit 99 ;;
    esac
    ;;
  fetch) exit 0 ;;
  merge-base)
    if [[ "$FIXTURE_SCENARIO" == ancestry-origin-failure && \
      "$3" == "$FIXTURE_SHA" && "$4" == "$FIXTURE_ORIGIN_REVISION" ]]; then
      exit 1
    fi
    if [[ "$FIXTURE_SCENARIO" == ancestry-live-failure && \
      "$3" == "$FIXTURE_SHA" && "$4" == "$FIXTURE_REVISION" ]]; then
      exit 1
    fi
    if [[ "$FIXTURE_SCENARIO" == ancestry-minimum-failure && \
      "$3" == 91e50a0 && "$4" == "$FIXTURE_REVISION" ]]; then
      exit 1
    fi
    exit 0
    ;;
  show)
    case "$2" in
      "$FIXTURE_SHA:deploy/teagram/ops/blob-encryption-common.inc")
        sed -e "s|/opt/teagram|$FIXTURE_REPO|g" \
          -e "s|/root|$FIXTURE_EVIDENCE|g" \
          -e "s|/etc/teagramd/media-keys|$FIXTURE_KEY_DIR|g" \
          -e "s|/etc/teagramd/media-key-backups|$FIXTURE_EVIDENCE/media-key-backups|g" \
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
    jq -cn --arg outcome "$(cat "$FIXTURE_AUTHORITY_FILE")" '{outcome:$outcome}'
    ;;
  *) echo "unexpected fixture Python helper: $1" >&2; exit 99 ;;
esac
PYTHON

cat > "$TMP/bin/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail
publish_encrypted_authority() {
  local transition_id=11111111-1111-4111-8111-111111111111
  local inventory="$FIXTURE_EVIDENCE/teagramd-blob-encryption-inventory-$transition_id.json"
  jq -cn '{schema:"teagram.blob-encryption-inventory/v1",
    encrypted:{classes:{valid_envelope:{count:1},no_valid_envelope:{count:0},missing:{count:0},unaccounted:{count:0,ids:[]}}},
    plaintext:{classes:{no_valid_envelope:{count:1},missing:{count:0},unaccounted:{count:0,ids:[]}}}}' > "$inventory"
  chmod 600 "$inventory"
  local inventory_sha
  inventory_sha=$(sha256sum "$inventory")
  inventory_sha=${inventory_sha%% *}
  jq -cn --arg sha "$inventory_sha" \
    --arg id "$transition_id" \
    '{transition_id:$id,evidence:{encrypted_inventory_sha256:$sha,object_count:1}}' \
    > "$FIXTURE_REPO/.state/blob-mode/mode.json"
  chmod 644 "$FIXTURE_REPO/.state/blob-mode/mode.json"
  printf 'encrypted\n' > "$FIXTURE_AUTHORITY_FILE"
  if [[ ! -e "$FIXTURE_EVIDENCE/blob-encryption-progress.jsonl" ]]; then
    printf '{"type":"header","schema":"teagram.blob-encryption-progress/v1","writes":"stopped"}\n' \
      > "$FIXTURE_EVIDENCE/blob-encryption-progress.jsonl"
    chmod 600 "$FIXTURE_EVIDENCE/blob-encryption-progress.jsonl"
  fi
}
case "$1" in
  image)
    [[ "$2" == inspect && "$3" == --format && "$4" == '{{.Id}}' && "$5" == teagramd:local ]] || {
      echo "unexpected fixture image command: $*" >&2
      exit 99
    }
    printf 'image-check\n' >> "$FIXTURE_EVENTS"
    if [[ "$FIXTURE_SCENARIO" == retagged-image ]]; then
      printf '%s\n' "$FIXTURE_RETAGGED_IMAGE"
    else
      printf '%s\n' "$FIXTURE_IMAGE"
    fi
    ;;
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
    if [[ "$format" == *'com.teagram.ops.operation'* ]]; then
      [[ "$container" == teagram-media-restore-* ]] || exit 99
      printf 'media-backup\n'
      exit
    fi
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
  run)
    if [[ "$*" == *'--label com.teagram.ops.operation=media-backup'* ]]; then
      restore_container=$5
      printf '%s\n' "$restore_container" > "$FIXTURE_RESTORE_CONTAINER"
      printf 'restore:start\n' >> "$FIXTURE_EVENTS"
      printf 'fixture-restore-container\n'
    elif [[ "$*" == *'/usr/local/bin/mtproto-smoke'* ]]; then
      printf 'smoke\n' >> "$FIXTURE_EVENTS"
      printf 'fixture smoke passed\n'
    else
      echo "unexpected fixture Docker run: $*" >&2
      exit 99
    fi
    ;;
  stop)
    [[ "$2" == --time ]] || { echo "unexpected stop: $*" >&2; exit 99; }
    case "$4" in
      teagram-teagramd-1)
        printf 'stop:server\n' >> "$FIXTURE_EVENTS"
        printf 'exited\n' > "$FIXTURE_SERVER_STATE"
        ;;
      teagram-media-restore-*)
        printf 'restore:stop\n' >> "$FIXTURE_EVENTS"
        ;;
      *) echo "unexpected fixture Docker stop: $*" >&2; exit 99 ;;
    esac
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
    if [[ "$2" == -i && "$3" == teagram-postgres-1 && "$4" == pg_dump ]]; then
      printf 'pg_dump\n' >> "$FIXTURE_EVENTS"
      [[ "$FIXTURE_SCENARIO" != backup-failure ]] || { echo 'fixture pg_dump failure' >&2; exit 42; }
      printf 'fixture archive\n'
    elif [[ "$2" == -i && "$3" == teagram-media-restore-* && "$4" == pg_restore ]]; then
      if [[ "$*" == *' --list '* ]]; then
        printf 'restore:pg_restore-list\n' >> "$FIXTURE_EVENTS"
        printf 'TABLE public fixture\n'
      else
        printf 'restore:pg_restore\n' >> "$FIXTURE_EVENTS"
      fi
    elif [[ "$2" == teagram-media-restore-* && "$3" == pg_isready ]]; then
      exit 0
    elif [[ "$2" == teagram-media-restore-* && "$3" == psql ]]; then
      printf 'restore:psql\n' >> "$FIXTURE_EVENTS"
      printf '1\n'
    else
      echo "unexpected fixture exec: $*" >&2
      exit 99
    fi
    ;;
  compose)
    if [[ "$*" == *' config --format json' ]]; then
      if [[ "$FIXTURE_SCENARIO" == compose-render-failure ]]; then
        echo 'fixture Compose render failure' >&2
        exit 42
      fi
      # Compose omits services whose profiles are inactive from config output.
      if [[ " $* " != *' --profile migration config --format json'* ]]; then
        jq -cn --arg key "$FIXTURE_KEY_DIR" \
          '{services:{teagramd:{image:"teagramd:local",
            environment:{TG_MEDIA_KEY_CURRENT:"1",TG_MEDIA_KEY_DIR:"/run/teagramd/media-keys"},
            volumes:[{source:$key,target:"/run/teagramd/media-keys",read_only:true}]}}}'
        exit
      fi
      jq -cn --arg key "$FIXTURE_KEY_DIR" --arg evidence "$FIXTURE_EVIDENCE" \
        --arg scenario "$FIXTURE_SCENARIO" \
        --arg state "$FIXTURE_REPO/.state/blob-mode" '
        {services:{
          "blob-encrypt-migrate":{
            image:"teagramd:local",entrypoint:["/usr/local/bin/blob-encrypt-migrate"],
            user:"0:0",read_only:true,profiles:["migration"],
            volumes:[
              {source:(if $scenario == "compose-mount-failure" then "/unexpected/media-keys" else $key end),
                target:"/run/teagramd/media-keys",read_only:true},
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
      if [[ " $* " == *' --cleanup '* ]]; then
        printf 'cleanup\n' >> "$FIXTURE_EVENTS"
      else
        printf 'migration\n' >> "$FIXTURE_EVENTS"
      fi
      if [[ "$FIXTURE_SCENARIO" == interrupted-migration ]]; then
        echo 'fixture interrupted migration' >&2
        exit 42
      fi
      if [[ "$FIXTURE_SCENARIO" == cleanup-gating ]]; then
        printf '{"mode":"encrypt","authority":"encrypted","writes":"stopped","objects":1,"bytes":1,"unaccounted":[{"kind":"file","id":"999","key":"files/999"}]}\n'
      elif [[ "$FIXTURE_SCENARIO" == encrypt-success ]]; then
        publish_encrypted_authority
        printf '{"mode":"encrypt","authority":"encrypted","writes":"stopped","objects":1,"bytes":7,"unaccounted":[]}\n'
      elif [[ "$FIXTURE_SCENARIO" == cleanup-success || \
        "$FIXTURE_SCENARIO" == cleanup-recheck-removes || \
        "$FIXTURE_SCENARIO" == cleanup-post-unaccounted ]] && \
        [[ " $* " == *' --cleanup '* ]]; then
        cleanup_calls=0
        [[ ! -f "$FIXTURE_CLEANUP_CALLS" ]] || cleanup_calls=$(cat "$FIXTURE_CLEANUP_CALLS")
        cleanup_calls=$((cleanup_calls + 1))
        printf '%s\n' "$cleanup_calls" > "$FIXTURE_CLEANUP_CALLS"
        removed=0
        if [[ "$cleanup_calls" == 1 ]]; then
          rm -- "$FIXTURE_PLAINTEXT_OBJECT"
          removed=1
        elif [[ "$FIXTURE_SCENARIO" == cleanup-recheck-removes && "$cleanup_calls" == 2 ]]; then
          removed=1
        fi
        printf '{"mode":"cleanup","authority":"encrypted","writes":"stopped","objects":1,"bytes":7,"plaintext_objects_removed":%s,"unaccounted":[]}\n' "$removed"
      elif [[ "$FIXTURE_SCENARIO" == cleanup-post-unaccounted && " $* " != *' --cleanup '* ]]; then
        encrypt_calls=0
        [[ ! -f "$FIXTURE_ENCRYPT_CALLS" ]] || encrypt_calls=$(cat "$FIXTURE_ENCRYPT_CALLS")
        encrypt_calls=$((encrypt_calls + 1))
        printf '%s\n' "$encrypt_calls" > "$FIXTURE_ENCRYPT_CALLS"
        if [[ "$encrypt_calls" == 2 ]]; then
          printf '{"mode":"encrypt","authority":"encrypted","writes":"stopped","objects":1,"bytes":7,"unaccounted":[{"kind":"file","id":"999","key":"files/999"}]}\n'
        else
          printf '{"mode":"encrypt","authority":"encrypted","writes":"stopped","objects":1,"bytes":7,"unaccounted":[]}\n'
        fi
      else
        printf '{"mode":"encrypt","authority":"encrypted","writes":"stopped","objects":1,"bytes":7,"unaccounted":[]}\n'
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
cat > "$TMP/bin/sync" <<'SYNC'
#!/usr/bin/env bash
[[ "${FIXTURE_SYNC_FAIL:-0}" != 1 ]] || exit 1
exec /usr/bin/sync "$@"
SYNC
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
  FIXTURE_AUTHORITY_FILE=$case_dir/authority
  FIXTURE_RESTORE_CONTAINER=$case_dir/restore-container
  FIXTURE_CLEANUP_CALLS=$case_dir/cleanup-calls
  FIXTURE_ENCRYPT_CALLS=$case_dir/encrypt-calls
  FIXTURE_PLAINTEXT_OBJECT=$case_dir/plaintext-source
  FIXTURE_EVENTS=$case_dir/events
  FIXTURE_SCENARIO=$scenario
  FIXTURE_COMPOSE_FILES=$FIXTURE_REPO/docker-compose.yml,$FIXTURE_REPO/docker-compose.override.yml
  TEAGRAM_BLOB_MODE_REPORT_ROOT=$FIXTURE_EVIDENCE
  export TEAGRAM_BLOB_MODE_REPORT_ROOT
  export FIXTURE_REPO FIXTURE_EVIDENCE FIXTURE_OPERATION_EVIDENCE FIXTURE_KEY_DIR FIXTURE_POSTGRES_STATE \
    FIXTURE_SERVER_STATE FIXTURE_RUSTFS_STATE FIXTURE_AUTHORITY_FILE FIXTURE_RESTORE_CONTAINER \
    FIXTURE_CLEANUP_CALLS FIXTURE_ENCRYPT_CALLS FIXTURE_PLAINTEXT_OBJECT FIXTURE_EVENTS FIXTURE_SCENARIO \
    FIXTURE_COMPOSE_FILES

  mkdir -p "$FIXTURE_REPO/.state/blob-mode" "$FIXTURE_EVIDENCE/media-key-backups" "$FIXTURE_KEY_DIR"
  mkdir -m 700 "$FIXTURE_OPERATION_EVIDENCE"
  chmod 700 "$FIXTURE_EVIDENCE" "$FIXTURE_EVIDENCE/media-key-backups" "$FIXTURE_KEY_DIR"
  printf 'fixture env\n' > "$FIXTURE_REPO/.env"
  printf 'fixture parked env\n' > "$FIXTURE_REPO/.env.rustfs-parked"
  printf 'fixture compose\n' > "$FIXTURE_REPO/docker-compose.yml"
  printf 'fixture override\n' > "$FIXTURE_REPO/docker-compose.override.yml"
  chmod 600 "$FIXTURE_REPO/.env" "$FIXTURE_REPO/.env.rustfs-parked"
  printf '%064d\n' 0 > "$FIXTURE_KEY_DIR/1.key"
  cp "$FIXTURE_KEY_DIR/1.key" "$FIXTURE_EVIDENCE/media-key-backups/1.key"
  chmod 600 "$FIXTURE_KEY_DIR/1.key" "$FIXTURE_EVIDENCE/media-key-backups/1.key"
  printf 'running\n' > "$FIXTURE_POSTGRES_STATE"
  printf 'running\n' > "$FIXTURE_SERVER_STATE"
  printf 'running\n' > "$FIXTURE_RUSTFS_STATE"
  printf '%s\n' "$authority" > "$FIXTURE_AUTHORITY_FILE"
  if [[ "$scenario" == cleanup-success || "$scenario" == cleanup-recheck-removes || \
    "$scenario" == cleanup-post-unaccounted ]]; then
    printf 'fixture plaintext source\n' > "$FIXTURE_PLAINTEXT_OBJECT"
  fi
  if [[ "$scenario" == key-backup-mismatch ]]; then
    printf 'different fixture backup\n' > "$FIXTURE_EVIDENCE/media-key-backups/1.key"
  fi
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
  chmod 644 "$FIXTURE_REPO/.state/blob-mode/mode.json"
  printf 'encrypted\n' > "$FIXTURE_AUTHORITY_FILE"
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

assert_hash_mismatch() {
  local invariant=$1
  grep -Eq "preflight mismatch invariant=${invariant} expected_sha256=[0-9a-f]{64} actual_sha256=[0-9a-f]{64}" \
    <<< "$output"
}

setup_case key-backup-create s3-accepted
rm "$FIXTURE_EVIDENCE/media-key-backups/1.key"
rmdir "$FIXTURE_EVIDENCE/media-key-backups"
chmod 755 "$FIXTURE_EVIDENCE"
sed -e "s|/etc/teagramd/media-keys|$FIXTURE_KEY_DIR|g" \
  -e "s|/etc/teagramd|$FIXTURE_EVIDENCE|g" \
  "$SCRIPT_DIR/ops/backup-media-key.sh" > "$TMP/backup-media-key.sh"
run_backup() {
  local status=0 result
  result=$(env OPS_SHA="$FIXTURE_SHA" PATH="$TMP/bin:$PATH" bash "$TMP/backup-media-key.sh" 2>&1) || status=$?
  printf '%s\n' "$status" > "$TMP/operation-status"
  printf '%s\n' "$result"
}
chmod 777 "$FIXTURE_EVIDENCE"
output=$(run_backup)
[[ "$(cat "$TMP/operation-status")" == 1 && "$output" == *'backup parent is writable by others'* ]]
[[ ! -e "$FIXTURE_EVIDENCE/media-key-backups/1.key" ]]
chmod 755 "$FIXTURE_EVIDENCE"
export FIXTURE_SYNC_FAIL=1
output=$(run_backup)
unset FIXTURE_SYNC_FAIL
[[ "$(cat "$TMP/operation-status")" == 1 && "$output" == *'cannot sync backup'* ]]
[[ ! -e "$FIXTURE_EVIDENCE/media-key-backups/1.key" ]]
rmdir "$FIXTURE_EVIDENCE/media-key-backups"
output=$(run_backup)
[[ "$(cat "$TMP/operation-status")" == 0 && "$output" == *media_key_backup=verified* ]]
cmp -s "$FIXTURE_KEY_DIR/1.key" "$FIXTURE_EVIDENCE/media-key-backups/1.key"
[[ "$(stat -c %a "$FIXTURE_EVIDENCE/media-key-backups/1.key")" == 600 ]]
[[ "$(stat -c %a "$FIXTURE_EVIDENCE/media-key-backups")" == 700 ]]
output=$(run_backup)
[[ "$(cat "$TMP/operation-status")" == 1 && "$output" == *'refusing to overwrite'* ]]
printf 'different fixture backup\n' > "$FIXTURE_EVIDENCE/media-key-backups/1.key"
output=$(run_backup)
[[ "$(cat "$TMP/operation-status")" == 1 && "$output" == *'refusing to overwrite'* ]]
[[ "$(cat "$FIXTURE_EVIDENCE/media-key-backups/1.key")" == 'different fixture backup' ]]
rm "$FIXTURE_KEY_DIR/1.key"
output=$(run_backup)
[[ "$(cat "$TMP/operation-status")" == 1 && "$output" == *"$FIXTURE_KEY_DIR/1.key"* && "$output" == *'65532:600:65532'* ]]
printf '%s\n' 'PASS backup creates a matching private copy and refuses missing keys or existing backups'

setup_case key-backup-missing s3-accepted
rm "$FIXTURE_EVIDENCE/media-key-backups/1.key"
output=$(run_encrypt)
[[ "$(cat "$TMP/operation-status")" == 1 && "$output" == *'media key metadata does not match reviewed preflight'* ]]
[[ "$(cat "$FIXTURE_EVENTS")" != *'stop:server'* ]]
printf '%s\n' 'PASS missing media-key backup stops preflight before teagramd'

setup_case key-backup-unsafe s3-accepted
chmod 755 "$FIXTURE_EVIDENCE/media-key-backups"
output=$(run_encrypt)
[[ "$(cat "$TMP/operation-status")" == 1 && "$output" == *'media key backup directory is unsafe'* ]]
[[ "$(cat "$FIXTURE_EVENTS")" != *'stop:server'* ]]
printf '%s\n' 'PASS unsafe media-key backup directory stops preflight before teagramd'

setup_case key-backup-parent-writable s3-accepted
chmod 777 "$FIXTURE_EVIDENCE"
output=$(run_encrypt)
[[ "$(cat "$TMP/operation-status")" == 1 && "$output" == *'media key backup parent is unsafe'* ]]
[[ "$(cat "$FIXTURE_EVENTS")" != *'stop:server'* ]]
printf '%s\n' 'PASS writable media-key backup parent stops preflight before teagramd'

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
[[ "$(cat "$FIXTURE_EVENTS")" == $'stop:server\nimage-check\nmigration\nstart:server' ]]
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
[[ "$(cat "$FIXTURE_EVENTS")" == $'stop:server\nimage-check\nmigration\nstart:server' ]]
printf '%s\n' 'PASS cleanup requires acceptance and refuses unaccounted objects before deleting plaintext'

setup_case restart-recovery encrypted
output=$(run_encrypt)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'RustFS restart failed'* ]]
[[ "$(cat "$FIXTURE_RUSTFS_STATE")" == running ]]
[[ "$(cat "$FIXTURE_SERVER_STATE")" == running ]]
[[ "$(cat "$FIXTURE_EVENTS")" == $'stop:server\nimage-check\nmigration\nrestart:rustfs\nstart:rustfs\nstart:server' ]]
printf '%s\n' 'PASS restart failure recovers RustFS and teagramd before returning'

setup_case encrypt-success s3-accepted
output=$(run_encrypt)
[[ "$(cat "$TMP/operation-status")" == 0 ]]
[[ "$output" == *'migration=pass authority=encrypted'* && "$output" == *'dump_restore=pass'* ]]
[[ "$(cat "$FIXTURE_AUTHORITY_FILE")" == encrypted ]]
[[ -s "$FIXTURE_REPO/.state/blob-mode/mode.json" ]]
[[ "$(cat "$FIXTURE_SERVER_STATE")" == running && "$(cat "$FIXTURE_RUSTFS_STATE")" == running ]]
manifest_count=0
for manifest in "$FIXTURE_OPERATION_EVIDENCE"/pgdump-pre-migration-*.manifest; do
  [[ -f "$manifest" ]] || continue
  manifest_count=$((manifest_count + 1))
  [[ "$(jq -er '.result' "$manifest")" == isolated-restore-pass ]]
  dump_path=$(jq -er '.dump_path' "$manifest")
  [[ -s "$dump_path" ]]
done
[[ "$manifest_count" == 1 ]]
[[ "$(cat "$FIXTURE_EVENTS")" == $'stop:server\npg_dump\nrestore:start\nrestore:pg_restore-list\nrestore:pg_restore\nrestore:psql\nrestore:stop\nimage-check\nmigration\nrestart:rustfs\nimage-check\nmigration\nstart:server\nsmoke' ]]
printf '%s\n' 'PASS encryption publishes verified backup and encrypted authority before restarting services'

setup_case cleanup-success encrypted
output=$(run_cleanup --acceptance-passed)
[[ "$(cat "$TMP/operation-status")" == 0 ]]
[[ "$output" == *'cleanup=pass authority=encrypted'* ]]
[[ "$output" == *'plaintext_objects_removed=1'* && "$output" == *'cleanup_recheck_removed=0'* ]]
[[ ! -e "$FIXTURE_PLAINTEXT_OBJECT" ]]
[[ "$(cat "$FIXTURE_SERVER_STATE")" == running && "$(cat "$FIXTURE_RUSTFS_STATE")" == running ]]
[[ "$(cat "$FIXTURE_EVENTS")" == $'stop:server\nimage-check\nmigration\nimage-check\ncleanup\nimage-check\ncleanup\nimage-check\nmigration\nrestart:rustfs\nimage-check\nmigration\nstart:server\nsmoke' ]]
printf '%s\n' 'PASS cleanup removes plaintext only after acceptance and verifies encrypted readback'

setup_case retagged-image s3-accepted
output=$(run_encrypt)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'teagramd:local no longer identifies the inspected running server image'* ]]
[[ "$(cat "$FIXTURE_SERVER_STATE")" == running ]]
[[ "$(cat "$FIXTURE_EVENTS")" == $'stop:server\npg_dump\nrestore:start\nrestore:pg_restore-list\nrestore:pg_restore\nrestore:psql\nrestore:stop\nimage-check\nstart:server' ]]
printf '%s\n' 'PASS migration refuses a retagged image and restores teagramd'

setup_case cleanup-recheck-removes encrypted
output=$(run_cleanup --acceptance-passed)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'referenced plaintext objects remained after cleanup verification'* ]]
[[ ! -e "$FIXTURE_PLAINTEXT_OBJECT" ]]
[[ "$(cat "$FIXTURE_SERVER_STATE")" == running && "$(cat "$FIXTURE_RUSTFS_STATE")" == running ]]
[[ "$(cat "$FIXTURE_EVENTS")" == $'stop:server\nimage-check\nmigration\nimage-check\ncleanup\nimage-check\ncleanup\nstart:server' ]]
printf '%s\n' 'PASS cleanup rejects a nonzero removal recheck and restores the server'

setup_case cleanup-post-unaccounted encrypted
output=$(run_cleanup --acceptance-passed)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'media migration result contains 1 unaccounted objects'* ]]
[[ "$(cat "$FIXTURE_SERVER_STATE")" == running && "$(cat "$FIXTURE_RUSTFS_STATE")" == running ]]
[[ "$(cat "$FIXTURE_EVENTS")" == $'stop:server\nimage-check\nmigration\nimage-check\ncleanup\nimage-check\ncleanup\nimage-check\nmigration\nstart:server' ]]
printf '%s\n' 'PASS cleanup refuses post-cleanup unaccounted objects and restores the server'

setup_case already-encrypted-success encrypted
output=$(run_encrypt)
[[ "$(cat "$TMP/operation-status")" == 0 ]]
[[ "$output" == *'migration=already_encrypted authority=encrypted'* ]]
[[ "$(cat "$FIXTURE_SERVER_STATE")" == running && "$(cat "$FIXTURE_RUSTFS_STATE")" == running ]]
[[ "$(cat "$FIXTURE_EVENTS")" == $'stop:server\nimage-check\nmigration\nrestart:rustfs\nimage-check\nmigration\nstart:server\nsmoke' ]]
printf '%s\n' 'PASS already-encrypted migration resumes with verified readback and smoke'

for scenario in ancestry-origin-failure ancestry-live-failure ancestry-minimum-failure; do
  setup_case "$scenario" s3-accepted
  output=$(run_encrypt)
  [[ "$(cat "$TMP/operation-status")" == 1 ]]
  case "$scenario" in
    ancestry-origin-failure) [[ "$output" == *'reviewed operation source is not on origin/main'* ]] ;;
    ancestry-live-failure) [[ "$output" == *'reviewed operation source has not been deployed'* ]] ;;
    ancestry-minimum-failure) [[ "$output" == *'live image does not contain the media encryption operator'* ]] ;;
  esac
  [[ "$(cat "$FIXTURE_EVENTS")" != *'stop:server'* ]]
done
printf '%s\n' 'PASS each source ancestry guard rejects before stopping teagramd'

setup_case compose-mount-failure s3-accepted
output=$(run_encrypt)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'Compose migration or media-key mount failed reviewed preflight'* ]]
assert_hash_mismatch migration_media_key_mount
[[ "$output" != *'/unexpected/media-keys'* && "$output" != *"$FIXTURE_KEY_DIR"* ]]
[[ "$(cat "$FIXTURE_EVENTS")" != *'stop:server'* ]]
printf '%s\n' 'PASS Compose media-key mount drift reports only invariant hashes before stopping teagramd'

setup_case key-backup-mismatch s3-accepted
output=$(run_encrypt)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'host-local media key backup differs from the mounted key'* ]]
[[ "$output" != *expected_sha256=* && "$output" != *actual_sha256=* ]]
[[ "$output" != *'different fixture backup'* && "$output" != *"$(printf '%064d' 0)"* ]]
[[ "$(cat "$FIXTURE_EVENTS")" != *'stop:server'* ]]
printf '%s\n' 'PASS mismatched media-key backup reveals no key digest before stopping teagramd'

setup_case compose-render-failure s3-accepted
output=$(run_encrypt)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'live Compose render failed; private diagnostic retained at'* ]]
diagnostic_count=0
for diagnostic in "$FIXTURE_OPERATION_EVIDENCE"/compose-render-*.stderr; do
  [[ -f "$diagnostic" ]] || continue
  diagnostic_count=$((diagnostic_count + 1))
  [[ "$(stat -c '%a' "$diagnostic")" == 600 ]]
  [[ "$(cat "$diagnostic")" == 'fixture Compose render failure' ]]
  [[ "$output" == *"private diagnostic retained at $diagnostic"* ]]
done
[[ "$diagnostic_count" == 1 ]]
[[ "$(cat "$FIXTURE_EVENTS")" != *'stop:server'* ]]
printf '%s\n' 'PASS Compose render diagnostics persist privately after the operation exits'
