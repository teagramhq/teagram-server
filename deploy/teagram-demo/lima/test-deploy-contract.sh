#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -P "$SCRIPT_DIR/../../.." && pwd)
TMP_DIR=$(mktemp -d)
ALPINE_CONTAINER=
cleanup_test() {
  [[ -z "$ALPINE_CONTAINER" ]] || docker rm -f "$ALPINE_CONTAINER" >/dev/null 2>&1 || true
  rm -rf -- "$TMP_DIR"
}
trap cleanup_test EXIT

failures=0
ok() { printf 'ok - %s\n' "$1"; }
not_ok() {
  printf 'not ok - %s\n' "$1" >&2
  failures=$((failures + 1))
}

# The root forced command accepts the exact server, web, and QA command forms
# used by charlie's forced command. Rejecting a command has no fallback.
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
  not_ok 'extra root wrapper arguments rejected'
else
  ok 'extra root wrapper arguments rejected'
fi

if grep -Eq '^([[:space:]]*Include[[:space:]]+/etc/ssh/sshd_config\.d|.*>>?[[:space:]]*/etc/ssh/sshd_config)' \
  "$SCRIPT_DIR/install-deploy-entry.sh"; then
  not_ok 'installer leaves port 22 configuration paths untouched'
else
  ok 'installer leaves port 22 configuration paths untouched'
fi
for expected in \
  'ListenAddress 100.117.107.78:2223' \
  'AllowUsers root' \
  'AuthorizedKeysFile /etc/ssh/authorized_keys/teagram-demo-deploy-root' \
  'AuthenticationMethods publickey' \
  'PermitRootLogin forced-commands-only' \
  'PasswordAuthentication no' \
  'PermitEmptyPasswords no' \
  'DisableForwarding yes' \
  'PermitTTY no' \
  'PermitTunnel no' \
  'PermitUserEnvironment no' \
  'PermitUserRC no' \
  'ForceCommand /usr/local/sbin/teagram-demo-deploy'; do
  if grep -Fqx "$expected" "$SCRIPT_DIR/sshd_config"; then
    ok "listener config: $expected"
  else
    not_ok "listener config: $expected"
  fi
done
# shellcheck disable=SC2016 # Match installer source text literally.
if grep -Fq 'verify_checkout_ownership || fail' "$SCRIPT_DIR/install-deploy-entry.sh" &&
  grep -Fq 'remove_legacy_deploy_access' "$SCRIPT_DIR/install-deploy-entry.sh" &&
  grep -Fq 'KEY_LINE="restrict,command=\"$ROOT_WRAPPER\"' "$SCRIPT_DIR/install-deploy-entry.sh" &&
  ! grep -Eq 'apk add.*sudo|sudo -l|visudo|ensure_deploy_account|chpasswd' \
    "$SCRIPT_DIR/install-deploy-entry.sh"; then
  ok 'installer preserves the checkout scan, installs the root key and removes the legacy sudo account without installing sudo'
else
  not_ok 'installer preserves the checkout scan, installs the root key and removes the legacy sudo account without installing sudo'
fi
if awk '
  index($0, "migrate_legacy_runtime_paths || fail") { migration = NR }
  index($0, "verify_checkout_ownership || fail") { scan = NR }
  END { exit !(migration > 0 && scan > migration) }
' "$SCRIPT_DIR/install-deploy-entry.sh"; then
  ok 'installer migrates legacy runtime paths before scanning checkout ownership'
else
  not_ok 'installer migrates legacy runtime paths before scanning checkout ownership'
fi
# shellcheck disable=SC2016 # Match installer source text literally.
if [[ ! -e "$SCRIPT_DIR/deploy-entry.sh" && ! -e "$SCRIPT_DIR/teagram-deploy.sudoers" ]] &&
  grep -Fq 'rm -- "$LEGACY_SUDOERS_FILE"' "$SCRIPT_DIR/install-deploy-entry.sh" &&
  grep -Fq 'deluser "$LEGACY_DEPLOY_USER"' "$SCRIPT_DIR/install-deploy-entry.sh"; then
  ok 'legacy sudo entrypoint and account are removed and their source files are retired'
else
  not_ok 'legacy sudo entrypoint and account are removed and their source files are retired'
fi
# shellcheck disable=SC2016 # Match literal source text without expanding it.
if grep -Fq 'COMMAND=${SSH_ORIGINAL_COMMAND:-}' "$SCRIPT_DIR/deploy-root-wrapper.sh" &&
  grep -Fq 'if [[ $# != 0 ]]' "$SCRIPT_DIR/deploy-root-wrapper.sh"; then
  ok 'root wrapper reads the client command from SSH_ORIGINAL_COMMAND without shell arguments'
else
  not_ok 'root wrapper reads the client command from SSH_ORIGINAL_COMMAND without shell arguments'
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

