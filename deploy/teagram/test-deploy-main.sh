#!/usr/bin/env bash
# Fixture test for deploy-main.sh's target checks: a fake origin and live
# checkout, and a fake docker that reports an approved local compose and image id.
set -euo pipefail
umask 077

script="$(cd "$(dirname "$0")" && pwd)/deploy-main.sh"
logs_script="$(cd "$(dirname "$0")" && pwd)/teagram-logs.sh"
ops_script="$(cd "$(dirname "$0")" && pwd)/teagram-ops.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

export GIT_AUTHOR_NAME=fixture GIT_AUTHOR_EMAIL=fixture@example.invalid
export GIT_COMMITTER_NAME=fixture GIT_COMMITTER_EMAIL=fixture@example.invalid
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

# The LXC forced command still loads this path. Keep it able to forward to the
# canonical deploy script after the repo's directory rename.
legacy_script="$(cd "$(dirname "$script")/.." && pwd)/telegram-server/deploy-main.sh"
bridge_repo="$tmp/bridge"
bridge_origin="$tmp/bridge-origin.git"
git init -q --bare "$bridge_origin"
git init -q -b main "$bridge_repo"
git -C "$bridge_repo" remote add origin "$bridge_origin"
mkdir -p "$bridge_repo/deploy/teagram"
cat > "$bridge_repo/deploy/teagram/deploy-main.sh" <<'TARGET'
#!/usr/bin/env bash
printf 'compatibility target %s\n' "${DEPLOY_SHA:?}"
TARGET
git -C "$bridge_repo" add deploy/teagram/deploy-main.sh
git -C "$bridge_repo" commit -q -m target
bridge_sha=$(git -C "$bridge_repo" rev-parse HEAD)
git -C "$bridge_repo" push -q origin HEAD:main
git -C "$bridge_repo" switch -q -c feature
cat > "$bridge_repo/deploy/teagram/deploy-main.sh" <<'UNTRUSTED'
#!/usr/bin/env bash
printf 'untrusted deploy script executed\n' > "$BRIDGE_EXEC_MARKER"
UNTRUSTED
git -C "$bridge_repo" add deploy/teagram/deploy-main.sh
git -C "$bridge_repo" commit -q -m untrusted
bridge_feature_sha=$(git -C "$bridge_repo" rev-parse HEAD)
git -C "$bridge_repo" push -q origin HEAD:feature
git -C "$bridge_repo" switch -q main
git -C "$bridge_repo" branch -D feature >/dev/null
git -C "$bridge_repo" fetch -q origin feature
bridge_output=$(cd "$bridge_repo" && DEPLOY_SHA="$bridge_sha" bash "$legacy_script")
if [[ "$bridge_output" != "compatibility target $bridge_sha" ]]; then
  printf 'FAIL legacy deploy entrypoint selected the wrong target: %s\n' "$bridge_output" >&2
  exit 1
fi
if (cd "$bridge_repo" && DEPLOY_SHA="$bridge_feature_sha" BRIDGE_EXEC_MARKER="$tmp/bridge-executed" bash "$legacy_script") >"$tmp/bridge-feature.log" 2>&1; then
  echo 'FAIL legacy deploy entrypoint accepted a non-main SHA' >&2
  exit 1
fi
if [[ -e "$tmp/bridge-executed" ]]; then
  echo 'FAIL legacy deploy entrypoint executed a non-main script' >&2
  exit 1
fi
grep -Fq "$bridge_feature_sha is not on origin/main; refusing" "$tmp/bridge-feature.log"
if (cd "$bridge_repo" && DEPLOY_SHA=invalid bash "$legacy_script") >"$tmp/bridge-invalid.log" 2>&1; then
  echo 'FAIL legacy deploy entrypoint accepted an invalid SHA' >&2
  exit 1
fi
grep -Fq 'DEPLOY_SHA must be a full 40-hex commit SHA' "$tmp/bridge-invalid.log"

