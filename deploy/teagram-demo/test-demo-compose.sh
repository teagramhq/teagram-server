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
ENV
printf 'TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE=%s\n' "$TMP_DIR/turnstile-secret-key" >> "$TMP_DIR/demo.env"
chmod 0600 "$TMP_DIR/demo.env"
printf 'fixture-secret-key\n' > "$TMP_DIR/turnstile-secret-key"
chmod 0600 "$TMP_DIR/turnstile-secret-key"

compose=(
  docker compose
  --env-file "$TMP_DIR/demo.env"
  --project-directory "$REPO_ROOT"
  --project-name teagram-demo
  --file "$SCRIPT_DIR/compose.yaml"
)
env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
  -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE \
  "${compose[@]}" config --format json > "$TMP_DIR/target.json"
jq -e --arg secretFile "$TMP_DIR/turnstile-secret-key" '
  .name == "teagram-demo"
  and (.services | keys | sort) == ["edge", "haproxy", "migrate", "postgres", "teagramd", "turnstile-egress"]
  and ((.services.teagramd.ports // []) | length) == 0
  and ((.services.edge.ports | length) == 1)
  and .services.edge.ports[0].host_ip == "127.0.0.1"
  and (.services.edge.ports[0].published | tostring) == "8080"
  and (.services | to_entries | map(.value.ports // []) | add | map(.published | tostring) | index("2443")) == null
  and ((.services.haproxy.ports // []) | length) == 0
  and .services.haproxy.platform == "linux/amd64"
  and .services.haproxy.image == "mirror.gcr.io/library/haproxy:3.2-alpine@sha256:51f173c6825753d9e3a888b1004a1c2cb730936cc9178730a5c555246bd161a2"
  and .services.haproxy.networks.backend.ipv4_address == "198.18.130.2"
  and .services.haproxy.healthcheck.test[-1] == "http://127.0.0.1:8404/healthz"
  and .services.edge.depends_on.haproxy.condition == "service_healthy"
  and (.services.teagramd.networks | keys) == ["backend"]
  and .services.teagramd.networks.backend.ipv4_address == "198.18.130.253"
  and .services.teagramd.environment.HTTPS_PROXY == "http://turnstile-egress:3128"
  and .services.teagramd.environment.HTTP_PROXY == "http://turnstile-egress:3128"
  and .services.teagramd.environment.NO_PROXY == "localhost,127.0.0.1,postgres,teagramd"
  and .services.teagramd.depends_on["turnstile-egress"].condition == "service_healthy"
  and .services["turnstile-egress"].platform == "linux/amd64"
  and .services["turnstile-egress"].image == "mirror.gcr.io/ubuntu/squid:7.2-26.04_edge@sha256:f6843088c0eab9e63ecb8a1c215d939cd97d0370611bb2bdb65c4a8e48b3c3eb"
  and (.services["turnstile-egress"].ports // [] | length) == 0
  and .services["turnstile-egress"].networks.backend.ipv4_address == "198.18.130.252"
  and .services["turnstile-egress"].networks.egress.gw_priority == 1
  and (.services["turnstile-egress"].networks | keys | sort) == ["backend", "egress"]
  and .services["turnstile-egress"].healthcheck.test == ["CMD", "/usr/bin/pebble", "health"]
  and any(.services["turnstile-egress"].volumes[]; .target == "/var/lib/pebble/default/layers/002-demo-health.yaml" and .read_only)
  and .networks.egress.internal != true
  and .services["turnstile-egress"].logging.options["max-size"] == "10m"
  and .services["turnstile-egress"].logging.options["max-file"] == "3"
  and (.services.teagramd.networks | keys) == ["backend"]
  and .networks.backend.internal == true
  and .networks.backend.ipam.config[0].subnet == "198.18.130.0/24"
  and (.services.edge.networks | keys | sort) == ["backend", "edge"]
  and .services.teagramd.environment.TG_REGISTRATION == "open"
  and .services.teagramd.environment.TG_LOG_LOGIN_CODES == "false"
  and .services.teagramd.environment.TG_MAX_FILE_BYTES == "16777216"
  and .services.teagramd.environment.TG_MAX_USER_STORAGE_BYTES == "268435456"
  and .services.teagramd.environment.TG_WEBSOCKET_LISTEN_ADDR == ":2446"
  and .services.teagramd.environment.TG_WEBSOCKET_ALLOWED_ORIGINS == "https://web.teagram.io"
  and .services.teagramd.environment.TG_CLIENT_ADDR_TRUST == "proxy-v2"
  and .services.teagramd.environment.TG_CLIENT_ADDR_PROXY_CIDRS == "198.18.130.2/32"
  and .services.teagramd.environment.TG_TURNSTILE_SITE_KEY == "fixture-site-key"
  and .services.teagramd.environment.TG_TURNSTILE_SECRET_KEY_FILE == "/run/secrets/teagram_demo_turnstile"
  and .secrets.teagram_demo_turnstile.file == $secretFile
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
  -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE \
  TEAGRAM_DEMO_EDGE_BIND=127.0.0.1:18080 \
  "${compose[@]}" config --format json > "$TMP_DIR/validation.json"
jq -e '
  .services.edge.ports | length == 1
  and .[0].host_ip == "127.0.0.1"
  and (.[0].published | tostring) == "18080"
' "$TMP_DIR/validation.json" >/dev/null

env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
  -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE \
  "${compose[@]}" --file "$SCRIPT_DIR/compose.bootstrap.yaml" \
  --file "$SCRIPT_DIR/compose.seed.yaml" config --format json > "$TMP_DIR/bootstrap.json"
jq -e '
  (.services.teagramd.volumes | map(.target) | sort) == ["/var/lib/teagramd", "/var/lib/teagramd-blobs"]
  and .services.teagramd.environment.TG_BLOB_DIR == "/var/lib/teagramd-blobs"
  and .services.teagramd.environment.HTTP_PROXY == ""
  and .services.teagramd.environment.HTTPS_PROXY == ""
  and (.services.teagramd.depends_on | keys) == ["migrate"]
  and .services.teagramd.environment.TG_CLIENT_ADDR_TRUST == "socket"
  and .services.teagramd.environment.TG_CLIENT_ADDR_PROXY_CIDRS == ""
  and .services.teagramd.environment.TG_TURNSTILE_SITE_KEY == ""
  and .services.teagramd.environment.TG_TURNSTILE_SECRET_KEY_FILE == ""
  and .services.teagramd.environment.TG_RATE_LIMIT_SIGN_UP_IP == "32"
  and .volumes.tgblobs.driver_opts.device == "/srv/teagram-demo-blobs/data"
' "$TMP_DIR/bootstrap.json" >/dev/null

env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
  -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE \
  "${compose[@]}" --file "$SCRIPT_DIR/compose.seed.yaml" config --format json > "$TMP_DIR/seed.json"
jq -e '
  .services.teagramd.environment.TG_CLIENT_ADDR_TRUST == "socket"
  and .services.teagramd.environment.TG_CLIENT_ADDR_PROXY_CIDRS == ""
  and .services.teagramd.environment.HTTP_PROXY == ""
  and .services.teagramd.environment.HTTPS_PROXY == ""
  and (.services.teagramd.depends_on | keys) == ["migrate"]
  and .services.teagramd.environment.TG_TURNSTILE_SITE_KEY == ""
  and .services.teagramd.environment.TG_TURNSTILE_SECRET_KEY_FILE == ""
  and .services.teagramd.environment.TG_RATE_LIMIT_SIGN_UP_IP == "32"
  and .services.teagramd.environment.TG_RATE_LIMIT_SIGN_UP_IP_WINDOW == "1h"
  and .services.teagramd.environment.TG_RATE_LIMIT_SEND_CODE_IP == "64"
  and .services.teagramd.environment.TG_RATE_LIMIT_SEND_CODE_IP_PHONES == "32"
' "$TMP_DIR/seed.json" >/dev/null
python3 "$SCRIPT_DIR/../teagramd/rollout-runner/blob-mode-state.py" compose \
  --checkout "$REPO_ROOT" < "$TMP_DIR/seed.json" > "$TMP_DIR/seed-inventory.json"
python3 "$SCRIPT_DIR/../teagramd/rollout-runner/blob-mode-state.py" compose \
  --checkout "$REPO_ROOT" < "$TMP_DIR/bootstrap.json" > "$TMP_DIR/bootstrap-inventory.json"
jq -e --slurpfile bootstrap "$TMP_DIR/bootstrap-inventory.json" '
  .services | length == 1
  and .[0].initial_local_guard_environment == {
    "TG_REPLICA_COUNT": "1",
    "TG_CLIENT_ADDR_TRUST": "socket"
  }
  and .[0].tg_environment_sha256 == $bootstrap[0].services[0].tg_environment_sha256
  and .[0].network == $bootstrap[0].services[0].network
' "$TMP_DIR/seed-inventory.json" >/dev/null

grep -Fq 'location = /apiws {' "$SCRIPT_DIR/nginx.conf"
# These patterns match literal Nginx variables.
# shellcheck disable=SC2016
grep -Fq 'set $teagramd http://haproxy:2446;' "$SCRIPT_DIR/nginx.conf"
# shellcheck disable=SC2016
grep -Fq 'proxy_set_header CF-Connecting-IP $http_cf_connecting_ip;' "$SCRIPT_DIR/nginx.conf"
grep -Fq '"CF-Connecting-IP": "192.0.2.1"' "$SCRIPT_DIR/smoke.py"
grep -Fq 'location = /admin { return 404; }' "$SCRIPT_DIR/nginx.conf"
grep -Fq 'location ^~ /admin/ { return 404; }' "$SCRIPT_DIR/nginx.conf"
grep -Fq 'image: mirror.gcr.io/library/nginx:1.28.0-alpine@sha256:' "$SCRIPT_DIR/compose.yaml"
grep -Fq 'http-request set-src req.hdr_ip(CF-Connecting-IP)' "$SCRIPT_DIR/haproxy.cfg"
grep -Fq 'req.hdr_cnt(CF-Connecting-IP) eq 1' "$SCRIPT_DIR/haproxy.cfg"
grep -Fq 'server teagramd teagramd:2446 check send-proxy-v2' "$SCRIPT_DIR/haproxy.cfg"
grep -Fq 'http-check send meth CONNECT uri challenges.cloudflare.com:443' "$SCRIPT_DIR/haproxy.cfg"
grep -Fq 'monitor fail if { nbsrv(turnstile_egress) eq 0 }' "$SCRIPT_DIR/haproxy.cfg"
grep -Fq 'bind :8404' "$SCRIPT_DIR/haproxy.cfg"
grep -Fq 'http-reuse never' "$SCRIPT_DIR/haproxy.cfg"
grep -Fq 'acl allowed_client src 198.18.130.2/32 198.18.130.253/32' "$SCRIPT_DIR/turnstile-egress.conf"
grep -Fq 'acl siteverify_host dstdomain challenges.cloudflare.com' "$SCRIPT_DIR/turnstile-egress.conf"
grep -Fq 'http_access allow allowed_client CONNECT CONNECT_PORT siteverify_host' "$SCRIPT_DIR/turnstile-egress.conf"
grep -Fq 'http_access deny all' "$SCRIPT_DIR/turnstile-egress.conf"
grep -Fq 'cache deny all' "$SCRIPT_DIR/turnstile-egress.conf"
grep -Fq 'level: ready' "$SCRIPT_DIR/pebble-health.yaml"
grep -Fq 'host: 127.0.0.1' "$SCRIPT_DIR/pebble-health.yaml"
grep -Fq 'port: 3128' "$SCRIPT_DIR/pebble-health.yaml"
grep -Fq 'location = /healthz {' "$SCRIPT_DIR/nginx.conf"
grep -Fq 'https://demo.teagram.io/healthz' "$SCRIPT_DIR/gatus.yaml"
grep -Fq 'base_url + "/healthz"' "$SCRIPT_DIR/smoke.py"

python3 - "$SCRIPT_DIR/wipe-weekly.sh" "$SCRIPT_DIR/init-demo.sh" \
  "$SCRIPT_DIR/deploy-main.sh" "$SCRIPT_DIR/demo-common.sh" \
  "$SCRIPT_DIR/forced-command.sh" <<'PY'
from pathlib import Path
import sys


def assert_text_order(text: str, name: str, fragments: list[str], start: int = 0) -> None:
    positions = [text.find(fragment, start) for fragment in fragments]
    if any(position < 0 for position in positions) or positions != sorted(positions):
        raise SystemExit(f"{name}: safety steps are missing or out of order")


def assert_order(path: Path, fragments: list[str], start: int = 0) -> None:
    assert_text_order(path.read_text(encoding="utf-8"), path.name, fragments, start)


assert_order(Path(sys.argv[1]), [
    "demo_compose stop edge haproxy teagramd turnstile-egress",
    "demo_prune_deploy_backups\n",
    "dropdb -U postgres --if-exists teagram",
    "demo_seed_compose up -d teagramd",
    "demo_prepare_seed_owner \"$TMP_DIR\"",
    '"$SEED_SCRIPT" --seed',
    '"$SEED_SCRIPT" --verify-seed --endpoint teagramd:2443',
    "demo_compose up -d teagramd",
    '"$SEED_SCRIPT" --verify-turnstile --endpoint teagramd:2443',
    "demo_compose up -d haproxy edge",
])
deploy_path = Path(sys.argv[3])
deploy = deploy_path.read_text(encoding="utf-8")
deploy_steps = [
    "demo_validation_compose up -d haproxy edge",
    'python3 "$SCRIPT_DIR/smoke.py" "$DEMO_STATE_DIR/client.json" 18080',
    "demo_compose up -d edge",
]
deploy_positions = [deploy.find(step) for step in deploy_steps]
if any(position < 0 for position in deploy_positions) or deploy_positions != sorted(deploy_positions):
    raise SystemExit("deploy-main.sh: public edge must start only after private validation smoke")
if "PUBLIC_ACTIVATION_STARTED=1" not in deploy or "database preserved" not in deploy:
    raise SystemExit("deploy-main.sh: activation failure must not restore a database after public writes")
backup_function = deploy.split("demo_quiesce_and_backup_database() {", 1)[1].split("\n}", 1)[0]
assert_text_order(backup_function, "deploy-main.sh backup function", [
    "SERVICES_STOPPED=1",
    "demo_compose stop edge haproxy",
    "demo_compose stop teagramd turnstile-egress",
    'DB_DUMP="$BACKUP_DIR/postgres.dump"',
    "demo_compose exec -T postgres pg_dump -U postgres -Fc teagram",
    "demo_compose_input exec -T postgres pg_restore --list",
    'BACKUP_READY=1',
])
assert_order(deploy_path, [
    "demo_quiesce_and_backup_database\n",
    "demo_compose build teagramd",
])
rollback = deploy.split("demo_rollback() {", 1)[1].split("\n}", 1)[0]
if "demo_restore_database || rollback_rc=1" not in rollback:
    raise SystemExit("deploy-main.sh: rollback must restore the tested database snapshot")
if "demo_wait_migration demo_compose 'deployment migrations'" not in deploy:
    raise SystemExit("deploy-main.sh: deployment must wait for migration completion")
if "demo_wait_migration demo_compose 'rollback migrations'" not in rollback:
    raise SystemExit("deploy-main.sh: rollback must wait for migration completion")
if "MIGRATE_STATUS=$(docker inspect" in deploy:
    raise SystemExit("deploy-main.sh: detached migration status must be checked after waiting")
if "PREV_IMAGE_TAG" in deploy or "teagramd:rollback-" in deploy:
    raise SystemExit("deploy-main.sh: rollback image must not get a persistent unique tag")
if 'docker image tag "$PREV_IMAGE_ID" teagramd:local' not in deploy:
    raise SystemExit("deploy-main.sh: rollback must restore the previous image tag")
waiter = Path(sys.argv[4]).read_text(encoding="utf-8")
if "demo_wait_migration()" not in waiter or 'docker wait "${migration_ids[0]}"' not in waiter:
    raise SystemExit("demo-common.sh: migration helper must wait for the container exit code")
forced_command = Path(sys.argv[5]).read_text(encoding="utf-8")
wipe = Path(sys.argv[1]).read_text(encoding="utf-8")
if 'LOCK_PATH="$DEMO_REPO_ROOT/.state/deploy.lock"' not in wipe:
    raise SystemExit("wipe-weekly.sh: deploy and wipe must share the deploy lock")
assert_text_order(forced_command, "forced-command.sh deploy lock", [
    "unset DEMO_DEPLOY_LOCK_HELD",
    'LOCK_PATH="$REPO/.state/deploy.lock"',
    'exec 9> "$LOCK_PATH"',
    "flock -x 9",
    'git -C "$REPO" fetch',
    'git -C "$REPO" reset --hard "$SHA"',
    'DEMO_DEPLOY_LOCK_HELD=1 DEPLOY_SHA="$SHA" PREVIOUS_SHA="$previous"',
    'git -C "$REPO" reset --hard "$previous"',
])
assert_order(deploy_path, [
    'LOCK_PATH="$DEMO_REPO_ROOT/.state/deploy.lock"',
    'if [[ "${DEMO_DEPLOY_LOCK_HELD:-0}" != 1 ]]; then',
    "flock -x 9",
    'git -C "$DEMO_REPO_ROOT" fetch',
])
init = Path(sys.argv[2]).read_text(encoding="utf-8")
cleanup = init.split("cleanup() {", 1)[1].split("\n}", 1)[0]
if "rc != 0" not in cleanup or "PUBLIC_SERVICES_MAY_BE_RUNNING == 1" not in cleanup:
    raise SystemExit("init-demo.sh: failed install must close a possibly public stack")
if "demo_compose stop edge haproxy" not in cleanup:
    raise SystemExit("init-demo.sh: failed install cleanup must stop edge and HAProxy")
assert_text_order(init, "init-demo.sh resume lock", [
    "demo_lock_init() {",
    "flock -x 9",
    "demo_lock_init\n",
    'MODE_FILE="$DEMO_AUTHORITY_DIR/mode.json"',
    'if [[ -e "$MODE_FILE" || -L "$MODE_FILE" ]]; then',
])
assert_order(Path(sys.argv[2]), [
    "PUBLIC_SERVICES_MAY_BE_RUNNING=1",
    "demo_compose up -d haproxy edge",
    'python3 "$SCRIPT_DIR/smoke.py" "$DEMO_STATE_DIR/client.json"',
    "exit 0",
])
assert_order(Path(sys.argv[2]), [
    "PUBLIC_SERVICES_MAY_BE_RUNNING=1",
    "demo_compose up -d haproxy edge",
    'python3 "$SCRIPT_DIR/smoke.py" "$DEMO_STATE_DIR/client.json"',
], start=init.index("exit 0") + len("exit 0"))
assert_order(Path(sys.argv[2]), [
    "demo_compose stop edge haproxy",
    "demo_seed_compose up -d teagramd",
    '"$SEED_SCRIPT" --seed',
    '"$SEED_SCRIPT" --verify-seed --endpoint teagramd:2443',
    "demo_compose up -d teagramd",
    '"$SEED_SCRIPT" --verify-turnstile --endpoint teagramd:2443',
    "demo_compose up -d haproxy edge",
])
if "demo_wait_migration demo_bootstrap_compose 'bootstrap migrations'" not in init:
    raise SystemExit("init-demo.sh: bootstrap must wait for migration completion")
if "MIGRATE_STATUS=$(docker inspect" in init:
    raise SystemExit("init-demo.sh: detached bootstrap migration status must be checked after waiting")
if "demo_wait_migration demo_compose 'post-reset migrations'" not in Path(sys.argv[1]).read_text(encoding="utf-8"):
    raise SystemExit("wipe-weekly.sh: reset must wait for migration completion")
if "demo_prune_deploy_backups()" not in Path(sys.argv[1]).read_text(encoding="utf-8"):
    raise SystemExit("wipe-weekly.sh: weekly reset must remove old deploy snapshots")
PY

INIT_LOCK_FUNCTION="$TMP_DIR/init-lock-function.sh"
sed -n '/^demo_lock_init() {$/,/^}/p' "$SCRIPT_DIR/init-demo.sh" > "$INIT_LOCK_FUNCTION"
cat > "$TMP_DIR/init-resume-lock-regression.sh" <<'BASH'
set -Eeuo pipefail
source "$INIT_LOCK_FUNCTION"
DEMO_REPO_ROOT="$TMP_DIR/resume-lock-repo"
LOCK_PATH="$DEMO_REPO_ROOT/.state/deploy.lock"
mkdir -p "$(dirname "$LOCK_PATH")"
exec 8> "$LOCK_PATH"
flock -x 8
(
  exec 8>&-
  : > "$TMP_DIR/resume-attempted"
  demo_lock_init
  : > "$TMP_DIR/resume-continued"
) &
resume_pid=$!
for attempt in {1..100}; do
  [[ -e "$TMP_DIR/resume-attempted" ]] && break
  sleep 0.01
done
[[ -e "$TMP_DIR/resume-attempted" ]] || {
  printf '%s\n' 'resume lock test did not start its child' >&2
  kill "$resume_pid" 2>/dev/null || true
  exit 1
}
sleep 0.1
[[ ! -e "$TMP_DIR/resume-continued" ]] || {
  printf '%s\n' 'resume continued while the wipe lock was held' >&2
  exit 1
}
flock -u 8
wait "$resume_pid"
[[ -e "$TMP_DIR/resume-continued" ]]
BASH
env \
  INIT_LOCK_FUNCTION="$INIT_LOCK_FUNCTION" \
  TMP_DIR="$TMP_DIR" \
  bash "$TMP_DIR/init-resume-lock-regression.sh"

PRUNE_FUNCTION="$TMP_DIR/demo-prune-backups.sh"
sed -n '/^demo_prune_deploy_backups() {$/,/^}/p' "$SCRIPT_DIR/wipe-weekly.sh" > "$PRUNE_FUNCTION"
cat > "$TMP_DIR/backup-prune-regression.sh" <<'BASH'
set -Eeuo pipefail
source "$PRUNE_FUNCTION"
BACKUP_ROOT="$DEMO_STATE_DIR/backups"
SENTINEL_DIR="$TMP_DIR/retention-outside"
mkdir -p "$BACKUP_ROOT/deploy-old" "$SENTINEL_DIR"
printf 'old user records\n' > "$BACKUP_ROOT/deploy-old/postgres.dump"
printf 'outside state\n' > "$SENTINEL_DIR/keep"
ln -s "$SENTINEL_DIR" "$BACKUP_ROOT/external-link"
demo_fail() { return 1; }
chown() { :; }
chmod() { :; }
demo_prune_deploy_backups
[[ -d "$BACKUP_ROOT" ]]
[[ -z "$(find "$BACKUP_ROOT" -mindepth 1 -maxdepth 1 -print -quit)" ]]
[[ -f "$SENTINEL_DIR/keep" ]]
rm -rf -- "$BACKUP_ROOT"
ln -s "$SENTINEL_DIR" "$BACKUP_ROOT"
if demo_prune_deploy_backups; then
  printf '%s\n' 'backup prune test followed a symlinked backup root' >&2
  exit 1
fi
[[ -f "$SENTINEL_DIR/keep" ]]
BASH
env \
  DEMO_STATE_DIR="$TMP_DIR/retention-state" \
  TMP_DIR="$TMP_DIR" \
  PRUNE_FUNCTION="$PRUNE_FUNCTION" \
  bash "$TMP_DIR/backup-prune-regression.sh"

BACKUP_FUNCTIONS="$TMP_DIR/deploy-backup-functions.sh"
sed -n \
  -e '/^demo_quiesce_and_backup_database() {$/,/^}/p' \
  -e '/^demo_restore_database() {$/,/^}/p' \
  "$SCRIPT_DIR/deploy-main.sh" > "$BACKUP_FUNCTIONS"
cat > "$TMP_DIR/deploy-backup-regression.sh" <<'BASH'
set -Eeuo pipefail
source "$BACKUP_FUNCTIONS"
EVENTS="$TMP_DIR/events"
DATABASE_STATE="$TMP_DIR/database"
RESTORE_CHECK="$TMP_DIR/restore-check"
mkdir -p "$BACKUP_DIR" "$TMP_DIR"
printf 'original-row\n' > "$DATABASE_STATE"
INGRESS_OPEN=1
SERVER_RUNNING=1
demo_fail() { return 1; }
chown() { :; }
chmod() { :; }
demo_compose() {
  if [[ "$*" == 'stop edge haproxy' ]]; then
    INGRESS_OPEN=0
    printf 'ingress-stopped\n' >> "$EVENTS"
  elif [[ "$*" == 'stop teagramd turnstile-egress' ]]; then
    [[ "$INGRESS_OPEN" == 0 ]] || return 1
    # A request accepted as the server drains must be in the rollback snapshot.
    printf 'acknowledged-during-shutdown\n' >> "$DATABASE_STATE"
    SERVER_RUNNING=0
    printf 'server-stopped\n' >> "$EVENTS"
  elif [[ "$*" == *'pg_dump -U postgres -Fc teagram'* ]]; then
    [[ "$SERVER_RUNNING" == 0 ]] || return 1
    printf 'database-dumped\n' >> "$EVENTS"
    cat "$DATABASE_STATE"
  elif [[ "$*" == *'psql -U postgres -Atqc'* ]]; then
    : # The isolated restore database does not exist yet.
  elif [[ "$*" == *'createdb -U postgres'* || "$*" == *'dropdb -U postgres'* ]]; then
    :
  else
    printf 'unexpected demo_compose call: %s\n' "$*" >&2
    return 1
  fi
}
demo_compose_input() {
  if [[ "$*" == *'pg_restore --list'* ]]; then
    printf 'TABLE DATA\n'
  elif [[ "$*" == *'-d teagram_restore_'* ]]; then
    cat > "$RESTORE_CHECK"
  elif [[ "$*" == *'-d teagram'* ]]; then
    cat > "$DATABASE_STATE"
  else
    printf 'unexpected demo_compose_input call: %s\n' "$*" >&2
    return 1
  fi
}
demo_quiesce_and_backup_database
grep -Fxq acknowledged-during-shutdown "$DB_DUMP"
printf 'partial-migration\n' > "$DATABASE_STATE"
demo_restore_database
grep -Fxq acknowledged-during-shutdown "$DATABASE_STATE"
grep -Fxq original-row "$DATABASE_STATE"
[[ "$(cat "$EVENTS")" == $'ingress-stopped\nserver-stopped\ndatabase-dumped' ]]
BASH
env \
  BACKUP_DIR="$TMP_DIR/deploy-backup" \
  TMP_DIR="$TMP_DIR/deploy-backup-tmp" \
  DB_DUMP='' \
  RESTORE_DB='' \
  RESTORE_DB_CREATED=0 \
  BACKUP_READY=0 \
  SERVICES_STOPPED=0 \
  DATABASE_MUTATION=0 \
  SHA=0123456789abcdef0123456789abcdef01234567 \
  BACKUP_FUNCTIONS="$BACKUP_FUNCTIONS" \
  bash "$TMP_DIR/deploy-backup-regression.sh"

MIGRATION_FUNCTION="$TMP_DIR/demo-wait-migration.sh"
sed -n '/^demo_wait_migration() {$/,/^}/p' "$SCRIPT_DIR/demo-common.sh" > "$MIGRATION_FUNCTION"
cat > "$TMP_DIR/migration-wait-regression.sh" <<'BASH'
set -Eeuo pipefail
source "$MIGRATION_FUNCTION"
WAIT_COMPLETE="$TMP_DIR/migration-wait-complete"
MOCK_EXIT_CODE=0
demo_fail() { return 1; }
demo_compose() {
  [[ "$*" == 'ps -a -q migrate' ]]
  printf 'migration-container\n'
}
docker() {
  [[ "$1" == wait && "$2" == migration-container ]]
  sleep 0.1
  : > "$WAIT_COMPLETE"
  printf '%s\n' "$MOCK_EXIT_CODE"
}
demo_wait_migration demo_compose 'fixture migrations'
[[ -f "$WAIT_COMPLETE" ]]
MOCK_EXIT_CODE=17
if demo_wait_migration demo_compose 'fixture migrations'; then
  printf '%s\n' 'migration wait test accepted a failed migration' >&2
  exit 1
fi
BASH
env TMP_DIR="$TMP_DIR" MIGRATION_FUNCTION="$MIGRATION_FUNCTION" \
  bash "$TMP_DIR/migration-wait-regression.sh"

CLEANUP_FUNCTION="$TMP_DIR/init-demo-cleanup.sh"
COMPOSE_CALLS="$TMP_DIR/init-demo-compose-calls"
TMP_WORK_DIR="$TMP_DIR/init-demo-work"
mkdir -p "$TMP_WORK_DIR"
sed -n '/^cleanup() {$/,/^}/p' "$SCRIPT_DIR/init-demo.sh" > "$CLEANUP_FUNCTION"
cat > "$TMP_DIR/init-demo-failure.sh" <<'BASH'
set -Eeuo pipefail
source "$CLEANUP_FUNCTION"
demo_compose() {
  printf '%s\n' "$*" >> "$COMPOSE_CALLS"
}
TMP_DIR="$TMP_WORK_DIR"
PUBLIC_SERVICES_MAY_BE_RUNNING=1
trap cleanup EXIT
false # simulate smoke failure after the public stack starts
BASH
if CLEANUP_FUNCTION="$CLEANUP_FUNCTION" COMPOSE_CALLS="$COMPOSE_CALLS" \
  TMP_WORK_DIR="$TMP_WORK_DIR" bash "$TMP_DIR/init-demo-failure.sh"; then
  printf '%s\n' 'init-demo.sh cleanup test accepted a simulated smoke failure' >&2
  exit 1
fi
grep -Fxq 'stop edge haproxy' "$COMPOSE_CALLS"
[[ ! -e "$TMP_WORK_DIR" ]]
printf 'demo Compose contract holds\n'
