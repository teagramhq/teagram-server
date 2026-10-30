#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
OP_SCRIPT=$SCRIPT_DIR/ops/apply-user-invite-quota.sh
TMP=$(mktemp -d)
trap 'rm -rf -- "$TMP"' EXIT

mkdir -p "$TMP/bin"
MOCK_UID=$(id -u)

cat > "$TMP/bin/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" == inspect ]]; then
  case "${3:-}" in
    *org.opencontainers.image.revision*) printf '%s\n' "$FIXTURE_SHA" ;;
    *com.docker.compose.project.config_files*)
      printf '%s,%s\n' "$FIXTURE_REPO/docker-compose.yml" "$FIXTURE_REPO/docker-compose.override.yml"
      ;;
    *'{{.State.Status}}'*) printf '%s\n' running ;;
    *'{{.Image}}'*) printf '%s\n' sha256:invite-fixture ;;
    *'range .Config.Env'*)
      inspect_count=0
      if [[ -f "$FIXTURE_INSPECT_COUNT" ]]; then
        IFS= read -r inspect_count < "$FIXTURE_INSPECT_COUNT"
      fi
      inspect_count=$((inspect_count + 1))
      printf '%s\n' "$inspect_count" > "$FIXTURE_INSPECT_COUNT"
      if [[ "$inspect_count" == "${FIXTURE_INSPECT_FAIL_AT:-0}" ]]; then
        echo 'fixture docker environment inspect failed' >&2
        exit 90
      fi
      cat "$FIXTURE_CONTAINER_ENV"
      ;;
    *) echo "unexpected fixture docker inspect: $*" >&2; exit 99 ;;
  esac
elif [[ "$1" == run ]]; then
  case "$*" in
    *'--network host'*'--read-only'*'--cap-drop ALL'*'--security-opt no-new-privileges'*'--user 65532:65532'*'--entrypoint /usr/local/bin/mtproto-smoke'*'--expect-user-invite-quota 5'*) ;;
    *) echo "invite app-config smoke did not use the reviewed restrictions: $*" >&2; exit 98 ;;
  esac
  [[ "${FIXTURE_SMOKE_FAIL:-0}" == 0 ]] || { echo 'fixture smoke failure' >&2; exit 91; }
  if [[ "${FIXTURE_SMOKE_MISMATCH:-0}" == 1 ]]; then
    printf '%s\n' 'help.getAppConfig registration_mode="invite" user_invites_enabled=false user_invite_quota=0; want invite, true, 5'
    exit 0
  fi
  printf '%s\n' 'help.getAppConfig passed on 100.124.236.66:2443 registration_mode=invite user_invites_enabled=true user_invite_quota=5'
else
  echo "unexpected fixture docker call: $*" >&2
  exit 99
fi
DOCKER

cat > "$TMP/bin/python3" <<'PYTHON'
#!/usr/bin/env bash
set -euo pipefail
[[ "${2:-}" == deployment-env ]] || { echo 'unexpected fixture python3 call' >&2; exit 99; }
printf '{"outcome":"s3-accepted","env_files":["%s/.env","%s/.env.rustfs-parked"]}\n' \
  "$FIXTURE_REPO" "$FIXTURE_REPO"
PYTHON

cat > "$TMP/runner-template.sh" <<'RUNNER'
#!/usr/bin/env bash
set -euo pipefail
[[ "$#" == 3 && "$1" == apply && "$2" == "$FIXTURE_SHA" && "$3" == "$FIXTURE_SHA" ]]
[[ "${ROLLOUT_RUNNER_SOURCE_SHA:-}" == "$FIXTURE_SHA" ]]
[[ "${ROLLOUT_RUNNER_ADDED_ENV:-}" == TG_USER_INVITE_QUOTA ]]
[[ "${ROLLOUT_RUNNER_PREVIOUS_ENV_FILE:-}" == "$FIXTURE_REPO/.state/.env.main-2844-before-user-invites" ]]
runner_count=0
if [[ -f "$FIXTURE_RUNNER_CALL_COUNT" ]]; then
  IFS= read -r runner_count < "$FIXTURE_RUNNER_CALL_COUNT"