git init -q -b main "$tmp/work"
mkdir -p "$tmp/work/deploy/teagramd/rollout-runner" "$tmp/work/deploy/link-edge"
printf 'tracked fixture\n' > "$tmp/work/tracked.txt"
printf '#!/usr/bin/env bash\n' > "$tmp/work/tracked-script.sh"
cat > "$tmp/work/deploy/teagramd/rollout-runner/rollout-runner.sh" <<'RUNNER'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${FIXTURE_EXPECT_S3_ENV:-0}" == 1 && \
    "${COMPOSE_ENV_FILES:-}" != "$DEPLOY_DIR/.env,$FIXTURE_PARKED_ENV" ]]; then
  echo "S3 rollout did not inherit the validated environment files" >&2
  exit 96
fi
runtime_dir="$DEPLOY_ROOT/teagramd-rollout-runner"
for file in rollout-runner.sh rollout-verifier.sh schema-result-gate.sh schema-result-gate.py blob-mode-state.py blob-transition-runner.py qualify-rustfs-transition.py stack-move-runner.py; do
  if ! git -C "$DEPLOY_DIR" show "$2:deploy/teagramd/rollout-runner/$file" | cmp - "$runtime_dir/$file"; then
    printf 'staged runtime is missing or stale: %s\n' "$file" >&2
    exit 90
  fi
done
git -C "$DEPLOY_DIR" merge --ff-only -q "$2"
image_id=${FIXTURE_POST_IMAGE_ID:-sha256:fixture}
printf 'source_sha=%s image_id=%s built_at=fixture\n' "$2" "$image_id" \
  > "$DEPLOY_ROOT/main1238-fixture.build/built-image.identity"
