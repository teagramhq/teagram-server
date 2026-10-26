#!/bin/sh
set -eu

PATH=/usr/sbin:/usr/bin:/sbin:/bin
IFS=' '
umask 077
export PATH IFS
unset CDPATH ENV BASH_ENV GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_CONFIG \
  GIT_CONFIG_COUNT GIT_CONFIG_PARAMETERS GIT_CONFIG_SYSTEM GIT_CONFIG_GLOBAL \
  GIT_CONFIG_KEY_0 GIT_CONFIG_VALUE_0 GIT_SSH_COMMAND GIT_TRACE
HOME=/root
export HOME

REPO=/opt/teagram-demo
WRAPPER_PATH=/usr/local/sbin/teagram-demo-ops
WRAPPER_SOURCE=deploy/teagram-demo/ops/teagram-demo-ops
SSH_DIR=/root/.ssh
AUTHORIZED_KEYS=$SSH_DIR/authorized_keys
BACKUP_DIR=/var/backups/teagram-demo-ops
EXPECTED_OPTIONS='restrict,command="/usr/local/sbin/teagram-demo-ops"'
KEY_COMMENT=multica-daemon@delta
MAIN_REF=refs/remotes/origin/main
WRAPPER_TEMP=
AUTH_TEMP=
CHECK_BEFORE=
CHECK_AFTER=
BACKUP_FILE=

fail() {
  printf 'demo ops install: %s\n' "$1" >&2
  if [ -n "$BACKUP_FILE" ]; then printf 'authorized_keys backup=%s\n' "$BACKUP_FILE" >&2; fi
  exit 1
}

cleanup() {
  status=$?
  trap - EXIT HUP INT TERM
  for path in "$WRAPPER_TEMP" "$AUTH_TEMP" "$CHECK_BEFORE" "$CHECK_AFTER"; do
    [ -n "$path" ] || continue
    rm -f -- "$path" || status=1
  done
  exit "$status"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

valid_sha() {
  [ "${#1}" -eq 40 ] || return 1
  case "$1" in *[!a-f0-9]*) return 1 ;; esac
}

key_state() {
  awk -v expected="$EXPECTED_OPTIONS" -v comment="$KEY_COMMENT" '
    {
      key_field = 0
      for (i = 1; i < NF; i++) {
        if ($i == "ssh-ed25519") {
          key_field = i
          break
        }
      }
      if (key_field > 0 && $(key_field + 2) == comment) {
        count++
        if (key_field == 1 && NF == 3) state = "unrestricted"
        else if (key_field == 2 && NF == 4 && $1 == expected) state = "restricted"
        else state = "invalid"
      }
    }
    END {
      if (count != 1 || state == "invalid" || state == "") exit 1
      print state
    }
  ' "$AUTHORIZED_KEYS"
}

rewrite_keys() {
  awk -v expected="$EXPECTED_OPTIONS" -v comment="$KEY_COMMENT" '
    {
      key_field = 0
      for (i = 1; i < NF; i++) {
        if ($i == "ssh-ed25519") {
          key_field = i
          break
        }
      }
      if (key_field > 0 && $(key_field + 2) == comment) {
        count++
        if (key_field == 1 && NF == 3) {
          print expected " " $1 " " $2 " " $3
          next
        }
        if (key_field == 2 && NF == 4 && $1 == expected) {
          print $0
          next
        }
        exit 1
      }
      print $0
    }
    END { if (count != 1) exit 1 }
  ' "$AUTHORIZED_KEYS"
}

[ "$(id -u)" -eq 0 ] || fail 'installation must run as root'
case "$#" in
  2) ACTION=$1; SHA=$2 ;;
  0) ACTION=apply; SHA=${OPS_SHA:-} ;;
  *) fail 'Usage: install-teagram-demo-ops.sh [check|apply] <main-sha>' ;;
esac
case "$ACTION" in check|apply) ;; *) fail 'action must be check or apply' ;; esac
valid_sha "$SHA" || fail 'commit must be a full lowercase 40-hex SHA'
[ -d "$REPO/.git" ] && [ ! -L "$REPO" ] || fail 'demo checkout is unavailable'
git -C "$REPO" fetch --quiet --no-tags origin "+refs/heads/main:$MAIN_REF" \
  || fail 'could not fetch main from origin'
