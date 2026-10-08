#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
VERIFIER="$SCRIPT_DIR/rollout-verifier.sh"
SCHEMA_GATE="$SCRIPT_DIR/schema-result-gate.sh"
MODE_HELPER="$SCRIPT_DIR/blob-mode-state.py"
REPO_ROOT=$(cd -P "$SCRIPT_DIR/../../.." && pwd)
REAL_PYTHON3=$(command -v python3)
REAL_FLOCK=$(command -v flock)
source "$SCRIPT_DIR/migration-fixture-source.sh"
if [ "$(id -u)" != 0 ]; then
  printf '%s\n' 'run the rollout runner fixtures as root to exercise production evidence checks' >&2
  exit 77
fi
TMP=$(mktemp -d "${TMPDIR:-/root}/main1238-rollout-fixtures.XXXXXXXX")
chmod 700 "$TMP"
MIGRATION_FIXTURE_DIR="$REPO_ROOT/.main1421-migration-fixture-$$-${RANDOM}"
if [ "${KEEP_FIXTURE_ARTIFACTS:-0}" = 1 ]; then
  trap 'printf "fixture_artifacts=%s migration_source=%s\\n" "$TMP" "$MIGRATION_FIXTURE_DIR"' EXIT
else
  cleanup_fixtures() {
    local root_file root phase checkout transition record_file
    for root_file in "$TMP"/*-root-path; do
      [ -f "$root_file" ] || continue
      root=$(cat "$root_file")
      checkout=$(cat "${root_file%-root-path}-checkout-path" 2>/dev/null || true)
      if [ -n "$checkout" ] && [ -f "$checkout/.state/blob-mode/mode.json" ]; then
        for record_file in "$checkout"/.state/blob-mode/journal/*.json "$checkout"/.state/blob-mode/mode.json; do
          [ -f "$record_file" ] || continue
          transition=$(jq -r '.transition_id' "$record_file")
          rm -f -- "/root/telegramd-blob-mode-report-$transition.json"
        done
      fi
      for phase in baseline backup build target rollback; do
        rm -rf -- "$root.$phase"
      done
    done
    rm -rf -- "$MIGRATION_FIXTURE_DIR"
    rm -rf -- "$TMP"
  }
  trap cleanup_fixtures EXIT
fi
load_production_migration_pins "$SCHEMA_GATE"
prepare_immutable_migration_source "$REPO_ROOT" "$MIGRATION_FIXTURE_DIR"
MIGRATIONS_DIR="$MIGRATION_FIXTURE_DIR/migrations"

PASS_COUNT=0
FAIL_COUNT=0
FAILURES=()
FIXTURE_INDEX=0
TARGET_SHA=ffffffffffffffffffffffffffffffffffffffff
BASELINE_SHA=9999999999999999999999999999999999999999
APPLY_TARGET_SHA=8888888888888888888888888888888888888888
BASE_IMAGE=sha256:0000000000000000000000000000000000000000000000000000000000000000
BUILT_IMAGE=sha256:1111111111111111111111111111111111111111111111111111111111111111
POSTGRES_IMAGE=sha256:2222222222222222222222222222222222222222222222222222222222222222
BASE_ID=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
TARGET_ID=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
APPLY_ID=ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
ROLLBACK_ID=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
POSTGRES_ID=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
MIGRATE_ID=eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee

pass() {
  PASS_COUNT=$((PASS_COUNT + 1))
  printf 'PASS %s\n' "$1"
}

fail() {
  FAIL_COUNT=$((FAIL_COUNT + 1))
  FAILURES+=("$1")
  printf 'FAIL %s\n' "$1"
}

write_mock_commands() {
  local bin=$1
  mkdir -m 700 "$bin"
  cat > "$bin/git" <<'SH'
#!/usr/bin/env bash
set -eu
if [ "${1:-}" = -C ]; then shift 2; fi
printf 'git %s\n' "$*" >> "$MOCK_EVENTS"
case "$*" in
  'fetch -q origin') exit 0 ;;
  'branch --show-current') printf 'main\n' ;;
  'rev-parse HEAD') cat "$MOCK_STATE/head" ;;
  'rev-parse origin/main') cat "$MOCK_STATE/origin" ;;
  "ls-tree -r -z $MOCK_TARGET_SHA -- migrations/")
    while IFS= read -r -d '' migration_file; do
      printf '100644 blob 0000000000000000000000000000000000000000\tmigrations/%s\0' "${migration_file##*/}"
    done < <(find "$MOCK_MIGRATIONS_DIR" -maxdepth 1 -type f -print0 | sort -z)
    if [ "${MOCK_SCENARIO:-}" = precheck-extra-file ]; then
      printf '100644 blob 0000000000000000000000000000000000000000\tmigrations/20261009000068_future.sql\0'
    fi
    ;;
  "show $MOCK_TARGET_SHA:migrations/"*)
    shown_path=${2#"$MOCK_TARGET_SHA:"}
    migration_file="$MOCK_MIGRATIONS_DIR/${shown_path#migrations/}"
    [ -f "$migration_file" ] || exit 1
    case "${MOCK_SCENARIO:-}" in
      precheck-tampered-sum)
        if [ "${shown_path#migrations/}" = atlas.sum ]; then cat "$migration_file"; printf '# tampered\n'; else cat "$migration_file"; fi
        ;;
      precheck-tampered-60|precheck-tampered-61|precheck-tampered-62|precheck-tampered-63|precheck-tampered-64|precheck-tampered-65|precheck-tampered-66|precheck-tampered-67)
        pin_number=${MOCK_SCENARIO##*-}
        if [[ "${shown_path##*/}" == *"0000${pin_number}_"* ]]; then cat "$migration_file"; printf '\n-- tampered fixture\n'; else cat "$migration_file"; fi
        ;;
      precheck-atlas-mismatch)
        if [[ "${shown_path##*/}" = 20261004000059_server_limit_leases.sql ]]; then cat "$migration_file"; printf '\n-- tampered legacy fixture\n'; else cat "$migration_file"; fi
        ;;
      *) cat "$migration_file" ;;
    esac
    ;;
  "show $MOCK_TARGET_SHA:deploy/telegramd/rollout-runner/rollout-runner.sh") cat "$MOCK_TARGET_RUNTIME_DIR/rollout-runner.sh" ;;
  "show $MOCK_TARGET_SHA:deploy/telegramd/rollout-runner/rollout-verifier.sh") cat "$MOCK_TARGET_RUNTIME_DIR/rollout-verifier.sh" ;;
  "show $MOCK_TARGET_SHA:deploy/telegramd/rollout-runner/schema-result-gate.sh") cat "$MOCK_TARGET_RUNTIME_DIR/schema-result-gate.sh" ;;
  "show $MOCK_TARGET_SHA:deploy/telegramd/rollout-runner/blob-mode-state.py") cat "$MOCK_TARGET_RUNTIME_DIR/blob-mode-state.py" ;;
  'status --porcelain=v1 --untracked-files=no')
    if [ -f "$MOCK_STATE/status-count" ]; then status_count=$(cat "$MOCK_STATE/status-count"); else status_count=0; fi
    status_count=$((status_count + 1))
    printf '%s\n' "$status_count" > "$MOCK_STATE/status-count"
    if [ "${MOCK_SCENARIO:-}" = dirty-staged ] && [ "$status_count" -eq 1 ]; then
      printf 'M  tracked-source.txt\n'
    elif [ "${MOCK_SCENARIO:-}" = dirty-unstaged ] && [ "$status_count" -eq 1 ]; then
      printf ' M tracked-source.txt\n'
    elif [ "${MOCK_SCENARIO:-}" = dirty-before-build ] && [ "$status_count" -eq 2 ]; then
      printf ' M tracked-source.txt\n'
    fi
    ;;
  'status --porcelain=v1 --untracked-files=all --ignored=matching -- cmd/ internal/ components/ utils/')
    if [ -f "$MOCK_STATE/build-input-status-count" ]; then build_input_status_count=$(cat "$MOCK_STATE/build-input-status-count"); else build_input_status_count=0; fi
    build_input_status_count=$((build_input_status_count + 1))
    printf '%s\n' "$build_input_status_count" > "$MOCK_STATE/build-input-status-count"
    if [ "${MOCK_SCENARIO:-}" = untracked-source ] && [ "$build_input_status_count" -eq 1 ]; then
      printf '?? internal/untracked.go\n'
    elif [ "${MOCK_SCENARIO:-}" = untracked-before-build ] && [ "$build_input_status_count" -eq 2 ]; then
      printf '?? cmd/untracked.go\n'
    fi
    ;;
  'status --porcelain=v1 --untracked-files=all --ignored=matching -- migrations/')
    if [ -f "$MOCK_STATE/migration-status-count" ]; then migration_status_count=$(cat "$MOCK_STATE/migration-status-count"); else migration_status_count=0; fi
    migration_status_count=$((migration_status_count + 1))
    printf '%s\n' "$migration_status_count" > "$MOCK_STATE/migration-status-count"
    if [ "${MOCK_SCENARIO:-}" = precheck-untracked ] && [ "$migration_status_count" -eq 1 ]; then
      printf '?? migrations/operator.sql\n'
    elif [ "${MOCK_SCENARIO:-}" = precheck-ignored ] && [ "$migration_status_count" -eq 1 ]; then
      printf '!! migrations/ignored.sql\n'
    elif [ "${MOCK_SCENARIO:-}" = precheck-dirty-after-ff ] && [ "$migration_status_count" -eq 2 ]; then
      printf '?? migrations/after-fast-forward.sql\n'
    fi
    ;;
  'merge --ff-only -q origin/main')
    cat "$MOCK_STATE/origin" > "$MOCK_STATE/head"
    printf '%s\n' target > "$MOCK_STATE/phase"
    if [ "${MOCK_SCENARIO:-}" = dirty-before-build ]; then
      printf '%s\n' 'operator edit survives rollout guard' > "$MOCK_CHECKOUT/tracked-source.txt"
    fi
    if [ "${MOCK_SCENARIO:-}" = untracked-before-build ]; then
      printf '%s\n' 'package telegramd' > "$MOCK_CHECKOUT/cmd/untracked.go"
    fi
    if [ "${MOCK_SCENARIO:-}" = runtime-mutation ]; then
      printf '%s\n' 'not the pinned verifier' > "$MOCK_CHECKOUT/deploy/telegramd/rollout-runner/rollout-verifier.sh"
      printf '%s\n' 'not the pinned schema gate' > "$MOCK_CHECKOUT/deploy/telegramd/rollout-runner/schema-result-gate.sh"
      printf '%s\n' '#!/bin/sh' 'exit 99' > "$MOCK_CHECKOUT/deploy/telegramd/rollout-runner/rollout-runner.sh"
    fi
    ;;
  'reset --hard '*)
    if [ "${MOCK_REQUIRE_FAILURE_MARKER:-0}" = 1 ] && \
       ! find "$MOCK_EVIDENCE_ROOT".* \( -name target-failure.txt -o -name preflight-failure.txt \) -type f -print -quit | grep -q .; then
      printf 'rollback started before target failure evidence was persisted\n' >&2
      exit 89
    fi
    printf '%s\n' "${*: -1}" > "$MOCK_STATE/head"
    printf '%s\n' baseline > "$MOCK_STATE/phase"
    ;;
  *) printf 'unexpected git wrapper call\n' >&2; exit 90 ;;
esac
SH
  cat > "$bin/python3" <<'SH'
#!/usr/bin/env bash
set -eu
script_name=${1:-}
script_name=${script_name##*/}
if { [ "$script_name" = blob-mode-state.py ] || [ "$script_name" = blob-mode-state.pinned ]; } && \
   [ "${2:-}" = initialize-local ] && [ "${MOCK_SCENARIO:-}" = interrupted-initial-publication ]; then
  "$MOCK_REAL_PYTHON3" "$@"
  [ -f "$MOCK_CHECKOUT/.state/blob-mode/journal/0000000001.json" ] || exit 96
  [ -f "$MOCK_CHECKOUT/.state/blob-mode/mode.json" ] || exit 97
  rm -- "$MOCK_CHECKOUT/.state/blob-mode/mode.json"
  exit 1
fi
exec "$MOCK_REAL_PYTHON3" "$@"
SH
  cat > "$bin/docker" <<'SH'
#!/usr/bin/env bash
set -eu
printf 'docker %s\n' "$*" >> "$MOCK_EVENTS"
phase=$(cat "$MOCK_STATE/phase")
if [ -n "${MOCK_REAL_GIT:-}" ]; then
  real_head=$("$MOCK_REAL_GIT" -C "$MOCK_CHECKOUT" rev-parse HEAD)
  if [ "$real_head" = "$MOCK_TARGET_SHA" ]; then phase=target; else phase=baseline; fi
fi
if [ "${1:-}" = image ] && [ "${2:-}" = inspect ]; then
  case "${MOCK_CAPTURE_IMAGE:-built}" in
    built) printf '%s\n' "$MOCK_BUILT_IMAGE" ;;
    old) printf '%s\n' "$MOCK_BASE_IMAGE" ;;
    wrong) printf 'sha256:wrong\n' ;;
    missing) exit 0 ;;
    *) exit 91 ;;
  esac
  exit 0
fi
if [ "${1:-}" = image ] && [ "${2:-}" = tag ]; then
  printf '%s\n' "${3:?}" > "$MOCK_STATE/tag"
  exit 0
fi
if [ "${1:-}" = ps ]; then
  printf '%s\n' "$(cat "$MOCK_STATE/telegramd")"
  exit 0
fi
if [ "${1:-}" = volume ] && [ "${2:-}" = inspect ]; then
  name=${3:?}
  jq -nc --arg name "$name" '[{Name:$name,Driver:"local",Labels:{"com.docker.compose.volume":"tgblobs","com.docker.compose.project":"fixture"}}]'
  exit 0
