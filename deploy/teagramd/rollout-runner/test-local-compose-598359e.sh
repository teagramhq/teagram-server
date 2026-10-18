#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
readonly ARTIFACT_NAME=.rollout-compose.local-598359e.yml
readonly ARTIFACT_SHA=714d7f870645e2581234feba81a86e9e529facd0b664b32b2f429eb583dc754f
readonly APP_TARGET_SHA=598359e900130c307dd84022b0077fd3276f6f36
TMP=$(mktemp -d "${TMPDIR:-/tmp}/telegramd-local-compose-598359e.XXXXXXXX")
chmod 700 "$TMP"
trap 'rm -rf -- "$TMP"' EXIT

if ! grep -Fq "TARGET_LOCAL_COMPOSE_TARGET_SHA=$APP_TARGET_SHA" "$SCRIPT_DIR/rollout-runner.sh"; then
  printf '%s\n' 'runner target pin differs from the 598359e application target' >&2
  exit 1
fi
actual_sha=$(sha256sum "$SCRIPT_DIR/local-compose-598359e.yml")
actual_sha=${actual_sha%% *}
[ "$actual_sha" = "$ARTIFACT_SHA" ] || {
  printf '%s\n' '598359e target-local Compose artifact digest differs from the reviewed pin' >&2
  exit 1
}
grep -Fq "TARGET_LOCAL_COMPOSE_SHA=$ARTIFACT_SHA" "$SCRIPT_DIR/rollout-runner.sh" || {
  printf '%s\n' 'runner artifact digest differs from the target-local Compose artifact' >&2
  exit 1
}
printf '%s\n' 'PASS artifact digest is pinned to the exact application target'
cmp -s <(tail -n +4 "$SCRIPT_DIR/local-compose-e58ba20.yml") <(tail -n +4 "$SCRIPT_DIR/local-compose-598359e.yml") || {
  printf '%s\n' '598359e artifact content differs from the reviewed e58ba20 local contract' >&2
  exit 1
}
printf '%s\n' 'PASS artifact body matches the reviewed e58ba20 local contract'

fixture="$TMP/project"
mkdir -m 700 "$fixture"
cp "$SCRIPT_DIR/local-compose-598359e.yml" "$fixture/$ARTIFACT_NAME"
chmod 600 "$fixture/$ARTIFACT_NAME"
cat > "$fixture/docker-compose.override.yml" <<'YAML'
services:
  telegramd:
    ports:
      - "127.0.0.1:2444:2444"
      - "100.64.0.10:2445:2445"
YAML
chmod 600 "$fixture/docker-compose.override.yml"

compose_with_inputs() {
  env \
    -u COMPOSE_PROFILES -u COMPOSE_PROJECT_NAME \
    -u TG_BLOB_S3_ENDPOINT -u TG_BLOB_S3_BUCKET -u TG_BLOB_S3_PREFIX \
    -u TG_BLOB_S3_REGION -u TG_BLOB_S3_ACCESS_KEY_ID \
    -u TG_BLOB_S3_SECRET_ACCESS_KEY -u TG_BLOB_S3_SECRET_ACCESS_KEY_FILE \
    -u TG_BLOB_S3_CA_PATH -u TG_BLOB_S3_ALLOW_INSECURE_HTTP \
    -u RUSTFS_ROOT_ACCESS_KEY -u RUSTFS_ROOT_SECRET_KEY \
    COMPOSE_FILE="$ARTIFACT_NAME:docker-compose.override.yml" \
    POSTGRES_PASSWORD=fixture \
    TG_PUBLIC_LINK_PREFIX=https://links.invalid \
    docker compose --env-file /dev/null --project-directory "$fixture" "$@"
}