git -C "$REPO" merge-base --is-ancestor "$SHA" "$MAIN_REF" \
  || fail "commit $SHA is not on origin/main"

entry=$(git -C "$REPO" ls-tree "$SHA" -- "$WRAPPER_SOURCE" | awk '{ print $1 " " $2 " " $3 }')
# shellcheck disable=SC2086
set -- $entry
[ "$#" -eq 3 ] && [ "$1" = 100755 ] && [ "$2" = blob ] \
  || fail 'main commit does not contain an executable ops wrapper'
WRAPPER_BLOB=$3
[ -f "$AUTHORIZED_KEYS" ] && [ ! -L "$AUTHORIZED_KEYS" ] \
  || fail 'root authorized_keys is missing or a symbolic link'
[ "$(stat -c '%u:%g:%a' "$SSH_DIR")" = 0:0:700 ] \
  || fail 'root SSH directory must be root-owned mode 0700'
[ "$(stat -c '%u:%g:%a' "$AUTHORIZED_KEYS")" = 0:0:600 ] \
  || fail 'authorized_keys must be root-owned mode 0600'
KEY_STATE=$(key_state) || fail 'expected exactly one unrestricted or already restricted multica-daemon@delta Ed25519 entry'
[ -d /usr/local/sbin ] && [ ! -L /usr/local/sbin ] \
  || fail '/usr/local/sbin is missing or a symbolic link'
[ "$(stat -c '%u:%g' /usr/local/sbin)" = 0:0 ] || fail '/usr/local/sbin must be root-owned'
mode=$(stat -c '%a' /usr/local/sbin) || fail 'could not inspect /usr/local/sbin permissions'
[ $((0$mode & 0022)) -eq 0 ] || fail '/usr/local/sbin must not be group- or world-writable'
if [ -e "$WRAPPER_PATH" ] || [ -L "$WRAPPER_PATH" ]; then
  [ -f "$WRAPPER_PATH" ] && [ ! -L "$WRAPPER_PATH" ] \
    || fail 'existing wrapper path is not a regular file'
fi
[ -d /var/backups ] && [ ! -L /var/backups ] || fail '/var/backups is missing or a symbolic link'
[ "$(stat -c '%u:%g' /var/backups)" = 0:0 ] || fail '/var/backups must be root-owned'
backup_parent_mode=$(stat -c '%a' /var/backups) || fail 'could not inspect /var/backups permissions'
[ $((0$backup_parent_mode & 0022)) -eq 0 ] || fail '/var/backups must not be group- or world-writable'
if [ -e "$BACKUP_DIR" ] || [ -L "$BACKUP_DIR" ]; then
  [ -d "$BACKUP_DIR" ] && [ ! -L "$BACKUP_DIR" ] \
    || fail 'authorized_keys backup path is not a directory'
  [ "$(stat -c '%u:%g:%a' "$BACKUP_DIR")" = 0:0:700 ] \
    || fail 'the authorized_keys backup directory must be root-only'
fi
if [ "$ACTION" = check ]; then
  WRAPPER_TEMP=$(mktemp /tmp/teagram-demo-ops-check.XXXXXXXX) \
    || fail 'could not create a temporary wrapper check'
else
  WRAPPER_TEMP=$(mktemp /usr/local/sbin/.teagram-demo-ops.XXXXXXXX) \
    || fail 'could not create a temporary wrapper'
fi
git -C "$REPO" show "$SHA:$WRAPPER_SOURCE" >"$WRAPPER_TEMP" \
  || fail 'could not read the reviewed wrapper from main'
chown root:root "$WRAPPER_TEMP" || fail 'could not secure the temporary wrapper'
chmod 0755 "$WRAPPER_TEMP" || fail 'could not set the wrapper mode'
sh -n "$WRAPPER_TEMP" || fail 'reviewed wrapper has invalid shell syntax'
probe=$(SSH_ORIGINAL_COMMAND=hostname "$WRAPPER_TEMP") \
  || fail 'reviewed wrapper did not pass its hostname smoke check'