FORCED_CHECKOUT_FUNCTION="$TMP_DIR/forced-checkout-function.sh"
{
  printf '%s\n' 'REPO=/opt/teagram-demo' "SHA=$SHA"
  sed -n '/^restore_checkout_after_deploy() {$/,/^}/p' "$REPO_ROOT/deploy/teagram-demo/forced-command.sh"
} > "$FORCED_CHECKOUT_FUNCTION"
cat > "$TMP_DIR/forced-checkout-regression.sh" <<'BASH'
set -Eeuo pipefail
source "$FORCED_CHECKOUT_FUNCTION"
GIT_LOG=$TMP_DIR/forced-git.log
git() { printf '%s\n' "$*" >> "$GIT_LOG"; }
: > "$GIT_LOG"
restore_checkout_after_deploy 75 previous-revision 2> "$TMP_DIR/post-cutover.err"
[[ ! -s "$GIT_LOG" ]]
grep -Fq 'preserving the checkout after post-cutover maintenance failed' "$TMP_DIR/post-cutover.err"
restore_checkout_after_deploy 1 previous-revision
grep -Fqx -- '-C /opt/teagram-demo reset --hard previous-revision' "$GIT_LOG"
BASH
if env FORCED_CHECKOUT_FUNCTION="$FORCED_CHECKOUT_FUNCTION" TMP_DIR="$TMP_DIR" \
  bash "$TMP_DIR/forced-checkout-regression.sh"; then
  ok 'post-cutover failures preserve the committed checkout while pre-cutover failures restore it'
else
  not_ok 'post-cutover failures preserve the committed checkout while pre-cutover failures restore it'
fi

ALPINE_FUNCTIONS="$TMP_DIR/alpine-functions.sh"
{
  printf '%s\n' 'REPO=/opt/teagram-demo' 'DEPLOY_USER=root' \
    'ROOT_WRAPPER=/usr/local/sbin/teagram-demo-deploy' \
    'AUTHORIZED_KEYS_DIR=/etc/ssh/authorized_keys' \
    'AUTHORIZED_KEYS=/etc/ssh/authorized_keys/teagram-demo-deploy-root' \
    'LEGACY_DEPLOY_USER=teagram-deploy' \
    'LEGACY_AUTHORIZED_KEYS=/etc/ssh/authorized_keys/teagram-deploy' \
    'LEGACY_ENTRY_PATH=/usr/local/libexec/teagram-demo-deploy-entry' \
    'LEGACY_SUDOERS_FILE=/etc/sudoers.d/teagram-deploy' \
    'RUNTIME_DIR=/var/lib/teagram-demo/lima/runtime'
  sed -n '/^fail() {$/,/^}/p' "$SCRIPT_DIR/install-deploy-entry.sh"
  sed -n '/^require_root_owned() {$/,/^}/p' "$SCRIPT_DIR/install-deploy-entry.sh"
  sed -n '/^install_key_file() {$/,/^}/p' "$SCRIPT_DIR/install-deploy-entry.sh"
  sed -n '/^reject_unmanaged_sudoers_rule() {$/,/^}/p' "$SCRIPT_DIR/install-deploy-entry.sh"
  sed -n '/^verify_checkout_ownership() {$/,/^}/p' "$SCRIPT_DIR/install-deploy-entry.sh"
  sed -n '/^migrate_legacy_runtime_path() {$/,/^}/p' "$SCRIPT_DIR/install-deploy-entry.sh"
  sed -n '/^migrate_legacy_runtime_paths() {$/,/^}/p' "$SCRIPT_DIR/install-deploy-entry.sh"
  sed -n '/^verify_legacy_deploy_access() {$/,/^}/p' "$SCRIPT_DIR/install-deploy-entry.sh"
  sed -n '/^remove_legacy_deploy_access() {$/,/^}/p' "$SCRIPT_DIR/install-deploy-entry.sh"
  sed -n '/^prepare_runtime_dirs() {$/,/^}/p' "$SCRIPT_DIR/prepare-blobs.sh"
} > "$ALPINE_FUNCTIONS"
cat > "$TMP_DIR/alpine-deploy-entry-regression.sh" <<'BASH'
set -Eeuo pipefail
source /tmp/alpine-functions.sh
chmod 0755 /

mkdir -p "$REPO/.git" "$REPO/deploy/teagram-demo"
printf 'root-owned checkout fixture\n' > "$REPO/deploy/teagram-demo/fixture"
chown -R root:root "$REPO"
chmod 0755 "$REPO" "$REPO/.git" "$REPO/deploy" "$REPO/deploy/teagram-demo"
chmod 0644 "$REPO/deploy/teagram-demo/fixture"

prepare_runtime_dirs
[[ "$RUNTIME_DIR" != "$REPO" && "$RUNTIME_DIR" != "$REPO/"* ]]
verify_checkout_ownership
legacy_state="$REPO/.state/teagram-demo"
legacy_tmp="$legacy_state/tmp"
mkdir -p "$legacy_tmp/turnstile-egress" "$legacy_tmp/haproxy" "$legacy_tmp/edge" \
  "$legacy_state/haproxy-run"
chmod 0755 "$REPO/.state" "$legacy_state" "$legacy_tmp"
printf 'legacy-turnstile\n' > "$legacy_tmp/turnstile-egress/migration-marker"
printf 'legacy-haproxy-tmp\n' > "$legacy_tmp/haproxy/migration-marker"
printf 'legacy-edge-tmp\n' > "$legacy_tmp/edge/migration-marker"
printf 'legacy-haproxy-socket\n' > "$legacy_state/haproxy-run/admin.sock"
chown 584792:584792 "$legacy_tmp/turnstile-egress" "$legacy_tmp/turnstile-egress/migration-marker"
chown 99:99 "$legacy_tmp/haproxy" "$legacy_tmp/edge" "$legacy_state/haproxy-run" \
  "$legacy_state/haproxy-run/admin.sock"
