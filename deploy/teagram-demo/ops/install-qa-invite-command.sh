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
WRAPPER_SOURCE=deploy/teagram-demo/forced-command.sh
WRAPPER_PATH=/usr/local/sbin/teagram-demo-forced-command
OLD_OPTIONS='command="/opt/teagram-demo/deploy/teagram-demo/forced-command.sh",restrict'
NEW_OPTIONS='command="/usr/local/sbin/teagram-demo-forced-command",restrict'
SSH_DIR=/root/.ssh
AUTHORIZED_KEYS=$SSH_DIR/authorized_keys
BACKUP_DIR=/var/backups/teagram-demo-deploy
MAIN_REF=refs/remotes/origin/main
WRAPPER_TEMP=
AUTH_TEMP=
BACKUP_FILE=

fail() {
  printf 'qa-invite command install: %s\n' "$1" >&2
  if [ -n "$BACKUP_FILE" ]; then printf 'authorized_keys backup=%s\n' "$BACKUP_FILE" >&2; fi
  exit 1
}

cleanup() {
  status=$?
  trap - EXIT HUP INT TERM
  for path in "$WRAPPER_TEMP" "$AUTH_TEMP"; do
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

command_state() {
  awk -v old="$OLD_OPTIONS" -v new="$NEW_OPTIONS" '
    {
      key_field = 0
      for (i = 1; i < NF; i++) if ($i == "ssh-ed25519") { key_field = i; break }
      if (key_field == 0 || $(key_field + 2) != "teagram-demo-deploy") next
      count++
      if (key_field != 2 || NF != 4) state = "invalid"
      else if ($1 == old) state = "old"
      else if ($1 == new) state = "new"
      else state = "invalid"
    }
    END { if (count != 1 || state == "invalid" || state == "") exit 1; print state }
  ' "$AUTHORIZED_KEYS"
}

rewrite_keys() {
  awk -v old="$OLD_OPTIONS" -v new="$NEW_OPTIONS" '
    {
      key_field = 0
      for (i = 1; i < NF; i++) if ($i == "ssh-ed25519") { key_field = i; break }
      if (key_field > 0 && $(key_field + 2) == "teagram-demo-deploy") {
        count++
        if (key_field != 2 || NF != 4 || ($1 != old && $1 != new)) exit 1
        print new " " $2 " " $3 " " $4
        next
      }
      print $0
    }
    END { if (count != 1) exit 1 }
  ' "$AUTHORIZED_KEYS"
}

[ "$(id -u)" -eq 0 ] || fail 'must run through the restricted demo ops menu'
case "$#" in
  0) SHA=${OPS_SHA:-} ;;
  1) SHA=$1 ;;
  *) fail 'usage: install-qa-invite-command.sh [main-sha]' ;;
esac
valid_sha "$SHA" || fail 'commit must be a full lowercase 40-hex SHA'
[ -d "$REPO/.git" ] && [ ! -L "$REPO" ] || fail 'demo checkout is unavailable'
[ -d "$REPO/.state" ] && [ ! -L "$REPO/.state" ] || fail 'demo state directory is unavailable'
LOCK_PATH=$REPO/.state/deploy.lock
[ ! -L "$LOCK_PATH" ] || fail 'demo deploy lock must not be a symbolic link'
exec 9> "$LOCK_PATH"
flock -x 9 || fail 'could not acquire the demo deployment lock'
git -C "$REPO" fetch --quiet --no-tags origin "+refs/heads/main:$MAIN_REF" || fail 'could not fetch main'
git -C "$REPO" merge-base --is-ancestor "$SHA" "$MAIN_REF" || fail 'requested commit is not on main'
entry=$(git -C "$REPO" ls-tree "$SHA" -- "$WRAPPER_SOURCE" | awk '{ print $1 " " $2 " " $3 }')
# shellcheck disable=SC2086
set -- $entry
[ "$#" -eq 3 ] && [ "$1" = 100755 ] && [ "$2" = blob ] || fail 'requested commit has no executable forced command'
WRAPPER_BLOB=$3
main_blob=$(git -C "$REPO" rev-parse "$MAIN_REF:$WRAPPER_SOURCE") || fail 'current main forced command is missing'
[ "$WRAPPER_BLOB" = "$main_blob" ] || fail 'forced command does not match the current main revision'
[ -f "$AUTHORIZED_KEYS" ] && [ ! -L "$AUTHORIZED_KEYS" ] || fail 'root authorized_keys is unavailable'
[ "$(stat -c '%u:%g:%a' "$SSH_DIR")" = 0:0:700 ] || fail 'root SSH directory must be root-owned mode 0700'
[ "$(stat -c '%u:%g:%a' "$AUTHORIZED_KEYS")" = 0:0:600 ] || fail 'authorized_keys must be root-owned mode 0600'
[ -d /usr/local/sbin ] && [ ! -L /usr/local/sbin ] || fail '/usr/local/sbin is unavailable'
[ "$(stat -c '%u:%g' /usr/local/sbin)" = 0:0 ] || fail '/usr/local/sbin must be root-owned'
mode=$(stat -c '%a' /usr/local/sbin) || fail 'could not inspect /usr/local/sbin'
[ $((0$mode & 0022)) -eq 0 ] || fail '/usr/local/sbin must not be group- or world-writable'
[ -d /var/backups ] && [ ! -L /var/backups ] || fail '/var/backups is unavailable'
[ "$(stat -c '%u:%g' /var/backups)" = 0:0 ] || fail '/var/backups must be root-owned'
mode=$(stat -c '%a' /var/backups) || fail 'could not inspect /var/backups'
[ $((0$mode & 0022)) -eq 0 ] || fail '/var/backups must not be group- or world-writable'

