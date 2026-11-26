#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -P "$SCRIPT_DIR/../../.." && pwd)
TMP_DIR=$(mktemp -d)
trap 'rm -rf -- "$TMP_DIR"' EXIT

failures=0
ok() { printf 'ok - %s\n' "$1"; }
not_ok() {
  printf 'not ok - %s\n' "$1" >&2
  failures=$((failures + 1))
}

# The privileged sudo wrapper accepts the exact server, web, and QA command
# forms used by charlie's forced command. Rejecting a command has no fallback.
# shellcheck source=deploy/teagram-demo/lima/command-contract.sh
source "$SCRIPT_DIR/command-contract.sh"
SHA=$(printf 'a%.0s' {1..40})
SHA2=$(printf 'b%.0s' {1..40})
DIGEST=$(printf 'c%.0s' {1..64})
IMAGE_DIGEST="sha256:$DIGEST"
valid_commands=(
  "$SHA $IMAGE_DIGEST"
  "qa-invite $SHA up $IMAGE_DIGEST"
  "qa-invite $SHA down"
  "qa-invite $SHA status"
  "qa-invite $SHA create-account test_operator"
  "web $SHA $SHA2 $IMAGE_DIGEST $DIGEST 0"
  "web $SHA $SHA2 $IMAGE_DIGEST $DIGEST 12345"
)
for command in "${valid_commands[@]}"; do
  if demo_deploy_command_valid "$command"; then
    ok "command accepted: ${command%% *}"
  else
    not_ok "command accepted: ${command%% *}"
  fi
done
invalid_commands=(
  "$SHA  $IMAGE_DIGEST"
  "${SHA:1} $IMAGE_DIGEST"
  "$SHA sha256:bad"
  "bash -c id"
  "qa-invite $SHA create-account 'test operator'"
  "web $SHA $SHA2 $IMAGE_DIGEST $DIGEST 0123"
)
for command in "${invalid_commands[@]}"; do
  if demo_deploy_command_valid "$command"; then
    not_ok "command rejected: ${command%% *}"
  else
    ok "command rejected: ${command%% *}"
  fi
done
if demo_deploy_command_valid "${valid_commands[0]}" extra; then
  not_ok 'extra sudo wrapper arguments rejected'
else
  ok 'extra sudo wrapper arguments rejected'
fi

if grep -Eq '^([[:space:]]*Include[[:space:]]+/etc/ssh/sshd_config\.d|.*>>?[[:space:]]*/etc/ssh/sshd_config)' \
  "$SCRIPT_DIR/install-deploy-entry.sh"; then
  not_ok 'installer leaves port 22 configuration paths untouched'
else
  ok 'installer leaves port 22 configuration paths untouched'
fi
for expected in \
  'ListenAddress 100.117.107.78:2223' \
  'AuthorizedKeysFile /etc/ssh/authorized_keys/%u' \
  'AuthenticationMethods publickey' \
  'PasswordAuthentication no' \
  'DisableForwarding yes' \
  'PermitTTY no' \
  'PermitTunnel no' \
  'PermitUserEnvironment no' \
  'PermitUserRC no' \
  'ForceCommand /usr/local/libexec/teagram-demo-deploy-entry'; do
  if grep -Fqx "$expected" "$SCRIPT_DIR/sshd_config"; then
    ok "listener config: $expected"
  else
    not_ok "listener config: $expected"
  fi
done
if grep -Fqx 'teagram-deploy ALL=(root) NOPASSWD: /usr/local/sbin/teagram-demo-deploy' \
  "$SCRIPT_DIR/teagram-deploy.sudoers"; then
  ok 'sudoers grants only the root wrapper'
else
  not_ok 'sudoers grants only the root wrapper'
fi
# shellcheck disable=SC2016 # Match literal source text without expanding it.
if grep -Fq 'exec sudo -n /usr/local/sbin/teagram-demo-deploy "${SSH_ORIGINAL_COMMAND:-}"' \
  "$SCRIPT_DIR/deploy-entry.sh"; then
  ok 'unprivileged entry forwards one command argument'
else
  not_ok 'unprivileged entry forwards one command argument'
fi
# shellcheck disable=SC2016 # Match literal source text without expanding it.
if grep -Fq 'demo_deploy_command_valid "$COMMAND"' "$SCRIPT_DIR/deploy-root-wrapper.sh"; then
  ok 'root wrapper revalidates the command'