chmod 1777 "$legacy_tmp/turnstile-egress" "$legacy_tmp/haproxy" "$legacy_tmp/edge"
chmod 0700 "$legacy_state/haproxy-run"
if verify_checkout_ownership 2>/dev/null; then
  printf 'legacy container-owned runtime paths were accepted inside the checkout\n' >&2
  exit 1
fi
migrate_legacy_runtime_paths
[[ ! -e "$legacy_tmp/turnstile-egress" && ! -e "$legacy_tmp/haproxy" && ! -e "$legacy_tmp/edge" &&
  ! -e "$legacy_state/haproxy-run" ]]
grep -Fqx 'legacy-turnstile' "$RUNTIME_DIR/tmp/turnstile-egress/migration-marker"
grep -Fqx 'legacy-haproxy-tmp' "$RUNTIME_DIR/tmp/haproxy/migration-marker"
grep -Fqx 'legacy-edge-tmp' "$RUNTIME_DIR/tmp/edge/migration-marker"
grep -Fqx 'legacy-haproxy-socket' "$RUNTIME_DIR/haproxy-run/admin.sock"
[[ $(stat -c '%u:%g:%a' "$RUNTIME_DIR/tmp/turnstile-egress") == '584792:584792:1777' &&
  $(stat -c '%u:%g:%a' "$RUNTIME_DIR/tmp/haproxy") == '0:0:1777' &&
  $(stat -c '%u:%g:%a' "$RUNTIME_DIR/tmp/edge") == '0:0:1777' &&
  $(stat -c '%u:%g:%a' "$RUNTIME_DIR/haproxy-run") == '99:99:700' ]]
verify_checkout_ownership
migrate_legacy_runtime_paths
verify_checkout_ownership
touch "$RUNTIME_DIR/haproxy-run/admin.sock" "$RUNTIME_DIR/tmp/haproxy/proxy-temp" \
  "$RUNTIME_DIR/tmp/edge/proxy-temp" "$RUNTIME_DIR/tmp/turnstile-egress/proxy-temp"
chown 99:99 "$RUNTIME_DIR/haproxy-run/admin.sock" "$RUNTIME_DIR/tmp/haproxy/proxy-temp" \
  "$RUNTIME_DIR/tmp/edge/proxy-temp"
chown 584792:584792 "$RUNTIME_DIR/tmp/turnstile-egress/proxy-temp"
verify_checkout_ownership

install -d -o root -g root -m 0755 /etc/ssh/authorized_keys /etc/sudoers.d \
  /usr/local/libexec /usr/local/sbin /var/empty
adduser -S -D -H -h /var/empty -s /bin/sh "$LEGACY_DEPLOY_USER"
printf '%s\n' 'teagram-deploy ALL=(root) NOPASSWD: /usr/local/sbin/teagram-demo-deploy' \
  > "$LEGACY_SUDOERS_FILE"
chmod 0440 "$LEGACY_SUDOERS_FILE"
printf 'legacy authorized key\n' > "$LEGACY_AUTHORIZED_KEYS"
chmod 0644 "$LEGACY_AUTHORIZED_KEYS"
printf '#!/bin/sh\nexit 1\n' > "$LEGACY_ENTRY_PATH"
chmod 0755 "$LEGACY_ENTRY_PATH"

cp /etc/passwd /tmp/legacy-passwd.valid
set_legacy_passwd_field() {
  local field=$1 value=$2
  awk -F: -v OFS=: -v name="$LEGACY_DEPLOY_USER" -v field="$field" -v value="$value" \
    '$1 == name { $field = value; changed = 1 } { print } END { if (!changed) exit 1 }' \
    /etc/passwd > /tmp/legacy-passwd.next
  cat /tmp/legacy-passwd.next > /etc/passwd
}
restore_legacy_passwd() { cat /tmp/legacy-passwd.valid > /etc/passwd; }

assert_legacy_files_survive() {
  [[ -f "$LEGACY_SUDOERS_FILE" && -f "$LEGACY_AUTHORIZED_KEYS" && -f "$LEGACY_ENTRY_PATH" ]]
  grep -Fqx 'teagram-deploy ALL=(root) NOPASSWD: /usr/local/sbin/teagram-demo-deploy' \
    "$LEGACY_SUDOERS_FILE"
  grep -Fqx 'legacy authorized key' "$LEGACY_AUTHORIZED_KEYS"
  grep -Fqx '#!/bin/sh' "$LEGACY_ENTRY_PATH"
  awk -F: -v name="$LEGACY_DEPLOY_USER" '$1 == name { found = 1 } END { exit !found }' /etc/passwd
}

expect_legacy_refusal() {
  local label=$1 expected=$2
  shift 2
  if ( "$@" ) > /tmp/legacy-refusal.out 2>&1; then
    printf 'legacy access cleanup accepted invalid %s\n' "$label" >&2
    exit 1
  fi
  grep -Fq "$expected" /tmp/legacy-refusal.out || {
    cat /tmp/legacy-refusal.out >&2
    printf 'legacy access cleanup rejected %s for the wrong reason\n' "$label" >&2
    exit 1
  }
  assert_legacy_files_survive
  printf 'ok - legacy access cleanup refuses %s and preserves legacy files\n' "$label"
}

