#!/usr/bin/env bash
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/teagram-ops.sh"
ops_dir="$(cd "$(dirname "$script")" && pwd)/ops"
failure_report_script="$(cd "$(dirname "$0")" && pwd)/ops/target-failure-report.sh"
evidence_script="$(cd "$(dirname "$0")" && pwd)/ops/rollout-evidence.sh"
tmp=$(mktemp -d)
cleanup() {
  /bin/rm -rf -- "$tmp"
}
trap cleanup EXIT

mkdir -p "$tmp/bin" "$tmp/repo/.state" "$tmp/root"
chmod 700 "$tmp/root"
MOCK_OWNER_UID=$(id -u)
export MOCK_OWNER_UID
export MOCK_OPS_DIR="$ops_dir"
printf 'TG_RSA_KEY_FINGERPRINT=fixture-value-never-print\nFIXTURE_EMPTY=\n' > "$tmp/repo/.env"
printf 'override fixture\n' > "$tmp/repo/docker-compose.override.yml"
printf 'state fixture\n' > "$tmp/repo/.state/rollout-env"
printf 'compose fixture\n' > "$tmp/repo/docker-compose.yml"
printf 'local blobs fixture\n' > "$tmp/repo/docker-compose.local-blobs.yml"
printf 'fixture env only\n' > "$tmp/repo/.env.rustfs-parked"

OPS_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
TARGET_SHA=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
OFF_MAIN_SHA=cccccccccccccccccccccccccccccccccccccccc
OLD_MAIN_SHA=ffffffffffffffffffffffffffffffffffffffff
CONTAINER_ID=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
REPORT_TARGET_SHA=abcdef1234567890abcdef1234567890abcdef12
REPORT_SOURCE_SHA=1234567890abcdef1234567890abcdef12345678
REPORT_PREFIX=${REPORT_TARGET_SHA:0:12}
export OPS_SHA TARGET_SHA OFF_MAIN_SHA OLD_MAIN_SHA CONTAINER_ID \
  REPORT_TARGET_SHA REPORT_SOURCE_SHA REPORT_PREFIX
export MOCK_CURRENT_SHA="$TARGET_SHA" MOCK_REPO="$tmp/repo"
export MOCK_REPORT_SCRIPT="$failure_report_script"
export MOCK_EVIDENCE_SCRIPT="$evidence_script"
export MOCK_INVITE_SCRIPT="$tmp/invite-web-signup-url.sh"
export MOCK_RUN_TEMP_LOG="$tmp/run-temp.log" MOCK_RM_LOG="$tmp/rm.log"
export MOCK_SHOW_LOG="$tmp/show.log"
export MOCK_ROOT="$tmp/root"
export MOCK_RETIRED_SCRIPT_MARKER="$tmp/retired-operation-ran"

