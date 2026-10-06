#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
RUNNER="$SCRIPT_DIR/rollout-runner.sh"
VERIFIER="$SCRIPT_DIR/rollout-verifier.sh"
SCHEMA_GATE="$SCRIPT_DIR/schema-result-gate.sh"
TMP=$(mktemp -d)
chmod 700 "$TMP"
if [ "${KEEP_FIXTURE_ARTIFACTS:-0}" = 1 ]; then
  trap 'printf "fixture_artifacts=%s\\n" "$TMP"' EXIT
else
  trap 'rm -rf -- "$TMP"' EXIT
fi

PASS_COUNT=0
FAIL_COUNT=0
FAILURES=()
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
  'merge --ff-only -q origin/main')
    cat "$MOCK_STATE/origin" > "$MOCK_STATE/head"
    printf '%s\n' target > "$MOCK_STATE/phase"
    ;;
  'reset --hard '*)
    if [ "${MOCK_REQUIRE_FAILURE_MARKER:-0}" = 1 ] && \
       ! find "$MOCK_EVIDENCE_ROOT" \( -name target-failure.txt -o -name preflight-failure.txt \) -type f -print -quit | grep -q .; then
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
        for field in migration_60_present migration_61_present migration_62_present files_subtype_constraint_valid files_media_metadata_constraint_valid files_empty files_media_kind_schema_ok files_width_schema_ok files_height_schema_ok reply_to_trusted_default_false; do
          printf '%s\ttrue\n' "$field"
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
  chmod 700 "$bin/git" "$bin/docker" "$bin/flock" "$bin/sync" "$bin/chmod" "$bin/nc" "$bin/curl" "$bin/date"
}

make_fixture() {
  local name=$1 scenario=$2 state bin checkout root env_file override base_config target_config
  state="$TMP/$name-state"
  bin="$TMP/$name-bin"
  checkout="$TMP/$name-checkout"
  root="$TMP/$name-evidence"
  mkdir -m 700 "$state" "$checkout" "$root"
  printf '%s\n' "$BASELINE_SHA" > "$state/head"
  printf '%s\n' "$TARGET_SHA" > "$state/origin"
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
  printf '%s\n' "$scenario" > "$TMP/$name-scenario"
}

run_fixture() {
  local name=$1 capture=${2:-built} fail_sync=${3:-0} chmod_match=${4:-} ready=${5:-2}
  local state bin checkout root scenario status require_marker=0
  state=$(cat "$TMP/$name-state-path")
  bin=$(cat "$TMP/$name-bin-path")
  checkout=$(cat "$TMP/$name-checkout-path")
  root=$(cat "$TMP/$name-root-path")
  scenario=$(cat "$TMP/$name-scenario")
  case "$scenario" in old-target-image|config-drift|readiness-timeout) require_marker=1 ;; esac
  [ -n "$chmod_match" ] && require_marker=1
  set +e
  env PATH="$bin:$PATH" \
    MOCK_STATE="$state" MOCK_EVENTS="$TMP/$name-events" MOCK_SCENARIO="$scenario" \
    MOCK_BASE_ID="$BASE_ID" MOCK_TARGET_ID="$TARGET_ID" MOCK_ROLLBACK_ID="$ROLLBACK_ID" \
    MOCK_POSTGRES_ID="$POSTGRES_ID" MOCK_MIGRATE_ID="$MIGRATE_ID" \
    MOCK_BASE_IMAGE="$BASE_IMAGE" MOCK_BUILT_IMAGE="$BUILT_IMAGE" MOCK_ACTUAL_TARGET_IMAGE="$BUILT_IMAGE" MOCK_POSTGRES_IMAGE="$POSTGRES_IMAGE" \
    MOCK_CAPTURE_IMAGE="$capture" MOCK_FAIL_SYNC="$fail_sync" MOCK_FAIL_CHMOD_MATCH="$chmod_match" \
    MOCK_REQUIRE_FAILURE_MARKER="$require_marker" MOCK_EVIDENCE_ROOT="$root" \
    MOCK_REAL_CHMOD="$(command -v chmod)" MOCK_REAL_DATE="$(command -v date)" MOCK_STAMP="20261006T120000Z" \
    ROLLOUT_RUNNER_TEST_MODE=1 ROLLOUT_RUNNER_CHECKOUT="$checkout" ROLLOUT_RUNNER_EVIDENCE_ROOT="$root" \
    ROLLOUT_RUNNER_LOCK_PATH="$TMP/$name.lock" ROLLOUT_RUNNER_ENV_FILE="$checkout/.env" \
    ROLLOUT_RUNNER_OVERRIDE_FILE="$checkout/docker-compose.override.yml" ROLLOUT_RUNNER_READY_SECONDS="$ready" \
    bash "$RUNNER" apply "$TARGET_SHA" "$BASELINE_SHA" >"$TMP/$name.stdout" 2>"$TMP/$name.stderr"
  status=$?
  set -e
  printf '%s' "$status"
}

assert_evidence_mode() {
  local root=$1 mode
  while IFS= read -r -d '' path; do
    mode=$(stat -c %a "$path")
    if [ -d "$path" ] && [ "$mode" != 700 ]; then return 1; fi
    if [ -f "$path" ] && [ "$mode" != 600 ]; then return 1; fi
  done < <(find "$root" -print0)
}

