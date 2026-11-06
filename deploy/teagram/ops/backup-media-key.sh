#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

readonly KEY_DIR=/etc/teagramd/media-keys
readonly KEY_FILE="$KEY_DIR/1.key"
readonly BACKUP_PARENT=/etc/teagramd
readonly BACKUP_DIR="$BACKUP_PARENT/media-key-backups"
readonly BACKUP_FILE="$BACKUP_DIR/1.key"

fail() { printf 'media key backup rejected: %s\n' "$1" >&2; exit 1; }
[[ "$(id -u)" == 0 ]] || fail 'root is required'
[[ "${OPS_SHA:-}" =~ ^[0-9a-f]{40}$ ]] || fail 'reviewed source SHA is invalid'
[[ "$#" == 0 ]] || fail 'no arguments are accepted'
[[ -d "$BACKUP_PARENT" && ! -L "$BACKUP_PARENT" && "$(stat -c '%u' -- "$BACKUP_PARENT")" == 0 ]] || fail 'backup parent is unsafe'
parent_mode=$(stat -c '%a' -- "$BACKUP_PARENT") || fail 'cannot inspect backup parent'
[[ "$parent_mode" =~ ^[0-7]{3,4}$ ]] && (( (8#$parent_mode & 0022) == 0 )) || fail 'backup parent is writable by others'
[[ -d "$KEY_DIR" && ! -L "$KEY_DIR" && "$(stat -c '%u:%a:%g' -- "$KEY_DIR")" == 65532:700:65532 ]] || fail 'live key directory is unsafe'
[[ -f "$KEY_FILE" && ! -L "$KEY_FILE" && "$(stat -c '%u:%a:%g' -- "$KEY_FILE")" == 65532:600:65532 ]] || fail "live key $KEY_FILE is missing or unsafe; expected owner:mode:group 65532:600:65532"
if [[ ! -e "$BACKUP_DIR" && ! -L "$BACKUP_DIR" ]]; then
  mkdir -m 700 -- "$BACKUP_DIR" || fail 'cannot create backup directory'
fi
[[ -d "$BACKUP_DIR" && ! -L "$BACKUP_DIR" && "$(stat -c '%u:%a' -- "$BACKUP_DIR")" == 0:700 ]] || fail 'backup directory is unsafe'
[[ ! -e "$BACKUP_FILE" && ! -L "$BACKUP_FILE" ]] || fail 'backup already exists; refusing to overwrite'

staged=$(mktemp "$BACKUP_DIR/.key.XXXXXXXX") || fail 'cannot stage backup'
cleanup() { rm -f -- "$staged"; }
trap cleanup EXIT
cat -- "$KEY_FILE" > "$staged" || fail 'cannot copy live key'
chmod 600 -- "$staged" || fail 'cannot protect backup'
live_digest=$(sha256sum < "$KEY_FILE") || fail 'cannot fingerprint live key'
backup_digest=$(sha256sum < "$staged") || fail 'cannot fingerprint backup'
[[ "$live_digest" == "$backup_digest" ]] || fail 'backup differs from live key'
sync || fail 'cannot sync backup'
ln -- "$staged" "$BACKUP_FILE" || fail 'cannot publish backup without overwrite'
printf 'media_key_backup=verified path=%s\n' "$BACKUP_FILE"
