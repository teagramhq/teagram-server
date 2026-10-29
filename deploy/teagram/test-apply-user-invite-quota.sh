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
    *'range .Config.Env'*) cat "$FIXTURE_CONTAINER_ENV" ;;
    *) echo "unexpected fixture docker inspect: $*" >&2; exit 99 ;;
  esac
elif [[ "$1" == run ]]; then
  case "$*" in
    *'--network host'*'--read-only'*'--cap-drop ALL'*'--security-opt no-new-privileges'*'--user 65532:65532'*'--entrypoint /usr/local/bin/mtproto-smoke'*'--expect-user-invite-quota 5'*) ;;
    *) echo "invite app-config smoke did not use the reviewed restrictions: $*" >&2; exit 98 ;;
  esac
  [[ "${FIXTURE_SMOKE_FAIL:-0}" == 0 ]] || { echo 'fixture smoke failure' >&2; exit 91; }
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
  FIXTURE_RUNNER_CALLED=$case_dir/runner-called
  origin=$case_dir/origin.git
  mkdir -p "$FIXTURE_REPO/.state" "$FIXTURE_ROOT/teagramd-rollout-runner"
  chmod 700 "$FIXTURE_REPO/.state"
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
  export FIXTURE_REPO FIXTURE_ROOT FIXTURE_CONTAINER_ENV FIXTURE_RUNNER_CALLED FIXTURE_SHA
}

run_operation() {
  local fail_runner=${1:-0} output status=0
  output=$(env OPS_SHA="$FIXTURE_SHA" FIXTURE_RUNNER_FAIL="$fail_runner" \
    PATH="$TMP/bin:$PATH" bash "$TMP/$(basename "$(dirname "$FIXTURE_REPO")")/op.sh" 2>&1) || status=$?
  printf '%s\n' "$status" > "$TMP/operation-status"
  printf '%s\n' "$output"
}

setup_case success
output=$(run_operation)
[[ "$(cat "$TMP/operation-status")" == 0 ]]
[[ "$output" == *'invite_listener=verified quota=5'* ]]
[[ "$output" == *'rollout=verified'* ]]
grep -qx 'TG_USER_INVITE_QUOTA=5' "$FIXTURE_REPO/.env"
grep -qx 'TG_USER_INVITE_QUOTA=5' "$FIXTURE_CONTAINER_ENV"
[[ -f "$FIXTURE_REPO/.state/.env.main-2844-before-user-invites" ]]
[[ -f "$FIXTURE_RUNNER_CALLED" ]]
printf '%s\n' 'PASS quota operation records the change and verifies the advertised app config'

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