set_legacy_passwd_field 3 0
expect_legacy_refusal 'uid 0' 'legacy deploy account has unexpected identity settings' \
  remove_legacy_deploy_access
restore_legacy_passwd
set_legacy_passwd_field 6 /root
expect_legacy_refusal 'unexpected home' 'legacy deploy account has unexpected identity settings' \
  remove_legacy_deploy_access
restore_legacy_passwd
set_legacy_passwd_field 7 /bin/false
expect_legacy_refusal 'unexpected shell' 'legacy deploy account has unexpected identity settings' \
  remove_legacy_deploy_access
restore_legacy_passwd

reject_with_privileged_group() {
  local fixture_group=$1
  id() {
    if [[ "${1:-}" == -nG && "${2:-}" == "$LEGACY_DEPLOY_USER" ]]; then
      printf '%s %s\n' "$LEGACY_DEPLOY_USER" "$fixture_group"
    else
      command id "$@"
    fi
  }
  remove_legacy_deploy_access
}
for group in docker wheel sudo; do
  expect_legacy_refusal "$group membership" \
    "legacy deploy account unexpectedly belongs to $group" \
    reject_with_privileged_group "$group"
done

printf '%s\n' 'teagram-deploy ALL=(root) NOPASSWD: /bin/sh' > /etc/sudoers
expect_legacy_refusal 'an unmanaged /etc/sudoers grant' \
  'an unmanaged sudoers rule still grants access to the legacy deploy account' \
  remove_legacy_deploy_access
rm -f /etc/sudoers
printf '%s\n' 'teagram-deploy ALL=(root) NOPASSWD: /bin/sh' > /etc/sudoers.d/unmanaged
chmod 0440 /etc/sudoers.d/unmanaged
expect_legacy_refusal 'an unmanaged sudoers.d grant' \
  'an unmanaged sudoers file still grants access to the legacy deploy account: /etc/sudoers.d/unmanaged' \
  remove_legacy_deploy_access
rm -f /etc/sudoers.d/unmanaged

printf '%s\n' '# force grep to report a read error' > /etc/sudoers.d/grep-read-error
chmod 0440 /etc/sudoers.d/grep-read-error
reject_with_sudoers_read_error() {
  grep() {
    local argument
    for argument in "$@"; do
      [[ "$argument" != /etc/sudoers.d/grep-read-error ]] || return 2
    done
    command grep "$@"
  }
  remove_legacy_deploy_access
}
expect_legacy_refusal 'a sudoers read error' \
  'could not inspect sudoers file: /etc/sudoers.d/grep-read-error' \
  reject_with_sudoers_read_error
rm -f /etc/sudoers.d/grep-read-error

remove_legacy_deploy_access
! id "$LEGACY_DEPLOY_USER" >/dev/null 2>&1
[[ ! -e "$LEGACY_SUDOERS_FILE" && ! -e "$LEGACY_AUTHORIZED_KEYS" && ! -e "$LEGACY_ENTRY_PATH" ]]

mkdir -p "$REPO/.git" "$REPO/deploy/teagram-demo/lima"
cp /tmp/deploy-root-wrapper.sh "$REPO/deploy/teagram-demo/lima/deploy-root-wrapper.sh"
cp /tmp/command-contract.sh "$REPO/deploy/teagram-demo/lima/command-contract.sh"
cat > "$REPO/deploy/teagram-demo/forced-command.sh" <<'COMMAND'
#!/bin/bash
set -Eeuo pipefail
printf 'executed:%s target:%s\n' "$SSH_ORIGINAL_COMMAND" "$DEMO_DEPLOY_TARGET"
COMMAND
chmod 0755 "$REPO/deploy/teagram-demo/forced-command.sh" \
  "$REPO/deploy/teagram-demo/lima/deploy-root-wrapper.sh" \
  "$REPO/deploy/teagram-demo/lima/command-contract.sh"
install -m 0755 "$REPO/deploy/teagram-demo/lima/deploy-root-wrapper.sh" \
  /usr/local/sbin/teagram-demo-deploy
ssh-keygen -q -t ed25519 -N '' -f /tmp/fixture-key
ssh-keygen -q -t ed25519 -N '' -f /tmp/other-key
KEY_LINE="restrict,command=\"$ROOT_WRAPPER\" $(cat /tmp/fixture-key.pub)"
SECOND_KEY_LINE="restrict,command=\"$ROOT_WRAPPER\" $(cat /tmp/other-key.pub)"
install_key_file "$KEY_LINE"
install_key_file "$KEY_LINE"
[[ $(stat -c '%u:%a' "$AUTHORIZED_KEYS") == '0:600' ]]
if (install_key_file "$SECOND_KEY_LINE") > /tmp/key-rotation-refusal.out 2>&1; then
  printf 'installer accepted a different key over the existing deploy key\n' >&2
  exit 1
fi
grep -Fq 'deploy authorized keys file differs from the requested deploy key' \
  /tmp/key-rotation-refusal.out
