#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -P "$SCRIPT_DIR/../../.." && pwd)
readonly ARTIFACT_NAME=.rollout-compose.initial-local.yml
readonly ARTIFACT_SHA=3a4f158c6e1f2ead6676fba85d8d95cfb15557a0fbd8e82230361e0af988e0f7
readonly APP_TARGET_SHA=777742cc4b3ab0fda6b504a82b314a90aa60918b
TMP=$(mktemp -d "${TMPDIR:-/tmp}/telegramd-initial-local-compose.XXXXXXXX")
chmod 700 "$TMP"
trap 'rm -rf -- "$TMP"' EXIT

documented_target=$(awk -F= '/^TARGET_SHA=/ && $2 ~ /^[0-9a-f]+$/ { print $2 }' "$SCRIPT_DIR/README.md")
if [ "$documented_target" != "$APP_TARGET_SHA" ] || [ "${#documented_target}" -ne 40 ]; then
  printf '%s\n' 'documented TARGET_SHA is not the exact 40-character application pin' >&2
  exit 1
fi
printf '%s\n' 'PASS documented target matches the exact 40-character application pin'

fixture="$TMP/project"
mkdir -m 700 "$fixture"
cp "$SCRIPT_DIR/initial-local-compose-777742.yml" "$fixture/$ARTIFACT_NAME"
chmod 600 "$fixture/$ARTIFACT_NAME"
cat > "$fixture/docker-compose.override.yml" <<'YAML'
services:
  telegramd:
    ports:
      - "127.0.0.1:2444:2444"
YAML
chmod 600 "$fixture/docker-compose.override.yml"

actual_sha=$(sha256sum "$fixture/$ARTIFACT_NAME")
actual_sha=${actual_sha%% *}
[ "$actual_sha" = "$ARTIFACT_SHA" ] || {
  printf '%s\n' 'initial-local Compose artifact digest differs from the reviewed pin' >&2
  exit 1
}

compose_with_inputs() {
  env \
    -u COMPOSE_FILE -u COMPOSE_PROFILES -u COMPOSE_PROJECT_NAME \
    -u TG_BLOB_S3_ENDPOINT -u TG_BLOB_S3_BUCKET -u TG_BLOB_S3_PREFIX \
    -u TG_BLOB_S3_REGION -u TG_BLOB_S3_ACCESS_KEY_ID \
    -u TG_BLOB_S3_SECRET_ACCESS_KEY -u TG_BLOB_S3_SECRET_ACCESS_KEY_FILE \
    -u TG_BLOB_S3_CA_PATH -u TG_BLOB_S3_ALLOW_INSECURE_HTTP \
    POSTGRES_PASSWORD=render-only-fixture \
    TG_PUBLIC_LINK_PREFIX=https://links.example.invalid/ \
    docker compose --env-file /dev/null --project-directory "$fixture" \
      --file "$fixture/$ARTIFACT_NAME" \
      --file "$fixture/docker-compose.override.yml" "$@"
}