printf '%s\n' "$image_id" > "${DEPLOY_IMAGE_ID_FILE:?}"
RUNNER
cat > "$tmp/work/deploy/link-edge/deploy-main.sh" <<'EDGE'
#!/usr/bin/env bash
set -euo pipefail
[[ $# == 1 ]]
[[ ${TG_INVITE_WEB_SIGNUP_URL:-} == https://web.example.test/signup ]]
[[ ${TG_INVITE_DESKTOP_URL:-} == '' ]]
printf '%s\n' "$1" >> "${LINK_EDGE_DEPLOY_LOG:?}"
printf 'link-edge\n' >> "${DEPLOY_ACTION_LOG:?}"
EDGE
for file in rollout-verifier.sh schema-result-gate.sh schema-result-gate.py blob-mode-state.py qualify-rustfs-transition.py stack-move-runner.py; do
  printf 'fixture\n' > "$tmp/work/deploy/teagramd/rollout-runner/$file"
done
cat > "$tmp/work/deploy/teagramd/rollout-runner/blob-transition-runner.py" <<'PYTHON'
#!/usr/bin/env python3
import json
import os
import sys

if sys.argv[1:] and sys.argv[1] == "deployment-env":
    outcome = os.environ.get("FIXTURE_AUTHORITY_OUTCOME", "initial-local")
    if outcome != "s3-accepted":
        print("blob transition rejected: deploy-s3-authority-invalid", file=sys.stderr)
        raise SystemExit(1)
    print(json.dumps({
        "outcome": outcome,
        "env_files": [os.path.join(os.environ["DEPLOY_DIR"], ".env"), os.environ["FIXTURE_PARKED_ENV"]],
    }, separators=(",", ":")))
    raise SystemExit(0)
raise SystemExit(2)
PYTHON
chmod 644 "$tmp/work/tracked.txt"
chmod 755 "$tmp/work/tracked-script.sh"
chmod 644 "$tmp/work/deploy/teagramd/rollout-runner/"*
git -C "$tmp/work" add tracked.txt tracked-script.sh deploy/teagramd/rollout-runner deploy/link-edge/deploy-main.sh
commit() {
  git -C "$tmp/work" commit -q --allow-empty -m "$1"
  git -C "$tmp/work" rev-parse HEAD
}
first=$(commit first)
mkdir -p "$tmp/work/deploy/teagram"
cp "$logs_script" "$tmp/work/deploy/teagram/teagram-logs.sh"
chmod 755 "$tmp/work/deploy/teagram/teagram-logs.sh"
cp "$ops_script" "$tmp/work/deploy/teagram/teagram-ops.sh"
chmod 755 "$tmp/work/deploy/teagram/teagram-ops.sh"
printf 'SYSLOGD_OPTS="-t -s 524288 -b 15"\n' > "$tmp/work/deploy/teagram/syslog.conf"
git -C "$tmp/work" add deploy/teagram/teagram-logs.sh deploy/teagram/teagram-ops.sh deploy/teagram/syslog.conf
second=$(commit second)
git -C "$tmp/work" checkout -q -b side "$first"
side=$(commit side)
git -C "$tmp/work" checkout -q main
git clone -q --bare "$tmp/work" "$tmp/origin.git"
git clone -q "$tmp/origin.git" "$tmp/live"
git -C "$tmp/work" push -q "$tmp/origin.git" main
printf 'third revision\n' > "$tmp/work/tracked.txt"
printf 'new tracked fixture\n' > "$tmp/work/new-tracked.txt"
chmod 644 "$tmp/work/new-tracked.txt"
git -C "$tmp/work" add new-tracked.txt
third=$(commit third)
git -C "$tmp/work" push -q "$tmp/origin.git" main
git -C "$tmp/live" reset -q --hard "$second"
printf 'TG_RSA_KEY_FINGERPRINT=12345\nTG_INVITE_WEB_SIGNUP_URL=https://web.example.test/signup\n' > "$tmp/live/.env"
printf 'fixture dump\n' > "$tmp/live/pgdump-fixture.sql"
printf 'fixture evidence\n' > "$tmp/live/rollout-evidence.txt"
chmod 640 "$tmp/live/.env"
chmod 600 "$tmp/live/pgdump-fixture.sql" "$tmp/live/rollout-evidence.txt"
local_files_before=$(stat -c '%n:%u:%g:%a' \
  "$tmp/live/.env" "$tmp/live/pgdump-fixture.sql" "$tmp/live/rollout-evidence.txt")

mkdir -p "$tmp/bin" "$tmp/root/main1238-fixture.build" "$tmp/root/usr/local/sbin"
mkdir -p "$tmp/root/etc/conf.d"
printf 'SYSLOGD_OPTS="-t"\n' > "$tmp/root/etc/conf.d/syslog"
chmod 644 "$tmp/root/etc/conf.d/syslog"
printf '#!/bin/sh\nprintf "legacy wrapper\\n"\n' > "$tmp/root/usr/local/sbin/teagram-logs"
chmod 755 "$tmp/root/usr/local/sbin/teagram-logs"
printf '#!/bin/sh\nprintf "legacy ops wrapper\\n"\n' > "$tmp/root/usr/local/sbin/teagram-ops"
chmod 700 "$tmp/root/usr/local/sbin/teagram-ops"
cat > "$tmp/bin/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${FIXTURE_EXPECT_S3_ENV:-0}" == 1 && "$1" == compose ]]; then
  case "$*" in
    *"--env-file $DEPLOY_DIR/.env --env-file $FIXTURE_PARKED_ENV"*) ;;
    *) echo "S3 deploy did not load both environment files: $*" >&2; exit 97 ;;
  esac