else
  not_ok 'root wrapper revalidates the command'
fi
# shellcheck disable=SC2016 # Match literal source text without expanding it.
if grep -Fq 'cmp -s "$SSH_BASE_EFFECTIVE" "$TMP_DIR/sshd-main.after"' \
  "$SCRIPT_DIR/install-deploy-entry.sh"; then
  ok 'installer verifies sshd -T for port 22 is unchanged'
else
  not_ok 'installer verifies sshd -T for port 22 is unchanged'
fi

# Exercise retention with a foreign repository and a shared image. Only
# unreferenced images owned exclusively by the demo repository may be removed.
# shellcheck source=deploy/teagram-demo/demo-common.sh
source "$REPO_ROOT/deploy/teagram-demo/demo-common.sh"
# shellcheck source=deploy/teagram-demo/image-retention.sh
source "$REPO_ROOT/deploy/teagram-demo/image-retention.sh"
DEMO_STATE_DIR="$TMP_DIR/state"
DEMO_IMAGE_HISTORY_FILE="$DEMO_STATE_DIR/successful-images.json"
mkdir -m 0700 "$DEMO_STATE_DIR"
MOCK_BIN="$TMP_DIR/bin"
mkdir -m 0700 "$MOCK_BIN"
REAL_STAT=$(command -v stat)
export TEAGRAM_TEST_HISTORY_FILE="$DEMO_IMAGE_HISTORY_FILE"
export TEAGRAM_TEST_REAL_STAT="$REAL_STAT"
cat > "$MOCK_BIN/stat" <<'EOF'
#!/usr/bin/env bash
if [[ ${1:-} == -c && ${2:-} == '%u:%a' && ${3:-} == "$TEAGRAM_TEST_HISTORY_FILE" ]]; then
  printf '0:600\n'
  exit 0
fi
exec "$TEAGRAM_TEST_REAL_STAT" "$@"
EOF
chmod 0755 "$MOCK_BIN/stat"
cat > "$MOCK_BIN/docker" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
repo=ghcr.io/teagramhq/teagram-server-demo
foreign=ghcr.io/other/stack
id1="sha256:$(printf '1%.0s' {1..64})"
id2="sha256:$(printf '2%.0s' {1..64})"
id3="sha256:$(printf '3%.0s' {1..64})"
id4="sha256:$(printf '4%.0s' {1..64})"
id5="sha256:$(printf '5%.0s' {1..64})"
id6="sha256:$(printf '6%.0s' {1..64})"
case "$*" in
  'image ls --digests --no-trunc --format {{.ID}} {{.Repository}}')
    printf '%s %s\n' "$id1" "$repo" "$id2" "$repo" "$id3" "$repo" \
      "$id4" "$repo" "$id5" "$repo" "$id5" "$foreign" "$id6" "$foreign"
    ;;
  'ps -a -q --no-trunc --filter ancestor='*) ;;
  'image inspect --format {{json .RepoDigests}} '*)
    image_id=${!#}
    if [[ $image_id == "$id5" ]]; then
      printf '["%s@sha256:%s","%s@sha256:%s"]\n' "$repo" "$(printf '5%.0s' {1..64})" "$foreign" "$(printf '5%.0s' {1..64})"
    else
      printf '["%s@sha256:%s"]\n' "$repo" "${image_id#sha256:}"
    fi
    ;;
  'image inspect --format {{json .RepoTags}} '*) printf '[]\n' ;;
  'image rm '*)
    image_id=${!#}
    printf '%s\n' "$image_id" >> "$TEAGRAM_TEST_REMOVED_FILE"
    if [[ ${TEAGRAM_TEST_FAIL_RM:-} == "$image_id" ]]; then exit 1; fi
    ;;
  *) printf 'unexpected docker invocation: %s\n' "$*" >&2; exit 2 ;;
esac
EOF
chmod 0755 "$MOCK_BIN/docker"