fi
if [ "${1:-}" = inspect ]; then
  shift
  if [ "${1:-}" = --type ]; then exit 1; fi
  if [ "$#" -gt 1 ] && [ "${2:-}" != --format ]; then
    objects=()
    for inspect_id in "$@"; do objects+=("$("$0" inspect "$inspect_id")"); done
    printf '%s\n' "${objects[@]}" | jq -s .
    exit 0
  fi
  subject=${1:-}
  shift || true
  if [ "${1:-}" = --format ]; then
    template=${2:-}
    case "$template" in
      '{{.State.Status}}|{{.State.ExitCode}}')
        if [ "$subject" = "$MOCK_MIGRATE_ID" ]; then printf 'exited|0\n'
        elif [ "$subject" = "$MOCK_TARGET_ID" ] && [ "${MOCK_SCENARIO:-}" = runtime-target-exited ]; then printf 'exited|1\n'
        else printf 'running|0\n'; fi
        ;;
      '{{.State.Health.Status}}') printf 'healthy\n' ;;
      '{{.Image}}')
        case "$subject" in
          "$MOCK_BASE_ID"|"$MOCK_ROLLBACK_ID") printf '%s\n' "$MOCK_BASE_IMAGE" ;;
          "$MOCK_TARGET_ID"|"$MOCK_REPLACEMENT_ID")
            if [ "${MOCK_SCENARIO:-success}" = old-target-image ]; then printf '%s\n' "$MOCK_BASE_IMAGE"; else printf '%s\n' "$MOCK_ACTUAL_TARGET_IMAGE"; fi
            ;;
          *) printf '%s\n' "$MOCK_POSTGRES_IMAGE" ;;
        esac
        ;;
      *) printf 'unexpected inspect format\n' >&2; exit 92 ;;
    esac
    exit 0
  fi
  case "$subject" in
    "$MOCK_BASE_ID") id=$MOCK_BASE_ID; image=$MOCK_BASE_IMAGE; cfg=baseline ;;
    "$MOCK_TARGET_ID"|"$MOCK_REPLACEMENT_ID")
      id=$subject
      cfg=target
      if [ "${MOCK_SCENARIO:-success}" = old-target-image ]; then image=$MOCK_BASE_IMAGE; else image=$MOCK_ACTUAL_TARGET_IMAGE; fi
      ;;
    "$MOCK_ROLLBACK_ID") id=$MOCK_ROLLBACK_ID; image=$MOCK_BASE_IMAGE; cfg=baseline ;;
    "$MOCK_POSTGRES_ID") id=$MOCK_POSTGRES_ID; image=$MOCK_POSTGRES_IMAGE; cfg=postgres ;;
    "$MOCK_MIGRATE_ID") id=$MOCK_MIGRATE_ID; image=$MOCK_POSTGRES_IMAGE; cfg=migrate ;;
    *) printf 'unknown inspect subject\n' >&2; exit 93 ;;
  esac
  if [ "$cfg" = postgres ]; then
    jq -nc --arg id "$id" --arg image "$image" '{Id:$id,Image:$image,Config:{Env:[],StopTimeout:120,Labels:{"com.docker.compose.project":"fixture","com.docker.compose.service":"postgres"}},State:{Status:"running",ExitCode:0,StartedAt:"2026-10-06T12:00:00Z",FinishedAt:"0001-01-01T00:00:00Z",Health:{Status:"healthy"}},HostConfig:{PortBindings:{}},Mounts:[]}'
  elif [ "$cfg" = migrate ]; then
    jq -nc --arg id "$id" --arg image "$image" '{Id:$id,Image:$image,Config:{Env:[],StopTimeout:120,Labels:{"com.docker.compose.project":"fixture","com.docker.compose.service":"migrate"}},State:{Status:"exited",ExitCode:0,StartedAt:"2026-10-06T12:00:00Z",FinishedAt:"2026-10-06T12:00:01Z"},HostConfig:{PortBindings:{}},Mounts:[]}'
  else
    env_json='["TG_SYNTHETIC_FLAG=fixture","TG_BLOB_DIR=/var/lib/telegramd-blobs"]'
    if [ "$cfg" = target ] && [ "${MOCK_SCENARIO:-success}" = runtime-backend-mismatch ]; then
      env_json='["TG_SYNTHETIC_FLAG=fixture","TG_BLOB_DIR=/unexpected-blob-dir","TG_REPLICA_COUNT=1","TG_CLIENT_ADDR_TRUST=socket"]'
    elif [ "$cfg" = target ] && [ "${MOCK_SCENARIO:-success}" != config-drift ]; then
      env_json='["TG_SYNTHETIC_FLAG=fixture","TG_BLOB_DIR=/var/lib/telegramd-blobs","TG_REPLICA_COUNT=1","TG_CLIENT_ADDR_TRUST=socket"]'
    elif [ "$cfg" = target ] && [ "${MOCK_SCENARIO:-success}" = config-drift ]; then
      env_json='["TG_SYNTHETIC_FLAG=fixture","TG_BLOB_DIR=/var/lib/telegramd-blobs","TG_REPLICA_COUNT=1","TG_CLIENT_ADDR_TRUST=socket","TG_UNRELATED=drift"]'
    fi
    jq -nc --arg id "$id" --arg image "$image" --argjson env "$env_json" --arg cfg "$cfg" --arg source "$MOCK_CHECKOUT/.state/blob-mode" --arg scenario "${MOCK_SCENARIO:-}" '{Id:$id,Image:$image,Config:{Env:$env,StopTimeout:120,Labels:{"com.docker.compose.project":"fixture","com.docker.compose.service":"telegramd"}},State:{Status:(if $scenario == "runtime-target-exited" and $cfg == "target" then "exited" else "running" end),ExitCode:(if $scenario == "runtime-target-exited" and $cfg == "target" then 1 else 0 end),StartedAt:"2026-10-06T12:00:00Z",FinishedAt:(if $scenario == "runtime-target-exited" and $cfg == "target" then "2026-10-06T12:00:01Z" else "0001-01-01T00:00:00Z" end)},HostConfig:{PortBindings:{"2443/tcp":[{HostIp:"127.0.0.1",HostPort:"2443"}],"2444/tcp":[{HostIp:"127.0.0.1",HostPort:"2444"}]}},Mounts:([{Type:"volume",Name:"identity",Source:"/synthetic/identity",Destination:"/var/lib/telegramd",Mode:"rw",RW:true,Propagation:"rprivate"},{Type:"volume",Name:(if $scenario == "runtime-volume-mismatch" and $cfg == "target" then "unexpected_tgblobs" else "fixture_tgblobs" end),Source:"/synthetic/blobs",Destination:"/var/lib/telegramd-blobs",Mode:"rw",RW:true,Propagation:"rprivate"}] + if $cfg == "target" and $scenario != "runtime-mode-unmounted" then [{Type:"bind",Name:"",Source:$source,Destination:"/run/telegramd/blob-mode",Mode:"ro",RW:false,Propagation:"rprivate"}] else [] end)}'
  fi
  exit 0
fi
if [ "${1:-}" = compose ]; then
  shift
  case "${1:-}" in
    ps)
      if [ "${2:-}" = -q ] && [ "${3:-}" = telegramd ]; then cat "$MOCK_STATE/telegramd"; exit 0; fi
      if [ "${2:-}" = -q ] && [ "${3:-}" = postgres ]; then printf '%s\n' "$MOCK_POSTGRES_ID"; exit 0; fi
      if [ "${2:-}" = -aq ] && [ "${3:-}" = telegramd ]; then cat "$MOCK_STATE/telegramd"; exit 0; fi
      if [ "${2:-}" = -aq ] && [ "${3:-}" = migrate ]; then printf '%s\n' "$MOCK_MIGRATE_ID"; exit 0; fi
      exit 94
      ;;
    config)
      if [ "$phase" = target ]; then cat "$MOCK_STATE/target-compose.json"; else cat "$MOCK_STATE/base-compose.json"; fi
      exit 0
      ;;
    build)
      printf 'build\n' >> "$MOCK_EVENTS"
      printf '%s\n' "$MOCK_BUILT_IMAGE" > "$MOCK_STATE/tag"
      exit 0
      ;;
    exec)
      if [[ " $* " == *' pg_dump '* ]]; then
        printf '%s\n' '-- PostgreSQL database dump complete'
        exit 0
      fi
      if [[ " $* " == *' psql '* ]] && [[ " $* " == *' -c '* ]]; then
        sql_text=$*
        if [[ "$sql_text" == *"revision_prefix_exact"* ]]; then
          read -r -a MOCK_APPROVED_REVISIONS <<< "${MOCK_APPROVED_REVISIONS:-}"
          read -r -a MOCK_APPROVED_REVISION_HASHES <<< "${MOCK_APPROVED_REVISION_HASHES:-}"
          start_revisions=${MOCK_START_REVISIONS:-'20261005000060 20261005000061 20261006000062'}
          prefix_ok=true
          complete_ok=true
          hashes_ok=true
          files_empty=true
          case "${MOCK_SCENARIO:-}" in
            precheck-gap) prefix_ok=false ;;
            precheck-extra-68) prefix_ok=false; hashes_ok=false ;;
            precheck-substituted-63) hashes_ok=false ;;
            precheck-incomplete) complete_ok=false ;;
            precheck-error) complete_ok=false ;;
            precheck-files) files_empty=false ;;
          esac
          printf 'check\trevision_prefix_exact\t%s\n' "$prefix_ok"
          printf 'check\trevisions_complete\t%s\n' "$complete_ok"
          printf 'check\trevision_hashes_exact\t%s\n' "$hashes_ok"
          printf 'check\tfiles_empty\t%s\n' "$files_empty"
          if [ "$start_revisions" != none ]; then
            for revision in $start_revisions; do
              revision_hash=fixture-unexpected
              for index in "${!MOCK_APPROVED_REVISIONS[@]}"; do
                if [ "${MOCK_APPROVED_REVISIONS[$index]}" = "$revision" ]; then revision_hash=${MOCK_APPROVED_REVISION_HASHES[$index]#h1:}; break; fi
              done
              applied=1
              total=1
              error_present=false
              if [ "${MOCK_SCENARIO:-}" = precheck-incomplete ] && [ "$revision" = 20261006000062 ]; then applied=0; fi
              if [ "${MOCK_SCENARIO:-}" = precheck-error ] && [ "$revision" = 20261006000062 ]; then error_present=true; fi
              if [ "${MOCK_SCENARIO:-}" = precheck-substituted-63 ] && [ "$revision" = 20261006000063 ]; then revision_hash=fixture-substituted; fi
              printf 'revision\t%s\t%s\t%s\t%s\t%s\n' "$revision" "$applied" "$total" "$error_present" "$revision_hash"
            done
          fi
          if [ "${MOCK_SCENARIO:-}" = precheck-extra-68 ]; then
            printf 'revision\t20261008000068\t1\t1\tfalse\tfixture-unexpected\n'
          fi
          exit 0
        fi
        approved_revisions='20261005000060 20261005000061 20261006000062 20261006000063 20261007000064 20261007000065 20261007000066 20261008000067'
        applied_revisions=${MOCK_APPLIED_REVISIONS:-$approved_revisions}
        approved_sorted=$(printf '%s\n' $approved_revisions | sort | tr '\n' ' ')
        applied_sorted=$(printf '%s\n' $applied_revisions | sort | tr '\n' ' ')
        for field in post_migration_migration_60_present post_migration_migration_61_present post_migration_migration_62_present post_migration_migration_63_present post_migration_migration_64_present post_migration_migration_65_present post_migration_migration_66_present post_migration_migration_67_present post_migration_approved_revision_set_exact post_migration_revision_detail_ok post_migration_poll_description_entities_schema_ok post_migration_files_subtype_constraint_valid post_migration_files_media_metadata_constraint_valid post_migration_files_empty post_migration_files_media_kind_schema_ok post_migration_files_width_schema_ok post_migration_files_height_schema_ok post_migration_reply_to_trusted_default_false post_migration_user_dialog_pins_schema_ok post_migration_cloud_drafts_schema_ok post_migration_cloud_draft_sync_schema_ok post_migration_cloud_draft_sync_changed_idx_present post_migration_user_dialog_pins_primary_key_columns_exact post_migration_user_dialog_pins_position_unique_columns_exact post_migration_cloud_drafts_primary_key_columns_exact post_migration_cloud_draft_sync_primary_key_columns_exact post_migration_user_dialog_unread_marks_schema_ok post_migration_user_dialog_unread_marks_primary_key_columns_exact post_migration_user_dialog_unread_marks_changed_idx_exact post_migration_secret_chats_admin_date_idx_exact post_migration_secret_chats_participant_date_idx_exact post_migration_secret_chats_index_names_exact; do
          value=true
          case "$field" in
            post_migration_migration_6[0-7]_present)
              checked_version=$(printf '%s\n' "$sql_text" | grep -A4 -F "('$field', EXISTS (" | sed -n "s/.*version = '\([0-9]\{14\}\)'.*/\1/p" | head -n1)
              value=false
              for revision in $applied_revisions; do
                if [ "$revision" = "$checked_version" ]; then value=true; break; fi
              done
              ;;
            post_migration_approved_revision_set_exact)
              expected_array=$(printf '%s\n' "$sql_text" | sed -n '/) = ARRAY\[/,/\]::text\[\]/p' | grep -oE "'20[0-9]{12}'" | tr -d "'" | sort | tr '\n' ' ')
              [ "$expected_array" = "$approved_sorted" ] || value=false
              [ "$applied_sorted" = "$approved_sorted" ] || value=false
              ;;
            post_migration_poll_description_entities_schema_ok)
              [[ "$*" == *"table_name = 'polls'"* &&
                 "$*" == *"column_name = 'description_entities'"* &&
                 "$*" == *"data_type = 'jsonb'"* &&
                 "$*" == *"is_nullable = 'NO'"* &&
                 "$*" == *"column_default"* &&
                 "$*" == *'{"version":1,"entities":[]}'* ]] || value=false
              ;;
          esac
          if [ "${MOCK_SCENARIO:-}" = schema-invalid-poll-description ] && [ "$field" = post_migration_poll_description_entities_schema_ok ]; then value=false; fi
          if [ "${MOCK_SCENARIO:-}" = schema-invalid-pins ] && [ "$field" = post_migration_user_dialog_pins_schema_ok ]; then value=false; fi
          if [ "${MOCK_SCENARIO:-}" = schema-invalid-revision-detail ] && [ "$field" = post_migration_revision_detail_ok ]; then value=false; fi
          if [ "${MOCK_SCENARIO:-}" = schema-invalid-unread-marks ] && [ "$field" = post_migration_user_dialog_unread_marks_schema_ok ]; then value=false; fi
          if [ "${MOCK_SCENARIO:-}" = schema-invalid-unread-pk ] && [ "$field" = post_migration_user_dialog_unread_marks_primary_key_columns_exact ]; then value=false; fi
          if [ "${MOCK_SCENARIO:-}" = schema-invalid-unread-index ] && [ "$field" = post_migration_user_dialog_unread_marks_changed_idx_exact ]; then value=false; fi
          if [ "${MOCK_SCENARIO:-}" = schema-invalid-secret-admin-date ] && [ "$field" = post_migration_secret_chats_admin_date_idx_exact ]; then value=false; fi
          if [ "${MOCK_SCENARIO:-}" = schema-invalid-secret-participant-date ] && [ "$field" = post_migration_secret_chats_participant_date_idx_exact ]; then value=false; fi
          if [ "${MOCK_SCENARIO:-}" = schema-invalid-secret-index-set ] && [ "$field" = post_migration_secret_chats_index_names_exact ]; then value=false; fi
          if [ "${MOCK_SCENARIO:-}" = schema-wrong-key ]; then
            case "$field" in
              post_migration_user_dialog_pins_primary_key_columns_exact|post_migration_cloud_drafts_primary_key_columns_exact|post_migration_cloud_draft_sync_primary_key_columns_exact|post_migration_user_dialog_unread_marks_primary_key_columns_exact)
                expected_key_columns="'owner_id', 'peer_type', 'peer_id'"
                ;;
              post_migration_user_dialog_pins_position_unique_columns_exact)
                expected_key_columns="'owner_id', 'position'"
                ;;
              *) expected_key_columns= ;;
            esac
            case "$field" in
              post_migration_user_dialog_pins_primary_key_columns_exact) expected_constraint=user_dialog_pins_pkey ;;
              post_migration_user_dialog_pins_position_unique_columns_exact) expected_constraint=user_dialog_pins_position_unique ;;
              post_migration_cloud_drafts_primary_key_columns_exact) expected_constraint=cloud_drafts_pkey ;;
              post_migration_cloud_draft_sync_primary_key_columns_exact) expected_constraint=cloud_draft_sync_pkey ;;
              post_migration_user_dialog_unread_marks_primary_key_columns_exact) expected_constraint=user_dialog_unread_marks_pkey ;;
              *) expected_constraint= ;;
            esac
            if [ -n "$expected_key_columns" ] && \
               [[ "$*" == *"'$field', EXISTS ("* ]] && \
               [[ "$*" == *"conname = '$expected_constraint'"* ]] && \
               [[ "$*" == *"key_columns = ARRAY[$expected_key_columns]::text[]"* ]]; then
              value=false
            fi
          fi
          printf '%s\t%s\n' "$field" "$value"
        done
        exit 0
      fi
      exit 95
      ;;
    up)
      if [[ " $* " == *' --no-build '* ]]; then
        printf '%s\n' rollback > "$MOCK_STATE/phase"
        printf '%s\n' "$MOCK_ROLLBACK_ID" > "$MOCK_STATE/telegramd"
      else
        printf '%s\n' target > "$MOCK_STATE/phase"
        printf '%s\n' "$MOCK_REPLACEMENT_ID" > "$MOCK_STATE/telegramd"
      fi
      exit 0
      ;;
    *) printf 'unexpected compose command\n' >&2; exit 96 ;;
  esac