make_fixture success built
status=$(run_fixture success)
root=$(cat "$TMP/success-root-path")
if [ "$status" = 0 ] && grep -q 'rollout=verified' "$TMP/success.stdout" && grep -q 'flock -x 9' "$TMP/success-events" && \
   awk 'index($0,"docker image inspect telegramd:local") {image=NR} $0=="docker compose up -d" {up=NR} END{exit !(image>0 && up>image)}' "$TMP/success-events" && \
   grep -q 'restore network none tmpfs only' "$TMP/success-events" && \
   find "$root" -name built-image.identity -exec grep -q "source_sha=$TARGET_SHA image_id=$BUILT_IMAGE" {} \; && \
   assert_evidence_mode "$root"; then
  pass 'built image captured after build, bound to target SHA, and compared before acceptance'
else
  fail 'built image capture, provenance binding, and target acceptance'
fi
phase_paths=$(find "$root" -mindepth 1 -maxdepth 1 -type d -printf '%f\n' | sort | wc -l | tr -d ' ')
if [ "$phase_paths" = 5 ]; then pass 'baseline, backup, build, target, and rollback evidence paths are distinct'; else fail 'distinct immutable phase evidence paths'; fi

make_fixture old-image old-target-image
status=$(run_fixture old-image)
root=$(cat "$TMP/old-image-root-path")
if [ "$status" != 0 ] && grep -q $'target_image_matches_built\t.*\tfail' "$root"/*.target/target-comparisons.tsv && \
   grep -q 'rollback=verified' "$TMP/old-image.stdout" && grep -q 'docker compose up -d --no-build --no-deps telegramd' "$TMP/old-image-events"; then
  pass 'old running image is rejected against captured built ID and baseline rollback verifies'
else
  fail 'old running image rejection and baseline rollback'
fi

make_fixture stale-pin built
status=$(run_fixture stale-pin old)
if [ "$status" != 0 ] && grep -q $'target_image_matches_built\t.*\tfail' "$(cat "$TMP/stale-pin-root-path")"/*.target/target-comparisons.tsv; then
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
rollback_rows=$(find "$root" -name rollback-equivalence.tsv -print -quit)
if [ "$status" != 0 ] && [ -n "$rollback_rows" ] && grep -q $'rollback\tcompose_replica_count\tunset\tunset\tpass' "$rollback_rows" && \
   grep -q $'rollback\tcompose_client_addr_trust\tunset\tunset\tpass' "$rollback_rows" && \
   grep -q 'baseline_equivalence=pass' "$root"/*.rollback/rollback-result.txt; then
  pass 'rollback accepts exact unset/unset baseline values'
else
  fail 'unset/unset baseline-equivalent rollback'
fi

make_fixture config-drift config-drift
status=$(run_fixture config-drift)
root=$(cat "$TMP/config-drift-root-path")
if [ "$status" != 0 ] && grep -q $'preflight\tconfig_sha256\t.*\tfail' "$root"/*.target/preflight-comparisons.tsv && \
   grep -q 'result=rejected exit=[1-9]' "$root"/*.target/preflight-failure.txt && \
   ! grep -q 'docker compose up -d$' "$TMP/config-drift-events" && grep -q 'target was not started' "$TMP/config-drift.stderr"; then
  pass 'unrelated Compose configuration drift rejects before build and service replacement'
else
  printf 'config_drift_status=%s\nconfig_drift_stderr=%s\nconfig_drift_events=%s\n' \
    "$status" "$(cat "$TMP/config-drift.stderr")" "$(cat "$TMP/config-drift-events")"
  cat "$root"/*.target/preflight-comparisons.tsv 2>/dev/null || true
  fail 'configuration drift rejection'
fi

make_fixture evidence-collision success
root=$(cat "$TMP/evidence-collision-root-path")
mkdir -m 700 "$root/main1238-${TARGET_SHA:0:12}-20261006T120000Z.baseline"
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

make_fixture persistence-failure success
status=$(run_fixture persistence-failure built 0 target-comparisons)
root=$(cat "$TMP/persistence-failure-root-path")
if [ "$status" != 0 ] && grep -q 'target_compare=reject' "$TMP/persistence-failure.stderr" && \
   grep -q 'rollback=verified' "$TMP/persistence-failure.stdout" && \
   grep -q 'target-failure.txt' <(find "$root" -type f -printf '%p\n'); then
  pass 'comparison evidence permission failure rejects target and preserves failure before rollback'
else
  fail 'comparison evidence persistence failure path'
fi

make_fixture bounded-readiness readiness-timeout
status=$(run_fixture bounded-readiness built 0 '' 1)
root=$(cat "$TMP/bounded-readiness-root-path")
if [ "$status" != 0 ] && grep -q 'result=timeout max_seconds=1' "$root"/*.target/readiness-target.tsv.result && \
   grep -q 'rollback=verified' "$TMP/bounded-readiness.stdout"; then
  pass 'target readiness remains bounded and rollback readiness completes separately'
else
  fail 'bounded target and rollback readiness'
fi

if [ "$(sha256sum "$VERIFIER" | awk '{print $1}')" = f5133ba9c4be17a587fe7e80d01bbe50aa905b9e68586fcdaf992816bdf9b38e ] && \
   [ "$(sha256sum "$SCHEMA_GATE" | awk '{print $1}')" = 4ecb2a3962749447d0534eda36676b81f098c8b966411c7a75817690586bc1ca ]; then
  pass 'runner consumes the exact approved verifier and schema-gate hashes'
else
  fail 'approved gate hash pinning'
fi

printf 'fixture_summary=passed:%s failed:%s\n' "$PASS_COUNT" "$FAIL_COUNT"
if [ "$FAIL_COUNT" -ne 0 ]; then
  printf 'fixture_failures=%s\n' "${FAILURES[*]}"
  exit 1
fi