ID1="sha256:$(printf '1%.0s' {1..64})"
ID2="sha256:$(printf '2%.0s' {1..64})"
ID3="sha256:$(printf '3%.0s' {1..64})"
ID4="sha256:$(printf '4%.0s' {1..64})"
ID5="sha256:$(printf '5%.0s' {1..64})"
printf '[{"revision":"%s","image_id":"%s","image_ref":"%s@sha256:%s"},{"revision":"%s","image_id":"%s","image_ref":"%s@sha256:%s"},{"revision":"%s","image_id":"%s","image_ref":"%s@sha256:%s"}]\n' \
  "$SHA" "$ID1" "$DEMO_IMAGE_REPOSITORY" "$(printf '1%.0s' {1..64})" \
  "$SHA2" "$ID2" "$DEMO_IMAGE_REPOSITORY" "$(printf '2%.0s' {1..64})" \
  "$SHA" "$ID3" "$DEMO_IMAGE_REPOSITORY" "$(printf '3%.0s' {1..64})" > "$DEMO_IMAGE_HISTORY_FILE"
chmod 0600 "$DEMO_IMAGE_HISTORY_FILE"
export PATH="$MOCK_BIN:$PATH"
export TEAGRAM_TEST_REMOVED_FILE="$TMP_DIR/removed"
: > "$TEAGRAM_TEST_REMOVED_FILE"
if demo_prune_images "$ID1" 2> "$TMP_DIR/retention-stderr"; then
  ok 'image retention completes when removals succeed'
else
  not_ok 'image retention completes when removals succeed'
fi
if [[ "$(<"$TEAGRAM_TEST_REMOVED_FILE")" == "$ID4" ]] &&
  grep -Fq "$ID5" "$TMP_DIR/retention-stderr" &&
  ! grep -Fq "sha256:$(printf '6%.0s' {1..64})" "$TEAGRAM_TEST_REMOVED_FILE"; then
  ok 'retention removes only excess demo images and protects shared/foreign images'
else
  not_ok 'retention removes only excess demo images and protects shared/foreign images'
fi

: > "$TEAGRAM_TEST_REMOVED_FILE"
if TEAGRAM_TEST_FAIL_RM="$ID4" demo_prune_images "$ID1" 2> "$TMP_DIR/removal-failure"; then
  not_ok 'failed image removal is returned as a failure'
else
  ok 'failed image removal is returned as a failure'
fi
if grep -Fq "could not remove demo image $ID4" "$TMP_DIR/removal-failure"; then
  ok 'failed image removal is reported with its ID'
else
  not_ok 'failed image removal is reported with its ID'
fi

ROTATION_FILE="$TMP_DIR/rotation.json"
cp -- "$DEMO_IMAGE_HISTORY_FILE" "$ROTATION_FILE"
chmod 0600 "$ROTATION_FILE"
DEMO_IMAGE_HISTORY_FILE="$ROTATION_FILE"
export TEAGRAM_TEST_HISTORY_FILE="$ROTATION_FILE"
demo_record_successful_image "$SHA2" "$ID4" \
  "$DEMO_IMAGE_REPOSITORY@sha256:$(printf '4%.0s' {1..64})" \
  "$SHA" "$ID1" "$DEMO_IMAGE_REPOSITORY@sha256:$(printf '1%.0s' {1..64})"
if jq -e --arg current "$ID4" --arg previous "$ID1" --arg older "$ID2" \
  '[.[].image_id] == [$current, $previous, $older]' "$ROTATION_FILE" >/dev/null; then
  ok 'successful image history keeps the running release and two previous releases'
else
  not_ok 'successful image history keeps the running release and two previous releases'
fi

python3 - "$REPO_ROOT" <<'PY'
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
compose = (root / "deploy/teagram-demo/lima/compose.yaml").read_text()
rollout = (root / "deploy/teagram-demo/lima/deploy-main.sh").read_text()
cleanup = rollout.split("lima_cleanup() {", 1)[1].split("\n}", 1)[0]
checks = {
    "lima Compose has no candidate server": "teagramd-next:" not in compose,
    "maintenance window stops before migration and start": (
        rollout.index('lima_compose "$PREV_IMAGE_REF" stop teagramd')
        < rollout.index('lima_compose "$CANDIDATE_IMAGE_REF" run --rm -T --no-deps migrate')
        < rollout.index('lima_compose "$CANDIDATE_IMAGE_REF" up -d --no-deps --force-recreate')
    ),
    "new release health is checked through loopback /healthz": 'HEALTH_URL=http://127.0.0.1:8080/healthz' in rollout
    and 'lima_wait_health || demo_fail' in rollout,
    "rollback removes the candidate and restarts the previous image": 'rm -f teagramd' in cleanup
    and 'lima_compose "$PREV_IMAGE_REF" up -d --no-deps --force-recreate' in cleanup,
    "rollback failure names both revisions and does not restore the database": 'requested_revision=%s previous_revision=%s' in cleanup
    and 'database was not restored or changed during rollback' in cleanup
    and 'pg_restore' not in cleanup,
}
for description, passed in checks.items():
    print(f"ok - {description}" if passed else f"not ok - {description}")
