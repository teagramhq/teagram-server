#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -P "$SCRIPT_DIR/../.." && pwd)
SHARED_SECRET_ROOT=/var/lib/teagram-demo

[[ "$EUID" -eq 0 ]] || {
  printf 'isolated moderation Compose test must run as root\n' >&2
  exit 1
}
for command_name in docker jq openssl python3; do
  command -v "$command_name" >/dev/null 2>&1 || {
    printf 'required command is missing: %s\n' "$command_name" >&2
    exit 1
  }
done
docker info >/dev/null 2>&1 || {
  printf 'Docker Engine is unavailable\n' >&2
  exit 1
}
docker compose version --short >/dev/null 2>&1 || {
  printf 'Docker Compose v2 is unavailable\n' >&2
  exit 1
}
[[ ! -e "$SHARED_SECRET_ROOT" && ! -L "$SHARED_SECRET_ROOT" ]] || {
  printf '%s already exists; refusing to touch demo credentials\n' "$SHARED_SECRET_ROOT" >&2
  exit 1
}

TMP_DIR=$(mktemp -d)
SECRET_ROOT_CREATED=0
ACTIVE_FIXTURE=
ACTIVE_PROJECT=
ACTIVE_BLOB_PATH=
ACTIVE_STAGE=

cleanup() {
  local rc=$? fixture_script cleanup_path
  trap - EXIT
  if [[ -n "$ACTIVE_FIXTURE" && -n "$ACTIVE_PROJECT" ]]; then
    fixture_script="$ACTIVE_FIXTURE/deploy/teagram-demo/test-general-moderation-compose.sh"
    if [[ -f "$fixture_script" && -f "$ACTIVE_FIXTURE/deploy/teagram-demo/.env" ]]; then
      SCRIPT_DIR="$ACTIVE_FIXTURE/deploy/teagram-demo"
      # shellcheck source=deploy/teagram-demo/demo-common.sh
      source "$SCRIPT_DIR/demo-common.sh"
      export TEAGRAM_DEMO_BLOB_PATH="$ACTIVE_BLOB_PATH"
      if ((rc != 0)); then
        printf 'isolated moderation Compose failed during %s (exit %s)\n' \
          "${ACTIVE_STAGE:-unknown stage}" "$rc" >&2
        demo_compose ps -a >&2 || true
        demo_compose logs --no-color --tail 80 postgres teagramd migrate haproxy >&2 || true
      fi
      if ! demo_compose down --volumes --remove-orphans; then
        rc=1
      fi
    fi
  fi
  if ((SECRET_ROOT_CREATED)); then
    rm -f -- \
      "$SHARED_SECRET_ROOT/secrets/seed-owner-password" \
      "$SHARED_SECRET_ROOT/secrets/seed-owner.session" \
      "$SHARED_SECRET_ROOT/secrets/seed-owner-created" \
      "$SHARED_SECRET_ROOT/secrets/turnstile-secret-key" \
      "$SHARED_SECRET_ROOT/secrets/turnstile-qa-bypass-token" \
      "$SHARED_SECRET_ROOT/secrets/demo-accounts/inventory.json" \
      "$SHARED_SECRET_ROOT/secrets/demo-accounts/moderation-smoke.session"
    for cleanup_path in \
      "$SHARED_SECRET_ROOT/secrets/demo-accounts" \
      "$SHARED_SECRET_ROOT/secrets" \
      "$SHARED_SECRET_ROOT"; do
      if [[ -e "$cleanup_path" || -L "$cleanup_path" ]]; then
        rmdir -- "$cleanup_path" 2>/dev/null || rc=1
      fi
    done
  fi
  rm -rf -- "$TMP_DIR"
  exit "$rc"
}
trap cleanup EXIT