fi
runner_count=$((runner_count + 1))
printf '%s\n' "$runner_count" > "$FIXTURE_RUNNER_CALL_COUNT"
: > "$FIXTURE_RUNNER_CALLED"
[[ "${FIXTURE_RUNNER_FAIL:-0}" == 0 ]] || exit 42
quota=$(awk -F= '$1 == "TG_USER_INVITE_QUOTA" { print $2 }' "$FIXTURE_REPO/.env")
[[ "$quota" == 5 ]]
printf 'TG_REGISTRATION=invite\nTG_USER_INVITE_QUOTA=%s\n' "$quota" > "$FIXTURE_CONTAINER_ENV"
env_sha=$(sha256sum "$FIXTURE_REPO/.env" | awk '{print $1}')
printf 'env_sha256=%s source_sha=%s\n' "$env_sha" "$FIXTURE_SHA" > "$FIXTURE_REPO/.state/rollout-env"
chmod 600 "$FIXTURE_REPO/.state/rollout-env"
RUNNER

chmod 755 "$TMP/bin/docker" "$TMP/bin/python3"

setup_case() {
  local name=$1 case_dir origin
  case_dir=$TMP/$name
  FIXTURE_REPO=$case_dir/repo
  FIXTURE_ROOT=$case_dir/root
  FIXTURE_CONTAINER_ENV=$case_dir/container-env
  FIXTURE_INSPECT_COUNT=$case_dir/container-inspect-count
  FIXTURE_RUNNER_CALLED=$case_dir/runner-called
  FIXTURE_RUNNER_CALL_COUNT=$case_dir/runner-call-count
  origin=$case_dir/origin.git
  mkdir -p "$FIXTURE_REPO/.state" "$FIXTURE_ROOT/teagramd-rollout-runner"
  chmod 750 "$FIXTURE_REPO/.state"
  printf 'TG_OTHER=stable\n' > "$FIXTURE_REPO/.env"
  printf 'fixture compose\n' > "$FIXTURE_REPO/docker-compose.yml"
  printf 'fixture override\n' > "$FIXTURE_REPO/docker-compose.override.yml"
  printf 'fixture parked environment\n' > "$FIXTURE_REPO/.env.rustfs-parked"
  chmod 600 "$FIXTURE_REPO/.env" "$FIXTURE_REPO/.env.rustfs-parked"
  git init -q -b main "$FIXTURE_REPO"
  git -C "$FIXTURE_REPO" add docker-compose.yml docker-compose.override.yml
  git -C "$FIXTURE_REPO" -c user.name=fixture -c user.email=fixture@example.invalid \
    commit -q -m fixture
  FIXTURE_SHA=$(git -C "$FIXTURE_REPO" rev-parse HEAD)
  git init -q --bare "$origin"
  git -C "$FIXTURE_REPO" remote add origin "$origin"
  git -C "$FIXTURE_REPO" push -q -u origin main
  cp "$TMP/runner-template.sh" "$FIXTURE_ROOT/teagramd-rollout-runner/rollout-runner.sh"
  printf 'fixture helper\n' > "$FIXTURE_ROOT/teagramd-rollout-runner/blob-transition-runner.py"
  chmod 600 "$FIXTURE_ROOT/teagramd-rollout-runner/rollout-runner.sh" \
    "$FIXTURE_ROOT/teagramd-rollout-runner/blob-transition-runner.py"
  printf 'TG_REGISTRATION=invite\n' > "$FIXTURE_CONTAINER_ENV"
  env_sha=$(sha256sum "$FIXTURE_REPO/.env" | awk '{print $1}')
  printf 'env_sha256=%s source_sha=%s\n' "$env_sha" "$FIXTURE_SHA" > "$FIXTURE_REPO/.state/rollout-env"
  chmod 600 "$FIXTURE_REPO/.state/rollout-env"
  sed -e "s|/opt/teagram|$FIXTURE_REPO|g" \
    -e "s|/root|$FIXTURE_ROOT|g" \
    -e "s|EXPECTED_ROOT_UID=0|EXPECTED_ROOT_UID=$MOCK_UID|g" \
    "$OP_SCRIPT" > "$case_dir/op.sh"
  chmod 755 "$case_dir/op.sh"
  export FIXTURE_REPO FIXTURE_ROOT FIXTURE_CONTAINER_ENV FIXTURE_INSPECT_COUNT FIXTURE_RUNNER_CALLED FIXTURE_RUNNER_CALL_COUNT FIXTURE_SHA
}