fi
if [ "${1:-}" = run ]; then
  if [[ " $* " == *' arigaio/atlas:1.2.0-alpine migrate validate --dir file:///migrations '* ]]; then
    if [ "${MOCK_SCENARIO:-}" = precheck-atlas-mismatch ]; then exit 1; fi
    exit 0
  fi
  name=''
  while [ "$#" -gt 0 ]; do
    if [ "$1" = --name ]; then name=$2; break; fi
    shift
  done
  printf '%s\n' "$name" > "$MOCK_STATE/restore-name"
  printf 'restore network none tmpfs only\n' >> "$MOCK_EVENTS"
  printf 'fixture-restore-container\n'
  exit 0
fi
if [ "${1:-}" = exec ]; then
  shift
  while [ "${1:-}" = -i ] || [ "${1:-}" = -t ]; do shift; done
  if [[ "${1:-}" = main1238-restore-* ]] && [ "${2:-}" = pg_isready ]; then exit 0; fi
  if [[ "${1:-}" = main1238-restore-* ]] && [ "${2:-}" = psql ]; then cat >/dev/null; exit 0; fi
  exit 97
fi
if [ "${1:-}" = rm ] && [ "${2:-}" = -f ]; then exit 0; fi
if [ "${1:-}" = logs ]; then
  if [ "${MOCK_SCENARIO:-success}" = readiness-timeout ] && [ "$(cat "$MOCK_STATE/phase")" = target ]; then exit 0; fi
  if [ "${MOCK_SCENARIO:-success}" = logs-failed ] && [ "$(cat "$MOCK_STATE/phase")" = target ]; then
    printf 'fixture log read failed\n' >&2
    exit 23
  fi
  printf 'level=INFO advertise=telegram-server.tailaa4918.ts.net:2443\n'
  exit 0
fi
printf 'unexpected docker wrapper call\n' >&2
exit 98
SH
  cat > "$bin/flock" <<'SH'
#!/usr/bin/env bash
set -eu
[ "$1" = -x ] && [ "$2" = 9 ] || exit 99
printf 'flock -x 9\n' >> "$MOCK_EVENTS"
SH
  cat > "$bin/sync" <<'SH'
#!/usr/bin/env bash
set -eu
if [ "${MOCK_FAIL_SYNC:-}" = 1 ]; then exit 101; fi
if [ -n "${MOCK_FAIL_SYNC_MATCH:-}" ] && [ -f "$MOCK_STATE/last-published" ]; then
  last_published=$(cat "$MOCK_STATE/last-published")
  if [[ "$last_published" == *"$MOCK_FAIL_SYNC_MATCH"* ]]; then exit 101; fi
fi
exit 0
SH
  cat > "$bin/chmod" <<'SH'
#!/usr/bin/env bash
set -eu
for arg in "$@"; do
  if [ -n "${MOCK_FAIL_CHMOD_MATCH:-}" ] && [[ "$arg" == *"$MOCK_FAIL_CHMOD_MATCH"* ]]; then exit 102; fi
done
exec "$MOCK_REAL_CHMOD" "$@"
SH
  cat > "$bin/ln" <<'SH'
#!/usr/bin/env bash
set -eu
for arg in "$@"; do
  if [ -n "${MOCK_FAIL_LN_MATCH:-}" ] && [[ "$arg" == *"$MOCK_FAIL_LN_MATCH"* ]]; then exit 103; fi
done
destination=${@: -1}
"$MOCK_REAL_LN" "$@"
printf '%s\n' "$destination" > "$MOCK_STATE/last-published"
SH
  cat > "$bin/nc" <<'SH'
#!/usr/bin/env bash
exit 0
SH
  cat > "$bin/curl" <<'SH'
#!/usr/bin/env bash
printf '401 0'
SH
  cat > "$bin/date" <<'SH'
#!/usr/bin/env bash
if [ "${1:-}" = -u ] && [ "${2:-}" = +%Y%m%dT%H%M%SZ ]; then printf '%s\n' "$MOCK_STAMP"; else exec "$MOCK_REAL_DATE" "$@"; fi
SH
  chmod 700 "$bin/git" "$bin/python3" "$bin/docker" "$bin/flock" "$bin/sync" "$bin/chmod" "$bin/ln" "$bin/nc" "$bin/curl" "$bin/date"
}

write_compose_fixture() {
  local checkout=$1 scenario=$2 base_config=$3 target_config=$4
  jq -nc '{name:"fixture",services:{telegramd:{stop_grace_period:"2m0s",environment:{TG_SYNTHETIC_FLAG:"fixture",TG_BLOB_DIR:"/var/lib/telegramd-blobs"},ports:[{target:2443,published:"2443",host_ip:"127.0.0.1",protocol:"tcp",mode:"host"},{target:2444,published:"2444",host_ip:"127.0.0.1",protocol:"tcp",mode:"host"}],volumes:[{type:"volume",source:"identity",target:"/var/lib/telegramd",read_only:false},{type:"volume",source:"tgblobs",target:"/var/lib/telegramd-blobs",read_only:false}],network_mode:"",networks:{telegram_server:{}}}},volumes:{tgblobs:{name:"fixture_tgblobs"},rustfsdata:{name:"fixture_rustfsdata"}}}' > "$base_config"
  if [ "$scenario" = config-drift ]; then
    jq -c --arg source "$checkout/.state/blob-mode" '.services.telegramd.environment.TG_REPLICA_COUNT="1" | .services.telegramd.environment.TG_CLIENT_ADDR_TRUST="socket" | .services.telegramd.environment.UNRELATED="changed" | .services.telegramd.volumes += [{type:"bind",source:$source,target:"/run/telegramd/blob-mode",read_only:true}]' "$base_config" > "$target_config"
  elif [ "$scenario" = missing-mode-mount ]; then
    jq -c '.services.telegramd.environment.TG_REPLICA_COUNT="1" | .services.telegramd.environment.TG_CLIENT_ADDR_TRUST="socket"' "$base_config" > "$target_config"
  elif [ "$scenario" = wrong-blob-backend ]; then
    jq -c --arg source "$checkout/.state/blob-mode" '.services.telegramd.environment.TG_BLOB_DIR="/tmp/unmounted-blobs" | .services.telegramd.environment.TG_REPLICA_COUNT="1" | .services.telegramd.environment.TG_CLIENT_ADDR_TRUST="socket" | .services.telegramd.volumes += [{type:"bind",source:$source,target:"/run/telegramd/blob-mode",read_only:true}]' "$base_config" > "$target_config"
  elif [ "$scenario" = missing-proxy-mode-mount ]; then
    jq -c --arg source "$checkout/.state/blob-mode" '.services.telegramd.environment.TG_REPLICA_COUNT="1" | .services.telegramd.environment.TG_CLIENT_ADDR_TRUST="socket" | .services.telegramd.volumes += [{type:"bind",source:$source,target:"/run/telegramd/blob-mode",read_only:true}] | .services["telegramd-proxy"]={environment:{TG_BLOB_DIR:"/var/lib/telegramd-blobs"},volumes:[{type:"volume",source:"tgblobs",target:"/var/lib/telegramd-blobs",read_only:false}]}' "$base_config" > "$target_config"
  elif [ "$scenario" = wrong-tgblobs-volume ]; then
    jq -c --arg source "$checkout/.state/blob-mode" '.services.telegramd.environment.TG_REPLICA_COUNT="1" | .services.telegramd.environment.TG_CLIENT_ADDR_TRUST="socket" | .services.telegramd.volumes += [{type:"bind",source:$source,target:"/run/telegramd/blob-mode",read_only:true}] | .volumes.tgblobs.name="unexpected_tgblobs"' "$base_config" > "$target_config"
  elif [ "$scenario" = wrong-mode-source ]; then
    jq -c '.services.telegramd.environment.TG_REPLICA_COUNT="1" | .services.telegramd.environment.TG_CLIENT_ADDR_TRUST="socket" | .services.telegramd.volumes += [{type:"bind",source:"/tmp/untrusted-mode",target:"/run/telegramd/blob-mode",read_only:true}]' "$base_config" > "$target_config"
  else
    jq -c --arg source "$checkout/.state/blob-mode" '.services.telegramd.environment.TG_REPLICA_COUNT="1" | .services.telegramd.environment.TG_CLIENT_ADDR_TRUST="socket" | .services.telegramd.volumes += [{type:"bind",source:$source,target:"/run/telegramd/blob-mode",read_only:true}]' "$base_config" > "$target_config"
  fi
}

make_fixture() {
  local name=$1 scenario=$2 applied=${3:-} starting=${4:-} state bin checkout root stamp env_file override base_config target_config target_runtime
  printf 'fixture setup: %s scenario=%s\n' "$name" "$scenario" >&2
  FIXTURE_INDEX=$((FIXTURE_INDEX + 1))
  stamp=$(printf '20261006T12%02d00Z' "$FIXTURE_INDEX")
  state="$TMP/$name-state"
  bin="$TMP/$name-bin"
  checkout="$TMP/$name-checkout"
  root="/root/main1238-${TARGET_SHA:0:12}-$stamp"
  mkdir -m 700 "$state" "$checkout"
  mkdir -p -m 700 "$checkout/deploy/telegramd/rollout-runner" "$checkout/cmd" "$checkout/internal" "$checkout/components" "$checkout/utils"
  cp "$SCRIPT_DIR/rollout-runner.sh" "$SCRIPT_DIR/rollout-verifier.sh" \
    "$SCRIPT_DIR/schema-result-gate.sh" "$MODE_HELPER" "$checkout/deploy/telegramd/rollout-runner/"
  chmod 600 "$checkout/deploy/telegramd/rollout-runner/"*.sh "$checkout/deploy/telegramd/rollout-runner/blob-mode-state.py"
  target_runtime="$state/target-runtime"
  mkdir -m 700 "$target_runtime"
  cp "$SCRIPT_DIR/rollout-runner.sh" "$SCRIPT_DIR/rollout-verifier.sh" \
    "$SCRIPT_DIR/schema-result-gate.sh" "$MODE_HELPER" "$target_runtime/"
  chmod 600 "$target_runtime/"*.sh "$target_runtime/blob-mode-state.py"
  if [ "$scenario" = stale-schema-gate-pin ]; then
    sed -i 's/^readonly APPROVED_SCHEMA_GATE_SHA=.*/readonly APPROVED_SCHEMA_GATE_SHA=c74323f1885cad8c87c4115ebd6eb9b37b3f0f04c8586a37362bdae960b40395/' \
      "$checkout/deploy/telegramd/rollout-runner/rollout-runner.sh" \
      "$target_runtime/rollout-runner.sh"
  fi
  printf '%s\n' "$BASELINE_SHA" > "$state/head"
  printf '%s\n' "$TARGET_SHA" > "$state/origin"
  printf '%s\n' baseline > "$state/phase"
  printf '%s\n' "$BASE_ID" > "$state/telegramd"
  printf '%s\n' "$BASE_IMAGE" > "$state/tag"
  printf '%s\n' 'fixture baseline source' > "$checkout/tracked-source.txt"
  env_file="$checkout/.env"
  override="$checkout/docker-compose.override.yml"
  printf 'FIXTURE=synthetic-only\n' > "$env_file"
  printf 'override: synthetic\n' > "$override"
  chmod 600 "$env_file" "$override"
  base_config="$state/base-compose.json"
  target_config="$state/target-compose.json"
  write_compose_fixture "$checkout" "$scenario" "$base_config" "$target_config"
  : > "$TMP/$name-events"
  write_mock_commands "$bin"
  printf '%s\n' "$state" > "$TMP/$name-state-path"
  printf '%s\n' "$bin" > "$TMP/$name-bin-path"
  printf '%s\n' "$checkout" > "$TMP/$name-checkout-path"
  printf '%s\n' "$checkout/deploy/telegramd/rollout-runner" > "$TMP/$name-runtime-path"
  printf '%s\n' "$target_runtime" > "$TMP/$name-target-runtime-path"
  printf '%s\n' "$TARGET_SHA" > "$TMP/$name-target-sha-path"
  printf '%s\n' "$BASELINE_SHA" > "$TMP/$name-baseline-sha-path"
  printf '%s\n' "$root" > "$TMP/$name-root-path"
  printf '%s\n' "$stamp" > "$TMP/$name-stamp"
  printf '%s\n' "$scenario" > "$TMP/$name-scenario"
  if [ -n "$applied" ]; then printf '%s\n' "$applied" > "$TMP/$name-applied-revisions"; fi
  if [ -n "$starting" ]; then printf '%s\n' "$starting" > "$TMP/$name-start-revisions"; fi
  if [ "$scenario" = ambiguous-state ]; then
    mkdir -m 700 -p "$checkout/.state/blob-mode"
    printf '%s\n' '{"not":"a published authority"}' > "$checkout/.state/blob-mode/mode.json"
    chmod 600 "$checkout/.state/blob-mode/mode.json"
  elif [ "$scenario" = abandoned-initialization ] || [ "$scenario" = abandoned-forbidden-override ]; then
    mkdir -m 700 -p "$checkout/.state/blob-mode/journal"
    printf '%s\n' 'incomplete pre-publication record' > "$checkout/.state/blob-mode/journal/.tmp-00000000-0000-4000-8000-000000000099"
    chmod 600 "$checkout/.state/blob-mode/journal/.tmp-00000000-0000-4000-8000-000000000099"
  fi
  if [ "$scenario" = forbidden-override ] || [ "$scenario" = abandoned-forbidden-override ]; then
    printf '%s\n' '# blob-mode mount override is forbidden' > "$override"
    chmod 600 "$override"
  elif [ "$scenario" = forbidden-blob-setting ]; then
    printf '%s\n' 'services:' '  telegramd:' '    environment:' '      TG_BLOB_S3_ENDPOINT: ""' > "$override"
    chmod 600 "$override"
  elif [ "$scenario" = forbidden-tgblobs-mount ]; then
    printf '%s\n' 'services:' '  telegramd:' '    volumes:' '      - tgblobs:/var/lib/telegramd-blobs:ro' > "$override"
    chmod 600 "$override"
  fi
}

git_for_fixture() {
  local source_root=$1
  shift
  git -c "safe.directory=$source_root" -c "safe.directory=$source_root/.git" "$@"
}