if not all(checks.values()):
    raise SystemExit(1)
PY

CLEANUP_FUNCTION="$TMP_DIR/lima-cleanup-function.sh"
sed -n '/^lima_cleanup() {$/,/^}/p' "$SCRIPT_DIR/deploy-main.sh" > "$CLEANUP_FUNCTION"
cat > "$TMP_DIR/lima-rollback-regression.sh" <<'BASH'
set -Eeuo pipefail
source "$CLEANUP_FUNCTION"
SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
PREV_IMAGE_REVISION=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
PREV_IMAGE_REF=previous-image
PREV_IMAGE_ID=sha256:previous
CANDIDATE_IMAGE_REF=candidate-image
CANDIDATE_IMAGE_ID=sha256:candidate
BACKUP_DIR=verified-backup
PREV_WEB_RELEASE=
OLD_SERVER_STOPPED=1
CANDIDATE_STARTED=1
DEPLOY_COMMITTED=0
ACTIVE_RELEASE_UPDATED=1
HISTORY_READY=1
ROLLBACK_HEALTH_FAIL=${ROLLBACK_HEALTH_FAIL:-0}
lima_compose() { printf 'compose %s %s\n' "$1" "${*:2}" >> "$ROLLBACK_LOG"; }
lima_wait_health() {
  printf 'health-check\n' >> "$ROLLBACK_LOG"
  (( ROLLBACK_HEALTH_FAIL == 0 ))
}
demo_write_active_release() { printf 'active %s %s\n' "$1" "$2" >> "$ROLLBACK_LOG"; }
demo_prune_images() { printf 'prune %s\n' "$*" >> "$ROLLBACK_LOG"; }
trap lima_cleanup EXIT
false
BASH

ROLLBACK_LOG="$TMP_DIR/rollback-success.log"
if CLEANUP_FUNCTION="$CLEANUP_FUNCTION" ROLLBACK_LOG="$ROLLBACK_LOG" \
  bash "$TMP_DIR/lima-rollback-regression.sh" 2> "$TMP_DIR/rollback-success.err"; then
  not_ok 'failed candidate remains a failed deploy after rollback'
else
  ok 'failed candidate remains a failed deploy after rollback'
fi
candidate_remove_line=$(grep -nF 'compose candidate-image rm -f teagramd' "$ROLLBACK_LOG" | cut -d: -f1)
previous_start_line=$(grep -nF 'compose previous-image up -d --no-deps --force-recreate --wait --wait-timeout 120 teagramd' \
  "$ROLLBACK_LOG" | cut -d: -f1)
if [[ -n "$candidate_remove_line" && -n "$previous_start_line" ]] &&
  (( candidate_remove_line < previous_start_line )) &&
  grep -Fq 'active teagramd previous-image' "$ROLLBACK_LOG" &&
  grep -Fq 'lima rollout rolled back to revision=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' "$TMP_DIR/rollback-success.err"; then
  ok 'failed candidate is removed before the previous image starts and passes health'
else
  not_ok 'failed candidate is removed before the previous image starts and passes health'
fi

ROLLBACK_LOG="$TMP_DIR/rollback-failure.log"
if CLEANUP_FUNCTION="$CLEANUP_FUNCTION" ROLLBACK_LOG="$ROLLBACK_LOG" ROLLBACK_HEALTH_FAIL=1 \
  bash "$TMP_DIR/lima-rollback-regression.sh" 2> "$TMP_DIR/rollback-failure.err"; then
  not_ok 'failed rollback health returns a nonzero deploy result'
else
  ok 'failed rollback health returns a nonzero deploy result'
fi
if grep -Fq 'requested_revision=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa previous_revision=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' \
  "$TMP_DIR/rollback-failure.err" &&
  ! grep -Eq 'compose .*exec .*postgres|pg_restore' "$ROLLBACK_LOG"; then
  ok 'failed rollback names both revisions and does no database restore'
else
  not_ok 'failed rollback names both revisions and does no database restore'
fi

if (( failures > 0 )); then
  exit 1
fi