cat > "$tmp/bin/git" <<'GIT'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == -C ]] || { echo "unexpected git call: $*" >&2; exit 99; }
shift 2
case "$1" in
  rev-parse) [[ "${2:-}" == HEAD ]] && printf '%s\n' "$MOCK_CURRENT_SHA" ;;
  branch) [[ "${2:-}" == --show-current ]] && printf '%s\n' main ;;
  status) [[ "${2:-}" == --porcelain ]] && printf ' M fixture\n' ;;
  fetch) exit 0 ;;
  merge-base)
    if [[ "${2:-}" == --is-ancestor && "${4:-}" == origin/main && \
      ( "${3:-}" == "$OPS_SHA" || "${3:-}" == "$TARGET_SHA" || \
        "${3:-}" == "$OLD_MAIN_SHA" ) ]]; then
      exit 0
    fi
    exit 1
    ;;
  cat-file)
    [[ "${2:-}" == -e ]] || exit 99
    case "${3:-}" in
      "$OPS_SHA:deploy/teagram/ops/"*.sh)
        [[ -f "$MOCK_OPS_DIR/${3##*/}" ]]
        ;;
      "$OLD_MAIN_SHA:deploy/teagram/ops/preflight-replay.sh"|\
      "$OLD_MAIN_SHA:deploy/teagram/ops/replay-check.sh") ;;
      *) exit 1 ;;
    esac
    ;;
  show)
    if [[ "${MOCK_STOP_AT_SHOW:-}" == true ]]; then
      expected="$OPS_SHA:deploy/teagram/ops/$MOCK_ALLOWED_NAME.sh"
      [[ "${2:-}" == "$expected" ]] || { echo "unexpected git show: $*" >&2; exit 99; }
      printf '%s\n' "$2" >> "$MOCK_SHOW_LOG"
      exit 67
    fi
    case "${2:-}" in
      "$OPS_SHA:deploy/teagram/ops/target-failure-report.sh")
        sed -e "s|/root|$MOCK_ROOT|g" "$MOCK_REPORT_SCRIPT"
        ;;
      "$OPS_SHA:deploy/teagram/ops/rollout-evidence.sh")
        sed -e "s|/root|$MOCK_ROOT|g" \
          -e "s|0:700|$MOCK_OWNER_UID:700|g" -e "s|0:600|$MOCK_OWNER_UID:600|g" "$MOCK_EVIDENCE_SCRIPT"
        ;;
      "$OPS_SHA:deploy/teagram/ops/invite-web-signup-url.sh")
        cat "$MOCK_INVITE_SCRIPT"
        ;;
      "$OLD_MAIN_SHA:deploy/teagram/ops/preflight-replay.sh"|\
      "$OLD_MAIN_SHA:deploy/teagram/ops/replay-check.sh")
        printf '#!/bin/sh\ntouch "%s"\n' "$MOCK_RETIRED_SCRIPT_MARKER"
        ;;
      *) exit 1 ;;
    esac
    ;;
  *) echo "unexpected git call: $*" >&2; exit 99 ;;
esac
GIT

cat > "$tmp/bin/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  'ps '*) printf '%s\n' 'teagram-teagramd-1 image:fixture Up 1 minute' ;;
  *'{{.Id}}'*) printf '%s\n' "$CONTAINER_ID" ;;
  *'{{.Image}}'*) printf '%s\n' 'sha256:fixture' ;;
  *'{{.State.StartedAt}}'*) printf '%s\n' '2026-10-26T00:00:00Z' ;;
  *'org.opencontainers.image.revision'*) printf '%s\n' "$TARGET_SHA" ;;
  *'com.docker.compose.project.config_files'*)
    printf '%s,%s\n' "$MOCK_REPO/docker-compose.yml" "$MOCK_REPO/docker-compose.override.yml"
    ;;
  *) echo "unexpected docker call: $*" >&2; exit 99 ;;
esac
DOCKER

cat > "$tmp/bin/logger" <<'LOGGER'
#!/bin/sh
exit 0
LOGGER
cat > "$tmp/bin/hostname" <<'HOSTNAME'
#!/bin/sh
printf '%s\n' fixture-host
HOSTNAME
cat > "$tmp/bin/mktemp" <<'MKTEMP'
#!/bin/sh
for arg do
  if [ "$arg" = /tmp/teagram-ops.XXXXXX ]; then
    path=$(/usr/bin/mktemp "$@")
    printf '%s\n' "$path" >> "$MOCK_RUN_TEMP_LOG"
    printf '%s\n' "$path"
    exit 0
  fi
done
exec /usr/bin/mktemp "$@"
MKTEMP
cat > "$tmp/bin/rm" <<'RM'
#!/bin/sh
for arg do
  case "$arg" in
    /tmp/teagram-ops.*) printf '%s\n' "$arg" >> "$MOCK_RM_LOG" ;;
  esac
done
exec /bin/rm "$@"
RM
cat > "$tmp/bin/teagram-logs" <<'LOGS'
#!/bin/sh
printf 'log_reader=%s\n' "${SSH_ORIGINAL_COMMAND:-}"
LOGS
chmod 755 "$tmp/bin/"*

cat > "$MOCK_INVITE_SCRIPT" <<'INVITE'
#!/usr/bin/env bash
set -euo pipefail
printf 'invite-operation args:'
printf ' <%s>' "$@"
printf '\n'
INVITE

sed \
  -e "s|/opt/teagram|$tmp/repo|g" \
  -e "s|0:700|$MOCK_OWNER_UID:700|g" \
  -e "s|0:600|$MOCK_OWNER_UID:600|g" \
  -e "s|/usr/local/sbin/teagram-logs|$tmp/bin/teagram-logs|g" \
  "$script" > "$tmp/teagram-ops.sh"