[[ "$(<"$AUTHORIZED_KEYS")" == "$KEY_LINE" ]]
printf 'ok - installer refuses a different deploy key without changing the installed key\n'
ssh-keygen -A >/dev/null
sed -e 's#^ListenAddress .*#ListenAddress 127.0.0.1:2223#' \
  -e 's#^PidFile .*#PidFile /tmp/sshd.pid#' \
  /tmp/lima-sshd_config > /tmp/sshd_config
/usr/sbin/sshd -t -f /tmp/sshd_config
/usr/sbin/sshd -T -f /tmp/sshd_config > /tmp/sshd-effective
grep -Fqx 'allowusers root' /tmp/sshd-effective
grep -Fqx 'permitrootlogin forced-commands-only' /tmp/sshd-effective
grep -Fqx 'authorizedkeysfile /etc/ssh/authorized_keys/teagram-demo-deploy-root' /tmp/sshd-effective
grep -Fqx 'forcecommand /usr/local/sbin/teagram-demo-deploy' /tmp/sshd-effective
/usr/sbin/sshd -E /tmp/sshd.log -f /tmp/sshd_config
SHA=$(printf 'a%.0s' {1..40})
DIGEST=$(printf 'c%.0s' {1..64})
VALID_COMMAND="$SHA sha256:$DIGEST"
output=
for _ in {1..10}; do
  if output=$(ssh -F /dev/null -p 2223 -i /tmp/fixture-key -o IdentitiesOnly=yes \
    -o BatchMode=yes -o PreferredAuthentications=publickey \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    root@127.0.0.1 "$VALID_COMMAND" 2>/tmp/ssh.err); then
    break
  fi
  sleep 1
done
if [[ "$output" != "executed:$VALID_COMMAND target:lima" ]]; then
  cat /tmp/ssh.err /tmp/sshd.log >&2
fi
[[ "$output" == "executed:$VALID_COMMAND target:lima" ]]
if ssh -F /dev/null -p 2223 -i /tmp/fixture-key -o IdentitiesOnly=yes \
  -o BatchMode=yes -o PreferredAuthentications=publickey \
  -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  root@127.0.0.1 'bash -c id' >/tmp/ssh-invalid.out 2>/tmp/ssh-invalid.err; then
  printf 'SSH accepted a command outside the reviewed contract\n' >&2
  exit 1
fi
grep -Fq 'lima deploy command is not in the reviewed command contract' /tmp/ssh-invalid.err
if ssh -F /dev/null -p 2223 -i /tmp/other-key -o IdentitiesOnly=yes \
  -o BatchMode=yes -o PreferredAuthentications=publickey \
  -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  root@127.0.0.1 "$VALID_COMMAND" >/tmp/ssh-other-key.out 2>/tmp/ssh-other-key.err; then
  printf 'SSH accepted a key outside the deploy authorized-keys file\n' >&2
  exit 1
fi
[[ ! -s /tmp/ssh-other-key.out ]]
BASH

ALPINE_CONTAINER="teagram-lima-deploy-$RANDOM-$$"
alpine_test_status=0
alpine_test_stage=container
if ! docker run -d --name "$ALPINE_CONTAINER" alpine:3.22 sh -c 'sleep 300' >/dev/null; then
  alpine_test_status=1
else
  alpine_test_stage=package-install
  if ! docker exec "$ALPINE_CONTAINER" sh -c 'apk add --no-cache bash openssh' >/dev/null; then
    alpine_test_status=1
  else
    alpine_test_stage=copy-scripts
    if ! docker cp "$ALPINE_FUNCTIONS" "$ALPINE_CONTAINER:/tmp/alpine-functions.sh" ||
      ! docker cp "$SCRIPT_DIR/sshd_config" "$ALPINE_CONTAINER:/tmp/lima-sshd_config" ||
      ! docker cp "$SCRIPT_DIR/deploy-root-wrapper.sh" "$ALPINE_CONTAINER:/tmp/deploy-root-wrapper.sh" ||
      ! docker cp "$SCRIPT_DIR/command-contract.sh" "$ALPINE_CONTAINER:/tmp/command-contract.sh" ||
      ! docker cp "$TMP_DIR/alpine-deploy-entry-regression.sh" "$ALPINE_CONTAINER:/tmp/alpine-deploy-entry-regression.sh"; then
      alpine_test_status=1
    else
      alpine_test_stage=run-regression
      if ! docker exec "$ALPINE_CONTAINER" bash /tmp/alpine-deploy-entry-regression.sh \
        > "$TMP_DIR/alpine-deploy-entry.out" 2> "$TMP_DIR/alpine-deploy-entry.err"; then
        alpine_test_status=1
      fi
    fi
  fi
fi
if (( alpine_test_status == 0 )); then
  ok 'Alpine migration moves Stage 1 runtime paths, removes the legacy sudo account, and verifies root forced-command SSH'
else
  not_ok 'Alpine migration moves Stage 1 runtime paths, removes the legacy sudo account, and verifies root forced-command SSH'
  printf 'Alpine regression stage failed: %s\n' "$alpine_test_stage" >&2
  cat "$TMP_DIR/alpine-deploy-entry.out" "$TMP_DIR/alpine-deploy-entry.err" >&2 2>/dev/null || true
