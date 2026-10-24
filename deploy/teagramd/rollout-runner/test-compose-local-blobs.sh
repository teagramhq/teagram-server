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
  and ((.services.teagramd.secrets // []) | length) == 1
  and any(.services.teagramd.secrets[]; .source == "teagramd_turnstile" and .target == "teagramd-turnstile")
  and .services.teagramd.environment.TG_TURNSTILE_SITE_KEY == ""
  and .services.teagramd.environment.TG_TURNSTILE_SECRET_KEY_FILE == ""
  and .services.teagramd.environment.TG_TURNSTILE_SIGNIN_ENABLED == "false"
  and .services.teagramd.environment.TG_LOG_LOGIN_CODES == "false"
  and ((.services.teagramd.networks | keys) == ["default"])
  and .secrets.teagramd_turnstile.file == "/dev/null"
  and ((.services.teagramd.depends_on | keys) == ["migrate"])
  and .services.teagramd.logging.driver == "syslog"
  and .services.teagramd.logging.options["syslog-address"] == "unixgram:///dev/log"
  and .services.teagramd.logging.options["syslog-facility"] == "daemon"
  and .services.teagramd.logging.options["syslog-format"] == "rfc5424"
  and .services.teagramd.logging.options.tag == "teagramd"
  and ((.services | has("rustfs")) | not)
  and ((.services | has("rustfs-init")) | not)
' "$TMP/local.json" >/dev/null

printf 'fixture-secret' >"$TMP/turnstile-secret"
chmod 444 "$TMP/turnstile-secret"
env -i PATH="$PATH" HOME="${HOME:-/root}" \
  TEAGRAM_TURNSTILE_SITE_KEY=fixture-site-key \
  TEAGRAM_TURNSTILE_SECRET_KEY_FILE="$TMP/turnstile-secret" \
  TG_TURNSTILE_SIGNIN_ENABLED=true \
  docker compose \
    --project-name local-blobs-credential-check \
    --project-directory "$REPO_ROOT" \
    --env-file "$TMP/no-blob-credentials.env" \
    --file "$REPO_ROOT/docker-compose.yml" \
    --file "$REPO_ROOT/docker-compose.local-blobs.yml" \
    config --format json >"$TMP/local-turnstile.json"
jq -e --arg secret_file "$TMP/turnstile-secret" '
  .services.teagramd.environment.TG_TURNSTILE_SITE_KEY == "fixture-site-key"
  and .services.teagramd.environment.TG_TURNSTILE_SECRET_KEY_FILE == "/run/secrets/teagramd-turnstile"
  and .services.teagramd.environment.TG_TURNSTILE_SIGNIN_ENABLED == "true"
  and ((.services.teagramd.secrets // []) | length) == 1
  and any(.services.teagramd.secrets[]; .source == "teagramd_turnstile" and .target == "teagramd-turnstile")
  and .secrets.teagramd_turnstile.file == $secret_file
' "$TMP/local-turnstile.json" >/dev/null

compose --file "$REPO_ROOT/docker-compose.yml" config --format json >"$TMP/s3.json"
jq -e '
  .services.teagramd.environment.TG_BLOB_S3_ENDPOINT == "http://rustfs:9000"
  and .services.teagramd.environment.TG_BLOB_S3_ACCESS_KEY_ID == ""
  and .services.teagramd.environment.TG_LOG_LOGIN_CODES == "false"
  and .services.teagramd.logging.driver == "syslog"
  and .services.teagramd.logging.options["syslog-address"] == "unixgram:///dev/log"
  and .services.teagramd.logging.options.tag == "teagramd"
  and .services["rustfs-init"].environment.TG_BLOB_S3_ACCESS_KEY_ID == ""
' "$TMP/s3.json" >/dev/null

env -i PATH="$PATH" HOME="${HOME:-/root}" TG_LOG_LOGIN_CODES=true docker compose \
  --project-name local-blobs-credential-check \
  --project-directory "$REPO_ROOT" \
  --env-file "$TMP/no-blob-credentials.env" \
  --file "$REPO_ROOT/docker-compose.yml" config --format json >"$TMP/login-codes-enabled.json"
jq -e '.services.teagramd.environment.TG_LOG_LOGIN_CODES == "false"' \
  "$TMP/login-codes-enabled.json" >/dev/null

printf '%s\n' 'PASS local blobs keep optional Turnstile while omitting RustFS and S3 credentials'