fi
case "$*" in
  *'--entrypoint /usr/local/bin/mtproto-smoke'*)
    case "$*" in
      *'--network host'*'--read-only'*'--cap-drop ALL'*'--security-opt no-new-privileges'*'--user 65532:65532'*) ;;
      *) echo "MTProto smoke must use the deployed image with host networking and non-root restrictions: $*" >&2; exit 98 ;;
    esac
    smoke_image=
    for arg in "$@"; do smoke_image=$arg; done
    expected_smoke_image=${FIXTURE_SMOKE_IMAGE_ID:-sha256:fixture}
    if [[ "$smoke_image" != "$expected_smoke_image" ]]; then
      echo "MTProto smoke used $smoke_image; expected deployed image $expected_smoke_image" >&2
      exit 98
    fi
    printf 'mtproto-smoke %s\n' "$smoke_image" >> "${DEPLOY_ACTION_LOG:?}"
    if [[ "${FIXTURE_SMOKE_SUCCESS:-1}" != 1 ]]; then
      echo 'fixture MTProto smoke failure' >&2
      exit 91
    fi
    printf 'MTProto auth-key exchange passed on 100.124.236.66:2443\n'
    ;;
  *config_files*teagram-teagramd-1*) echo "${FIXTURE_COMPOSE_FILES:-$DEPLOY_DIR/.rollout-compose.local-abc.yml,$DEPLOY_DIR/docker-compose.override.yml}" ;;
  *config_files*) echo "unexpected live container name: $*" >&2; exit 98 ;;
  *'{{.Image}}'*)
    if [[ -f "${DEPLOY_IMAGE_ID_FILE:?}" ]]; then
      cat "$DEPLOY_IMAGE_ID_FILE"
    else
      echo 'sha256:fixture'
    fi
    ;;
  *'pg_dump'*)
    [[ "${FIXTURE_DEPLOY_SUCCESS:-0}" == 1 ]] || { echo "unexpected docker call: $*" >&2; exit 99; }
    printf 'fixture dump complete\n'
    ;;
  *org.opencontainers.image.revision*) echo "$DEPLOY_SHA" ;;
  *'{{range .Config.Env}}'*) echo 'TG_RSA_KEY_FINGERPRINT=12345' ;;
  *'ps -a'*) ;;
  *'logs --since 3m teagramd'*)
    printf 'readiness\n' >> "${DEPLOY_ACTION_LOG:?}"
    echo 'advertise=teagram.tailaa4918.ts.net:2443'
    ;;
  *) echo "unexpected docker call: $*" >&2; exit 99 ;;
esac
DOCKER
chmod 755 "$tmp/bin/docker"
cat > "$tmp/bin/flock" <<'FLOCK'
#!/usr/bin/env bash
set -euo pipefail
target=$(readlink "/proc/$PPID/fd/${2:?missing file descriptor}")
printf '%s\n' "$target" >> "$DEPLOY_LOCK_LOG"
FLOCK
chmod 755 "$tmp/bin/flock"

failures=0
export LINK_EDGE_DEPLOY_LOG="$tmp/link-edge-deploys"
export DEPLOY_ACTION_LOG="$tmp/deploy-actions"
export DEPLOY_IMAGE_ID_FILE="$tmp/root/current-image-id"
: > "$LINK_EDGE_DEPLOY_LOG"
: > "$DEPLOY_ACTION_LOG"
# expect <exit code> <last line> [VAR=value...]
expect() {
  local want_rc="$1" want_last="$2" want_contains='' out rc=0
  shift 2
  if [[ "${1:-}" == --contains ]]; then
    want_contains="$2"
    shift 2
  fi
  out=$(env "$@" PATH="$tmp/bin:$PATH" DEPLOY_DIR="$tmp/live" DEPLOY_ROOT="$tmp/root" \
    DEPLOY_LOCK_LOG="$tmp/lock-targets" bash "$script" 2>&1) || rc=$?
  if [[ "$rc" != "$want_rc" || "${out##*$'\n'}" != "$want_last" || \
      ( -n "$want_contains" && "$out" != *"$want_contains"* ) ]]; then
    printf 'FAIL %s: want rc=%s last=%q, got rc=%s output:\n%s\n' "$*" "$want_rc" "$want_last" "$rc" "$out" >&2
    failures=$((failures + 1))
  fi
}