fi
docker rm -f "$ALPINE_CONTAINER" >/dev/null 2>&1 || true
ALPINE_CONTAINER=

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
charlie = (root / "deploy/teagram-demo/deploy-main.sh").read_text()
charlie_cleanup = charlie.split("demo_cleanup() {", 1)[1].split("\n}", 1)[0]
forced = (root / "deploy/teagram-demo/forced-command.sh").read_text()
checks = {
    "lima Compose has no candidate server": "teagramd-next:" not in compose,
    "maintenance window stops before migration and start": (
        rollout.index('lima_compose "$PREV_IMAGE_REF" stop teagramd')
        < rollout.index('lima_compose "$CANDIDATE_IMAGE_REF" run --rm -T --no-deps migrate')
        < rollout.index('lima_compose "$CANDIDATE_IMAGE_REF" up -d --no-deps --force-recreate')
    ),
    "verified database backup precedes the candidate rollout": (
        rollout.index("lima_backup_database\n") < rollout.index("lima_rollout_candidate\n")
    ),
    "new release health is checked through loopback /healthz": 'HEALTH_URL=http://127.0.0.1:8080/healthz' in rollout
    and 'lima_wait_health 60 || demo_fail' in rollout,
    "each candidate health probe is bounded by the remaining deadline": 'timeout "$remaining" wget -q -O /dev/null -T "$remaining" "$HEALTH_URL"' in rollout,
    "post-cutover image retention failure preserves the committed revision and succeeds": 'POST_COMMIT_RETENTION_FAILED=1' in rollout
    and 'POST_COMMIT_RETENTION_FAILED == 1' in cleanup and 'rc=0' in cleanup
    and 'image retention failed' in cleanup
    and 'rc == 75 && DEPLOY_COMMITTED == 0' in cleanup,
    "charlie reports post-cutover retention failures without rewinding the checkout": 'POST_CUTOVER_RETENTION_FAILED=1' in charlie
    and 'rc=75' in charlie_cleanup and 'checkout must stay on this revision' in charlie_cleanup
    and 'elif (( rc == 75 ))' in charlie_cleanup,
    "forced command skips checkout restore for committed deploy status": 'deploy_rc == 75' in forced
    and 'restore_checkout_after_deploy "$deploy_rc" "$previous"' in forced,
    "all writable proxy paths are outside the privileged checkout": all(
        f"/var/lib/teagram-demo/lima/runtime/{suffix}" in compose
        for suffix in ("tmp/turnstile-egress", "tmp/haproxy", "tmp/edge", "haproxy-run")
    ) and ".state/teagram-demo/tmp/" not in compose,
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

HEALTH_FUNCTION="$TMP_DIR/lima-health-function.sh"
sed -n '/^lima_wait_health() {$/,/^}/p' "$SCRIPT_DIR/deploy-main.sh" > "$HEALTH_FUNCTION"
SLOW_WGET_DIR="$TMP_DIR/slow-wget"
mkdir -p "$SLOW_WGET_DIR"
cat > "$SLOW_WGET_DIR/wget" <<'SH'
#!/bin/sh
printf '%s\n' "$*" > "$WGET_LOG"
exec sleep 10
SH
chmod 0755 "$SLOW_WGET_DIR/wget"
cat > "$TMP_DIR/lima-health-regression.sh" <<'BASH'
set -Eeuo pipefail
source "$HEALTH_FUNCTION"
HEALTH_URL=http://127.0.0.1:8080/healthz
SECONDS=0
started=$(date +%s)
if lima_wait_health 1; then
  printf 'slow health response unexpectedly passed\n' >&2
  exit 1
fi
elapsed=$(( $(date +%s) - started ))
[[ $elapsed -le 2 ]]
[[ "$(<"$WGET_LOG")" == '-q -O /dev/null -T 1 http://127.0.0.1:8080/healthz' ]]
BASH
if HEALTH_FUNCTION="$HEALTH_FUNCTION" WGET_LOG="$TMP_DIR/wget.log" \
  PATH="$SLOW_WGET_DIR:$PATH" bash "$TMP_DIR/lima-health-regression.sh"; then
  ok 'a slow health response receives the remaining timeout and cannot extend the rollout deadline'
else
  not_ok 'a slow health response receives the remaining timeout and cannot extend the rollout deadline'
fi

BACKUP_FUNCTIONS="$TMP_DIR/lima-backup-functions.sh"
{
  sed -n '/^lima_compose() {$/,/^}/p' "$SCRIPT_DIR/deploy-main.sh"
  sed -n '/^lima_backup_database() {$/,/^}/p' "$SCRIPT_DIR/deploy-main.sh"
} > "$BACKUP_FUNCTIONS"
BACKUP_BIN="$TMP_DIR/backup-bin"
mkdir -m 0700 "$BACKUP_BIN"
cat > "$BACKUP_BIN/docker" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
case "$*" in
  *' pg_dump -U postgres -Fc teagram')
    printf 'fake custom-format postgres dump\n'
    ;;
  *' pg_restore --list')
    input_bytes=$(wc -c)
    printf 'pg_restore stdin bytes=%s\n' "$input_bytes" >&2
    if (( input_bytes == 0 )); then
      printf 'pg_restore received empty stdin\n' >&2
      exit 1
    fi
    ;;
  *)
    printf 'unexpected docker invocation: %s\n' "$*" >&2
    exit 2
    ;;