install -d -o root -g root -m 0700 "$SHARED_SECRET_ROOT/secrets"
SECRET_ROOT_CREATED=1
printf 'fixture-turnstile-secret\n' > "$SHARED_SECRET_ROOT/secrets/turnstile-secret-key"
printf 'fixture-qa-bypass-token-0123456789abcdef\n' > "$SHARED_SECRET_ROOT/secrets/turnstile-qa-bypass-token"
chown root:65532 "$SHARED_SECRET_ROOT/secrets/turnstile-secret-key" \
  "$SHARED_SECRET_ROOT/secrets/turnstile-qa-bypass-token"
chmod 0440 "$SHARED_SECRET_ROOT/secrets/turnstile-secret-key" \
  "$SHARED_SECRET_ROOT/secrets/turnstile-qa-bypass-token"

copy_fixture_checkout() {
  local destination="$1" project_name="$2"
  install -d -o root -g root -m 0755 "$destination/deploy" "$destination/cmd/demo-seed"
  cp -a -- "$SCRIPT_DIR" "$destination/deploy/teagram-demo"
  rm -f -- "$destination/deploy/teagram-demo/.env"
  cp -a -- "$REPO_ROOT/migrations" "$destination/migrations"
  cp -- "$REPO_ROOT/Dockerfile" "$destination/Dockerfile"
  cp -- "$REPO_ROOT/cmd/demo-seed/main.go" "$REPO_ROOT/cmd/demo-seed/seed.go" \
    "$destination/cmd/demo-seed/"
  python3 - "$destination/deploy/teagram-demo/compose.yaml" "$project_name" \
    "$destination/deploy/teagram-demo/demo-common.sh" <<'PY'
import pathlib
import sys

compose = pathlib.Path(sys.argv[1])
contents = compose.read_text(encoding="utf-8")
mount = "    - .state/blob-mode:/run/teagramd/blob-mode:ro\n"
if contents.count(mount) != 1:
    raise SystemExit("isolated Compose fixture expected one local blob-mode authority mount")
# This disposable stack uses local blob volumes and has no rollout journal.
compose.write_text(contents.replace(mount, "", 1), encoding="utf-8")

common = pathlib.Path(sys.argv[3])
contents = common.read_text(encoding="utf-8")
project_flag = "--project-name teagram-demo"
if contents.count(project_flag) != 2:
    raise SystemExit("isolated Compose fixture expected two default project-name arguments")
common.write_text(contents.replace(project_flag, f"--project-name {sys.argv[2]}"), encoding="utf-8")
PY
}

