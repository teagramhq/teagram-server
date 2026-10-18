#!/bin/sh
set -eu

require_secret_file() {
	name=$1
	path=$2
	if [ ! -s "$path" ]; then
		printf '%s is required\n' "$name" >&2
		exit 1
	fi
}

: "${TG_BLOB_S3_ACCESS_KEY_ID:?TG_BLOB_S3_ACCESS_KEY_ID is required}"
require_secret_file RUSTFS_ROOT_ACCESS_KEY /run/secrets/rustfs-root-access-key
require_secret_file RUSTFS_ROOT_SECRET_KEY /run/secrets/rustfs-root-secret-key
require_secret_file TG_BLOB_S3_SECRET_ACCESS_KEY_FILE /run/secrets/teagramd-blob-secret-key

mc alias set local http://rustfs:9000 \
	"$(cat /run/secrets/rustfs-root-access-key)" \
	"$(cat /run/secrets/rustfs-root-secret-key)" >/dev/null
mc mb --ignore-existing local/teagram >/dev/null
mc anonymous set private local/teagram >/dev/null
mc admin policy create local teagramd-blob /policy/teagramd-blob.json >/dev/null
if ! mc admin user info local "$TG_BLOB_S3_ACCESS_KEY_ID" >/dev/null 2>&1; then
	mc admin user add local "$TG_BLOB_S3_ACCESS_KEY_ID" \
		"$(cat /run/secrets/teagramd-blob-secret-key)" >/dev/null
fi
mc admin policy attach local teagramd-blob --user "$TG_BLOB_S3_ACCESS_KEY_ID" >/dev/null