run_operation() {
  local fail_runner=${1:-0} fail_inspect_at=${2:-0} mode=${3:-} fail_smoke=${4:-0}
  local fail_smoke_mismatch=${5:-0} output status=0
  local -a operation_args=()
  [[ -z "$mode" ]] || operation_args+=("$mode")
  output=$(env OPS_SHA="$FIXTURE_SHA" FIXTURE_RUNNER_FAIL="$fail_runner" \
    FIXTURE_SMOKE_FAIL="$fail_smoke" FIXTURE_SMOKE_MISMATCH="$fail_smoke_mismatch" \
    FIXTURE_INSPECT_FAIL_AT="$fail_inspect_at" FIXTURE_INSPECT_COUNT="$FIXTURE_INSPECT_COUNT" \
    PATH="$TMP/bin:$PATH" bash "$TMP/$(basename "$(dirname "$FIXTURE_REPO")")/op.sh" "${operation_args[@]}" 2>&1) || status=$?
  printf '%s\n' "$status" > "$TMP/operation-status"
  printf '%s\n' "$output"
}

setup_case success
output=$(run_operation)
[[ "$(cat "$TMP/operation-status")" == 0 ]]
[[ "$output" == *'invite_listener=verified quota=5'* ]]
[[ "$output" == *'rollout=verified'* ]]
[[ "$(stat -c '%a' -- "$FIXTURE_REPO/.state")" == 750 ]]
grep -qx 'TG_USER_INVITE_QUOTA=5' "$FIXTURE_REPO/.env"
grep -qx 'TG_USER_INVITE_QUOTA=5' "$FIXTURE_CONTAINER_ENV"
[[ -f "$FIXTURE_REPO/.state/.env.main-2844-before-user-invites" ]]
[[ -f "$FIXTURE_RUNNER_CALLED" ]]
printf '%s\n' 'PASS quota operation records the change and verifies the advertised app config'

setup_case state-dir-group-writable
chmod 770 "$FIXTURE_REPO/.state"
output=$(run_operation)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'deployment state directory failed ownership or mode checks'* ]]
[[ ! -e "$FIXTURE_RUNNER_CALLED" ]]
[[ ! -e "$FIXTURE_REPO/.state/.env.main-2844-before-user-invites" ]]
printf '%s\n' 'PASS writable deployment state directory is rejected before mutation'

setup_case rollback
original_env_sha=$(sha256sum "$FIXTURE_REPO/.env" | awk '{print $1}')
original_record=$(cat "$FIXTURE_REPO/.state/rollout-env")
output=$(run_operation 1)
[[ "$(cat "$TMP/operation-status")" == 42 ]]
[[ "$output" == *'rollback_env=restored'* ]]
[[ "$(sha256sum "$FIXTURE_REPO/.env" | awk '{print $1}')" == "$original_env_sha" ]]
[[ "$(cat "$FIXTURE_REPO/.state/rollout-env")" == "$original_record" ]]
if grep -q '^TG_USER_INVITE_QUOTA=' "$FIXTURE_CONTAINER_ENV"; then
  echo 'runner failure left the invite quota enabled' >&2
  exit 1
fi
printf '%s\n' 'PASS runner failure restores the prior environment and verified record'

setup_case preexisting
printf 'TG_USER_INVITE_QUOTA=5\n' >> "$FIXTURE_REPO/.env"
env_sha=$(sha256sum "$FIXTURE_REPO/.env" | awk '{print $1}')
printf 'env_sha256=%s source_sha=%s\n' "$env_sha" "$FIXTURE_SHA" > "$FIXTURE_REPO/.state/rollout-env"
chmod 600 "$FIXTURE_REPO/.state/rollout-env"
output=$(run_operation)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'user invite quota already exists'* ]]
[[ ! -e "$FIXTURE_RUNNER_CALLED" ]]
printf '%s\n' 'PASS existing quota is rejected before invoking the rollout runner'

setup_case inspect-extra-listener
output=$(run_operation 0 2)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'cannot inspect additional WebSocket listener settings'* ]]
[[ ! -e "$FIXTURE_RUNNER_CALLED" ]]
printf '%s\n' 'PASS failed WebSocket listener inspection rejects the operation'