esac
EOF
cat > "$BACKUP_BIN/chown" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod 0755 "$BACKUP_BIN/docker" "$BACKUP_BIN/chown"
cat > "$TMP_DIR/lima-backup-regression.sh" <<'BASH'
set -Eeuo pipefail
source "$LIMA_BACKUP_FUNCTIONS"
SCRIPT_DIR=$LIMA_SCRIPT_DIR
REPO_ROOT=$LIMA_REPO_ROOT
ENV_FILE=$LIMA_ENV_FILE
BACKUP_DIR=$LIMA_BACKUP_DIR
PREV_IMAGE_REF=previous-image
demo_fail() { printf '%s\n' "$1" >&2; return 1; }
lima_backup_database
[[ -s "$BACKUP_DIR/postgres.dump" && -s "$BACKUP_DIR/postgres.dump.sha256" ]]
[[ "$(stat -c '%a' "$BACKUP_DIR/postgres.dump")" == 600 ]]
BASH
BACKUP_DIR="$TMP_DIR/lima-backup"
mkdir -m 0700 "$BACKUP_DIR"
if LIMA_BACKUP_FUNCTIONS="$BACKUP_FUNCTIONS" LIMA_SCRIPT_DIR="$SCRIPT_DIR" \
  LIMA_REPO_ROOT="$REPO_ROOT" LIMA_ENV_FILE="$TMP_DIR/lima.env" \
  LIMA_BACKUP_DIR="$BACKUP_DIR" PATH="$BACKUP_BIN:$PATH" \
  bash "$TMP_DIR/lima-backup-regression.sh" > /dev/null 2> "$TMP_DIR/lima-backup.err"; then
  if grep -Eq '^pg_restore stdin bytes=[1-9][0-9]*$' "$TMP_DIR/lima-backup.err"; then
    ok 'database restore-list check receives the generated dump on stdin'
  else
    not_ok 'database restore-list check receives the generated dump on stdin'
  fi
else
  not_ok 'database restore-list check receives the generated dump on stdin'
fi

CLEANUP_FUNCTION="$TMP_DIR/lima-cleanup-function.sh"
sed -n '/^lima_cleanup() {$/,/^}/p' "$SCRIPT_DIR/deploy-main.sh" > "$CLEANUP_FUNCTION"

ROLLOUT_FUNCTION="$TMP_DIR/lima-rollout-function.sh"
sed -n '/^lima_rollout_candidate() {$/,/^}/p' "$SCRIPT_DIR/deploy-main.sh" > "$ROLLOUT_FUNCTION"
cat > "$TMP_DIR/lima-success-regression.sh" <<'BASH'
set -Eeuo pipefail
source "$ROLLOUT_FUNCTION"
source "$CLEANUP_FUNCTION"
SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
IMAGE_DIGEST=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
PREV_SHA=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
PREV_IMAGE_REVISION=$PREV_SHA
PREV_IMAGE_REF=previous-image
PREV_IMAGE_ID=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
PREV_WEB_RELEASE=
BACKUP_DIR=verified-backup
CANDIDATE_IMAGE_REF=
CANDIDATE_IMAGE_ID=
OLD_SERVER_STOPPED=0
CANDIDATE_STARTED=0
ACTIVE_RELEASE_UPDATED=0
DEPLOY_COMMITTED=0
POST_COMMIT_RETENTION_FAILED=0

demo_pull_image_by_sha() { printf '%s\n' "$LIMA_CANDIDATE_REF"; }
lima_compose() {
  printf 'compose %s %s\n' "$1" "${*:2}" >> "$LIMA_ROLLOUT_LOG"
  if [[ "${*:2}" == 'ps --status running -q teagramd' ]]; then
    printf 'candidate-container\n'
  fi
}
lima_wait_health() { printf 'health-check\n' >> "$LIMA_ROLLOUT_LOG"; }
docker() {
  printf 'docker %s\n' "$*" >> "$LIMA_ROLLOUT_LOG"
  case "$*" in
    "image inspect --format {{.Id}} $LIMA_CANDIDATE_REF"|"inspect --format {{.Image}} candidate-container")
      printf '%s\n' "$LIMA_CANDIDATE_ID"
      ;;
    *'org.opencontainers.image.revision'*) printf '%s\n' "$SHA" ;;
    *) printf 'unexpected docker invocation: %s\n' "$*" >&2; return 2 ;;
  esac
}
demo_write_active_release() {
  printf '%s %s\n' "$1" "$2" > "$LIMA_ACTIVE_RELEASE_FILE"
  printf 'active-release %s %s\n' "$1" "$2" >> "$LIMA_ROLLOUT_LOG"
}
demo_record_successful_image() { printf 'record-success\n' >> "$LIMA_ROLLOUT_LOG"; }
demo_prune_images() {
  printf 'prune %s\n' "$*" >> "$LIMA_ROLLOUT_LOG"
  if [[ ${FAIL_PRUNE:-0} == 1 ]]; then
    printf 'could not remove demo image %s\n' "$LIMA_OLD_IMAGE_ID" >&2
    return 1
  fi
}
demo_fail() { printf '%s\n' "$1" >&2; return 1; }
trap lima_cleanup EXIT

