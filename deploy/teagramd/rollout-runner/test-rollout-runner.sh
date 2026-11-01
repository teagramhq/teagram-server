#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REAL_DOCKER=$(command -v docker)
VERIFIER="$SCRIPT_DIR/rollout-verifier.sh"
SCHEMA_GATE="$SCRIPT_DIR/schema-result-gate.sh"
MODE_HELPER="$SCRIPT_DIR/blob-mode-state.py"
STACK_MOVE_HELPER="$SCRIPT_DIR/stack-move-runner.py"
bash "$SCRIPT_DIR/test-rollout-runtime-pins.sh"
if [ "$(id -u)" != 0 ]; then
  printf '%s\n' 'run the rollout runner fixtures as root to exercise production evidence checks' >&2
  exit 77
fi
bash "$SCRIPT_DIR/test-rollout-verifier-env-additions.sh"
TMP=$(mktemp -d "${TMPDIR:-/root}/main1238-rollout-fixtures.XXXXXXXX")
chmod 700 "$TMP"
cleanup_fixtures() {
  local root_file root phase checkout transition record_file report phase_dir
  for root_file in "$TMP"/*-root-path; do
    [ -f "$root_file" ] || continue
    root=$(cat "$root_file")
    checkout=$(cat "${root_file%-root-path}-checkout-path" 2>/dev/null || true)
    if [ -n "$checkout" ] && [ -f "$checkout/.state/blob-mode/mode.json" ]; then
      for record_file in "$checkout"/.state/blob-mode/journal/*.json "$checkout"/.state/blob-mode/mode.json; do
        [ -f "$record_file" ] || continue
        transition=$(jq -r '.transition_id' "$record_file")
        report="/root/teagramd-blob-mode-report-$transition.json"
        if [ -f "$report" ]; then
          while IFS= read -r phase_dir; do
            case "$phase_dir" in
              transition-proof-fixtures-*) rm -rf -- "/root/$phase_dir" ;;
            esac
          done < <(jq -r '.phase_files[]? | split("/")[0]' "$report" | sort -u)
          rm -f -- "$report"
        fi
        if [ "$(jq -r '.outcome' "$record_file")" = stack-moved-local ]; then
          rm -rf -- "/root/stack-move-$transition"
        fi
      done
    fi
    for phase in baseline backup build target rollback; do
      rm -rf -- "$root.$phase"
    done
  done
  rm -rf -- "$TMP"
}
if [ "${KEEP_FIXTURE_ARTIFACTS:-0}" = 1 ]; then
  trap 'printf "fixture_artifacts=%s\\n" "$TMP"' EXIT
else
  trap cleanup_fixtures EXIT
fi

PASS_COUNT=0
FAIL_COUNT=0
FAILURES=()
FIXTURE_INDEX=0
TARGET_SHA=ffffffffffffffffffffffffffffffffffffffff
BASELINE_SHA=9999999999999999999999999999999999999999
APPLY_TARGET_SHA=8888888888888888888888888888888888888888
APPLY_SOURCE_SHA=7777777777777777777777777777777777777777
APPLY_ORIGIN_SHA=6666666666666666666666666666666666666666
LIVE_BASELINE_SHA=9f06974f64721a97d2a5a10665f491b59ef71649
E58_LOCAL_COMPOSE_TARGET_SHA=e58ba203505a37bf5ad3181f22e59a934d4a7442
E58_LOCAL_COMPOSE_ARTIFACT_SHA=d3426792677d0510a75dd254fbbd5fd9d9b19a01d710ec5b9b59f3e8a31447c7
TARGET_LOCAL_COMPOSE_TARGET_SHA=598359e900130c307dd84022b0077fd3276f6f36
TARGET_LOCAL_COMPOSE_ARTIFACT_SHA=714d7f870645e2581234feba81a86e9e529facd0b664b32b2f429eb583dc754f
DF9_LOCAL_COMPOSE_TARGET_SHA=df9ffe538defd4b99bd9edb2405caacddd1aa1f6
DF9_LOCAL_COMPOSE_ARTIFACT_SHA=b48e1bc4727b9ed5e05d247fb7f5e3424eca8c1f9b496727e56127b67f7a5ce1
APPROVED_LOCAL_COMPOSE_CONTENT_SHA=d025a25d3e388489ddaf181e3d38b4ecd56906aeab75a9f9fbabdc38e017cfdb
SOURCE_LOCAL_COMPOSE_TARGET_SHA=5602c7c13eb1571278f43129345a4b52f0630377
SOURCE_LOCAL_COMPOSE_ARTIFACT_SHA=05fc5a3e7dbdc2feac58fce870796baebc055b617bc81f6d6b206c5a2c184cc5
SOURCE_LOCAL_COMPOSE_CONTENT_SHA=577dd9885aa9babd3370a97417d1653884f0567c6f50e7feb208ca9d11ca15bd
CCE_LOCAL_COMPOSE_TARGET_SHA=cce00d185e8e7734b482f116e465647f6ec1fa26
CCE_LOCAL_COMPOSE_ARTIFACT_SHA=72a91a77db188f106b0062420722166436d38c6cb1974b73b9dd2873fbc5c11c
CCE_LOCAL_COMPOSE_CONTENT_SHA=577dd9885aa9babd3370a97417d1653884f0567c6f50e7feb208ca9d11ca15bd
INITIAL_LOCAL_TARGET_SHA=777742cc4b3ab0fda6b504a82b314a90aa60918b
INITIAL_LOCAL_LEGACY_BASELINE_SHA=932994e26a86eb1c9ad60f81b3d222b19d3f40b7
INITIAL_LOCAL_COMPOSE_ARTIFACT_SHA=d764d291ad2ea91cff841d14a0cd2bbcb5981961dbc67406bfeba1196a7b3fa4
BASE_IMAGE=sha256:0000000000000000000000000000000000000000000000000000000000000000
BUILT_IMAGE=sha256:1111111111111111111111111111111111111111111111111111111111111111
POSTGRES_IMAGE=sha256:2222222222222222222222222222222222222222222222222222222222222222
BASE_ID=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
TARGET_ID=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
APPLY_ID=ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff
ROLLBACK_ID=cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
POSTGRES_ID=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
MIGRATE_ID=eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
RUSTFS_ID=1212121212121212121212121212121212121212121212121212121212121212

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
if [ -n "${MOCK_REAL_GIT:-}" ]; then
  printf 'git %s\n' "$*" >> "$MOCK_EVENTS"
  exec "$MOCK_REAL_GIT" "$@"
fi
if [ "${1:-}" = -C ]; then shift 2; fi
printf 'git %s\n' "$*" >> "$MOCK_EVENTS"
if [ "${1:-}" = cat-file ] && [ "${2:-}" = -e ]; then
  case "${3:-}" in
    "$MOCK_TARGET_SHA"'^{commit}'|"$MOCK_SOURCE_SHA"'^{commit}'|\
    "$MOCK_TARGET_SHA:docker-compose.yml"|"$MOCK_TARGET_SHA:docker-compose.local-blobs.yml") exit 0 ;;
    *) exit 1 ;;
  esac
fi
if [ "${1:-}" = merge-base ] && [ "${2:-}" = --is-ancestor ]; then
  ancestor=${3:-}
  descendant=${4:-}
  origin=$(cat "$MOCK_STATE/origin")
  if [ "$ancestor" = "$MOCK_TARGET_SHA" ]; then
    [ "$descendant" = "$MOCK_TARGET_SHA" ] && exit 0
    [ "$descendant" = "$MOCK_SOURCE_SHA" ] && \
      [ "${MOCK_SCENARIO:-}" != target-local-carry-forward-cce-target-not-ancestor-source ] && exit 0
    [ "$descendant" = "$origin" ] && [ "$origin" = "$MOCK_ORIGIN_SHA" ] && \
      [ "${MOCK_SCENARIO:-}" = target-local-carry-forward-cce-target-not-ancestor-source ] && exit 0
  fi
  [ "$MOCK_SOURCE_SHA" != "$MOCK_TARGET_SHA" ] && [ "$ancestor" = "$MOCK_SOURCE_SHA" ] && \
    [ "$descendant" = "$origin" ] && [ "$origin" = "$MOCK_SOURCE_SHA" ] && exit 0
  [ "$ancestor" = "$MOCK_SOURCE_SHA" ] && [ "$descendant" = "$origin" ] && \
    [ "$origin" = "$MOCK_ORIGIN_SHA" ] && \
    [ "${MOCK_SCENARIO:-}" = target-local-carry-forward-cce-target-not-ancestor-source ] && exit 0
  [ "$ancestor" = "$MOCK_BASELINE_SHA" ] && [ "$descendant" = "$MOCK_TARGET_SHA" ] && \
    [ "${MOCK_SCENARIO:-}" != target-local-cce-historical-over-live-baseline ] && exit 0
  exit 1
fi
case "$*" in
  'fetch -q origin') exit 0 ;;
  'branch --show-current') printf '%s\n' "${MOCK_BRANCH-main}" ;;
  'rev-parse HEAD') cat "$MOCK_STATE/head" ;;
  'rev-parse origin/main') cat "$MOCK_STATE/origin" ;;
  'ls-files --error-unmatch -- docker-compose.yml docker-compose.local-blobs.yml')
    printf '%s\n' docker-compose.local-blobs.yml docker-compose.yml
    ;;
  "show $MOCK_SOURCE_SHA:deploy/teagramd/rollout-runner/rollout-runner.sh") cat "$MOCK_TARGET_RUNTIME_DIR/rollout-runner.sh" ;;
  "show $MOCK_SOURCE_SHA:deploy/teagramd/rollout-runner/rollout-verifier.sh") cat "$MOCK_TARGET_RUNTIME_DIR/rollout-verifier.sh" ;;
  "show $MOCK_SOURCE_SHA:deploy/teagramd/rollout-runner/schema-result-gate.sh") cat "$MOCK_TARGET_RUNTIME_DIR/schema-result-gate.sh" ;;
  "show $MOCK_SOURCE_SHA:deploy/teagramd/rollout-runner/schema-result-gate.py") cat "$MOCK_TARGET_RUNTIME_DIR/schema-result-gate.py" ;;
  "show $MOCK_SOURCE_SHA:deploy/teagramd/rollout-runner/blob-mode-state.py") cat "$MOCK_TARGET_RUNTIME_DIR/blob-mode-state.py" ;;
  "show $MOCK_SOURCE_SHA:deploy/teagramd/rollout-runner/qualify-rustfs-transition.py") cat "$MOCK_TARGET_RUNTIME_DIR/qualify-rustfs-transition.py" ;;
  "show $MOCK_SOURCE_SHA:deploy/teagramd/rollout-runner/stack-move-runner.py") cat "$MOCK_TARGET_RUNTIME_DIR/stack-move-runner.py" ;;
  "show $MOCK_SOURCE_SHA:deploy/teagramd/rollout-runner/local-compose-e58ba20.yml") cat "$MOCK_TARGET_RUNTIME_DIR/local-compose-e58ba20.yml" ;;
  "show $MOCK_SOURCE_SHA:deploy/teagramd/rollout-runner/local-compose-598359e.yml") cat "$MOCK_TARGET_RUNTIME_DIR/local-compose-598359e.yml" ;;
  "show $MOCK_SOURCE_SHA:deploy/teagramd/rollout-runner/local-compose-df9ffe5.yml") cat "$MOCK_TARGET_RUNTIME_DIR/local-compose-df9ffe5.yml" ;;
  "show $MOCK_SOURCE_SHA:deploy/teagramd/rollout-runner/local-compose-5602c7c.yml") cat "$MOCK_TARGET_RUNTIME_DIR/local-compose-5602c7c.yml" ;;
  "show $MOCK_SOURCE_SHA:deploy/teagramd/rollout-runner/local-compose-cce00d1.yml") cat "$MOCK_TARGET_RUNTIME_DIR/local-compose-cce00d1.yml" ;;
  'status --porcelain=v1 --untracked-files=all --ignored=matching -- migrations/')
    if [ "${MOCK_SCENARIO:-}" = dirty-migration-inputs ]; then
      printf ' M migrations/20261008000069_profile_photo_gallery.sql\n'
    fi
    ;;
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
  'merge --ff-only -q '*)
    if [ "${MOCK_SCENARIO:-}" = target-local-apply-success ] && [ "$(umask)" != 0022 ]; then
      printf 'target checkout merge used umask %s instead of 0022\n' "$(umask)" >&2
      exit 91
    fi
    printf '%s\n' "${*: -1}" > "$MOCK_STATE/head"
    printf '%s\n' target > "$MOCK_STATE/phase"
    if [ "${MOCK_SCENARIO:-}" = dirty-before-build ]; then
      printf '%s\n' 'operator edit survives rollout guard' > "$MOCK_CHECKOUT/tracked-source.txt"
    fi
    if [ "${MOCK_SCENARIO:-}" = untracked-before-build ]; then
      printf '%s\n' 'package teagramd' > "$MOCK_CHECKOUT/cmd/untracked.go"
    fi
    if [ "${MOCK_SCENARIO:-}" = gate-mutation ]; then
      printf '%s\n' 'fixture-only post-merge mutation' >> "${MOCK_EVIDENCE_ROOT}.baseline/rollout-verifier.pinned"
    fi
    if [ "${MOCK_SCENARIO:-}" = runtime-mutation ]; then
      printf '%s\n' 'not the pinned verifier' > "$MOCK_CHECKOUT/deploy/teagramd/rollout-runner/rollout-verifier.sh"
      printf '%s\n' 'not the pinned schema gate' > "$MOCK_CHECKOUT/deploy/teagramd/rollout-runner/schema-result-gate.sh"
      printf '%s\n' 'not the pinned schema gate helper' > "$MOCK_CHECKOUT/deploy/teagramd/rollout-runner/schema-result-gate.py"
      printf '%s\n' '#!/bin/sh' 'exit 99' > "$MOCK_CHECKOUT/deploy/teagramd/rollout-runner/rollout-runner.sh"
    fi
    ;;
  'reset --hard '*)
    if [ "${MOCK_REQUIRE_FAILURE_MARKER:-0}" = 1 ] && \
       ! find "$MOCK_EVIDENCE_ROOT".* \( -name target-failure.txt -o -name preflight-failure.txt \) -type f -print -quit | grep -q .; then
      printf 'rollback started before target failure evidence was persisted\n' >&2
      exit 89
    fi
    umask > "$MOCK_STATE/reset-umask"
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
printf 'python3 %s %s\n' "$script_name" "${2:-}" >> "$MOCK_EVENTS"
if { [ "$script_name" = stack-move-runner.py ] || [ "$script_name" = stack-move-runner.pinned ]; } && \
   [ "${2:-}" = inspect ]; then
  case "${MOCK_SCENARIO:-}" in
    project-layout-changed)
      printf '%s\n' '{"changed":true,"layout_diagnostics":{"reason":"persistent_layout_mismatch","changed_components":["server_mounts","volumes"],"public_profile":"not_applicable"},"differences":[{"service":"teagramd","mount_target":"/var/lib/teagramd","field":"source"}]}'
      ;;
    project-layout-report-missing)
      printf '%s\n' '{"changed":true,"layout_diagnostics":{"reason":"persistent_layout_mismatch","changed_components":["server_mounts","volumes"],"public_profile":"not_applicable"}}'
      ;;
    project-layout-report-malformed)
      printf '%s\n' '{"changed":true,"layout_diagnostics":{"reason":"persistent_layout_mismatch","changed_components":["server_mounts","volumes"],"public_profile":"not_applicable"},"differences":[{"service":"teagramd","mount_target":"/var/lib/teagramd","unexpected":"do-not-log-value"}]}'
      ;;
    project-layout-report-empty)
      printf '%s\n' '{"changed":true,"layout_diagnostics":{"reason":"persistent_layout_mismatch","changed_components":["server_mounts","volumes"],"public_profile":"not_applicable"},"differences":[]}'
      ;;
    *)
      printf '%s\n' '{"changed":false,"source_database":"teagram","target_database":"teagram"}'
      ;;
  esac
  exit 0
fi
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
if [ "${1:-}" = compose ] && [ -n "${MOCK_COMPOSE_FILE_EVENTS:-}" ]; then
  if [ "${MOCK_SCENARIO:-}" = s3-accepted-apply ] && \
     [ "${COMPOSE_ENV_FILES:-}" != "${MOCK_EXPECTED_COMPOSE_ENV_FILES:-}" ]; then
    printf 'expected COMPOSE_ENV_FILES=%s actual=%s\n' \
      "${MOCK_EXPECTED_COMPOSE_ENV_FILES:-unset}" "${COMPOSE_ENV_FILES:-unset}" >&2
    exit 89
  fi
  compose_command=''
  printf -v compose_command '%q ' "$@"
  printf '%s\t%s\t%s\n' "${COMPOSE_FILE:-unset}" "$compose_command" \
    "${COMPOSE_ENV_FILES:-unset}" >> "$MOCK_COMPOSE_FILE_EVENTS"
fi
phase=$(cat "$MOCK_STATE/phase")
if [ -n "${MOCK_REAL_GIT:-}" ]; then
  real_head=$("$MOCK_REAL_GIT" -C "$MOCK_CHECKOUT" rev-parse HEAD)
  if [ "$real_head" = "$MOCK_TARGET_SHA" ]; then phase=target; else phase=baseline; fi
fi
if [ "${1:-}" = image ] && [ "${2:-}" = inspect ]; then
  if [ "${3:-}" = --format ] && [ "${4:-}" = '{{json .Config.Env}}' ]; then
    printf '%s\n' '[]'
    exit 0
  fi
  if [[ "$*" == *'org.opencontainers.image.revision'* ]]; then
    case "${MOCK_SCENARIO:-}" in
      target-local-carry-forward-cce-label-empty) printf '\n' ;;
      target-local-carry-forward-cce-label-mismatch) printf 'wrong-revision\n' ;;
      *) printf '%s\n' "$MOCK_TARGET_SHA" ;;
    esac
    exit 0
  fi
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
  if [ "${2:-}" = --all ] && [ "${3:-}" = --quiet ] && [ "${4:-}" = --no-trunc ]; then
    project=''
    service=''
    for arg in "$@"; do
      case "$arg" in
        label=com.docker.compose.project=*) project=${arg#*=}; project=${project#*=} ;;
        label=com.docker.compose.service=*) service=${arg#*=}; service=${service#*=} ;;
      esac
    done
    [ "$project" = fixture ] || { printf 'unexpected Compose project filter: %s\n' "$project" >&2; exit 95; }
    case "$service" in
      public-haproxy|teagramd-public-1|teagramd-public-2) ;;
      *) printf 'unexpected public profile service filter: %s\n' "$service" >&2; exit 96 ;;
    esac
    exit 0
  fi
  if [ "${2:-}" = --no-trunc ] && [ "${3:-}" = -q ]; then
    if [ "${MOCK_SCENARIO:-}" = initial-local-replica-network-drift ]; then
      printf '%s\n' "${MOCK_BASE_ID//a/1}"
    fi
    cat "$MOCK_STATE/teagramd"
    printf '%s\n' "$MOCK_POSTGRES_ID"
    case "${MOCK_SCENARIO:-}" in
      s3-accepted-apply|recovered-local-apply) printf '%s\n' "$MOCK_RUSTFS_ID" ;;
    esac
  else
    printf '%s\n' "$(cat "$MOCK_STATE/teagramd")"
  fi
  exit 0
fi
if [ "${1:-}" = volume ] && [ "${2:-}" = inspect ]; then
  name=${3:?}
  jq -nc --arg name "$name" '[{Name:$name,Driver:"local",Labels:{"com.docker.compose.volume":"tgblobs","com.docker.compose.project":"fixture"}}]'
  exit 0
fi
if [ "${1:-}" = inspect ]; then
  shift
  if [ "${1:-}" = --type ]; then
    [ "${2:-}" = container ] || exit 1
    restore_name=${3:-}
    if [ -f "$MOCK_STATE/restore-name" ] && \
       [ "$(cat "$MOCK_STATE/restore-name")" = "$restore_name" ] && \
       [ "$(cat "$MOCK_STATE/restore-present" 2>/dev/null || printf 0)" = 1 ]; then
      printf '{"Id":"fixture-restore-container"}\n'
      exit 0
    fi
    printf 'Error: No such object: %s\n' "$restore_name" >&2
    exit 1
  fi
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
      '{{ index .Config.Labels "com.docker.compose.project" }}') printf 'fixture\n' ;;
      '{{.HostConfig.LogConfig.Type}}')
        if [ -f "$MOCK_STATE/unsupported-live-logger" ] && [ "$subject" = "$MOCK_BASE_ID" ]; then
          printf 'journald\n'
        elif [ "${MOCK_SCENARIO:-}" = pre-rollout-log-persisted ] || \
           { [ "${MOCK_SCENARIO:-}" = initial-local-legacy-logging-readiness-timeout ] && [ "$subject" = "$MOCK_BASE_ID" ]; } || \
           { [ "${MOCK_SCENARIO:-}" = target-local-syslog-logging-rollout ] && \
             { [ "$phase" = baseline ] || [ "$phase" = rollback ]; }; } || \
           { [ "${MOCK_SCENARIO:-}" = target-local-old-target-image-logging-mismatch ] && [ "$phase" = baseline ]; }; then
          printf 'json-file\n'
        else
          printf 'syslog\n'
        fi
        ;;
      '{{.State.Status}}|{{.State.ExitCode}}')
        if [ "$subject" = "$MOCK_MIGRATE_ID" ]; then printf 'exited|0\n'
        elif [ "$subject" = "$MOCK_TARGET_ID" ] && [ "${MOCK_SCENARIO:-}" = runtime-target-exited ]; then printf 'exited|1\n'
        else printf 'running|0\n'; fi
        ;;
      '{{.State.Health.Status}}') printf 'healthy\n' ;;
      '{{.Image}}')
        case "$subject" in
          "$MOCK_BASE_ID") printf '%s\n' "$MOCK_BASE_IMAGE" ;;
          "$MOCK_ROLLBACK_ID")
            if [ "${MOCK_SCENARIO:-}" = target-local-carry-forward-image-reuse-stale-build ] || \
               [ "${MOCK_SCENARIO:-}" = target-local-carry-forward-image-reuse-stop-timeout-mismatch ]; then
              printf '%s\n' "$MOCK_ACTUAL_TARGET_IMAGE"
            else
              printf '%s\n' "$MOCK_BASE_IMAGE"
            fi
            ;;
          "$MOCK_TARGET_ID"|"$MOCK_REPLACEMENT_ID")
            if [ "${MOCK_SCENARIO:-success}" = old-target-image ] || [ "${MOCK_SCENARIO:-}" = target-local-old-target-image-logging-mismatch ] || [ "${MOCK_SCENARIO:-}" = target-local-598-rollback ] || [ "${MOCK_SCENARIO:-}" = target-local-df9-rollback ] || [ "${MOCK_SCENARIO:-}" = target-local-welcome-channel-addition-rollback ]; then printf '%s\n' "$MOCK_BASE_IMAGE"; else printf '%s\n' "$MOCK_ACTUAL_TARGET_IMAGE"; fi
            ;;
          *) printf '%s\n' "$MOCK_POSTGRES_IMAGE" ;;
        esac
        ;;
      *) printf 'unexpected inspect format\n' >&2; exit 92 ;;
    esac
    exit 0
  fi
  case "$subject" in
    "$MOCK_BASE_ID"|"${MOCK_BASE_ID//a/1}") id=$subject; image=$MOCK_BASE_IMAGE; cfg=baseline ;;
    "$MOCK_TARGET_ID"|"$MOCK_REPLACEMENT_ID")
      id=$subject
      cfg=target
      if [ "${MOCK_SCENARIO:-success}" = old-target-image ] || [ "${MOCK_SCENARIO:-}" = target-local-old-target-image-logging-mismatch ] || [ "${MOCK_SCENARIO:-}" = target-local-598-rollback ] || [ "${MOCK_SCENARIO:-}" = target-local-df9-rollback ] || [ "${MOCK_SCENARIO:-}" = target-local-welcome-channel-addition-rollback ]; then image=$MOCK_BASE_IMAGE; else image=$MOCK_ACTUAL_TARGET_IMAGE; fi
      ;;
    "$MOCK_ROLLBACK_ID")
      id=$MOCK_ROLLBACK_ID
      image=$MOCK_BASE_IMAGE
      if [ "${MOCK_SCENARIO:-}" = target-local-carry-forward-image-reuse-stale-build ] || \
         [ "${MOCK_SCENARIO:-}" = target-local-carry-forward-image-reuse-stop-timeout-mismatch ]; then
        image=$MOCK_ACTUAL_TARGET_IMAGE
      fi
      cfg=rollback
      ;;
    "$MOCK_POSTGRES_ID") id=$MOCK_POSTGRES_ID; image=$MOCK_POSTGRES_IMAGE; cfg=postgres ;;
    "$MOCK_MIGRATE_ID") id=$MOCK_MIGRATE_ID; image=$MOCK_POSTGRES_IMAGE; cfg=migrate ;;
    "$MOCK_RUSTFS_ID") id=$MOCK_RUSTFS_ID; image=$MOCK_POSTGRES_IMAGE; cfg=rustfs ;;
    *) printf 'unknown inspect subject\n' >&2; exit 93 ;;
  esac
  if [ "$cfg" = postgres ]; then
    jq -nc --arg id "$id" --arg image "$image" '{Id:$id,Image:$image,Config:{Env:[],StopTimeout:120,Labels:{"com.docker.compose.project":"fixture","com.docker.compose.service":"postgres"}},State:{Status:"running",ExitCode:0,StartedAt:"2026-10-06T12:00:00Z",FinishedAt:"0001-01-01T00:00:00Z",Health:{Status:"healthy"}},HostConfig:{PortBindings:{}},Mounts:[{Type:"volume",Name:"fixture_pgdata",Source:"/synthetic/pgdata",Destination:"/var/lib/postgresql/data",Mode:"rw",RW:true,Propagation:"rprivate"}]}'
  elif [ "$cfg" = migrate ]; then
    jq -nc --arg id "$id" --arg image "$image" '{Id:$id,Image:$image,Config:{Env:[],StopTimeout:120,Labels:{"com.docker.compose.project":"fixture","com.docker.compose.service":"migrate"}},State:{Status:"exited",ExitCode:0,StartedAt:"2026-10-06T12:00:00Z",FinishedAt:"2026-10-06T12:00:01Z"},HostConfig:{PortBindings:{}},Mounts:[]}'
  elif [ "$cfg" = rustfs ]; then
    jq -nc --arg id "$id" --arg image "$image" --arg network "$MOCK_RUSTFS_PRIVATE_NETWORK" \
      --arg volume "$MOCK_RUSTFS_VOLUME" '{Id:$id,Image:$image,Config:{Env:[],StopTimeout:120,Labels:{"com.docker.compose.project":"fixture","com.docker.compose.service":"rustfs"}},State:{Status:"running",ExitCode:0,StartedAt:"2026-10-06T12:00:00Z",FinishedAt:"0001-01-01T00:00:00Z",Health:{Status:"healthy"}},HostConfig:{PortBindings:{},NetworkMode:$network},NetworkSettings:{Networks:{($network):{}}},Mounts:[{Type:"volume",Name:$volume,Source:"/synthetic/rustfsdata",Destination:"/data",Mode:"rw",RW:true,Propagation:"rprivate"}]}'
  else
    env_json='["TG_RSA_KEY_PATH=/var/lib/teagramd/server_key.pem","TG_SYNTHETIC_FLAG=fixture","TG_BLOB_DIR=/var/lib/teagramd-blobs"]'
    if [[ "${MOCK_SCENARIO:-}" == moved-stack-9ef1110-to-da0107d* ]] || \
       [ "${MOCK_SCENARIO:-}" = recovered-local-apply ]; then
      env_json='["TG_RSA_KEY_PATH=/var/lib/teagramd/server_key.pem","TG_SYNTHETIC_FLAG=fixture","TG_BLOB_DIR=/var/lib/telegramd-blobs","TG_REPLICA_COUNT=1","TG_CLIENT_ADDR_TRUST=socket"]'
    elif [ "${MOCK_SCENARIO:-}" = s3-accepted-apply ]; then
      env_json='["TG_RSA_KEY_PATH=/var/lib/teagramd/server_key.pem","TG_SYNTHETIC_FLAG=fixture","TG_BLOB_S3_ENDPOINT=http://rustfs:9000","TG_BLOB_S3_BUCKET=teagram","TG_BLOB_S3_PREFIX=teagramd/","TG_BLOB_S3_REGION=us-east-1","TG_BLOB_S3_ACCESS_KEY_ID=fixture-access-key","TG_BLOB_S3_SECRET_ACCESS_KEY_FILE=/run/secrets/teagramd-blob-secret-key","TG_BLOB_S3_ALLOW_INSECURE_HTTP=true","TG_REPLICA_COUNT=1","TG_CLIENT_ADDR_TRUST=socket"]'
    elif [ "${MOCK_SCENARIO:-}" = target-local-running-backend-mismatch ] || [ "${MOCK_SCENARIO:-}" = target-local-df9-running-backend-mismatch ]; then
      env_json='["TG_RSA_KEY_PATH=/var/lib/teagramd/server_key.pem","TG_SYNTHETIC_FLAG=fixture","TG_BLOB_DIR=/var/lib/teagramd-blobs","TG_BLOB_S3_ENDPOINT=https://objects.fixture.invalid","TG_BLOB_S3_BUCKET=fixture","TG_BLOB_S3_PREFIX=fixture/","TG_REPLICA_COUNT=1","TG_CLIENT_ADDR_TRUST=socket"]'
    elif { [ "$cfg" = target ] || [ "$cfg" = rollback ]; } && [ "${MOCK_SCENARIO:-success}" = runtime-backend-mismatch ]; then
      env_json='["TG_RSA_KEY_PATH=/var/lib/teagramd/server_key.pem","TG_SYNTHETIC_FLAG=fixture","TG_BLOB_DIR=/unexpected-blob-dir","TG_REPLICA_COUNT=1","TG_CLIENT_ADDR_TRUST=socket"]'
    elif { [ "$cfg" = target ] || [ "$cfg" = rollback ]; } && [ "${MOCK_SCENARIO:-}" = public-websocket-defaults ]; then
      env_json='["TG_RSA_KEY_PATH=/var/lib/teagramd/server_key.pem","TG_SYNTHETIC_FLAG=fixture","TG_BLOB_DIR=/var/lib/teagramd-blobs","TG_PUBLIC_WEBSOCKET_LISTEN_ADDR=","TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR=","TG_PUBLIC_REGISTRATION=closed","TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS=","TG_REPLICA_COUNT=1","TG_CLIENT_ADDR_TRUST=socket"]'
    elif { [ "$cfg" = target ] || [ "$cfg" = rollback ]; } && [[ "${MOCK_SCENARIO:-}" == target-local-welcome-channel-* ]]; then
      env_json='["TG_RSA_KEY_PATH=/var/lib/teagramd/server_key.pem","TG_SYNTHETIC_FLAG=fixture","TG_BLOB_DIR=/var/lib/teagramd-blobs","TG_REPLICA_COUNT=1","TG_CLIENT_ADDR_TRUST=socket","TG_WELCOME_CHANNEL="]'
    elif { [ "$cfg" = target ] || [ "$cfg" = rollback ]; } && [ "${MOCK_SCENARIO:-success}" != config-drift ]; then
      env_json='["TG_RSA_KEY_PATH=/var/lib/teagramd/server_key.pem","TG_SYNTHETIC_FLAG=fixture","TG_BLOB_DIR=/var/lib/teagramd-blobs","TG_REPLICA_COUNT=1","TG_CLIENT_ADDR_TRUST=socket"]'
    elif { [ "$cfg" = target ] || [ "$cfg" = rollback ]; } && [ "${MOCK_SCENARIO:-success}" = config-drift ]; then
      env_json='["TG_RSA_KEY_PATH=/var/lib/teagramd/server_key.pem","TG_SYNTHETIC_FLAG=fixture","TG_BLOB_DIR=/var/lib/teagramd-blobs","TG_REPLICA_COUNT=1","TG_CLIENT_ADDR_TRUST=socket","TG_UNRELATED=drift"]'
    fi
    if [ "$id" = "$MOCK_BASE_ID" ] && [ -n "${MOCK_BASE_ENV_EXTRA:-}" ]; then
      env_json=$(jq -c --arg extra "$MOCK_BASE_ENV_EXTRA" '. + [$extra]' <<< "$env_json")
    elif [ "$id" = "$MOCK_REPLACEMENT_ID" ] && [ -n "${MOCK_TARGET_ENV_EXTRA:-}" ]; then
      env_json=$(jq -c --arg extra "$MOCK_TARGET_ENV_EXTRA" '. + [$extra]' <<< "$env_json")
    elif [ "$id" = "$MOCK_ROLLBACK_ID" ] && [ -n "${MOCK_ROLLBACK_ENV_EXTRA:-}" ]; then
      env_json=$(jq -c --arg extra "$MOCK_ROLLBACK_ENV_EXTRA" '. + [$extra]' <<< "$env_json")
    fi
    if [ -f "$MOCK_STATE/websocket-origin-override-change" ]; then
      case "$id" in
        "$MOCK_BASE_ID"|"$MOCK_TARGET_ID"|"$MOCK_ROLLBACK_ID") origin_value='https://teagram.tailaa4918.ts.net' ;;
        "$MOCK_REPLACEMENT_ID") origin_value='https://teagram.tailaa4918.ts.net,https://web.teagram.io' ;;
        *) exit 94 ;;
      esac
      env_json=$(jq -c --arg value "$origin_value" '. + ["TG_WEBSOCKET_ALLOWED_ORIGINS=" + $value]' <<< "$env_json")
    fi
    login_codes=false
    if { [ "${MOCK_SCENARIO:-}" = initial-local-legacy-logging-readiness-timeout ] && [ "$cfg" = baseline ]; } || \
       [ "${MOCK_SCENARIO:-}" = target-local-syslog-logging-unmigrated ] || \
       { [ "${MOCK_SCENARIO:-}" = target-local-syslog-logging-rollout ] && [ "$phase" != target ]; }; then
      login_codes=true
    fi
    env_json=$(jq -c --arg value "$login_codes" \
      'map(select(type != "string" or (startswith("TG_LOG_LOGIN_CODES=") | not))) + ["TG_LOG_LOGIN_CODES=" + $value]' \
      <<< "$env_json")
    compose_service=teagramd
    if [ "${MOCK_SCENARIO:-}" = initial-local-target-compose-render ]; then
      compose_service=$(jq -er 'if .services.telegramd then "telegramd" elif .services.teagramd then "teagramd" else error("server service missing") end' \
        "$MOCK_STATE/target-compose.json")
      key_path=/var/lib/teagramd
      [ "$compose_service" != telegramd ] || key_path=/var/lib/telegramd
      env_json=$(jq -c --arg service "$compose_service" --arg key_path "$key_path" \
        '["TG_RSA_KEY_PATH=" + $key_path + "/server_key.pem"] + [.services[$service].environment | to_entries[] | "\(.key)=\(.value | tostring)"]' \
        "$MOCK_STATE/target-compose.json")
      if [ "$cfg" = baseline ]; then
        env_json=$(jq -c 'map(select(type != "string" or ((startswith("TG_REPLICA_COUNT=") or startswith("TG_CLIENT_ADDR_TRUST=")) | not)))' <<< "$env_json")
      fi
    fi
    turnstile_source=''
    if [ "$cfg" = baseline ] || [ "$cfg" = target ] || [ "$cfg" = rollback ]; then
      if [ -f "$MOCK_STATE/started-compose-$id.json" ]; then
        turnstile_compose="$MOCK_STATE/started-compose-$id.json"
      elif [ "$cfg" = baseline ] || [ "$cfg" = rollback ]; then
        turnstile_compose="$MOCK_STATE/base-compose.json"
      else
        turnstile_compose="$MOCK_STATE/target-compose.json"
      fi
      turnstile_env=$(jq -c '
        (.services.teagramd // .services.telegramd).environment as $env
        | if ($env | has("TG_TURNSTILE_SITE_KEY")) then [
            "TG_TURNSTILE_SITE_KEY=" + $env.TG_TURNSTILE_SITE_KEY,
            "TG_TURNSTILE_SECRET_KEY_FILE=" + $env.TG_TURNSTILE_SECRET_KEY_FILE,
            "TG_TURNSTILE_SIGNIN_ENABLED=" + $env.TG_TURNSTILE_SIGNIN_ENABLED
          ] else [] end
      ' "$turnstile_compose")
      if [ "$(jq -er 'length' <<< "$turnstile_env")" -gt 0 ]; then
        env_json=$(jq -c --argjson turnstile "$turnstile_env" '
          map(select(type != "string" or ((split("=")[0]) as $key | (["TG_TURNSTILE_SITE_KEY", "TG_TURNSTILE_SECRET_KEY_FILE", "TG_TURNSTILE_SIGNIN_ENABLED"] | index($key) == null)))) + $turnstile
        ' <<< "$env_json")
      fi
      turnstile_source=$(jq -r '.secrets.teagramd_turnstile.file // ""' "$turnstile_compose")
    fi
    log_config_type=syslog
    log_config='{"syslog-address":"unixgram:///dev/log","syslog-facility":"daemon","syslog-format":"rfc5424","tag":"teagramd"}'
    if [ -f "$MOCK_STATE/unsupported-live-logger" ] && [ "$cfg" = baseline ]; then
      log_config_type=journald
      log_config='{}'
    elif { [ "${MOCK_SCENARIO:-}" = initial-local-legacy-logging-readiness-timeout ] && [ "$cfg" = baseline ]; } || \
       { { [ "${MOCK_SCENARIO:-}" = target-local-syslog-logging-rollout ] || \
           [ "${MOCK_SCENARIO:-}" = target-local-syslog-logging-unmigrated ]; } && \
         { [ "$phase" = baseline ] || [ "$phase" = rollback ]; }; }; then
      log_config_type=json-file
      log_config='{}'
    elif [ "${MOCK_SCENARIO:-}" = target-local-old-target-image-logging-mismatch ] && [ "$phase" = baseline ]; then
      log_config_type=json-file
      log_config='{}'
    fi
    jq -nc \
      --arg id "$id" --arg base_id "$MOCK_BASE_ID" --arg replacement_id "$MOCK_REPLACEMENT_ID" --arg image "$image" --argjson env "$env_json" \
      --arg cfg "$cfg" --arg source "$MOCK_CHECKOUT/.state/blob-mode" \
      --arg log_config_type "$log_config_type" --argjson log_config "$log_config" \
      --arg scenario "${MOCK_SCENARIO:-}" --arg phase "$phase" --arg turnstile_source "$turnstile_source" \
      --arg compose_service "$compose_service" \
      '{
        Id:$id,Image:$image,
        Config:{Env:$env,StopTimeout:(if $scenario == "target-local-carry-forward-image-reuse-stop-timeout-mismatch" and $cfg == "target" and $phase == "target" then 60 else 120 end),Labels:{"com.docker.compose.project":"fixture","com.docker.compose.service":(if $scenario == "initial-local-target-compose-render" then $compose_service else "teagramd" end)}},
        State:{Status:(if $scenario == "runtime-target-exited" and $cfg == "target" then "exited" else "running" end),ExitCode:(if $scenario == "runtime-target-exited" and $cfg == "target" then 1 else 0 end),StartedAt:"2026-10-06T12:00:00Z",FinishedAt:(if $scenario == "runtime-target-exited" and $cfg == "target" then "2026-10-06T12:00:01Z" else "0001-01-01T00:00:00Z" end)},
        HostConfig:{LogConfig:{Type:$log_config_type,Config:$log_config},PortBindings:({"2443/tcp":[{HostIp:"127.0.0.1",HostPort:"2443"}]} +
          if $scenario == "target-local-carry-forward-public-listener-addition" and $id == $replacement_id then
            {"2446/tcp":[{HostIp:"127.0.0.1",HostPort:"2446"}]}
          else {} end),NetworkMode:(if $scenario == "initial-local-target-compose-render" and $compose_service == "telegramd" then "fixture_default" else "fixture_teagram_server" end)},
        NetworkSettings:{Networks:((if $scenario == "initial-local-target-compose-render" and $compose_service == "telegramd" then {fixture_default:{}} else {fixture_teagram_server:{}} end) +
          (if $scenario == "s3-accepted-apply" then {fixture_rustfs_private:{}} else {} end) +
          (if $scenario == "initial-local-replica-network-drift" and $id != $base_id then {edge:{}} else {} end))},
        Mounts:(if $scenario == "initial-local-target-compose-render" and $compose_service == "telegramd" then
          ([{Type:"volume",Name:"fixture_tgkey",Source:"/synthetic/telegramd-key",Destination:"/var/lib/telegramd",Mode:"rw",RW:true,Propagation:"rprivate"},{Type:"volume",Name:"fixture_tgblobs",Source:"/synthetic/telegramd-blobs",Destination:"/var/lib/telegramd-blobs",Mode:"rw",RW:true,Propagation:"rprivate"}] +
            if ($cfg == "target" and $phase == "target") or $cfg == "rollback" then [{Type:"bind",Name:"",Source:$source,Destination:"/run/telegramd/blob-mode",Mode:"ro",RW:false,Propagation:"rprivate"}] else [] end)
          else ((if ($scenario == "moved-stack-9ef1110-to-da0107d" or $scenario == "moved-stack-9ef1110-to-da0107d-render-mismatch" or $scenario == "recovered-local-apply") then [{Type:"volume",Name:"identity",Source:"/synthetic/identity",Destination:"/var/lib/teagramd",Mode:"rw",RW:true,Propagation:"rprivate"},{Type:"volume",Name:"teagram_tgblobs",Source:"/synthetic/blobs",Destination:"/var/lib/teagramd-blobs",Mode:"ro",RW:false,Propagation:"rprivate"},{Type:"volume",Name:"teagram_tgblobs",Source:"/synthetic/blobs",Destination:"/var/lib/telegramd-blobs",Mode:"rw",RW:true,Propagation:"rprivate"},{Type:"bind",Name:"",Source:$source,Destination:"/run/teagramd/blob-mode",Mode:"ro",RW:false,Propagation:"rprivate"}] else ([{Type:"volume",Name:"identity",Source:"/synthetic/identity",Destination:"/var/lib/teagramd",Mode:"rw",RW:true,Propagation:"rprivate"},{Type:"volume",Name:(if $scenario == "runtime-volume-mismatch" and $cfg == "target" then "unexpected_tgblobs" else "fixture_tgblobs" end),Source:"/synthetic/blobs",Destination:"/var/lib/teagramd-blobs",Mode:"rw",RW:(if $scenario == "s3-accepted-apply" then false else true end),Propagation:"rprivate"}] + if ($cfg == "target" and $scenario != "runtime-mode-unmounted") or $cfg == "rollback" then [{Type:"bind",Name:"",Source:$source,Destination:"/run/teagramd/blob-mode",Mode:"ro",RW:false,Propagation:"rprivate"}] else [] end) end) + if $turnstile_source == "" then [] else [{Type:"bind",Name:"",Source:$turnstile_source,Destination:"/run/secrets/teagramd-turnstile",Mode:"ro",RW:false,Propagation:"rprivate"}] end)
        end)
      }'
  fi
  exit 0
fi
  if [ "${1:-}" = compose ]; then
  shift
  compose_profile_enabled=0
  if [ "${1:-}" = --profile ]; then
    [ "${2:-}" = migration ] || exit 94
    compose_profile_enabled=1
    shift 2
  fi
  case "${1:-}" in
    ps)
      if [ "${2:-}" = -q ] && { [ "${3:-}" = teagramd ] || [ "${3:-}" = telegramd ]; }; then cat "$MOCK_STATE/teagramd"; exit 0; fi
      if [ "${2:-}" = -q ] && [ "${3:-}" = postgres ]; then printf '%s\n' "$MOCK_POSTGRES_ID"; exit 0; fi
      if [ "${2:-}" = -aq ] && { [ "${3:-}" = teagramd ] || [ "${3:-}" = telegramd ]; }; then cat "$MOCK_STATE/teagramd"; exit 0; fi
      if [ "${2:-}" = -aq ] && [ "${3:-}" = migrate ]; then printf '%s\n' "$MOCK_MIGRATE_ID"; exit 0; fi
      exit 94
      ;;
      config)
        if [ "${2:-}" = --services ]; then
          if [ "${MOCK_SCENARIO:-}" = initial-local-target-compose-render ]; then
            jq -r '.services | keys[]' "$MOCK_STATE/target-compose.json"
          else
            printf '%s\n' teagramd
          fi
          exit 0
        fi
        config_count=0
        [ ! -f "$MOCK_STATE/compose-config-count" ] || config_count=$(cat "$MOCK_STATE/compose-config-count")
        config_count=$((config_count + 1))
        printf '%s\n' "$config_count" > "$MOCK_STATE/compose-config-count"
        if [ -f "$MOCK_STATE/public-websocket-addition" ]; then
          if [ "$phase" = baseline ] || [ "$phase" = rollback ]; then config_file="$MOCK_STATE/base-compose.json"; else config_file="$MOCK_STATE/target-compose.json"; fi
          public_compose=$(cat "$config_file")
          while IFS= read -r env_line; do
            key=${env_line%%=*}
            case "$key" in
              TG_PUBLIC_WEBSOCKET_LISTEN_ADDR|TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR|TG_PUBLIC_REGISTRATION|TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS)
                public_compose=$(jq -c --arg key "$key" --arg value "${env_line#*=}" \
                  '.services.teagramd.environment[$key]=$value' <<< "$public_compose")
                ;;
            esac
          done < "$MOCK_CHECKOUT/.env"
          printf '%s\n' "$public_compose"
        elif { [ "${MOCK_SCENARIO:-}" = target-local-syslog-logging-rollout ] || \
               [ "${MOCK_SCENARIO:-}" = target-local-syslog-logging-unmigrated ]; } && [ "$config_count" -lt 3 ]; then
          cat "$MOCK_STATE/base-compose.json"
        elif [ "${MOCK_SCENARIO:-}" = target-local-syslog-logging-rollout ] || \
             [ "${MOCK_SCENARIO:-}" = target-local-syslog-logging-unmigrated ]; then
          cat "$MOCK_STATE/target-compose.json"
        elif [ "${MOCK_SCENARIO:-}" = config-drift ] && [ "$config_count" -ge 3 ]; then
          cat "$MOCK_STATE/target-compose.drift.json"
        elif [[ "${MOCK_SCENARIO:-}" == public-websocket-* ]] && [ "$config_count" -lt 3 ]; then
          cat "$MOCK_STATE/base-compose.json"
        elif [[ "${MOCK_SCENARIO:-}" == *public-listener-* ]] || \
             [ "${MOCK_SCENARIO:-}" = target-local-old-target-image-logging-mismatch ]; then
          if [ "$phase" = baseline ] || [ "$phase" = rollback ]; then cat "$MOCK_STATE/base-compose.json"; else cat "$MOCK_STATE/target-compose.json"; fi
        elif [ -f "$MOCK_STATE/websocket-origin-override-change" ]; then
          if [ "$phase" = baseline ] || [ "$phase" = rollback ]; then config_file="$MOCK_STATE/base-compose.json"; else config_file="$MOCK_STATE/target-compose.json"; fi
          override_origin=$(sed -n 's/^      TG_WEBSOCKET_ALLOWED_ORIGINS: //p' "$MOCK_CHECKOUT/docker-compose.override.yml")
          [ -n "$override_origin" ] || exit 94
          jq -c --arg value "$override_origin" \
            '.services.teagramd.environment.TG_WEBSOCKET_ALLOWED_ORIGINS=$value' "$config_file"
        elif [ -f "$MOCK_STATE/websocket-origin-change" ]; then
          if [ "$phase" = baseline ] || [ "$phase" = rollback ]; then cat "$MOCK_STATE/base-compose.json"; else cat "$MOCK_STATE/target-compose.json"; fi
        elif [[ "${MOCK_SCENARIO:-}" == target-local-welcome-channel-* ]]; then
          if [ "$config_count" -ge 3 ]; then cat "$MOCK_STATE/target-compose.json"; else cat "$MOCK_STATE/base-compose.json"; fi
        elif [ "${MOCK_SCENARIO:-}" = target-local-carry-forward-cce-literal-build-arg ]; then
          jq -c '.services.teagramd.build.args.TEAGRAMD_SOURCE_REVISION="literal-override"' \
            "$MOCK_STATE/target-compose.json"
        elif [ "${MOCK_SCENARIO:-}" = target-local-carry-forward-cce-wrong-context ]; then
          jq -c --arg context "$MOCK_CHECKOUT/elsewhere" '.services.teagramd.build.context=$context' \
            "$MOCK_STATE/target-compose.json"
        elif [ "${MOCK_SCENARIO:-}" = stack-move-retained-rustfs-volume ] && [ "$compose_profile_enabled" = 0 ]; then
          jq -c 'del(.volumes.rustfsdata)' "$MOCK_STATE/target-compose.json"
        elif [[ "${MOCK_SCENARIO:-}" == moved-stack-9ef1110-to-da0107d* ]] || \
             [ "${MOCK_SCENARIO:-}" = recovered-local-apply ]; then
          if [ "${MOCK_SCENARIO:-}" = moved-stack-9ef1110-to-da0107d-render-mismatch ]; then
            moved_blob_dir=/var/lib/not-authoritative
          elif [ -n "${TEAGRAMD_STACK_MOVE_BLOB_DIR:-}" ]; then
            moved_blob_dir=$TEAGRAMD_STACK_MOVE_BLOB_DIR
          else
            moved_blob_dir=$(sed -n 's/^TEAGRAMD_STACK_MOVE_BLOB_DIR=//p' "$MOCK_CHECKOUT/.env")
            [ -n "$moved_blob_dir" ] || moved_blob_dir=/var/lib/teagramd-blobs
          fi
          jq -c --arg blob_dir "$moved_blob_dir" '
            .services.teagramd.environment.TG_BLOB_DIR=$blob_dir |
            .services.teagramd.volumes |= (
              map(if .source == "tgblobs" and .target == "/var/lib/teagramd-blobs" then .read_only=true else . end) +
              [{type:"volume",source:"tgblobs",target:$blob_dir,read_only:false}]
            ) |
            .volumes.tgblobs.name="teagram_tgblobs" |
            .volumes.rustfsdata={name:"teagram_rustfsdata"}
          ' "$MOCK_STATE/target-compose.json"
        elif [ "${TEAGRAMD_SOURCE_REVISION+x}" = x ]; then
          jq -c --arg revision "$TEAGRAMD_SOURCE_REVISION" \
            '.services.teagramd.build.args.TEAGRAMD_SOURCE_REVISION=$revision' "$MOCK_STATE/target-compose.json"
        else
          cat "$MOCK_STATE/target-compose.json"
        fi
      exit 0
      ;;
    run)
      if [[ " $* " == *' migrate validate '* ]]; then
        if [ "${MOCK_SCENARIO:-}" = schema-atlas-invalid ]; then exit 1; fi
        exit 0
      fi
      exit 94
      ;;
    build)
      printf 'build\n' >> "$MOCK_EVENTS"
      printf 'build revision=%s\n' "${TEAGRAMD_SOURCE_REVISION-unset}" >> "$MOCK_EVENTS"
      printf '%s\n' "$MOCK_BUILT_IMAGE" > "$MOCK_STATE/tag"
      exit 0
      ;;
    exec)
      if [[ " $* " == *' pg_dump '* ]]; then
        printf '%s\n' '-- PostgreSQL database dump complete'
        exit 0
      fi
      if [[ " $* " == *' pg_dumpall '* ]]; then
        printf '%s\n' 'CREATE ROLE postgres;' 'CREATE ROLE browser_acceptance_probe;'
        exit 0
      fi
      if [[ " $* " == *' psql '* ]] && [[ " $* " == *' -c '* ]]; then
        if [[ " $* " == *'CREATE ROLE browser_acceptance_probe '* ]]; then
          printf 'docker compose exec postgres role-create browser_acceptance_probe\n' >> "$MOCK_EVENTS"
        fi
        if [ "${MOCK_SCENARIO:-}" = schema-db-unavailable ] && [ "${SCHEMA_GATE_MODE:-}" = pre ]; then exit 1; fi
        migration_index=0
        while IFS=' ' read -r filename checksum; do
          [ -n "$filename" ] || continue
          [[ "$filename" = *.sql ]] || continue
          version=${filename%%_*}
          if [ "${MOCK_SCENARIO:-}" = schema-pre-prefix ] && \
             [ "${SCHEMA_GATE_MODE:-}" = pre ] && [ "$migration_index" -ge 7 ]; then
            migration_index=$((migration_index + 1))
            continue
          fi
          if [ "${MOCK_SCENARIO:-}" = schema-post-missing-69 ] && \
             [ "${SCHEMA_GATE_MODE:-}" = post ] && [ "$version" = 20261008000069 ]; then
            migration_index=$((migration_index + 1))
            continue
          fi
          actual_hash=${checksum#h1:}
          revision_type=2
          applied=1
          total=1
          error=false
          error_stmt=false
          partial=false
          if [ "${SCHEMA_GATE_MODE:-}" = post ] && [ "$version" = 20261008000069 ]; then
            case "${MOCK_SCENARIO:-}" in
              schema-post-failed-69)
                error=true
                error_stmt=true
                ;;
              schema-post-partial-69)
                revision_type=1
                applied=1
                total=2
                error=true
                error_stmt=true
                partial=true
                ;;
              schema-post-hash-mismatch-69) actual_hash=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA= ;;
            esac
          fi
          printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
            "$version" "$actual_hash" "$revision_type" "$applied" "$total" \
            "$error" "$error_stmt" "$partial"
          migration_index=$((migration_index + 1))
        done < <(tail -n +2 "$MOCK_CHECKOUT/migrations/atlas.sum")
        if [ "${MOCK_SCENARIO:-}" = schema-post-unexpected ] && [ "${SCHEMA_GATE_MODE:-}" = post ]; then
          printf '%s\t%s\t2\t1\t1\tfalse\tfalse\tfalse\n' \
            20261009000070 AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=
        fi
        exit 0
      fi
      exit 95
      ;;
    up)
      # Record the render each container starts from, so a later run sees
      # the live container as it was started rather than the new render.
      if [[ " $* " == *' --no-build '* ]]; then
        printf '%s\n' rollback > "$MOCK_STATE/phase"
        printf '%s\n' "$MOCK_ROLLBACK_ID" > "$MOCK_STATE/teagramd"
        cp -- "$MOCK_STATE/base-compose.json" "$MOCK_STATE/started-compose-$MOCK_ROLLBACK_ID.json" 2>/dev/null || true
      else
        printf '%s\n' target > "$MOCK_STATE/phase"
        if [ "${MOCK_SCENARIO:-}" = target-container-unavailable ]; then
          : > "$MOCK_STATE/teagramd"
        else
          printf '%s\n' "$MOCK_REPLACEMENT_ID" > "$MOCK_STATE/teagramd"
        fi
        cp -- "$MOCK_STATE/target-compose.json" "$MOCK_STATE/started-compose-$MOCK_REPLACEMENT_ID.json"
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
  printf '1\n' > "$MOCK_STATE/restore-present"
  printf 'restore network none tmpfs only\n' >> "$MOCK_EVENTS"
  printf 'fixture-restore-container\n'
  exit 0
fi
if [ "${1:-}" = exec ]; then
  shift
  while [ "${1:-}" = -i ] || [ "${1:-}" = -t ]; do shift; done
  container=${1:-}
  shift || true
  if [[ "$container" = main1238-restore-* ]] && [ "${1:-}" = pg_isready ]; then exit 0; fi
  if [[ "$container" = main1238-restore-* ]] && [ "${1:-}" = psql ]; then
    input=$(cat)
    if [[ "$input" == *'CREATE ROLE browser_acceptance_probe'* ]]; then
      printf 'docker exec restore globals role browser_acceptance_probe\n' >> "$MOCK_EVENTS"
      if [ "${MOCK_SCENARIO:-}" = restore-role-create-failure ]; then exit 1; fi
    fi
    if [ "${MOCK_SCENARIO:-}" = restore-psql-failure ]; then exit 1; fi
    exit 0
  fi
  exit 97
fi
if [ "${1:-}" = rm ] && [ "${2:-}" = -f ]; then
  restore_name=${3:-}
  if [[ "$restore_name" = main1238-restore-* ]]; then
    if [ "${MOCK_SCENARIO:-}" = restore-rm-failure ]; then exit 1; fi
    if [ "${MOCK_SCENARIO:-}" = restore-remains-after-rm ]; then exit 0; fi
    printf '0\n' > "$MOCK_STATE/restore-present"
  fi
  exit 0
fi
if [ "${1:-}" = logs ]; then
  if [[ " $* " == *' --timestamps '* ]]; then
    printf '2026-10-23T21:40:00.123456789Z pre-rollout-line\n'
    if [ "${MOCK_SCENARIO:-}" = pre-rollout-log-persisted ]; then
      printf '2026-10-23T21:40:01.123456789Z level=INFO msg="login code issued" phone=fixture code=secret-fixture-code\n'
      printf '2026-10-23T21:40:02.123456789Z pre-rollout-stderr-line\n' >&2
      printf '2026-10-23T21:40:03.123456789Z level=INFO msg="login code issued" phone=fixture code=stderr-secret-fixture-code\n' >&2
    fi
    exit 0
  fi
  if { [ "${MOCK_SCENARIO:-success}" = readiness-timeout ] || \
       [ "${MOCK_SCENARIO:-}" = initial-local-legacy-logging-readiness-timeout ]; } && \
     [ "$(cat "$MOCK_STATE/phase")" = target ]; then exit 0; fi
  if [ "${MOCK_SCENARIO:-success}" = logs-failed ] && [ "$(cat "$MOCK_STATE/phase")" = target ]; then
    printf 'fixture log read failed\n' >&2
    exit 23
  fi
  printf 'level=INFO advertise=teagram.tailaa4918.ts.net:2443\n'
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
if [ "${MOCK_SCENARIO:-}" = schema-helper-mutated-after-pin ] && \
   [[ "$destination" == */schema-result-gate.py ]]; then
  printf '%s\n' '# mutated after pin' >> "$destination"
fi
printf '%s\n' "$destination" > "$MOCK_STATE/last-published"
SH
  cat > "$bin/bash" <<'SH'
#!/bin/sh
set -eu
case "${1:-}" in
  */rollout-runner.pinned)
    if [ "${MOCK_SCENARIO:-}" = schema-helper-mutated-after-pin ]; then
      printf '%s\n' pinned_runner_started >> "$MOCK_EVENTS"
    fi
    ;;
esac
exec "$MOCK_REAL_BASH" "$@"
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
  chmod 700 "$bin/git" "$bin/python3" "$bin/docker" "$bin/flock" "$bin/sync" "$bin/chmod" "$bin/ln" "$bin/bash" "$bin/nc" "$bin/curl" "$bin/date"
}

write_compose_fixture() {
  local checkout=$1 scenario=$2 base_config=$3 target_config=$4
  jq -nc --arg migrations "$checkout/migrations" --arg mode_source "$checkout/.state/blob-mode" \
    --arg build_context "$checkout" '
    {
      name:"fixture",
      services:{
        teagramd:{
          build:{context:$build_context,args:{TEAGRAMD_SOURCE_REVISION:""}},
          stop_grace_period:"2m0s",
          environment:{TG_SYNTHETIC_FLAG:"fixture",TG_BLOB_DIR:"/var/lib/teagramd-blobs",TG_REPLICA_COUNT:"1",TG_CLIENT_ADDR_TRUST:"socket",TG_LOG_LOGIN_CODES:"false"},
          logging:{driver:"syslog",options:{"syslog-address":"unixgram:///dev/log","syslog-facility":"daemon","syslog-format":"rfc5424",tag:"teagramd"}},
          ports:[{target:2443,published:"2443",host_ip:"127.0.0.1",protocol:"tcp",mode:"ingress"}],
          volumes:[
            {type:"volume",source:"tgkey",target:"/var/lib/teagramd",read_only:false},
            {type:"volume",source:"tgblobs",target:"/var/lib/teagramd-blobs",read_only:false},
            {type:"bind",source:$mode_source,target:"/run/teagramd/blob-mode",read_only:true}
          ],
          network_mode:"",networks:{teagram_server:{}}
        },
        postgres:{image:"mirror.gcr.io/library/postgres:16-alpine",environment:{POSTGRES_DB:"teagram"},volumes:[{type:"volume",source:"pgdata",target:"/var/lib/postgresql/data",read_only:false}]},
        migrate:{volumes:[{type:"bind",source:$migrations,target:"/migrations",read_only:true}]}
      },
      volumes:{tgkey:{name:"identity"},tgblobs:{name:"fixture_tgblobs"},pgdata:{name:"fixture_pgdata"}},
      networks:{teagram_server:{name:"fixture_teagram_server"}}
    }
  ' > "$base_config"
  if [ "$scenario" = config-drift ]; then
    cp -- "$base_config" "$target_config"
  elif [ "$scenario" = public-websocket-defaults ]; then
    jq -c '.services.teagramd.environment += {TG_PUBLIC_WEBSOCKET_LISTEN_ADDR:"",TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR:"",TG_PUBLIC_REGISTRATION:"closed",TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS:""}' "$base_config" > "$target_config"
  elif [ "$scenario" = public-websocket-enabled ]; then
    jq -c '.services.teagramd.environment += {TG_PUBLIC_WEBSOCKET_LISTEN_ADDR:"0.0.0.0:2444",TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR:"",TG_PUBLIC_REGISTRATION:"closed",TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS:""}' "$base_config" > "$target_config"
  elif [ "$scenario" = missing-mode-mount ]; then
    jq -c '.services.teagramd.volumes |= map(select(.target != "/run/teagramd/blob-mode"))' "$base_config" > "$target_config"
  elif [ "$scenario" = wrong-blob-backend ]; then
    jq -c '.services.teagramd.environment.TG_BLOB_DIR="/tmp/unmounted-blobs"' "$base_config" > "$target_config"
  elif [ "$scenario" = initial-local-s3-backend ]; then
    jq -c '.services.teagramd.environment.TG_BLOB_S3_ENDPOINT="https://objects.fixture.invalid" | .services.teagramd.environment.TG_BLOB_S3_BUCKET="fixture" | .services.teagramd.environment.TG_BLOB_S3_PREFIX="fixture/"' "$base_config" > "$target_config"
  elif [[ "$scenario" == initial-local-* ]]; then
    cp -- "$base_config" "$target_config"
    case "$scenario" in
      initial-local-port-drift)
        jq -c '.services.teagramd.ports[0].host_ip="0.0.0.0"' "$target_config" > "${target_config%.json}.drift.json"
        mv "${target_config%.json}.drift.json" "$target_config"
        ;;
      initial-local-network-drift)
        jq -c '.services.teagramd.networks.edge={} | .networks.edge={name:"edge",external:true}' "$target_config" > "${target_config%.json}.drift.json"
        mv "${target_config%.json}.drift.json" "$target_config"
        ;;
      initial-local-key-mount-drift)
        jq -c '.volumes.tgkey.name="unexpected_tgkey"' "$target_config" > "${target_config%.json}.drift.json"
        mv "${target_config%.json}.drift.json" "$target_config"
        ;;
      initial-local-pgdata-drift)
        jq -c '.volumes.pgdata.name="unexpected_pgdata"' "$target_config" > "${target_config%.json}.drift.json"
        mv "${target_config%.json}.drift.json" "$target_config"
        ;;
      initial-local-environment-drift)
        jq -c '.services.teagramd.environment.TG_SYNTHETIC_FLAG="target-changed"' "$target_config" > "${target_config%.json}.drift.json"
        mv "${target_config%.json}.drift.json" "$target_config"
        ;;
    esac
  elif [ "$scenario" = missing-proxy-mode-mount ]; then
    jq -c '.services["teagramd-proxy"]={environment:{TG_BLOB_DIR:"/var/lib/teagramd-blobs"},volumes:[{type:"volume",source:"tgblobs",target:"/var/lib/teagramd-blobs",read_only:false}]}' "$base_config" > "$target_config"
  elif [ "$scenario" = wrong-tgblobs-volume ]; then
    jq -c '.volumes.tgblobs.name="unexpected_tgblobs"' "$base_config" > "$target_config"
  elif [ "$scenario" = wrong-mode-source ]; then
    jq -c '(.services.teagramd.volumes[] | select(.target == "/run/teagramd/blob-mode").source)="/tmp/untrusted-mode"' "$base_config" > "$target_config"
  else
    cp -- "$base_config" "$target_config"
  fi
  if [ "$scenario" = config-drift ]; then
    jq -c '.services.teagramd.environment.UNRELATED="changed"' "$target_config" > "${target_config%.json}.drift.json"
  fi
}

make_fixture() {
  local name=$1 scenario=$2 state bin checkout root stamp env_file override base_config target_config target_runtime source_root
  printf 'fixture setup: %s scenario=%s\n' "$name" "$scenario" >&2
  FIXTURE_INDEX=$((FIXTURE_INDEX + 1))
  stamp=$(printf '20261006T12%02d00Z' "$FIXTURE_INDEX")
  state="$TMP/$name-state"
  bin="$TMP/$name-bin"
  checkout="$TMP/$name-checkout"
  root="/root/main1238-${TARGET_SHA:0:12}-$stamp"
  mkdir -m 700 "$state" "$checkout"
  mkdir -p -m 700 "$checkout/deploy/teagramd/rollout-runner" "$checkout/cmd" "$checkout/internal" "$checkout/components" "$checkout/utils"
  source_root=$(cd "$SCRIPT_DIR/../../.." && pwd -P)
  cp -a "$source_root/migrations" "$checkout/migrations"
  cp "$SCRIPT_DIR/rollout-runner.sh" "$SCRIPT_DIR/rollout-verifier.sh" \
    "$SCRIPT_DIR/schema-result-gate.sh" "$SCRIPT_DIR/schema-result-gate.py" "$MODE_HELPER" \
    "$SCRIPT_DIR/qualify-rustfs-transition.py" "$STACK_MOVE_HELPER" "$checkout/deploy/teagramd/rollout-runner/"
  cp "$SCRIPT_DIR/local-compose-e58ba20.yml" "$SCRIPT_DIR/local-compose-598359e.yml" \
    "$SCRIPT_DIR/local-compose-df9ffe5.yml" "$SCRIPT_DIR/local-compose-5602c7c.yml" \
    "$SCRIPT_DIR/local-compose-cce00d1.yml" \
    "$checkout/deploy/teagramd/rollout-runner/"
  chmod 600 "$checkout/deploy/teagramd/rollout-runner/"*.sh "$checkout/deploy/teagramd/rollout-runner/"*.py
  target_runtime="$state/target-runtime"
  mkdir -m 700 "$target_runtime"
  cp "$SCRIPT_DIR/rollout-runner.sh" "$SCRIPT_DIR/rollout-verifier.sh" \
    "$SCRIPT_DIR/schema-result-gate.sh" "$SCRIPT_DIR/schema-result-gate.py" "$MODE_HELPER" \
    "$SCRIPT_DIR/qualify-rustfs-transition.py" "$STACK_MOVE_HELPER" "$target_runtime/"
  cp "$SCRIPT_DIR/local-compose-e58ba20.yml" "$SCRIPT_DIR/local-compose-598359e.yml" \
    "$SCRIPT_DIR/local-compose-df9ffe5.yml" "$SCRIPT_DIR/local-compose-5602c7c.yml" \
    "$SCRIPT_DIR/local-compose-cce00d1.yml" \
    "$target_runtime/"
  chmod 600 "$target_runtime/"*.sh "$target_runtime/"*.py
  printf '%s\n' "$BASELINE_SHA" > "$state/head"
  printf '%s\n' "$TARGET_SHA" > "$state/origin"
  printf '%s\n' baseline > "$state/phase"
  printf '%s\n' "$BASE_ID" > "$state/teagramd"
  printf '%s\n' "$BASE_IMAGE" > "$state/tag"
  printf '%s\n' 'fixture baseline source' > "$checkout/tracked-source.txt"
  env_file="$checkout/.env"
  override="$checkout/docker-compose.override.yml"
  if [ "$scenario" = initial-local-target-compose-render ]; then
    printf 'FIXTURE=synthetic-only\nPOSTGRES_PASSWORD=fixture-password\nTG_PUBLIC_LINK_PREFIX=https://links.example.invalid/\n' > "$env_file"
  elif [[ "$name" == moved-stack-* ]]; then
    printf 'FIXTURE=synthetic-only\nTEAGRAMD_STACK_MOVE_BLOB_DIR=/var/lib/teagramd-blobs\n' > "$env_file"
  else
    printf 'FIXTURE=synthetic-only\n' > "$env_file"
  fi
  if [ "$scenario" = initial-local-target-compose-render ]; then
    printf 'services: {}\n' > "$override"
  else
    printf 'override: synthetic\n' > "$override"
  fi
  cp "$source_root/docker-compose.yml" "$source_root/docker-compose.local-blobs.yml" "$checkout/"
  cp "$SCRIPT_DIR/initial-local-compose-777742.yml" "$checkout/.rollout-compose.initial-local.yml"
  cp "$SCRIPT_DIR/local-compose-e58ba20.yml" "$checkout/.rollout-compose.local-e58ba20.yml"
  cp "$SCRIPT_DIR/local-compose-598359e.yml" "$checkout/.rollout-compose.local-598359e.yml"
  cp "$SCRIPT_DIR/local-compose-df9ffe5.yml" "$checkout/.rollout-compose.local-df9ffe5.yml"
  cp "$SCRIPT_DIR/local-compose-5602c7c.yml" "$checkout/.rollout-compose.local-5602c7c.yml"
  cp "$SCRIPT_DIR/local-compose-cce00d1.yml" "$checkout/.rollout-compose.local-cce00d1.yml"
  chmod 600 "$checkout/.rollout-compose.initial-local.yml" "$checkout/.rollout-compose.local-e58ba20.yml" \
    "$checkout/.rollout-compose.local-598359e.yml" "$checkout/.rollout-compose.local-df9ffe5.yml" \
    "$checkout/.rollout-compose.local-5602c7c.yml" "$checkout/.rollout-compose.local-cce00d1.yml"
  chmod 600 "$env_file" "$override"
  base_config="$state/base-compose.json"
  target_config="$state/target-compose.json"
  write_compose_fixture "$checkout" "$scenario" "$base_config" "$target_config"
  if [ "$scenario" = initial-local-target-compose-render ]; then
    "$REAL_DOCKER" compose --project-directory "$checkout" -p fixture \
      -f "$checkout/.rollout-compose.initial-local.yml" -f "$override" \
      config --format json > "$target_config"
  fi
  : > "$TMP/$name-events"
  : > "$TMP/$name-compose-selections"
  write_mock_commands "$bin"
  printf '%s\n' "$state" > "$TMP/$name-state-path"
  printf '%s\n' "$bin" > "$TMP/$name-bin-path"
  printf '%s\n' "$checkout" > "$TMP/$name-checkout-path"
  printf '%s\n' "$checkout/deploy/teagramd/rollout-runner" > "$TMP/$name-runtime-path"
  printf '%s\n' "$target_runtime" > "$TMP/$name-target-runtime-path"
  printf '%s\n' "$TARGET_SHA" > "$TMP/$name-target-sha-path"
  printf '%s\n' "$BASELINE_SHA" > "$TMP/$name-baseline-sha-path"
  printf '%s\n' "$root" > "$TMP/$name-root-path"
  printf '%s\n' "$stamp" > "$TMP/$name-stamp"
  printf '%s\n' "$scenario" > "$TMP/$name-scenario"
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
    printf '%s\n' 'services:' '  teagramd:' '    environment:' '      TG_BLOB_S3_ENDPOINT: ""' > "$override"
    chmod 600 "$override"
  elif [ "$scenario" = forbidden-tgblobs-mount ]; then
    printf '%s\n' 'services:' '  teagramd:' '    volumes:' '      - tgblobs:/var/lib/teagramd-blobs:ro' > "$override"
    chmod 600 "$override"
  fi
}

git_for_fixture() {
  local source_root=$1
  shift
  git -c "safe.directory=$source_root" -c "safe.directory=$source_root/.git" "$@"
}

make_real_git_fixture() {
  local name=$1 scenario=$2 source_root origin checkout runtime_dir state bin target_sha target_tree
  local baseline_tree baseline_sha target_commit index tracked_path author_header committer_header git_config
  local root stamp env_file override base_config target_config
  source_root=$(cd "$SCRIPT_DIR/../../.." && pwd -P)
  target_sha=$(git_for_fixture "$source_root" -C "$source_root" rev-parse HEAD)
  target_tree=$(git_for_fixture "$source_root" -C "$source_root" rev-parse "$target_sha^{tree}")
  origin="$TMP/$name-origin.git"
  checkout="$TMP/$name-checkout"
  runtime_dir="$TMP/$name-runtime"
  state="$TMP/$name-state"
  bin="$TMP/$name-bin"
  index="$TMP/$name-index"
  git_config="$TMP/$name-gitconfig"
  FIXTURE_INDEX=$((FIXTURE_INDEX + 1))
  stamp=$(printf '20261007T16%02d00Z' "$FIXTURE_INDEX")
  root="/root/main1238-${target_sha:0:12}-$stamp"
  mkdir -m 700 "$state"

  printf '[safe]\n\tdirectory = %s/.git\n' "$source_root" > "$git_config"
  chmod 600 "$git_config"
  GIT_CONFIG_GLOBAL="$git_config" git clone --shared "$source_root" "$origin" >/dev/null
  GIT_INDEX_FILE="$index" git -C "$origin" read-tree "$target_tree"
  for tracked_path in \
    deploy/teagramd/rollout-runner/rollout-runner.sh \
    deploy/teagramd/rollout-runner/rollout-verifier.sh \
    deploy/teagramd/rollout-runner/schema-result-gate.sh \
    deploy/teagramd/rollout-runner/schema-result-gate.py \
    deploy/teagramd/rollout-runner/blob-mode-state.py \
    deploy/teagramd/rollout-runner/stack-move-runner.py; do
    mode=$(git_for_fixture "$source_root" -C "$source_root" ls-tree "$target_sha" -- "$tracked_path" | awk '{print $1}')
    if [ -z "$mode" ] && [ "$tracked_path" = deploy/teagramd/rollout-runner/stack-move-runner.py ]; then
      mode=100644
    fi
    [ -n "$mode" ] || continue
    blob=$(git -C "$origin" hash-object -w "$SCRIPT_DIR/${tracked_path##*/}")
    GIT_INDEX_FILE="$index" git -C "$origin" update-index --add --cacheinfo "$mode,$blob,$tracked_path"
  done
  target_tree=$(GIT_INDEX_FILE="$index" git -C "$origin" write-tree)
  while IFS= read -r tracked_path; do
    GIT_INDEX_FILE="$index" git -C "$origin" update-index --force-remove -- "$tracked_path"
  done < <(git_for_fixture "$source_root" -C "$source_root" ls-tree -r --name-only "$target_sha" -- deploy/teagramd/rollout-runner)
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
  git -C "$origin" update-ref refs/heads/main "$target_commit"
  git -C "$origin" symbolic-ref HEAD refs/heads/main
  git clone --shared --branch main "$origin" "$checkout" >/dev/null
  git -C "$checkout" update-ref refs/heads/main "$baseline_sha"
  git -C "$checkout" reset --hard "$baseline_sha" >/dev/null
  mkdir -m 700 "$runtime_dir"
  cp "$SCRIPT_DIR/rollout-runner.sh" "$SCRIPT_DIR/rollout-verifier.sh" \
    "$SCRIPT_DIR/schema-result-gate.sh" "$SCRIPT_DIR/schema-result-gate.py" "$MODE_HELPER" \
    "$SCRIPT_DIR/qualify-rustfs-transition.py" "$STACK_MOVE_HELPER" "$runtime_dir/"
  chmod 600 "$runtime_dir/"*.sh "$runtime_dir/"*.py
  if [ "$scenario" = source-mismatch ]; then
    printf '%s\n' '# fixture source mismatch' >> "$runtime_dir/rollout-runner.sh"
  fi

  printf '%s\n' "$baseline_sha" > "$state/head"
  printf '%s\n' "$target_commit" > "$state/origin"
  printf '%s\n' baseline > "$state/phase"
  printf '%s\n' "$BASE_ID" > "$state/teagramd"
  printf '%s\n' "$BASE_IMAGE" > "$state/tag"
  env_file="$checkout/.env"
  override="$checkout/docker-compose.override.yml"
  printf 'FIXTURE=synthetic-only\n' > "$env_file"
  printf 'override: synthetic\n' > "$override"
  cp "$SCRIPT_DIR/initial-local-compose-777742.yml" "$checkout/.rollout-compose.initial-local.yml"
  chmod 600 "$checkout/.rollout-compose.initial-local.yml"
  chmod 600 "$env_file" "$override"
  base_config="$state/base-compose.json"
  target_config="$state/target-compose.json"
  write_compose_fixture "$checkout" "$scenario" "$base_config" "$target_config"
  : > "$TMP/$name-events"
  : > "$TMP/$name-compose-selections"
  write_mock_commands "$bin"
  rm -- "$bin/git"
  printf '%s\n' "$state" > "$TMP/$name-state-path"
  printf '%s\n' "$bin" > "$TMP/$name-bin-path"
  printf '%s\n' "$checkout" > "$TMP/$name-checkout-path"
  printf '%s\n' "$runtime_dir" > "$TMP/$name-runtime-path"
  printf '%s\n' "$runtime_dir" > "$TMP/$name-target-runtime-path"
  printf '%s\n' "$target_commit" > "$TMP/$name-target-sha-path"
  printf '%s\n' "$baseline_sha" > "$TMP/$name-baseline-sha-path"
  printf '%s\n' "$(command -v git)" > "$TMP/$name-real-git-path"
  printf '%s\n' "$root" > "$TMP/$name-root-path"
  printf '%s\n' "$stamp" > "$TMP/$name-stamp"
  printf '%s\n' "$scenario" > "$TMP/$name-scenario"
}

make_fixed_target_transition_fixture() {
  local name=$1 source_root origin checkout runtime_dir state bin index git_config target_sha baseline_sha
  local runtime_source_sha runtime_tree author_header committer_header tracked_path mode blob root stamp
  local env_file override base_config target_config
  source_root=$(cd "$SCRIPT_DIR/../../.." && pwd -P)
  target_sha=$INITIAL_LOCAL_TARGET_SHA
  baseline_sha=$INITIAL_LOCAL_LEGACY_BASELINE_SHA
  origin="$TMP/$name-origin.git"
  checkout="$TMP/$name-checkout"
  runtime_dir="$TMP/$name-runtime"
  state="$TMP/$name-state"
  bin="$TMP/$name-bin"
  index="$TMP/$name-index"
  git_config="$TMP/$name-gitconfig"
  FIXTURE_INDEX=$((FIXTURE_INDEX + 1))
  stamp=$(printf '20261009T18%02d00Z' "$FIXTURE_INDEX")
  root="/root/main1238-${target_sha:0:12}-$stamp"
  mkdir -m 700 "$state" "$runtime_dir"

  printf '[safe]\n\tdirectory = %s/.git\n' "$source_root" > "$git_config"
  chmod 600 "$git_config"
  # CI checkouts are shallow and may not contain these historical commits.
  # Fetch missing objects from the repository remote before sharing them with
  # the fixture, rather than asking local upload-pack for an unadvertised SHA.
  for revision in "$baseline_sha" "$target_sha"; do
    if ! git_for_fixture "$source_root" -C "$source_root" cat-file -e "$revision^{commit}" 2>/dev/null; then
      git_for_fixture "$source_root" -C "$source_root" fetch -q --no-tags origin "$revision" || {
        printf 'cannot fetch required fixture commit from origin: %s\n' "$revision" >&2
        exit 1
      }
    fi
  done
  GIT_CONFIG_GLOBAL="$git_config" git clone --shared --bare "$source_root" "$origin" >/dev/null
  mkdir -p "$origin/objects/info"
  printf '%s\n' "$source_root/.git/objects" > "$origin/objects/info/alternates"
  git -C "$origin" update-ref refs/fixture/legacy "$baseline_sha"
  GIT_INDEX_FILE="$index" git -C "$origin" read-tree "$target_sha^{tree}"
  for tracked_path in \
    deploy/teagramd/rollout-runner/rollout-runner.sh \
    deploy/teagramd/rollout-runner/rollout-verifier.sh \
    deploy/teagramd/rollout-runner/schema-result-gate.sh \
    deploy/teagramd/rollout-runner/schema-result-gate.py \
    deploy/teagramd/rollout-runner/blob-mode-state.py \
    deploy/teagramd/rollout-runner/qualify-rustfs-transition.py \
    deploy/teagramd/rollout-runner/stack-move-runner.py; do
    mode=$(git -C "$origin" ls-tree "$target_sha" -- "$tracked_path" | awk '{print $1}')
    if [ -z "$mode" ]; then
      # The immutable target predates the renamed runtime path; use the
      # checked-in runtime file's mode for the synthetic overlay.
      mode=$(git -C "$source_root" ls-files --stage -- "$tracked_path" | awk '{print $1}')
    fi
    [ -n "$mode" ] || { printf 'fixed target is missing runtime path: %s\n' "$tracked_path" >&2; exit 1; }
    blob=$(git -C "$origin" hash-object -w "$SCRIPT_DIR/${tracked_path##*/}")
    GIT_INDEX_FILE="$index" git -C "$origin" update-index --add --cacheinfo "$mode,$blob,$tracked_path"
  done
  runtime_tree=$(GIT_INDEX_FILE="$index" git -C "$origin" write-tree)
  rm -- "$index"
  author_header=$(git -C "$origin" cat-file commit "$target_sha" | sed -n '/^author /p')
  committer_header=$(git -C "$origin" cat-file commit "$target_sha" | sed -n '/^committer /p')
  runtime_source_sha=$(
    {
      printf 'tree %s\nparent %s\n%s\n%s\n\nfixture reviewed external rollout runtime source\n' \
        "$runtime_tree" "$target_sha" "$author_header" "$committer_header"
    } | git -C "$origin" hash-object -t commit -w --stdin
  )
  git -C "$origin" update-ref refs/heads/main "$runtime_source_sha"
  git -C "$origin" symbolic-ref HEAD refs/heads/main
  git clone --shared --branch main "$origin" "$checkout" >/dev/null
  git -C "$checkout" fetch origin refs/fixture/legacy:refs/fixture/legacy >/dev/null
  git -C "$checkout" reset --hard "$baseline_sha" >/dev/null

  cp "$SCRIPT_DIR/rollout-runner.sh" "$SCRIPT_DIR/rollout-verifier.sh" \
    "$SCRIPT_DIR/schema-result-gate.sh" "$SCRIPT_DIR/schema-result-gate.py" "$MODE_HELPER" \
    "$SCRIPT_DIR/qualify-rustfs-transition.py" "$STACK_MOVE_HELPER" "$runtime_dir/"
  chmod 600 "$runtime_dir/"*.sh "$runtime_dir/"*.py
  printf '%s\n' "$baseline_sha" > "$state/head"
  printf '%s\n' "$runtime_source_sha" > "$state/origin"
  printf '%s\n' baseline > "$state/phase"
  printf '%s\n' "$BASE_ID" > "$state/teagramd"
  printf '%s\n' "$BASE_IMAGE" > "$state/tag"
  env_file="$checkout/.env"
  override="$checkout/docker-compose.override.yml"
  printf 'FIXTURE=synthetic-only\n' > "$env_file"
  printf 'override: synthetic\n' > "$override"
  cp "$SCRIPT_DIR/initial-local-compose-777742.yml" "$checkout/.rollout-compose.initial-local.yml"
  chmod 600 "$checkout/.rollout-compose.initial-local.yml" "$env_file" "$override"
  base_config="$state/base-compose.json"
  target_config="$state/target-compose.json"
  write_compose_fixture "$checkout" initial-local-target-compose-render "$base_config" "$target_config"
  : > "$TMP/$name-events"
  : > "$TMP/$name-compose-selections"
  write_mock_commands "$bin"
  printf '%s\n' "$state" > "$TMP/$name-state-path"
  printf '%s\n' "$origin" > "$TMP/$name-origin-path"
  printf '%s\n' "$bin" > "$TMP/$name-bin-path"
  printf '%s\n' "$checkout" > "$TMP/$name-checkout-path"
  printf '%s\n' "$runtime_dir" > "$TMP/$name-runtime-path"
  printf '%s\n' "$runtime_dir" > "$TMP/$name-target-runtime-path"
  printf '%s\n' "$target_sha" > "$TMP/$name-target-sha-path"
  printf '%s\n' "$baseline_sha" > "$TMP/$name-baseline-sha-path"
  printf '%s\n' "$(command -v git)" > "$TMP/$name-real-git-path"
  printf '%s\n' "$root" > "$TMP/$name-root-path"
  printf '%s\n' "$stamp" > "$TMP/$name-stamp"
  printf '%s\n' initial-local-target-compose-render > "$TMP/$name-scenario"
  printf '%s\n' "$runtime_source_sha" > "$TMP/$name-runtime-source-sha-path"
}

run_fixture() {
  local name=$1 capture=${2:-built} fail_sync=${3:-0} chmod_match=${4:-} ready=${5:-2} ln_match=${6:-} sync_match=${7:-} action=${8:-initialize-local} runtime_source_sha=${9:-}
  local branch=main
  local state bin checkout root stamp scenario status require_marker=0 runner runtime_dir target_runtime target_sha baseline_sha real_git compose_file compose_env_files='' rustfs_private_network='' rustfs_volume=''
  local -a runner_args=() ambient_revision=() compose_env_args=()
  local replacement_id=$TARGET_ID added_env='' changed_env='' previous_env_file='' previous_override_file='' baseline_env_extra='' target_env_extra='' rollback_env_extra='' adopt_env=0
  [ ! -f "$TMP/$name-adopt-env" ] || adopt_env=$(cat "$TMP/$name-adopt-env")
  [ ! -f "$TMP/$name-added-env" ] || added_env=$(cat "$TMP/$name-added-env")
  [ ! -f "$TMP/$name-changed-env" ] || changed_env=$(cat "$TMP/$name-changed-env")
  [ ! -f "$TMP/$name-previous-env-path" ] || previous_env_file=$(cat "$TMP/$name-previous-env-path")
  [ ! -f "$TMP/$name-previous-override-path" ] || previous_override_file=$(cat "$TMP/$name-previous-override-path")
  [ ! -f "$TMP/$name-baseline-env-extra" ] || baseline_env_extra=$(cat "$TMP/$name-baseline-env-extra")
  [ ! -f "$TMP/$name-target-env-extra" ] || target_env_extra=$(cat "$TMP/$name-target-env-extra")
  [ ! -f "$TMP/$name-rollback-env-extra" ] || rollback_env_extra=$(cat "$TMP/$name-rollback-env-extra")
  state=$(cat "$TMP/$name-state-path")
  if [ -f "$state/replacement-id" ]; then replacement_id=$(cat "$state/replacement-id"); fi
  bin=$(cat "$TMP/$name-bin-path")
  checkout=$(cat "$TMP/$name-checkout-path")
  runtime_dir=$(cat "$TMP/$name-runtime-path")
  target_runtime=$(cat "$TMP/$name-target-runtime-path")
  target_sha=$(cat "$TMP/$name-target-sha-path")
  baseline_sha=$(cat "$TMP/$name-baseline-sha-path")
  if [ -f "$TMP/$name-runtime-source-sha-path" ]; then
    runtime_source_sha=$(cat "$TMP/$name-runtime-source-sha-path")
  fi
  [ -n "$runtime_source_sha" ] || runtime_source_sha=$target_sha
  real_git=$(cat "$TMP/$name-real-git-path" 2>/dev/null || true)
  stamp=$(cat "$TMP/$name-stamp")
  root="/root/main1238-${target_sha:0:12}-$stamp"
  printf '%s\n' "$root" > "$TMP/$name-root-path"
  scenario=$(cat "$TMP/$name-scenario")
  case "$scenario" in
    target-local-carry-forward-detached-checkout) branch='' ;;
    target-local-carry-forward-non-main-checkout) branch=fixture-feature ;;
  esac
  compose_file='.rollout-compose.initial-local.yml:docker-compose.override.yml'
  case "$scenario" in
    s3-accepted-apply)
      compose_file="$checkout/docker-compose.yml:$checkout/docker-compose.override.yml"
      compose_env_files=$(cat "$TMP/$name-compose-env-files")
      compose_env_args=("COMPOSE_ENV_FILES=$compose_env_files")
      rustfs_private_network=fixture_rustfs_private
      rustfs_volume=fixture_rustfsdata
      ;;
    recovered-local-apply)
      compose_file="$checkout/docker-compose.yml:$checkout/docker-compose.local-blobs.yml:$checkout/docker-compose.override.yml"
      rustfs_private_network=teagram_rustfs_private
      rustfs_volume=teagram_rustfsdata
      ;;
    stack-move-retained-rustfs-volume)
      compose_file='.rollout-compose.local-e58ba20.yml:docker-compose.override.yml'
      ;;
    target-local-source-selection-omits-override)
      compose_file='.rollout-compose.local-5602c7c.yml'
      ;;
    target-local-source-extra-compose-file)
      compose_file='.rollout-compose.local-5602c7c.yml:docker-compose.unapproved.yml:docker-compose.override.yml'
      ;;
    target-local-source-wrong-order)
      compose_file='docker-compose.override.yml:.rollout-compose.local-5602c7c.yml'
      ;;
    target-local-source-*)
      compose_file='.rollout-compose.local-5602c7c.yml:docker-compose.override.yml'
      ;;
    target-local-e58-artifact-with-598-target)
      compose_file='.rollout-compose.local-e58ba20.yml:docker-compose.override.yml'
      ;;
    target-local-598-artifact-with-df9-target)
      compose_file='.rollout-compose.local-598359e.yml:docker-compose.override.yml'
      ;;
    target-local-df9-selection-omits-override)
      compose_file='.rollout-compose.local-df9ffe5.yml'
      ;;
    target-local-df9-extra-compose-file)
      compose_file='.rollout-compose.local-df9ffe5.yml:docker-compose.unapproved.yml:docker-compose.override.yml'
      ;;
    target-local-df9-wrong-order)
      compose_file='docker-compose.override.yml:.rollout-compose.local-df9ffe5.yml'
      ;;
    target-local-carry-forward-selection-omits-override)
      compose_file='.rollout-compose.local-df9ffe5.yml'
      ;;
    target-local-carry-forward-extra-compose-file)
      compose_file='.rollout-compose.local-df9ffe5.yml:docker-compose.unapproved.yml:docker-compose.override.yml'
      ;;
    target-local-carry-forward-wrong-order)
      compose_file='docker-compose.override.yml:.rollout-compose.local-df9ffe5.yml'
      ;;
    target-local-carry-forward-cce-filename-df9)
      compose_file='.rollout-compose.local-df9ffe5.yml:docker-compose.override.yml'
      ;;
    target-local-carry-forward-df9-filename-cce)
      compose_file='.rollout-compose.local-cce00d1.yml:docker-compose.override.yml'
      ;;
    target-local-carry-forward-cce-*)
      compose_file='.rollout-compose.local-cce00d1.yml:docker-compose.override.yml'
      ;;
    target-local-carry-forward-*)
      compose_file='.rollout-compose.local-df9ffe5.yml:docker-compose.override.yml'
      ;;
    target-local-df9-*)
      compose_file='.rollout-compose.local-df9ffe5.yml:docker-compose.override.yml'
      ;;
    target-local-cce-selection-omits-override)
      compose_file='.rollout-compose.local-cce00d1.yml'
      ;;
    target-local-cce-extra-compose-file)
      compose_file='.rollout-compose.local-cce00d1.yml:docker-compose.unapproved.yml:docker-compose.override.yml'
      ;;
    target-local-cce-wrong-order)
      compose_file='docker-compose.override.yml:.rollout-compose.local-cce00d1.yml'
      ;;
    target-local-cce-*)
      compose_file='.rollout-compose.local-cce00d1.yml:docker-compose.override.yml'
      ;;
    target-local-df9-artifact-with-cce-target)
      compose_file='.rollout-compose.local-df9ffe5.yml:docker-compose.override.yml'
      ;;
    target-local-cce-artifact-with-df9-target)
      compose_file='.rollout-compose.local-cce00d1.yml:docker-compose.override.yml'
      ;;
    target-local-598-selection-omits-override)
      compose_file='.rollout-compose.local-598359e.yml'
      ;;
    target-local-598-extra-compose-file)
      compose_file='.rollout-compose.local-598359e.yml:docker-compose.unapproved.yml:docker-compose.override.yml'
      ;;
    target-local-598-wrong-order)
      compose_file='docker-compose.override.yml:.rollout-compose.local-598359e.yml'
      ;;
    target-local-598-*)
      compose_file='.rollout-compose.local-598359e.yml:docker-compose.override.yml'
      ;;
    moved-stack-9ef1110-to-da0107d*)
      compose_file="$checkout/docker-compose.yml:$checkout/docker-compose.local-blobs.yml:$checkout/docker-compose.override.yml"
      ;;
    target-local-selection-omits-override)
      compose_file='.rollout-compose.local-e58ba20.yml'
      ;;
    target-local-*)
      compose_file='.rollout-compose.local-e58ba20.yml:docker-compose.override.yml'
      ;;
    unpinned-local-arbitrary)
      compose_file='docker-compose.unreviewed.yml:docker-compose.override.yml'
      ;;
    compose-file-omits-override)
      compose_file='.rollout-compose.initial-local.yml'
      ;;
    initial-local-extra-compose-file)
      compose_file='.rollout-compose.initial-local.yml:docker-compose.yml:docker-compose.override.yml'
      ;;
    initial-local-compose-file-wrong-order)
      compose_file='docker-compose.override.yml:.rollout-compose.initial-local.yml'
      ;;
  esac
  if [ "$scenario" = target-local-source-ambient-different ] || \
     [ "$scenario" = target-local-cce-ambient-different ] || \
     [ "$scenario" = target-local-carry-forward-cce-literal-build-arg ]; then
    ambient_revision=(TEAGRAMD_SOURCE_REVISION=ambient-revision-must-not-win)
  fi
  runner="$runtime_dir/rollout-runner.sh"
  case "$scenario" in old-target-image|target-local-old-target-image-logging-mismatch|target-local-598-rollback|target-local-df9-rollback|target-local-welcome-channel-addition-rollback|config-drift|target-local-carry-forward-public-listener-wide-host|target-local-carry-forward-public-listener-other-port|public-websocket-enabled|readiness-timeout|initial-local-legacy-logging-readiness-timeout|logs-failed|runtime-target-exited|target-container-unavailable|target-local-carry-forward-image-reuse-stale-build|target-local-carry-forward-image-reuse-stop-timeout-mismatch|schema-post-failed-69|schema-post-missing-69) require_marker=1 ;; esac
  [ "$name" = marker-write-failed ] && require_marker=0
  [ -n "$chmod_match" ] && require_marker=1
  if [ "$action" = reconcile ]; then
    runner_args=("$action" "$target_sha")
  else
    runner_args=("$action" "$target_sha" "$baseline_sha")
  fi
  printf '%s\n' 0 > "$state/compose-config-count"
  printf 'fixture runner: %s action=%s\n' "$name" "$action" >&2
  set +e
  (cd "$checkout" && timeout --signal=TERM --kill-after=5s 180s env \
    -u COMPOSE_ENV_FILES -u TEAGRAMD_SOURCE_REVISION PATH="$bin:$PATH" "${compose_env_args[@]}" "${ambient_revision[@]}" \
    MOCK_REAL_BASH="$(command -v bash)" \
    MOCK_STATE="$state" MOCK_EVENTS="$TMP/$name-events" MOCK_COMPOSE_FILE_EVENTS="$TMP/$name-compose-selections" MOCK_SCENARIO="$scenario" MOCK_BRANCH="$branch" \
    MOCK_CHECKOUT="$checkout" MOCK_TARGET_SHA="$target_sha" MOCK_TARGET_RUNTIME_DIR="$target_runtime" MOCK_REAL_GIT="$real_git" \
    MOCK_SOURCE_SHA="$runtime_source_sha" MOCK_ORIGIN_SHA="$APPLY_ORIGIN_SHA" MOCK_BASELINE_SHA="$baseline_sha" ROLLOUT_RUNNER_SOURCE_SHA="$runtime_source_sha" \
    MOCK_BASE_ID="$BASE_ID" MOCK_TARGET_ID="$TARGET_ID" MOCK_REPLACEMENT_ID="$replacement_id" MOCK_ROLLBACK_ID="$ROLLBACK_ID" \
    MOCK_POSTGRES_ID="$POSTGRES_ID" MOCK_MIGRATE_ID="$MIGRATE_ID" MOCK_RUSTFS_ID="$RUSTFS_ID" \
    MOCK_RUSTFS_PRIVATE_NETWORK="$rustfs_private_network" MOCK_RUSTFS_VOLUME="$rustfs_volume" \
    MOCK_EXPECTED_COMPOSE_ENV_FILES="$compose_env_files" \
    MOCK_BASE_IMAGE="$BASE_IMAGE" MOCK_BUILT_IMAGE="$BUILT_IMAGE" MOCK_ACTUAL_TARGET_IMAGE="$BUILT_IMAGE" MOCK_POSTGRES_IMAGE="$POSTGRES_IMAGE" \
    MOCK_CAPTURE_IMAGE="$capture" MOCK_FAIL_SYNC="$fail_sync" MOCK_FAIL_CHMOD_MATCH="$chmod_match" MOCK_FAIL_LN_MATCH="$ln_match" MOCK_FAIL_SYNC_MATCH="$sync_match" \
    MOCK_REQUIRE_FAILURE_MARKER="$require_marker" MOCK_EVIDENCE_ROOT="$root" MOCK_BASE_ENV_EXTRA="$baseline_env_extra" MOCK_TARGET_ENV_EXTRA="$target_env_extra" MOCK_ROLLBACK_ENV_EXTRA="$rollback_env_extra" \
    MOCK_REAL_PYTHON3="$(command -v python3)" MOCK_REAL_CHMOD="$(command -v chmod)" MOCK_REAL_LN="$(command -v ln)" MOCK_REAL_DATE="$(command -v date)" MOCK_STAMP="$stamp" \
    ROLLOUT_RUNNER_TEST_MODE=1 ROLLOUT_RUNNER_CHECKOUT="$checkout" ROLLOUT_RUNNER_EVIDENCE_ROOT=/root \
    ROLLOUT_RUNNER_TEST_LOG_ARCHIVE="$TMP/$name-legacy.log" \
    ROLLOUT_RUNNER_TEST_CHECKOUT="$checkout" \
    ROLLOUT_RUNNER_LOCK_PATH="$TMP/$name.lock" ROLLOUT_RUNNER_ENV_FILE="$checkout/.env" \
    ROLLOUT_RUNNER_ADDED_ENV="$added_env" ROLLOUT_RUNNER_CHANGED_ENV="$changed_env" \
    ROLLOUT_RUNNER_PREVIOUS_ENV_FILE="$previous_env_file" \
    ROLLOUT_RUNNER_PREVIOUS_OVERRIDE_FILE="$previous_override_file" \
    ROLLOUT_RUNNER_ADOPT_ENV="$adopt_env" \
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

prepare_target_local_apply_fixture() {
  local name=$1 scenario=$2 target_sha=${3:-$E58_LOCAL_COMPOSE_TARGET_SHA} baseline_sha=${4:-$TARGET_SHA}
  local state checkout root stamp artifact_file target_runtime runner_copy altered_sha origin_sha runtime_source_sha
  origin_sha=$APPLY_TARGET_SHA
  runtime_source_sha=$APPLY_TARGET_SHA
  prepare_apply_fixture "$name" success || return 1
  state=$(cat "$TMP/$name-state-path")
  checkout=$(cat "$TMP/$name-checkout-path")
  target_runtime=$(cat "$TMP/$name-target-runtime-path")
  root=$(cat "$TMP/$name-root-path")
  stamp=$(cat "$TMP/$name-stamp")
  case "$scenario" in
    target-local-source-*) artifact_file=.rollout-compose.local-5602c7c.yml ;;
    target-local-598-*) artifact_file=.rollout-compose.local-598359e.yml ;;
    target-local-df9-*) artifact_file=.rollout-compose.local-df9ffe5.yml ;;
    target-local-cce-*) artifact_file=.rollout-compose.local-cce00d1.yml ;;
    target-local-carry-forward-cce-filename-df9) artifact_file=.rollout-compose.local-df9ffe5.yml ;;
    target-local-carry-forward-df9-filename-cce) artifact_file=.rollout-compose.local-cce00d1.yml ;;
    target-local-carry-forward-cce-*) artifact_file=.rollout-compose.local-cce00d1.yml ;;
    target-local-carry-forward-*) artifact_file=.rollout-compose.local-df9ffe5.yml ;;
    target-local-e58-*) artifact_file=.rollout-compose.local-e58ba20.yml ;;
    *) artifact_file=.rollout-compose.local-e58ba20.yml ;;
  esac
  : > "$TMP/$name-compose-selections" || return 1
  printf '%s\n' "$target_sha" > "$TMP/$name-target-sha-path" || return 1
  printf '%s\n' "$baseline_sha" > "$TMP/$name-baseline-sha-path" || return 1
  printf '%s\n' "$baseline_sha" > "$state/head" || return 1
  case "$scenario" in
    target-local-carry-forward-cce-source-off-main)
      origin_sha=$APPLY_TARGET_SHA
      runtime_source_sha=$APPLY_SOURCE_SHA
      ;;
    target-local-carry-forward-cce-target-not-ancestor-source)
      origin_sha=$APPLY_ORIGIN_SHA
      runtime_source_sha=$APPLY_SOURCE_SHA
      ;;
    target-local-carry-forward-cce-*)
      origin_sha=$APPLY_SOURCE_SHA
      runtime_source_sha=$APPLY_SOURCE_SHA
      ;;
    target-local-cce-historical-over-live-baseline)
      origin_sha=$target_sha
      runtime_source_sha=$target_sha
      ;;
  esac
  printf '%s\n' "$origin_sha" > "$state/origin" || return 1
  printf '%s\n' "$runtime_source_sha" > "$TMP/$name-runtime-source-sha-path" || return 1
  printf '%s\n' "/root/main1238-${target_sha:0:12}-$stamp" > "$TMP/$name-root-path" || return 1
  printf '%s\n' "$scenario" > "$TMP/$name-scenario" || return 1
  case "$scenario" in
    target-local-carry-forward-cce-filename-df9)
      cp -- "$SCRIPT_DIR/local-compose-cce00d1.yml" "$checkout/$artifact_file" || return 1
      ;;
    target-local-carry-forward-df9-filename-cce)
      cp -- "$SCRIPT_DIR/local-compose-df9ffe5.yml" "$checkout/$artifact_file" || return 1
      ;;
    target-local-carry-forward-cce-changed-byte)
      sed -i 's/stop_grace_period: 120s/stop_grace_period: 90s/' "$checkout/$artifact_file" || return 1
      ;;
    target-local-carry-forward-cce-source-artifact-missing)
      rm -- "$target_runtime/local-compose-cce00d1.yml" || return 1
      ;;
    target-local-carry-forward-cce-source-artifact-different)
      sed -i 's/stop_grace_period: 120s/stop_grace_period: 90s/' \
        "$target_runtime/local-compose-cce00d1.yml" || return 1
      ;;
    target-local-carry-forward-cce-source-runner-different)
      printf '%s\n' '# changed runner at reviewed source' >> "$target_runtime/rollout-runner.sh"
      ;;
  esac
  case "$scenario" in
    target-local-render-backend-mismatch|target-local-598-render-backend-mismatch|target-local-df9-render-backend-mismatch)
      jq -c '
        .services.teagramd.environment.TG_BLOB_S3_ENDPOINT="https://objects.fixture.invalid" |
        .services.teagramd.environment.TG_BLOB_S3_BUCKET="fixture-bucket" |
        .services.teagramd.environment.TG_BLOB_S3_PREFIX="fixture/"
      ' "$state/target-compose.json" > "$state/target-compose.next.json" || return 1
      mv -- "$state/target-compose.next.json" "$state/target-compose.json" || return 1
      ;;
    target-local-render-volume-mismatch|target-local-598-render-volume-mismatch|target-local-df9-render-volume-mismatch)
      jq -c '.volumes.tgblobs.name="unexpected_tgblobs"' "$state/target-compose.json" \
        > "$state/target-compose.next.json" || return 1
      mv -- "$state/target-compose.next.json" "$state/target-compose.json" || return 1
      ;;
    target-local-wrong-artifact)
      printf '%s\n' 'tampered artifact' >> "$checkout/$artifact_file"
      ;;
    target-local-598-wrong-digest)
      printf '%s\n' 'tampered artifact' >> "$checkout/$artifact_file"
      ;;
    target-local-df9-wrong-digest)
      printf '%s\n' 'tampered artifact' >> "$checkout/$artifact_file"
      ;;
    target-local-source-wrong-digest)
      printf '%s\n' 'tampered artifact' >> "$checkout/$artifact_file"
      cp -- "$checkout/$artifact_file" "$target_runtime/local-compose-5602c7c.yml" || return 1
      ;;
    target-local-source-tampered-content)
      sed -i 's/stop_grace_period: 120s/stop_grace_period: 90s/' "$checkout/$artifact_file" || return 1
      cp -- "$checkout/$artifact_file" "$target_runtime/local-compose-5602c7c.yml" || return 1
      altered_sha=$(sha256sum "$checkout/$artifact_file") || return 1
      altered_sha=${altered_sha%% *}
      for runner_copy in \
        "$checkout/deploy/teagramd/rollout-runner/rollout-runner.sh" \
        "$target_runtime/rollout-runner.sh"; do
        sed -i "s/SOURCE_LOCAL_COMPOSE_SHA=$SOURCE_LOCAL_COMPOSE_ARTIFACT_SHA/SOURCE_LOCAL_COMPOSE_SHA=$altered_sha/" \
          "$runner_copy" || return 1
      done
      ;;
    target-local-source-tampered-body-pin)
      for runner_copy in \
        "$checkout/deploy/teagramd/rollout-runner/rollout-runner.sh" \
        "$target_runtime/rollout-runner.sh"; do
        sed -i "s/SOURCE_LOCAL_COMPOSE_CONTENT_SHA=$SOURCE_LOCAL_COMPOSE_CONTENT_SHA/SOURCE_LOCAL_COMPOSE_CONTENT_SHA=0000000000000000000000000000000000000000000000000000000000000000/" \
          "$runner_copy" || return 1
      done
      ;;
    target-local-cce-wrong-digest)
      printf '%s\n' 'tampered artifact' >> "$checkout/$artifact_file"
      cp -- "$checkout/$artifact_file" "$target_runtime/local-compose-cce00d1.yml" || return 1
      ;;
    target-local-cce-tampered-content)
      sed -i 's/stop_grace_period: 120s/stop_grace_period: 90s/' "$checkout/$artifact_file" || return 1
      cp -- "$checkout/$artifact_file" "$target_runtime/local-compose-cce00d1.yml" || return 1
      altered_sha=$(sha256sum "$checkout/$artifact_file") || return 1
      altered_sha=${altered_sha%% *}
      for runner_copy in \
        "$checkout/deploy/teagramd/rollout-runner/rollout-runner.sh" \
        "$target_runtime/rollout-runner.sh"; do
        sed -i "s/CCE_LOCAL_COMPOSE_SHA=$CCE_LOCAL_COMPOSE_ARTIFACT_SHA/CCE_LOCAL_COMPOSE_SHA=$altered_sha/" \
          "$runner_copy" || return 1
      done
      ;;
    target-local-cce-tampered-body-pin)
      for runner_copy in \
        "$checkout/deploy/teagramd/rollout-runner/rollout-runner.sh" \
        "$target_runtime/rollout-runner.sh"; do
        sed -i "s/CCE_LOCAL_COMPOSE_CONTENT_SHA=$CCE_LOCAL_COMPOSE_CONTENT_SHA/CCE_LOCAL_COMPOSE_CONTENT_SHA=0000000000000000000000000000000000000000000000000000000000000000/" \
          "$runner_copy" || return 1
      done
      ;;
    target-local-cce-source-provenance)
      printf '%s\n' '# changed outside the reviewed source revision' >> "$target_runtime/local-compose-cce00d1.yml"
      ;;
    target-local-598-symlink)
      mv -- "$checkout/$artifact_file" "$checkout/$artifact_file.real" || return 1
      ln -s -- "$artifact_file.real" "$checkout/$artifact_file" || return 1
      ;;
    target-local-598-non-root-owner)
      chown 1:1 "$checkout/$artifact_file" || return 1
      ;;
    target-local-598-wrong-mode)
      chmod 640 "$checkout/$artifact_file" || return 1
      ;;
    target-local-df9-symlink)
      mv -- "$checkout/$artifact_file" "$checkout/$artifact_file.real" || return 1
      ln -s -- "$artifact_file.real" "$checkout/$artifact_file" || return 1
      ;;
    target-local-df9-non-root-owner)
      chown 1:1 "$checkout/$artifact_file" || return 1
      ;;
    target-local-df9-wrong-mode)
      chmod 640 "$checkout/$artifact_file" || return 1
      ;;
    target-local-carry-forward-header-change)
      sed -i '1c# Carried forward from the last reviewed local Compose content.' "$checkout/$artifact_file" || return 1
      ;;
    target-local-carry-forward-wrong-content)
      sed -i 's/stop_grace_period: 120s/stop_grace_period: 90s/' "$checkout/$artifact_file" || return 1
      ;;
    target-local-df9-extra-compose-file|target-local-carry-forward-extra-compose-file)
      printf 'services: {}\n' > "$checkout/docker-compose.unapproved.yml" || return 1
      ;;
    target-local-598-extra-compose-file|target-local-source-extra-compose-file|target-local-cce-extra-compose-file)
      printf 'services: {}\n' > "$checkout/docker-compose.unapproved.yml" || return 1
      ;;
    target-local-missing-override|target-local-598-missing-override|target-local-df9-missing-override)
      rm -- "$checkout/docker-compose.override.yml"
      ;;
  esac
}

create_moved_blob_authority_fixture() {
  local test_module=$1 fixture_root=$2 state_dir=$3 report_root=$4
  python3 - "$test_module" "$fixture_root" "$state_dir" "$report_root" <<'PY'
import importlib.util
import pathlib
import shutil
import sys
from types import SimpleNamespace
from unittest.mock import patch

module_path = pathlib.Path(sys.argv[1])
spec = importlib.util.spec_from_file_location("blob_mode_fixtures", module_path)
assert spec is not None and spec.loader is not None
fixtures = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixtures)
blob_mode = fixtures.blob_mode
fixture = fixtures.BlobModeStateTests()
fixture.setUp()
root = pathlib.Path(sys.argv[2])
root.mkdir(mode=0o700, parents=True)
fixture.volume = blob_mode.STACK_MOVE_SOURCE_VOLUMES["tgblobs"]
legacy_blob_dir = blob_mode.MOVED_BLOB_TARGET
layouts = dict(blob_mode.RUNTIME_LAYOUTS)
layouts["teagramd"] = {**layouts["teagramd"], "blobs": legacy_blob_dir}
with (
    patch.object(blob_mode, "BLOB_TARGET", legacy_blob_dir),
    patch.object(blob_mode, "BLOB_TARGETS", {legacy_blob_dir}),
    patch.object(blob_mode, "RUNTIME_LAYOUTS", layouts),
):
    authority_state, authority_reports, lock_path = fixture.create_initial_authority(root)

inventories = {"source": {}, "target": {}}
for logical in blob_mode.STACK_MOVE_SOURCE_VOLUMES:
    line = blob_mode.hashlib.sha256(f"moved {logical}".encode()).hexdigest()
    for side in ("source", "target"):
        path = root / f"{logical}.{side}.sha256"
        path.write_text(f"{line}  ./fixture.bin\n", encoding="utf-8")
        path.chmod(0o600)
        inventories[side][logical] = path
with (
    patch.object(blob_mode, "require_runner_lock"),
    patch.object(blob_mode, "file_stat", side_effect=fixture.allow_unowned_private_files),
    patch.object(blob_mode.os, "fchown"),
    patch.object(blob_mode.os, "chown"),
):
    blob_mode.publish_local_stack_move(SimpleNamespace(
        state_dir=authority_state,
        report_root=authority_reports,
        lock_path=lock_path,
        source_inventories=inventories["source"],
        target_inventories=inventories["target"],
    ))

destination_state = pathlib.Path(sys.argv[3])
destination_state.parent.mkdir(mode=0o755, parents=True, exist_ok=True)
shutil.copytree(authority_state, destination_state)
destination_reports = pathlib.Path(sys.argv[4])
for item in authority_reports.iterdir():
    destination = destination_reports / item.name
    if item.is_dir():
        shutil.copytree(item, destination)
    else:
        shutil.copy2(item, destination)
PY
}

create_transition_authority_fixture() {
  local test_module=$1 fixture_root=$2 state_dir=$3 report_root=$4 outcome=$5 moved=${6:-0}
  python3 - "$test_module" "$fixture_root" "$state_dir" "$report_root" "$outcome" "$moved" <<'PY'
import importlib.util
import json
import pathlib
import sys
from types import SimpleNamespace
from unittest.mock import patch

module_path = pathlib.Path(sys.argv[1])
spec = importlib.util.spec_from_file_location("blob_mode_fixtures", module_path)
assert spec is not None and spec.loader is not None
fixtures = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixtures)
blob_mode = fixtures.blob_mode
fixture = fixtures.BlobModeStateTests()
fixture.setUp()
root = pathlib.Path(sys.argv[2])
root.mkdir(mode=0o700, parents=True)
state_dir = pathlib.Path(sys.argv[3])
report_root = pathlib.Path(sys.argv[4])
outcome = sys.argv[5]
moved = sys.argv[6] == "1"
records = [
    json.loads(path.read_text(encoding="utf-8"))
    for path in sorted((state_dir / "journal").glob("*.json"))
]
previous = records[-1]
fixture.volume = previous["volumes"]["tgblobs"]
local_blob_dir = blob_mode.MOVED_BLOB_TARGET if moved else blob_mode.BLOB_TARGET
layouts = dict(blob_mode.RUNTIME_LAYOUTS)
layouts["teagramd"] = {**layouts["teagramd"], "blobs": local_blob_dir}
rustfs_volume = previous["volumes"]["rustfsdata"] or (
    "teagram_rustfsdata" if moved else "fixture_rustfsdata"
)

with (
    patch.object(blob_mode, "BLOB_TARGET", local_blob_dir),
    patch.object(blob_mode, "BLOB_TARGETS", {local_blob_dir}),
    patch.object(blob_mode, "RUNTIME_LAYOUTS", layouts),
    patch.object(blob_mode, "require_runner_lock"),
    patch.object(blob_mode, "file_stat", side_effect=fixture.allow_unowned_private_files),
    patch.object(blob_mode.os, "fchown"),
    patch.object(blob_mode.os, "chown"),
):
    outcomes = ["s3-accepted"] if outcome == "s3-accepted" else ["s3-accepted", "recovered-local"]
    for next_outcome in outcomes:
        records, _head, _mode_bytes = blob_mode.read_authority(state_dir, report_root)
        previous = records[-1]
        proof = fixture.transition_proof(next_outcome)
        proof["volumes"] = {
            "tgblobs": previous["volumes"]["tgblobs"],
            "rustfsdata": rustfs_volume,
        }
        if next_outcome == "recovered-local":
            proof["backend"] = previous_local = next(
                record["backend"] for record in reversed(records[:-1])
                if record["backend"].get("kind") == "local"
            )
            proof["backend"] = {"kind": "local", "dir": previous_local["dir"]}
        fixture.materialize_transition_proof(proof, report_root)
        proof_path = root / f"{next_outcome}.json"
        proof_path.write_text(json.dumps(proof), encoding="utf-8")
        proof_path.chmod(0o600)
        blob_mode.publish_transition(SimpleNamespace(
            outcome=next_outcome,
            proof=proof_path,
            state_dir=state_dir,
            report_root=report_root,
            lock_path=root / "deploy.lock",
        ))
PY
}

remove_authority_reports() {
  local state_dir=$1 report_root=$2 record transition report phase_dir
  for record in "$state_dir"/journal/*.json; do
    [ -f "$record" ] || continue
    transition=$(jq -er '.transition_id' "$record") || return 1
    report="$report_root/teagramd-blob-mode-report-$transition.json"
    [ -f "$report" ] || continue
    while IFS= read -r phase_dir; do
      case "$phase_dir" in
        transition-proof-fixtures-*) rm -rf -- "$report_root/$phase_dir" || return 1 ;;
        *) return 1 ;;
      esac
    done < <(jq -r '.phase_files[]? | split("/")[0]' "$report" | sort -u)
    rm -f -- "$report" || return 1
  done
}

configure_s3_compose_fixture() {
  local state=$1 rustfs_volume=$2 private_network=$3
  local config next
  for config in "$state/base-compose.json" "$state/target-compose.json"; do
    next="$config.next.json"
    jq -c --arg rustfs_volume "$rustfs_volume" --arg private_network "$private_network" '
      .services.teagramd.environment |= (
        del(.TG_BLOB_DIR) + {
          TG_BLOB_S3_ENDPOINT:"http://rustfs:9000",
          TG_BLOB_S3_BUCKET:"teagram",
          TG_BLOB_S3_PREFIX:"teagramd/",
          TG_BLOB_S3_REGION:"us-east-1",
          TG_BLOB_S3_ACCESS_KEY_ID:"fixture-access-key",
          TG_BLOB_S3_SECRET_ACCESS_KEY_FILE:"/run/secrets/teagramd-blob-secret-key",
          TG_BLOB_S3_ALLOW_INSECURE_HTTP:"true"
        }
      ) |
      .services.teagramd.networks += {rustfs_private:{}} |
      .services.teagramd.volumes |= map(
        if .target == "/var/lib/teagramd-blobs" then .read_only=true else . end
      ) |
      .services.rustfs={
        networks:{rustfs_private:{}},
        volumes:[{type:"volume",source:"rustfsdata",target:"/data",read_only:false}]
      } |
      .volumes.rustfsdata={name:$rustfs_volume} |
      .networks.rustfs_private={
        name:$private_network,
        driver:"bridge",
        internal:true,
        enable_ipv6:false,
        driver_opts:{"com.docker.network.bridge.gateway_mode_ipv4":"isolated"}
      }
    ' "$config" > "$next" || return 1
    mv -- "$next" "$config" || return 1
  done
}

prepare_moved_stack_apply_fixture() {
  local name=$1 scenario=$2
  local baseline_sha=9ef11103543557fa31475cb8edd51fa741099a8f
  local target_sha=da0107dfeadbeafbc1abfc4dd1594796ae661ed1
  local checkout state stamp previous_transition previous_record
  prepare_apply_fixture "$name" "$scenario" || return 1
  checkout=$(cat "$TMP/$name-checkout-path")
  state=$(cat "$TMP/$name-state-path")
  stamp=$(cat "$TMP/$name-stamp")
  previous_record="$checkout/.state/blob-mode/mode.json"
  previous_transition=$(jq -er '.transition_id' "$previous_record") || return 1
  rm -f -- "/root/teagramd-blob-mode-report-$previous_transition.json" || return 1
  rm -rf -- "$checkout/.state/blob-mode" || return 1
  create_moved_blob_authority_fixture \
    "$SCRIPT_DIR/test-blob-mode-state.py" "$TMP/$name-moved-authority" \
    "$checkout/.state/blob-mode" /root || return 1
  printf '%s\n' "$baseline_sha" > "$state/head" || return 1
  printf '%s\n' "$target_sha" > "$state/origin" || return 1
  printf '%s\n' "$baseline_sha" > "$TMP/$name-baseline-sha-path" || return 1
  printf '%s\n' "$target_sha" > "$TMP/$name-target-sha-path" || return 1
  printf '%s\n' "$target_sha" > "$TMP/$name-runtime-source-sha-path" || return 1
  printf '%s\n' "/root/main1238-${target_sha:0:12}-$stamp" > "$TMP/$name-root-path" || return 1
  printf '%s\n' "$scenario" > "$TMP/$name-scenario" || return 1
  : > "$TMP/$name-compose-selections" || return 1
}

prepare_target_local_uninitialized_fixture() {
  local name=$1 scenario=$2 target_sha=$3 state stamp
  make_fixture "$name" "$scenario" || return 1
  state=$(cat "$TMP/$name-state-path")
  stamp=$(cat "$TMP/$name-stamp")
  printf '%s\n' "$target_sha" > "$TMP/$name-target-sha-path" || return 1
  printf '%s\n' "$APPLY_TARGET_SHA" > "$state/origin" || return 1
  if [ "$target_sha" = "$TARGET_LOCAL_COMPOSE_TARGET_SHA" ] || [ "$target_sha" = "$E58_LOCAL_COMPOSE_TARGET_SHA" ] || [ "$target_sha" = "$DF9_LOCAL_COMPOSE_TARGET_SHA" ]; then
    printf '%s\n' "/root/main1238-${target_sha:0:12}-$stamp" > "$TMP/$name-root-path" || return 1
  fi
}

assert_target_local_rejected_before_backup() {
  local name=$1 expected_error=$2 status state root events head_before container_before runtime_source_sha
  state=$(cat "$TMP/$name-state-path")
  head_before=$(cat "$state/head")
  container_before=$(cat "$state/teagramd")
  runtime_source_sha=$(cat "$TMP/$name-runtime-source-sha-path" 2>/dev/null || printf '%s' "$APPLY_TARGET_SHA")
  status=$(run_fixture "$name" built 0 '' 2 '' '' apply "$runtime_source_sha")
  root=$(cat "$TMP/$name-root-path")
  events=$(cat "$TMP/$name-events")
  if [ "$status" != 0 ] && grep -q "$expected_error" "$TMP/$name.stderr" && \
     ! grep -Eq '^docker compose exec -T postgres pg_dump|^docker compose build|^docker compose up -d' "$TMP/$name-events" && \
     [ "$(cat "$state/head")" = "$head_before" ] && [ "$(cat "$state/teagramd")" = "$container_before" ] && \
     [ ! -e "$root.backup/backup-manifest.txt" ] && [ ! -e "$root.build/build-result.txt" ]; then
    pass "$name rejects before backup, build, or replacement"
  else
    printf 'target_local_reject_status=%s\ntarget_local_reject_stderr=%s\ntarget_local_reject_events=%s\n' \
      "$status" "$(cat "$TMP/$name.stderr")" "$events" >&2
    fail "$name must reject before backup, build, or replacement"
  fi
}

authority_fingerprint() {
  local state_dir=$1 report=$2
  find "$state_dir" -type f -print0 | sort -z | xargs -0 sha256sum
  sha256sum "$report"
}

show_fixture_failure() {
  local name=$1 status=$2 root checkout
  root=$(cat "$TMP/$name-root-path")
  checkout=$(cat "$TMP/$name-checkout-path")
  printf 'fixture_failure=%s status=%s\nfixture_stdout:\n' "$name" "$status" >&2
  cat "$TMP/$name.stdout" >&2
  printf 'fixture_stderr:\n' >&2
  cat "$TMP/$name.stderr" >&2
  if [ -f "$root.target/preflight-comparisons.tsv" ]; then
    printf 'preflight_comparisons:\n' >&2
    cat "$root.target/preflight-comparisons.tsv" >&2
  fi
  if [ -f "$root.target/target-comparisons.tsv" ]; then
    printf 'target_comparisons:\n' >&2
    cat "$root.target/target-comparisons.tsv" >&2
  fi
  if [ -f "$root.rollback/rollback-equivalence.tsv" ]; then
    printf 'rollback_equivalence:\n' >&2
    cat "$root.rollback/rollback-equivalence.tsv" >&2
  fi
  if [ -f "$root.baseline/env-baseline.txt" ]; then
    printf 'env_baseline:\n' >&2
    cat "$root.baseline/env-baseline.txt" >&2
  fi
  if [ -f "$root.target/target.snapshot.json" ]; then
    printf 'target_environment_evidence:\n' >&2
    jq '{env_sha256,container_env_sha256,container_env_added,container_env_changed}' \
      "$root.target/target.snapshot.json" >&2
  fi
  if [ -f "$checkout/.state/rollout-env" ]; then
    printf 'recorded_environment:\n' >&2
    cat "$checkout/.state/rollout-env" >&2
  fi
  printf 'fixture_events:\n' >&2
  cat "$TMP/$name-events" >&2
}

assert_initial_local_compose_rejected() {
  local name=$1 expected_error=$2 status checkout state root phase no_evidence=1
  status=$(run_fixture "$name")
  checkout=$(cat "$TMP/$name-checkout-path")
  state=$(cat "$TMP/$name-state-path")
  root=$(cat "$TMP/$name-root-path")
  for phase in baseline backup build target rollback; do
    [ ! -e "$root.$phase" ] || no_evidence=0
  done
  if [ "$status" != 0 ] && \
     grep -Fxq "rollout runner rejected: $expected_error" "$TMP/$name.stderr" && \
     [ ! -s "$TMP/$name-events" ] && [ "$no_evidence" = 1 ] && \
     [ ! -e "$checkout/.state/blob-mode" ] && \
     [ "$(cat "$state/head")" = "$BASELINE_SHA" ] && \
     [ "$(cat "$state/teagramd")" = "$BASE_ID" ]; then
    pass "$name rejects before rollout side effects"
  else
    show_fixture_failure "$name" "$status"
    fail "$name must reject before rollout side effects"
  fi
}

make_real_git_fixture real-git-source-mismatch source-mismatch
checkout=$(cat "$TMP/real-git-source-mismatch-checkout-path")
runtime_dir=$(cat "$TMP/real-git-source-mismatch-runtime-path")
target_sha=$(cat "$TMP/real-git-source-mismatch-target-sha-path")
baseline_sha=$(cat "$TMP/real-git-source-mismatch-baseline-sha-path")
root=$(cat "$TMP/real-git-source-mismatch-root-path")
if [ -z "$(git -C "$checkout" ls-tree "$baseline_sha" deploy/teagramd/rollout-runner)" ] && \
   [ -n "$(git -C "$checkout" ls-tree "$target_sha" deploy/teagramd/rollout-runner)" ]; then
  pass 'real-git baseline has no rollout runner while target tracks it'
else
  fail 'real-git fixture baseline and target shape'
fi
status=$(run_fixture real-git-source-mismatch)
no_evidence=1
for phase in baseline backup build target rollback; do
  [ ! -e "$root.$phase" ] || no_evidence=0
done
if [ "$status" != 0 ] && grep -q 'runtime copy differs from reviewed source revision' "$TMP/real-git-source-mismatch.stderr" && \
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
   [ -f "$checkout/deploy/teagramd/rollout-runner/rollout-runner.sh" ] && \
   [ "$(stat -c '%a' "$checkout/deploy/teagramd/rollout-runner/rollout-runner.sh")" = 755 ] && \
   [ "$(stat -c '%a' "$checkout/deploy/teagramd/rollout-runner/blob-mode-state.py")" = 644 ] && \
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

make_fixed_target_transition_fixture real-fixed-initial-local-transition
checkout=$(cat "$TMP/real-fixed-initial-local-transition-checkout-path")
origin=$(cat "$TMP/real-fixed-initial-local-transition-origin-path")
root=$(cat "$TMP/real-fixed-initial-local-transition-root-path")
baseline_sha=$(cat "$TMP/real-fixed-initial-local-transition-baseline-sha-path")
target_sha=$(cat "$TMP/real-fixed-initial-local-transition-target-sha-path")
runtime_source_sha=$(cat "$TMP/real-fixed-initial-local-transition-runtime-source-sha-path")
status=$(run_fixture real-fixed-initial-local-transition built 0 '' 2 '' '' initialize-local "$runtime_source_sha")
compose_selection=$(awk -F '\t' '{print $1}' "$TMP/real-fixed-initial-local-transition-compose-selections" | sort -u)
baseline_inspect_line=$(grep -n "^docker inspect $BASE_ID$" "$TMP/real-fixed-initial-local-transition-events" | head -n 1 | cut -d: -f1 || true)
initial_preflight_line=$(grep -n '^python3 blob-mode-state.pinned preflight-initial-local$' "$TMP/real-fixed-initial-local-transition-events" | cut -d: -f1 || true)
publish_line=$(grep -n '^python3 blob-mode-state.pinned initialize-local$' "$TMP/real-fixed-initial-local-transition-events" | cut -d: -f1 || true)
target_transition_line=$(awk -v target="$target_sha" '
  index($0, "git -C ") &&
    (index($0, " merge --ff-only -q " target) || index($0, " reset --hard -q " target)) {
    count++
    line=NR
  }
  END {if (count == 1) print line}
' "$TMP/real-fixed-initial-local-transition-events")
baseline_snapshot="$root.baseline/baseline.snapshot.json"
target_render_snapshot="$root.baseline/initial-local-target-render.snapshot.json"
provenance="$root.baseline/initial-local-provenance.txt"
if [ "$status" = 0 ] && grep -q 'rollout=verified' "$TMP/real-fixed-initial-local-transition.stdout" && \
   [ "$(git -C "$checkout" rev-parse HEAD)" = "$target_sha" ] && \
   [ "$baseline_sha" = "$INITIAL_LOCAL_LEGACY_BASELINE_SHA" ] && \
   [ "$target_sha" = "$INITIAL_LOCAL_TARGET_SHA" ] && \
   [ "$(git -C "$origin" show -s --format=%P "$runtime_source_sha")" = "$target_sha" ] && \
   [ "$compose_selection" = '.rollout-compose.initial-local.yml:docker-compose.override.yml' ] && \
   [ -n "$baseline_inspect_line" ] && [ -n "$initial_preflight_line" ] && [ -n "$publish_line" ] && [ -n "$target_transition_line" ] && \
   jq -e '.provenance.container_source == "docker-inspect" and .provenance.compose_source == "live-container-inspection" and .compose_replica_count == "not-captured" and .container_replica_count == "unset" and .container_client_addr_trust == "unset"' "$baseline_snapshot" >/dev/null && \
   jq -e '.provenance.compose_source == "selected-compose-render" and .provenance.compose_selection == ".rollout-compose.initial-local.yml:docker-compose.override.yml" and .compose_replica_count == "1" and .compose_client_addr_trust == "socket"' "$target_render_snapshot" >/dev/null && \
   grep -q 'baseline_source=running-unguarded-containers' "$provenance" && \
   grep -q 'target_source=pinned-target-compose' "$provenance" && \
   grep -q 'baseline_exposure_source=docker-inspect' "$provenance" && \
   grep -q 'baseline_key_mount_source=docker-inspect' "$provenance" && \
   grep -q 'baseline_environment_source=docker-inspect' "$provenance" && \
   grep -q 'baseline_pgdata_mount_source=docker-inspect' "$provenance" && \
   grep -q 'target_pgdata_source=pinned-target-compose' "$provenance" && \
   awk -v baseline_inspect="$baseline_inspect_line" -v initial_preflight="$initial_preflight_line" -v transition="$target_transition_line" -v target="$target_sha" -v publish="$publish_line" '
     /^docker compose config --format json$/ {
       if (NR <= baseline_inspect) next
       configs++
       if (configs == 1) first_config=NR
       if (configs == 2) second_config=NR
       if (configs == 4) post_reset_config=NR
       if (configs == 5) published_target_config=NR
     }
     /^docker compose exec -T postgres pg_dump -U postgres / {dump=NR}
     END {exit !(baseline_inspect < first_config && first_config < second_config && second_config < initial_preflight && initial_preflight < dump && dump < transition && transition < post_reset_config && post_reset_config < published_target_config && published_target_config < publish && configs >= 5)}
   ' "$TMP/real-fixed-initial-local-transition-events"; then
  pass 'real Git legacy 932994 to fixed target 777742 preserves capture order through publication'
else
  printf 'fixed_transition_status=%s\nfixed_transition_stderr=%s\nfixed_transition_events=%s\n' \
    "$status" "$(cat "$TMP/real-fixed-initial-local-transition.stderr")" \
    "$(cat "$TMP/real-fixed-initial-local-transition-events")" >&2
  printf 'fixed_transition_head=%s baseline=%s target=%s runtime_source=%s runtime_parent=%s compose_selection=%q baseline_inspect=%s preflight=%s publish=%s\n' \
    "$(git -C "$checkout" rev-parse HEAD)" "$baseline_sha" "$target_sha" "$runtime_source_sha" \
    "$(git -C "$origin" show -s --format=%P "$runtime_source_sha")" "$compose_selection" \
    "$baseline_inspect_line" "$initial_preflight_line" "$publish_line" >&2
  printf 'fixed_transition_baseline=%s\nfixed_transition_render=%s\nfixed_transition_provenance=%s\n' \
    "$(jq -c '{provenance,compose_replica_count,container_replica_count,container_client_addr_trust}' "$baseline_snapshot" 2>/dev/null || true)" \
    "$(jq -c '{provenance,compose_replica_count,compose_client_addr_trust}' "$target_render_snapshot" 2>/dev/null || true)" \
    "$(cat "$provenance" 2>/dev/null || true)" >&2
  grep -nE '^docker compose config --format json$|^docker compose exec -T postgres pg_dump|merge --ff-only -q |reset --hard -q |^python3 blob-mode-state.pinned preflight-initial-local$|^python3 blob-mode-state.pinned initialize-local$' \
    "$TMP/real-fixed-initial-local-transition-events" >&2
  fail 'real-Git fixed legacy-to-target transition, capture provenance, and publication order'
fi

make_fixed_target_transition_fixture real-fixed-initial-local-legacy-logging-rollback
printf '%s\n' initial-local-legacy-logging-readiness-timeout \
  > "$TMP/real-fixed-initial-local-legacy-logging-rollback-scenario"
runtime_source_sha=$(cat "$TMP/real-fixed-initial-local-legacy-logging-rollback-runtime-source-sha-path")
status=$(run_fixture real-fixed-initial-local-legacy-logging-rollback built 0 '' 2 '' '' initialize-local "$runtime_source_sha")
root=$(cat "$TMP/real-fixed-initial-local-legacy-logging-rollback-root-path")
state=$(cat "$TMP/real-fixed-initial-local-legacy-logging-rollback-state-path")
baseline_sha=$(cat "$TMP/real-fixed-initial-local-legacy-logging-rollback-baseline-sha-path")
rollback_rows="$root.rollback/rollback-equivalence.tsv"
preflight_rows="$root.target/preflight-comparisons.tsv"
default_json_logging_sha=$(printf '%s' '{"config":{},"type":"json-file"}' | sha256sum | awk '{print $1}')
approved_syslog_logging_sha=$(printf '%s' '{"config":{"syslog-address":"unixgram:///dev/log","syslog-facility":"daemon","syslog-format":"rfc5424","tag":"teagramd"},"type":"syslog"}' | sha256sum | awk '{print $1}')
if [ "$status" != 0 ] && grep -q 'rollback=verified' "$TMP/real-fixed-initial-local-legacy-logging-rollback.stdout" && \
   grep -Eq 'target_failure_gate=target_readiness_rejected_exit_[0-9]+ rollback_exit=0$' "$TMP/real-fixed-initial-local-legacy-logging-rollback.stdout" && \
   grep -q $'preflight\tinitial_local_logging_transition\tjson-file:true\tsyslog-v1:false\tpass' "$preflight_rows" && \
   awk -F '\t' -v expected="$default_json_logging_sha" \
     '$3 == "live_baseline_container_logging_sha256" && $4 == expected && $5 == expected && $6 == "pass" {found=1} END {exit !found}' \
     "$preflight_rows" && \
   grep -q 'reason=target_readiness_rejected_exit_' "$root.target/target-failure.txt" && \
   grep -q 'result=verified .*baseline_equivalence=pass' "$root.rollback/rollback-result.txt" && \
   grep -q $'rollback\tinitial_local_logging_transition\tjson-file:true\tsyslog-v1:false\tpass' "$rollback_rows" && \
   awk -F '\t' -v expected="$default_json_logging_sha" -v actual="$approved_syslog_logging_sha" \
     '$3 == "live_baseline_container_logging_sha256" && $4 == expected && $5 == actual && $6 == "pass" {found=1} END {exit !found}' \
     "$rollback_rows" && \
   grep -q $'rollback\tlive_baseline_container_log_login_codes\ttrue\tfalse\tpass' "$rollback_rows" && \
   jq -e --arg expected "$default_json_logging_sha" \
     '.container_log_login_codes == "true" and .container_logging_sha256 == $expected' \
     "$root.baseline/baseline.snapshot.json" >/dev/null && \
   jq -e --arg expected "$approved_syslog_logging_sha" \
     '.container_log_login_codes == "false" and .container_logging_sha256 == $expected' \
     "$root.rollback/rollback.snapshot.json" >/dev/null && \
   [ "$(cat "$state/head")" = "$baseline_sha" ] && [ "$(cat "$state/teagramd")" = "$ROLLBACK_ID" ]; then
  pass 'initial-local readiness failure verifies rollback across the approved legacy logging transition'
else
  show_fixture_failure real-fixed-initial-local-legacy-logging-rollback "$status"
  fail 'initial-local readiness failure must verify rollback from legacy json-file logging'
fi

make_fixture project-layout-change project-layout-changed
status=$(run_fixture project-layout-change built 0 '' 2 '' '' apply)
root=$(cat "$TMP/project-layout-change-root-path")
state=$(cat "$TMP/project-layout-change-state-path")
if [ "$status" != 0 ] && \
   grep -q 'Compose project or persistent mounts changed (reason=persistent_layout_mismatch components=server_mounts,volumes public_profile=not_applicable); use the explicit move-stack action' "$TMP/project-layout-change.stderr" && \
   grep -Fq 'layout_difference={"service":"teagramd","mount_target":"/var/lib/teagramd","field":"source"}' "$TMP/project-layout-change.stderr" && \
   grep -q '^python3 stack-move-runner.py inspect$' "$TMP/project-layout-change-events" && \
   ! grep -Eq '^docker compose exec -T postgres pg_dump|^docker compose (build|up)|^git .* (merge|reset)' "$TMP/project-layout-change-events" && \
   [ "$(cat "$state/head")" = "$BASELINE_SHA" ] && \
   [ ! -e "$root.backup" ] && [ ! -e "$root.target" ]; then
  pass 'apply logs a sanitized layout difference and rejects it before touching the live stack'
else
  show_fixture_failure project-layout-change "$status"
  fail 'apply must refuse a project or persistent-mount change before live-stack effects'
fi

for report_kind in missing malformed empty; do
  fixture_name="project-layout-report-$report_kind"
  make_fixture "$fixture_name" "$fixture_name"
  status=$(run_fixture "$fixture_name" built 0 '' 2 '' '' apply)
  root=$(cat "$TMP/$fixture_name-root-path")
  state=$(cat "$TMP/$fixture_name-state-path")
  if [ "$status" != 0 ] && \
     grep -q 'target Compose layout difference report is missing, malformed, or empty' "$TMP/$fixture_name.stderr" && \
     ! grep -q '^layout_difference=' "$TMP/$fixture_name.stderr" && \
     ! grep -q 'do-not-log-value' "$TMP/$fixture_name.stderr" && \
     grep -q '^python3 stack-move-runner.py inspect$' "$TMP/$fixture_name-events" && \
     ! grep -Eq '^docker compose exec -T postgres pg_dump|^docker compose (build|up)|^git .* (merge|reset)' "$TMP/$fixture_name-events" && \
     [ "$(cat "$state/head")" = "$BASELINE_SHA" ] && \
     [ ! -e "$root.backup" ] && [ ! -e "$root.target" ]; then
    pass "apply rejects a $report_kind layout difference report without logging unchecked fields"
  else
    show_fixture_failure "$fixture_name" "$status"
    fail "apply must reject a $report_kind layout difference report before live-stack effects"
  fi
done

make_fixture success built
status=$(run_fixture success)
root=$(cat "$TMP/success-root-path")
if [ "$status" = 0 ] && grep -q 'rollout=verified' "$TMP/success.stdout" && grep -q 'flock -x 9' "$TMP/success-events" && \
   awk 'index($0,"docker image inspect teagramd:local") {image=NR} $0=="docker compose up -d" {up=NR} END{exit !(image>0 && up>image)}' "$TMP/success-events" && \
   grep -q 'restore network none tmpfs only' "$TMP/success-events" && \
   awk '
     /^docker exec -i main1238-restore-[A-Za-z0-9-]+ psql -X -v ON_ERROR_STOP=1 -U postgres -d postgres$/ {globals_import++}
     /^docker exec restore globals role browser_acceptance_probe$/ {scratch_role_create++}
     /^docker compose exec .*CREATE ROLE browser_acceptance_probe / {compose_role_create++}
     END {exit !(globals_import == 1 && scratch_role_create == 1 && compose_role_create == 0)}
   ' "$TMP/success-events" && \
   find "$root".* -name built-image.identity -exec grep -q "source_sha=$TARGET_SHA image_id=$BUILT_IMAGE" {} \; && \
   assert_evidence_mode "$root"; then
  pass 'built image captured after build, bound to target SHA, and compared before acceptance'
else
  fail 'built image capture, provenance binding, and target acceptance'
fi

make_fixture pre-rollout-log-persisted pre-rollout-log-persisted
printf '# teagramd-container-id=previous-container\n2026-10-16T21:40:00Z msg="login code issued" code=old-secret-fixture\n' \
  > "$TMP/pre-rollout-log-persisted-legacy.log"
status=$(run_fixture pre-rollout-log-persisted)
legacy_archive="$TMP/pre-rollout-log-persisted-legacy.log"
reader_bin="$TMP/pre-rollout-log-reader-bin"
mkdir -m 700 "$reader_bin"
cat > "$reader_bin/logger" <<'SH'
#!/usr/bin/env sh
exit 0
SH
chmod 700 "$reader_bin/logger"
reader_script="$TMP/pre-rollout-log-reader.sh"
sed \
  -e "s|/root/teagramd-legacy-logs.log|$legacy_archive|g" \
  -e "s|/var/log/messages|$TMP/pre-rollout-log-messages|g" \
  "$SCRIPT_DIR/../../teagram/teagram-logs.sh" > "$reader_script"
chmod 700 "$reader_script"
retrieved=$(PATH="$reader_bin:$PATH" SSH_ORIGINAL_COMMAND='2000-01-01T00:00:00Z' "$reader_script")
if [ "$status" = 0 ] && grep -Fq 'pre-rollout-line' <<< "$retrieved" && \
   grep -Fq 'pre-rollout-stderr-line' <<< "$retrieved" && \
   ! grep -Fq 'stderr-secret-fixture-code' <<< "$retrieved" && \
   ! grep -Fq 'stderr-secret-fixture-code' "$legacy_archive" && \
   ! grep -Fq 'secret-fixture-code' "$legacy_archive" && \
   ! grep -Fq 'old-secret-fixture' "$legacy_archive" && \
   awk '/docker logs --since .*--timestamps --tail 5000/ {snapshot=NR} $0=="docker compose up -d" {up=NR} END{exit !(snapshot>0 && snapshot<up)}' \
     "$TMP/pre-rollout-log-persisted-events"; then
  pass 'pre-rollout logs are archived before the container is replaced'
else
  show_fixture_failure pre-rollout-log-persisted "$status"
  fail 'pre-rollout log persistence before container replacement'
fi

for restore_failure in restore-psql-failure restore-role-create-failure restore-rm-failure restore-remains-after-rm; do
  name="isolated-$restore_failure"
  make_fixture "$name" "$restore_failure"
  status=$(run_fixture "$name")
  root=$(cat "$TMP/$name-root-path")
  if [ "$status" != 0 ] && [ -f "$root.backup/backup-result.txt" ] && \
     [ ! -e "$root.backup/backup-manifest.txt" ] && \
     ! grep -q '^docker compose build' "$TMP/$name-events" && \
     ! grep -q '^build$' "$TMP/$name-events"; then
    pass "$restore_failure retains failure evidence and stops before build"
  else
    show_fixture_failure "$name" "$status"
    fail "$restore_failure must fail closed before build"
  fi
done

make_fixture missing-running-reference missing-running-reference
state=$(cat "$TMP/missing-running-reference-state-path")
checkout=$(cat "$TMP/missing-running-reference-checkout-path")
: > "$state/teagramd"
status=$(run_fixture missing-running-reference)
if [ "$status" != 0 ] && \
   grep -q 'baseline teagramd container ID is unavailable' "$TMP/missing-running-reference.stderr" && \
   ! grep -q 'pg_dump' "$TMP/missing-running-reference-events" && \
   ! grep -Eq '^docker compose (build|up)( |$)|^git merge --ff-only' "$TMP/missing-running-reference-events" && \
   [ ! -e "$checkout/.state/blob-mode" ] && \
   [ "$(cat "$state/head")" = "$BASELINE_SHA" ] && [ ! -s "$state/teagramd" ]; then
  pass 'missing running reference stops before backup, authority publication, checkout advance, or replacement'
else
  show_fixture_failure missing-running-reference "$status"
  fail 'missing running reference must reject before rollout side effects'
fi

make_fixture fixed-app-target-with-reviewed-tool-source success
state=$(cat "$TMP/fixed-app-target-with-reviewed-tool-source-state-path")
printf '%s\n' "$APPLY_TARGET_SHA" > "$state/origin"
status=$(run_fixture fixed-app-target-with-reviewed-tool-source built 0 '' 2 '' '' initialize-local "$APPLY_TARGET_SHA")
root=$(cat "$TMP/fixed-app-target-with-reviewed-tool-source-root-path")
compose_selection=$(awk -F '\t' '{print $1}' "$TMP/fixed-app-target-with-reviewed-tool-source-compose-selections" | sort -u)
if [ "$status" = 0 ] && grep -q "rollout=verified sha=$TARGET_SHA" "$TMP/fixed-app-target-with-reviewed-tool-source.stdout" && \
   [ "$(cat "$state/head")" = "$TARGET_SHA" ] && [ "$(cat "$state/origin")" = "$APPLY_TARGET_SHA" ] && \
   [ "$compose_selection" = '.rollout-compose.initial-local.yml:docker-compose.override.yml' ] && \
   grep -q "source_revision=$APPLY_TARGET_SHA" "$root.baseline/runtime-pins.txt"; then
  pass 'reviewed tool source runs the exact fixed application target with the pinned local Compose render'
else
  show_fixture_failure fixed-app-target-with-reviewed-tool-source "$status"
  fail 'runtime source and application target must remain independently pinned'
fi

make_fixture initial-local-target-compose-render initial-local-target-compose-render
state=$(cat "$TMP/initial-local-target-compose-render-state-path")
printf '%s\n' "$APPLY_TARGET_SHA" > "$state/origin"
printf '%s\n' "$INITIAL_LOCAL_TARGET_SHA" > "$TMP/initial-local-target-compose-render-target-sha-path"
status=$(run_fixture initial-local-target-compose-render built 0 '' 2 '' '' initialize-local "$APPLY_TARGET_SHA")
root=$(cat "$TMP/initial-local-target-compose-render-root-path")
checkout=$(cat "$TMP/initial-local-target-compose-render-checkout-path")
state=$(cat "$TMP/initial-local-target-compose-render-state-path")
transition=''
report=''
if [ -f "$checkout/.state/blob-mode/mode.json" ]; then
  transition=$(jq -er '.transition_id' "$checkout/.state/blob-mode/mode.json")
  report="/root/teagramd-blob-mode-report-$transition.json"
fi
preflight_line=$(grep -n '^python3 blob-mode-state.pinned preflight-initial-local$' "$TMP/initial-local-target-compose-render-events" | cut -d: -f1 || true)
dump_line=$(grep -n '^docker compose exec -T postgres pg_dump -U postgres ' "$TMP/initial-local-target-compose-render-events" | cut -d: -f1 || true)
merge_line=$(grep -n '^git merge --ff-only -q ' "$TMP/initial-local-target-compose-render-events" | cut -d: -f1 || true)
initialize_line=$(grep -n '^python3 blob-mode-state.pinned initialize-local$' "$TMP/initial-local-target-compose-render-events" | cut -d: -f1 || true)
if [ "$status" = 0 ] && grep -q 'rollout=verified' "$TMP/initial-local-target-compose-render.stdout" && \
   [ "$(cat "$state/head")" = "$INITIAL_LOCAL_TARGET_SHA" ] && \
   [ "$(cat "$state/teagramd")" = "$TARGET_ID" ] && \
   [ -n "$transition" ] && [ -f "$report" ] && \
   [ -n "$preflight_line" ] && [ -n "$dump_line" ] && [ -n "$merge_line" ] && [ -n "$initialize_line" ] && \
   [ "$preflight_line" -lt "$dump_line" ] && [ "$dump_line" -lt "$merge_line" ] && [ "$merge_line" -lt "$initialize_line" ] && \
   grep -q 'result=pass' "$root.baseline/initial-local-preflight.txt" && \
   grep -q $'preflight\tinitial_local_logging_transition\tsyslog-v1:false\tsyslog-v1:false\tpass' "$root.target/preflight-comparisons.tsv" && \
   grep -q 'baseline_source=running-unguarded-containers' "$root.baseline/initial-local-provenance.txt" && \
   grep -q 'target_source=pinned-target-compose' "$root.baseline/initial-local-provenance.txt" && \
   grep -q "target_artifact_sha256=$INITIAL_LOCAL_COMPOSE_ARTIFACT_SHA" "$root.baseline/initial-local-provenance.txt" && \
   jq -e '.services.telegramd != null and .services.teagramd == null' "$state/target-compose.json" >/dev/null && \
   grep -q 'baseline_pgdata_mount_source=docker-inspect' "$root.baseline/initial-local-provenance.txt" && \
   grep -q 'target_pgdata_source=pinned-target-compose' "$root.baseline/initial-local-provenance.txt" && \
   grep -q "target_sha=$INITIAL_LOCAL_TARGET_SHA" "$root.baseline/initial-local-preflight.txt" && \
   grep -q "source_revision=$APPLY_TARGET_SHA" "$root.baseline/runtime-pins.txt" && \
   jq -e --arg source "$checkout/.state/blob-mode" '
     .target.compose.services[0].name == "telegramd"
     and .target.compose.services[0].blob_mode_mounts == [{type:"bind",source:$source,target:"/run/telegramd/blob-mode",read_only:true}]
     and .target.compose.volumes.rustfsdata == null
     and any(.baseline.containers.containers[]; .service == "telegramd" and .mode_mounts == []
       and .ports == [{target:"2443",published:"2443",host_ip:"127.0.0.1",protocol:"tcp"}]
       and .tgkey_mounts == [{type:"volume",source:"fixture_tgkey",target:"/var/lib/telegramd",read_only:false}])
     and any(.baseline.containers.containers[]; .service == "postgres"
       and .pgdata_mounts == [{type:"volume",source:"fixture_pgdata",target:"/var/lib/postgresql/data",read_only:false}])
     and .target.compose.postgres_mounts == [{type:"volume",source:"fixture_pgdata",target:"/var/lib/postgresql/data",read_only:false}]
     and .inspection_kind == "unguarded-local-baseline-to-pinned-target"
   ' "$report" >/dev/null && \
   grep -q '^docker compose config --services$' "$TMP/initial-local-target-compose-render-events" && \
   grep -q '^docker compose build -q telegramd$' "$TMP/initial-local-target-compose-render-events" && \
   grep -q '^docker compose ps -q telegramd$' "$TMP/initial-local-target-compose-render-events" && \
   awk '/docker compose config --format json/ {if (!first) first=NR; last=NR} /^docker compose exec -T postgres pg_dump -U postgres / {dump=NR} END {exit !(first > 0 && dump > first && last > dump)}' "$TMP/initial-local-target-compose-render-events"; then
  pass 'pinned guarded target render is kept distinct from the unguarded running baseline through initialize-local'
else
  show_fixture_failure initial-local-target-compose-render "$status"
  fail 'initial-local accepts the pinned guarded render while recording the actual unguarded baseline'
fi

make_fixture initial-local-unsupported-live-logger initial-local-target-compose-render
state=$(cat "$TMP/initial-local-unsupported-live-logger-state-path")
printf '%s\n' "$APPLY_TARGET_SHA" > "$state/origin"
printf '%s\n' "$INITIAL_LOCAL_TARGET_SHA" > "$TMP/initial-local-unsupported-live-logger-target-sha-path"
: > "$state/unsupported-live-logger"
status=$(run_fixture initial-local-unsupported-live-logger built 0 '' 2 '' '' initialize-local "$APPLY_TARGET_SHA")
root=$(cat "$TMP/initial-local-unsupported-live-logger-root-path")
checkout=$(cat "$TMP/initial-local-unsupported-live-logger-checkout-path")
state=$(cat "$TMP/initial-local-unsupported-live-logger-state-path")
if [ "$status" != 0 ] && \
   grep -q 'initial-local preflight rejected an unsupported live logging profile' "$TMP/initial-local-unsupported-live-logger.stderr" && \
   grep -q $'preflight\tinitial_local_logging_transition\tsha256:.*:false\tsyslog-v1:false\tfail' "$root.target/preflight-comparisons.tsv" && \
   ! grep -Eq '^docker compose (build|up)( |$)|^python3 blob-mode-state.pinned initialize-local$' "$TMP/initial-local-unsupported-live-logger-events" && \
   [ "$(cat "$state/head")" = "$BASELINE_SHA" ] && [ "$(cat "$state/teagramd")" = "$BASE_ID" ] && \
   [ ! -e "$checkout/.state/blob-mode" ]; then
  pass 'initial-local rejects an unsupported live logger before building or replacing the service'
else
  show_fixture_failure initial-local-unsupported-live-logger "$status"
  fail 'initial-local must reject unsupported live logging before service replacement'
fi

for scenario in initial-local-port-drift initial-local-network-drift initial-local-replica-network-drift initial-local-key-mount-drift initial-local-pgdata-drift initial-local-environment-drift; do
  make_fixture "$scenario" "$scenario"
  state=$(cat "$TMP/$scenario-state-path")
  baseline_sha=$(cat "$TMP/$scenario-baseline-sha-path")
  printf '%s\n' "$APPLY_TARGET_SHA" > "$state/origin"
  printf '%s\n' "$INITIAL_LOCAL_TARGET_SHA" > "$TMP/$scenario-target-sha-path"
  status=$(run_fixture "$scenario" built 0 '' 2 '' '' initialize-local "$APPLY_TARGET_SHA")
  checkout=$(cat "$TMP/$scenario-checkout-path")
  if [ "$status" != 0 ] && \
     grep -q 'initial-local baseline or pinned target render was rejected before backup' "$TMP/$scenario.stderr" && \
     { [ "$scenario" != initial-local-replica-network-drift ] || grep -q 'baseline-inspection-invalid' "$TMP/$scenario.stderr"; } && \
     ! grep -q '^docker compose exec -T postgres pg_dump' "$TMP/$scenario-events" && \
     ! grep -Eq '^docker compose (build|up)( |$)|^python3 blob-mode-state.pinned initialize-local$|^git (merge --ff-only -q |reset --hard -q )' "$TMP/$scenario-events" && \
     [ ! -e "$checkout/.state/blob-mode" ] && [ "$(cat "$state/head")" = "$baseline_sha" ]; then
    pass "$scenario rejects before database backup, publication, or service replacement"
  else
    show_fixture_failure "$scenario" "$status"
    fail "$scenario must reject before rollout side effects"
  fi
done

make_fixture initial-local-unsafe-logging initial-local-unsafe-logging
state=$(cat "$TMP/initial-local-unsafe-logging-state-path")
printf '%s\n' "$APPLY_TARGET_SHA" > "$state/origin"
printf '%s\n' "$INITIAL_LOCAL_TARGET_SHA" > "$TMP/initial-local-unsafe-logging-target-sha-path"
jq -c '.services.teagramd.environment.TG_LOG_LOGIN_CODES="true" | del(.services.teagramd.logging)' \
  "$state/target-compose.json" > "$state/target-compose.next.json"
mv -- "$state/target-compose.next.json" "$state/target-compose.json"
status=$(run_fixture initial-local-unsafe-logging built 0 '' 2 '' '' initialize-local "$APPLY_TARGET_SHA")
root=$(cat "$TMP/initial-local-unsafe-logging-root-path")
if [ "$status" != 0 ] && \
   grep -q 'initial-local target must use syslog and disable login-code logging before backup' "$TMP/initial-local-unsafe-logging.stderr" && \
   grep -q $'compose_logging_policy\tsyslog-v1\tdefault\tfail' "$root.baseline/initial-local-logging-profile-preflight.tsv" && \
   grep -q $'compose_log_login_codes\tfalse\ttrue\tfail' "$root.baseline/initial-local-logging-profile-preflight.tsv" && \
   ! grep -q '^docker compose exec -T postgres pg_dump' "$TMP/initial-local-unsafe-logging-events" && \
   ! grep -Eq '^docker compose (build|up)( |$)|^git (merge --ff-only -q |reset --hard -q )' "$TMP/initial-local-unsafe-logging-events"; then
  pass 'initial-local default logging with login codes is rejected before backup or replacement'
else
  show_fixture_failure initial-local-unsafe-logging "$status"
  fail 'initial-local target must reject unsafe logging before backup'
fi

checkout=$(cat "$TMP/success-checkout-path")
root=$(cat "$TMP/success-root-path")
transition=$(jq -er '.transition_id' "$checkout/.state/blob-mode/mode.json")
mode_report="/root/teagramd-blob-mode-report-$transition.json"
if cmp -s "$checkout/.state/blob-mode/mode.json" "$checkout/.state/blob-mode/journal/0000000001.json" && \
   [ "$(stat -c %a "$mode_report")" = 600 ] && [ "$(stat -c %u "$mode_report")" = 0 ] && \
   [ "$(jq -r '.evidence.report_sha256' "$checkout/.state/blob-mode/mode.json")" = "$(sha256sum "$mode_report" | awk '{print $1}')" ] && \
   jq -e --arg source "$checkout/.state/blob-mode" '
     all(.services[]; .blob_mode_mounts == [{type:"bind",source:$source,target:"/run/teagramd/blob-mode",read_only:true}]
       and .tgblobs_mounts == [{type:"volume",source:"fixture_tgblobs",target:"/var/lib/teagramd-blobs",read_only:false}])
   ' "$root.target/target-blob-compose.json" >/dev/null && \
   jq -e --arg source "$checkout/.state/blob-mode" '
     any(.containers[]; .service | startswith("teagramd"))
     and all(.containers[];
       if .service | startswith("teagramd") then
         .mode_mounts == [{type:"bind",source:$source,target:"/run/teagramd/blob-mode",read_only:true}]
         and .tgblobs_mounts == [{type:"volume",name:"fixture_tgblobs",target:"/var/lib/teagramd-blobs",rw:true}]
       else .mode_mounts == [] end)
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
   [ "$(cat "$state/head")" = "$BASELINE_SHA" ] && [ "$(cat "$state/teagramd")" = "$BASE_ID" ] && \
   grep -q 'blob authority rejected' "$TMP/apply-without-authority.stderr"; then
  pass 'ordinary apply requires an existing authority and leaves the live baseline untouched'
else
  fail 'ordinary apply without authority must fail before replacement'
fi

if prepare_apply_fixture apply-unpinned-local success; then
  state=$(cat "$TMP/apply-unpinned-local-state-path")
  live_id=$(cat "$state/teagramd")
  checkout=$(cat "$TMP/apply-unpinned-local-checkout-path")
  transition=$(jq -er '.transition_id' "$checkout/.state/blob-mode/mode.json")
  report="/root/teagramd-blob-mode-report-$transition.json"
  authority_before=$(authority_fingerprint "$checkout/.state/blob-mode" "$report")
  printf 'services:\n  teagramd:\n    image: fixture:unreviewed\n' > "$checkout/docker-compose.unreviewed.yml"
  chmod 600 "$checkout/docker-compose.unreviewed.yml"
  printf '%s\n' unpinned-local-arbitrary > "$TMP/apply-unpinned-local-scenario"
  status=$(run_fixture apply-unpinned-local built 0 '' 2 '' '' apply)
  authority_after=$(authority_fingerprint "$checkout/.state/blob-mode" "$report")
  if [ "$status" != 0 ] && grep -q 'local-backend apply requires a reviewed local Compose artifact or unchanged carried-forward content' \
       "$TMP/apply-unpinned-local.stderr" && \
     ! grep -Eq '^docker compose exec -T postgres pg_dump|^docker compose build|^docker compose up -d' \
       "$TMP/apply-unpinned-local-events" && \
     [ "$(cat "$state/head")" = "$TARGET_SHA" ] && [ "$(cat "$state/teagramd")" = "$live_id" ] && \
     [ "$authority_before" = "$authority_after" ]; then
    pass 'arbitrary local Compose selection for an unpinned target rejects before backup, build, or replacement'
  else
    show_fixture_failure apply-unpinned-local "$status"
    fail 'unreviewed local Compose selection must fail closed before rollout side effects'
  fi
else
  fail 'unpinned local apply fixture requires a valid initialized authority'
fi

if prepare_moved_stack_apply_fixture moved-stack-9ef1110-to-da0107d moved-stack-9ef1110-to-da0107d; then
  state=$(cat "$TMP/moved-stack-9ef1110-to-da0107d-state-path")
  checkout=$(cat "$TMP/moved-stack-9ef1110-to-da0107d-checkout-path")
  root=$(cat "$TMP/moved-stack-9ef1110-to-da0107d-root-path")
  target_sha=da0107dfeadbeafbc1abfc4dd1594796ae661ed1
  expected_selection="$checkout/docker-compose.yml:$checkout/docker-compose.local-blobs.yml:$checkout/docker-compose.override.yml"
  status=$(run_fixture moved-stack-9ef1110-to-da0107d built 0 '' 2 '' '' apply "$target_sha")
  compose_selection=$(awk -F '\t' '{print $1}' "$TMP/moved-stack-9ef1110-to-da0107d-compose-selections" | sort -u)
  if [ "$status" = 0 ] && grep -q "rollout=verified sha=$target_sha" "$TMP/moved-stack-9ef1110-to-da0107d.stdout" && \
     [ "$(cat "$state/head")" = "$target_sha" ] && \
     [ "$compose_selection" = "$expected_selection" ] && \
     jq -e --arg dir /var/lib/telegramd-blobs '
       .services | all(.[]; .backend == {kind:"local",dir:$dir} and
         any(.tgblobs_mounts[]; .target == $dir and .read_only == false) and
         any(.tgblobs_mounts[]; .target == "/var/lib/teagramd-blobs" and .read_only == true))
     ' "$root.target/pre-backup-blob-compose.json" >/dev/null && \
     grep -q "compose_contract=tracked-local-authority blob_dir=/var/lib/telegramd-blobs" \
       "$root.target/pre-backup-blob-authority.txt" && \
     awk '/docker compose .*config --format json/ && config == 0 {config=NR} /^docker compose exec -T postgres pg_dump -U postgres / {backup=NR} END {exit !(config > 0 && backup > config)}' \
       "$TMP/moved-stack-9ef1110-to-da0107d-events" && assert_evidence_mode "$root"; then
    pass '9ef1110 to da0107d deploy renders the tracked local Compose selection from moved-stack authority instead of .env'
  else
    show_fixture_failure moved-stack-9ef1110-to-da0107d "$status"
    fail 'moved-stack tracked local Compose selection must match authority and pass the existing rollout gates'
  fi
else
  fail 'moved-stack rollout fixture requires a valid stack-moved-local authority'
fi

if prepare_moved_stack_apply_fixture moved-stack-9ef1110-to-da0107d-render-mismatch moved-stack-9ef1110-to-da0107d-render-mismatch; then
  state=$(cat "$TMP/moved-stack-9ef1110-to-da0107d-render-mismatch-state-path")
  root=$(cat "$TMP/moved-stack-9ef1110-to-da0107d-render-mismatch-root-path")
  head_before=$(cat "$state/head")
  container_before=$(cat "$state/teagramd")
  status=$(run_fixture moved-stack-9ef1110-to-da0107d-render-mismatch built 0 '' 2 '' '' apply \
    da0107dfeadbeafbc1abfc4dd1594796ae661ed1)
  if [ "$status" != 0 ] && grep -q 'render-backend-mismatch' \
       "$TMP/moved-stack-9ef1110-to-da0107d-render-mismatch.stderr" && \
     ! grep -Eq '^docker compose exec -T postgres pg_dump|^docker compose build|^docker compose up -d' \
       "$TMP/moved-stack-9ef1110-to-da0107d-render-mismatch-events" && \
     [ "$(cat "$state/head")" = "$head_before" ] && \
     [ "$(cat "$state/teagramd")" = "$container_before" ] && \
     [ ! -e "$root.backup/backup-manifest.txt" ]; then
    pass 'moved-stack tracked local Compose render mismatch rejects before backup or target start'
  else
    show_fixture_failure moved-stack-9ef1110-to-da0107d-render-mismatch "$status"
    fail 'moved-stack tracked local Compose render mismatch must fail before backup or target start'
  fi
else
  fail 'moved-stack mismatch fixture requires a valid stack-moved-local authority'
fi

if prepare_apply_fixture s3-accepted-apply s3-accepted-apply; then
  state=$(cat "$TMP/s3-accepted-apply-state-path")
  checkout=$(cat "$TMP/s3-accepted-apply-checkout-path")
  root=$(cat "$TMP/s3-accepted-apply-root-path")
  if create_transition_authority_fixture "$SCRIPT_DIR/test-blob-mode-state.py" \
    "$TMP/s3-accepted-apply-authority" "$checkout/.state/blob-mode" /root s3-accepted && \
    configure_s3_compose_fixture "$state" fixture_rustfsdata fixture_rustfs_private; then
    printf '%s\n' 'TG_BLOB_S3_ACCESS_KEY_ID=fixture-access-key' \
      'TG_BLOB_S3_SECRET_ACCESS_KEY=fixture-secret' > "$checkout/.env.rustfs-parked"
    chmod 600 "$checkout/.env.rustfs-parked"
    expected_env_files="$checkout/.env,$checkout/.env.rustfs-parked"
    printf '%s\n' "$expected_env_files" > "$TMP/s3-accepted-apply-compose-env-files"
    status=$(run_fixture s3-accepted-apply built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
    if [ "$status" = 0 ] && \
       grep -q "rollout=verified sha=$APPLY_TARGET_SHA" "$TMP/s3-accepted-apply.stdout" && \
       [ "$(cat "$state/head")" = "$APPLY_TARGET_SHA" ] && \
       awk -F '\t' \
         -v expected_selection="$checkout/docker-compose.yml:$checkout/docker-compose.override.yml" \
         -v expected_env_files="$expected_env_files" '
           $1 == expected_selection {
             selected++
             if ($2 == "compose up -d ") target_up++
             if (NF < 3 || $3 != expected_env_files) bad=1
           }
           END {exit !(selected > 0 && target_up == 1 && !bad)}
         ' \
         "$TMP/s3-accepted-apply-compose-selections" && \
       jq -e '.services | all(.[]; .backend.kind == "s3")' \
         "$root.target/pre-backup-blob-compose.json" >/dev/null && \
       grep -q 'backend=s3 ' "$root.target/pre-backup-blob-authority.txt" && assert_evidence_mode "$root"; then
      pass 'ordinary S3 apply carries the selected Compose pair and parked credential files through the real runner'
    else
      show_fixture_failure s3-accepted-apply "$status"
      fail 'ordinary S3 apply must preserve authority, environment-file selection, and existing rollout gates'
    fi
  else
    fail 'S3 apply fixture requires publishable authority and a matching S3 Compose render'
  fi
else
  fail 'S3 apply fixture requires a valid initialized local authority'
fi

missing_reports_apply_name=s3-accepted-apply-missing-reports
if prepare_apply_fixture "$missing_reports_apply_name" s3-accepted-apply; then
  state=$(cat "$TMP/$missing_reports_apply_name-state-path")
  checkout=$(cat "$TMP/$missing_reports_apply_name-checkout-path")
  root=$(cat "$TMP/$missing_reports_apply_name-root-path")
  if create_transition_authority_fixture "$SCRIPT_DIR/test-blob-mode-state.py" \
    "$TMP/$missing_reports_apply_name-authority" "$checkout/.state/blob-mode" /root s3-accepted && \
    configure_s3_compose_fixture "$state" fixture_rustfsdata fixture_rustfs_private; then
    printf '%s\n' 'TG_BLOB_S3_ACCESS_KEY_ID=fixture-access-key' \
      'TG_BLOB_S3_SECRET_ACCESS_KEY=fixture-secret' > "$checkout/.env.rustfs-parked"
    chmod 600 "$checkout/.env.rustfs-parked"
    expected_env_files="$checkout/.env,$checkout/.env.rustfs-parked"
    printf '%s\n' "$expected_env_files" > "$TMP/$missing_reports_apply_name-compose-env-files"
    if remove_authority_reports "$checkout/.state/blob-mode" /root; then
      status=$(run_fixture "$missing_reports_apply_name" built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
      deployment_validation_count=$(grep -c \
        '^python3 blob-mode-state.pinned deployment-validate$' \
        "$TMP/$missing_reports_apply_name-events" || true)
      if python3 "$SCRIPT_DIR/blob-mode-state.py" validate \
        --state-dir "$checkout/.state/blob-mode" --report-root /root \
        --containers "$root.target/pre-backup-blob-containers.json" \
        --compose "$root.target/pre-backup-blob-compose.json" \
        --override "$checkout/docker-compose.override.yml" --checkout "$checkout" \
        >/dev/null 2>&1; then
        strict_validation_rejected=0
      else
        strict_validation_rejected=1
      fi
      if [ "$status" = 0 ] && \
         grep -q "rollout=verified sha=$APPLY_TARGET_SHA" "$TMP/$missing_reports_apply_name.stdout" && \
         [ "$(cat "$state/head")" = "$APPLY_TARGET_SHA" ] && \
         [ "$deployment_validation_count" -ge 3 ] && [ "$strict_validation_rejected" = 1 ] && \
         grep -q 'backend=s3 ' "$root.target/pre-backup-blob-authority.txt" && assert_evidence_mode "$root"; then
        pass 'S3 apply tolerates missing reports through read-only rollout validations while strict validation rejects them'
      else
        show_fixture_failure "$missing_reports_apply_name" "$status"
        fail 'S3 apply must tolerate missing reports through replacement without weakening strict validation'
      fi
    else
      fail 'missing-report apply fixture could not remove its authority reports'
    fi
  else
    fail 'missing-report S3 apply fixture requires publishable authority and a matching Compose render'
  fi
else
  fail 'missing-report S3 apply fixture requires a valid initialized local authority'
fi

recovered_apply_name=moved-stack-9ef1110-to-da0107d-recovered-local-apply
if prepare_moved_stack_apply_fixture "$recovered_apply_name" recovered-local-apply; then
  state=$(cat "$TMP/$recovered_apply_name-state-path")
  checkout=$(cat "$TMP/$recovered_apply_name-checkout-path")
  root=$(cat "$TMP/$recovered_apply_name-root-path")
  if create_transition_authority_fixture "$SCRIPT_DIR/test-blob-mode-state.py" \
    "$TMP/$recovered_apply_name-authority" "$checkout/.state/blob-mode" /root recovered-local 1; then
    status=$(run_fixture "$recovered_apply_name" built 0 '' 2 '' '' apply da0107dfeadbeafbc1abfc4dd1594796ae661ed1)
    compose_selection=$(awk -F '\t' '{print $1}' "$TMP/$recovered_apply_name-compose-selections" | sort -u)
    if [ "$status" = 0 ] && \
       grep -q 'rollout=verified sha=da0107dfeadbeafbc1abfc4dd1594796ae661ed1' \
         "$TMP/$recovered_apply_name.stdout" && \
       [ "$(cat "$state/head")" = da0107dfeadbeafbc1abfc4dd1594796ae661ed1 ] && \
       [ "$compose_selection" = "$checkout/docker-compose.yml:$checkout/docker-compose.local-blobs.yml:$checkout/docker-compose.override.yml" ] && \
       jq -e --arg dir /var/lib/telegramd-blobs '
         .outcome == "recovered-local" and .backend == {kind:"local",dir:$dir}
       ' "$checkout/.state/blob-mode/mode.json" >/dev/null && \
       jq -e --arg dir /var/lib/telegramd-blobs '
         .services | all(.[]; .backend == {kind:"local",dir:$dir} and
           any(.tgblobs_mounts[]; .target == $dir and .read_only == false) and
           any(.tgblobs_mounts[]; .target == "/var/lib/teagramd-blobs" and .read_only == true))
       ' "$root.target/pre-backup-blob-compose.json" >/dev/null && \
       grep -q 'compose_contract=tracked-local-authority blob_dir=/var/lib/telegramd-blobs' \
         "$root.target/pre-backup-blob-authority.txt" && assert_evidence_mode "$root"; then
      pass 'ordinary deploy after recovery renders the tracked local Compose pair from recovered-local authority'
    else
      show_fixture_failure "$recovered_apply_name" "$status"
      fail 'ordinary deploy after recovery must render the authority directory before backup and pass rollout gates'
    fi
  else
    fail 'recovered-local apply fixture requires a valid moved-stack S3 and recovery authority chain'
  fi
else
  fail 'recovered-local apply fixture requires a valid moved-stack local authority'
fi

if prepare_apply_fixture apply-backend-flip runtime-backend-mismatch; then
  state=$(cat "$TMP/apply-backend-flip-state-path")
  checkout=$(cat "$TMP/apply-backend-flip-checkout-path")
  live_id=$(cat "$state/teagramd")
  transition=$(jq -er '.transition_id' "$checkout/.state/blob-mode/mode.json")
  report="/root/teagramd-blob-mode-report-$transition.json"
  authority_before=$(authority_fingerprint "$checkout/.state/blob-mode" "$report")
  status=$(run_fixture apply-backend-flip built 0 '' 2 '' '' apply)
  authority_after=$(authority_fingerprint "$checkout/.state/blob-mode" "$report")
  if [ "$status" != 0 ] && grep -q 'running-backend-mismatch' "$TMP/apply-backend-flip.stderr" && \
     ! grep -Eq '^docker (stop|kill)( |$)|^docker compose (build|up|stop|down)( |$)' "$TMP/apply-backend-flip-events" && \
     [ "$(cat "$state/head")" = "$TARGET_SHA" ] && \
     [ "$(cat "$state/teagramd")" = "$live_id" ] && \
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
  live_id=$(cat "$state/teagramd")
  transition=$(jq -er '.transition_id' "$checkout/.state/blob-mode/mode.json")
  report="/root/teagramd-blob-mode-report-$transition.json"
  authority_before=$(authority_fingerprint "$checkout/.state/blob-mode" "$report")
  jq -c '
    .services.teagramd.environment.TG_BLOB_S3_ENDPOINT="https://objects.fixture.invalid" |
    .services.teagramd.environment.TG_BLOB_S3_BUCKET="fixture-bucket" |
    .services.teagramd.environment.TG_BLOB_S3_PREFIX="fixture/"
  ' "$state/target-compose.json" > "$state/s3-compose.json"
  mv -- "$state/s3-compose.json" "$state/target-compose.json"
  cp -- "$state/target-compose.json" "$state/base-compose.json"
  status=$(run_fixture apply-s3-render built 0 '' 2 '' '' apply)
  authority_after=$(authority_fingerprint "$checkout/.state/blob-mode" "$report")
  if [ "$status" != 0 ] && grep -q 'render-backend-mismatch' "$TMP/apply-s3-render.stderr" && \
     ! grep -Eq '^docker (stop|kill)( |$)|^docker compose (build|up|stop|down)( |$)' "$TMP/apply-s3-render-events" && \
     [ "$(cat "$state/head")" = "$TARGET_SHA" ] && \
     [ "$(cat "$state/teagramd")" = "$live_id" ] && \
     [ "$authority_before" = "$authority_after" ]; then
    pass 'ordinary apply rejects an S3 render against initial-local authority before replacing the live baseline'
  else
    show_fixture_failure apply-s3-render "$status"
    fail 'S3-render apply must preserve live containers and authority before build or up'
  fi
else
  fail 'S3-render apply fixture requires a valid initialized authority'
fi

if prepare_target_local_apply_fixture target-local-apply-success target-local-apply-success; then
  state=$(cat "$TMP/target-local-apply-success-state-path")
  root=$(cat "$TMP/target-local-apply-success-root-path")
  status=$(run_fixture target-local-apply-success built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
  compose_selection=$(awk -F '\t' '{print $1}' "$TMP/target-local-apply-success-compose-selections" | sort -u)
  if [ "$status" = 0 ] && grep -q "rollout=verified sha=$E58_LOCAL_COMPOSE_TARGET_SHA" "$TMP/target-local-apply-success.stdout" && \
     [ "$(cat "$state/head")" = "$E58_LOCAL_COMPOSE_TARGET_SHA" ] && \
     awk -F '\t' '$2 == "container_replaced" && $3 != $4 && $5 == "pass" { found=1 } END { exit !found }' "$root.target/target-comparisons.tsv" && \
     [ "$compose_selection" = '.rollout-compose.local-e58ba20.yml:docker-compose.override.yml' ] && \
     grep -q "source_revision=$APPLY_TARGET_SHA target_sha=$E58_LOCAL_COMPOSE_TARGET_SHA expected_baseline_sha=$TARGET_SHA" "$root.baseline/runtime-pins.txt" && \
     grep -q "target_local_compose_target_sha=$E58_LOCAL_COMPOSE_TARGET_SHA target_local_compose_sha256=$E58_LOCAL_COMPOSE_ARTIFACT_SHA" "$root.baseline/runtime-pins.txt" && \
     grep -q "result=pass target_sha=$E58_LOCAL_COMPOSE_TARGET_SHA expected_baseline_sha=$TARGET_SHA compose_sha256=$E58_LOCAL_COMPOSE_ARTIFACT_SHA" "$root.target/pre-backup-blob-authority.txt" && \
     awk '$0 == "docker compose config --format json" && config == 0 {config=NR} $0 == "docker compose exec -T postgres pg_dump -U postgres teagram" {backup=NR} END {exit !(config > 0 && backup > config)}' "$TMP/target-local-apply-success-events" && \
     assert_evidence_mode "$root"; then
    pass 'e58ba20 apply pins the reviewed source, live baseline, artifact digest, and same-backend preflight before backup'
  else
    show_fixture_failure target-local-apply-success "$status"
    fail 'e58ba20 local apply must pass the pinned same-backend preflight before the existing rollout gates'
  fi
else
  fail 'e58ba20 local apply fixture requires a valid initialized local authority'
fi

if prepare_target_local_apply_fixture stack-move-retained-rustfs-volume stack-move-retained-rustfs-volume; then
  state=$(cat "$TMP/stack-move-retained-rustfs-volume-state-path")
  checkout=$(cat "$TMP/stack-move-retained-rustfs-volume-checkout-path")
  root=$(cat "$TMP/stack-move-retained-rustfs-volume-root-path")
  head_before=$(cat "$state/head")
  container_before=$(cat "$state/teagramd")
  jq -c '.volumes.rustfsdata={name:"fixture_rustfsdata"}' "$state/target-compose.json" > "$state/target-compose.next.json" && \
    mv -- "$state/target-compose.next.json" "$state/target-compose.json"
  jq -c '.volumes.rustfsdata="fixture_rustfsdata"' "$checkout/.state/blob-mode/mode.json" > \
    "$checkout/.state/blob-mode/mode.next.json" && \
    chmod 644 "$checkout/.state/blob-mode/mode.next.json" && \
    mv -- "$checkout/.state/blob-mode/mode.next.json" "$checkout/.state/blob-mode/mode.json"
  status=$(run_fixture stack-move-retained-rustfs-volume built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
  if [ "$status" != 0 ] && grep -q 'mode-head-mismatch' "$TMP/stack-move-retained-rustfs-volume.stderr" && \
     grep -Fq 'docker compose --profile migration config --format json' "$TMP/stack-move-retained-rustfs-volume-events" && \
     jq -e '.volumes.rustfsdata == "fixture_rustfsdata"' "$root.target/pre-backup-blob-compose.json" >/dev/null && \
     ! grep -Eq '^docker compose exec -T postgres pg_dump|^docker compose build|^docker compose up -d' \
       "$TMP/stack-move-retained-rustfs-volume-events" && \
     [ "$(cat "$state/head")" = "$head_before" ] && [ "$(cat "$state/teagramd")" = "$container_before" ] && \
     [ ! -e "$root.backup/backup-manifest.txt" ] && [ ! -e "$root.build/build-result.txt" ]; then
    pass 'stack-moved authority renders the retained migration-profile RustFS volume before backup'
  else
    show_fixture_failure stack-move-retained-rustfs-volume "$status"
    fail 'stack-moved authority must retain the dormant RustFS volume in its preflight render'
  fi
else
  fail 'stack-moved retained RustFS fixture requires a valid initialized local authority'
fi

if prepare_target_local_apply_fixture image-reuse-apply-success target-local-carry-forward-image-reuse-success "$APPLY_TARGET_SHA"; then
  state=$(cat "$TMP/image-reuse-apply-success-state-path")
  root=$(cat "$TMP/image-reuse-apply-success-root-path")
  baseline_id=$(cat "$state/teagramd")
  printf '%s\n' "$baseline_id" > "$state/replacement-id"
  status=$(run_fixture image-reuse-apply-success built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
  comparisons="$root.target/target-comparisons.tsv"
  if [ "$status" = 0 ] && [ "$(cat "$state/teagramd")" = "$baseline_id" ] && \
     grep -Fq "container_id=$baseline_id container_action=reused" "$root.target/target-result.txt" && \
     grep -Fq "readiness=pass schema_gate=pass" "$root.target/target-result.txt" && \
     awk -F '\t' -v id="$baseline_id" '$2 == "container_reused" && $3 == id && $4 == id && $5 == "pass" { found=1 } END { exit !found }' "$comparisons" && \
     awk -F '\t' -v image="$BUILT_IMAGE" '$2 == "baseline_image_matches_built" && $3 == image && $4 == image && $5 == "pass" { found=1 } END { exit !found }' "$comparisons" && \
     awk -F '\t' -v image="$BUILT_IMAGE" '$2 == "target_image_matches_built" && $3 == image && $4 == image && $5 == "pass" { found=1 } END { exit !found }' "$comparisons" && \
     ! awk -F '\t' '$2 == "container_replaced" { found=1 } END { exit !found }' "$comparisons" && \
     assert_evidence_mode "$root"; then
    pass 'ordinary apply reuses the unchanged container only with matching baseline, built, and target image IDs'
  else
    show_fixture_failure image-reuse-apply-success "$status"
    fail 'proven image reuse must pass readiness and schema checks without claiming replacement'
  fi
else
  fail 'image reuse apply fixture requires a valid initialized local authority'
fi

if prepare_target_local_apply_fixture image-reuse-stale-build target-local-carry-forward-image-reuse-stale-build "$APPLY_TARGET_SHA"; then
  state=$(cat "$TMP/image-reuse-stale-build-state-path")
  root=$(cat "$TMP/image-reuse-stale-build-root-path")
  baseline_id=$(cat "$state/teagramd")
  printf '%s\n' "$baseline_id" > "$state/replacement-id"
  status=$(run_fixture image-reuse-stale-build old 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
  comparisons="$root.target/target-comparisons.tsv"
  if [ "$status" != 0 ] && grep -q "rollback=verified baseline_sha=$TARGET_SHA" "$TMP/image-reuse-stale-build.stdout" && \
     [ "$(cat "$state/head")" = "$TARGET_SHA" ] && [ "$(cat "$state/teagramd")" = "$ROLLBACK_ID" ] && \
     grep -q "source_sha=$APPLY_TARGET_SHA image_id=$BASE_IMAGE" "$root.build/built-image.identity" && \
     awk -F '\t' -v id="$baseline_id" '$2 == "container_reused" && $3 == id && $4 == id && $5 == "fail" { found=1 } END { exit !found }' "$comparisons" && \
     awk -F '\t' -v image="$BUILT_IMAGE" -v built="$BASE_IMAGE" '$2 == "baseline_image_matches_built" && $3 == image && $4 == built && $5 == "fail" { found=1 } END { exit !found }' "$comparisons" && \
     grep -q $'target_image_matches_built\t.*\tfail' "$comparisons" && \
     grep -q 'baseline_equivalence=pass' "$root.rollback/rollback-result.txt"; then
    pass 'same-container reuse rejects a stale target build and verifies baseline rollback'
  else
    show_fixture_failure image-reuse-stale-build "$status"
    fail 'stale built image must reject unchanged-container reuse and verify rollback'
  fi
else
  fail 'stale image reuse fixture requires a valid initialized local authority'
fi

if prepare_target_local_apply_fixture image-reuse-stop-timeout-mismatch target-local-carry-forward-image-reuse-stop-timeout-mismatch "$APPLY_TARGET_SHA"; then
  state=$(cat "$TMP/image-reuse-stop-timeout-mismatch-state-path")
  root=$(cat "$TMP/image-reuse-stop-timeout-mismatch-root-path")
  baseline_id=$(cat "$state/teagramd")
  printf '%s\n' "$baseline_id" > "$state/replacement-id"
  status=$(run_fixture image-reuse-stop-timeout-mismatch built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
  comparisons="$root.target/target-comparisons.tsv"
  if [ "$status" != 0 ] && grep -q "rollback=verified baseline_sha=$TARGET_SHA" "$TMP/image-reuse-stop-timeout-mismatch.stdout" && \
     [ "$(cat "$state/head")" = "$TARGET_SHA" ] && [ "$(cat "$state/teagramd")" = "$ROLLBACK_ID" ] && \
     awk -F '\t' -v id="$baseline_id" '$2 == "container_reused" && $3 == id && $4 == id && $5 == "pass" { found=1 } END { exit !found }' "$comparisons" && \
     grep -q $'baseline_image_matches_built\t.*\tpass' "$comparisons" && \
     grep -q $'target_image_matches_built\t.*\tpass' "$comparisons" && \
     grep -q $'target_state\trunning\trunning\tpass' "$comparisons" && \
     grep -q $'target_inspected_stop_timeout\t120\t60\tfail' "$comparisons" && \
     grep -q 'baseline_equivalence=pass' "$root.rollback/rollback-result.txt"; then
    pass 'matching image identity does not excuse a reused-container comparison mismatch; baseline rollback verifies'
  else
    show_fixture_failure image-reuse-stop-timeout-mismatch "$status"
    fail 'reused-container comparison mismatch must reject and verify baseline rollback'
  fi
else
  fail 'comparison mismatch reuse fixture requires a valid initialized local authority'
fi

if prepare_target_local_apply_fixture target-local-598-apply-success target-local-598-apply-success "$TARGET_LOCAL_COMPOSE_TARGET_SHA"; then
  state=$(cat "$TMP/target-local-598-apply-success-state-path")
  root=$(cat "$TMP/target-local-598-apply-success-root-path")
  status=$(run_fixture target-local-598-apply-success built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
  compose_selection=$(awk -F '\t' '{print $1}' "$TMP/target-local-598-apply-success-compose-selections" | sort -u)
  if [ "$status" = 0 ] && grep -q "rollout=verified sha=$TARGET_LOCAL_COMPOSE_TARGET_SHA" "$TMP/target-local-598-apply-success.stdout" && \
     [ "$(cat "$state/head")" = "$TARGET_LOCAL_COMPOSE_TARGET_SHA" ] && \
     [ "$compose_selection" = '.rollout-compose.local-598359e.yml:docker-compose.override.yml' ] && \
     grep -q "source_revision=$APPLY_TARGET_SHA target_sha=$TARGET_LOCAL_COMPOSE_TARGET_SHA expected_baseline_sha=$TARGET_SHA" "$root.baseline/runtime-pins.txt" && \
     grep -q "target_local_compose_target_sha=$TARGET_LOCAL_COMPOSE_TARGET_SHA target_local_compose_sha256=$TARGET_LOCAL_COMPOSE_ARTIFACT_SHA" "$root.baseline/runtime-pins.txt" && \
     grep -q 'target_local_compose_source_file=deploy/teagramd/rollout-runner/local-compose-598359e.yml target_local_compose_runtime_file=.rollout-compose.local-598359e.yml' "$root.baseline/runtime-pins.txt" && \
     grep -q "result=pass target_sha=$TARGET_LOCAL_COMPOSE_TARGET_SHA expected_baseline_sha=$TARGET_SHA compose_sha256=$TARGET_LOCAL_COMPOSE_ARTIFACT_SHA" "$root.target/pre-backup-blob-authority.txt" && \
     awk '$0 == "docker compose config --format json" && config == 0 {config=NR} $0 == "docker compose exec -T postgres pg_dump -U postgres teagram" {backup=NR} END {exit !(config > 0 && backup > config)}' "$TMP/target-local-598-apply-success-events" && \
     assert_evidence_mode "$root"; then
    pass '598359e apply pins the reviewed source, baseline, artifact digest, and local authority before backup'
  else
    show_fixture_failure target-local-598-apply-success "$status"
    fail '598359e local apply must pass its pinned artifact and authority checks before existing rollout gates'
  fi
else
  fail '598359e local apply fixture requires a valid initialized local authority'
fi

if prepare_target_local_apply_fixture target-local-df9-apply-success target-local-df9-apply-success "$DF9_LOCAL_COMPOSE_TARGET_SHA"; then
  state=$(cat "$TMP/target-local-df9-apply-success-state-path")
  root=$(cat "$TMP/target-local-df9-apply-success-root-path")
  status=$(run_fixture target-local-df9-apply-success built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
  compose_selection=$(awk -F '\t' '{print $1}' "$TMP/target-local-df9-apply-success-compose-selections" | sort -u)
  if [ "$status" = 0 ] && grep -q "rollout=verified sha=$DF9_LOCAL_COMPOSE_TARGET_SHA" "$TMP/target-local-df9-apply-success.stdout" && \
     [ "$(cat "$state/head")" = "$DF9_LOCAL_COMPOSE_TARGET_SHA" ] && \
     [ "$compose_selection" = '.rollout-compose.local-df9ffe5.yml:docker-compose.override.yml' ] && \
     grep -q "source_revision=$APPLY_TARGET_SHA target_sha=$DF9_LOCAL_COMPOSE_TARGET_SHA expected_baseline_sha=$TARGET_SHA" "$root.baseline/runtime-pins.txt" && \
     grep -q "target_local_compose_target_sha=$DF9_LOCAL_COMPOSE_TARGET_SHA target_local_compose_sha256=$DF9_LOCAL_COMPOSE_ARTIFACT_SHA" "$root.baseline/runtime-pins.txt" && \
     grep -q 'target_local_compose_source_file=deploy/teagramd/rollout-runner/local-compose-df9ffe5.yml target_local_compose_runtime_file=.rollout-compose.local-df9ffe5.yml' "$root.baseline/runtime-pins.txt" && \
     grep -q "result=pass target_sha=$DF9_LOCAL_COMPOSE_TARGET_SHA expected_baseline_sha=$TARGET_SHA compose_sha256=$DF9_LOCAL_COMPOSE_ARTIFACT_SHA" "$root.target/pre-backup-blob-authority.txt" && \
     awk '$0 == "docker compose config --format json" && config == 0 {config=NR} $0 == "docker compose exec -T postgres pg_dump -U postgres teagram" {backup=NR} END {exit !(config > 0 && backup > config)}' "$TMP/target-local-df9-apply-success-events" && \
     assert_evidence_mode "$root"; then
    pass 'df9ffe5 apply pins the reviewed source, baseline, artifact digest, and local authority before backup'
  else
    show_fixture_failure target-local-df9-apply-success "$status"
    fail 'df9ffe5 local apply must pass its pinned artifact and authority checks before existing rollout gates'
  fi
else
  fail 'df9ffe5 local apply fixture requires a valid initialized local authority'
fi

assert_source_revision_forwarding() {
  local name=$1 scenario=$2 state root status compose_selection
  if prepare_target_local_apply_fixture "$name" "$scenario" "$SOURCE_LOCAL_COMPOSE_TARGET_SHA"; then
    state=$(cat "$TMP/$name-state-path")
    root=$(cat "$TMP/$name-root-path")
    status=$(run_fixture "$name" built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
    compose_selection=$(awk -F '\t' '{print $1}' "$TMP/$name-compose-selections" | sort -u)
    if [ "$status" = 0 ] && grep -q "rollout=verified sha=$SOURCE_LOCAL_COMPOSE_TARGET_SHA" "$TMP/$name.stdout" && \
       [ "$(cat "$state/head")" = "$SOURCE_LOCAL_COMPOSE_TARGET_SHA" ] && \
       [ "$compose_selection" = '.rollout-compose.local-5602c7c.yml:docker-compose.override.yml' ] && \
       grep -q "source_revision=$APPLY_TARGET_SHA target_sha=$SOURCE_LOCAL_COMPOSE_TARGET_SHA expected_baseline_sha=$TARGET_SHA" "$root.baseline/runtime-pins.txt" && \
       grep -q "target_local_compose_target_sha=$SOURCE_LOCAL_COMPOSE_TARGET_SHA target_local_compose_sha256=$SOURCE_LOCAL_COMPOSE_ARTIFACT_SHA" "$root.baseline/runtime-pins.txt" && \
       grep -Fxq "build revision=$SOURCE_LOCAL_COMPOSE_TARGET_SHA" "$TMP/$name-events" && \
       ! grep -Fxq 'build revision=ambient-revision-must-not-win' "$TMP/$name-events" && \
       grep -q "result=pass target_sha=$SOURCE_LOCAL_COMPOSE_TARGET_SHA expected_baseline_sha=$TARGET_SHA compose_sha256=$SOURCE_LOCAL_COMPOSE_ARTIFACT_SHA" "$root.target/pre-backup-blob-authority.txt"; then
      pass "$name forwards the validated application target over the ambient revision"
    else
      show_fixture_failure "$name" "$status"
      fail "$name must pin its local artifact and build revision before existing rollout gates"
    fi
  else
    fail "$name fixture requires a valid initialized local authority"
  fi
}

assert_source_revision_forwarding target-local-source-apply-success target-local-source-apply-success
assert_source_revision_forwarding target-local-source-ambient-different target-local-source-ambient-different

prepare_target_local_apply_fixture target-local-source-wrong-digest target-local-source-wrong-digest \
  "$SOURCE_LOCAL_COMPOSE_TARGET_SHA"
assert_target_local_rejected_before_backup target-local-source-wrong-digest \
  'target-local Compose artifact differs from the reviewed pin'

prepare_target_local_apply_fixture target-local-source-tampered-content target-local-source-tampered-content \
  "$SOURCE_LOCAL_COMPOSE_TARGET_SHA"
assert_target_local_rejected_before_backup target-local-source-tampered-content \
  'target-local Compose content differs from the reviewed content pin'

prepare_target_local_apply_fixture target-local-source-tampered-body-pin target-local-source-tampered-body-pin \
  "$SOURCE_LOCAL_COMPOSE_TARGET_SHA"
assert_target_local_rejected_before_backup target-local-source-tampered-body-pin \
  'target-local Compose content differs from the reviewed content pin'

assert_cce_revision_forwarding() {
  local name=$1 scenario=$2 state root status compose_selection
  if prepare_target_local_apply_fixture "$name" "$scenario" "$CCE_LOCAL_COMPOSE_TARGET_SHA"; then
    state=$(cat "$TMP/$name-state-path")
    root=$(cat "$TMP/$name-root-path")
    status=$(run_fixture "$name" built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
    compose_selection=$(awk -F '\t' '{print $1}' "$TMP/$name-compose-selections" | sort -u)
    if [ "$status" = 0 ] && grep -q "rollout=verified sha=$CCE_LOCAL_COMPOSE_TARGET_SHA" "$TMP/$name.stdout" && \
       [ "$(cat "$state/head")" = "$CCE_LOCAL_COMPOSE_TARGET_SHA" ] && \
       [ "$compose_selection" = '.rollout-compose.local-cce00d1.yml:docker-compose.override.yml' ] && \
       grep -q "source_revision=$APPLY_TARGET_SHA target_sha=$CCE_LOCAL_COMPOSE_TARGET_SHA expected_baseline_sha=$TARGET_SHA" "$root.baseline/runtime-pins.txt" && \
       grep -q "target_local_compose_target_sha=$CCE_LOCAL_COMPOSE_TARGET_SHA target_local_compose_sha256=$CCE_LOCAL_COMPOSE_ARTIFACT_SHA" "$root.baseline/runtime-pins.txt" && \
       grep -q "target_local_compose_content_sha256=$CCE_LOCAL_COMPOSE_CONTENT_SHA" "$root.baseline/runtime-pins.txt" && \
       grep -Fxq "build revision=$CCE_LOCAL_COMPOSE_TARGET_SHA" "$TMP/$name-events" && \
       ! grep -Fxq 'build revision=ambient-revision-must-not-win' "$TMP/$name-events" && \
       grep -q "result=pass target_sha=$CCE_LOCAL_COMPOSE_TARGET_SHA expected_baseline_sha=$TARGET_SHA compose_sha256=$CCE_LOCAL_COMPOSE_ARTIFACT_SHA" "$root.target/pre-backup-blob-authority.txt"; then
      pass "$name selects the current-target artifact and forwards the validated application target"
    else
      show_fixture_failure "$name" "$status"
      fail "$name must pin the current-target artifact and build revision before existing rollout gates"
    fi
  else
    fail "$name fixture requires a valid initialized local authority"
  fi
}

assert_cce_revision_forwarding target-local-cce-apply-success target-local-cce-apply-success
assert_cce_revision_forwarding target-local-cce-ambient-different target-local-cce-ambient-different

assert_carried_revision_forwarding() {
  local name=$1 scenario=$2 target_sha=$3 baseline_sha=$4 body_sha=$5 origin_sha=$6
  local source_file=$7 runtime_file=$8 source_sha=$9 state root status compose_selection checkout build_provenance_ok=0
  if prepare_target_local_apply_fixture "$name" "$scenario" "$target_sha" "$baseline_sha"; then
    state=$(cat "$TMP/$name-state-path")
    root=$(cat "$TMP/$name-root-path")
    checkout=$(cat "$TMP/$name-checkout-path")
    status=$(run_fixture "$name" built 0 '' 2 '' '' apply "$source_sha")
    compose_selection=$(awk -F '\t' '{print $1}' "$TMP/$name-compose-selections" | sort -u)
    if [ "$body_sha" = "$SOURCE_LOCAL_COMPOSE_CONTENT_SHA" ]; then
      if grep -Fq "revision_label_json=\"$target_sha\"" "$root.build/built-image.identity" && \
         jq -e --arg source "$target_sha" --arg context "$checkout" \
           '.source_sha == $source and .context == $context and .revision == $source' \
           "$root.build/build-compose-provenance.json" >/dev/null; then
        build_provenance_ok=1
      fi
    elif grep -Fq 'revision_label_json=' "$root.build/built-image.identity" && \
         [ ! -e "$root.build/build-compose-provenance.json" ]; then
      build_provenance_ok=1
    fi
    if [ "$status" = 0 ] && grep -q "rollout=verified sha=$target_sha" "$TMP/$name.stdout" && \
       [ "$(cat "$state/head")" = "$target_sha" ] && \
       grep -Fq "target_local_compose_reviewed_origin_sha=$origin_sha" "$root.baseline/runtime-pins.txt" && \
       grep -Fq "target_sha=$target_sha expected_baseline_sha=$baseline_sha" "$root.baseline/runtime-pins.txt" && \
       grep -Fq "source_revision=$source_sha target_sha=$target_sha" "$root.baseline/runtime-pins.txt" && \
       grep -Fq "target_local_compose_content_sha256=$body_sha" "$root.baseline/runtime-pins.txt" && \
       grep -Fq "target_local_compose_source_file=$source_file target_local_compose_runtime_file=$runtime_file" \
         "$root.baseline/runtime-pins.txt" && \
       grep -Fq "target_local_compose_sha256=$(sha256sum "$checkout/$runtime_file" | cut -d ' ' -f 1)" \
         "$root.baseline/runtime-pins.txt" && \
       grep -Fq "build revision=$target_sha" "$TMP/$name-events" && \
       [ "$build_provenance_ok" = 1 ] && \
       [ "$compose_selection" = "$runtime_file:docker-compose.override.yml" ] && assert_evidence_mode "$root"; then
      pass "$name selects the body-reviewed forwarding contract and binds target, source, context, and image label"
    else
      show_fixture_failure "$name" "$status"
      fail "$name must forward and attest the validated target revision"
    fi
  else
    fail "$name fixture requires a valid initialized local authority"
  fi
}

assert_carried_revision_forwarding target-local-carry-forward-cce-main-baseline \
  target-local-carry-forward-cce-filename-df9 "$LIVE_BASELINE_SHA" "$LIVE_BASELINE_SHA" \
  "$SOURCE_LOCAL_COMPOSE_CONTENT_SHA" "$CCE_LOCAL_COMPOSE_TARGET_SHA" \
  deploy/teagramd/rollout-runner/local-compose-cce00d1.yml \
  .rollout-compose.local-df9ffe5.yml "$APPLY_SOURCE_SHA"
assert_carried_revision_forwarding target-local-carry-forward-cce-fast-forward \
  target-local-carry-forward-cce-fast-forward "$APPLY_TARGET_SHA" "$LIVE_BASELINE_SHA" \
  "$SOURCE_LOCAL_COMPOSE_CONTENT_SHA" "$CCE_LOCAL_COMPOSE_TARGET_SHA" \
  deploy/teagramd/rollout-runner/local-compose-cce00d1.yml \
  .rollout-compose.local-cce00d1.yml "$APPLY_SOURCE_SHA"

prepare_target_local_apply_fixture target-local-carry-forward-cce-changed-byte \
  target-local-carry-forward-cce-changed-byte "$APPLY_TARGET_SHA" "$LIVE_BASELINE_SHA"
assert_target_local_rejected_before_backup target-local-carry-forward-cce-changed-byte \
  'carried-forward local Compose content is not a reviewed contract'

prepare_target_local_apply_fixture target-local-carry-forward-cce-source-artifact-missing \
  target-local-carry-forward-cce-source-artifact-missing "$APPLY_TARGET_SHA" "$LIVE_BASELINE_SHA"
assert_target_local_rejected_before_backup target-local-carry-forward-cce-source-artifact-missing \
  'cannot read the last reviewed local Compose content'

prepare_target_local_apply_fixture target-local-carry-forward-cce-source-artifact-different \
  target-local-carry-forward-cce-source-artifact-different "$APPLY_TARGET_SHA" "$LIVE_BASELINE_SHA"
assert_target_local_rejected_before_backup target-local-carry-forward-cce-source-artifact-different \
  'last reviewed local Compose source differs from the approved content'

prepare_target_local_apply_fixture target-local-carry-forward-cce-source-runner-different \
  target-local-carry-forward-cce-source-runner-different "$APPLY_TARGET_SHA" "$LIVE_BASELINE_SHA"
assert_target_local_rejected_before_backup target-local-carry-forward-cce-source-runner-different \
  'runtime copy differs from reviewed source revision: deploy/teagramd/rollout-runner/rollout-runner.sh'

prepare_target_local_apply_fixture target-local-carry-forward-cce-source-off-main \
  target-local-carry-forward-cce-source-off-main "$APPLY_TARGET_SHA" "$LIVE_BASELINE_SHA"
assert_target_local_rejected_before_backup target-local-carry-forward-cce-source-off-main \
  'rollout runtime source is not a reviewed origin/main commit'

prepare_target_local_apply_fixture target-local-carry-forward-cce-target-not-ancestor-source \
  target-local-carry-forward-cce-target-not-ancestor-source "$APPLY_TARGET_SHA" "$LIVE_BASELINE_SHA"
assert_target_local_rejected_before_backup target-local-carry-forward-cce-target-not-ancestor-source \
  'reviewed runtime source does not descend from the fixed application target'

prepare_target_local_apply_fixture target-local-cce-historical-over-live-baseline \
  target-local-cce-historical-over-live-baseline "$CCE_LOCAL_COMPOSE_TARGET_SHA" "$LIVE_BASELINE_SHA"
assert_target_local_rejected_before_backup target-local-cce-historical-over-live-baseline \
  'authorized application target is not a fast-forward from the live baseline'

assert_carried_revision_forwarding target-local-carry-forward-df9-filename-cce \
  target-local-carry-forward-df9-filename-cce "$APPLY_TARGET_SHA" "$LIVE_BASELINE_SHA" \
  "$APPROVED_LOCAL_COMPOSE_CONTENT_SHA" "$DF9_LOCAL_COMPOSE_TARGET_SHA" \
  deploy/teagramd/rollout-runner/local-compose-df9ffe5.yml \
  .rollout-compose.local-cce00d1.yml "$APPLY_TARGET_SHA"

assert_carried_build_provenance_rejected() {
  local name=$1 expected_error=$2 expected_reason=$3 expect_build=$4
  local status state root baseline_sha baseline_image error_ok=0 reason_ok=0 head_ok=0 tag_ok=0 up_ok=0 build_ok=0
  prepare_target_local_apply_fixture "$name" "$name" "$APPLY_TARGET_SHA" "$LIVE_BASELINE_SHA" || return 1
  state=$(cat "$TMP/$name-state-path")
  root=$(cat "$TMP/$name-root-path")
  baseline_sha=$(cat "$TMP/$name-baseline-sha-path")
  status=$(run_fixture "$name" built 0 '' 2 '' '' apply "$APPLY_SOURCE_SHA")
  baseline_image=$(jq -er '.image_id | ltrimstr("sha256:")' "$root.baseline/baseline.snapshot.json")
  [ "$(cat "$state/head")" = "$baseline_sha" ] && head_ok=1
  [ "$(cat "$state/tag")" = "$baseline_image" ] && tag_ok=1
  grep -Fq "$expected_error" "$TMP/$name.stderr" && error_ok=1
  grep -Fq "reason=$expected_reason" "$root.build/build-result.txt" && reason_ok=1
  ! grep -Fxq 'docker compose up -d' "$TMP/$name-events" && up_ok=1
  if [ "$expect_build" = 1 ]; then
    grep -Fxq build "$TMP/$name-events" && build_ok=1
  else
    ! grep -Fxq build "$TMP/$name-events" && build_ok=1
  fi
  if [ "$status" != 0 ] && [ "$error_ok" = 1 ] && [ "$reason_ok" = 1 ] && \
     [ "$head_ok" = 1 ] && [ "$tag_ok" = 1 ] && [ "$up_ok" = 1 ] && [ "$build_ok" = 1 ]; then
    pass "$name rejects build provenance and restores the checkout and image tag before target up"
  else
    printf 'build_provenance_rejection status=%s error=%s reason=%s head=%s tag=%s expected_tag=%s up_absent=%s build=%s expected_build=%s\n' \
      "$status" "$error_ok" "$reason_ok" "$head_ok" "$tag_ok" "$baseline_image" "$up_ok" "$build_ok" "$expect_build" >&2
    show_fixture_failure "$name" "$status"
    fail "$name must reject build provenance and restore the baseline before target up"
  fi
}

assert_carried_build_provenance_rejected target-local-carry-forward-cce-literal-build-arg \
  'resolved Compose build revision differs from the validated application target' \
  compose-build-provenance-rejected 0
assert_carried_build_provenance_rejected target-local-carry-forward-cce-wrong-context \
  'resolved Compose build context differs from the validated application checkout' \
  compose-build-provenance-rejected 0
assert_carried_build_provenance_rejected target-local-carry-forward-cce-label-empty \
  'built image revision label differs from the validated application target' \
  built-image-revision-label-mismatch 1
assert_carried_build_provenance_rejected target-local-carry-forward-cce-label-mismatch \
  'built image revision label differs from the validated application target' \
  built-image-revision-label-mismatch 1

prepare_target_local_apply_fixture target-local-cce-wrong-digest target-local-cce-wrong-digest \
  "$CCE_LOCAL_COMPOSE_TARGET_SHA"
assert_target_local_rejected_before_backup target-local-cce-wrong-digest \
  'target-local Compose artifact differs from the reviewed pin'

prepare_target_local_apply_fixture target-local-cce-tampered-content target-local-cce-tampered-content \
  "$CCE_LOCAL_COMPOSE_TARGET_SHA"
assert_target_local_rejected_before_backup target-local-cce-tampered-content \
  'target-local Compose content differs from the reviewed content pin'

prepare_target_local_apply_fixture target-local-cce-tampered-body-pin target-local-cce-tampered-body-pin \
  "$CCE_LOCAL_COMPOSE_TARGET_SHA"
assert_target_local_rejected_before_backup target-local-cce-tampered-body-pin \
  'target-local Compose content differs from the reviewed content pin'

prepare_target_local_apply_fixture target-local-cce-source-provenance target-local-cce-source-provenance \
  "$CCE_LOCAL_COMPOSE_TARGET_SHA"
assert_target_local_rejected_before_backup target-local-cce-source-provenance \
  'runtime copy differs from reviewed source revision: deploy/teagramd/rollout-runner/local-compose-cce00d1.yml'

for guarded_rejection in \
  target-local-cce-selection-omits-override \
  target-local-cce-extra-compose-file \
  target-local-cce-wrong-order; do
  prepare_target_local_apply_fixture "$guarded_rejection" "$guarded_rejection" \
    "$CCE_LOCAL_COMPOSE_TARGET_SHA"
  case "$guarded_rejection" in
    target-local-cce-selection-omits-override) expected_error='COMPOSE_FILE omits the existing docker-compose.override.yml' ;;
    target-local-cce-extra-compose-file) expected_error='target-local COMPOSE_FILE includes an unapproved Compose file' ;;
    target-local-cce-wrong-order) expected_error='existing Compose override must follow the target-local artifact' ;;
  esac
  assert_target_local_rejected_before_backup "$guarded_rejection" "$expected_error"
done

prepare_target_local_apply_fixture target-local-cce-artifact-with-df9-target \
  target-local-cce-artifact-with-df9-target "$DF9_LOCAL_COMPOSE_TARGET_SHA"
assert_target_local_rejected_before_backup target-local-cce-artifact-with-df9-target \
  'target-local COMPOSE_FILE includes an unapproved Compose file'

prepare_target_local_apply_fixture target-local-df9-artifact-with-cce-target \
  target-local-df9-artifact-with-cce-target "$CCE_LOCAL_COMPOSE_TARGET_SHA"
assert_target_local_rejected_before_backup target-local-df9-artifact-with-cce-target \
  'target-local COMPOSE_FILE includes an unapproved Compose file'

prepare_target_local_apply_fixture target-local-carry-forward-apply-success \
  target-local-carry-forward-apply-success "$APPLY_TARGET_SHA"
state=$(cat "$TMP/target-local-carry-forward-apply-success-state-path")
checkout=$(cat "$TMP/target-local-carry-forward-apply-success-checkout-path")
root=$(cat "$TMP/target-local-carry-forward-apply-success-root-path")
status=$(run_fixture target-local-carry-forward-apply-success built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
compose_selection=$(awk -F '\t' '{print $1}' "$TMP/target-local-carry-forward-apply-success-compose-selections" | sort -u)
if [ "$status" = 0 ] && grep -q "rollout=verified sha=$APPLY_TARGET_SHA" "$TMP/target-local-carry-forward-apply-success.stdout" && \
   [ "$(cat "$state/head")" = "$APPLY_TARGET_SHA" ] && \
   [ "$compose_selection" = '.rollout-compose.local-df9ffe5.yml:docker-compose.override.yml' ] && \
   [ ! -e "$checkout/.rollout-compose.local-${APPLY_TARGET_SHA:0:7}.yml" ] && \
   grep -q "target_local_compose_target_sha=$DF9_LOCAL_COMPOSE_TARGET_SHA target_local_compose_sha256=$DF9_LOCAL_COMPOSE_ARTIFACT_SHA" "$root.baseline/runtime-pins.txt" && \
   grep -q "target_local_compose_content_sha256=$APPROVED_LOCAL_COMPOSE_CONTENT_SHA" "$root.baseline/runtime-pins.txt" && \
   grep -q "compose_content_sha256=$APPROVED_LOCAL_COMPOSE_CONTENT_SHA" "$root.target/pre-backup-blob-authority.txt"; then
  pass 'new target carries forward the live pinned Compose file without a target-specific artifact'
else
  show_fixture_failure target-local-carry-forward-apply-success "$status"
  fail 'new local target must reuse the live reviewed Compose content'
fi

prepare_target_local_apply_fixture target-local-carry-forward-detached-checkout \
  target-local-carry-forward-detached-checkout "$APPLY_TARGET_SHA"
state=$(cat "$TMP/target-local-carry-forward-detached-checkout-state-path")
status=$(run_fixture target-local-carry-forward-detached-checkout built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
if [ "$status" = 0 ] && grep -q "rollout=verified sha=$APPLY_TARGET_SHA" \
  "$TMP/target-local-carry-forward-detached-checkout.stdout" && \
  [ "$(cat "$state/head")" = "$APPLY_TARGET_SHA" ]; then
  pass 'apply accepts the detached checkout created by a stack move'
else
  show_fixture_failure target-local-carry-forward-detached-checkout "$status"
  fail 'apply must accept a detached checkout when the reviewed SHA gates pass'
fi

prepare_target_local_apply_fixture target-local-carry-forward-non-main-checkout \
  target-local-carry-forward-non-main-checkout "$APPLY_TARGET_SHA"
assert_target_local_rejected_before_backup target-local-carry-forward-non-main-checkout \
  'deployment checkout must be on main or detached'

write_env_record() {
  local checkout=$1 env_sha
  env_sha=$(sha256sum "$checkout/.env" | awk '{print $1}')
  printf 'env_sha256=%s source_sha=%s\n' "$env_sha" "$TARGET_SHA" > "$checkout/.state/rollout-env"
  chmod 600 "$checkout/.state/rollout-env"
}

# Registers the directory as a fixture root first, so the EXIT cleanup removes
# it even when the run stops before the explicit removal.
write_verified_env_evidence() {
  local dir=$1 env_file=$2 root=${1%.target}
  [ "$root" != "$dir" ] || return 1
  printf '%s\n' "$root" > "$TMP/synthetic-${root##*/}-root-path"
  mkdir -m 700 "$dir"
  printf 'result=verified source_sha=%s\n' "$APPLY_TARGET_SHA" > "$dir/target-result.txt"
  jq -nc --arg env_sha "$(sha256sum "$env_file" | awk '{print $1}')" \
    '{env_sha256:$env_sha,provenance:{compose_source:"selected-compose-render"}}' > "$dir/target.snapshot.json"
  chmod 600 "$dir"/*
}

write_verified_override_evidence() {
  local dir=$1 override_file=$2 root=${1%.target}
  [ "$root" != "$dir" ] || return 1
  printf '%s\n' "$root" > "$TMP/synthetic-${root##*/}-root-path"
  mkdir -m 700 "$dir"
  printf 'result=verified source_sha=%s\n' "$APPLY_TARGET_SHA" > "$dir/target-result.txt"
  jq -nc --arg override_sha "$(sha256sum "$override_file" | awk '{print $1}')" \
    '{override_sha256:$override_sha,provenance:{compose_source:"selected-compose-render"}}' > "$dir/target.snapshot.json"
  chmod 600 "$dir"/*
}

prepare_websocket_origin_override_change_fixture() {
  local name=$1 scenario=${2:-target-local-df9-rollback}
  local state checkout previous_override override_evidence_root
  prepare_target_local_apply_fixture "$name" "$scenario" \
    "$DF9_LOCAL_COMPOSE_TARGET_SHA" "$TARGET_LOCAL_COMPOSE_TARGET_SHA" || return 1
  state=$(cat "$TMP/$name-state-path")
  checkout=$(cat "$TMP/$name-checkout-path")
  previous_override="$TMP/$name-previous.yml"
  printf '%s\n' 'services:' '  teagramd:' '    environment:' \
    '      TG_WEBSOCKET_ALLOWED_ORIGINS: https://teagram.tailaa4918.ts.net' \
    > "$checkout/docker-compose.override.yml"
  cp -p -- "$checkout/docker-compose.override.yml" "$previous_override"
  sed -i 's#https://teagram.tailaa4918.ts.net#https://teagram.tailaa4918.ts.net,https://web.teagram.io#' \
    "$checkout/docker-compose.override.yml"
  chmod 600 "$checkout/docker-compose.override.yml" "$previous_override"
  printf '%s\n' TG_WEBSOCKET_ALLOWED_ORIGINS > "$TMP/$name-changed-env"
  printf '%s\n' "$previous_override" > "$TMP/$name-previous-override-path"
  override_evidence_root="/root/main1238-${APPLY_TARGET_SHA:0:12}-$(cat "$TMP/$name-stamp")"
  write_verified_override_evidence "$override_evidence_root.target" "$previous_override"
  touch "$state/websocket-origin-override-change"
}

# The fixtures share /root, so a host with no verified .env at all is staged by
# parking the evidence earlier fixtures left there. Only directories recorded by
# this run's fixtures move, never other evidence on the host, so an interrupted
# run loses nothing the EXIT cleanup would not remove anyway.
park_verified_evidence() {
  local parked="$TMP/parked-evidence" root_file root phase dir
  mkdir -m 700 "$parked"
  for root_file in "$TMP"/*-root-path; do
    [ -f "$root_file" ] || continue
    root=$(cat "$root_file")
    for phase in target rollback; do
      dir="$root.$phase"
      [ -d "$dir" ] || continue
      mv -- "$dir" "$parked/"
    done
  done
}

restore_verified_evidence() {
  local parked="$TMP/parked-evidence" dir
  for dir in "$parked"/*; do
    [ -d "$dir" ] || continue
    mv -- "$dir" /root/
  done
  rmdir -- "$parked"
}

prepare_env_addition_fixture() {
  local name=$1 added=$2 previous_lines=$3 added_lines=$4 container_extra=$5 scenario=${6:-target-local-carry-forward-env-addition}
  local target_sha=${7:-$APPLY_TARGET_SHA} baseline_sha=${8:-$TARGET_SHA} checkout previous state line key value
  local rollback_extra=${9:-}
  prepare_target_local_apply_fixture "$name" "$scenario" "$target_sha" "$baseline_sha" || return 1
  state=$(cat "$TMP/$name-state-path")
  checkout=$(cat "$TMP/$name-checkout-path")
  previous="$TMP/$name-previous.env"
  if [ -n "$previous_lines" ]; then
    printf '%s\n' "$previous_lines" >> "$checkout/.env"
    write_env_record "$checkout" || return 1
  fi
  cp -p -- "$checkout/.env" "$previous" || return 1
  if [ "$name" = env-addition-changed-pin ]; then
    sed -i 's/^TG_RSA_KEY_FINGERPRINT=.*/TG_RSA_KEY_FINGERPRINT=changed/' "$checkout/.env" || return 1
  fi
  [ -z "$added_lines" ] || printf '%s\n' "$added_lines" >> "$checkout/.env"
  [ -z "$added" ] || printf '%s\n' "$added" > "$TMP/$name-added-env"
  [ -z "$added" ] || printf '%s\n' "$previous" > "$TMP/$name-previous-env-path"
  [ -z "$container_extra" ] || printf '%s\n' "$container_extra" > "$TMP/$name-target-env-extra"
  [ -z "$rollback_extra" ] || printf '%s\n' "$rollback_extra" > "$TMP/$name-rollback-env-extra"
  if [ -n "$added" ]; then
    while IFS= read -r line; do
      [ -n "$line" ] || continue
      key=${line%%=*}
      value=${line#*=}
      jq -c --arg key "$key" --arg value "$value" \
        '.services.teagramd.environment[$key]=$value' "$state/target-compose.json" > "$TMP/$name-target-compose.json" || return 1
      mv -- "$TMP/$name-target-compose.json" "$state/target-compose.json" || return 1
    done <<< "$added_lines"
  fi
}

prepare_env_change_fixture() {
  local name=$1 scenario=${2:-target-local-carry-forward-env-value-change}
  local target_sha=${3:-$APPLY_TARGET_SHA} baseline_sha=${4:-$TARGET_SHA}
  local target_extra=${5-TG_REGISTRATION=open} rollback_extra=${6-}
  local key=${7:-TG_REGISTRATION} old_value=${8:-closed} new_value=${9:-open}
  local setup_lines=${10:-} baseline_extra=${11:-}
  local checkout state previous env_tmp line matched=0
  prepare_target_local_apply_fixture "$name" "$scenario" "$target_sha" "$baseline_sha" || return 1
  state=$(cat "$TMP/$name-state-path")
  checkout=$(cat "$TMP/$name-checkout-path")
  printf '%s=%s\n' "$key" "$old_value" >> "$checkout/.env"
  [ -z "$setup_lines" ] || printf '%s\n' "$setup_lines" >> "$checkout/.env"
  write_env_record "$checkout" || return 1
  previous="$TMP/$name-previous.env"
  cp -p -- "$checkout/.env" "$previous" || return 1
  env_tmp="$TMP/$name-updated.env"
  : > "$env_tmp" || return 1
  while IFS= read -r line || [ -n "$line" ]; do
    if [[ "$line" == "${key}="* ]] && [ "${line#*=}" = "$old_value" ]; then
      printf '%s=%s\n' "$key" "$new_value" >> "$env_tmp" || return 1
      matched=$((matched + 1))
    else
      printf '%s\n' "$line" >> "$env_tmp" || return 1
    fi
  done < "$checkout/.env"
  [ "$matched" -eq 1 ] || return 1
  mv -- "$env_tmp" "$checkout/.env" || return 1
  printf '%s\n' "$key" > "$TMP/$name-changed-env"
  printf '%s\n' "$previous" > "$TMP/$name-previous-env-path"
  [ -z "$baseline_extra" ] || printf '%s\n' "$baseline_extra" > "$TMP/$name-baseline-env-extra"
  [ -z "$target_extra" ] || printf '%s\n' "$target_extra" > "$TMP/$name-target-env-extra"
  [ -z "$rollback_extra" ] || printf '%s\n' "$rollback_extra" > "$TMP/$name-rollback-env-extra"
  jq -c --arg key "$key" --arg value "$new_value" '.services.teagramd.environment[$key]=$value' \
    "$state/target-compose.json" > "$TMP/$name-target-compose.json" || return 1
  mv -- "$TMP/$name-target-compose.json" "$state/target-compose.json"
}

# The live container was started from base-compose.json; the new render is
# target-compose.json.
record_live_render() {
  local state=$1
  cp -- "$state/base-compose.json" "$state/started-compose-$(cat "$state/teagramd").json"
}

turnstile_defaults_filter='
  .services.teagramd.environment.TG_TURNSTILE_SITE_KEY="" |
  .services.teagramd.environment.TG_TURNSTILE_SECRET_KEY_FILE="" |
  .services.teagramd.environment.TG_TURNSTILE_SIGNIN_ENABLED="false" |
  .services.teagramd.secrets |= ((. // []) | map(select(.source != "teagramd_turnstile")) + [{source:"teagramd_turnstile",target:"teagramd-turnstile"}]) |
  .secrets.teagramd_turnstile={file:"/dev/null"}
'

# Gives the live container the disabled Turnstile defaults that this Compose
# renders when no Turnstile host settings are configured.
seed_live_turnstile_defaults() {
  local name=$1 state
  state=$(cat "$TMP/$name-state-path")
  jq -c "$turnstile_defaults_filter" "$state/base-compose.json" > "$TMP/$name-base-compose.json" || return 1
  mv -- "$TMP/$name-base-compose.json" "$state/base-compose.json" || return 1
  record_live_render "$state"
}

prepare_signin_env_change_fixture() {
  local name=$1 old_value=$2 new_value=$3
  local checkout state secret_dir secret_source setup_lines config value
  setup_lines=$'TEAGRAM_TURNSTILE_SITE_KEY=fixture-site-key\nTEAGRAM_TURNSTILE_SECRET_KEY_FILE=.secrets/teagramd-turnstile'
  prepare_env_change_fixture "$name" target-local-carry-forward-env-value-change-signin \
    "$APPLY_TARGET_SHA" "$TARGET_SHA" '' '' TG_TURNSTILE_SIGNIN_ENABLED \
    "$old_value" "$new_value" "$setup_lines" || return 1
  checkout=$(cat "$TMP/$name-checkout-path")
  state=$(cat "$TMP/$name-state-path")
  secret_dir="$checkout/.secrets"
  secret_source="$secret_dir/teagramd-turnstile"
  mkdir -m 700 "$secret_dir"
  : > "$secret_source"
  chmod 444 "$secret_source"
  for config in base target; do
    if [ "$config" = base ]; then value=$old_value; else value=$new_value; fi
    jq -c --arg source "$secret_source" --arg signin "$value" '
      .services.teagramd.environment.TG_TURNSTILE_SITE_KEY="fixture-site-key" |
      .services.teagramd.environment.TG_TURNSTILE_SECRET_KEY_FILE="/run/secrets/teagramd-turnstile" |
      .services.teagramd.environment.TG_TURNSTILE_SIGNIN_ENABLED=$signin |
      .services.teagramd.secrets |= ((. // []) | map(select(.source != "teagramd_turnstile")) + [{source:"teagramd_turnstile",target:"teagramd-turnstile"}]) |
      .secrets.teagramd_turnstile={file:$source}
    ' "$state/$config-compose.json" > "$TMP/$name-$config-compose.json" || return 1
    mv -- "$TMP/$name-$config-compose.json" "$state/$config-compose.json" || return 1
  done
  record_live_render "$state"
}

assert_signin_env_change() {
  local name=$1 old_value=$2 new_value=$3
  local root checkout previous_env_sha current_env_sha status
  prepare_signin_env_change_fixture "$name" "$old_value" "$new_value" || return 1
  root=$(cat "$TMP/$name-root-path")
  checkout=$(cat "$TMP/$name-checkout-path")
  previous_env_sha=$(sha256sum "$TMP/$name-previous.env" | awk '{print $1}')
  current_env_sha=$(sha256sum "$checkout/.env" | awk '{print $1}')
  status=$(run_fixture "$name" built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
  if [ "$status" = 0 ] && grep -q "rollout=verified sha=$APPLY_TARGET_SHA" "$TMP/$name.stdout" && \
     [ "$previous_env_sha" != "$current_env_sha" ] && \
     grep -qx "recorded_env_sha256=$previous_env_sha recorded_from=record env_sha256=$current_env_sha added=none changed=TG_TURNSTILE_SIGNIN_ENABLED" "$root.baseline/env-baseline.txt" && \
     grep -qx "env_sha256=$current_env_sha source_sha=$APPLY_TARGET_SHA" "$checkout/.state/rollout-env" && \
     [ "$(jq -r '.container_env_changed' "$root.target/target.snapshot.json")" = TG_TURNSTILE_SIGNIN_ENABLED ] && \
     grep -q $'\tcontainer_env_sha256\t.*\tpass' "$root.target/target-comparisons.tsv"; then
    pass "TG_TURNSTILE_SIGNIN_ENABLED changes from $old_value to $new_value and records the new .env digest"
  else
    show_fixture_failure "$name" "$status"
    fail "TG_TURNSTILE_SIGNIN_ENABLED change from $old_value to $new_value must deploy through apply"
  fi
}

prepare_env_change_fixture env-value-change-success
root=$(cat "$TMP/env-value-change-success-root-path")
checkout=$(cat "$TMP/env-value-change-success-checkout-path")
previous_env_sha=$(sha256sum "$TMP/env-value-change-success-previous.env" | awk '{print $1}')
current_env_sha=$(sha256sum "$checkout/.env" | awk '{print $1}')
status=$(run_fixture env-value-change-success built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
if [ "$status" = 0 ] && grep -q "rollout=verified sha=$APPLY_TARGET_SHA" "$TMP/env-value-change-success.stdout" && \
   [ "$previous_env_sha" != "$current_env_sha" ] && \
   grep -qx "recorded_env_sha256=$previous_env_sha recorded_from=record env_sha256=$current_env_sha added=none changed=TG_REGISTRATION" "$root.baseline/env-baseline.txt" && \
   grep -qx "env_sha256=$current_env_sha source_sha=$APPLY_TARGET_SHA" "$checkout/.state/rollout-env" && \
   [ "$(jq -r '.container_env_changed' "$root.target/target.snapshot.json")" = TG_REGISTRATION ] && \
   grep -q $'\tcontainer_env_sha256\t.*\tpass' "$root.target/target-comparisons.tsv"; then
  pass 'named TG_REGISTRATION value change deploys, verifies the container value, and records the new .env digest'
else
  show_fixture_failure env-value-change-success "$status"
  fail 'named TG_REGISTRATION value change must deploy through apply'
fi

prepare_env_change_fixture tailnet-exchange-limit-change-success \
  target-local-carry-forward-env-value-change "$APPLY_TARGET_SHA" "$TARGET_SHA" \
  TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET=0 TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET=12 \
  TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET 12 0 '' TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET=12
state=$(cat "$TMP/tailnet-exchange-limit-change-success-state-path")
checkout=$(cat "$TMP/tailnet-exchange-limit-change-success-checkout-path")
root=$(cat "$TMP/tailnet-exchange-limit-change-success-root-path")
jq -c '.services.teagramd.environment.TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET="12"' \
  "$state/base-compose.json" > "$TMP/tailnet-exchange-limit-change-success-base-compose.json"
mv -- "$TMP/tailnet-exchange-limit-change-success-base-compose.json" "$state/base-compose.json"
record_live_render "$state"
previous_env_sha=$(sha256sum "$TMP/tailnet-exchange-limit-change-success-previous.env" | awk '{print $1}')
current_env_sha=$(sha256sum "$checkout/.env" | awk '{print $1}')
status=$(run_fixture tailnet-exchange-limit-change-success built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
if [ "$status" = 0 ] && grep -q "rollout=verified sha=$APPLY_TARGET_SHA" \
     "$TMP/tailnet-exchange-limit-change-success.stdout" && \
   grep -qx "recorded_env_sha256=$previous_env_sha recorded_from=record env_sha256=$current_env_sha added=none changed=TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET" \
     "$root.baseline/env-baseline.txt" && \
   [ "$(jq -r '.container_env_changed' "$root.target/target.snapshot.json")" = TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET ] && \
   grep -q $'preflight\tconfig_sha256\t.*\tpass' "$root.target/preflight-comparisons.tsv" && \
   grep -q $'\tcontainer_env_sha256\t.*\tpass' "$root.target/target-comparisons.tsv" && \
   grep -qx "env_sha256=$current_env_sha source_sha=$APPLY_TARGET_SHA" "$checkout/.state/rollout-env"; then
  pass 'named tailnet limit changes verify the configured value and preserve remaining rollout digests'
else
  show_fixture_failure tailnet-exchange-limit-change-success "$status"
  fail 'named tailnet limit change must deploy through apply'
fi

websocket_origin_before=https://teagram.tailaa4918.ts.net
websocket_origin_after=https://teagram.tailaa4918.ts.net,https://web.teagram.io
prepare_env_change_fixture websocket-origin-value-change \
  target-local-carry-forward-env-value-change "$APPLY_TARGET_SHA" "$TARGET_SHA" \
  "TG_WEBSOCKET_ALLOWED_ORIGINS=$websocket_origin_after" \
  "TG_WEBSOCKET_ALLOWED_ORIGINS=$websocket_origin_before" \
  TG_WEBSOCKET_ALLOWED_ORIGINS "$websocket_origin_before" "$websocket_origin_after" '' \
  "TG_WEBSOCKET_ALLOWED_ORIGINS=$websocket_origin_before"
state=$(cat "$TMP/websocket-origin-value-change-state-path")
checkout=$(cat "$TMP/websocket-origin-value-change-checkout-path")
root=$(cat "$TMP/websocket-origin-value-change-root-path")
touch "$state/websocket-origin-change"
jq -c --arg value "$websocket_origin_before" \
  '.services.teagramd.environment.TG_WEBSOCKET_ALLOWED_ORIGINS=$value' \
  "$state/base-compose.json" > "$TMP/websocket-origin-value-change-base-compose.json"
mv -- "$TMP/websocket-origin-value-change-base-compose.json" "$state/base-compose.json"
previous_env_sha=$(sha256sum "$TMP/websocket-origin-value-change-previous.env" | awk '{print $1}')
current_env_sha=$(sha256sum "$checkout/.env" | awk '{print $1}')
status=$(run_fixture websocket-origin-value-change built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
if [ "$status" = 0 ] && grep -q "rollout=verified sha=$APPLY_TARGET_SHA" "$TMP/websocket-origin-value-change.stdout" && \
   grep -qx "recorded_env_sha256=$previous_env_sha recorded_from=record env_sha256=$current_env_sha added=none changed=TG_WEBSOCKET_ALLOWED_ORIGINS" \
     "$root.baseline/env-baseline.txt" && \
   grep -qx "env_sha256=$current_env_sha source_sha=$APPLY_TARGET_SHA" "$checkout/.state/rollout-env" && \
   [ "$(jq -r '.container_env_changed' "$root.target/target.snapshot.json")" = TG_WEBSOCKET_ALLOWED_ORIGINS ] && \
   grep -q $'\tcontainer_env_sha256\t.*\tpass' "$root.target/target-comparisons.tsv"; then
  pass 'named TG_WEBSOCKET_ALLOWED_ORIGINS value change deploys through apply'
else
  show_fixture_failure websocket-origin-value-change "$status"
  fail 'named TG_WEBSOCKET_ALLOWED_ORIGINS value change must deploy through apply'
fi

prepare_env_change_fixture public-registration-value-change \
  target-local-carry-forward-env-value-change "$APPLY_TARGET_SHA" "$TARGET_SHA" \
  TG_PUBLIC_REGISTRATION=closed '' TG_PUBLIC_REGISTRATION invite closed '' TG_PUBLIC_REGISTRATION=invite
root=$(cat "$TMP/public-registration-value-change-root-path")
checkout=$(cat "$TMP/public-registration-value-change-checkout-path")
sed -i 's/^TG_PUBLIC_REGISTRATION=closed$/TG_PUBLIC_REGISTRATION="closed"/' "$checkout/.env"
current_env_sha=$(sha256sum "$checkout/.env" | awk '{print $1}')
status=$(run_fixture public-registration-value-change built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
if [ "$status" = 0 ] && grep -q "rollout=verified sha=$APPLY_TARGET_SHA" "$TMP/public-registration-value-change.stdout" && \
   [ "$(jq -r '.container_env_changed' "$root.target/target.snapshot.json")" = TG_PUBLIC_REGISTRATION ] && \
   grep -qx "env_sha256=$current_env_sha source_sha=$APPLY_TARGET_SHA" "$checkout/.state/rollout-env"; then
  pass 'quoted TG_PUBLIC_REGISTRATION changes from invite to closed through the named change path'
else
  show_fixture_failure public-registration-value-change "$status"
  fail 'quoted TG_PUBLIC_REGISTRATION change from invite to closed must use the named change path'
fi

prepare_env_change_fixture env-value-change-rollback target-local-df9-rollback "$APPLY_TARGET_SHA" \
  "$TARGET_LOCAL_COMPOSE_TARGET_SHA" TG_REGISTRATION=open TG_REGISTRATION=open
root=$(cat "$TMP/env-value-change-rollback-root-path")
checkout=$(cat "$TMP/env-value-change-rollback-checkout-path")
current_env_sha=$(sha256sum "$checkout/.env" | awk '{print $1}')
status=$(run_fixture env-value-change-rollback built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
if [ "$status" != 0 ] && grep -q "rollback=verified baseline_sha=$TARGET_LOCAL_COMPOSE_TARGET_SHA" "$TMP/env-value-change-rollback.stdout" && \
   [ "$(jq -r '.container_env_changed' "$root.rollback/rollback.snapshot.json")" = TG_REGISTRATION ] && \
   grep -q $'rollback\tcontainer_env_sha256\t.*\tpass' "$root.rollback/rollback-equivalence.tsv" && \
   grep -qx "env_sha256=$current_env_sha source_sha=$TARGET_LOCAL_COMPOSE_TARGET_SHA" "$checkout/.state/rollout-env"; then
  pass 'rollback keeps TG_REGISTRATION open and records the changed .env digest when Compose retains it'
else
  show_fixture_failure env-value-change-rollback "$status"
  fail 'rollback must verify and retain the named TG_REGISTRATION value change'
fi

prepare_env_change_fixture websocket-origin-value-change-rollback \
  target-local-df9-rollback "$DF9_LOCAL_COMPOSE_TARGET_SHA" "$TARGET_LOCAL_COMPOSE_TARGET_SHA" \
  "TG_WEBSOCKET_ALLOWED_ORIGINS=$websocket_origin_after" \
  "TG_WEBSOCKET_ALLOWED_ORIGINS=$websocket_origin_before" \
  TG_WEBSOCKET_ALLOWED_ORIGINS "$websocket_origin_before" "$websocket_origin_after" '' \
  "TG_WEBSOCKET_ALLOWED_ORIGINS=$websocket_origin_before"
state=$(cat "$TMP/websocket-origin-value-change-rollback-state-path")
checkout=$(cat "$TMP/websocket-origin-value-change-rollback-checkout-path")
root=$(cat "$TMP/websocket-origin-value-change-rollback-root-path")
touch "$state/websocket-origin-change"
jq -c --arg value "$websocket_origin_before" \
  '.services.teagramd.environment.TG_WEBSOCKET_ALLOWED_ORIGINS=$value' \
  "$state/base-compose.json" > "$TMP/websocket-origin-value-change-rollback-base-compose.json"
mv -- "$TMP/websocket-origin-value-change-rollback-base-compose.json" "$state/base-compose.json"
previous_env_sha=$(sha256sum "$TMP/websocket-origin-value-change-rollback-previous.env" | awk '{print $1}')
status=$(run_fixture websocket-origin-value-change-rollback built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
if [ "$status" != 0 ] && grep -q "rollback=verified baseline_sha=$TARGET_LOCAL_COMPOSE_TARGET_SHA" \
     "$TMP/websocket-origin-value-change-rollback.stdout" && \
   cmp -s "$checkout/.env" "$TMP/websocket-origin-value-change-rollback-previous.env" && \
   grep -qx "env_sha256=$previous_env_sha source_sha=$TARGET_LOCAL_COMPOSE_TARGET_SHA" \
     "$checkout/.state/rollout-env" && \
   [ "$(jq -r '.container_env_changed' "$root.rollback/rollback.snapshot.json")" = TG_WEBSOCKET_ALLOWED_ORIGINS ] && \
   grep -q $'rollback\tenv_sha256\t.*\tpass' "$root.rollback/rollback-equivalence.tsv" && \
   grep -q $'rollback\tcontainer_env_sha256\t.*\tpass' "$root.rollback/rollback-equivalence.tsv"; then
  pass 'failed TG_WEBSOCKET_ALLOWED_ORIGINS change restores the previous .env before rollback'
else
  show_fixture_failure websocket-origin-value-change-rollback "$status"
  fail 'rollback must restore the previous .env for TG_WEBSOCKET_ALLOWED_ORIGINS'
fi

park_verified_evidence
prepare_websocket_origin_override_change_fixture websocket-origin-override-group-writable
checkout=$(cat "$TMP/websocket-origin-override-group-writable-checkout-path")
chmod 622 "$checkout/docker-compose.override.yml"
assert_target_local_rejected_before_backup websocket-origin-override-group-writable \
  'Compose override permissions must not allow group or world writes'

prepare_websocket_origin_override_change_fixture websocket-origin-override-non-root-owned
checkout=$(cat "$TMP/websocket-origin-override-non-root-owned-checkout-path")
chown 65534:65534 "$checkout/docker-compose.override.yml"
assert_target_local_rejected_before_backup websocket-origin-override-non-root-owned \
  'override value changes require a root-owned regular non-symlink override file'

prepare_websocket_origin_override_change_fixture websocket-origin-override-change-success \
  target-local-carry-forward-env-value-change
checkout=$(cat "$TMP/websocket-origin-override-change-success-checkout-path")
root=$(cat "$TMP/websocket-origin-override-change-success-root-path")
previous_override="$TMP/websocket-origin-override-change-success-previous.yml"
override_evidence_root="/root/main1238-${APPLY_TARGET_SHA:0:12}-$(cat "$TMP/websocket-origin-override-change-success-stamp")"
current_env_sha=$(sha256sum "$checkout/.env" | awk '{print $1}')
previous_override_sha=$(sha256sum "$previous_override" | awk '{print $1}')
current_override_sha=$(sha256sum "$checkout/docker-compose.override.yml" | awk '{print $1}')
expected_env_baseline="recorded_env_sha256=$current_env_sha recorded_from=record env_sha256=$current_env_sha added=none changed=TG_WEBSOCKET_ALLOWED_ORIGINS source=override previous_override_sha256=$previous_override_sha previous_override_from=$override_evidence_root.target/target.snapshot.json current_override_sha256=$current_override_sha"
status=$(run_fixture websocket-origin-override-change-success built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
if [ "$status" = 0 ] && \
   grep -q "rollout=verified sha=$DF9_LOCAL_COMPOSE_TARGET_SHA" \
     "$TMP/websocket-origin-override-change-success.stdout" && \
   [ "$previous_override_sha" != "$current_override_sha" ] && \
   grep -Fxq "$expected_env_baseline" "$root.baseline/env-baseline.txt" && \
   [ "$(jq -r '.override_sha256' "$root.target/target.snapshot.json")" = "$current_override_sha" ] && \
   [ "$(jq -r '.container_env_changed' "$root.target/target.snapshot.json")" = TG_WEBSOCKET_ALLOWED_ORIGINS ] && \
   grep -q $'\toverride_sha256\t.*\tpass' "$root.target/target-comparisons.tsv" && \
   grep -q $'\tcontainer_env_sha256\t.*\tpass' "$root.target/target-comparisons.tsv" && \
   grep -qx "env_sha256=$current_env_sha source_sha=$DF9_LOCAL_COMPOSE_TARGET_SHA" \
     "$checkout/.state/rollout-env"; then
  pass 'override-only TG_WEBSOCKET_ALLOWED_ORIGINS change verifies and records target evidence'
else
  show_fixture_failure websocket-origin-override-change-success "$status"
  fail 'override-only TG_WEBSOCKET_ALLOWED_ORIGINS change must verify and record target evidence'
fi
restore_verified_evidence

park_verified_evidence
prepare_websocket_origin_override_change_fixture websocket-origin-override-change-rollback
state=$(cat "$TMP/websocket-origin-override-change-rollback-state-path")
checkout=$(cat "$TMP/websocket-origin-override-change-rollback-checkout-path")
root=$(cat "$TMP/websocket-origin-override-change-rollback-root-path")
previous_override="$TMP/websocket-origin-override-change-rollback-previous.yml"
status=$(run_fixture websocket-origin-override-change-rollback built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
if [ "$status" != 0 ] && grep -q "rollback=verified baseline_sha=$TARGET_LOCAL_COMPOSE_TARGET_SHA" \
     "$TMP/websocket-origin-override-change-rollback.stdout" && \
   cmp -s "$checkout/docker-compose.override.yml" "$previous_override" && \
   [ "$(jq -r '.container_env_changed' "$root.rollback/rollback.snapshot.json")" = TG_WEBSOCKET_ALLOWED_ORIGINS ] && \
   grep -q $'rollback\toverride_sha256\t.*\tpass' "$root.rollback/rollback-equivalence.tsv" && \
   grep -q $'rollback\tcontainer_env_sha256\t.*\tpass' "$root.rollback/rollback-equivalence.tsv"; then
  pass 'override-only TG_WEBSOCKET_ALLOWED_ORIGINS change restores and verifies the previous override on rollback'
else
  show_fixture_failure websocket-origin-override-change-rollback "$status"
  fail 'override-only TG_WEBSOCKET_ALLOWED_ORIGINS change must restore and verify the previous override'
fi
restore_verified_evidence

prepare_env_change_fixture env-value-change-unsupported
printf '%s\n' TG_LOG_LOGIN_CODES > "$TMP/env-value-change-unsupported-changed-env"
assert_target_local_rejected_before_backup env-value-change-unsupported \
  'environment value change TG_LOG_LOGIN_CODES is not an approved setting'

prepare_env_change_fixture env-value-change-invalid-value
checkout=$(cat "$TMP/env-value-change-invalid-value-checkout-path")
sed -i 's/^TG_REGISTRATION=open$/TG_REGISTRATION=unknown/' "$checkout/.env"
assert_target_local_rejected_before_backup env-value-change-invalid-value \
  'TG_REGISTRATION may change only between closed and open'

assert_signin_env_change signin-value-change-disable true false
assert_signin_env_change signin-value-change-enable false true

prepare_signin_env_change_fixture signin-value-change-invalid-value true maybe
assert_target_local_rejected_before_backup signin-value-change-invalid-value \
  'snapshot verifier: resolved Compose Turnstile mapping is incomplete or unsafe'

prepare_env_addition_fixture env-addition-success TG_TURNSTILE_SIGNIN_ENABLED '' \
  'TG_TURNSTILE_SIGNIN_ENABLED=false' ''
seed_live_turnstile_defaults env-addition-success
state=$(cat "$TMP/env-addition-success-state-path")
jq -c "$turnstile_defaults_filter" "$state/target-compose.json" > "$TMP/env-addition-success-target-compose.json"
mv -- "$TMP/env-addition-success-target-compose.json" "$state/target-compose.json"
root=$(cat "$TMP/env-addition-success-root-path")
checkout=$(cat "$TMP/env-addition-success-checkout-path")
status=$(run_fixture env-addition-success built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
previous_env_sha=$(sha256sum "$TMP/env-addition-success-previous.env" | awk '{print $1}')
current_env_sha=$(sha256sum "$checkout/.env" | awk '{print $1}')
if [ "$status" = 0 ] && grep -q "rollout=verified sha=$APPLY_TARGET_SHA" "$TMP/env-addition-success.stdout" && \
   [ "$previous_env_sha" != "$current_env_sha" ] && \
   grep -qx "recorded_env_sha256=$previous_env_sha recorded_from=record env_sha256=$current_env_sha added=TG_TURNSTILE_SIGNIN_ENABLED" "$root.baseline/env-baseline.txt" && \
   grep -qx "env_sha256=$current_env_sha source_sha=$APPLY_TARGET_SHA" "$checkout/.state/rollout-env" && \
   [ "$(jq -r '.container_env_added' "$root.baseline/baseline.snapshot.json")" = none ] && \
   [ "$(jq -r '.container_env_added' "$root.target/target.snapshot.json")" = TG_TURNSTILE_SIGNIN_ENABLED ] && \
   grep -q $'\tcontainer_env_sha256\t.*\tpass' "$root.target/target-comparisons.tsv"; then
  pass 'named .env addition deploys and records the previous and new .env digests'
else
  show_fixture_failure env-addition-success "$status"
  fail 'named .env addition must deploy through apply'
fi

prepare_env_addition_fixture turnstile-env-addition-success \
  TEAGRAM_TURNSTILE_SITE_KEY,TEAGRAM_TURNSTILE_SECRET_KEY_FILE '' \
  $'TEAGRAM_TURNSTILE_SITE_KEY=fixture-site-key\nTEAGRAM_TURNSTILE_SECRET_KEY_FILE=/placeholder' ''
seed_live_turnstile_defaults turnstile-env-addition-success
root=$(cat "$TMP/turnstile-env-addition-success-root-path")
checkout=$(cat "$TMP/turnstile-env-addition-success-checkout-path")
state=$(cat "$TMP/turnstile-env-addition-success-state-path")
secret_dir="$checkout/.secrets"
secret_source="$secret_dir/teagramd-turnstile"
mkdir -m 700 "$secret_dir"
: > "$secret_source"
chmod 444 "$secret_source"
sed -i "s|^TEAGRAM_TURNSTILE_SECRET_KEY_FILE=.*|TEAGRAM_TURNSTILE_SECRET_KEY_FILE=$secret_source|" "$checkout/.env"
jq -c --arg source "$secret_source" '
  .services.teagramd.environment |= del(.TEAGRAM_TURNSTILE_SITE_KEY, .TEAGRAM_TURNSTILE_SECRET_KEY_FILE) |
  .services.teagramd.environment.TG_TURNSTILE_SITE_KEY="fixture-site-key" |
  .services.teagramd.environment.TG_TURNSTILE_SECRET_KEY_FILE="/run/secrets/teagramd-turnstile" |
  .services.teagramd.environment.TG_TURNSTILE_SIGNIN_ENABLED="false" |
  .services.teagramd.secrets |= ((. // []) | map(select(.source != "teagramd_turnstile")) + [{source:"teagramd_turnstile",target:"teagramd-turnstile"}]) |
  .secrets.teagramd_turnstile={file:$source}
' "$state/target-compose.json" > "$TMP/turnstile-env-addition-success-target-compose.json"
mv -- "$TMP/turnstile-env-addition-success-target-compose.json" "$state/target-compose.json"
status=$(run_fixture turnstile-env-addition-success built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
current_env_sha=$(sha256sum "$checkout/.env" | awk '{print $1}')
if [ "$status" = 0 ] && grep -q "rollout=verified sha=$APPLY_TARGET_SHA" "$TMP/turnstile-env-addition-success.stdout" && \
   grep -qx "env_sha256=$current_env_sha source_sha=$APPLY_TARGET_SHA" "$checkout/.state/rollout-env" && \
   [ "$(jq -r '.container_env_added' "$root.target/target.snapshot.json")" = TEAGRAM_TURNSTILE_SECRET_KEY_FILE,TEAGRAM_TURNSTILE_SITE_KEY ] && \
   grep -q $'\tcontainer_env_sha256\t.*\tpass' "$root.target/target-comparisons.tsv"; then
  pass 'named TEAGRAM Turnstile host additions deploy through their mapped container settings'
else
  show_fixture_failure turnstile-env-addition-success "$status"
  fail 'mapped Turnstile .env additions must deploy through apply'
fi

prepare_env_addition_fixture public-websocket-addition-rollback TG_PUBLIC_WEBSOCKET_LISTEN_ADDR '' \
  'TG_PUBLIC_WEBSOCKET_LISTEN_ADDR=0.0.0.0:2444' TG_PUBLIC_WEBSOCKET_LISTEN_ADDR=0.0.0.0:2444 \
  target-local-df9-rollback "$DF9_LOCAL_COMPOSE_TARGET_SHA" "$TARGET_LOCAL_COMPOSE_TARGET_SHA"
state=$(cat "$TMP/public-websocket-addition-rollback-state-path")
checkout=$(cat "$TMP/public-websocket-addition-rollback-checkout-path")
root=$(cat "$TMP/public-websocket-addition-rollback-root-path")
jq -c '.services.teagramd.environment += {
  TG_PUBLIC_WEBSOCKET_LISTEN_ADDR:"",
  TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR:"",
  TG_PUBLIC_REGISTRATION:"closed",
  TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS:"",
  TG_WELCOME_CHANNEL:""
}' "$state/base-compose.json" > "$TMP/public-websocket-addition-rollback-base-compose.json"
mv -- "$TMP/public-websocket-addition-rollback-base-compose.json" "$state/base-compose.json"
jq -c '.services.teagramd.environment += {
  TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR:"",
  TG_PUBLIC_REGISTRATION:"closed",
  TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS:"",
  TG_WELCOME_CHANNEL:""
}' "$state/target-compose.json" > "$TMP/public-websocket-addition-rollback-target-compose.json"
mv -- "$TMP/public-websocket-addition-rollback-target-compose.json" "$state/target-compose.json"
: > "$state/public-websocket-addition"
previous_env_sha=$(sha256sum "$TMP/public-websocket-addition-rollback-previous.env" | awk '{print $1}')
status=$(run_fixture public-websocket-addition-rollback built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
if [ "$status" != 0 ] && grep -q "rollback=verified baseline_sha=$TARGET_LOCAL_COMPOSE_TARGET_SHA" "$TMP/public-websocket-addition-rollback.stdout" && \
   [ "$(jq -r '.container_env_added' "$root.target/target.snapshot.json")" = TG_PUBLIC_WEBSOCKET_LISTEN_ADDR ] && \
   [ "$(jq -r '.container_env_added' "$root.rollback/rollback.snapshot.json")" = none ] && \
   [ "$(sha256sum "$checkout/.env" | awk '{print $1}')" = "$previous_env_sha" ] && \
   grep -qx "env_sha256=$previous_env_sha source_sha=$TARGET_LOCAL_COMPOSE_TARGET_SHA" "$checkout/.state/rollout-env" && \
   grep -q $'rollback\tenv_sha256\t.*\tpass' "$root.rollback/rollback-equivalence.tsv" && \
   grep -q $'rollback\tcontainer_env_sha256\t.*\tpass' "$root.rollback/rollback-equivalence.tsv"; then
  pass 'rollback restores the previous .env and container environment after a public listener addition'
else
  show_fixture_failure public-websocket-addition-rollback "$status"
  fail 'public listener addition rollback must restore the previous environment'
fi

prepare_env_addition_fixture tailnet-exchange-limit-addition-rollback \
  TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET '' \
  TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET=0 TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET=0 \
  target-local-df9-rollback "$DF9_LOCAL_COMPOSE_TARGET_SHA" "$TARGET_LOCAL_COMPOSE_TARGET_SHA" \
  TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET=
state=$(cat "$TMP/tailnet-exchange-limit-addition-rollback-state-path")
checkout=$(cat "$TMP/tailnet-exchange-limit-addition-rollback-checkout-path")
root=$(cat "$TMP/tailnet-exchange-limit-addition-rollback-root-path")
jq -c '.services.teagramd.environment.TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET=""' \
  "$state/base-compose.json" > "$TMP/tailnet-exchange-limit-addition-rollback-base-compose.json"
mv -- "$TMP/tailnet-exchange-limit-addition-rollback-base-compose.json" "$state/base-compose.json"
printf '%s\n' TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET= \
  > "$TMP/tailnet-exchange-limit-addition-rollback-baseline-env-extra"
record_live_render "$state"
previous_env_sha=$(sha256sum "$TMP/tailnet-exchange-limit-addition-rollback-previous.env" | awk '{print $1}')
status=$(run_fixture tailnet-exchange-limit-addition-rollback built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
if [ "$status" != 0 ] && grep -q "rollback=verified baseline_sha=$TARGET_LOCAL_COMPOSE_TARGET_SHA" \
     "$TMP/tailnet-exchange-limit-addition-rollback.stdout" && \
   [ "$(jq -r '.container_env_added' "$root.baseline/baseline.snapshot.json")" = none ] && \
   [ "$(jq -r '.container_env_added' "$root.target/target.snapshot.json")" = TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET ] && \
   [ "$(jq -r '.container_env_added' "$root.rollback/rollback.snapshot.json")" = none ] && \
   [ "$(sha256sum "$checkout/.env" | awk '{print $1}')" = "$previous_env_sha" ] && \
   grep -qx "env_sha256=$previous_env_sha source_sha=$TARGET_LOCAL_COMPOSE_TARGET_SHA" "$checkout/.state/rollout-env" && \
   grep -q $'rollback\tenv_sha256\t.*\tpass' "$root.rollback/rollback-equivalence.tsv" && \
   grep -q $'rollback\tcontainer_env_sha256\t.*\tpass' "$root.rollback/rollback-equivalence.tsv"; then
  pass 'tailnet limit addition rollback restores the recorded .env and preserves the empty baseline mapping'
else
  show_fixture_failure tailnet-exchange-limit-addition-rollback "$status"
  fail 'tailnet limit addition rollback must restore the previous environment'
fi

prepare_env_addition_fixture invite-quota-addition-rollback \
  TG_USER_INVITE_QUOTA '' TG_USER_INVITE_QUOTA=5 TG_USER_INVITE_QUOTA=5 \
  target-local-df9-rollback "$DF9_LOCAL_COMPOSE_TARGET_SHA" "$TARGET_LOCAL_COMPOSE_TARGET_SHA" \
  TG_USER_INVITE_QUOTA=
state=$(cat "$TMP/invite-quota-addition-rollback-state-path")
checkout=$(cat "$TMP/invite-quota-addition-rollback-checkout-path")
root=$(cat "$TMP/invite-quota-addition-rollback-root-path")
jq -c '.services.teagramd.environment.TG_USER_INVITE_QUOTA=""' \
  "$state/base-compose.json" > "$TMP/invite-quota-addition-rollback-base-compose.json"
mv -- "$TMP/invite-quota-addition-rollback-base-compose.json" "$state/base-compose.json"
printf '%s\n' TG_USER_INVITE_QUOTA= \
  > "$TMP/invite-quota-addition-rollback-baseline-env-extra"
record_live_render "$state"
previous_env_sha=$(sha256sum "$TMP/invite-quota-addition-rollback-previous.env" | awk '{print $1}')
status=$(run_fixture invite-quota-addition-rollback built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
if [ "$status" != 0 ] && grep -q "rollback=verified baseline_sha=$TARGET_LOCAL_COMPOSE_TARGET_SHA" \
     "$TMP/invite-quota-addition-rollback.stdout" && \
   [ "$(jq -r '.container_env_added' "$root.baseline/baseline.snapshot.json")" = none ] && \
   [ "$(jq -r '.container_env_added' "$root.target/target.snapshot.json")" = TG_USER_INVITE_QUOTA ] && \
   [ "$(jq -r '.container_env_added' "$root.rollback/rollback.snapshot.json")" = none ] && \
   [ "$(sha256sum "$checkout/.env" | awk '{print $1}')" = "$previous_env_sha" ] && \
   grep -qx "env_sha256=$previous_env_sha source_sha=$TARGET_LOCAL_COMPOSE_TARGET_SHA" "$checkout/.state/rollout-env" && \
   grep -q $'rollback\tenv_sha256\t.*\tpass' "$root.rollback/rollback-equivalence.tsv" && \
   grep -q $'rollback\tcontainer_env_sha256\t.*\tpass' "$root.rollback/rollback-equivalence.tsv"; then
  pass 'invite quota addition rollback restores the prior .env and empty Compose default'
else
  show_fixture_failure invite-quota-addition-rollback "$status"
  fail 'invite quota addition rollback must restore the previous environment'
fi

prepare_env_addition_fixture env-addition-rollback TG_NEW_FLAG '' 'TG_NEW_FLAG=on' TG_NEW_FLAG=on \
  target-local-df9-rollback "$DF9_LOCAL_COMPOSE_TARGET_SHA" "$TARGET_LOCAL_COMPOSE_TARGET_SHA"
root=$(cat "$TMP/env-addition-rollback-root-path")
checkout=$(cat "$TMP/env-addition-rollback-checkout-path")
previous_env_sha=$(sha256sum "$TMP/env-addition-rollback-previous.env" | awk '{print $1}')
status=$(run_fixture env-addition-rollback built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
if [ "$status" != 0 ] && grep -q "rollback=verified baseline_sha=$TARGET_LOCAL_COMPOSE_TARGET_SHA" "$TMP/env-addition-rollback.stdout" && \
   [ "$(jq -r '.container_env_added' "$root.target/target.snapshot.json")" = TG_NEW_FLAG ] && \
   [ "$(jq -r '.container_env_added' "$root.rollback/rollback.snapshot.json")" = none ] && \
   grep -q $'rollback\tcontainer_env_sha256\t.*\tpass' "$root.rollback/rollback-equivalence.tsv" && \
   grep -q 'baseline_equivalence=pass' "$root.rollback/rollback-result.txt" && \
   grep -qx "env_sha256=$previous_env_sha source_sha=$TARGET_LOCAL_COMPOSE_TARGET_SHA" "$checkout/.state/rollout-env"; then
  clear_fixture_phases "$root"
  printf '%s\n' target-local-df9-apply-success > "$TMP/env-addition-rollback-scenario"
  status=$(run_fixture env-addition-rollback built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
  current_env_sha=$(sha256sum "$checkout/.env" | awk '{print $1}')
  if [ "$status" = 0 ] && grep -q "rollout=verified sha=$DF9_LOCAL_COMPOSE_TARGET_SHA" "$TMP/env-addition-rollback.stdout" && \
     [ "$(jq -r '.container_env_added' "$root.target/target.snapshot.json")" = TG_NEW_FLAG ] && \
     grep -qx "env_sha256=$current_env_sha source_sha=$DF9_LOCAL_COMPOSE_TARGET_SHA" "$checkout/.state/rollout-env"; then
    pass 'rollback preserves the previous .env digest and allows the named addition to be retried'
  else
    show_fixture_failure env-addition-rollback "$status"
    fail 'named .env addition must verify when retried after rollback'
  fi
else
  show_fixture_failure env-addition-rollback "$status"
  fail 'rollback must preserve the prior .env digest and omit the addition from the old container'
fi

prepare_env_addition_fixture env-addition-rollback-retained TG_NEW_FLAG '' 'TG_NEW_FLAG=on' TG_NEW_FLAG=on \
  target-local-df9-rollback "$DF9_LOCAL_COMPOSE_TARGET_SHA" "$TARGET_LOCAL_COMPOSE_TARGET_SHA" TG_NEW_FLAG=on
root=$(cat "$TMP/env-addition-rollback-retained-root-path")
checkout=$(cat "$TMP/env-addition-rollback-retained-checkout-path")
current_env_sha=$(sha256sum "$checkout/.env" | awk '{print $1}')
status=$(run_fixture env-addition-rollback-retained built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
if [ "$status" != 0 ] && grep -q "rollback=verified baseline_sha=$TARGET_LOCAL_COMPOSE_TARGET_SHA" "$TMP/env-addition-rollback-retained.stdout" && \
   [ "$(jq -r '.container_env_added' "$root.rollback/rollback.snapshot.json")" = TG_NEW_FLAG ] && \
   grep -qx "env_sha256=$current_env_sha source_sha=$TARGET_LOCAL_COMPOSE_TARGET_SHA" "$checkout/.state/rollout-env"; then
  clear_fixture_phases "$root"
  printf '%s\n' target-local-df9-apply-success > "$TMP/env-addition-rollback-retained-scenario"
  rm -- "$TMP/env-addition-rollback-retained-added-env" "$TMP/env-addition-rollback-retained-previous-env-path"
  status=$(run_fixture env-addition-rollback-retained built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
  if [ "$status" = 0 ] && grep -q "rollout=verified sha=$DF9_LOCAL_COMPOSE_TARGET_SHA" "$TMP/env-addition-rollback-retained.stdout" && \
     [ "$(jq -r '.container_env_added' "$root.baseline/baseline.snapshot.json")" = none ] && \
     grep -qx "env_sha256=$current_env_sha source_sha=$DF9_LOCAL_COMPOSE_TARGET_SHA" "$checkout/.state/rollout-env"; then
    pass 'rollback retains all named additions, records the current .env, and permits an unnamed retry'
  else
    show_fixture_failure env-addition-rollback-retained "$status"
    fail 'retry without names must verify when rollback retains all named additions'
  fi
else
  show_fixture_failure env-addition-rollback-retained "$status"
  fail 'rollback retaining all named additions must record the current .env digest'
fi

prepare_env_addition_fixture env-addition-unnamed TG_OTHER_FLAG '' 'TG_NEW_FLAG=on' TG_NEW_FLAG=on
assert_target_local_rejected_before_backup env-addition-unnamed \
  'environment file adds a line that is not a named environment addition'

prepare_env_addition_fixture env-addition-changed-pin TG_NEW_FLAG 'TG_RSA_KEY_FINGERPRINT=0123456789abcdef' \
  'TG_NEW_FLAG=on' TG_NEW_FLAG=on
assert_target_local_rejected_before_backup env-addition-changed-pin \
  'environment file changes or removes settings of the previous .env'

for protected_env in TG_RSA_KEY_FINGERPRINT TG_BLOB_S3_ENDPOINT RUSTFS_ACCESS_KEY; do
  prepare_env_addition_fixture "env-addition-protected-$protected_env" "$protected_env" '' "$protected_env=fixture" ''
  assert_target_local_rejected_before_backup "env-addition-protected-$protected_env" \
    "environment addition $protected_env is a protected deployment setting"
done

prepare_env_addition_fixture env-addition-live-setting TG_SYNTHETIC_FLAG '' 'TG_SYNTHETIC_FLAG=changed' ''
assert_target_local_rejected_before_backup env-addition-live-setting \
  'environment addition is already set in the live container: TG_SYNTHETIC_FLAG'

prepare_env_addition_fixture env-unlisted-addition '' '' 'TG_UNUSED_FLAG=on' ''
assert_target_local_rejected_before_backup env-unlisted-addition \
  'environment file differs from the last verified .env; name additions, name a supported value change, or restore it'

prepare_env_addition_fixture env-unlisted-inactive-change '' 'RUSTFS_ACCESS_KEY=parked' '' ''
checkout=$(cat "$TMP/env-unlisted-inactive-change-checkout-path")
sed -i 's/^RUSTFS_ACCESS_KEY=.*/RUSTFS_ACCESS_KEY=changed/' "$checkout/.env"
assert_target_local_rejected_before_backup env-unlisted-inactive-change \
  'environment file differs from the last verified .env; name additions, name a supported value change, or restore it'

prepare_env_addition_fixture env-addition-unverified-previous TG_NEW_FLAG '' 'TG_NEW_FLAG=on' ''
checkout=$(cat "$TMP/env-addition-unverified-previous-checkout-path")
printf 'env_sha256=%064d source_sha=%s\n' 0 "$TARGET_SHA" > "$checkout/.state/rollout-env"
assert_target_local_rejected_before_backup env-addition-unverified-previous \
  'previous environment file is not the last verified .env'

prepare_env_addition_fixture env-adopt-recorded '' '' '' ''
printf '1\n' > "$TMP/env-adopt-recorded-adopt-env"
assert_target_local_rejected_before_backup env-adopt-recorded \
  'a verified .env is already recorded; adoption is only for the first guarded apply'

prepare_env_addition_fixture env-adopt '' '' '' ''
checkout=$(cat "$TMP/env-adopt-checkout-path")
root=$(cat "$TMP/env-adopt-root-path")
rm -- "$checkout/.state/rollout-env"
printf '1\n' > "$TMP/env-adopt-adopt-env"
park_verified_evidence
status=$(run_fixture env-adopt built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
restore_verified_evidence
current_env_sha=$(sha256sum "$checkout/.env" | awk '{print $1}')
if [ "$status" = 0 ] && grep -q "rollout=verified sha=$APPLY_TARGET_SHA" "$TMP/env-adopt.stdout" && \
   grep -qx "recorded_env_sha256=adopted recorded_from=adopted env_sha256=$current_env_sha added=none" "$root.baseline/env-baseline.txt" && \
   grep -qx "env_sha256=$current_env_sha source_sha=$APPLY_TARGET_SHA" "$checkout/.state/rollout-env"; then
  pass 'apply with neither a record nor verified evidence adopts the live .env and records it'
else
  show_fixture_failure env-adopt "$status"
  fail 'adopting the live .env must deploy and record it'
fi

prepare_env_addition_fixture env-adopt-evidence '' '' 'TG_UNUSED_FLAG=on' ''
checkout=$(cat "$TMP/env-adopt-evidence-checkout-path")
rm -- "$checkout/.state/rollout-env"
printf '1\n' > "$TMP/env-adopt-evidence-adopt-env"
latest_evidence="/root/main1238-${APPLY_TARGET_SHA:0:12}-29991231T235959Z.target"
write_verified_env_evidence "$latest_evidence" "$TMP/env-adopt-evidence-previous.env"
assert_target_local_rejected_before_backup env-adopt-evidence \
  'a verified .env is already in evidence; adoption is only for a host without one'
rm -rf -- "$latest_evidence"

prepare_env_addition_fixture env-bootstrap-evidence '' '' '' ''
checkout=$(cat "$TMP/env-bootstrap-evidence-checkout-path")
root=$(cat "$TMP/env-bootstrap-evidence-root-path")
rm -- "$checkout/.state/rollout-env"
latest_evidence="/root/main1238-${APPLY_TARGET_SHA:0:12}-29991231T235959Z.target"
write_verified_env_evidence "$latest_evidence" "$checkout/.env"
status=$(run_fixture env-bootstrap-evidence built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
current_env_sha=$(sha256sum "$checkout/.env" | awk '{print $1}')
if [ "$status" = 0 ] && grep -q "rollout=verified sha=$APPLY_TARGET_SHA" "$TMP/env-bootstrap-evidence.stdout" && \
   grep -qx "recorded_env_sha256=$current_env_sha recorded_from=$latest_evidence/target.snapshot.json env_sha256=$current_env_sha added=none" "$root.baseline/env-baseline.txt" && \
   grep -qx "env_sha256=$current_env_sha source_sha=$APPLY_TARGET_SHA" "$checkout/.state/rollout-env"; then
  pass 'missing .env record is taken from the latest verified evidence'
else
  show_fixture_failure env-bootstrap-evidence "$status"
  fail 'missing .env record must fall back to the latest verified evidence'
fi

prepare_env_addition_fixture env-bootstrap-evidence-changed '' '' 'TG_UNUSED_FLAG=on' ''
checkout=$(cat "$TMP/env-bootstrap-evidence-changed-checkout-path")
rm -- "$checkout/.state/rollout-env"
assert_target_local_rejected_before_backup env-bootstrap-evidence-changed \
  'environment file differs from the last verified .env; name additions, name a supported value change, or restore it'
rm -rf -- "$latest_evidence"

# Synthetic evidence dated 2999 outranks every real deployment's evidence on the
# host, so a run interrupted before the explicit removal must still lose it.
interrupted_evidence="/root/main1238-${APPLY_TARGET_SHA:0:12}-29991230T000000Z.target"
scratch=$(mktemp -d "$TMP/interrupted-run.XXXXXXXX")
if (
  TMP=$scratch
  write_verified_env_evidence "$interrupted_evidence" "$checkout/.env"
  [ -d "$interrupted_evidence" ] || exit 1
  cleanup_fixtures
  [ ! -e "$interrupted_evidence" ] && [ ! -e "$scratch" ]
); then
  pass 'EXIT cleanup removes synthetic verified evidence an interrupted run left behind'
else
  rm -rf -- "$interrupted_evidence" "$scratch"
  fail 'synthetic verified evidence must be registered for EXIT cleanup'
fi

prepare_env_addition_fixture env-addition-container-unnamed '' '' '' TG_NEW_FLAG=on
root=$(cat "$TMP/env-addition-container-unnamed-root-path")
status=$(run_fixture env-addition-container-unnamed built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
if [ "$status" != 0 ] && ! grep -q 'rollout=verified' "$TMP/env-addition-container-unnamed.stdout" && \
   grep -q $'\tcontainer_env_sha256\t.*\tfail' "$root.target/target-comparisons.tsv"; then
  pass 'container environment addition without a named .env addition is rejected'
else
  show_fixture_failure env-addition-container-unnamed "$status"
  fail 'unnamed container environment addition must be rejected'
fi

prepare_env_addition_fixture env-addition-container-missing TG_NEW_FLAG '' 'TG_NEW_FLAG=on' '' \
  target-local-df9-rollback
root=$(cat "$TMP/env-addition-container-missing-root-path")
status=$(run_fixture env-addition-container-missing built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
if [ "$status" != 0 ] && ! grep -q 'rollout=verified' "$TMP/env-addition-container-missing.stdout" && \
   grep -q 'reason=snapshot_capture_failed' "$root.target/target-failure.txt" && \
   grep -q '^rollback=verified ' "$TMP/env-addition-container-missing.stdout"; then
  pass 'named .env addition missing from the target container is rejected and rolled back'
else
  show_fixture_failure env-addition-container-missing "$status"
  fail 'named .env addition missing from the target container must be rejected'
fi

prepare_env_addition_fixture env-addition-container-mismatched TG_NEW_FLAG '' 'TG_NEW_FLAG=on' TG_NEW_FLAG=off \
  target-local-df9-rollback
root=$(cat "$TMP/env-addition-container-mismatched-root-path")
status=$(run_fixture env-addition-container-mismatched built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
if [ "$status" != 0 ] && ! grep -q 'rollout=verified' "$TMP/env-addition-container-mismatched.stdout" && \
   grep -q 'reason=snapshot_capture_failed' "$root.target/target-failure.txt" && \
   grep -q '^rollback=verified ' "$TMP/env-addition-container-mismatched.stdout"; then
  pass 'named .env addition with a mismatched target container value is rejected and rolled back'
else
  show_fixture_failure env-addition-container-mismatched "$status"
  fail 'named .env addition with a mismatched target container value must be rejected'
fi

prepare_target_local_apply_fixture target-local-carry-forward-header-change \
  target-local-carry-forward-header-change "$APPLY_TARGET_SHA"
status=$(run_fixture target-local-carry-forward-header-change built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
if [ "$status" = 0 ] && grep -q "rollout=verified sha=$APPLY_TARGET_SHA" "$TMP/target-local-carry-forward-header-change.stdout"; then
  pass 'carried-forward Compose content permits a new header comment'
else
  show_fixture_failure target-local-carry-forward-header-change "$status"
  fail 'carried-forward local Compose validation must ignore header comments'
fi

prepare_target_local_apply_fixture target-local-carry-forward-wrong-content \
  target-local-carry-forward-wrong-content "$APPLY_TARGET_SHA"
assert_target_local_rejected_before_backup target-local-carry-forward-wrong-content \
  'target-local Compose artifact must be selected first and match the reviewed local content'

for guarded_rejection in \
  target-local-598-wrong-digest \
  target-local-598-symlink \
  target-local-598-non-root-owner \
  target-local-598-wrong-mode \
  target-local-598-missing-override \
  target-local-598-selection-omits-override \
  target-local-598-extra-compose-file \
  target-local-598-wrong-order; do
  prepare_target_local_apply_fixture "$guarded_rejection" "$guarded_rejection" "$TARGET_LOCAL_COMPOSE_TARGET_SHA"
  case "$guarded_rejection" in
    target-local-598-wrong-digest) expected_error='target-local Compose artifact differs from the reviewed pin' ;;
    target-local-598-symlink|target-local-598-non-root-owner|target-local-598-wrong-mode) expected_error='target-local Compose artifact must be a root-owned mode-0600 regular file' ;;
    target-local-598-missing-override) expected_error='target-local rollout requires the existing docker-compose.override.yml' ;;
    target-local-598-selection-omits-override) expected_error='COMPOSE_FILE omits the existing docker-compose.override.yml' ;;
    target-local-598-extra-compose-file) expected_error='target-local COMPOSE_FILE includes an unapproved Compose file' ;;
    target-local-598-wrong-order) expected_error='existing Compose override must follow the target-local artifact' ;;
  esac
  assert_target_local_rejected_before_backup "$guarded_rejection" "$expected_error"
done

for guarded_rejection in \
  target-local-df9-wrong-digest \
  target-local-df9-symlink \
  target-local-df9-non-root-owner \
  target-local-df9-wrong-mode \
  target-local-df9-missing-override \
  target-local-df9-selection-omits-override \
  target-local-df9-extra-compose-file \
  target-local-df9-wrong-order; do
  prepare_target_local_apply_fixture "$guarded_rejection" "$guarded_rejection" "$DF9_LOCAL_COMPOSE_TARGET_SHA"
  case "$guarded_rejection" in
    target-local-df9-wrong-digest) expected_error='target-local Compose artifact differs from the reviewed pin' ;;
    target-local-df9-symlink|target-local-df9-non-root-owner|target-local-df9-wrong-mode) expected_error='target-local Compose artifact must be a root-owned mode-0600 regular file' ;;
    target-local-df9-missing-override) expected_error='target-local rollout requires the existing docker-compose.override.yml' ;;
    target-local-df9-selection-omits-override) expected_error='COMPOSE_FILE omits the existing docker-compose.override.yml' ;;
    target-local-df9-extra-compose-file) expected_error='target-local COMPOSE_FILE includes an unapproved Compose file' ;;
    target-local-df9-wrong-order) expected_error='existing Compose override must follow the target-local artifact' ;;
  esac
  assert_target_local_rejected_before_backup "$guarded_rejection" "$expected_error"
done

prepare_target_local_apply_fixture target-local-df9-render-backend-mismatch target-local-df9-render-backend-mismatch "$DF9_LOCAL_COMPOSE_TARGET_SHA"
assert_target_local_rejected_before_backup target-local-df9-render-backend-mismatch 'render-backend-mismatch'

prepare_target_local_apply_fixture target-local-df9-render-volume-mismatch target-local-df9-render-volume-mismatch "$DF9_LOCAL_COMPOSE_TARGET_SHA"
assert_target_local_rejected_before_backup target-local-df9-render-volume-mismatch 'render-volume-mismatch'

prepare_target_local_apply_fixture target-local-df9-running-backend-mismatch target-local-df9-running-backend-mismatch "$DF9_LOCAL_COMPOSE_TARGET_SHA"
assert_target_local_rejected_before_backup target-local-df9-running-backend-mismatch 'running-backend-mismatch'

prepare_target_local_apply_fixture target-local-carry-forward-selection-omits-override \
  target-local-carry-forward-selection-omits-override "$APPLY_TARGET_SHA"
assert_target_local_rejected_before_backup target-local-carry-forward-selection-omits-override \
  'COMPOSE_FILE omits the existing docker-compose.override.yml'

prepare_target_local_apply_fixture target-local-carry-forward-extra-compose-file \
  target-local-carry-forward-extra-compose-file "$APPLY_TARGET_SHA"
assert_target_local_rejected_before_backup target-local-carry-forward-extra-compose-file \
  'target-local COMPOSE_FILE includes an unapproved Compose file'

prepare_target_local_apply_fixture target-local-carry-forward-wrong-order \
  target-local-carry-forward-wrong-order "$APPLY_TARGET_SHA"
assert_target_local_rejected_before_backup target-local-carry-forward-wrong-order \
  'target-local Compose artifact must be selected first and match the reviewed local content'

prepare_target_local_apply_fixture target-local-df9-artifact-with-598-target target-local-df9-artifact-with-598-target "$TARGET_LOCAL_COMPOSE_TARGET_SHA"
assert_target_local_rejected_before_backup target-local-df9-artifact-with-598-target 'target-local COMPOSE_FILE includes an unapproved Compose file'

prepare_target_local_apply_fixture target-local-598-artifact-with-df9-target target-local-598-artifact-with-df9-target "$DF9_LOCAL_COMPOSE_TARGET_SHA"
assert_target_local_rejected_before_backup target-local-598-artifact-with-df9-target 'target-local COMPOSE_FILE includes an unapproved Compose file'

prepare_target_local_apply_fixture target-local-598-render-backend-mismatch target-local-598-render-backend-mismatch "$TARGET_LOCAL_COMPOSE_TARGET_SHA"
assert_target_local_rejected_before_backup target-local-598-render-backend-mismatch 'render-backend-mismatch'

prepare_target_local_apply_fixture target-local-598-render-volume-mismatch target-local-598-render-volume-mismatch "$TARGET_LOCAL_COMPOSE_TARGET_SHA"
assert_target_local_rejected_before_backup target-local-598-render-volume-mismatch 'render-volume-mismatch'

prepare_target_local_apply_fixture target-local-e58-artifact-with-598-target target-local-e58-artifact-with-598-target "$TARGET_LOCAL_COMPOSE_TARGET_SHA"
assert_target_local_rejected_before_backup target-local-e58-artifact-with-598-target 'target-local COMPOSE_FILE includes an unapproved Compose file'

prepare_target_local_apply_fixture target-local-598-rollback target-local-598-rollback "$TARGET_LOCAL_COMPOSE_TARGET_SHA"
state=$(cat "$TMP/target-local-598-rollback-state-path")
root=$(cat "$TMP/target-local-598-rollback-root-path")
status=$(run_fixture target-local-598-rollback built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
compose_selection=$(awk -F '\t' '{print $1}' "$TMP/target-local-598-rollback-compose-selections" | sort -u)
if [ "$status" != 0 ] && grep -q "rollback=verified baseline_sha=$TARGET_SHA" "$TMP/target-local-598-rollback.stdout" && \
   [ "$TARGET_SHA" != "$TARGET_LOCAL_COMPOSE_TARGET_SHA" ] && [ "$(cat "$state/head")" = "$TARGET_SHA" ] && \
   [ "$(cat "$state/teagramd")" = "$ROLLBACK_ID" ] && \
   [ "$compose_selection" = '.rollout-compose.local-598359e.yml:docker-compose.override.yml' ] && \
   grep -q "target_sha=$TARGET_LOCAL_COMPOSE_TARGET_SHA expected_baseline_sha=$TARGET_SHA" "$root.baseline/runtime-pins.txt"; then
  pass '598359e rollback restores a captured baseline SHA different from its pinned application target'
else
  show_fixture_failure target-local-598-rollback "$status"
  fail 'target-local guard must preserve captured-baseline rollback for 598359e'
fi

prepare_target_local_apply_fixture target-local-df9-rollback target-local-df9-rollback "$DF9_LOCAL_COMPOSE_TARGET_SHA" "$TARGET_LOCAL_COMPOSE_TARGET_SHA"
state=$(cat "$TMP/target-local-df9-rollback-state-path")
root=$(cat "$TMP/target-local-df9-rollback-root-path")
status=$(run_fixture target-local-df9-rollback built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
compose_selection=$(awk -F '\t' '{print $1}' "$TMP/target-local-df9-rollback-compose-selections" | sort -u)
if [ "$status" != 0 ] && grep -q "rollback=verified baseline_sha=$TARGET_LOCAL_COMPOSE_TARGET_SHA" "$TMP/target-local-df9-rollback.stdout" && \
   [ "$(cat "$state/head")" = "$TARGET_LOCAL_COMPOSE_TARGET_SHA" ] && \
   [ "$(cat "$state/teagramd")" = "$ROLLBACK_ID" ] && \
   [ "$compose_selection" = '.rollout-compose.local-df9ffe5.yml:docker-compose.override.yml' ] && \
   grep -q "target_sha=$DF9_LOCAL_COMPOSE_TARGET_SHA expected_baseline_sha=$TARGET_LOCAL_COMPOSE_TARGET_SHA" "$root.baseline/runtime-pins.txt"; then
  pass 'df9ffe5 rollback restores the captured 598359e baseline under the separately pinned target artifact'
else
  show_fixture_failure target-local-df9-rollback "$status"
  fail 'df9ffe5 target-local guard must preserve rollback to the captured 598359e baseline'
fi

prepare_target_local_apply_fixture target-local-wrong-artifact target-local-wrong-artifact
assert_target_local_rejected_before_backup target-local-wrong-artifact 'target-local Compose artifact differs from the reviewed pin'

prepare_target_local_apply_fixture target-local-missing-override target-local-missing-override
assert_target_local_rejected_before_backup target-local-missing-override 'target-local rollout requires the existing docker-compose.override.yml'

prepare_target_local_apply_fixture target-local-selection-omits-override target-local-selection-omits-override
assert_target_local_rejected_before_backup target-local-selection-omits-override 'COMPOSE_FILE omits the existing docker-compose.override.yml'

prepare_target_local_apply_fixture target-local-render-backend-mismatch target-local-render-backend-mismatch
assert_target_local_rejected_before_backup target-local-render-backend-mismatch 'render-backend-mismatch'

prepare_target_local_apply_fixture target-local-render-volume-mismatch target-local-render-volume-mismatch
assert_target_local_rejected_before_backup target-local-render-volume-mismatch 'render-volume-mismatch'

prepare_target_local_apply_fixture target-local-running-backend-mismatch target-local-running-backend-mismatch
assert_target_local_rejected_before_backup target-local-running-backend-mismatch 'running-backend-mismatch'

prepare_target_local_uninitialized_fixture target-local-no-authority target-local-no-authority "$E58_LOCAL_COMPOSE_TARGET_SHA"
assert_target_local_rejected_before_backup target-local-no-authority 'state-unavailable'

prepare_target_local_uninitialized_fixture target-local-wrong-app-target target-local-wrong-app-target "$APPLY_TARGET_SHA"
assert_target_local_rejected_before_backup target-local-wrong-app-target 'state-unavailable'

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
   [ "$(cat "$state/teagramd")" = "$BASE_ID" ] && [ "$no_evidence" = 1 ]; then
  pass 'explicit Compose file lists must retain the existing override before any capture or start'
else
  fail 'missing Compose override must reject before publication, backup, or start'
fi

make_fixture initial-local-wrong-artifact-digest initial-local-wrong-artifact-digest
checkout=$(cat "$TMP/initial-local-wrong-artifact-digest-checkout-path")
printf '%s\n' 'tampered artifact' >> "$checkout/.rollout-compose.initial-local.yml"
assert_initial_local_compose_rejected initial-local-wrong-artifact-digest \
  'initial-local Compose artifact differs from the reviewed pin'

make_fixture initial-local-wrong-artifact-owner initial-local-wrong-artifact-owner
checkout=$(cat "$TMP/initial-local-wrong-artifact-owner-checkout-path")
chown 1:1 "$checkout/.rollout-compose.initial-local.yml"
assert_initial_local_compose_rejected initial-local-wrong-artifact-owner \
  'initial-local Compose artifact must be a root-owned mode-0600 regular file'

make_fixture initial-local-wrong-artifact-mode initial-local-wrong-artifact-mode
checkout=$(cat "$TMP/initial-local-wrong-artifact-mode-checkout-path")
chmod 640 "$checkout/.rollout-compose.initial-local.yml"
assert_initial_local_compose_rejected initial-local-wrong-artifact-mode \
  'initial-local Compose artifact must be a root-owned mode-0600 regular file'

make_fixture initial-local-extra-compose-file initial-local-extra-compose-file
checkout=$(cat "$TMP/initial-local-extra-compose-file-checkout-path")
cp "$(cd "$SCRIPT_DIR/../../.." && pwd -P)/docker-compose.yml" "$checkout/docker-compose.yml"
chmod 600 "$checkout/docker-compose.yml"
assert_initial_local_compose_rejected initial-local-extra-compose-file \
  'initial-local COMPOSE_FILE includes an unapproved Compose file'

make_fixture initial-local-compose-file-wrong-order initial-local-compose-file-wrong-order
assert_initial_local_compose_rejected initial-local-compose-file-wrong-order \
  'existing Compose override must follow the initial-local artifact'

make_fixture initial-local-invalid-s3 initial-local-s3-backend
status=$(run_fixture initial-local-invalid-s3)
checkout=$(cat "$TMP/initial-local-invalid-s3-checkout-path")
state=$(cat "$TMP/initial-local-invalid-s3-state-path")
if [ "$status" != 0 ] && grep -Eq 'initial-render-backend|resolved Compose preflight rejected unapproved drift' "$TMP/initial-local-invalid-s3.stderr" && \
   [ ! -e "$checkout/.state/blob-mode/mode.json" ] && \
   ! grep -Eq '^docker compose (build|up|stop|down)( |$)' "$TMP/initial-local-invalid-s3-events" && \
   [ "$(cat "$state/head")" = "$BASELINE_SHA" ] && [ "$(cat "$state/teagramd")" = "$BASE_ID" ]; then
  pass 'initial-local rejects an S3 render before publishing authority or replacing the baseline'
else
  show_fixture_failure initial-local-invalid-s3 "$status"
  fail 'initial-local must reject an invalid S3 backend selection'
fi

for rejected in missing-mode-mount missing-proxy-mode-mount wrong-tgblobs-volume wrong-mode-source wrong-blob-backend forbidden-override forbidden-blob-setting forbidden-tgblobs-mount; do
  make_fixture "blob-reject-$rejected" "$rejected"
  status=$(run_fixture "blob-reject-$rejected")
  state=$(cat "$TMP/blob-reject-$rejected-state-path")
  if [ "$status" != 0 ] && \
     ! grep -Eq '^docker compose (build|up|stop|down)( |$)' "$TMP/blob-reject-$rejected-events" && \
     [ ! -e "$(cat "$TMP/blob-reject-$rejected-checkout-path")/.state/blob-mode/mode.json" ] && \
     [ "$(cat "$state/head")" = "$BASELINE_SHA" ] && [ "$(cat "$state/teagramd")" = "$BASE_ID" ]; then
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
   [ "$(cat "$(cat "$TMP/ambiguous-state-state-path")/teagramd")" = "$BASE_ID" ] && \
   grep -q 'cannot inspect durable blob authority volumes' "$TMP/ambiguous-state.stderr"; then
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
   [ -f "/root/teagramd-blob-mode-report-$new_transition.json" ]; then
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
   [ "$(cat "$(cat "$TMP/abandoned-invalid-initialization-state-path")/teagramd")" = "$BASE_ID" ]; then
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
report="/root/teagramd-blob-mode-report-$transition.json"
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
   [ "$(cat "$state/teagramd")" = "$BASE_ID" ] && [ "$(cat "$state/head")" = "$TARGET_SHA" ]; then
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
mode_report="/root/teagramd-blob-mode-report-$transition.json"
printf ' ' >> "$mode_report"
state_before=$(sha256sum "$checkout/.state/blob-mode/mode.json" "$checkout/.state/blob-mode/journal/0000000001.json")
if [ "$status" = 0 ]; then
  set +e
  printf 'fixture validation: blob-report-tamper\n' >&2
  real_bash=$(command -v bash)
  real_python3=$(command -v python3)
  PATH="$bin:$PATH" MOCK_REAL_BASH="$real_bash" MOCK_REAL_PYTHON3="$real_python3" MOCK_EVENTS="$TMP/blob-report-tamper-validation-events" \
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
   [ "$(cat "$(cat "$TMP/blob-report-tamper-state-path")/teagramd")" = "$TARGET_ID" ]; then
  pass 'tampered private report rejects validation without rewriting authority or replacing containers'
else
  fail 'report hash and state-byte preservation gate'
fi

make_fixture runtime-mode-unmounted runtime-mode-unmounted
status=$(run_fixture runtime-mode-unmounted)
root=$(cat "$TMP/runtime-mode-unmounted-root-path")
if [ "$status" != 0 ] && grep -q 'rollback=verified' "$TMP/runtime-mode-unmounted.stdout" && \
   grep -q '^docker compose up -d --no-build --no-deps teagramd$' "$TMP/runtime-mode-unmounted-events" && \
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
   grep -q '^docker compose up -d --no-build --no-deps teagramd$' "$TMP/runtime-target-exited-events" && \
   [ "$(cat "$state/teagramd")" = "$ROLLBACK_ID" ]; then
  pass 'exited target with empty serving inventory validates authority and restores the inspected baseline'
else
  fail 'exited target rollback with empty serving inventory'
fi

make_fixture target-container-unavailable target-container-unavailable
status=$(run_fixture target-container-unavailable)
root=$(cat "$TMP/target-container-unavailable-root-path")
state=$(cat "$TMP/target-container-unavailable-state-path")
if [ "$status" != 0 ] && grep -q 'rollback=verified' "$TMP/target-container-unavailable.stdout" && \
   grep -q '^docker compose up -d --no-build --no-deps teagramd$' "$TMP/target-container-unavailable-events" && \
   [ "$(cat "$state/head")" = "$BASELINE_SHA" ] && \
   [ "$(cat "$state/teagramd")" = "$ROLLBACK_ID" ] && \
   grep -q 'reason=target_container_id_unavailable' "$root.target/target-failure.txt"; then
  pass 'target with no running container restores the baseline and verifies rollback'
else
  fail 'target container unavailable rollback must restore the baseline'
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
     ! grep -q '^docker compose up -d --no-build --no-deps teagramd$' "$TMP/$runtime_mismatch-events" && \
     [ "$(cat "$state/teagramd")" = "$TARGET_ID" ]; then
    pass "$runtime_mismatch preserves the current container when fresh rollback authority validation rejects"
  else
    fail "$runtime_mismatch must not replace a container with mismatched authority"
  fi
done

make_fixture old-image old-target-image
status=$(run_fixture old-image)
root=$(cat "$TMP/old-image-root-path")
if [ "$status" != 0 ] && grep -q $'target_image_matches_built\t.*\tfail' "$root".target/target-comparisons.tsv && \
   grep -q 'rollback=verified' "$TMP/old-image.stdout" && grep -q 'docker compose up -d --no-build --no-deps teagramd' "$TMP/old-image-events"; then
  pass 'old running image is rejected against captured built ID and baseline rollback verifies'
else
  fail 'old running image rejection and baseline rollback'
fi

prepare_apply_fixture rollback-logging-mismatch target-local-old-target-image-logging-mismatch
state=$(cat "$TMP/rollback-logging-mismatch-state-path")
jq -c 'del(.services.teagramd.logging)' "$state/base-compose.json" > "$state/base-compose.next.json"
mv -- "$state/base-compose.next.json" "$state/base-compose.json"
status=$(run_fixture rollback-logging-mismatch built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
root=$(cat "$TMP/rollback-logging-mismatch-root-path")
if [ "$status" != 0 ] && \
   grep -q $'target_image_matches_built\t.*\tfail' "$root".target/target-comparisons.tsv && \
   grep -q $'rollback\tcontainer_logging_sha256\t.*\tfail' "$root".rollback/rollback-equivalence.tsv && \
   ! grep -q 'rollback=verified' "$TMP/rollback-logging-mismatch.stdout"; then
  pass 'rollback with a different inspected logging driver is not reported as verified'
else
  show_fixture_failure rollback-logging-mismatch "$status"
  fail 'mismatched inspected rollback logging configuration must be rejected'
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
if [ "$status" != 0 ] && [ -n "$rollback_rows" ] && grep -q $'rollback\tselected_render_compose_replica_count\t1\t1\tpass' "$rollback_rows" && \
   grep -q $'rollback\tselected_render_compose_client_addr_trust\tsocket\tsocket\tpass' "$rollback_rows" && \
   grep -q $'rollback\ttarget_container_replica_count\tunset\t1\tpass' "$rollback_rows" && \
   grep -q $'rollback\ttarget_container_client_addr_trust\tunset\tsocket\tpass' "$rollback_rows" && \
   grep -q 'baseline_equivalence=pass' "$root".rollback/rollback-result.txt; then
  pass 'rollback compares live unset container values to target-render settings without mixing provenance'
else
  fail 'live baseline inspection and pinned rollback render equivalence'
fi

make_fixture config-drift config-drift
status=$(run_fixture config-drift)
root=$(cat "$TMP/config-drift-root-path")
state=$(cat "$TMP/config-drift-state-path")
if [ "$status" != 0 ] && grep -q $'preflight\tselected_render_config_sha256\t.*\tfail' "$root".target/preflight-comparisons.tsv && \
   [ "$(cat "$state/reset-umask" 2>/dev/null)" = 0022 ] && \
   grep -q 'result=rejected exit=[1-9]' "$root".target/preflight-failure.txt && \
   ! grep -q 'docker compose up -d$' "$TMP/config-drift-events" && grep -q 'target was not started' "$TMP/config-drift.stderr"; then
  pass 'unrelated Compose configuration drift rejects before build and service replacement'
else
  printf 'config_drift_status=%s\nconfig_drift_stderr=%s\nconfig_drift_events=%s\n' \
    "$status" "$(cat "$TMP/config-drift.stderr")" "$(cat "$TMP/config-drift-events")"
  cat "$root".target/preflight-comparisons.tsv 2>/dev/null || true
  fail 'configuration drift rejection'
fi

make_fixture gate-mutation gate-mutation
status=$(run_fixture gate-mutation)
state=$(cat "$TMP/gate-mutation-state-path")
if [ "$status" != 0 ] && \
   grep -q 'pinned gate hashes changed after fast-forward; target was not started' "$TMP/gate-mutation.stderr" && \
   [ "$(cat "$state/phase")" = baseline ] && [ "$(cat "$state/reset-umask" 2>/dev/null)" = 0022 ] && \
   ! grep -q '^docker compose build' "$TMP/gate-mutation-events"; then
  pass 'post-merge gate rejection restores the baseline checkout with tracked file modes'
else
  printf 'gate_mutation_status=%s\ngate_mutation_stderr=%s\ngate_mutation_events=%s\n' \
    "$status" "$(cat "$TMP/gate-mutation.stderr")" "$(cat "$TMP/gate-mutation-events")"
  fail 'post-merge gate rejection must restore the baseline checkout under umask 022'
fi

prepare_public_listener_apply_fixture() {
  local name=$1 scenario=$2 port=$3 host_ip=$4 state
  prepare_target_local_apply_fixture "$name" "$scenario" "$APPLY_TARGET_SHA" "$TARGET_SHA" || return 1
  state=$(cat "$TMP/$name-state-path")
  jq -c --arg port "$port" --arg host_ip "$host_ip" \
    '.services.teagramd.ports += [{target:($port | tonumber),published:$port,host_ip:$host_ip,protocol:"tcp",mode:"ingress"}]' \
    "$state/target-compose.json" > "$TMP/$name-target-compose.json" || return 1
  mv -- "$TMP/$name-target-compose.json" "$state/target-compose.json" || return 1
  printf '%s\n' "$scenario" > "$TMP/$name-scenario"
}

prepare_public_listener_apply_fixture target-local-carry-forward-public-listener-addition \
  target-local-carry-forward-public-listener-addition 2446 127.0.0.1
status=$(run_fixture target-local-carry-forward-public-listener-addition built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
root=$(cat "$TMP/target-local-carry-forward-public-listener-addition-root-path")
if [ "$status" = 0 ] && grep -q "rollout=verified sha=$APPLY_TARGET_SHA" "$TMP/target-local-carry-forward-public-listener-addition.stdout" && \
   grep -q $'preflight\tconfig_sha256\t.*\tpass' "$root".target/preflight-comparisons.tsv && \
   grep -q $'preflight\tcompose_public_ws_2446\tfalse\ttrue\tpass' "$root".target/preflight-comparisons.tsv && \
   grep -q $'\tcompose_public_ws_2446\tfalse\ttrue\tpass' "$root".target/target-comparisons.tsv && \
   grep -q $'\texposure_public_ws_2446\tfalse\ttrue\tpass' "$root".target/target-comparisons.tsv && \
   grep -q $'\texposure_sha256\t.*\tpass' "$root".target/target-comparisons.tsv; then
  pass 'only the exact loopback public WebSocket binding is accepted and verified after startup'
else
  show_fixture_failure target-local-carry-forward-public-listener-addition "$status"
  fail 'the exact loopback public WebSocket binding must deploy through apply'
fi

for scenario in target-local-carry-forward-public-listener-wide-host target-local-carry-forward-public-listener-other-port; do
  if [[ "$scenario" == *wide-host ]]; then port=2446; host_ip=0.0.0.0; else port=2450; host_ip=127.0.0.1; fi
  prepare_public_listener_apply_fixture "$scenario" "$scenario" "$port" "$host_ip"
  status=$(run_fixture "$scenario" built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
  root=$(cat "$TMP/$scenario-root-path")
  if [ "$status" != 0 ] && \
     grep -q $'preflight\tconfig_sha256\t.*\tfail' "$root".target/preflight-comparisons.tsv && \
     grep -q 'result=rejected exit=[1-9]' "$root".target/preflight-failure.txt && \
     ! grep -q '^docker compose up -d$' "$TMP/$scenario-events"; then
    pass "$scenario rejects before service replacement"
  else
    show_fixture_failure "$scenario" "$status"
    fail "$scenario must remain unapproved by the exact loopback port exception"
  fi
done

make_fixture public-websocket-defaults public-websocket-defaults
status=$(run_fixture public-websocket-defaults)
root=$(cat "$TMP/public-websocket-defaults-root-path")
if [ "$status" = 0 ] && grep -q $'preflight\tselected_render_config_sha256\t.*\tpass' "$root".target/preflight-comparisons.tsv && \
   grep -q '^docker compose up -d$' "$TMP/public-websocket-defaults-events"; then
  pass 'disabled public WebSocket Compose defaults can be added without live config drift'
else
  printf 'public_websocket_defaults_status=%s\npublic_websocket_defaults_stderr=%s\n' \
    "$status" "$(cat "$TMP/public-websocket-defaults.stderr")"
  cat "$root".target/preflight-comparisons.tsv 2>/dev/null || true
  fail 'disabled public WebSocket default compatibility'
fi

make_fixture public-websocket-enabled public-websocket-enabled
status=$(run_fixture public-websocket-enabled)
root=$(cat "$TMP/public-websocket-enabled-root-path")
if [ "$status" != 0 ] && grep -q 'reason=resolved-config-capture-failed' "$root".target/preflight-failure.txt && \
   ! grep -q '^docker compose up -d' "$TMP/public-websocket-enabled-events" && \
   grep -q 'snapshot verifier: resolved Compose has unsafe public environment settings' "$TMP/public-websocket-enabled.stderr" && \
   grep -q 'resolved Compose preflight capture failed; target was not started' "$TMP/public-websocket-enabled.stderr"; then
  pass 'enabled public WebSocket Compose settings remain rejected before startup'
else
  printf 'public_websocket_enabled_status=%s\npublic_websocket_enabled_stderr=%s\n' \
    "$status" "$(cat "$TMP/public-websocket-enabled.stderr")"
  cat "$root".target/preflight-comparisons.tsv 2>/dev/null || true
  fail 'enabled public WebSocket configuration rejection'
fi

prepare_target_local_apply_fixture welcome-channel-addition \
  target-local-welcome-channel-addition "$E58_LOCAL_COMPOSE_TARGET_SHA"
state=$(cat "$TMP/welcome-channel-addition-state-path")
root=$(cat "$TMP/welcome-channel-addition-root-path")
jq -c '.services.teagramd.environment.TG_WELCOME_CHANNEL=""' \
  "$state/target-compose.json" > "$state/target-compose.next.json"
mv -- "$state/target-compose.next.json" "$state/target-compose.json"
status=$(run_fixture welcome-channel-addition built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
if [ "$status" = 0 ] && \
   grep -q $'preflight\tconfig_sha256\t.*\tpass' "$root".target/preflight-comparisons.tsv && \
   grep -q $'\tconfig_sha256\t.*\tpass' "$root".target/target-comparisons.tsv && \
   grep -q $'\tcontainer_env_sha256\t.*\tpass' "$root".target/target-comparisons.tsv && \
   grep -q "rollout=verified sha=$E58_LOCAL_COMPOSE_TARGET_SHA" "$TMP/welcome-channel-addition.stdout"; then
  pass 'ordinary apply accepts the empty welcome-channel Compose default'
else
  show_fixture_failure welcome-channel-addition "$status"
  fail 'ordinary apply should preserve equivalence when welcome channel is empty'
fi

prepare_target_local_apply_fixture welcome-channel-addition-rollback \
  target-local-welcome-channel-addition-rollback "$E58_LOCAL_COMPOSE_TARGET_SHA"
state=$(cat "$TMP/welcome-channel-addition-rollback-state-path")
root=$(cat "$TMP/welcome-channel-addition-rollback-root-path")
jq -c '.services.teagramd.environment.TG_WELCOME_CHANNEL=""' \
  "$state/target-compose.json" > "$state/target-compose.next.json"
mv -- "$state/target-compose.next.json" "$state/target-compose.json"
status=$(run_fixture welcome-channel-addition-rollback built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
if [ "$status" != 0 ] && \
   grep -q "rollback=verified baseline_sha=$TARGET_SHA" "$TMP/welcome-channel-addition-rollback.stdout" && \
   grep -q $'rollback\tconfig_sha256\t.*\tpass' "$root".rollback/rollback-equivalence.tsv && \
   grep -q $'rollback\tcontainer_env_sha256\t.*\tpass' "$root".rollback/rollback-equivalence.tsv && \
   grep -q 'baseline_equivalence=pass' "$root".rollback/rollback-result.txt; then
  pass 'rollback accepts the empty welcome-channel Compose default'
else
  show_fixture_failure welcome-channel-addition-rollback "$status"
  fail 'rollback should preserve equivalence when welcome channel is empty'
fi

prepare_target_local_apply_fixture syslog-logging-rollout \
  target-local-syslog-logging-rollout "$E58_LOCAL_COMPOSE_TARGET_SHA"
state=$(cat "$TMP/syslog-logging-rollout-state-path")
root=$(cat "$TMP/syslog-logging-rollout-root-path")
jq -c '.services.teagramd.environment.TG_LOG_LOGIN_CODES="true" | del(.services.teagramd.logging)' \
  "$state/base-compose.json" > "$state/base-compose.next.json"
mv -- "$state/base-compose.next.json" "$state/base-compose.json"
jq -c '.services.teagramd.environment.TG_LOG_LOGIN_CODES="false" | .services.teagramd.logging={driver:"syslog",options:{"syslog-address":"unixgram:///dev/log","syslog-facility":"daemon","syslog-format":"rfc5424",tag:"teagramd"}}' \
  "$state/target-compose.json" > "$state/target-compose.next.json"
mv -- "$state/target-compose.next.json" "$state/target-compose.json"
status=$(run_fixture syslog-logging-rollout built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
if [ "$status" = 0 ] && \
   grep -q $'preflight\tconfig_sha256\t.*\tpass' "$root.target/preflight-comparisons.tsv" && \
   grep -q $'preflight\tcompose_log_login_codes\ttrue\tfalse\tpass' "$root.target/preflight-comparisons.tsv" && \
   grep -q $'preflight\tcompose_logging_policy\tdefault\tsyslog-v1\tpass' "$root.target/preflight-comparisons.tsv" && \
   grep -q $'container_env_sha256\t.*\tpass' "$root.target/target-comparisons.tsv" && \
   grep -q $'compose_log_login_codes\ttrue\tfalse\tpass' "$root.target/target-comparisons.tsv" && \
   grep -q $'container_log_login_codes\ttrue\tfalse\tpass' "$root.target/target-comparisons.tsv" && \
   grep -q $'container_logging_sha256\t.*\tpass' "$root.target/target-comparisons.tsv" && \
   grep -q $'compose_logging_policy\tdefault\tsyslog-v1\tpass' "$root.target/target-comparisons.tsv" && \
   grep -q "rollout=verified sha=$E58_LOCAL_COMPOSE_TARGET_SHA" "$TMP/syslog-logging-rollout.stdout"; then
  pass 'complete previous-to-current Compose profile disables login-code logging and verifies retained syslog'
else
  show_fixture_failure syslog-logging-rollout "$status"
  fail 'complete previous-to-current Compose profile should pass preflight and target verification'
fi

prepare_target_local_apply_fixture syslog-logging-unmigrated \
  target-local-syslog-logging-unmigrated "$E58_LOCAL_COMPOSE_TARGET_SHA"
state=$(cat "$TMP/syslog-logging-unmigrated-state-path")
root=$(cat "$TMP/syslog-logging-unmigrated-root-path")
jq -c '.services.teagramd.environment.TG_LOG_LOGIN_CODES="true" | del(.services.teagramd.logging)' \
  "$state/base-compose.json" > "$state/base-compose.next.json"
mv -- "$state/base-compose.next.json" "$state/base-compose.json"
jq -c '.services.teagramd.environment.TG_LOG_LOGIN_CODES="true" | del(.services.teagramd.logging)' \
  "$state/target-compose.json" > "$state/target-compose.next.json"
mv -- "$state/target-compose.next.json" "$state/target-compose.json"
status=$(run_fixture syslog-logging-unmigrated built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
if [ "$status" != 0 ] && \
   grep -q $'preflight\tcompose_log_login_codes\ttrue\ttrue\tfail' "$root.target/preflight-comparisons.tsv" && \
   grep -q $'preflight\tcompose_logging_policy\tdefault\tdefault\tfail' "$root.target/preflight-comparisons.tsv" && \
   ! grep -q '^docker compose up -d$' "$TMP/syslog-logging-unmigrated-events"; then
  pass 'unchanged default JSON-file logging with login codes is rejected before target startup'
else
  show_fixture_failure syslog-logging-unmigrated "$status"
  fail 'default:true to default:true logging must be rejected before target startup'
fi

prepare_target_local_apply_fixture welcome-channel-active \
  target-local-welcome-channel-active "$E58_LOCAL_COMPOSE_TARGET_SHA"
state=$(cat "$TMP/welcome-channel-active-state-path")
jq -c '.services.teagramd.environment.TG_WELCOME_CHANNEL="welcome"' \
  "$state/target-compose.json" > "$state/target-compose.next.json"
mv -- "$state/target-compose.next.json" "$state/target-compose.json"
status=$(run_fixture welcome-channel-active built 0 '' 2 '' '' apply "$APPLY_TARGET_SHA")
if [ "$status" != 0 ] && \
   ! grep -q 'docker compose up -d$' "$TMP/welcome-channel-active-events" && \
   grep -q 'cannot capture preflight snapshot' "$TMP/welcome-channel-active.stderr" && \
   grep -q 'resolved Compose preflight capture failed; target was not started' "$TMP/welcome-channel-active.stderr"; then
  pass 'ordinary apply rejects an active welcome channel before target startup'
else
  show_fixture_failure welcome-channel-active "$status"
  fail 'active welcome channel must be rejected before target startup'
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
   grep -q 'authorized application target is not a reviewed origin/main commit' "$TMP/main-drift.stderr"; then
  pass 'unreviewed application target stops under lock before backup or build'
else
  fail 'reviewed application target guard'
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
     [ "$(cat "$state/teagramd")" = "$BASE_ID" ] && \
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
   [ "$(cat "$state/teagramd")" = "$BASE_ID" ] && \
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
   [ "$(cat "$checkout/cmd/untracked.go")" = 'package teagramd' ] && \
   [ "$(cat "$state/head")" = "$TARGET_SHA" ] && [ "$(cat "$state/teagramd")" = "$BASE_ID" ] && \
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
   [ "$(cat "$state/head")" = "$TARGET_SHA" ] && [ "$(cat "$state/teagramd")" = "$BASE_ID" ] && \
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

make_fixture schema-prefix schema-pre-prefix
status=$(run_fixture schema-prefix)
root=$(cat "$TMP/schema-prefix-root-path")
if [ "$status" = 0 ] && grep -q 'rollout=verified' "$TMP/schema-prefix.stdout" && \
   grep -q 'revision_set=prefix' "$root.target/schema-result-gate-pre.tsv" && \
   grep -q 'revision_set=complete' "$root.target/schema-result-gate-post.tsv"; then
  pass 'pre-deploy gate accepts a complete prefix and post-deploy gate requires the full set'
else
  fail 'pre-deploy prefix and post-deploy complete-set gates'
fi

for schema_failure in schema-post-failed-69 schema-post-missing-69; do
  make_fixture "$schema_failure" "$schema_failure"
  root=$(cat "$TMP/$schema_failure-root-path")
  state=$(cat "$TMP/$schema_failure-state-path")
  status=$(run_fixture "$schema_failure")
  evidence="$root.target/schema-result-gate-post.tsv"
  if [ "$status" != 0 ] && grep -q 'rollback=verified' "$TMP/$schema_failure.stdout" && \
     grep -q '^docker compose up -d --no-build --no-deps teagramd$' "$TMP/$schema_failure-events" && \
     [ "$(cat "$state/head")" = "$BASELINE_SHA" ] && [ "$(cat "$state/teagramd")" = "$ROLLBACK_ID" ] && \
     grep -q 'gate_result=reject' "$evidence"; then
    if [ "$schema_failure" = schema-post-failed-69 ]; then
      grep -q 'revision_20261008000069=partial_or_failed' "$evidence" || { fail 'failed version 69 record was not named'; continue; }
    else
      grep -q 'revision_20261008000069=missing' "$evidence" || { fail 'missing version 69 was not named'; continue; }
    fi
    pass "schema gate rejects $schema_failure and verifies baseline rollback"
  else
    fail "schema gate rejects $schema_failure"
  fi
done

for schema_failure in schema-db-unavailable dirty-migration-inputs; do
  make_fixture "$schema_failure" "$schema_failure"
  root=$(cat "$TMP/$schema_failure-root-path")
  status=$(run_fixture "$schema_failure")
  evidence="$root.target/schema-result-gate-pre.tsv"
  no_target_mutation=1
  grep -q '^docker compose build' "$TMP/$schema_failure-events" && no_target_mutation=0
  grep -q '^docker compose up -d$' "$TMP/$schema_failure-events" && no_target_mutation=0
  if [ "$status" != 0 ] && [ "$no_target_mutation" -eq 1 ] && grep -q 'gate_result=reject' "$evidence"; then
    if [ "$schema_failure" = schema-db-unavailable ]; then
      grep -q 'database_revision_query=unavailable' "$evidence" || { fail 'unavailable database evidence was not named'; continue; }
    else
      grep -q 'migration_inputs_clean=false' "$evidence" || { fail 'dirty migration inputs were not named'; continue; }
    fi
    pass "pre-deploy schema gate stops on $schema_failure"
  else
    fail "pre-deploy schema gate must stop on $schema_failure"
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
     grep -q 'docker compose up -d --no-build --no-deps teagramd' "$TMP/$name-events" && \
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
compose_selection=$(awk -F '\t' '{print $1}' "$TMP/bounded-readiness-compose-selections" | sort -u)
if [ "$status" != 0 ] && grep -q 'result=bounded_timeout max_seconds=1' "$root".target/readiness-target.result && \
   grep -q 'rollback=verified' "$TMP/bounded-readiness.stdout" && \
   [ "$compose_selection" = '.rollout-compose.initial-local.yml:docker-compose.override.yml' ] && \
   grep -q $'\tcompose build -q teagramd' "$TMP/bounded-readiness-compose-selections" && \
   grep -q $'\tcompose up -d' "$TMP/bounded-readiness-compose-selections" && \
   grep -q $'\tcompose up -d --no-build --no-deps teagramd' "$TMP/bounded-readiness-compose-selections"; then
  pass 'pinned local Compose render is reused for preflight, build, startup, readiness, and rollback'
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
if [ -f "$root".baseline/schema-result-gate.py ]; then
  pinned_schema_helper_sha=$(sha256sum "$root".baseline/schema-result-gate.py | awk '{print $1}')
else
  pinned_schema_helper_sha=missing
fi
if [ -f "$root".baseline/qualify-rustfs-transition.py ]; then
  pinned_transition_qualifier_sha=$(sha256sum "$root".baseline/qualify-rustfs-transition.py | awk '{print $1}')
else
  pinned_transition_qualifier_sha=missing
fi
if [ "$status" = 0 ] && grep -q 'rollout=verified' "$TMP/pinned-runtime.stdout" && \
   grep -q 'not the pinned verifier' "$checkout/deploy/teagramd/rollout-runner/rollout-verifier.sh" && \
   grep -q 'not the pinned schema gate helper' "$checkout/deploy/teagramd/rollout-runner/schema-result-gate.py" && \
   grep -q 'schema_gate=pass' "$root".target/target-result.txt && \
   [ -f "$root".baseline/rollout-runner.pinned ] && [ -f "$root".baseline/rollout-verifier.pinned ] && \
   [ -f "$root".baseline/schema-result-gate.pinned ] && [ -f "$root".baseline/schema-result-gate.py ] && \
   [ -f "$root".baseline/blob-mode-state.pinned ] && \
   [ -f "$root".baseline/qualify-rustfs-transition.py ] && \
   [ -f "$root".baseline/stack-move-runner.pinned ] && \
   grep -q "schema_gate_helper_sha256=$pinned_schema_helper_sha" "$root".baseline/runtime-pins.txt && \
   grep -q "transition_qualifier_sha256=$pinned_transition_qualifier_sha" "$root".baseline/runtime-pins.txt; then
  pass 'fast-forward source rewrites cannot replace pinned runner or approved gates in flight'
else
  fail 'pinned runtime survives target checkout mutation'
fi

make_fixture pinned-schema-helper schema-helper-mutated-after-pin
status=$(run_fixture pinned-schema-helper)
root=$(cat "$TMP/pinned-schema-helper-root-path")
if [ "$status" != 0 ] && \
   grep -q 'schema gate helper hash differs from reviewed artifact' "$TMP/pinned-schema-helper.stderr" && \
   ! grep -q '^pinned_runner_started$' "$TMP/pinned-schema-helper-events" && \
   ! grep -Eq '^docker compose (build|up)|^docker compose exec -T postgres pg_dump' "$TMP/pinned-schema-helper-events" && \
   grep -q 'mutated after pin' "$root".baseline/schema-result-gate.py; then
  pass 'mutated pinned schema helper is rejected before the pinned runner starts'
else
  printf 'pinned_schema_helper_status=%s\npinned_schema_helper_stderr=%s\npinned_schema_helper_events=%s\n' \
    "$status" "$(cat "$TMP/pinned-schema-helper.stderr")" "$(cat "$TMP/pinned-schema-helper-events")" >&2
  fail 'mutated pinned schema helper is rejected during initial pin verification'
fi

gate_sha=$(sha256sum "$SCHEMA_GATE" | awk '{print $1}')
gate_helper_sha=$(sha256sum "$SCRIPT_DIR/schema-result-gate.py" | awk '{print $1}')
verifier_sha=$(sha256sum "$VERIFIER" | awk '{print $1}')
if grep -q "readonly APPROVED_VERIFIER_SHA=$verifier_sha" "$SCRIPT_DIR/rollout-runner.sh" && \
   grep -q "readonly APPROVED_SCHEMA_GATE_SHA=$gate_sha" "$SCRIPT_DIR/rollout-runner.sh" && \
   grep -q "readonly APPROVED_SCHEMA_GATE_HELPER_SHA=$gate_helper_sha" "$SCRIPT_DIR/rollout-runner.sh"; then
  pass 'runner consumes the approved verifier, schema gate, and manifest helper hashes'
else
  fail 'approved gate hash pinning'
fi

printf 'fixture_summary=passed:%s failed:%s\n' "$PASS_COUNT" "$FAIL_COUNT"
if [ "$FAIL_COUNT" -ne 0 ]; then
  printf 'fixture_failures=%s\n' "${FAILURES[*]}"
  exit 1
fi