bad='DEPLOY_SHA must be a full 40-hex commit SHA'
expect 1 "$bad"
expect 1 "$bad" DEPLOY_SHA=
expect 1 "$bad" DEPLOY_SHA="${second:0:7}"
expect 1 "$bad" DEPLOY_SHA="${second}0"
expect 1 "$bad" DEPLOY_SHA="$(tr a-f A-F <<<"$second")"
expect 1 "$side is not on origin/main; refusing" DEPLOY_SHA="$side"
expect 1 "0000000000000000000000000000000000000000 is not on origin/main; refusing" DEPLOY_SHA=0000000000000000000000000000000000000000
chmod 600 "$tmp/live/tracked.txt" "$tmp/live/tracked-script.sh"
expect 0 "live is newer ($second), skipped $first" --contains \
  'tracked file modes: normalized=2 mismatches=0' DEPLOY_SHA="$first"
if [[ "$(stat -c '%a' "$tmp/live/tracked.txt")" != 644 || \
    "$(stat -c '%a' "$tmp/live/tracked-script.sh")" != 755 ]]; then
  echo 'FAIL tracked file modes were not restored from git' >&2
  failures=$((failures + 1))
fi
local_files_after=$(stat -c '%n:%u:%g:%a' \
  "$tmp/live/.env" "$tmp/live/pgdump-fixture.sql" "$tmp/live/rollout-evidence.txt")
if [[ "$local_files_after" != "$local_files_before" ]]; then
  echo 'FAIL untracked host-local file owners or modes changed' >&2
  failures=$((failures + 1))
fi

printf 'sha256:already-deployed\n' > "$DEPLOY_IMAGE_ID_FILE"
echo "source_sha=$second image_id=sha256:already-deployed built_at=fixture" > "$tmp/root/main1238-fixture.build/built-image.identity"
: > "$DEPLOY_ACTION_LOG"
expect 0 "already at $second with runner provenance" DEPLOY_SHA="$second" \
  FIXTURE_SMOKE_IMAGE_ID=sha256:already-deployed
if [[ "$(cat "$DEPLOY_ACTION_LOG")" != $'link-edge\nmtproto-smoke sha256:already-deployed' ]]; then
  echo 'FAIL an already deployed server SHA did not run the host-side smoke after the link edge' >&2
  failures=$((failures + 1))
fi
if ! cmp -s "$tmp/root/usr/local/sbin/teagram-logs" "$tmp/work/deploy/teagram/teagram-logs.sh"; then
  echo 'FAIL deploy did not install the commit-tracked teagram-logs command' >&2
  failures=$((failures + 1))
fi
if ! cmp -s "$tmp/root/usr/local/sbin/teagram-ops" "$tmp/work/deploy/teagram/teagram-ops.sh" || \
  [[ "$(stat -c '%a' "$tmp/root/usr/local/sbin/teagram-ops")" != 700 ]]; then
  echo 'FAIL deploy did not install the commit-tracked root-only teagram-ops command' >&2
  failures=$((failures + 1))
fi
if ! cmp -s "$tmp/root/etc/conf.d/syslog" "$tmp/work/deploy/teagram/syslog.conf"; then
  echo 'FAIL deploy did not install the bounded syslog rotation configuration' >&2
  failures=$((failures + 1))
fi
syslog_backup="$tmp/root/teagram-syslog-before-${second:0:7}"
if [[ ! -s "$syslog_backup" || "$(stat -c '%a' "$syslog_backup")" != 600 ]] || \
  ! grep -Fqx 'SYSLOGD_OPTS="-t"' "$syslog_backup"; then
  echo 'FAIL deploy did not keep a root-only copy of the previous syslog configuration' >&2
  failures=$((failures + 1))