[ -n "$probe" ] || fail 'reviewed wrapper returned an empty hostname'
if output=$(SSH_ORIGINAL_COMMAND='bash -c id' "$WRAPPER_TEMP" 2>&1); then
  fail 'reviewed wrapper accepted an arbitrary shell command'
fi
case "$output" in *'usage: teagram-demo-ops'*) ;; *) fail 'reviewed wrapper did not return its usage text for an arbitrary command' ;; esac
installed_blob=$(git hash-object "$WRAPPER_TEMP") || fail 'could not fingerprint the reviewed wrapper'
[ "$installed_blob" = "$WRAPPER_BLOB" ] || fail 'temporary wrapper does not match the reviewed main blob'
if [ "$ACTION" = check ]; then
  printf 'would_install=%s\nkey_state=%s\nbackup_directory=%s\n' \
    "$WRAPPER_PATH" "$KEY_STATE" "$BACKUP_DIR"
  exit 0
fi
mv -f -- "$WRAPPER_TEMP" "$WRAPPER_PATH" || fail 'could not install the root-owned wrapper'
WRAPPER_TEMP=

if [ "$KEY_STATE" = unrestricted ]; then
  install -d -o root -g root -m 0700 "$BACKUP_DIR" \
    || fail 'could not create the root-only backup directory under /var/backups'
  [ "$(stat -c '%u:%g:%a' "$BACKUP_DIR")" = 0:0:700 ] \
    || fail 'the authorized_keys backup directory must be root-only'
  BACKUP_FILE=$(mktemp "$BACKUP_DIR/authorized_keys.before.XXXXXXXX") \
    || fail 'could not create an authorized_keys backup'
  cp -p "$AUTHORIZED_KEYS" "$BACKUP_FILE" || fail 'could not copy authorized_keys to its backup'
  chown root:root "$BACKUP_FILE" || fail 'could not secure the authorized_keys backup owner'
  chmod 0600 "$BACKUP_FILE" || fail 'could not secure the authorized_keys backup mode'
  cmp -s "$AUTHORIZED_KEYS" "$BACKUP_FILE" || fail 'authorized_keys backup does not match the live file'
  AUTH_TEMP=$(mktemp "$SSH_DIR/authorized_keys.XXXXXXXX") \
    || fail 'could not create a temporary authorized_keys file'
  rewrite_keys >"$AUTH_TEMP" || fail 'could not restrict only the multica-daemon@delta key'
  chown root:root "$AUTH_TEMP" || fail 'could not secure the updated authorized_keys owner'
  chmod 0600 "$AUTH_TEMP" || fail 'could not set the updated authorized_keys mode'
  mv -f -- "$AUTH_TEMP" "$AUTHORIZED_KEYS" || fail 'could not activate the restricted key'
  AUTH_TEMP=
  CHECK_BEFORE=$(mktemp /tmp/teagram-demo-keys-before.XXXXXXXX) \
    || fail 'could not create the authorized_keys verification file'
  CHECK_AFTER=$(mktemp /tmp/teagram-demo-keys-after.XXXXXXXX) \
    || fail 'could not create the authorized_keys verification file'
  awk -v comment="$KEY_COMMENT" '$NF != comment { print $0 }' "$BACKUP_FILE" >"$CHECK_BEFORE"
  awk -v comment="$KEY_COMMENT" '$NF != comment { print $0 }' "$AUTHORIZED_KEYS" >"$CHECK_AFTER"
  cmp -s "$CHECK_BEFORE" "$CHECK_AFTER" || fail 'an unrelated authorized_keys entry changed'
  printf 'backup=%s\n' "$BACKUP_FILE"
else
  printf '%s\n' 'backup=not-needed-key-was-already-restricted'
fi

[ "$(key_state)" = restricted ] || fail 'multica-daemon@delta is not restricted to the ops wrapper'
[ "$(stat -c '%u:%g:%a' "$WRAPPER_PATH")" = 0:0:755 ] \
  || fail 'installed wrapper must be root-owned mode 0755'
printf 'wrapper=%s\nkey_state=restricted\n' "$WRAPPER_PATH"