replace_migrations_tree() {
  local repo=$1 tree=$2 replacement_tree=$3 record path found=0
  local -a entries=()
  while IFS= read -r -d '' record; do
    path=${record#*$'\t'}
    if [ "$path" = migrations ]; then
      entries+=("040000 tree $replacement_tree"$'\t'migrations)
      found=$((found + 1))
    else
      entries+=("$record")
    fi
  done < <(git_for_fixture "$repo" -C "$repo" ls-tree -z "$tree")
  [ "$found" -eq 1 ] || { printf '%s\n' 'fixture tree has no unique migrations directory' >&2; return 1; }
  printf '%s\0' "${entries[@]}" | git_for_fixture "$repo" -C "$repo" mktree -z
}

make_real_git_fixture() {
  local name=$1 scenario=$2 migration_source=${3:-approved} source_root origin checkout runtime_dir state bin target_sha target_tree
  local baseline_tree baseline_sha target_commit index tracked_path author_header committer_header git_config source_git_dir
  local root stamp env_file override base_config target_config source_head_sha source_migrations_tree
  local immutable_migrations_tree target_migrations_tree
  source_root=$(cd "$SCRIPT_DIR/../../.." && pwd -P)
  source_head_sha=$(git_for_fixture "$source_root" -C "$source_root" rev-parse HEAD)
  target_sha=$source_head_sha
  target_tree=$(git_for_fixture "$source_root" -C "$source_root" rev-parse "$source_head_sha^{tree}")
  source_migrations_tree=$(git_for_fixture "$source_root" -C "$source_root" rev-parse "$source_head_sha:migrations")
  immutable_migrations_tree=$(git_for_fixture "$source_root" -C "$source_root" rev-parse "$IMMUTABLE_MIGRATION_SOURCE_COMMIT:migrations")
  case "$migration_source" in
    approved)
      target_tree=$(replace_migrations_tree "$source_root" "$target_tree" "$immutable_migrations_tree")
      target_migrations_tree=$immutable_migrations_tree
      ;;
    current-head)
      target_migrations_tree=$source_migrations_tree
      ;;
    *)
      printf 'unknown real-git migration fixture source: %s\n' "$migration_source" >&2
      return 1
      ;;
  esac
  origin="$TMP/$name-origin.git"
  checkout="$TMP/$name-checkout"
  runtime_dir="$TMP/$name-runtime"
  state="$TMP/$name-state"
  bin="$TMP/$name-bin"
  index="$TMP/$name-index"
  git_config="$TMP/$name-gitconfig"
  FIXTURE_INDEX=$((FIXTURE_INDEX + 1))
  stamp=$(printf '20261007T16%02d00Z' "$FIXTURE_INDEX")
  mkdir -m 700 "$state"

  source_git_dir=$(git_for_fixture "$source_root" -C "$source_root" rev-parse --absolute-git-dir)
  printf '[safe]\n\tdirectory = %s\n\tdirectory = %s\n' "$source_root" "$source_git_dir" > "$git_config"
  chmod 600 "$git_config"
  GIT_CONFIG_GLOBAL="$git_config" git clone --shared "$source_root" "$origin" >/dev/null
  GIT_INDEX_FILE="$index" git -C "$origin" read-tree "$target_tree"
  while IFS= read -r tracked_path; do
    GIT_INDEX_FILE="$index" git -C "$origin" update-index --force-remove -- "$tracked_path"
  done < <(git_for_fixture "$source_root" -C "$source_root" ls-tree -r --name-only "$target_sha" -- deploy/telegramd/rollout-runner)
  baseline_tree=$(GIT_INDEX_FILE="$index" git -C "$origin" write-tree)
  rm -- "$index"
  author_header=$(git_for_fixture "$source_root" -C "$source_root" cat-file commit "$target_sha" | sed -n '/^author /p')
  committer_header=$(git_for_fixture "$source_root" -C "$source_root" cat-file commit "$target_sha" | sed -n '/^committer /p')
  baseline_sha=$(
    {
      printf 'tree %s\n%s\n%s\n\nfixture baseline without rollout runner\n' \
        "$baseline_tree" "$author_header" "$committer_header"
    } | git -C "$origin" hash-object -t commit -w --stdin
  )
  target_commit=$(
    {
      printf 'tree %s\nparent %s\n%s\n%s\n\nfixture target with rollout runner\n' \
        "$target_tree" "$baseline_sha" "$author_header" "$committer_header"
    } | git -C "$origin" hash-object -t commit -w --stdin
  )
  root="/root/main1238-${target_commit:0:12}-$stamp"
  git -C "$origin" update-ref refs/heads/main "$target_commit"
  git -C "$origin" symbolic-ref HEAD refs/heads/main
  git clone --shared --branch main "$origin" "$checkout" >/dev/null
  git -C "$checkout" update-ref refs/heads/main "$baseline_sha"
  git -C "$checkout" reset --hard "$baseline_sha" >/dev/null
  mkdir -m 700 "$runtime_dir"
  cp "$SCRIPT_DIR/rollout-runner.sh" "$SCRIPT_DIR/rollout-verifier.sh" \
    "$SCRIPT_DIR/schema-result-gate.sh" "$MODE_HELPER" "$runtime_dir/"
  chmod 600 "$runtime_dir/"*.sh "$runtime_dir/blob-mode-state.py"
  if [ "$scenario" = source-mismatch ]; then
    printf '%s\n' '# fixture source mismatch' >> "$runtime_dir/rollout-runner.sh"
  fi

  printf '%s\n' "$baseline_sha" > "$state/head"
  printf '%s\n' "$target_commit" > "$state/origin"
  printf '%s\n' baseline > "$state/phase"
  printf '%s\n' "$BASE_ID" > "$state/telegramd"
  printf '%s\n' "$BASE_IMAGE" > "$state/tag"
  env_file="$checkout/.env"
  override="$checkout/docker-compose.override.yml"
  printf 'FIXTURE=synthetic-only\n' > "$env_file"
  printf 'override: synthetic\n' > "$override"
  chmod 600 "$env_file" "$override"
  base_config="$state/base-compose.json"
  target_config="$state/target-compose.json"
  write_compose_fixture "$checkout" "$scenario" "$base_config" "$target_config"
  : > "$TMP/$name-events"
  write_mock_commands "$bin"
  rm -- "$bin/git"
  printf '%s\n' "$state" > "$TMP/$name-state-path"
  printf '%s\n' "$bin" > "$TMP/$name-bin-path"
  printf '%s\n' "$checkout" > "$TMP/$name-checkout-path"
  printf '%s\n' "$runtime_dir" > "$TMP/$name-runtime-path"
  printf '%s\n' "$runtime_dir" > "$TMP/$name-target-runtime-path"
  printf '%s\n' "$target_commit" > "$TMP/$name-target-sha-path"
  printf '%s\n' "$source_head_sha" > "$TMP/$name-source-head-sha-path"
  printf '%s\n' "$source_migrations_tree" > "$TMP/$name-source-head-migrations-tree-path"
  printf '%s\n' "$immutable_migrations_tree" > "$TMP/$name-immutable-migrations-tree-path"
  printf '%s\n' "$target_migrations_tree" > "$TMP/$name-target-migrations-tree-path"
  printf '%s\n' "$baseline_sha" > "$TMP/$name-baseline-sha-path"
  printf '%s\n' "$(command -v git)" > "$TMP/$name-real-git-path"
  printf '%s\n' "$root" > "$TMP/$name-root-path"
  printf '%s\n' "$stamp" > "$TMP/$name-stamp"
  printf '%s\n' "$scenario" > "$TMP/$name-scenario"
}

seed_real_git_authority() {
  local name=$1 state bin checkout target_sha baseline_sha lock baseline_containers current_containers
  local baseline_compose target_compose
  state=$(cat "$TMP/$name-state-path")
  bin=$(cat "$TMP/$name-bin-path")
  checkout=$(cat "$TMP/$name-checkout-path")
  target_sha=$(cat "$TMP/$name-target-sha-path")
  baseline_sha=$(cat "$TMP/$name-baseline-sha-path")
  lock="$TMP/$name-seed.lock"
  baseline_containers="$state/baseline-containers.json"
  current_containers="$state/current-containers.json"
  baseline_compose="$state/baseline-compose-inventory.json"
  target_compose="$state/target-compose-inventory.json"

  "$REAL_PYTHON3" "$MODE_HELPER" compose --checkout "$checkout" \
    < "$state/base-compose.json" > "$baseline_compose"
  "$REAL_PYTHON3" "$MODE_HELPER" compose --checkout "$checkout" \
    < "$state/target-compose.json" > "$target_compose"
  env PATH="$bin:$PATH" MOCK_STATE="$state" MOCK_EVENTS="$TMP/$name-events" \
    MOCK_CHECKOUT="$checkout" MOCK_SCENARIO=success MOCK_BASE_ID="$BASE_ID" \
    MOCK_TARGET_ID="$TARGET_ID" MOCK_REPLACEMENT_ID="$TARGET_ID" \
    MOCK_ROLLBACK_ID="$ROLLBACK_ID" MOCK_POSTGRES_ID="$POSTGRES_ID" \
    MOCK_MIGRATE_ID="$MIGRATE_ID" MOCK_BASE_IMAGE="$BASE_IMAGE" \
    MOCK_ACTUAL_TARGET_IMAGE="$BUILT_IMAGE" MOCK_POSTGRES_IMAGE="$POSTGRES_IMAGE" \
    "$bin/docker" inspect "$BASE_ID" |
      "$REAL_PYTHON3" "$MODE_HELPER" containers --checkout "$checkout" > "$baseline_containers"
  cp -- "$baseline_containers" "$current_containers"
  (
    exec 9>"$lock"
    "$REAL_FLOCK" -x 9
    env PATH="$bin:$PATH" MOCK_STATE="$state" MOCK_EVENTS="$TMP/$name-events" \
      MOCK_CHECKOUT="$checkout" MOCK_SCENARIO=success MOCK_BASE_ID="$BASE_ID" \
      MOCK_TARGET_ID="$TARGET_ID" MOCK_REPLACEMENT_ID="$TARGET_ID" \
      MOCK_ROLLBACK_ID="$ROLLBACK_ID" MOCK_POSTGRES_ID="$POSTGRES_ID" \
      MOCK_MIGRATE_ID="$MIGRATE_ID" MOCK_BASE_IMAGE="$BASE_IMAGE" \
      MOCK_ACTUAL_TARGET_IMAGE="$BUILT_IMAGE" MOCK_POSTGRES_IMAGE="$POSTGRES_IMAGE" \
      "$REAL_PYTHON3" "$MODE_HELPER" initialize-local \
        --state-dir "$checkout/.state/blob-mode" --report-root /root \
        --baseline-containers "$baseline_containers" --current-containers "$current_containers" \
        --baseline-compose "$baseline_compose" --target-compose "$target_compose" \
        --override "$checkout/docker-compose.override.yml" --checkout "$checkout" \
        --target-sha "$target_sha" --baseline-sha "$baseline_sha" --lock-path "$lock"
  )
  printf '20261005000060 20261005000061 20261006000062\n' > "$TMP/$name-start-revisions"
  local transition report
  transition=$(jq -er '.transition_id' "$checkout/.state/blob-mode/mode.json")
  report="/root/telegramd-blob-mode-report-$transition.json"
  [ -f "$report" ] && [ -f "$checkout/.state/blob-mode/journal/0000000001.json" ] || return 1
  printf '%s\n' "$report" > "$TMP/$name-report-path"
}

run_fixture() {
  local name=$1 capture=${2:-built} fail_sync=${3:-0} chmod_match=${4:-} ready=${5:-2} ln_match=${6:-} sync_match=${7:-} action=${8:-initialize-local}
  local state bin checkout root stamp scenario status require_marker=0 runner runtime_dir target_runtime target_sha baseline_sha real_git applied_revisions start_revisions compose_file
  local -a runner_args=()
  local replacement_id=$TARGET_ID
  state=$(cat "$TMP/$name-state-path")
  if [ -f "$state/replacement-id" ]; then replacement_id=$(cat "$state/replacement-id"); fi
  bin=$(cat "$TMP/$name-bin-path")
  checkout=$(cat "$TMP/$name-checkout-path")
  runtime_dir=$(cat "$TMP/$name-runtime-path")
  target_runtime=$(cat "$TMP/$name-target-runtime-path")
  target_sha=$(cat "$TMP/$name-target-sha-path")
  baseline_sha=$(cat "$TMP/$name-baseline-sha-path")
  real_git=$(cat "$TMP/$name-real-git-path" 2>/dev/null || true)
  root=$(cat "$TMP/$name-root-path")
  stamp=$(cat "$TMP/$name-stamp")
  scenario=$(cat "$TMP/$name-scenario")
  applied_revisions=$(cat "$TMP/$name-applied-revisions" 2>/dev/null || true)
  start_revisions=$(cat "$TMP/$name-start-revisions" 2>/dev/null || printf '20261005000060 20261005000061 20261006000062')
  compose_file='docker-compose.yml:docker-compose.override.yml:docker-compose.local-blobs.yml'
  if [ "$scenario" = compose-file-omits-override ]; then
    compose_file='docker-compose.yml:docker-compose.local-blobs.yml'
  fi
  runner="$runtime_dir/rollout-runner.sh"
  case "$scenario" in old-target-image|config-drift|readiness-timeout|logs-failed|runtime-target-exited) require_marker=1 ;; esac
  [ "$name" = marker-write-failed ] && require_marker=0
  [ -n "$chmod_match" ] && require_marker=1
  if [ "$action" = reconcile ]; then
    runner_args=("$action" "$target_sha")
  else
    runner_args=("$action" "$target_sha" "$baseline_sha")
  fi
  printf 'fixture runner: %s action=%s\n' "$name" "$action" >&2
  set +e
  (cd "$checkout" && timeout --signal=TERM --kill-after=5s 180s env PATH="$bin:$PATH" \
    MOCK_STATE="$state" MOCK_EVENTS="$TMP/$name-events" MOCK_SCENARIO="$scenario" \
    MOCK_APPLIED_REVISIONS="$applied_revisions" MOCK_START_REVISIONS="$start_revisions" \
    MOCK_APPROVED_REVISIONS="${APPROVED_REVISIONS[*]}" MOCK_APPROVED_REVISION_HASHES="${APPROVED_REVISION_HASHES[*]}" \
    MOCK_MIGRATIONS_DIR="$MIGRATIONS_DIR" \
    MOCK_CHECKOUT="$checkout" MOCK_TARGET_SHA="$target_sha" MOCK_TARGET_RUNTIME_DIR="$target_runtime" MOCK_REAL_GIT="$real_git" \
    MOCK_BASE_ID="$BASE_ID" MOCK_TARGET_ID="$TARGET_ID" MOCK_REPLACEMENT_ID="$replacement_id" MOCK_ROLLBACK_ID="$ROLLBACK_ID" \
    MOCK_POSTGRES_ID="$POSTGRES_ID" MOCK_MIGRATE_ID="$MIGRATE_ID" \
    MOCK_BASE_IMAGE="$BASE_IMAGE" MOCK_BUILT_IMAGE="$BUILT_IMAGE" MOCK_ACTUAL_TARGET_IMAGE="$BUILT_IMAGE" MOCK_POSTGRES_IMAGE="$POSTGRES_IMAGE" \
    MOCK_CAPTURE_IMAGE="$capture" MOCK_FAIL_SYNC="$fail_sync" MOCK_FAIL_CHMOD_MATCH="$chmod_match" MOCK_FAIL_LN_MATCH="$ln_match" MOCK_FAIL_SYNC_MATCH="$sync_match" \
    MOCK_REQUIRE_FAILURE_MARKER="$require_marker" MOCK_EVIDENCE_ROOT="$root" \
    MOCK_REAL_PYTHON3="$(command -v python3)" MOCK_REAL_CHMOD="$(command -v chmod)" MOCK_REAL_LN="$(command -v ln)" MOCK_REAL_DATE="$(command -v date)" MOCK_STAMP="$stamp" \
    ROLLOUT_RUNNER_TEST_MODE=1 ROLLOUT_RUNNER_CHECKOUT="$checkout" ROLLOUT_RUNNER_EVIDENCE_ROOT=/root \
    ROLLOUT_RUNNER_TEST_CHECKOUT="$checkout" \
    ROLLOUT_RUNNER_LOCK_PATH="$TMP/$name.lock" ROLLOUT_RUNNER_ENV_FILE="$checkout/.env" \
    ROLLOUT_RUNNER_TEST_SOURCE_DIR="$runtime_dir" \
    ROLLOUT_RUNNER_OVERRIDE_FILE="$checkout/docker-compose.override.yml" ROLLOUT_RUNNER_READY_SECONDS="$ready" \
    COMPOSE_FILE="$compose_file" \
    bash "$runner" "${runner_args[@]}" >"$TMP/$name.stdout" 2>"$TMP/$name.stderr")
  status=$?
  set -e
  printf 'fixture runner finished: %s status=%s\n' "$name" "$status" >&2
  printf '%s' "$status"
}

assert_evidence_mode() {
  local root=$1 mode phase path
  for phase in baseline backup build target rollback; do
    while IFS= read -r -d '' path; do
      mode=$(stat -c %a "$path")
      if [ -d "$path" ] && [ "$mode" != 700 ]; then return 1; fi
      if [ -f "$path" ] && [ "$mode" != 600 ]; then return 1; fi
    done < <(find "$root.$phase" -print0)
  done
}

clear_fixture_phases() {
  local root=$1 phase
  for phase in baseline backup build target rollback; do
    rm -rf -- "$root.$phase"
  done
}