fi
backup="$tmp/root/teagram-logs-before-${second:0:7}"
if [[ ! -s "$backup" || "$(stat -c '%a' "$backup")" != 600 || "$(<"$backup")" != *'legacy wrapper'* ]]; then
  echo 'FAIL deploy did not keep a root-only copy of the prior teagram-logs command' >&2
  failures=$((failures + 1))
fi
ops_backup="$tmp/root/teagram-ops-before-${second:0:7}"
if [[ ! -s "$ops_backup" || "$(stat -c '%a' "$ops_backup")" != 600 || "$(<"$ops_backup")" != *'legacy ops wrapper'* ]]; then
  echo 'FAIL deploy did not keep a root-only copy of the prior teagram-ops command' >&2
  failures=$((failures + 1))
fi
if [[ "$(tail -n 1 "$LINK_EDGE_DEPLOY_LOG")" != "$second" ]]; then
  echo 'FAIL an already deployed server SHA did not refresh the link edge' >&2
  failures=$((failures + 1))
fi
: > "$DEPLOY_ACTION_LOG"
expect 91 'fixture MTProto smoke failure' FIXTURE_SMOKE_SUCCESS=0 DEPLOY_SHA="$second" \
  FIXTURE_SMOKE_IMAGE_ID=sha256:already-deployed
if [[ "$(cat "$DEPLOY_ACTION_LOG")" != $'link-edge\nmtproto-smoke sha256:already-deployed' ]]; then
  echo 'FAIL a failed host-side smoke did not follow the link-edge update' >&2
  failures=$((failures + 1))
fi

# Without a provenance record, an equal SHA must reach the deploy path
# (the fake docker refuses the pg_dump call).
echo "source_sha=$first image_id=sha256:other " > "$tmp/root/main1238-fixture.build/built-image.identity"
expect 99 "unexpected docker call: compose exec -T postgres pg_dump -U postgres teagram" DEPLOY_SHA="$second"
# A newer commit on main deploys.
expect 99 "unexpected docker call: compose exec -T postgres pg_dump -U postgres teagram" DEPLOY_SHA="$third"
mkdir -p "$tmp/root/teagramd-rollout-runner"
printf 'stale qualifier fixture\n' > "$tmp/root/teagramd-rollout-runner/qualify-rustfs-transition.py"
printf 'sha256:before-third-rollout\n' > "$DEPLOY_IMAGE_ID_FILE"
: > "$DEPLOY_ACTION_LOG"
expect 0 "deployed $third" --contains \
  'tracked file modes: normalized=1 mismatches=0' FIXTURE_DEPLOY_SUCCESS=1 DEPLOY_SHA="$third" \
  FIXTURE_POST_IMAGE_ID=sha256:after-third-rollout FIXTURE_SMOKE_IMAGE_ID=sha256:after-third-rollout
if [[ "$(cat "$DEPLOY_ACTION_LOG")" != $'readiness\nlink-edge\nmtproto-smoke sha256:after-third-rollout' ]]; then
  echo 'FAIL a new server rollout did not run the host-side smoke after readiness and link-edge update' >&2
  failures=$((failures + 1))
fi
if [[ "$(tail -n 1 "$LINK_EDGE_DEPLOY_LOG")" != "$third" ]]; then
  echo 'FAIL a successful server rollout did not refresh the link edge at the same SHA' >&2
  failures=$((failures + 1))
fi
if [[ "$(stat -c '%a' "$tmp/live/tracked.txt")" != 644 || \
    "$(stat -c '%a' "$tmp/live/tracked-script.sh")" != 755 || \
    "$(stat -c '%a' "$tmp/live/new-tracked.txt")" != 644 ]]; then
  echo 'FAIL tracked modes were not restored after the rollout updated the checkout' >&2
  failures=$((failures + 1))
