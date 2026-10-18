#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -P "$SCRIPT_DIR/../../.." && pwd)
TMP=$(mktemp -d "${TMPDIR:-/tmp}/teagramd-local-blobs-compose.XXXXXXXX")
trap 'rm -rf -- "$TMP"' EXIT

cat >"$TMP/no-blob-credentials.env" <<'ENV'
POSTGRES_PASSWORD=fixture-password
TG_PUBLIC_LINK_PREFIX=https://links.example.test/
ENV

compose() {
  env -i PATH="$PATH" HOME="${HOME:-/root}" docker compose \
    --project-name local-blobs-credential-check \
    --project-directory "$REPO_ROOT" \
    --env-file "$TMP/no-blob-credentials.env" "$@"
}

compose \
  --file "$REPO_ROOT/docker-compose.yml" \
  --file "$REPO_ROOT/docker-compose.local-blobs.yml" \
  config --format json >"$TMP/local.json"
jq -e '
  .services.teagramd.environment.TG_BLOB_DIR == "/var/lib/teagramd-blobs"
  and ([.services.teagramd.environment | keys[] | select(startswith("TG_BLOB_S3_"))] | length) == 0
  and ((.services.teagramd.secrets // []) | length) == 0
  and ((.services.teagramd.depends_on | keys) == ["migrate"])
  and ((.services | has("rustfs")) | not)
  and ((.services | has("rustfs-init")) | not)
' "$TMP/local.json" >/dev/null

compose --file "$REPO_ROOT/docker-compose.yml" config --format json >"$TMP/s3.json"
jq -e '
  .services.teagramd.environment.TG_BLOB_S3_ENDPOINT == "http://rustfs:9000"
  and .services.teagramd.environment.TG_BLOB_S3_ACCESS_KEY_ID == ""
  and .services["rustfs-init"].environment.TG_BLOB_S3_ACCESS_KEY_ID == ""
' "$TMP/s3.json" >/dev/null

printf '%s\n' 'PASS local blobs render without RustFS or S3 credentials and S3 render defers validation to startup'