prepare_apply_fixture() {
  local name=$1 scenario=$2 status state root stamp
  make_fixture "$name" success || return 1
  status=$(run_fixture "$name") || return 1
  [ "$status" = 0 ] || return 1
  state=$(cat "$TMP/$name-state-path")
  root=$(cat "$TMP/$name-root-path")
  stamp=$(cat "$TMP/$name-stamp")
  clear_fixture_phases "$root" || return 1
  : > "$TMP/$name-events" || return 1
  cp -- "$state/target-compose.json" "$state/base-compose.json" || return 1
  printf '%s\n' "$APPLY_ID" > "$state/replacement-id" || return 1
  printf '%s\n' "$TARGET_SHA" > "$state/head" || return 1
  printf '%s\n' "$APPLY_TARGET_SHA" > "$state/origin" || return 1
  printf '%s\n' baseline > "$state/phase" || return 1
  printf '%s\n' "$APPLY_TARGET_SHA" > "$TMP/$name-target-sha-path" || return 1
  printf '%s\n' "$TARGET_SHA" > "$TMP/$name-baseline-sha-path" || return 1
  printf '%s\n' "$scenario" > "$TMP/$name-scenario" || return 1
  printf '%s\n' "/root/main1238-${APPLY_TARGET_SHA:0:12}-$stamp" > "$TMP/$name-root-path" || return 1
}

authority_fingerprint() {
  local state_dir=$1 report=$2
  {
    find "$state_dir" -type f -print0 | sort -z | xargs -0 sha256sum
    sha256sum "$report"
  } | sha256sum | awk '{print $1}'
}

checkout_fingerprint() {
  local checkout=$1
  find "$checkout" -path "$checkout/.git" -prune -o -type f -print0 |
    sort -z | xargs -0 sha256sum | sha256sum | awk '{print $1}'
}

capture_mock_revision_rows() {
  local name=$1 output=$2 state bin checkout start_revisions
  state=$(cat "$TMP/$name-state-path")
  bin=$(cat "$TMP/$name-bin-path")
  checkout=$(cat "$TMP/$name-checkout-path")
  start_revisions=$(cat "$TMP/$name-start-revisions")
  (
    cd "$checkout"
    env PATH="$bin:$PATH" MOCK_STATE="$state" MOCK_EVENTS="$TMP/$name-events" \
      MOCK_SCENARIO=$(cat "$TMP/$name-scenario") MOCK_START_REVISIONS="$start_revisions" \
      MOCK_APPROVED_REVISIONS="${APPROVED_REVISIONS[*]}" \
      MOCK_APPROVED_REVISION_HASHES="${APPROVED_REVISION_HASHES[*]}" \
      MOCK_CHECKOUT="$checkout" MOCK_TARGET_SHA="$(cat "$TMP/$name-target-sha-path")" \
      MOCK_BASE_ID="$BASE_ID" MOCK_TARGET_ID="$TARGET_ID" MOCK_REPLACEMENT_ID="$TARGET_ID" \
      MOCK_ROLLBACK_ID="$ROLLBACK_ID" MOCK_POSTGRES_ID="$POSTGRES_ID" \
      MOCK_MIGRATE_ID="$MIGRATE_ID" MOCK_BASE_IMAGE="$BASE_IMAGE" \
      MOCK_BUILT_IMAGE="$BUILT_IMAGE" MOCK_ACTUAL_TARGET_IMAGE="$BUILT_IMAGE" \
      MOCK_POSTGRES_IMAGE="$POSTGRES_IMAGE" \
      "$bin/docker" compose exec -T postgres psql -X -A -t -v ON_ERROR_STOP=1 \
        -U postgres -d telegram -c 'SELECT revision_prefix_exact'
  ) > "$output"
}

show_fixture_failure() {
  local name=$1 status=$2 root
  root=$(cat "$TMP/$name-root-path")
  printf 'fixture_failure=%s status=%s\nfixture_stdout:\n' "$name" "$status" >&2
  cat "$TMP/$name.stdout" >&2
  printf 'fixture_stderr:\n' >&2
  cat "$TMP/$name.stderr" >&2
  if [ -f "$root.target/target-comparisons.tsv" ]; then
    printf 'target_comparisons:\n' >&2
    cat "$root.target/target-comparisons.tsv" >&2
  fi
  if [ -f "$root.rollback/rollback-equivalence.tsv" ]; then
    printf 'rollback_equivalence:\n' >&2
    cat "$root.rollback/rollback-equivalence.tsv" >&2
  fi
  printf 'fixture_events:\n' >&2
  cat "$TMP/$name-events" >&2
}

make_real_git_fixture real-git-source-mismatch source-mismatch
checkout=$(cat "$TMP/real-git-source-mismatch-checkout-path")
runtime_dir=$(cat "$TMP/real-git-source-mismatch-runtime-path")
target_sha=$(cat "$TMP/real-git-source-mismatch-target-sha-path")
baseline_sha=$(cat "$TMP/real-git-source-mismatch-baseline-sha-path")
root=$(cat "$TMP/real-git-source-mismatch-root-path")
if [ -z "$(git -C "$checkout" ls-tree "$baseline_sha" deploy/telegramd/rollout-runner)" ] && \
   [ -n "$(git -C "$checkout" ls-tree "$target_sha" deploy/telegramd/rollout-runner)" ]; then
  pass 'real-git baseline has no rollout runner while target tracks it'
else
  fail 'real-git fixture baseline and target shape'
fi
status=$(run_fixture real-git-source-mismatch)
no_evidence=1
for phase in baseline backup build target rollback; do
  [ ! -e "$root.$phase" ] || no_evidence=0
done
if [ "$status" != 0 ] && grep -q 'runtime copy differs from authorized target' "$TMP/real-git-source-mismatch.stderr" && \
   ! grep -Eq '^docker compose (build|up)|^docker compose exec -T postgres pg_dump' "$TMP/real-git-source-mismatch-events" && \
   [ "$no_evidence" = 1 ] && \
   [ "$(git -C "$checkout" rev-parse HEAD)" = "$baseline_sha" ]; then
  pass 'real git rejects a mismatched external runner before backup'
else
  printf 'real_git_mismatch_status=%s\nreal_git_mismatch_stderr=%s\nreal_git_mismatch_events=%s\nreal_git_mismatch_head=%s\nreal_git_mismatch_baseline=%s\nreal_git_mismatch_evidence=%s\n' \
    "$status" "$(cat "$TMP/real-git-source-mismatch.stderr")" \
    "$(cat "$TMP/real-git-source-mismatch-events")" \
    "$(git -C "$checkout" rev-parse HEAD)" "$baseline_sha" "$no_evidence"
  fail 'real-git external runner hash check'
fi

make_real_git_fixture real-git-no-runner success
checkout=$(cat "$TMP/real-git-no-runner-checkout-path")
target_sha=$(cat "$TMP/real-git-no-runner-target-sha-path")
baseline_sha=$(cat "$TMP/real-git-no-runner-baseline-sha-path")
runtime_dir=$(cat "$TMP/real-git-no-runner-runtime-path")
status=$(run_fixture real-git-no-runner)
if [ "$status" = 0 ] && grep -q 'rollout=verified' "$TMP/real-git-no-runner.stdout" && \
   [ "$(git -C "$checkout" rev-parse HEAD)" = "$target_sha" ] && \
   [ -f "$checkout/deploy/telegramd/rollout-runner/rollout-runner.sh" ] && \
   grep -q '^docker compose exec -T postgres pg_dump' "$TMP/real-git-no-runner-events" && \
   grep -q '^docker compose build' "$TMP/real-git-no-runner-events" && \
   grep -q '^docker compose up -d$' "$TMP/real-git-no-runner-events" && \
   ! grep -q 'untracked working tree files would be overwritten' "$TMP/real-git-no-runner.stderr"; then
  pass 'real git fast-forwards a no-runner baseline using an external pinned source'
else
  printf 'real_git_status=%s\nreal_git_stderr=%s\nreal_git_events=%s\n' \
    "$status" "$(cat "$TMP/real-git-no-runner.stderr")" "$(cat "$TMP/real-git-no-runner-events")"
  fail 'real-git fast-forward from baseline without runner'
fi

make_real_git_fixture real-git-current-head success current-head
checkout=$(cat "$TMP/real-git-current-head-checkout-path")
target_sha=$(cat "$TMP/real-git-current-head-target-sha-path")
baseline_sha=$(cat "$TMP/real-git-current-head-baseline-sha-path")
root=$(cat "$TMP/real-git-current-head-root-path")
source_migrations_tree=$(cat "$TMP/real-git-current-head-source-head-migrations-tree-path")
immutable_migrations_tree=$(cat "$TMP/real-git-current-head-immutable-migrations-tree-path")
target_migrations_tree=$(cat "$TMP/real-git-current-head-target-migrations-tree-path")
if [ "$source_migrations_tree" != "$immutable_migrations_tree" ] && \
   [ "$target_migrations_tree" = "$source_migrations_tree" ] && \
   [ "$(git -C "$checkout" rev-parse "$target_sha:migrations")" = "$source_migrations_tree" ]; then
  pass 'real-git current-HEAD target uses a migration tree different from immutable #484'
else
  fail 'real-git current-HEAD migration tree differs from immutable #484'
fi
seed_real_git_authority real-git-current-head
state=$(cat "$TMP/real-git-current-head-state-path")
report=$(cat "$TMP/real-git-current-head-report-path")
before_checkout=$(checkout_fingerprint "$checkout")
before_authority=$(authority_fingerprint "$checkout/.state/blob-mode" "$report")
before_tag=$(cat "$state/tag")
capture_mock_revision_rows real-git-current-head "$TMP/real-git-current-head-db-before.tsv"
: > "$TMP/real-git-current-head-events"
status=$(run_fixture real-git-current-head built 0 '' 2 '' '' apply)
capture_mock_revision_rows real-git-current-head "$TMP/real-git-current-head-db-after.tsv"
after_checkout=$(checkout_fingerprint "$checkout")
after_authority=$(authority_fingerprint "$checkout/.state/blob-mode" "$report")
if [ "$status" != 0 ] && \
   grep -q 'approved migration precheck rejected; baseline state was left unchanged' "$TMP/real-git-current-head.stderr" && \
   grep -q '^precheck_result=reject$' "$root.target/schema-precheck.tsv" && \
   grep -q '^precheck_migration_tree_exact=false$' "$root.target/schema-precheck.tsv" && \
   [ "$(git -C "$checkout" rev-parse HEAD)" = "$baseline_sha" ] && \
   [ "$before_checkout" = "$after_checkout" ] && \
   [ "$before_authority" = "$after_authority" ] && \
   [ "$before_tag" = "$(cat "$state/tag")" ] && \
   cmp -s "$TMP/real-git-current-head-db-before.tsv" "$TMP/real-git-current-head-db-after.tsv" && \
   ! grep -Eq '^docker compose exec -T postgres pg_dump|^docker compose (build|up)|^docker compose run .*migrate' \
     "$TMP/real-git-current-head-events"; then
  pass 'real-git current-HEAD precheck rejects before side effects and preserves checkout, tag, authority, and DB revisions'
else
  printf 'current_head_status=%s checkout_before=%s checkout_after=%s authority_before=%s authority_after=%s tag_before=%s tag_after=%s\n' \
    "$status" "$before_checkout" "$after_checkout" "$before_authority" "$after_authority" \
    "$before_tag" "$(cat "$state/tag")" >&2
  printf 'current_head_runner_stderr:\n' >&2
  cat "$TMP/real-git-current-head.stderr" >&2
  [ ! -f "$root.target/schema-precheck.tsv" ] || cat "$root.target/schema-precheck.tsv" >&2
  printf 'current_head_events:\n' >&2
  cat "$TMP/real-git-current-head-events" >&2
  fail 'real-git current-HEAD precheck rejection ordering or state preservation'
fi

make_fixture success built
status=$(run_fixture success)
root=$(cat "$TMP/success-root-path")
if [ "$status" = 0 ] && grep -q 'rollout=verified' "$TMP/success.stdout" && grep -q 'flock -x 9' "$TMP/success-events" && \
   awk 'index($0,"docker image inspect telegramd:local") {image=NR} $0=="docker compose up -d" {up=NR} END{exit !(image>0 && up>image)}' "$TMP/success-events" && \
   grep -q 'restore network none tmpfs only' "$TMP/success-events" && \
   find "$root".* -name built-image.identity -exec grep -q "source_sha=$TARGET_SHA image_id=$BUILT_IMAGE" {} \; && \
   assert_evidence_mode "$root"; then
  pass 'built image captured after build, bound to target SHA, and compared before acceptance'
else
  fail 'built image capture, provenance binding, and target acceptance'
fi
checkout=$(cat "$TMP/success-checkout-path")
root=$(cat "$TMP/success-root-path")
transition=$(jq -er '.transition_id' "$checkout/.state/blob-mode/mode.json")
mode_report="/root/telegramd-blob-mode-report-$transition.json"
if cmp -s "$checkout/.state/blob-mode/mode.json" "$checkout/.state/blob-mode/journal/0000000001.json" && \
   [ "$(stat -c %a "$mode_report")" = 600 ] && [ "$(stat -c %u "$mode_report")" = 0 ] && \
   [ "$(jq -r '.evidence.report_sha256' "$checkout/.state/blob-mode/mode.json")" = "$(sha256sum "$mode_report" | awk '{print $1}')" ] && \
   jq -e --arg source "$checkout/.state/blob-mode" '
     all(.services[]; .blob_mode_mounts == [{type:"bind",source:$source,target:"/run/telegramd/blob-mode",read_only:true}]
       and .tgblobs_mounts == [{type:"volume",source:"fixture_tgblobs",target:"/var/lib/telegramd-blobs",read_only:false}])
   ' "$root.target/target-blob-compose.json" >/dev/null && \
   jq -e --arg source "$checkout/.state/blob-mode" '
     all(.containers[]; .mode_mounts == [{type:"bind",source:$source,target:"/run/telegramd/blob-mode",read_only:true}]
       and .tgblobs_mounts == [{type:"volume",name:"fixture_tgblobs",target:"/var/lib/telegramd-blobs",rw:true}])
   ' "$root.target/target-blob-containers.json" >/dev/null; then
  pass 'initial-local report is private and hash-bound, and the active directory bind stays read-only'
else
  fail 'durable report binding or read-only authority mount'
fi
phase_paths=$(find /root -mindepth 1 -maxdepth 1 -type d -path "$root.*" -printf '%f\n' | sort | wc -l | tr -d ' ')
if [ "$phase_paths" = 5 ]; then pass 'baseline, backup, build, target, and rollback evidence paths are distinct'; else fail 'distinct immutable phase evidence paths'; fi

make_fixture apply-without-authority success
status=$(run_fixture apply-without-authority built 0 '' 2 '' '' apply)
state=$(cat "$TMP/apply-without-authority-state-path")
if [ "$status" != 0 ] && [ ! -e "$(cat "$TMP/apply-without-authority-checkout-path")/.state/blob-mode" ] && \
   ! grep -Eq '^docker compose (build|up|stop|down)( |$)' "$TMP/apply-without-authority-events" && \
   [ "$(cat "$state/head")" = "$BASELINE_SHA" ] && [ "$(cat "$state/telegramd")" = "$BASE_ID" ] && \
   grep -q 'blob authority rejected' "$TMP/apply-without-authority.stderr"; then
  pass 'ordinary apply requires an existing authority and leaves the live baseline untouched'
else
  fail 'ordinary apply without authority must fail before replacement'
fi

