#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

readonly ACCOUNTS_FILE=/home/qa/.qa-accounts-invite
readonly LOCK_FILE=/home/qa/.qa-accounts-invite.lock
readonly ORIGINAL_COMMAND=${SSH_ORIGINAL_COMMAND:-}

fail() {
  printf '%s\n' 'qa-invite account writer rejected the request' >&2
  exit 1
}

valid_handle() { [[ "$1" =~ ^[a-z][a-z0-9_]{1,31}$ ]]; }
valid_password() { [[ "$1" =~ ^[0-9a-f]{64}$ ]]; }

[[ "$(id -un)" == qa && "$(id -u)" -eq 1000 ]] || fail
[[ "$ORIGINAL_COMMAND" == health || "$ORIGINAL_COMMAND" == store || "$ORIGINAL_COMMAND" =~ ^remove\ [a-z][a-z0-9_]{1,31}$ ]] || fail

if [[ ! -e "$ACCOUNTS_FILE" && ! -L "$ACCOUNTS_FILE" ]]; then
  install -m 0600 -o qa -g qa /dev/null "$ACCOUNTS_FILE"
else
  [[ -f "$ACCOUNTS_FILE" && ! -L "$ACCOUNTS_FILE" && "$(stat -c '%u:%a' "$ACCOUNTS_FILE")" == '1000:600' ]] || fail
fi
if [[ -e "$LOCK_FILE" || -L "$LOCK_FILE" ]]; then
  [[ -f "$LOCK_FILE" && ! -L "$LOCK_FILE" && "$(stat -c '%u:%a' "$LOCK_FILE")" == '1000:600' ]] || fail
else
  install -m 0600 -o qa -g qa /dev/null "$LOCK_FILE"
fi
exec 9>> "$LOCK_FILE"
flock -x 9

if [[ "$ORIGINAL_COMMAND" == health ]]; then
  printf '%s\n' 'qa-account-writer=ready'
  exit 0
fi

if [[ "$ORIGINAL_COMMAND" == store ]]; then
  IFS=$'\t' read -r handle password extra || fail
  [[ -z "${extra:-}" ]] || fail
  valid_handle "$handle" && valid_password "$password" || fail
  ! awk -F '\t' -v handle="$handle" '$1 == handle { found=1 } END { exit !found }' "$ACCOUNTS_FILE" || fail
  temp=$(mktemp /home/qa/.qa-accounts-invite.XXXXXXXX)
  trap 'rm -f -- "$temp"' EXIT
  cat -- "$ACCOUNTS_FILE" > "$temp"
  printf '%s\t%s\n' "$handle" "$password" >> "$temp"
  chown qa:qa "$temp"
  chmod 0600 "$temp"
  mv -f -- "$temp" "$ACCOUNTS_FILE"
  printf 'stored=%s\n' "$handle"
  exit 0
fi

handle=${ORIGINAL_COMMAND#remove }
valid_handle "$handle" || fail
temp=$(mktemp /home/qa/.qa-accounts-invite.XXXXXXXX)
trap 'rm -f -- "$temp"' EXIT
awk -F '\t' -v handle="$handle" '$1 != handle { print $0 }' "$ACCOUNTS_FILE" > "$temp"
chown qa:qa "$temp"
chmod 0600 "$temp"
mv -f -- "$temp" "$ACCOUNTS_FILE"
printf 'removed=%s\n' "$handle"