setup_case inspect-public-websocket-listener
output=$(run_operation 0 3)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'cannot inspect additional WebSocket listener settings'* ]]
[[ ! -e "$FIXTURE_RUNNER_CALLED" ]]
printf '%s\n' 'PASS failed public WebSocket listener inspection rejects the operation'

setup_case inspect-account-age
output=$(run_operation 0 4)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'cannot inspect live account age setting'* ]]
[[ ! -e "$FIXTURE_RUNNER_CALLED" ]]
printf '%s\n' 'PASS failed account-age inspection rejects the operation'

setup_case inspect-existing-quota
output=$(run_operation 0 5)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'cannot inspect live user invite quota'* ]]
[[ ! -e "$FIXTURE_RUNNER_CALLED" ]]
printf '%s\n' 'PASS failed quota inspection rejects the operation'

setup_case extra-websocket-listener
printf 'TG_REGISTRATION=invite\nTG_WEBSOCKET_LISTEN_ADDR=127.0.0.1:2443\n' > "$FIXTURE_CONTAINER_ENV"
output=$(run_operation)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'additional WebSocket listener is active'* ]]
[[ ! -e "$FIXTURE_RUNNER_CALLED" ]]
printf '%s\n' 'PASS active WebSocket listener rejects the operation'

setup_case account-age-override
printf 'TG_REGISTRATION=invite\nTG_USER_INVITE_MIN_ACCOUNT_AGE=14d\n' > "$FIXTURE_CONTAINER_ENV"
output=$(run_operation)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'live account age setting is not using the 7d default'* ]]
[[ ! -e "$FIXTURE_RUNNER_CALLED" ]]
printf '%s\n' 'PASS account-age override rejects the operation'

setup_case post-apply-age-inspect-failure
output=$(run_operation 0 8)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'cannot verify live account age setting after apply'* ]]
[[ -f "$FIXTURE_RUNNER_CALLED" ]]
printf '%s\n' 'PASS failed post-apply account-age inspection rejects success'

setup_case app-config-probe-retry
output=$(run_operation 0 0 '' 1)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'help.getAppConfig did not report user invites enabled'* ]]
grep -qx 'TG_USER_INVITE_QUOTA=5' "$FIXTURE_REPO/.env"
grep -qx 'TG_USER_INVITE_QUOTA=5' "$FIXTURE_CONTAINER_ENV"
applied_env=$(sha256sum "$FIXTURE_REPO/.env" | awk '{print $1}')
applied_record=$(cat "$FIXTURE_REPO/.state/rollout-env")
[[ "$(cat "$FIXTURE_RUNNER_CALL_COUNT")" == 1 ]]

output=$(run_operation 0 0 verify)
[[ "$(cat "$TMP/operation-status")" == 0 ]]
[[ "$output" == *'verification=passed'* ]]
[[ "$(sha256sum "$FIXTURE_REPO/.env" | awk '{print $1}')" == "$applied_env" ]]
[[ "$(cat "$FIXTURE_REPO/.state/rollout-env")" == "$applied_record" ]]
[[ "$(cat "$FIXTURE_RUNNER_CALL_COUNT")" == 1 ]]
printf '%s\n' 'PASS transient app-config failure can be retried with read-only verification'

setup_case app-config-probe-mismatch
output=$(run_operation 0 0 '' 0 1)
[[ "$(cat "$TMP/operation-status")" == 1 ]]
[[ "$output" == *'help.getAppConfig registration_mode="invite" user_invites_enabled=false user_invite_quota=0; want invite, true, 5'* ]]
printf '%s\n' 'PASS app-config mismatch details are printed on probe failure'

setup_case rollback-inspect-failure
output=$(run_operation 1 7)
[[ "$(cat "$TMP/operation-status")" == 42 ]]
[[ "$output" == *'rollback_env=unverified'* ]]
if [[ "$output" == *'rollback_env=restored'* ]]; then
  echo 'failed container inspection falsely reported an environment rollback' >&2
  exit 1
fi
grep -qx 'TG_USER_INVITE_QUOTA=5' "$FIXTURE_REPO/.env"
printf '%s\n' 'PASS failed post-run inspection does not claim rollback verification'