if prepare_apply_fixture apply-same-backend success; then
  state=$(cat "$TMP/apply-same-backend-state-path")
  live_id=$(cat "$state/telegramd")
  checkout=$(cat "$TMP/apply-same-backend-checkout-path")
  transition=$(jq -er '.transition_id' "$checkout/.state/blob-mode/mode.json")
  report="/root/telegramd-blob-mode-report-$transition.json"
  authority_before=$(authority_fingerprint "$checkout/.state/blob-mode" "$report")
  status=$(run_fixture apply-same-backend built 0 '' 2 '' '' apply)
  authority_after=$(authority_fingerprint "$checkout/.state/blob-mode" "$report")
  if [ "$status" = 0 ] && grep -q 'rollout=verified' "$TMP/apply-same-backend.stdout" && \
     awk '$0 == "docker compose build -q telegramd" {build++; build_line=NR} $0 == "docker compose up -d" {up++; up_line=NR} END {exit !(build == 1 && up == 1 && build_line < up_line)}' \
       "$TMP/apply-same-backend-events" && \
     [ "$(cat "$state/head")" = "$APPLY_TARGET_SHA" ] && \
     [ "$(cat "$state/telegramd")" = "$APPLY_ID" ] && \
     [ "$(cat "$state/telegramd")" != "$live_id" ] && \
     [ "$authority_before" = "$authority_after" ]; then
    pass 'ordinary apply with valid same-backend authority builds and replaces the service'
  else
    show_fixture_failure apply-same-backend "$status"
    fail 'same-backend apply must validate authority before its single build and up'
  fi
else
  fail 'same-backend apply fixture requires a valid initialized authority'
fi

if prepare_apply_fixture apply-backend-flip runtime-backend-mismatch; then
  state=$(cat "$TMP/apply-backend-flip-state-path")
  checkout=$(cat "$TMP/apply-backend-flip-checkout-path")
  live_id=$(cat "$state/telegramd")
  transition=$(jq -er '.transition_id' "$checkout/.state/blob-mode/mode.json")
  report="/root/telegramd-blob-mode-report-$transition.json"
  authority_before=$(authority_fingerprint "$checkout/.state/blob-mode" "$report")
  status=$(run_fixture apply-backend-flip built 0 '' 2 '' '' apply)
  authority_after=$(authority_fingerprint "$checkout/.state/blob-mode" "$report")
  if [ "$status" != 0 ] && grep -q 'running-backend-mismatch' "$TMP/apply-backend-flip.stderr" && \
     ! grep -Eq '^docker (stop|kill)( |$)|^docker compose (build|up|stop|down)( |$)' "$TMP/apply-backend-flip-events" && \
     [ "$(cat "$state/head")" = "$TARGET_SHA" ] && \
     [ "$(cat "$state/telegramd")" = "$live_id" ] && \
     [ "$authority_before" = "$authority_after" ]; then
    pass 'ordinary apply rejects a live backend flip without building or replacing the baseline'
  else
    show_fixture_failure apply-backend-flip "$status"
    fail 'backend-flip apply must preserve live containers and authority before build or up'
  fi
else
  fail 'backend-flip apply fixture requires a valid initialized authority'
fi

if prepare_apply_fixture apply-s3-render success; then
  state=$(cat "$TMP/apply-s3-render-state-path")
  checkout=$(cat "$TMP/apply-s3-render-checkout-path")
  live_id=$(cat "$state/telegramd")
  transition=$(jq -er '.transition_id' "$checkout/.state/blob-mode/mode.json")
  report="/root/telegramd-blob-mode-report-$transition.json"
  authority_before=$(authority_fingerprint "$checkout/.state/blob-mode" "$report")
  jq -c '
    .services.telegramd.environment.TG_BLOB_S3_ENDPOINT="https://objects.fixture.invalid" |
    .services.telegramd.environment.TG_BLOB_S3_BUCKET="fixture-bucket" |
    .services.telegramd.environment.TG_BLOB_S3_PREFIX="fixture/"
  ' "$state/target-compose.json" > "$state/s3-compose.json"
  mv -- "$state/s3-compose.json" "$state/target-compose.json"
  cp -- "$state/target-compose.json" "$state/base-compose.json"
  status=$(run_fixture apply-s3-render built 0 '' 2 '' '' apply)
  authority_after=$(authority_fingerprint "$checkout/.state/blob-mode" "$report")
  if [ "$status" != 0 ] && grep -q 'render-backend-mismatch' "$TMP/apply-s3-render.stderr" && \
     ! grep -Eq '^docker (stop|kill)( |$)|^docker compose (build|up|stop|down)( |$)' "$TMP/apply-s3-render-events" && \
     [ "$(cat "$state/head")" = "$TARGET_SHA" ] && \
     [ "$(cat "$state/telegramd")" = "$live_id" ] && \
     [ "$authority_before" = "$authority_after" ]; then
    pass 'ordinary apply rejects an S3 render against initial-local authority before replacing the live baseline'
  else
    show_fixture_failure apply-s3-render "$status"
    fail 'S3-render apply must preserve live containers and authority before build or up'
  fi
else
  fail 'S3-render apply fixture requires a valid initialized authority'
fi

make_fixture compose-file-omits-override compose-file-omits-override
status=$(run_fixture compose-file-omits-override)
checkout=$(cat "$TMP/compose-file-omits-override-checkout-path")
state=$(cat "$TMP/compose-file-omits-override-state-path")
root=$(cat "$TMP/compose-file-omits-override-root-path")
no_evidence=1
for phase in baseline backup build target rollback; do
  [ ! -e "$root.$phase" ] || no_evidence=0
done
if [ "$status" != 0 ] && grep -q 'COMPOSE_FILE omits the existing docker-compose.override.yml' \
   "$TMP/compose-file-omits-override.stderr" && [ ! -s "$TMP/compose-file-omits-override-events" ] && \
   [ ! -e "$checkout/.state/blob-mode" ] && [ "$(cat "$state/head")" = "$BASELINE_SHA" ] && \
   [ "$(cat "$state/telegramd")" = "$BASE_ID" ] && [ "$no_evidence" = 1 ]; then
  pass 'explicit Compose file lists must retain the existing override before any capture or start'
else
  fail 'missing Compose override must reject before publication, backup, or start'
fi

for rejected in missing-mode-mount missing-proxy-mode-mount wrong-tgblobs-volume wrong-mode-source wrong-blob-backend forbidden-override forbidden-blob-setting forbidden-tgblobs-mount; do
  make_fixture "blob-reject-$rejected" "$rejected"
  status=$(run_fixture "blob-reject-$rejected")
  state=$(cat "$TMP/blob-reject-$rejected-state-path")
  if [ "$status" != 0 ] && \
     ! grep -Eq '^docker compose (build|up|stop|down)( |$)' "$TMP/blob-reject-$rejected-events" && \
     [ ! -e "$(cat "$TMP/blob-reject-$rejected-checkout-path")/.state/blob-mode/mode.json" ] && \
     [ "$(cat "$state/head")" = "$BASELINE_SHA" ] && [ "$(cat "$state/telegramd")" = "$BASE_ID" ]; then
    pass "$rejected fails pre-replacement with no authority publication"
  else
    fail "$rejected must reject before build or replacement"
  fi
done

make_fixture ambiguous-state ambiguous-state
checkout=$(cat "$TMP/ambiguous-state-checkout-path")
state_before=$(sha256sum "$checkout/.state/blob-mode/mode.json" | awk '{print $1}')
status=$(run_fixture ambiguous-state)
state_after=$(sha256sum "$checkout/.state/blob-mode/mode.json" | awk '{print $1}')
if [ "$status" != 0 ] && [ "$state_before" = "$state_after" ] && \
   ! grep -Eq '^docker compose (build|up|stop|down)( |$)' "$TMP/ambiguous-state-events" && \
   [ "$(cat "$(cat "$TMP/ambiguous-state-state-path")/telegramd")" = "$BASE_ID" ] && \
   grep -q 'state-already-exists' "$TMP/ambiguous-state.stderr"; then
  pass 'ambiguous pre-existing authority rejects without changing its bytes or containers'
else
  fail 'ambiguous authority must remain byte-identical and running'
fi

make_fixture abandoned-initialization abandoned-initialization
status=$(run_fixture abandoned-initialization)
checkout=$(cat "$TMP/abandoned-initialization-checkout-path")
new_transition=$(jq -er '.transition_id' "$checkout/.state/blob-mode/mode.json" 2>/dev/null || true)
if [ "$status" = 0 ] && [ -n "$new_transition" ] && \
   [ "$new_transition" != 00000000-0000-4000-8000-000000000099 ] && \
   [ ! -e "$checkout/.state/blob-mode/journal/.tmp-00000000-0000-4000-8000-000000000099" ] && \
   [ -f "/root/telegramd-blob-mode-report-$new_transition.json" ]; then
  pass 'abandoned pre-publication staging is cleared and retry creates fresh transition evidence'
else
  fail 'abandoned initial publication retry'
fi

make_fixture abandoned-invalid-initialization abandoned-forbidden-override
checkout=$(cat "$TMP/abandoned-invalid-initialization-checkout-path")
staging="$checkout/.state/blob-mode/journal/.tmp-00000000-0000-4000-8000-000000000099"
staging_before=$(sha256sum "$staging" | awk '{print $1}')
status=$(run_fixture abandoned-invalid-initialization)
staging_after=$(sha256sum "$staging" | awk '{print $1}')
if [ "$status" != 0 ] && [ "$staging_before" = "$staging_after" ] && \
   [ -f "$staging" ] && [ ! -e "$checkout/.state/blob-mode/mode.json" ] && \
   ! grep -Eq '^docker compose (build|up|stop|down)( |$)' "$TMP/abandoned-invalid-initialization-events" && \
   [ "$(cat "$(cat "$TMP/abandoned-invalid-initialization-state-path")/telegramd")" = "$BASE_ID" ]; then
  pass 'invalid initialization leaves abandoned state bytes and running containers unchanged'
else
  fail 'invalid initialization must preserve pre-publication state and baseline containers'
fi

make_fixture interrupted-initial-publication interrupted-initial-publication
status=$(run_fixture interrupted-initial-publication)
state=$(cat "$TMP/interrupted-initial-publication-state-path")
checkout=$(cat "$TMP/interrupted-initial-publication-checkout-path")
root=$(cat "$TMP/interrupted-initial-publication-root-path")
authority="$checkout/.state/blob-mode"
journal="$authority/journal/0000000001.json"
transition=$(jq -er '.transition_id' "$journal" 2>/dev/null || true)
report="/root/telegramd-blob-mode-report-$transition.json"
if [ "$status" != 0 ] && [ "$(cat "$state/head")" = "$TARGET_SHA" ] && \
   [ -n "$transition" ] && [ -f "$journal" ] && [ ! -e "$authority/mode.json" ] && \
   [ -f "$report" ] && ! grep -Eq '^docker compose (build|up|stop|down)( |$)' "$TMP/interrupted-initial-publication-events" && \
   grep -q 'target checkout retained for reconcile' "$TMP/interrupted-initial-publication.stderr"; then
  pass 'committed initial-local journal keeps the reviewed target checked out for reconciliation without starting it'
else
  fail 'committed initial-local publication must retain target checkout and keep service untouched'
fi

if [ -f "$journal" ] && [ -n "$transition" ] && [ -f "$report" ]; then
clear_fixture_phases "$root"
: > "$TMP/interrupted-initial-publication-events"
printf '%s\n' "$TARGET_SHA" > "$TMP/interrupted-initial-publication-baseline-sha-path"
journal_before=$(sha256sum "$journal" "$report")
status=$(run_fixture interrupted-initial-publication built 0 '' 2 '' '' apply)
journal_after=$(sha256sum "$journal" "$report")
if [ "$status" != 0 ] && grep -q 'mode-head-mismatch' "$TMP/interrupted-initial-publication.stderr" && \
   [ "$journal_before" = "$journal_after" ] && \
   ! grep -Eq '^docker (stop|kill)( |$)|^docker compose (build|up|stop|down)( |$)' "$TMP/interrupted-initial-publication-events" && \
   [ "$(cat "$state/telegramd")" = "$BASE_ID" ] && [ "$(cat "$state/head")" = "$TARGET_SHA" ]; then
  pass 'apply rejects a committed journal without mode.json before stopping or replacing containers'
else
  fail 'apply must fail closed on a journal without mode.json before stopping containers'
fi

: > "$TMP/interrupted-initial-publication-events"
status=$(run_fixture interrupted-initial-publication built 0 '' 2 '' '' reconcile)
if [ "$status" = 0 ] && grep -q 'blob_mode=reconciled' "$TMP/interrupted-initial-publication.stdout" && \
   cmp -s "$authority/mode.json" "$journal" && [ ! -e "$authority/.mode.json.tmp-$transition" ]; then
  pass 'reconcile publishes mode.json from a matching synced report when no mode temporary exists'
else
  fail 'reconcile from a matching report without a mode temporary'
fi

if [ -f "$authority/mode.json" ] && cmp -s "$authority/mode.json" "$journal"; then
  rm -- "$authority/mode.json"
  cp -- "$journal" "$authority/.mode.json.tmp-$transition"
  chmod 644 -- "$authority/.mode.json.tmp-$transition"
  : > "$TMP/interrupted-initial-publication-events"
  status=$(run_fixture interrupted-initial-publication built 0 '' 2 '' '' reconcile)
  if [ "$status" = 0 ] && grep -q 'blob_mode=reconciled' "$TMP/interrupted-initial-publication.stdout" && \
     cmp -s "$authority/mode.json" "$journal" && [ ! -e "$authority/.mode.json.tmp-$transition" ]; then
    pass 'reconcile fsyncs and publishes a matching pre-existing mode temporary'
  else
    fail 'reconcile from a matching report with a mode temporary'
  fi
else
  fail 'reconcile from a matching report with a mode temporary'
fi

good_report="$TMP/interrupted-initial-publication-good-report.json"
cp -p -- "$report" "$good_report"
printf ' ' >> "$report"
state_before=$(authority_fingerprint "$authority" "$report")
: > "$TMP/interrupted-initial-publication-events"
status=$(run_fixture interrupted-initial-publication built 0 '' 2 '' '' reconcile)
state_after=$(authority_fingerprint "$authority" "$report")
if [ "$status" != 0 ] && grep -q 'report-digest' "$TMP/interrupted-initial-publication.stderr" && \
   [ "$state_before" = "$state_after" ]; then
  pass 'reconcile rejects a report digest mismatch without changing authority or report bytes'
else
  fail 'report digest mismatch must reject without changing authority or report bytes'
fi

cp -p -- "$good_report" "$report"
jq -c '.transition_id="00000000-0000-4000-8000-000000000099"' "$report" > "$TMP/interrupted-initial-publication-provenance-report.json"
cat "$TMP/interrupted-initial-publication-provenance-report.json" > "$report"
chmod 600 -- "$report"
report_digest=$(sha256sum "$report" | awk '{print $1}')
jq -c --arg digest "$report_digest" '.evidence.report_sha256=$digest' "$journal" > "$TMP/interrupted-initial-publication-provenance-journal.json"
cat "$TMP/interrupted-initial-publication-provenance-journal.json" > "$journal"
chmod 644 -- "$journal"
state_before=$(authority_fingerprint "$authority" "$report")
: > "$TMP/interrupted-initial-publication-events"
status=$(run_fixture interrupted-initial-publication built 0 '' 2 '' '' reconcile)
state_after=$(authority_fingerprint "$authority" "$report")
if [ "$status" != 0 ] && grep -q 'report-provenance' "$TMP/interrupted-initial-publication.stderr" && \
   [ "$state_before" = "$state_after" ]; then
  pass 'reconcile rejects a digest-matched report provenance mismatch without changing authority bytes'
else
  fail 'report provenance mismatch must reject without changing authority bytes'
fi
else
  fail 'interrupted initial publication did not leave a committed journal and matching report for reconciliation'
fi

