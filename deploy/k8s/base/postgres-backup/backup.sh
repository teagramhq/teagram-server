#!/bin/sh
set -eu
umask 077

stamp=$(date -u +%Y%m%dT%H%M%SZ)
backup_uid=${POD_UID:-manual}
temporary="/backups/.teagram-${stamp}-${backup_uid}.dump.tmp"
destination="/backups/teagram-${stamp}-${backup_uid}.dump"
trap 'rm -f -- "$temporary"' EXIT HUP INT TERM

pg_dump --format=custom --no-owner --no-acl --file "$temporary"
pg_restore --list "$temporary" >/dev/null
chmod 0600 "$temporary"
mv -- "$temporary" "$destination"
bytes=$(wc -c < "$destination" | tr -d '[:space:]')
checksum=$(sha256sum "$destination" | awk '{print $1}')
printf 'backup=verified path=%s bytes=%s sha256=%s\n' \
  "$destination" "$bytes" "$checksum"

printf '%s\n' /backups/teagram-*.dump \
  | sort -r \
  | tail -n +8 \
  | while IFS= read -r existing; do
      [ -f "$existing" ] && rm -f -- "$existing"
    done
printf 'backup=retention-complete retained=7\n'
