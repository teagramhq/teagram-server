#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
VERIFIER="$SCRIPT_DIR/rollout-verifier.sh"
SCHEMA_GATE="$SCRIPT_DIR/schema-result-gate.sh"
if [ "$(id -u)" != 0 ]; then
  printf '%s\n' 'run the rollout runner fixtures as root to exercise production evidence checks' >&2
  exit 77
fi
TMP=$(mktemp -d "${TMPDIR:-/root}/main1238-rollout-fixtures.XXXXXXXX")
chmod 700 "$TMP"
if [ "${KEEP_FIXTURE_ARTIFACTS:-0}" = 1 ]; then
  trap 'printf "fixture_artifacts=%s\\n" "$TMP"' EXIT
else
  cleanup_fixtures() {
    local root_file root phase
    for root_file in "$TMP"/*-root-path; do
      [ -f "$root_file" ] || continue
      root=$(cat "$root_file")
      for phase in baseline backup build target rollback; do
        rm -rf -- "$root.$phase"
      done
    done
    rm -rf -- "$TMP"
  }
  trap cleanup_fixtures EXIT
fi

PASS_COUNT=0
FAIL_COUNT=0
FAILURES=()
FIXTURE_INDEX=0
TARGET_SHA=ffffffffffffffffffffffffffffffffffffffff
BASELINE_SHA=9999999999999999999999999999999999999999
BASE_IMAGE=sha256:0000000000000000000000000000000000000000000000000000000000000000
BUILT_IMAGE=sha256:1111111111111111111111111111111111111111111111111111111111111111
POSTGRES_IMAGE=sha256:2222222222222222222222222222222222222222222222222222222222222222
BASE_ID=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
TARGET_ID=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
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
  cat > "$bin/docker" <<'SH'
#!/usr/bin/env bash
set -eu
printf 'docker %s\n' "$*" >> "$MOCK_EVENTS"
phase=$(cat "$MOCK_STATE/phase")
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
if [ "${1:-}" = inspect ]; then
  shift
  if [ "${1:-}" = --type ]; then exit 1; fi
  subject=${1:-}
  shift || true
  if [ "${1:-}" = --format ]; then
    template=${2:-}
    case "$template" in
      '{{.State.Status}}|{{.State.ExitCode}}')
        if [ "$subject" = "$MOCK_MIGRATE_ID" ]; then printf 'exited|0\n'; else printf 'running|0\n'; fi
        ;;
      '{{.State.Health.Status}}') printf 'healthy\n' ;;
      '{{.Image}}')
        case "$subject" in
          "$MOCK_BASE_ID"|"$MOCK_ROLLBACK_ID") printf '%s\n' "$MOCK_BASE_IMAGE" ;;
          "$MOCK_TARGET_ID")
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
    "$MOCK_TARGET_ID")
      id=$MOCK_TARGET_ID
      cfg=target
      if [ "${MOCK_SCENARIO:-success}" = old-target-image ]; then image=$MOCK_BASE_IMAGE; else image=$MOCK_ACTUAL_TARGET_IMAGE; fi
      ;;
    "$MOCK_ROLLBACK_ID") id=$MOCK_ROLLBACK_ID; image=$MOCK_BASE_IMAGE; cfg=baseline ;;
    "$MOCK_POSTGRES_ID") id=$MOCK_POSTGRES_ID; image=$MOCK_POSTGRES_IMAGE; cfg=postgres ;;
    "$MOCK_MIGRATE_ID") id=$MOCK_MIGRATE_ID; image=$MOCK_POSTGRES_IMAGE; cfg=migrate ;;
    *) printf 'unknown inspect subject\n' >&2; exit 93 ;;
  esac
  if [ "$cfg" = postgres ]; then
    jq -nc --arg id "$id" --arg image "$image" '{Id:$id,Image:$image,Config:{Env:[],StopTimeout:120},State:{Status:"running",ExitCode:0,StartedAt:"2026-10-06T12:00:00Z",FinishedAt:"0001-01-01T00:00:00Z",Health:{Status:"healthy"}},HostConfig:{PortBindings:{}},Mounts:[]}'
  elif [ "$cfg" = migrate ]; then
    jq -nc --arg id "$id" --arg image "$image" '{Id:$id,Image:$image,Config:{Env:[],StopTimeout:120},State:{Status:"exited",ExitCode:0,StartedAt:"2026-10-06T12:00:00Z",FinishedAt:"2026-10-06T12:00:01Z"},HostConfig:{PortBindings:{}},Mounts:[]}'
  else
    env_json='["TG_SYNTHETIC_FLAG=fixture"]'
    if [ "$cfg" = target ] && [ "${MOCK_SCENARIO:-success}" != config-drift ]; then
      env_json='["TG_SYNTHETIC_FLAG=fixture","TG_REPLICA_COUNT=1","TG_CLIENT_ADDR_TRUST=socket"]'
    elif [ "$cfg" = target ] && [ "${MOCK_SCENARIO:-success}" = config-drift ]; then
      env_json='["TG_SYNTHETIC_FLAG=fixture","TG_REPLICA_COUNT=1","TG_CLIENT_ADDR_TRUST=socket","TG_UNRELATED=drift"]'
    fi
    jq -nc --arg id "$id" --arg image "$image" --argjson env "$env_json" '{Id:$id,Image:$image,Config:{Env:$env,StopTimeout:120},State:{Status:"running",ExitCode:0,StartedAt:"2026-10-06T12:00:00Z",FinishedAt:"0001-01-01T00:00:00Z"},HostConfig:{PortBindings:{"2443/tcp":[{HostIp:"127.0.0.1",HostPort:"2443"}],"2444/tcp":[{HostIp:"127.0.0.1",HostPort:"2444"}]}},Mounts:[{Type:"volume",Name:"identity",Source:"/synthetic/identity",Destination:"/var/lib/telegramd",Mode:"rw",RW:true,Propagation:"rprivate"},{Type:"volume",Name:"blobs",Source:"/synthetic/blobs",Destination:"/var/lib/telegramd-blobs",Mode:"rw",RW:true,Propagation:"rprivate"}]}'
  fi
  exit 0
fi
if [ "${1:-}" = compose ]; then
  shift
  case "${1:-}" in
    ps)
      if [ "${2:-}" = -q ] && [ "${3:-}" = telegramd ]; then cat "$MOCK_STATE/telegramd"; exit 0; fi
      if [ "${2:-}" = -q ] && [ "${3:-}" = postgres ]; then printf '%s\n' "$MOCK_POSTGRES_ID"; exit 0; fi
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
        for field in post_migration_migration_60_present post_migration_migration_61_present post_migration_migration_62_present post_migration_migration_63_present post_migration_migration_64_present post_migration_approved_revision_set_exact post_migration_files_subtype_constraint_valid post_migration_files_media_metadata_constraint_valid post_migration_files_empty post_migration_files_media_kind_schema_ok post_migration_files_width_schema_ok post_migration_files_height_schema_ok post_migration_reply_to_trusted_default_false post_migration_user_dialog_pins_schema_ok post_migration_cloud_drafts_schema_ok post_migration_cloud_draft_sync_schema_ok post_migration_cloud_draft_sync_changed_idx_present; do
          value=true
          if [ "${MOCK_SCENARIO:-}" = schema-missing-64 ] && [[ "$field" = post_migration_migration_64_present || "$field" = post_migration_approved_revision_set_exact ]]; then value=false; fi
          if [ "${MOCK_SCENARIO:-}" = schema-extra-revision ] && [ "$field" = post_migration_approved_revision_set_exact ]; then value=false; fi
          if [ "${MOCK_SCENARIO:-}" = schema-invalid-pins ] && [ "$field" = post_migration_user_dialog_pins_schema_ok ]; then value=false; fi
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
        printf '%s\n' "$MOCK_TARGET_ID" > "$MOCK_STATE/telegramd"
      fi
      exit 0
      ;;
    *) printf 'unexpected compose command\n' >&2; exit 96 ;;
  esac
fi
if [ "${1:-}" = run ]; then
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
  chmod 700 "$bin/git" "$bin/docker" "$bin/flock" "$bin/sync" "$bin/chmod" "$bin/ln" "$bin/nc" "$bin/curl" "$bin/date"
}

make_fixture() {
  local name=$1 scenario=$2 state bin checkout root stamp env_file override base_config target_config
  FIXTURE_INDEX=$((FIXTURE_INDEX + 1))
  stamp=$(printf '20261006T12%02d00Z' "$FIXTURE_INDEX")
  state="$TMP/$name-state"
  bin="$TMP/$name-bin"
  checkout="$TMP/$name-checkout"
  root="/root/main1238-${TARGET_SHA:0:12}-$stamp"
  mkdir -m 700 "$state" "$checkout"
  mkdir -p -m 700 "$checkout/deploy/telegramd/rollout-runner" "$checkout/cmd" "$checkout/internal" "$checkout/components" "$checkout/utils"
  cp "$SCRIPT_DIR/rollout-runner.sh" "$SCRIPT_DIR/rollout-verifier.sh" \
    "$SCRIPT_DIR/schema-result-gate.sh" "$checkout/deploy/telegramd/rollout-runner/"
  chmod 700 "$checkout/deploy/telegramd/rollout-runner/"*.sh
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
  jq -nc '{services:{telegramd:{stop_grace_period:"2m0s",environment:{TG_SYNTHETIC_FLAG:"fixture"},ports:[{target:2443,published:"2443",host_ip:"127.0.0.1",protocol:"tcp",mode:"host"},{target:2444,published:"2444",host_ip:"127.0.0.1",protocol:"tcp",mode:"host"}],volumes:[{type:"volume",source:"identity",target:"/var/lib/telegramd",read_only:false},{type:"volume",source:"blobs",target:"/var/lib/telegramd-blobs",read_only:false}],network_mode:"",networks:{telegram_server:{}}}}}' > "$base_config"
  cp "$base_config" "$target_config"
  if [ "$scenario" = config-drift ]; then
    jq -c '.services.telegramd.environment.TG_REPLICA_COUNT="1" | .services.telegramd.environment.TG_CLIENT_ADDR_TRUST="socket" | .services.telegramd.environment.UNRELATED="changed"' "$base_config" > "$target_config"
  else
    jq -c '.services.telegramd.environment.TG_REPLICA_COUNT="1" | .services.telegramd.environment.TG_CLIENT_ADDR_TRUST="socket"' "$base_config" > "$target_config"
  fi
  : > "$TMP/$name-events"
  write_mock_commands "$bin"
  printf '%s\n' "$state" > "$TMP/$name-state-path"
  printf '%s\n' "$bin" > "$TMP/$name-bin-path"
  printf '%s\n' "$checkout" > "$TMP/$name-checkout-path"
  printf '%s\n' "$root" > "$TMP/$name-root-path"
  printf '%s\n' "$stamp" > "$TMP/$name-stamp"
  printf '%s\n' "$scenario" > "$TMP/$name-scenario"
}

run_fixture() {
  local name=$1 capture=${2:-built} fail_sync=${3:-0} chmod_match=${4:-} ready=${5:-2} ln_match=${6:-} sync_match=${7:-}
  local state bin checkout root stamp scenario status require_marker=0 runner
  state=$(cat "$TMP/$name-state-path")
  bin=$(cat "$TMP/$name-bin-path")
  checkout=$(cat "$TMP/$name-checkout-path")
  root=$(cat "$TMP/$name-root-path")
  stamp=$(cat "$TMP/$name-stamp")
  scenario=$(cat "$TMP/$name-scenario")
  runner="$checkout/deploy/telegramd/rollout-runner/rollout-runner.sh"
  case "$scenario" in old-target-image|config-drift|readiness-timeout|logs-failed) require_marker=1 ;; esac
  [ "$name" = marker-write-failed ] && require_marker=0
  [ -n "$chmod_match" ] && require_marker=1
  set +e
  (cd "$checkout" && env PATH="$bin:$PATH" \
    MOCK_STATE="$state" MOCK_EVENTS="$TMP/$name-events" MOCK_SCENARIO="$scenario" \
    MOCK_CHECKOUT="$checkout" \
    MOCK_BASE_ID="$BASE_ID" MOCK_TARGET_ID="$TARGET_ID" MOCK_ROLLBACK_ID="$ROLLBACK_ID" \
    MOCK_POSTGRES_ID="$POSTGRES_ID" MOCK_MIGRATE_ID="$MIGRATE_ID" \
    MOCK_BASE_IMAGE="$BASE_IMAGE" MOCK_BUILT_IMAGE="$BUILT_IMAGE" MOCK_ACTUAL_TARGET_IMAGE="$BUILT_IMAGE" MOCK_POSTGRES_IMAGE="$POSTGRES_IMAGE" \
    MOCK_CAPTURE_IMAGE="$capture" MOCK_FAIL_SYNC="$fail_sync" MOCK_FAIL_CHMOD_MATCH="$chmod_match" MOCK_FAIL_LN_MATCH="$ln_match" MOCK_FAIL_SYNC_MATCH="$sync_match" \
    MOCK_REQUIRE_FAILURE_MARKER="$require_marker" MOCK_EVIDENCE_ROOT="$root" \
    MOCK_REAL_CHMOD="$(command -v chmod)" MOCK_REAL_LN="$(command -v ln)" MOCK_REAL_DATE="$(command -v date)" MOCK_STAMP="$stamp" \
    ROLLOUT_RUNNER_TEST_MODE=1 ROLLOUT_RUNNER_CHECKOUT="$checkout" ROLLOUT_RUNNER_EVIDENCE_ROOT=/root \
    ROLLOUT_RUNNER_TEST_CHECKOUT="$checkout" \
    ROLLOUT_RUNNER_LOCK_PATH="$TMP/$name.lock" ROLLOUT_RUNNER_ENV_FILE="$checkout/.env" \
    ROLLOUT_RUNNER_OVERRIDE_FILE="$checkout/docker-compose.override.yml" ROLLOUT_RUNNER_READY_SECONDS="$ready" \
    bash "$runner" apply "$TARGET_SHA" "$BASELINE_SHA" >"$TMP/$name.stdout" 2>"$TMP/$name.stderr")
  status=$?
  set -e
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
phase_paths=$(find /root -mindepth 1 -maxdepth 1 -type d -path "$root.*" -printf '%f\n' | sort | wc -l | tr -d ' ')
if [ "$phase_paths" = 5 ]; then pass 'baseline, backup, build, target, and rollback evidence paths are distinct'; else fail 'distinct immutable phase evidence paths'; fi

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

for schema_failure in missing-64 extra-revision invalid-pins; do
  name="schema-$schema_failure"
  case "$schema_failure" in
    missing-64) expected_schema_row='post_migration_migration_64_present=false' ;;
    extra-revision) expected_schema_row='post_migration_approved_revision_set_exact=false' ;;
    invalid-pins) expected_schema_row='post_migration_user_dialog_pins_schema_ok=false' ;;
  esac
  make_fixture "$name" "schema-$schema_failure"
  root=$(cat "$TMP/$name-root-path")
  state=$(cat "$TMP/$name-state-path")
  status=$(run_fixture "$name")
  evidence="$root.target/schema-result-gate.tsv"
  if [ "$status" != 0 ] && grep -q 'rollback=verified' "$TMP/$name.stdout" && \
     grep -q '^docker compose up -d --no-build --no-deps telegramd$' "$TMP/$name-events" && \
     [ "$(cat "$state/head")" = "$BASELINE_SHA" ] && [ "$(cat "$state/telegramd")" = "$ROLLBACK_ID" ] && \
     grep -q "$expected_schema_row" "$evidence" && grep -q 'gate_result=reject' "$evidence"; then
    pass "schema gate rejects $schema_failure and verifies baseline rollback"
  else
    fail "schema gate rejects $schema_failure"
  fi
done

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
   [ -f "$root".baseline/schema-result-gate.pinned ]; then
  pass 'fast-forward source rewrites cannot replace pinned runner or approved gates in flight'
else
  fail 'pinned runtime survives target checkout mutation'
fi

if [ "$(sha256sum "$VERIFIER" | awk '{print $1}')" = b2e52f57b1d7230fc6c27a9fe299da37a5f70a16d3ed1c7f479123f462844487 ] && \
   [ "$(sha256sum "$SCHEMA_GATE" | awk '{print $1}')" = b9482c9cde6039d5b44e58b397e5f434bd6b9d35a12a27aa8085c5ca317f4d41 ]; then
  pass 'runner consumes the exact approved verifier and schema-gate hashes'
else
  fail 'approved gate hash pinning'
fi

printf 'fixture_summary=passed:%s failed:%s\n' "$PASS_COUNT" "$FAIL_COUNT"
if [ "$FAIL_COUNT" -ne 0 ]; then
  printf 'fixture_failures=%s\n' "${FAILURES[*]}"
  exit 1
fi