make_fixture blob-report-tamper success
status=$(run_fixture blob-report-tamper)
checkout=$(cat "$TMP/blob-report-tamper-checkout-path")
root=$(cat "$TMP/blob-report-tamper-root-path")
bin=$(cat "$TMP/blob-report-tamper-bin-path")
transition=$(jq -er '.transition_id' "$checkout/.state/blob-mode/mode.json")
mode_report="/root/telegramd-blob-mode-report-$transition.json"
printf ' ' >> "$mode_report"
state_before=$(sha256sum "$checkout/.state/blob-mode/mode.json" "$checkout/.state/blob-mode/journal/0000000001.json")
if [ "$status" = 0 ]; then
  set +e
  printf 'fixture validation: blob-report-tamper\n' >&2
  real_python3=$(command -v python3)
  PATH="$bin:$PATH" MOCK_REAL_PYTHON3="$real_python3" MOCK_EVENTS="$TMP/blob-report-tamper-validation-events" \
    timeout --signal=TERM --kill-after=2s 15s bash -c 'python3 "$1" validate --state-dir "$2/.state/blob-mode" --report-root /root --containers "$3.target/target-blob-containers.json" --compose "$3.target/target-blob-compose.json" --override "$2/docker-compose.override.yml" --checkout "$2"' \
      _ "$MODE_HELPER" "$checkout" "$root" >"$TMP/blob-report-tamper-validation.stdout" 2>"$TMP/blob-report-tamper-validation.stderr"
  validation_status=$?
  set -e
  printf 'fixture validation finished: blob-report-tamper status=%s\n' "$validation_status" >&2
else
  validation_status=0
fi
state_after=$(sha256sum "$checkout/.state/blob-mode/mode.json" "$checkout/.state/blob-mode/journal/0000000001.json")
if [ "$status" = 0 ] && [ "$validation_status" != 0 ] && \
   [ "$state_before" = "$state_after" ] && grep -q 'report-digest' "$TMP/blob-report-tamper-validation.stderr" && \
   [ "$(cat "$(cat "$TMP/blob-report-tamper-state-path")/telegramd")" = "$TARGET_ID" ]; then
  pass 'tampered private report rejects validation without rewriting authority or replacing containers'
else
  fail 'report hash and state-byte preservation gate'
fi

make_fixture runtime-mode-unmounted runtime-mode-unmounted
status=$(run_fixture runtime-mode-unmounted)
root=$(cat "$TMP/runtime-mode-unmounted-root-path")
if [ "$status" != 0 ] && grep -q 'rollback=verified' "$TMP/runtime-mode-unmounted.stdout" && \
   grep -q '^docker compose up -d --no-build --no-deps telegramd$' "$TMP/runtime-mode-unmounted-events" && \
   [ -f "$root.target/target-blob-containers.json" ]; then
  pass 'unguarded target with matching storage authority rolls back to the inspected baseline'
else
  fail 'unguarded target rollback with matching storage authority'
fi

make_fixture runtime-target-exited runtime-target-exited
status=$(run_fixture runtime-target-exited)
root=$(cat "$TMP/runtime-target-exited-root-path")
state=$(cat "$TMP/runtime-target-exited-state-path")
if [ "$status" != 0 ] && jq -e '.state == "exited"' "$root.target/target.snapshot.json" >/dev/null && \
   jq -e '.containers == []' "$root.rollback/rollback-pre-up-blob-containers.json" >/dev/null && \
   grep -q 'rollback=verified' "$TMP/runtime-target-exited.stdout" && \
   grep -q '^docker compose up -d --no-build --no-deps telegramd$' "$TMP/runtime-target-exited-events" && \
   [ "$(cat "$state/telegramd")" = "$ROLLBACK_ID" ]; then
  pass 'exited target with empty serving inventory validates authority and restores the inspected baseline'
else
  fail 'exited target rollback with empty serving inventory'
fi

for runtime_mismatch in runtime-volume-mismatch runtime-backend-mismatch; do
  make_fixture "$runtime_mismatch" "$runtime_mismatch"
  status=$(run_fixture "$runtime_mismatch")
  root=$(cat "$TMP/$runtime_mismatch-root-path")
  state=$(cat "$TMP/$runtime_mismatch-state-path")
  inventory="$root.rollback/rollback-pre-up-blob-containers.json"
  if [ "$runtime_mismatch" = runtime-volume-mismatch ]; then
    mismatch_reason=running-volume-mismatch
    inventory_mismatch='"name":"unexpected_tgblobs"'
  else
    mismatch_reason=running-backend-mismatch
    inventory_mismatch='"dir":"/unexpected-blob-dir"'
  fi
  if [ "$status" != 0 ] && grep -q "blob authority rejected: $mismatch_reason" "$TMP/$runtime_mismatch.stderr" && \
     [ -f "$inventory" ] && grep -q "$inventory_mismatch" "$inventory" && \
     [ -f "$root.rollback/rollback-blob-authority-rejected.txt" ] && \
     ! grep -q 'rollback=verified' "$TMP/$runtime_mismatch.stdout" && \
     ! grep -q '^docker compose up -d --no-build --no-deps telegramd$' "$TMP/$runtime_mismatch-events" && \
     [ "$(cat "$state/telegramd")" = "$TARGET_ID" ]; then
    pass "$runtime_mismatch preserves the current container when fresh rollback authority validation rejects"
  else
    fail "$runtime_mismatch must not replace a container with mismatched authority"
  fi
done

make_fixture old-image old-target-image
status=$(run_fixture old-image)
root=$(cat "$TMP/old-image-root-path")
if [ "$status" != 0 ] && grep -q $'target_image_matches_built\t.*\tfail' "$root".target/target-comparisons.tsv && \
   grep -q 'rollback=verified' "$TMP/old-image.stdout" && grep -q 'docker compose up -d --no-build --no-deps telegramd' "$TMP/old-image-events"; then
  pass 'old running image is rejected against captured built ID and baseline rollback verifies'
else
  fail 'old running image rejection and baseline rollback'
fi

make_fixture stale-pin built
status=$(run_fixture stale-pin old)
if [ "$status" != 0 ] && grep -q $'target_image_matches_built\t.*\tfail' "$(cat "$TMP/stale-pin-root-path")".target/target-comparisons.tsv; then
  pass 'stale captured old image ID cannot pass against a different running target image'
else
  fail 'stale captured image ID rejection'
fi

for capture in wrong missing; do
  make_fixture "bad-pin-$capture" success
  status=$(run_fixture "bad-pin-$capture" "$capture")
  events="$TMP/bad-pin-$capture-events"
  state=$(cat "$TMP/bad-pin-$capture-state-path")
  if [ "$status" != 0 ] && ! grep -q 'docker compose up -d' "$events" && \
     [ "$(cat "$state/head")" = "$BASELINE_SHA" ] && grep -q 'built image identity' "$TMP/bad-pin-$capture.stderr"; then
    pass "${capture} built image ID fails closed before service replacement"
  else
    fail "${capture} built image ID rejection"
  fi
done

make_fixture valid-unset-rollback old-target-image
status=$(run_fixture valid-unset-rollback)
root=$(cat "$TMP/valid-unset-rollback-root-path")
rollback_rows=$(find "$root".* -name rollback-equivalence.tsv -print -quit)
if [ "$status" != 0 ] && [ -n "$rollback_rows" ] && grep -q $'rollback\tcompose_replica_count\tunset\tunset\tpass' "$rollback_rows" && \
   grep -q $'rollback\tcompose_client_addr_trust\tunset\tunset\tpass' "$rollback_rows" && \
   grep -q 'baseline_equivalence=pass' "$root".rollback/rollback-result.txt; then
  pass 'rollback accepts exact unset/unset baseline values'
else
  fail 'unset/unset baseline-equivalent rollback'
fi

make_fixture config-drift config-drift
status=$(run_fixture config-drift)
root=$(cat "$TMP/config-drift-root-path")
if [ "$status" != 0 ] && grep -q $'preflight\tconfig_sha256\t.*\tfail' "$root".target/preflight-comparisons.tsv && \
   grep -q 'result=rejected exit=[1-9]' "$root".target/preflight-failure.txt && \
   ! grep -q 'docker compose up -d$' "$TMP/config-drift-events" && grep -q 'target was not started' "$TMP/config-drift.stderr"; then
  pass 'unrelated Compose configuration drift rejects before build and service replacement'
else
  printf 'config_drift_status=%s\nconfig_drift_stderr=%s\nconfig_drift_events=%s\n' \
    "$status" "$(cat "$TMP/config-drift.stderr")" "$(cat "$TMP/config-drift-events")"
  cat "$root".target/preflight-comparisons.tsv 2>/dev/null || true
  fail 'configuration drift rejection'
fi

make_fixture evidence-collision success
root=$(cat "$TMP/evidence-collision-root-path")
mkdir -m 700 "$root.baseline"
status=$(run_fixture evidence-collision)
if [ "$status" != 0 ] && ! grep -q 'docker compose exec -T postgres pg_dump' "$TMP/evidence-collision-events" && \
   grep -q 'evidence collision' "$TMP/evidence-collision.stderr"; then
  pass 'evidence collision stops before backup or deployment mutation'
else
  fail 'evidence path collision rejection'
fi

make_fixture main-drift success
state=$(cat "$TMP/main-drift-state-path")
printf '%s\n' 8888888888888888888888888888888888888888 > "$state/origin"
status=$(run_fixture main-drift)
if [ "$status" != 0 ] && ! grep -q '^docker ' "$TMP/main-drift-events" && \
   grep -q 'origin/main differs from the authorized target' "$TMP/main-drift.stderr"; then
  pass 'origin/main drift stops under lock before backup or build'
else
  fail 'origin/main drift guard'
fi

for dirty_state in staged unstaged; do
  name="dirty-$dirty_state"
  make_fixture "$name" "$name"
  checkout=$(cat "$TMP/$name-checkout-path")
  state=$(cat "$TMP/$name-state-path")
  printf 'operator %s edit\n' "$dirty_state" > "$checkout/tracked-source.txt"
  status=$(run_fixture "$name")
  if [ "$status" != 0 ] && ! grep -q 'docker compose exec -T postgres pg_dump' "$TMP/$name-events" && \
     ! grep -q '^docker compose build' "$TMP/$name-events" && \
     ! grep -q '^docker compose up -d' "$TMP/$name-events" && \
     ! grep -q '^git reset --hard' "$TMP/$name-events" && \
     [ "$(cat "$checkout/tracked-source.txt")" = "operator $dirty_state edit" ] && \
     [ "$(cat "$state/telegramd")" = "$BASE_ID" ] && \
     grep -q 'staged or unstaged tracked changes' "$TMP/$name.stderr"; then
    pass "$dirty_state tracked edits stop before backup and preserve operator content"
  else
    fail "$dirty_state tracked edit guard before backup"
  fi
done

make_fixture untracked-source untracked-source
checkout=$(cat "$TMP/untracked-source-checkout-path")
state=$(cat "$TMP/untracked-source-state-path")
printf '%s\n' 'operator source file' > "$checkout/internal/untracked.go"
status=$(run_fixture untracked-source)
if [ "$status" != 0 ] && ! grep -q 'docker compose exec -T postgres pg_dump' "$TMP/untracked-source-events" && \
   ! grep -q '^docker compose build' "$TMP/untracked-source-events" && \
   ! grep -q '^docker compose up -d' "$TMP/untracked-source-events" && \
   ! grep -q '^git reset --hard' "$TMP/untracked-source-events" && \
   [ "$(cat "$checkout/internal/untracked.go")" = 'operator source file' ] && \
   [ "$(cat "$state/telegramd")" = "$BASE_ID" ] && \
   grep -q 'untracked Docker build inputs' "$TMP/untracked-source.stderr"; then
  pass 'untracked Docker build input stops before backup and preserves operator file'
else
  fail 'untracked Docker build input guard before backup'
fi

make_fixture untracked-before-build untracked-before-build
checkout=$(cat "$TMP/untracked-before-build-checkout-path")
state=$(cat "$TMP/untracked-before-build-state-path")
status=$(run_fixture untracked-before-build)
build_input_checks=$(grep -c '^git status --porcelain=v1 --untracked-files=all --ignored=matching -- cmd/ internal/ components/ utils/$' "$TMP/untracked-before-build-events" || true)
if [ "$status" != 0 ] && [ "$build_input_checks" = 2 ] && \
   ! grep -q '^docker compose build' "$TMP/untracked-before-build-events" && \
   ! grep -q '^docker compose up -d' "$TMP/untracked-before-build-events" && \
   ! grep -q '^git reset --hard' "$TMP/untracked-before-build-events" && \
   [ "$(cat "$checkout/cmd/untracked.go")" = 'package telegramd' ] && \
   [ "$(cat "$state/head")" = "$TARGET_SHA" ] && [ "$(cat "$state/telegramd")" = "$BASE_ID" ] && \
   grep -q 'untracked Docker build inputs' "$TMP/untracked-before-build.stderr"; then
  pass 'untracked Docker build input appearing before build stops replacement and remains intact'
else
  fail 'untracked Docker build input guard immediately before build'
fi

make_fixture dirty-before-build dirty-before-build
checkout=$(cat "$TMP/dirty-before-build-checkout-path")
state=$(cat "$TMP/dirty-before-build-state-path")
status=$(run_fixture dirty-before-build)
status_checks=$(grep -c '^git status --porcelain=v1 --untracked-files=no$' "$TMP/dirty-before-build-events" || true)
if [ "$status" != 0 ] && [ "$status_checks" = 2 ] && \
   ! grep -q '^docker compose build' "$TMP/dirty-before-build-events" && \
   ! grep -q '^docker compose up -d' "$TMP/dirty-before-build-events" && \
   ! grep -q '^git reset --hard' "$TMP/dirty-before-build-events" && \
   [ "$(cat "$checkout/tracked-source.txt")" = 'operator edit survives rollout guard' ] && \
   [ "$(cat "$state/head")" = "$TARGET_SHA" ] && [ "$(cat "$state/telegramd")" = "$BASE_ID" ] && \
   grep -q 'staged or unstaged tracked changes' "$TMP/dirty-before-build.stderr"; then
  pass 'tracked edits appearing before build stop rollout and remain intact'
else
  fail 'tracked edit guard immediately before build'
fi

for failure in publish sync; do
  name="backup-manifest-$failure"
  make_fixture "$name" success
  if [ "$failure" = publish ]; then
    status=$(run_fixture "$name" built 0 '' 2 backup-manifest.txt)
    expected_error='cannot publish immutable evidence file'
  else
    status=$(run_fixture "$name" built 0 '' 2 '' backup-manifest.txt)
    expected_error='cannot sync published evidence'
  fi
  state=$(cat "$TMP/$name-state-path")
  if [ "$status" != 0 ] && ! grep -q '^docker compose build' "$TMP/$name-events" && \
     ! grep -q '^docker compose up -d$' "$TMP/$name-events" && \
     [ "$(cat "$state/head")" = "$BASELINE_SHA" ] && grep -q "$expected_error" "$TMP/$name.stderr"; then
    pass "backup manifest $failure failure stops before build and deployment"
  else
    fail "backup manifest $failure failure is propagated"
  fi
done

for ((prefix_length = 0; prefix_length <= 8; prefix_length++)); do
  name="starting-prefix-$prefix_length"
  if [ "$prefix_length" -eq 0 ]; then
    starting_revisions=none
    expected_starting=none
  else
    starting_revisions=$(printf '%s ' "${APPROVED_REVISIONS[@]:0:prefix_length}")
    expected_starting=$(IFS=,; printf '%s' "${APPROVED_REVISIONS[*]:0:prefix_length}")
  fi
  if [ "$prefix_length" -eq 8 ]; then
    expected_applied=none
    expected_batch=already_applied
  else
    expected_applied=$(IFS=,; printf '%s' "${APPROVED_REVISIONS[*]:prefix_length}")
    expected_batch=60-67
  fi
  make_fixture "$name" success '' "$starting_revisions"
  root=$(cat "$TMP/$name-root-path")
  status=$(run_fixture "$name")
  if [ "$status" = 0 ] && grep -q '^precheck_result=pass$' "$root.target/schema-precheck.tsv" && \
     grep -q "^schema_batch=$expected_batch$" "$root.target/schema-precheck.tsv" && \
     grep -q "^starting_revision_ids=$expected_starting$" "$root.target/schema-precheck.tsv" && \
     grep -q "^applied_now=$expected_applied$" "$root.target/schema-precheck.tsv" && \
     grep -q '^gate_result=pass$' "$root.target/schema-result-gate.tsv"; then
    pass "schema precheck accepts starting prefix length $prefix_length and records its pending IDs"
  else
    fail "schema precheck starting prefix length $prefix_length"
  fi
