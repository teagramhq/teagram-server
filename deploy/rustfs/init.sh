#!/bin/sh
set -eu

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