chmod 755 "$tmp/teagram-ops.sh"
export PATH="$tmp/bin:$PATH"

run_command() {
  SSH_ORIGINAL_COMMAND=$1 bash "$tmp/teagram-ops.sh"
}
expect_status() {
  local expected=$1 command=$2 output status=0
  output=$(run_command "$command" 2>&1) || status=$?
  if [[ "$status" != "$expected" ]]; then
    printf 'FAIL command %q: expected exit %s, got %s\n%s\n' "$command" "$expected" "$status" "$output" >&2
    return 1
  fi
}

[[ "$(run_command hostname)" == fixture-host ]]
live=$(run_command live)
[[ "$live" == *"live_sha=$TARGET_SHA"* && "$live" == *'dirty_paths=1'* && "$live" == *'container_id='* ]]
[[ "$(run_command ps)" == *'teagram-teagramd-1 image:fixture Up 1 minute'* ]]
[[ "$(run_command 'logs 2026-10-26T00:00:00Z')" == 'log_reader=2026-10-26T00:00:00Z' ]]
expect_status 64 'logs'
expect_status 64 'logs now extra'

env_output=$(run_command env-names)
[[ "$env_output" == $'FIXTURE_EMPTY\nTG_RSA_KEY_FINGERPRINT' ]]
[[ "$env_output" != *fixture-value-never-print* ]]
fingerprints=$(run_command fingerprints)
[[ "$(printf '%s\n' "$fingerprints" | wc -l | tr -d ' ')" == 5 ]]
[[ "$fingerprints" != *fixture-value-never-print* ]]
for help in help -h --help; do
  help_output=$(run_command "$help" 2>&1)
  [[ "$help_output" == *'hostname'* && "$help_output" == *'env-names'* && "$help_output" == *'run'* ]]
  [[ "$help_output" == *'target-failure-report|rollout-evidence|invite-web-signup-url|rollback-public-websocket|apply-user-invite-quota'* ]]
  [[ "$help_output" == *'apply-user-invite-quota default and'* && \
    "$help_output" == *'rollback-public-websocket mutate live state'* && \
    "$help_output" == *'apply-user-invite-quota verify is read-only'* ]]
  [[ "$help_output" == *'invite-web-signup-url default (apply) and --rollback'* ]]
  [[ "$help_output" == *'mutate the live .env; --check is read-only'* ]]
done

active_operation_count=0
for operation_path in "$ops_dir"/*.sh; do
  operation_name=${operation_path##*/}
  operation_name=${operation_name%.sh}
  active_operation_count=$((active_operation_count + 1))
  export MOCK_STOP_AT_SHOW=true MOCK_ALLOWED_NAME="$operation_name"
  expect_status 67 "run $OPS_SHA $operation_name"
  [[ "$(tail -n 1 "$MOCK_SHOW_LOG")" == "$OPS_SHA:deploy/teagram/ops/$operation_name.sh" ]]
done
unset MOCK_STOP_AT_SHOW MOCK_ALLOWED_NAME

report_dir="$MOCK_ROOT/main1238-${REPORT_PREFIX}-20261027T124539Z.target"
mkdir -m 700 "$report_dir"
printf 'captured_at_utc=2026-10-27T12:45:39Z target_sha=%s source_sha=%s reason=target_readiness_rejected_exit_1 target_snapshot=%064d\n' \
  "$REPORT_TARGET_SHA" "$REPORT_SOURCE_SHA" 0 > "$report_dir/target-failure.txt"
printf 'role=target result=failed exit=1 max_seconds=120\n' > "$report_dir/readiness-target.result"
chmod 600 "$report_dir/target-failure.txt" "$report_dir/readiness-target.result"
report_output=$(run_command "run $OPS_SHA target-failure-report $REPORT_PREFIX")
[[ "$report_output" == *"target_sha=$REPORT_TARGET_SHA"* && \
  "$report_output" == *'target_failure_gate=target_readiness_rejected_exit_1'* && \
  "$report_output" == *'readiness_result=failed'* ]]

