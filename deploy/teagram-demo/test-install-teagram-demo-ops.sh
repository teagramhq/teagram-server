#!/usr/bin/env bash
set -Eeuo pipefail

[[ "$EUID" -eq 0 ]] || {
  printf '%s\n' 'demo ops installer fixture requires root' >&2
  exit 1
}

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
WORK_DIR=$(mktemp -d /tmp/teagram-demo-ops-installer-test.XXXXXXXX)
cleanup() {
  local status=$?
  trap - EXIT HUP INT TERM
  case "$WORK_DIR" in
    /tmp/teagram-demo-ops-installer-test.????????) rm -rf -- "$WORK_DIR" || status=1 ;;
    *) printf '%s\n' 'test refused to remove an unexpected temporary path' >&2; status=1 ;;
  esac
  exit "$status"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

FIXTURE_ROOT=$WORK_DIR/root
FIXTURE_REPO=$FIXTURE_ROOT/opt/teagram-demo
FIXTURE_SSH=$FIXTURE_ROOT/root/.ssh
FIXTURE_SBIN=$FIXTURE_ROOT/usr/local/sbin
FIXTURE_BACKUPS=$FIXTURE_ROOT/var/backups
ORIGIN=$WORK_DIR/origin.git
INSTALLER=$WORK_DIR/install-teagram-demo-ops.sh
ORIGINAL_KEYS=$WORK_DIR/authorized_keys.before
EXPECTED_KEYS=$WORK_DIR/authorized_keys.after

mkdir -p "$FIXTURE_ROOT/opt" "$FIXTURE_SSH" "$FIXTURE_SBIN" "$FIXTURE_BACKUPS"
chmod 0700 "$FIXTURE_SSH"
chmod 0755 "$FIXTURE_SBIN" "$FIXTURE_BACKUPS"

git init --bare --quiet "$ORIGIN"
git clone --quiet "$ORIGIN" "$FIXTURE_REPO" 2>/dev/null
git -C "$FIXTURE_REPO" checkout --quiet -b main
git -C "$FIXTURE_REPO" config user.name 'Ops Installer Test'
git -C "$FIXTURE_REPO" config user.email 'ops-installer-test@example.invalid'
mkdir -p "$FIXTURE_REPO/deploy/teagram-demo/ops"
cat >"$FIXTURE_REPO/deploy/teagram-demo/ops/teagram-demo-ops" <<'SH'
#!/bin/sh
set -eu
case "${SSH_ORIGINAL_COMMAND:-}" in
  hostname) printf '%s\n' 'demo-fixture-host' ;;
  *) printf '%s\n' 'usage: teagram-demo-ops' >&2; exit 1 ;;
esac
SH
chmod 0755 "$FIXTURE_REPO/deploy/teagram-demo/ops/teagram-demo-ops"
git -C "$FIXTURE_REPO" add deploy/teagram-demo/ops/teagram-demo-ops
git -C "$FIXTURE_REPO" commit --quiet -m 'test demo ops installer fixture'
git -C "$FIXTURE_REPO" push --quiet --set-upstream origin main
MAIN_SHA=$(git -C "$FIXTURE_REPO" rev-parse HEAD)

cat >"$FIXTURE_SSH/authorized_keys" <<'KEYS'
ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA retained-key
no-agent-forwarding,command="fixture-command" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAB retained-options
ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAC multica-daemon@delta
KEYS
chown root:root "$FIXTURE_SSH/authorized_keys"
chmod 0600 "$FIXTURE_SSH/authorized_keys"
cp -p "$FIXTURE_SSH/authorized_keys" "$ORIGINAL_KEYS"

TARGET_BEFORE='ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAC multica-daemon@delta'
TARGET_AFTER='restrict,command="/usr/local/sbin/teagram-demo-ops" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAC multica-daemon@delta'
grep -Fxq -- "$TARGET_BEFORE" "$ORIGINAL_KEYS"

cat >"$EXPECTED_KEYS" <<'KEYS'
ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA retained-key
no-agent-forwarding,command="fixture-command" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAB retained-options
restrict,command="/usr/local/sbin/teagram-demo-ops" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAC multica-daemon@delta
KEYS

sed \
  -e "s|^REPO=/opt/teagram-demo$|REPO=$FIXTURE_REPO|" \
  -e "s|^WRAPPER_PATH=/usr/local/sbin/teagram-demo-ops$|WRAPPER_PATH=$FIXTURE_SBIN/teagram-demo-ops|" \
  -e "s|^SSH_DIR=/root/.ssh$|SSH_DIR=$FIXTURE_SSH|" \
  -e "s|^BACKUP_DIR=/var/backups/teagram-demo-ops$|BACKUP_DIR=$FIXTURE_BACKUPS/teagram-demo-ops|" \
  -e "s|/usr/local/sbin ]|$FIXTURE_SBIN ]|g" \
  -e "s|/usr/local/sbin)|$FIXTURE_SBIN)|g" \
  -e "s|/usr/local/sbin/.teagram-demo-ops.|$FIXTURE_SBIN/.teagram-demo-ops.|g" \
  -e "s|/var/backups ]|$FIXTURE_BACKUPS ]|g" \
  -e "s|/var/backups)|$FIXTURE_BACKUPS)|g" \
  "$SCRIPT_DIR/ops/install-teagram-demo-ops.sh" >"$INSTALLER"

OUTPUT=$(sh "$INSTALLER" apply "$MAIN_SHA")
[[ "$OUTPUT" == *'key_state=restricted'* ]]
grep -Fxq -- "$TARGET_AFTER" "$FIXTURE_SSH/authorized_keys"
cmp -s "$EXPECTED_KEYS" "$FIXTURE_SSH/authorized_keys"
[[ "$(stat -c '%u:%g:%a' "$FIXTURE_SSH/authorized_keys")" == 0:0:600 ]]

BACKUP_DIR=$FIXTURE_BACKUPS/teagram-demo-ops
[[ "$(stat -c '%u:%g:%a' "$BACKUP_DIR")" == 0:0:700 ]]
mapfile -t BACKUPS < <(find "$BACKUP_DIR" -maxdepth 1 -type f -name 'authorized_keys.before.*' -print)
[[ "${#BACKUPS[@]}" -eq 1 ]]
BACKUP_FILE=${BACKUPS[0]}
[[ "$(stat -c '%u:%g:%a' "$BACKUP_FILE")" == 0:0:600 ]]
cmp -s "$ORIGINAL_KEYS" "$BACKUP_FILE"

printf '%s\n' 'demo ops installer backs up authorized_keys and restricts only the target key'