(cd "$fixture" && compose_with_inputs config --quiet)
(cd "$fixture" && compose_with_inputs config --format json > "$TMP/local-compose.json")
jq -e '
  (.services | keys) == ["migrate", "postgres", "telegramd"]
  and .services.telegramd.environment.TG_BLOB_DIR == "/var/lib/telegramd-blobs"
  and .services.telegramd.environment.TG_PUBLIC_LINK_PREFIX == "https://links.invalid"
  and (.services.telegramd.environment | keys | sort) == ["TG_AUTHKEY_ENC_KEY", "TG_AUTHKEY_ENC_KEY_FILE", "TG_BLOB_DIR", "TG_CLIENT_ADDR_TRUST", "TG_LOG_LOGIN_CODES", "TG_POSTGRES_DSN", "TG_PUBLIC_LINK_PREFIX", "TG_REGISTRATION", "TG_REPLICA_COUNT", "TG_REPLICA_ID", "TG_RSA_KEY_FINGERPRINT"]
  and .services.telegramd.environment.TG_AUTHKEY_ENC_KEY == ""
  and .services.telegramd.environment.TG_AUTHKEY_ENC_KEY_FILE == "/var/lib/telegramd/enc_key.hex"
  and .services.telegramd.environment.TG_LOG_LOGIN_CODES == "true"
  and .services.telegramd.environment.TG_REGISTRATION == "closed"
  and .services.telegramd.environment.TG_REPLICA_ID == ""
  and .services.telegramd.environment.TG_REPLICA_COUNT == "1"
  and .services.telegramd.environment.TG_CLIENT_ADDR_TRUST == "socket"
  and .services.telegramd.environment.TG_RSA_KEY_FINGERPRINT == ""
  and .services.telegramd.environment.TG_POSTGRES_DSN == "postgres://postgres:fixture@postgres:5432/telegram?sslmode=disable"
  and .services.postgres.environment.POSTGRES_PASSWORD == "fixture"
  and .services.postgres.environment.POSTGRES_DB == "telegram"
  and .services.postgres.image == "postgres:16-alpine"
  and .services.migrate.image == "arigaio/atlas:1.2.0-alpine"
  and .services.telegramd.image == "telegramd:local"
  and ((.services.postgres.ports // []) | length) == 0
  and .services.telegramd.read_only == true
  and ([.services.telegramd.environment | keys[] | select(startswith("TG_BLOB_S3_"))] | length) == 0
  and ([.services.telegramd.environment | keys[] | select(startswith("RUSTFS_"))] | length) == 0
  and ([.services.telegramd.depends_on | keys[]] == ["migrate"])
  and .services.migrate.depends_on.postgres.condition == "service_healthy"
  and .services.telegramd.depends_on.migrate.condition == "service_completed_successfully"
  and ((.services.telegramd.secrets // []) | length) == 0
  and ((.secrets // {}) | length) == 0
  and (.services.telegramd.stop_grace_period == "120s" or .services.telegramd.stop_grace_period == "2m0s")
  and any(.services.telegramd.volumes[]; .target == "/var/lib/telegramd-blobs" and .type == "volume" and .source == "tgblobs" and (.read_only // false) == false)
  and any(.services.telegramd.volumes[]; .target == "/var/lib/telegramd" and .type == "volume" and .source == "tgkey")
  and any(.services.telegramd.volumes[]; .target == "/run/telegramd/blob-mode" and .read_only == true)
  and (.volumes.pgdata.name == "telegram-server_pgdata")
  and (.volumes.tgkey.name == "telegram-server_tgkey")
  and (.volumes.tgblobs.name == "telegram-server_tgblobs")
  and any(.services.telegramd.ports[]; .target == 2443 and .host_ip == "127.0.0.1")
  and any(.services.telegramd.ports[]; .target == 2444 and .host_ip == "127.0.0.1")
  and any(.services.telegramd.ports[]; .target == 2445 and .host_ip == "100.64.0.10")
' "$TMP/local-compose.json" >/dev/null
[ ! -e "$fixture/.env" ] && [ ! -e "$fixture/.secrets" ] || {
  printf '%s\n' 'credential-free render fixture unexpectedly provisioned secret files' >&2
  exit 1
}
printf '%s\n' 'PASS exact target-local Compose selection renders local services without RustFS inputs'
printf '%s\n' 'PASS volume identities, authority mount, migration ordering, grace, and merged bindings are preserved'

if (
  cd "$fixture"
  env \
    -u COMPOSE_PROFILES -u COMPOSE_PROJECT_NAME \
    -u POSTGRES_PASSWORD -u TG_BLOB_S3_ACCESS_KEY_ID \
    -u RUSTFS_ROOT_ACCESS_KEY -u RUSTFS_ROOT_SECRET_KEY \
    TG_PUBLIC_LINK_PREFIX=https://links.invalid \
    COMPOSE_FILE="$ARTIFACT_NAME:docker-compose.override.yml" \
    docker compose --env-file /dev/null --project-directory "$fixture" config --quiet
) > /dev/null 2> "$TMP/missing-postgres.stderr"; then
  printf '%s\n' 'missing PostgreSQL input unexpectedly rendered' >&2
  exit 1
fi
if (
  cd "$fixture"
  env \
    -u COMPOSE_PROFILES -u COMPOSE_PROJECT_NAME \
    -u TG_PUBLIC_LINK_PREFIX -u TG_BLOB_S3_ACCESS_KEY_ID \
    -u RUSTFS_ROOT_ACCESS_KEY -u RUSTFS_ROOT_SECRET_KEY \
    POSTGRES_PASSWORD=fixture \
    COMPOSE_FILE="$ARTIFACT_NAME:docker-compose.override.yml" \
    docker compose --env-file /dev/null --project-directory "$fixture" config --quiet
) > /dev/null 2> "$TMP/missing-origin.stderr"; then
  printf '%s\n' 'missing public-origin input unexpectedly rendered' >&2
  exit 1
fi
printf '%s\n' 'PASS missing PostgreSQL and public-origin inputs remain rejected'