KEY_STATE=$(command_state) || fail 'demo deploy key entry is missing, ambiguous, or not restricted'
if [ -e "$WRAPPER_PATH" ] || [ -L "$WRAPPER_PATH" ]; then
  [ -f "$WRAPPER_PATH" ] && [ ! -L "$WRAPPER_PATH" ] || fail 'stable forced command path is unsafe'
fi
WRAPPER_TEMP=$(mktemp /usr/local/sbin/.teagram-demo-forced-command.XXXXXXXX) || fail 'could not stage the forced command'
git -C "$REPO" show "$SHA:$WRAPPER_SOURCE" > "$WRAPPER_TEMP" || fail 'could not read the reviewed forced command'
chown root:root "$WRAPPER_TEMP"
chmod 0755 "$WRAPPER_TEMP"
bash -n "$WRAPPER_TEMP" || fail 'reviewed forced command has invalid syntax'
[ "$(git hash-object "$WRAPPER_TEMP")" = "$WRAPPER_BLOB" ] || fail 'staged forced command differs from the reviewed main blob'

if [ "$KEY_STATE" = old ]; then
  install -d -o root -g root -m 0700 "$BACKUP_DIR" || fail 'could not create the root-only backup directory'
  [ "$(stat -c '%u:%g:%a' "$BACKUP_DIR")" = 0:0:700 ] || fail 'backup directory must be root-only'
  BACKUP_FILE=$(mktemp "$BACKUP_DIR/authorized_keys.before.XXXXXXXX") || fail 'could not create the authorized_keys backup'
  cp -p "$AUTHORIZED_KEYS" "$BACKUP_FILE" || fail 'could not copy authorized_keys'
  chown root:root "$BACKUP_FILE"
  chmod 0600 "$BACKUP_FILE"
  cmp -s "$AUTHORIZED_KEYS" "$BACKUP_FILE" || fail 'authorized_keys backup differs from the live file'
fi

mv -f -- "$WRAPPER_TEMP" "$WRAPPER_PATH" || fail 'could not install the stable forced command'
WRAPPER_TEMP=
if [ "$KEY_STATE" = old ]; then
  AUTH_TEMP=$(mktemp "$SSH_DIR/authorized_keys.XXXXXXXX") || fail 'could not stage authorized_keys'
  rewrite_keys > "$AUTH_TEMP" || fail 'could not update only the deploy key command'
  chown root:root "$AUTH_TEMP"
  chmod 0600 "$AUTH_TEMP"
  mv -f -- "$AUTH_TEMP" "$AUTHORIZED_KEYS" || fail 'could not activate the deploy key command'
  AUTH_TEMP=
fi
[ "$(command_state)" = new ] || fail 'deploy key does not use the stable forced command'
[ "$(stat -c '%u:%g:%a' "$WRAPPER_PATH")" = 0:0:755 ] || fail 'installed forced command has unsafe ownership or mode'
printf 'forced_command=%s\nkey_state=restricted\n' "$WRAPPER_PATH"