lima_rollout_candidate
[[ $OLD_SERVER_STOPPED == 1 && $CANDIDATE_STARTED == 1 &&
  $ACTIVE_RELEASE_UPDATED == 1 && $DEPLOY_COMMITTED == 1 ]]
BASH
ROLLOUT_LOG="$TMP_DIR/lima-success.log"
ACTIVE_RELEASE_FILE="$TMP_DIR/active-release"
LIMA_CANDIDATE_REF=ghcr.io/teagramhq/teagram-server-demo@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
LIMA_CANDIDATE_ID=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
if ROLLOUT_FUNCTION="$ROLLOUT_FUNCTION" CLEANUP_FUNCTION="$CLEANUP_FUNCTION" LIMA_ROLLOUT_LOG="$ROLLOUT_LOG" \
  LIMA_ACTIVE_RELEASE_FILE="$ACTIVE_RELEASE_FILE" LIMA_CANDIDATE_REF="$LIMA_CANDIDATE_REF" \
  LIMA_CANDIDATE_ID="$LIMA_CANDIDATE_ID" \
  bash "$TMP_DIR/lima-success-regression.sh" > "$TMP_DIR/lima-success.out" 2> "$TMP_DIR/lima-success.err"; then
  stop_line=$(grep -nF 'compose previous-image stop teagramd' "$ROLLOUT_LOG" | cut -d: -f1)
  migrate_line=$(grep -nF 'compose '"$LIMA_CANDIDATE_REF"' run --rm -T --no-deps migrate' "$ROLLOUT_LOG" | cut -d: -f1)
  start_line=$(grep -nF 'compose '"$LIMA_CANDIDATE_REF"' up -d --no-deps --force-recreate --wait --wait-timeout 120 teagramd' \
    "$ROLLOUT_LOG" | cut -d: -f1)
  health_line=$(grep -nFx 'health-check' "$ROLLOUT_LOG" | cut -d: -f1)
  release_line=$(grep -nF 'active-release teagramd '"$LIMA_CANDIDATE_REF" "$ROLLOUT_LOG" | cut -d: -f1)
  record_line=$(grep -nFx 'record-success' "$ROLLOUT_LOG" | cut -d: -f1)
  prune_line=$(grep -nF 'prune '"$LIMA_CANDIDATE_ID" "$ROLLOUT_LOG" | cut -d: -f1)
  if [[ -n "$stop_line" && -n "$migrate_line" && -n "$start_line" &&
    -n "$health_line" && -n "$release_line" && -n "$record_line" && -n "$prune_line" ]] &&
    (( stop_line < migrate_line && migrate_line < start_line && start_line < health_line &&
      health_line < release_line && release_line < record_line && record_line < prune_line )) &&
    grep -Fqx "teagramd $LIMA_CANDIDATE_REF" "$ACTIVE_RELEASE_FILE" &&
    grep -Fq 'lima demo deployed revision=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' "$TMP_DIR/lima-success.out"; then
    ok 'healthy candidate rollout exits zero and records the active release after health'
  else
    not_ok 'healthy candidate rollout exits zero and records the active release after health'
  fi
else
  not_ok 'healthy candidate rollout exits zero and records the active release after health'
fi

RETENTION_LOG="$TMP_DIR/lima-retention-failure.log"
ACTIVE_RELEASE_FILE="$TMP_DIR/active-release-retention-failure"
OLD_IMAGE_ID=sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
if ROLLOUT_FUNCTION="$ROLLOUT_FUNCTION" CLEANUP_FUNCTION="$CLEANUP_FUNCTION" LIMA_ROLLOUT_LOG="$RETENTION_LOG" \
  LIMA_ACTIVE_RELEASE_FILE="$ACTIVE_RELEASE_FILE" LIMA_CANDIDATE_REF="$LIMA_CANDIDATE_REF" \
  LIMA_CANDIDATE_ID="$LIMA_CANDIDATE_ID" LIMA_OLD_IMAGE_ID="$OLD_IMAGE_ID" FAIL_PRUNE=1 \
  bash "$TMP_DIR/lima-success-regression.sh" > "$TMP_DIR/lima-retention-failure.out" \
    2> "$TMP_DIR/lima-retention-failure.err"; then
  if grep -Fq "could not remove demo image $OLD_IMAGE_ID" "$TMP_DIR/lima-retention-failure.err" &&
    grep -Fq 'lima cutover remains committed at revision=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa; image retention failed, continuing with the verified release' \
      "$TMP_DIR/lima-retention-failure.err" &&
    grep -Fqx "teagramd $LIMA_CANDIDATE_REF" "$ACTIVE_RELEASE_FILE" &&
    grep -Fq 'lima demo deployed revision=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' "$TMP_DIR/lima-retention-failure.out" &&
    ! grep -Fq 'compose previous-image up' "$RETENTION_LOG"; then
    ok 'post-cutover retention failure reports the removal error and returns success for the committed deploy'
  else
    not_ok 'post-cutover retention failure reports the removal error and returns success for the committed deploy'
    cat "$TMP_DIR/lima-retention-failure.err" >&2
  fi
else
  not_ok 'post-cutover retention failure reports the removal error and returns success for the committed deploy'
  cat "$TMP_DIR/lima-retention-failure.err" >&2
fi

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