done

for precheck_failure in \
  extra-file tampered-sum tampered-60 tampered-61 tampered-62 tampered-63 \
  tampered-64 tampered-65 tampered-66 tampered-67 atlas-mismatch untracked ignored \
  gap substituted-63 incomplete error extra-68 files; do
  name="precheck-$precheck_failure"
  starting_revisions=
  case "$precheck_failure" in
    gap) starting_revisions='20261005000060 20261006000062' ;;
  esac
  make_fixture "$name" "$name" '' "$starting_revisions"
  root=$(cat "$TMP/$name-root-path")
  state=$(cat "$TMP/$name-state-path")
  checkout=$(cat "$TMP/$name-checkout-path")
  status=$(run_fixture "$name")
  evidence="$root.target/schema-precheck.tsv"
  if [ -f "$evidence" ] && grep -q '^precheck_result=reject$' "$evidence" && \
     [ -f "$root.target/schema-precheck-revisions.tsv" ] && \
     [ "$status" != 0 ] && [ "$(cat "$state/head")" = "$BASELINE_SHA" ] && \
     [ "$(cat "$state/tag")" = "$BASE_IMAGE" ] && \
     [ ! -e "$checkout/.state/blob-mode/mode.json" ] && \
     ! grep -Eq '^docker compose exec -T postgres pg_dump|^docker compose build|^docker compose up -d$' "$TMP/$name-events"; then
    pass "$name rejects before backup, build, deployment or authority publication with private evidence"
  else
    fail "$name precheck rejection ordering or evidence"
    show_fixture_failure "$name" "$status"
  fi
done

prepare_apply_fixture precheck-authority-unchanged precheck-error
checkout=$(cat "$TMP/precheck-authority-unchanged-checkout-path")
state=$(cat "$TMP/precheck-authority-unchanged-state-path")
root=$(cat "$TMP/precheck-authority-unchanged-root-path")
baseline_sha=$(cat "$TMP/precheck-authority-unchanged-baseline-sha-path")
before_tag=$(cat "$state/tag")
transition=$(jq -er '.transition_id' "$checkout/.state/blob-mode/mode.json")
mode_report="/root/telegramd-blob-mode-report-$transition.json"
before_authority=$(authority_fingerprint "$checkout/.state/blob-mode" "$mode_report")
status=$(run_fixture precheck-authority-unchanged built 0 '' 2 '' '' apply)
after_authority=$(authority_fingerprint "$checkout/.state/blob-mode" "$mode_report")
if [ "$status" != 0 ] && [ "$before_authority" = "$after_authority" ] && \
   [ "$(cat "$state/head")" = "$baseline_sha" ] && [ "$(cat "$state/tag")" = "$before_tag" ] && \
   ! grep -Eq '^docker compose exec -T postgres pg_dump|^docker compose build|^docker compose up -d$' "$TMP/precheck-authority-unchanged-events" && \
   grep -q '^precheck_result=reject$' "$root.target/schema-precheck.tsv"; then
  pass 'ordinary apply precheck rejection preserves existing blob authority bytes and image tag'
else
  fail 'ordinary apply precheck authority preservation'
fi

make_fixture dirty-migration-after-ff precheck-dirty-after-ff
status=$(run_fixture dirty-migration-after-ff)
state=$(cat "$TMP/dirty-migration-after-ff-state-path")
checkout=$(cat "$TMP/dirty-migration-after-ff-checkout-path")
root=$(cat "$TMP/dirty-migration-after-ff-root-path")
if [ "$status" != 0 ] && [ "$(cat "$state/head")" = "$BASELINE_SHA" ] && \
   [ "$(cat "$state/tag")" = "${BASE_IMAGE#sha256:}" ] && \
   [ -f "$root.target/schema-target-tree-check.tsv" ] && \
   grep -q '^target_tree_check=reject$' "$root.target/schema-target-tree-check.tsv" && \
   ! grep -Eq '^docker compose build|^docker compose up -d$' "$TMP/dirty-migration-after-ff-events"; then
  pass 'migration dirt introduced after fast-forward restores checkout and image tag before build or up'
else
  fail 'post-fast-forward migration cleanliness guard'
  printf 'post_fast_forward_state_head=%s state_tag=%s\n' "$(cat "$state/head")" "$(cat "$state/tag")" >&2
  cat "$TMP/dirty-migration-after-ff.stdout" "$TMP/dirty-migration-after-ff.stderr" >&2
  [ ! -f "$root.target/schema-target-tree-check.tsv" ] || cat "$root.target/schema-target-tree-check.tsv" >&2
  cat "$TMP/dirty-migration-after-ff-events" >&2
fi

make_fixture stale-schema-gate-pin stale-schema-gate-pin
status=$(run_fixture stale-schema-gate-pin)
state=$(cat "$TMP/stale-schema-gate-pin-state-path")
if [ "$status" != 0 ] && grep -q 'schema gate hash differs from reviewed artifact' "$TMP/stale-schema-gate-pin.stderr" && \
   [ "$(cat "$state/head")" = "$BASELINE_SHA" ] && \
   ! grep -Eq '^docker compose exec -T postgres pg_dump|^docker compose build|^docker compose up -d$' "$TMP/stale-schema-gate-pin-events"; then
  pass 'runner rejects the old schema-gate digest before backup or deployment'
else
  fail 'old schema-gate digest rejection'
fi

for schema_failure in missing-60 missing-61 missing-62 missing-63 missing-64 missing-65 missing-66 missing-67 extra-revision invalid-pins invalid-poll-description invalid-revision-detail invalid-unread-marks invalid-unread-pk invalid-unread-index invalid-secret-admin-date invalid-secret-participant-date invalid-secret-index-set wrong-key; do
  name="schema-$schema_failure"
  applied_revisions=
  expected_rows=()
  case "$schema_failure" in
    missing-6[0-7])
      missing_number=${schema_failure#missing-}
      missing_revision=${APPROVED_REVISIONS[$((missing_number - 60))]}
      applied_revisions=$(printf '%s\n' "${APPROVED_REVISIONS[@]}" | grep -vxF -- "$missing_revision" | tr '\n' ' ')
      expected_rows=("post_migration_migration_${missing_number}_present=false" 'post_migration_approved_revision_set_exact=false')
      ;;
    extra-revision)
      applied_revisions="$(printf '%s ' "${APPROVED_REVISIONS[@]}")20261008000068"
      expected_rows=('post_migration_approved_revision_set_exact=false')
      ;;
    invalid-pins) expected_rows=('post_migration_user_dialog_pins_schema_ok=false') ;;
    invalid-poll-description) expected_rows=('post_migration_poll_description_entities_schema_ok=false') ;;
    invalid-revision-detail) expected_rows=('post_migration_revision_detail_ok=false') ;;
    invalid-unread-marks) expected_rows=('post_migration_user_dialog_unread_marks_schema_ok=false') ;;
    invalid-unread-pk) expected_rows=('post_migration_user_dialog_unread_marks_primary_key_columns_exact=false') ;;
    invalid-unread-index) expected_rows=('post_migration_user_dialog_unread_marks_changed_idx_exact=false') ;;
    invalid-secret-admin-date) expected_rows=('post_migration_secret_chats_admin_date_idx_exact=false') ;;
    invalid-secret-participant-date) expected_rows=('post_migration_secret_chats_participant_date_idx_exact=false') ;;
    invalid-secret-index-set) expected_rows=('post_migration_secret_chats_index_names_exact=false') ;;
    wrong-key) ;;
  esac
  make_fixture "$name" "schema-$schema_failure" "$applied_revisions"
  root=$(cat "$TMP/$name-root-path")
  state=$(cat "$TMP/$name-state-path")
  status=$(run_fixture "$name")
  evidence="$root.target/schema-result-gate.tsv"
  schema_rows_ok=1
  if [ "$schema_failure" = wrong-key ]; then
    for key_field in \
      post_migration_user_dialog_pins_primary_key_columns_exact \
      post_migration_user_dialog_pins_position_unique_columns_exact \
      post_migration_cloud_drafts_primary_key_columns_exact \
      post_migration_cloud_draft_sync_primary_key_columns_exact \
      post_migration_user_dialog_unread_marks_primary_key_columns_exact; do
      grep -q "^$key_field=false$" "$evidence" || schema_rows_ok=0
    done
  else
    for row in "${expected_rows[@]}"; do
      grep -q "^$row$" "$evidence" || schema_rows_ok=0
    done
  fi
  if [ "$status" != 0 ] && grep -q 'rollback=verified' "$TMP/$name.stdout" && \
     grep -q '^docker compose up -d --no-build --no-deps telegramd$' "$TMP/$name-events" && \
     [ "$(cat "$state/head")" = "$BASELINE_SHA" ] && [ "$(cat "$state/telegramd")" = "$ROLLBACK_ID" ] && \
     [ "$schema_rows_ok" -eq 1 ] && grep -q 'gate_result=reject' "$evidence"; then
    pass "schema gate rejects $schema_failure and verifies baseline rollback"
  else
    fail "schema gate rejects $schema_failure"
  fi
done

gate_revision_checks_ok=1
approved_sorted=$(printf '%s\n' "${APPROVED_REVISIONS[@]}" | sort | tr '\n' ' ')
  for index in 0 1 2 3 4 5 6 7; do
  field="post_migration_migration_$((60 + index))_present"
  checked_revision=$(grep -A4 -F "('$field', EXISTS (" "$SCHEMA_GATE" | sed -n "s/.*version = '\([0-9]\{14\}\)'.*/\1/p" | sed -n 1p || true)
  [ "$checked_revision" = "${APPROVED_REVISIONS[$index]}" ] || gate_revision_checks_ok=0
  grep -qF -- "  $field" "$SCHEMA_GATE" || gate_revision_checks_ok=0
done
expected_revision_array=$(sed -n '/) = ARRAY\[/,/\]::text\[\]/p' "$SCHEMA_GATE" | grep -oE "'20[0-9]{12}'" | tr -d "'" | sort | tr '\n' ' ' || true)
[ "$expected_revision_array" = "$approved_sorted" ] || gate_revision_checks_ok=0
if [ "$gate_revision_checks_ok" -eq 1 ]; then
  pass 'schema gate SQL checks each of the eight approved revision IDs exactly'
else
  fail 'schema gate SQL revision ID coverage'
fi

for failure in publish sync; do
  name="rollback-result-$failure"
  make_fixture "$name" old-target-image
  if [ "$failure" = publish ]; then
    status=$(run_fixture "$name" built 0 '' 2 rollback-result.txt)
    expected_error='cannot publish immutable evidence file'
  else
    status=$(run_fixture "$name" built 0 '' 2 '' rollback-result.txt)
    expected_error='cannot sync published evidence'
  fi
  if [ "$status" != 0 ] && ! grep -q 'rollback=verified' "$TMP/$name.stdout" && \
     grep -q "$expected_error" "$TMP/$name.stderr" && \
     grep -q 'docker compose up -d --no-build --no-deps telegramd' "$TMP/$name-events" && \
     [ "$(cat "$(cat "$TMP/$name-state-path")/head")" = "$BASELINE_SHA" ]; then
    pass "rollback result $failure failure is not reported as verified"
  else
    fail "rollback result $failure failure propagation"
  fi
done

make_fixture persistence-failure success
status=$(run_fixture persistence-failure built 0 target-comparisons)
root=$(cat "$TMP/persistence-failure-root-path")
if [ "$status" != 0 ] && grep -q 'target_compare=reject' "$TMP/persistence-failure.stderr" && \
   grep -q 'rollback=verified' "$TMP/persistence-failure.stdout" && \
   grep -q 'target-failure.txt' <(find "$root".* -type f -printf '%p\n'); then
  pass 'comparison evidence permission failure rejects target and preserves failure before rollback'
else
  fail 'comparison evidence persistence failure path'
fi

make_fixture bounded-readiness readiness-timeout
status=$(run_fixture bounded-readiness built 0 '' 1)
root=$(cat "$TMP/bounded-readiness-root-path")
if [ "$status" != 0 ] && grep -q 'result=bounded_timeout max_seconds=1' "$root".target/readiness-target.result && \
   grep -q 'rollback=verified' "$TMP/bounded-readiness.stdout"; then
  pass 'target readiness remains bounded and rollback readiness completes separately'
else
  fail 'bounded target and rollback readiness'
fi

make_fixture logs-failed logs-failed
status=$(run_fixture logs-failed built 0 '' 1)
root=$(cat "$TMP/logs-failed-root-path")
if [ "$status" != 0 ] && grep -q 'advertise=unknown[[:space:]].*errors=unknown' "$root".target/readiness-target.tsv && \
   grep -q 'result=bounded_timeout max_seconds=1' "$root".target/readiness-target.result && \
   grep -q 'rollback=verified' "$TMP/logs-failed.stdout"; then
  pass 'failed docker logs remain unknown in persisted readiness evidence and trigger rollback'
else
  fail 'failed docker logs are represented as unknown'
fi

make_fixture marker-write-failed old-target-image
status=$(run_fixture marker-write-failed built 0 '' 2 target-failure.txt)
root=$(cat "$TMP/marker-write-failed-root-path")
if [ "$status" != 0 ] && grep -q 'rolling back anyway' "$TMP/marker-write-failed.stderr" && \
   grep -q 'rollback=verified' "$TMP/marker-write-failed.stdout" && \
   [ ! -e "$root".target/target-failure.txt ] && \
   grep -q 'target_failure_marker=unavailable' "$root".rollback/target-failure-marker-warning.txt; then
  pass 'failure-marker write failure does not withhold verified baseline rollback'
else
  fail 'rollback continues when target failure marker cannot persist'
fi

make_fixture pinned-runtime runtime-mutation
status=$(run_fixture pinned-runtime)
root=$(cat "$TMP/pinned-runtime-root-path")
checkout=$(cat "$TMP/pinned-runtime-checkout-path")
if [ "$status" = 0 ] && grep -q 'rollout=verified' "$TMP/pinned-runtime.stdout" && \
   grep -q 'not the pinned verifier' "$checkout/deploy/telegramd/rollout-runner/rollout-verifier.sh" && \
   grep -q 'schema_gate=pass' "$root".target/target-result.txt && \
   [ -f "$root".baseline/rollout-runner.pinned ] && [ -f "$root".baseline/rollout-verifier.pinned ] && \
   [ -f "$root".baseline/schema-result-gate.pinned ] && [ -f "$root".baseline/blob-mode-state.pinned ]; then
  pass 'fast-forward source rewrites cannot replace pinned runner or approved gates in flight'
else
  fail 'pinned runtime survives target checkout mutation'
fi

if [ "$(sha256sum "$VERIFIER" | awk '{print $1}')" = 441fc37d7cc93e3ae33a30074eac75c73ec9bd55de2175706af52a5d7f77437f ] && \
   [ "$(sha256sum "$SCHEMA_GATE" | awk '{print $1}')" = 6ad85f0161f03169b0656f0d9c01d5c4a71a472156fb139e55f694a068cda2c4 ]; then
  pass 'runner consumes the exact approved verifier and schema-gate hashes'
else
  fail 'approved gate hash pinning'
fi

printf 'fixture_summary=passed:%s failed:%s\n' "$PASS_COUNT" "$FAIL_COUNT"
if [ "$FAIL_COUNT" -ne 0 ]; then
  printf 'fixture_failures=%s\n' "${FAILURES[*]}"
  exit 1
fi