compose_with_inputs config --quiet
compose_with_inputs config --format json > "$TMP/local-compose.json"
jq -e '
  (.services | keys) == ["migrate", "postgres", "telegramd"]
  and .services.telegramd.environment.TG_BLOB_DIR == "/var/lib/telegramd-blobs"
  and .services.telegramd.environment.TG_PUBLIC_LINK_PREFIX == "https://links.example.invalid/"
  and ([.services.telegramd.environment | keys[] | select(startswith("TG_BLOB_S3_"))] | length) == 0
  and ([.services.telegramd.depends_on | keys[]] == ["migrate"])
  and ((.services.telegramd.secrets // []) | length) == 0
  and (.services.telegramd.stop_grace_period == "120s" or .services.telegramd.stop_grace_period == "2m0s")
  and any(.services.telegramd.volumes[]; .target == "/var/lib/telegramd-blobs" and (.read_only // false) == false)
  and any(.services.telegramd.volumes[]; .target == "/var/lib/telegramd" and (.read_only // false) == false)
  and any(.services.telegramd.volumes[]; .target == "/run/telegramd/blob-mode" and .read_only == true)
  and any(.services.telegramd.ports[]; .target == 2444 and .host_ip == "127.0.0.1")
  and (.volumes.pgdata != null and .volumes.tgkey != null and .volumes.tgblobs != null)
' "$TMP/local-compose.json" >/dev/null
[ ! -e "$fixture/.env" ] && [ ! -e "$fixture/.secrets" ] || {
  printf '%s\n' 'credential-free render fixture unexpectedly provisioned secret files' >&2
  exit 1
}
printf '%s\n' 'PASS initial-local render has no S3 inputs, services, dependencies, or secret mounts'
printf '%s\n' 'PASS initial-local render preserves writable volumes, migration ordering, grace, and override ports'

if env \
  -u COMPOSE_FILE -u COMPOSE_PROFILES -u COMPOSE_PROJECT_NAME \
  -u POSTGRES_PASSWORD -u TG_BLOB_S3_ACCESS_KEY_ID \
  TG_PUBLIC_LINK_PREFIX=https://links.example.invalid/ \
  docker compose --env-file /dev/null --project-directory "$fixture" \
    --file "$fixture/$ARTIFACT_NAME" --file "$fixture/docker-compose.override.yml" \
    config --quiet > /dev/null 2> "$TMP/missing-postgres.stderr"; then
  printf '%s\n' 'missing PostgreSQL input unexpectedly rendered' >&2
  exit 1
fi
if env \
  -u COMPOSE_FILE -u COMPOSE_PROFILES -u COMPOSE_PROJECT_NAME \
  -u TG_PUBLIC_LINK_PREFIX -u TG_BLOB_S3_ACCESS_KEY_ID \
  POSTGRES_PASSWORD=render-only-fixture \
  docker compose --env-file /dev/null --project-directory "$fixture" \
    --file "$fixture/$ARTIFACT_NAME" --file "$fixture/docker-compose.override.yml" \
    config --quiet > /dev/null 2> "$TMP/missing-origin.stderr"; then
  printf '%s\n' 'missing public-origin input unexpectedly rendered' >&2
  exit 1
fi
printf '%s\n' 'PASS missing PostgreSQL and public-origin inputs remain rejected'

mkdir -m 700 "$fixture/.secrets"
printf '%s\n' 'render-only-fixture-secret' > "$fixture/.secrets/telegramd-blob-secret-key"
chmod 444 "$fixture/.secrets/telegramd-blob-secret-key"

s3_compose_with_inputs() {
  env \
    -u COMPOSE_FILE -u COMPOSE_PROFILES -u COMPOSE_PROJECT_NAME \
    -u TG_BLOB_S3_ENDPOINT -u TG_BLOB_S3_BUCKET -u TG_BLOB_S3_PREFIX \
    -u TG_BLOB_S3_REGION -u TG_BLOB_S3_ACCESS_KEY_ID \
    -u TG_BLOB_S3_SECRET_ACCESS_KEY -u TG_BLOB_S3_SECRET_ACCESS_KEY_FILE \
    -u TG_BLOB_S3_CA_PATH -u TG_BLOB_S3_ALLOW_INSECURE_HTTP \
    POSTGRES_PASSWORD=render-only-fixture \
    TG_PUBLIC_LINK_PREFIX=https://links.example.invalid/ \
    TG_BLOB_S3_ACCESS_KEY_ID=fixture-app-access \
    RUSTFS_ROOT_ACCESS_KEY=fixture-root-access \
    RUSTFS_ROOT_SECRET_KEY=fixture-root-secret \
    docker compose --env-file /dev/null --project-directory "$fixture" \
      --file "$REPO_ROOT/docker-compose.yml" --profile migration "$@"
}

if env \
  -u COMPOSE_FILE -u COMPOSE_PROFILES -u COMPOSE_PROJECT_NAME \
  -u TG_BLOB_S3_ENDPOINT -u TG_BLOB_S3_BUCKET -u TG_BLOB_S3_PREFIX \
  -u TG_BLOB_S3_REGION -u TG_BLOB_S3_ACCESS_KEY_ID \
  -u TG_BLOB_S3_SECRET_ACCESS_KEY -u TG_BLOB_S3_SECRET_ACCESS_KEY_FILE \
  -u TG_BLOB_S3_CA_PATH -u TG_BLOB_S3_ALLOW_INSECURE_HTTP \
  POSTGRES_PASSWORD=render-only-fixture \
  TG_PUBLIC_LINK_PREFIX=https://links.example.invalid/ \
  RUSTFS_ROOT_ACCESS_KEY=fixture-root-access \
  RUSTFS_ROOT_SECRET_KEY=fixture-root-secret \
  docker compose --env-file /dev/null --project-directory "$fixture" \
    --file "$REPO_ROOT/docker-compose.yml" --profile migration \
    config --quiet > /dev/null 2> "$TMP/missing-s3-credential.stderr"; then
  printf '%s\n' 'S3 Compose selection unexpectedly rendered without an access key' >&2
  exit 1
fi
if ! s3_compose_with_inputs config --format json > "$TMP/s3-compose.json" 2> "$TMP/s3-compose.stderr"; then
  printf '%s\n' 'S3 Compose fixture did not render with the complete disposable inputs' >&2
  exit 1
fi
jq -e '
  (.services.rustfs.ports // []) == []
  and .services.telegramd.environment.TG_BLOB_S3_ENDPOINT == "http://rustfs:9000"
  and .services.telegramd.environment.TG_BLOB_S3_BUCKET == "telegram"
  and .services.telegramd.environment.TG_BLOB_S3_PREFIX == "telegramd/"
  and .services.telegramd.environment.TG_BLOB_S3_ACCESS_KEY_ID == "fixture-app-access"
  and .services.telegramd.depends_on["rustfs-init"].condition == "service_completed_successfully"
  and any(.services.telegramd.secrets[]; .source == "telegramd_blob_secret_key")
  and (.secrets.telegramd_blob_secret_key.file | endswith("/.secrets/telegramd-blob-secret-key"))
' "$TMP/s3-compose.json" >/dev/null
printf '%s\n' 'PASS explicit S3 Compose selection still requires credentials and keeps RustFS private'