invite_check=$(run_command "run $OPS_SHA invite-web-signup-url --check")
[[ "$invite_check" == 'invite-operation args: <--check>' ]]
invite_apply=$(run_command "run $OPS_SHA invite-web-signup-url")
[[ "$invite_apply" == 'invite-operation args: <>' ]]
invite_rollback=$(run_command "run $OPS_SHA invite-web-signup-url --rollback")
[[ "$invite_rollback" == 'invite-operation args: <--rollback>' ]]

sentinel="$tmp/arbitrary-command-ran"
expect_status 64 "touch $sentinel"
[[ ! -e "$sentinel" ]]
expect_status 64 'run'
expect_status 64 "run gggggggggggggggggggggggggggggggggggggggg rollout-evidence"
expect_status 64 "run ${OPS_SHA:0:7} rollout-evidence"
expect_status 65 "run $OFF_MAIN_SHA rollout-evidence"
expect_status 64 "run $OPS_SHA missing-script $TARGET_SHA"
expect_status 66 "run $OLD_MAIN_SHA rollout-evidence"
expect_status 64 "run $OLD_MAIN_SHA preflight-replay"
expect_status 64 "run $OLD_MAIN_SHA replay-check"
[[ ! -e "$MOCK_RETIRED_SCRIPT_MARKER" ]]
for bad_name in 'nested/name' . .. 'Uppercase'; do
  expect_status 64 "run $OPS_SHA $bad_name $TARGET_SHA"
done

chmod 700 "$MOCK_ROOT"
old_target_dir="$MOCK_ROOT/main1238-${OFF_MAIN_SHA:0:12}-20261027T230000Z.target"
latest_target_dir="$MOCK_ROOT/main1238-${TARGET_SHA:0:12}-20261028T040000Z.target"
mkdir -m 700 "$old_target_dir" "$latest_target_dir"
printf 'captured_at_utc=2026-10-27T23:00:10Z target_sha=%s source_sha=%s reason=target_readiness_rejected_exit_2 target_snapshot=unavailable\n' \
  "$OFF_MAIN_SHA" "$OPS_SHA" > "$old_target_dir/target-failure.txt"
printf 'captured_at_utc=2026-10-28T04:03:00Z target_sha=%s source_sha=%s reason=target_readiness_rejected_exit_1 target_snapshot=0123456789abcdef\n' \
  "$TARGET_SHA" "$OFF_MAIN_SHA" > "$latest_target_dir/target-failure.txt"
printf '2026-10-28T04:02:57Z\trole=target\telapsed=0\tcontainer_id=%s\tstate=running\texit=0\tadvertise=1\terrors=0\tmigrate=exited/0\tpostgres=healthy\ttcp=failed\ttls=failed\ttls_http=000\ttls_verify=unknown\n' \
  "$CONTAINER_ID" > "$latest_target_dir/readiness-target.tsv"
chmod 600 "$old_target_dir/target-failure.txt" "$latest_target_dir/target-failure.txt" \
  "$latest_target_dir/readiness-target.tsv"
latest_log="$MOCK_ROOT/rollout-${TARGET_SHA:0:7}-20261028T035454Z.log"
later_log="$MOCK_ROOT/rollout-${TARGET_SHA:0:7}-20261028T040100Z.log"
printf 'oldest runner line\n' > "$latest_log"
for line_number in {1..85}; do
  printf 'runner line %02d\n' "$line_number" >> "$latest_log"