run_signin_mode() {
  local signin_enabled="$1" fixture="$TMP_DIR/checkout-$1" seed_dir owner_password
  local client_config seed_output smoke_output general_chat_enabled
  [[ "$signin_enabled" == true || "$signin_enabled" == false ]] || return 2
  ACTIVE_STAGE="configure sign-in mode $signin_enabled"
  ACTIVE_FIXTURE="$fixture"
  ACTIVE_PROJECT="teagram-demo-moderation-${signin_enabled}-$$"
  copy_fixture_checkout "$fixture" "$ACTIVE_PROJECT"
  ACTIVE_BLOB_PATH="$fixture/blob-data"
  seed_dir="$fixture/deploy/teagram-demo"
  owner_password=$(openssl rand -hex 32)
  general_chat_enabled=true
  if [[ "$signin_enabled" == false ]]; then
    general_chat_enabled=false
  fi
  install -d -o root -g root -m 0700 "$SHARED_SECRET_ROOT/secrets/demo-accounts"
  install -d -o root -g root -m 0755 "$fixture/blob-data"
  cat > "$seed_dir/.env" <<ENV
POSTGRES_PASSWORD=$(openssl rand -hex 32)
TG_AUTHKEY_ENC_KEY=$(openssl rand -hex 32)
TG_PUBLIC_LINK_PREFIX=https://demo.teagram.io/
TG_ADVERTISE_ADDR=demo.teagram.io:443
TG_ADVERTISE_DEMO_GUEST_SIGN_UP=true
# validate-env.py requires General enabled; test the off state via process env.
TG_DEMO_GENERAL_CHAT_ENABLED=true
TG_WEBSOCKET_LISTEN_ADDR=:2446
TG_WEBSOCKET_ALLOWED_ORIGINS=https://web.teagram.io,https://demo.teagram.io
TG_WEBSOCKET_BROWSER_ONLY=true
TG_REGISTRATION=open
TG_TURNSTILE_SIGNIN_ENABLED=$signin_enabled
TG_LOG_LOGIN_CODES=false
TG_MAX_FILE_BYTES=16777216
TG_MAX_USER_STORAGE_BYTES=268435456
TG_UPLOADS_DISABLED=false
TEAGRAM_DEMO_BLOB_PATH=/srv/teagram-demo-blobs/data
TEAGRAM_DEMO_TURNSTILE_SITE_KEY=fixture-site-key
TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE=/var/lib/teagram-demo/secrets/turnstile-secret-key
ENV
  chown root:root "$seed_dir/.env"
  chmod 0600 "$seed_dir/.env"
  printf '%s\n' "$owner_password" > "$SHARED_SECRET_ROOT/secrets/seed-owner-password"
  chown root:root "$SHARED_SECRET_ROOT/secrets/seed-owner-password"
  chmod 0600 "$SHARED_SECRET_ROOT/secrets/seed-owner-password"
  : > "$SHARED_SECRET_ROOT/secrets/seed-owner.session"
  chown root:root "$SHARED_SECRET_ROOT/secrets/seed-owner.session"
  chmod 0600 "$SHARED_SECRET_ROOT/secrets/seed-owner.session"

  export TEAGRAM_DEMO_BLOB_PATH="$ACTIVE_BLOB_PATH"
  # shellcheck source=deploy/teagram-demo/demo-common.sh
  source "$seed_dir/demo-common.sh"
  demo_require_env
  export TG_DEMO_GENERAL_CHAT_ENABLED="$general_chat_enabled"
  ACTIVE_STAGE="bootstrap server identity with sign-in mode $signin_enabled"
  demo_compose --profile maintenance run --rm --no-deps teagramd-maintenance \
    bootstrap-identity >/dev/null
  ACTIVE_STAGE="start private server with sign-in mode $signin_enabled"
  demo_seed_compose up -d --wait --wait-timeout 120 teagramd
  ACTIVE_STAGE="generate client config with sign-in mode $signin_enabled"
  demo_prepare_client_config_dir
  client_config="$TMP_DIR/client-$signin_enabled.json"
  demo_compose --profile maintenance run --rm --no-deps teagramd-maintenance \
    client-config > "$client_config"
  install -o root -g root -m 0644 "$client_config" "$DEMO_CLIENT_CONFIG"
  demo_seed_owner_admin_command() {
    shift
    demo_compose_input --profile maintenance run --rm --no-deps --user 0:65532 \
      teagramd-maintenance admin "$@"
  }
  ACTIVE_STAGE="create seed owner with sign-in mode $signin_enabled"
  demo_prepare_seed_owner "$TMP_DIR"
  if [[ "$general_chat_enabled" == false ]]; then
    ACTIVE_STAGE="seed with General disabled"
    seed_output=$(TG_DEMO_GENERAL_CHAT_ENABLED="$general_chat_enabled" \
      "$seed_dir/seed-demo.sh" --seed --endpoint teagramd:2443 \
      --owner-username demo_seed_owner --owner-password-file "$SHARED_SECRET_ROOT/secrets/seed-owner-password")
    [[ "$seed_output" == *'general_chat=0'* ]] || {
      printf 'flag-off seed unexpectedly reported a General group\n' >&2
      return 1
    }
    assert_general_chat_owner 0 0
    export TG_DEMO_GENERAL_CHAT_ENABLED=true
    ACTIVE_STAGE="enable General and restart the private server"
    demo_seed_compose up -d --force-recreate --wait --wait-timeout 120 teagramd
  fi
  ACTIVE_STAGE="seed demo accounts with sign-in mode $signin_enabled"
  seed_output=$("$seed_dir/seed-demo.sh" --seed --endpoint teagramd:2443 \
    --owner-username demo_seed_owner --owner-password-file "$SHARED_SECRET_ROOT/secrets/seed-owner-password")
  [[ "$seed_output" == *'demo seed verified'* ]] || {
    printf 'private demo seed did not complete in sign-in mode %s\n' "$signin_enabled" >&2
    return 1
  }
  assert_general_chat_owner 1 1
  if [[ "$signin_enabled" == false ]]; then
    ACTIVE_STAGE="verify General convergence after init, reseed, and restart"
    seed_general_count_decoys
    "$seed_dir/test-seed-compose.sh"
    assert_general_chat_owner 1 1

    ACTIVE_STAGE="reset isolated database and reseed General"
    demo_compose down --volumes --remove-orphans
    rm -rf -- "$ACTIVE_BLOB_PATH"
    install -d -o root -g root -m 0755 "$ACTIVE_BLOB_PATH"
    demo_reset_seed_owner_marker
    rm -f -- "$SHARED_SECRET_ROOT/secrets/demo-accounts/inventory.json"
    demo_compose --profile maintenance run --rm --no-deps teagramd-maintenance \
      bootstrap-identity >/dev/null
    demo_seed_compose up -d --wait --wait-timeout 120 teagramd
    demo_prepare_client_config_dir
    client_config="$TMP_DIR/client-reset-$signin_enabled.json"
    demo_compose --profile maintenance run --rm --no-deps teagramd-maintenance \
      client-config > "$client_config"
    install -o root -g root -m 0644 "$client_config" "$DEMO_CLIENT_CONFIG"
    demo_prepare_seed_owner "$TMP_DIR"
    ACTIVE_STAGE="seed demo accounts after database reset"
    seed_output=$("$seed_dir/seed-demo.sh" --seed --endpoint teagramd:2443 \
      --owner-username demo_seed_owner --owner-password-file "$SHARED_SECRET_ROOT/secrets/seed-owner-password")
    [[ "$seed_output" == *'general_chat=1'* ]] || {
      printf 'database-reset seed did not report General enabled\n' >&2
      return 1
    }
    assert_general_chat_owner 1 1
  fi
  # Isolate this username-based moderation smoke from lookup budget spent by seeding.
  demo_compose exec -T postgres psql -v ON_ERROR_STOP=1 -U postgres -d teagram \
    -c "DELETE FROM rate_limits; DELETE FROM send_code_ip_calls; DELETE FROM send_code_ip_phones; DELETE FROM public_send_code_ip_calls; DELETE FROM public_send_code_ip_phones; DELETE FROM username_lookups WHERE caller_id = (SELECT id FROM users WHERE username = 'demo_seed_owner');"
  demo_prepare_haproxy_runtime
  ACTIVE_STAGE="moderation smoke with sign-in mode $signin_enabled"
  smoke_output=$("$seed_dir/test-general-moderation-compose.sh" \
    teagramd:2443 "$(openssl rand -hex 16)")
  for evidence in \
    'general moderation smoke verified deleted post and refused banned guest rejoin' \
    'general moderation smoke post removed and guest membership restored' \
    'production moderation Compose smoke=verified message_deleted=true short_handle_ban=true'; do
    [[ "$smoke_output" == *"$evidence"* ]] || {
      printf 'moderation smoke did not report %s in sign-in mode %s\n' "$evidence" "$signin_enabled" >&2
      return 1
    }
  done
  [[ ! -e "$SHARED_SECRET_ROOT/secrets/demo-accounts/moderation-smoke.session" ]] || {
    printf 'moderation cleanup retained its guest session in sign-in mode %s\n' "$signin_enabled" >&2
    return 1
  }
  ACTIVE_STAGE="clean isolated Compose project with sign-in mode $signin_enabled"
  printf '%s\n' "$smoke_output"
  demo_compose down --volumes --remove-orphans
  ACTIVE_FIXTURE=
  ACTIVE_PROJECT=
  ACTIVE_BLOB_PATH=
  ACTIVE_STAGE=
  rm -f -- \
    "$SHARED_SECRET_ROOT/secrets/seed-owner-password" \
    "$SHARED_SECRET_ROOT/secrets/seed-owner.session" \
    "$SHARED_SECRET_ROOT/secrets/seed-owner-created" \
    "$SHARED_SECRET_ROOT/secrets/demo-accounts/inventory.json"
  rmdir -- "$SHARED_SECRET_ROOT/secrets/demo-accounts"
}

