#!/bin/sh
set -eu
umask 077

backup_directory=${1:-/backups}
mode=${2:-run}
if [ "$#" -gt 2 ] || { [ "$mode" != run ] && [ "$mode" != --prune-only ]; }; then
  printf 'usage: %s [dump-directory] [--prune-only]\n' "$0" >&2
  exit 2
fi
if [ ! -d "$backup_directory" ]; then
  printf 'backup directory does not exist: %s\n' "$backup_directory" >&2
  exit 1
fi

find "$backup_directory" -maxdepth 1 -type f \
  -name '.teagram-*.dump.tmp' -mmin +30 -exec rm -f -- {} +

prune_backups() (
  cd "$backup_directory"
  printf '%s\n' teagram-*.dump \
    | sort -r \
    | tail -n +8 \
    | while IFS= read -r existing; do
        [ -f "$existing" ] && rm -f -- "$existing"
      done
)

if [ "$mode" = --prune-only ]; then
  prune_backups
  printf 'backup=retention-complete retained=7\n'
  exit 0
fi

stamp=$(date -u +%Y%m%dT%H%M%SZ)
backup_uid=${POD_UID:-manual}
temporary="$backup_directory/.teagram-${stamp}-${backup_uid}.dump.tmp"
destination="$backup_directory/teagram-${stamp}-${backup_uid}.dump"
trap 'rm -f -- "$temporary"' EXIT HUP INT TERM

pg_dump --format=custom --no-owner --no-acl --file "$temporary"
pg_restore --list "$temporary" >/dev/null
chmod 0600 "$temporary"
mv -- "$temporary" "$destination"
bytes=$(wc -c < "$destination" | tr -d '[:space:]')
checksum=$(sha256sum "$destination" | awk '{print $1}')
printf 'backup=verified path=%s bytes=%s sha256=%s\n' \
  "$destination" "$bytes" "$checksum"

prune_backups
printf 'backup=retention-complete retained=7\n'