done
{
  printf 'TG_DB_PASSWORD=secret-runner-password\n'
  printf '{"client_key":"secret-json-key"}\n'
  printf 'Authorization: Bearer secret-runner-token\n'
  printf 'remote=postgres://runner:secret-url-password@db/teagram\n'
  printf 'request=Bearer secret-bearer-token\n'
  printf 'TG_DB_PASS=env-value-with-no-blocklist-needle\n'
  printf '%s\n' '-----BEGIN PRIVATE KEY-----'
  printf '%s\n' 'MIIEowIBAAKCAQEArandompemfixture'
  printf 'compose stderr: services.teagram.environment.TG_DB_PASS=compose-value-no-blocklist-needle\n'
  printf 'target_failure_gate=target_readiness_rejected_exit_1 rollback_exit=0\n'
  printf 'target_failure_gate=not_a_runner_gate rollback_exit=0\n'
  printf 'rollback=verified baseline_sha=%s container_id=%s\n' "$OFF_MAIN_SHA" "$CONTAINER_ID"
} >> "$latest_log"
printf 'later runner log must not be selected\n' > "$later_log"
chmod 600 "$latest_log" "$later_log"
output=$(run_command "run $OPS_SHA rollout-evidence")
[[ "$output" == *"rollout_failure target_sha=$TARGET_SHA source_sha=$OFF_MAIN_SHA failed_gate=target_readiness_rejected_exit_1"* ]]
[[ "$output" == *'evidence_captured_at_utc=2026-10-28T04:03:00Z'* ]]
[[ "$output" == *'tcp=failed'* && "$output" == *'tls=failed'* && "$output" != *"$CONTAINER_ID"* ]]
[[ "$output" == *"runner_log=rollout-${TARGET_SHA:0:7}-20261028T035454Z.log"* ]]
[[ "$output" == *'target_failure_gate=target_readiness_rejected_exit_1 rollback_exit=0'* ]]
[[ "$output" == *"rollback=verified baseline_sha=$OFF_MAIN_SHA container_id=[REDACTED]"* ]]
[[ "$output" != *'target_failure_gate=not_a_runner_gate'* ]]
[[ "$output" != *'runner line 85'* && "$output" != *'oldest runner line'* ]]
[[ "$output" != *'later runner log must not be selected'* ]]
[[ "$output" != *'secret-runner-password'* && "$output" != *'secret-runner-token'* && \
   "$output" != *'secret-url-password'* && "$output" != *'secret-bearer-token'* && \
   "$output" != *'secret-json-key'* && "$output" != *'env-value-with-no-blocklist-needle'* && \
   "$output" != *'compose-value-no-blocklist-needle'* && "$output" != *'MIIEowIBAAKCAQEArandompemfixture'* && \
   "$output" != *'-----BEGIN PRIVATE KEY-----'* && "$output" != *'TG_DB_PASS='* ]]

WARNING_SHA=eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee
warning_target_dir="$MOCK_ROOT/main1238-${WARNING_SHA:0:12}-20261028T043000Z.target"
warning_rollback_dir="${warning_target_dir%.target}.rollback"
mkdir -m 700 "$warning_target_dir" "$warning_rollback_dir"
printf 'target_sha=%s reason=target_readiness_rejected_exit_3 target_failure_marker=unavailable\n' \
  "$WARNING_SHA" > "$warning_rollback_dir/target-failure-marker-warning.txt"
warning_log="$MOCK_ROOT/rollout-${WARNING_SHA:0:7}-20261028T042958Z.log"
printf 'latest warning-backed rollout log\n' > "$warning_log"
chmod 600 "$warning_rollback_dir/target-failure-marker-warning.txt" "$warning_log"
output=$(run_command "run $OPS_SHA rollout-evidence")
[[ "$output" == *"rollout_failure target_sha=$WARNING_SHA source_sha=unavailable failed_gate=target_readiness_rejected_exit_3 target_failure_marker=unavailable"* ]]
[[ "$output" == *'failure_marker_warning=target-failure-marker-warning.txt'* ]]
[[ "$output" == *'evidence_captured_at_utc=unavailable'* && \
   "$output" == *'rollout_started_at_utc=2026-10-28T04:30:00Z'* ]]
[[ "$output" == *"runner_log=rollout-${WARNING_SHA:0:7}-20261028T042958Z.log"* && \
   "$output" != *"runner_log=rollout-${TARGET_SHA:0:7}-20261028T035454Z.log"* ]]
expect_status 64 "run $OPS_SHA rollout-evidence extra"

mapfile -t runner_temps < "$MOCK_RUN_TEMP_LOG"
mapfile -t removed_temps < "$MOCK_RM_LOG"
expected_runner_temps=$((7 + active_operation_count))
[[ "${#runner_temps[@]}" == "$expected_runner_temps" && "${#removed_temps[@]}" == "${#runner_temps[@]}" ]]
for index in "${!runner_temps[@]}"; do
  runner_temp=${runner_temps[$index]}
  [[ "$runner_temp" == /tmp/teagram-ops.* && "${removed_temps[$index]}" == "$runner_temp" && ! -e "$runner_temp" ]]
done
printf '%s\n' 'teagram-ops fixture cases pass'
