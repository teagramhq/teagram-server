#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -P "$SCRIPT_DIR/../.." && pwd)
TMP_DIR=$(mktemp -d)
trap 'rm -rf -- "$TMP_DIR"' EXIT

cat > "$TMP_DIR/demo.env" <<'ENV'
POSTGRES_PASSWORD=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
TG_AUTHKEY_ENC_KEY=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
TG_PUBLIC_LINK_PREFIX=https://demo.teagram.io/
TG_ADVERTISE_ADDR=demo.teagram.io:443
TG_WEBSOCKET_LISTEN_ADDR=:2446
TG_WEBSOCKET_ALLOWED_ORIGINS=https://web.teagram.io
TG_LOG_LOGIN_CODES=false
TG_REGISTRATION=open
TG_MAX_FILE_BYTES=16777216
TG_MAX_USER_STORAGE_BYTES=268435456
TEAGRAM_DEMO_BLOB_PATH=/srv/teagram-demo-blobs/data
TEAGRAM_DEMO_TURNSTILE_SITE_KEY=fixture-site-key
TEAGRAM_DEMO_TURNSTILE_SECRET_KEY=fixture-secret-key
ENV
chmod 0600 "$TMP_DIR/demo.env"

compose=(
  docker compose
  --env-file "$TMP_DIR/demo.env"
  --project-directory "$REPO_ROOT"
  --project-name teagram-demo
  --file "$SCRIPT_DIR/compose.yaml"
)
env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
  "${compose[@]}" config --format json > "$TMP_DIR/target.json"
jq -e '
  .name == "teagram-demo"
  and (.services | keys | sort) == ["edge", "migrate", "postgres", "teagramd"]
  and ((.services.teagramd.ports // []) | length) == 0
  and ((.services.edge.ports | length) == 1)
  and .services.edge.ports[0].host_ip == "127.0.0.1"
  and (.services.edge.ports[0].published | tostring) == "8080"
  and (.services | to_entries | map(.value.ports // []) | add | map(.published | tostring) | index("2443")) == null
  and (.services.teagramd.networks | keys) == ["backend"]
  and .networks.backend.internal == true
  and (.services.edge.networks | keys | sort) == ["backend", "edge"]
  and .services.teagramd.environment.TG_REGISTRATION == "open"
  and .services.teagramd.environment.TG_LOG_LOGIN_CODES == "false"
  and .services.teagramd.environment.TG_MAX_FILE_BYTES == "16777216"
  and .services.teagramd.environment.TG_MAX_USER_STORAGE_BYTES == "268435456"
  and .services.teagramd.environment.TG_WEBSOCKET_LISTEN_ADDR == ":2446"
  and .services.teagramd.environment.TG_WEBSOCKET_ALLOWED_ORIGINS == "https://web.teagram.io"
  and .services.teagramd.environment.TG_CLIENT_ADDR_TRUST == "socket"
  and .services.postgres.environment.PGDATA == "/var/lib/postgresql/18/docker"
  and .services.postgres.platform == "linux/amd64"
  and (.services.postgres.image | endswith("@sha256:d8703cd7fba306b9fec9268ecedfa8a966846c053036a60e3635791957eb2f66"))
  and .services.migrate.platform == "linux/amd64"
  and (.services.migrate.image | endswith("@sha256:5abea953e8e048d5b27b1b3e2fc83e17fdc1f7cd0d908a8432ed0c7e6394c58f"))
  and any(.services.postgres.volumes[]; .target == "/var/lib/postgresql")
  and .volumes.tgblobs.driver == "local"
  and .volumes.tgblobs.driver_opts.type == "none"
  and .volumes.tgblobs.driver_opts.o == "bind"
  and .volumes.tgblobs.driver_opts.device == "/srv/teagram-demo-blobs/data"
' "$TMP_DIR/target.json" >/dev/null

env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
  "${compose[@]}" --file "$SCRIPT_DIR/compose.bootstrap.yaml" config --format json > "$TMP_DIR/bootstrap.json"
jq -e '
  (.services.teagramd.volumes | map(.target) | sort) == ["/var/lib/teagramd", "/var/lib/teagramd-blobs"]
  and .services.teagramd.environment.TG_BLOB_DIR == "/var/lib/teagramd-blobs"
  and .volumes.tgblobs.driver_opts.device == "/srv/teagram-demo-blobs/data"
' "$TMP_DIR/bootstrap.json" >/dev/null

grep -Fq 'location = /apiws {' "$SCRIPT_DIR/nginx.conf"
grep -Fq 'location = /admin { return 404; }' "$SCRIPT_DIR/nginx.conf"
grep -Fq 'location ^~ /admin/ { return 404; }' "$SCRIPT_DIR/nginx.conf"
grep -Fq 'image: mirror.gcr.io/library/nginx:1.28.0-alpine@sha256:' "$SCRIPT_DIR/compose.yaml"
printf 'demo Compose contract holds\n'