fi
local_files_after=$(stat -c '%n:%u:%g:%a' \
  "$tmp/live/.env" "$tmp/live/pgdump-fixture.sql" "$tmp/live/rollout-evidence.txt")
if [[ "$local_files_after" != "$local_files_before" ]]; then
  echo 'FAIL untracked host-local file owners or modes changed after rollout' >&2
  failures=$((failures + 1))
fi

unapproved_compose="$tmp/live/docker-compose.yml,$tmp/live/docker-compose.override.yml"
expect 1 'live S3 Compose has no valid s3-accepted authority or credentials; refusing' \
  FIXTURE_COMPOSE_FILES="$unapproved_compose" DEPLOY_SHA="$third"

fourth=$(commit fourth)
git -C "$tmp/work" push -q "$tmp/origin.git" main
moved_compose="$tmp/live/docker-compose.yml,$tmp/live/docker-compose.local-blobs.yml,$tmp/live/docker-compose.override.yml"
expect 0 "deployed $fourth" --contains \
  'runner exit 0' FIXTURE_DEPLOY_SUCCESS=1 \
  FIXTURE_COMPOSE_FILES="$moved_compose" DEPLOY_SHA="$fourth"
if [[ "$(tail -n 1 "$LINK_EDGE_DEPLOY_LOG")" != "$fourth" ]]; then
  echo 'FAIL a successful local-blob rollout did not refresh the link edge at the same SHA' >&2
  failures=$((failures + 1))
fi

fifth=$(commit fifth)
git -C "$tmp/work" push -q "$tmp/origin.git" main
mkdir -p "$tmp/telegram-server"
printf 'fixture parked credentials\n' > "$tmp/telegram-server/.env.rustfs-parked"
expect 1 'live S3 Compose has no valid s3-accepted authority or credentials; refusing' \
  FIXTURE_COMPOSE_FILES="$unapproved_compose" \
  FIXTURE_PARKED_ENV="$tmp/telegram-server/.env.rustfs-parked" DEPLOY_SHA="$fifth"
expect 0 "deployed $fifth" --contains 'runner exit 0' FIXTURE_DEPLOY_SUCCESS=1 \
  FIXTURE_COMPOSE_FILES="$unapproved_compose" FIXTURE_AUTHORITY_OUTCOME=s3-accepted \
  FIXTURE_EXPECT_S3_ENV=1 FIXTURE_PARKED_ENV="$tmp/telegram-server/.env.rustfs-parked" DEPLOY_SHA="$fifth"

smoke_failure=$(commit smoke-failure)
git -C "$tmp/work" push -q "$tmp/origin.git" main
printf 'sha256:before-failed-rollout\n' > "$DEPLOY_IMAGE_ID_FILE"
: > "$DEPLOY_ACTION_LOG"
expect 91 'fixture MTProto smoke failure' FIXTURE_DEPLOY_SUCCESS=1 DEPLOY_SHA="$smoke_failure" \
  FIXTURE_POST_IMAGE_ID=sha256:after-failed-rollout \
  FIXTURE_SMOKE_IMAGE_ID=sha256:after-failed-rollout FIXTURE_SMOKE_SUCCESS=0
if [[ "$(cat "$DEPLOY_ACTION_LOG")" != $'readiness\nlink-edge\nmtproto-smoke sha256:after-failed-rollout' ]]; then
  echo 'FAIL a fresh rollout did not use its new image and propagate a failed host-side smoke' >&2
  failures=$((failures + 1))
fi

if [[ ! -s "$tmp/lock-targets" ]] || grep -Fvxq /tmp/teagram-deploy.lock "$tmp/lock-targets"; then
  echo 'FAIL deploy-main.sh did not use the teagram deploy lock' >&2
  failures=$((failures + 1))
fi

if [[ "$failures" != 0 ]]; then
  echo "$failures deploy-main.sh fixture case(s) failed" >&2
  exit 1
fi
echo "deploy-main.sh fixture cases pass"