assert_general_chat_owner() {
  local expected_groups="$1" expected_owner_groups="$2" actual
  actual=$(demo_compose exec -T postgres psql -Atq -v ON_ERROR_STOP=1 -U postgres -d teagram \
    -c "SELECT count(*)::text || '|' || count(*) FILTER (WHERE c.deleted_at IS NULL AND creator.username = 'demo_seed_owner')::text FROM channels c JOIN users creator ON creator.id = c.creator_id WHERE c.title = 'General' AND c.about = 'An open chat for demo guests to meet and talk.' AND c.megagroup AND c.deleted_at IS NULL AND creator.username = 'demo_seed_owner'")
  [[ "$actual" == "$expected_groups|$expected_owner_groups" ]] || {
    printf 'expected %s General groups with %s owned by demo_seed_owner; got %s\n' \
      "$expected_groups" "$expected_owner_groups" "$actual" >&2
    return 1
  }
}

seed_general_count_decoys() {
  demo_compose exec -T postgres psql -v ON_ERROR_STOP=1 -U postgres -d teagram -c "
    WITH fixture_channel_id AS (
      SELECT COALESCE(MAX(id) FILTER (WHERE id < 2147483648), 0) + 1 AS id
      FROM channels
    )
    INSERT INTO channels (id, title, about, creator_id, megagroup, deleted_at)
    SELECT fixture_channel_id.id, source.title, source.about, seed_owner.id, true, clock_timestamp()
    FROM channels source
    CROSS JOIN fixture_channel_id
    JOIN users seed_owner ON seed_owner.username = 'demo_seed_owner'
    WHERE source.title = 'General'
      AND source.about = 'An open chat for demo guests to meet and talk.'
      AND source.megagroup
      AND source.deleted_at IS NULL
    ORDER BY source.id
    LIMIT 1;

    WITH fixture_channel_id AS (
      SELECT COALESCE(MAX(id) FILTER (WHERE id < 2147483648), 0) + 1 AS id
      FROM channels
    )
    INSERT INTO channels (id, title, about, creator_id, megagroup)
    SELECT fixture_channel_id.id, source.title, source.about, guest.id, true
    FROM channels source
    CROSS JOIN fixture_channel_id
    JOIN users guest ON guest.username = 'demo_iona'
    WHERE source.title = 'General'
      AND source.about = 'An open chat for demo guests to meet and talk.'
      AND source.megagroup
      AND source.deleted_at IS NULL
    ORDER BY source.id
    LIMIT 1;
  " >/dev/null
  local actual
  actual=$(demo_compose exec -T postgres psql -Atq -v ON_ERROR_STOP=1 -U postgres -d teagram \
    -c "SELECT count(*)::text || '|' || count(*) FILTER (WHERE c.deleted_at IS NOT NULL)::text || '|' || count(*) FILTER (WHERE c.deleted_at IS NULL AND guest.username = 'demo_iona')::text FROM channels c JOIN users guest ON guest.id = c.creator_id WHERE c.title = 'General' AND c.about = 'An open chat for demo guests to meet and talk.' AND c.megagroup AND (c.deleted_at IS NOT NULL OR guest.username = 'demo_iona')")
  [[ "$actual" == '2|1|1' ]] || {
    printf 'expected deleted and guest-owned General count decoys, got %s\n' "$actual" >&2
    return 1
  }
}

run_signin_mode false
run_signin_mode true
