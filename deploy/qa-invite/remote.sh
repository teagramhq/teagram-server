#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

readonly REPO_ROOT=${QA_INVITE_REPO_ROOT:-}
readonly STATE_DIR=${QA_INVITE_STATE_DIR:-/opt/teagram-demo/.state/qa-invite}
readonly ENV_FILE=$STATE_DIR/.env
readonly TLS_DIR=$STATE_DIR/tls
readonly ACCOUNT_DIR=$STATE_DIR/accounts
readonly BACKUP_DIR=$STATE_DIR/backups
readonly COMPOSE_FILE=$REPO_ROOT/deploy/qa-invite/compose.yaml
readonly IMAGE_REPOSITORY=ghcr.io/teagramhq/teagram-server-demo
readonly HOST_ADDRESS=10.96.46.70
readonly HOST_NAME=teagram-demo.incus
TEMP_FILES=()

fail() {
  printf 'qa-invite: %s\n' "$1" >&2
  exit 1
}

cleanup() {
  local path
  for path in "${TEMP_FILES[@]}"; do
    [[ -n "$path" ]] && rm -f -- "$path"
  done
}
trap cleanup EXIT

valid_sha() { [[ "$1" =~ ^[0-9a-f]{40}$ ]]; }
valid_digest() { [[ "$1" =~ ^sha256:[0-9a-f]{64}$ ]]; }
valid_name() { [[ "$1" =~ ^[A-Za-z][A-Za-z0-9_]{1,31}$ ]]; }

[[ -n "$REPO_ROOT" && -f "$COMPOSE_FILE" && ! -L "$COMPOSE_FILE" ]] || fail 'reviewed Compose files are unavailable'
[[ "$(id -u)" -eq 0 ]] || fail 'must run through the restricted demo deploy command'
[[ ! -L /opt/teagram-demo/.state && ! -L "$STATE_DIR" && ! -L "$ACCOUNT_DIR" && ! -L "$BACKUP_DIR" ]] ||
  fail 'qa-invite state path is unsafe'
install -d -o root -g root -m 0700 "$STATE_DIR" "$ACCOUNT_DIR" "$BACKUP_DIR"
[[ "$(stat -c '%u:%a' "$STATE_DIR")" == '0:700' ]] || fail 'qa-invite state directory must be root-only'

env_value() {
  local key=$1
  awk -F= -v key="$key" '$1 == key { sub(/^[^=]*=/, ""); print; found++ } END { if (found != 1) exit 1 }' "$ENV_FILE"
}

validate_env() {
  local count image_ref bind state_dir
  count=$(awk 'NF { count++ } END { print count+0 }' "$ENV_FILE")
  [[ "$count" == 5 ]] || fail 'private env file has an unexpected variable set'
  awk -F= '
    BEGIN {
      allowed["POSTGRES_PASSWORD"]=1
      allowed["TG_AUTHKEY_ENC_KEY"]=1
      allowed["TEAGRAM_QA_INVITE_IMAGE_REF"]=1
      allowed["TEAGRAM_QA_INVITE_BIND_ADDRESS"]=1
      allowed["TEAGRAM_QA_INVITE_STATE_DIR"]=1
    }
    NF != 2 || !allowed[$1] || seen[$1]++ { bad=1 }
    NF == 2 && allowed[$1] { count++ }
    END { if (bad || count != 5) exit 1 }
  ' "$ENV_FILE" || fail 'private env file is malformed'
  image_ref=$(env_value TEAGRAM_QA_INVITE_IMAGE_REF) || fail 'private env file is malformed'
  bind=$(env_value TEAGRAM_QA_INVITE_BIND_ADDRESS) || fail 'private env file is malformed'
  state_dir=$(env_value TEAGRAM_QA_INVITE_STATE_DIR) || fail 'private env file is malformed'
  [[ "$image_ref" =~ ^$IMAGE_REPOSITORY@sha256:[0-9a-f]{64}$ && "$bind" == "$HOST_ADDRESS" &&
    "$state_dir" == "$STATE_DIR" ]] || fail 'private env file contains an invalid endpoint or image reference'
  local postgres_password authkey_enc_key
  postgres_password=$(env_value POSTGRES_PASSWORD) || fail 'private env file is malformed'
  authkey_enc_key=$(env_value TG_AUTHKEY_ENC_KEY) || fail 'private env file is malformed'
  [[ "$postgres_password" =~ ^[0-9a-f]{64}$ && "$authkey_enc_key" =~ ^[0-9a-f]{64}$ &&
    "$postgres_password" != "$authkey_enc_key" ]] || fail 'private env secrets are invalid'
}

write_env() {
  local image_ref=$1 postgres_password authkey_enc_key temp
  if [[ -e "$ENV_FILE" || -L "$ENV_FILE" ]]; then
    [[ -f "$ENV_FILE" && ! -L "$ENV_FILE" && "$(stat -c '%u:%a' "$ENV_FILE")" == '0:600' ]] ||
      fail 'private env file has unsafe ownership or mode'
    validate_env
    postgres_password=$(env_value POSTGRES_PASSWORD)
    authkey_enc_key=$(env_value TG_AUTHKEY_ENC_KEY)
  else
    postgres_password=$(openssl rand -hex 32) || fail 'could not generate database secret'
    authkey_enc_key=$(openssl rand -hex 32) || fail 'could not generate auth-key secret'
    [[ "$postgres_password" != "$authkey_enc_key" ]] || fail 'generated secrets collided'
  fi
  [[ "$image_ref" =~ ^$IMAGE_REPOSITORY@sha256:[0-9a-f]{64}$ ]] || fail 'image reference is invalid'
  temp=$(mktemp "$STATE_DIR/.env.XXXXXXXX")
  TEMP_FILES+=("$temp")
  chmod 0600 "$temp"
  chown root:root "$temp"
  cat > "$temp" <<ENV
POSTGRES_PASSWORD=$postgres_password
TG_AUTHKEY_ENC_KEY=$authkey_enc_key
TEAGRAM_QA_INVITE_IMAGE_REF=$image_ref
TEAGRAM_QA_INVITE_BIND_ADDRESS=$HOST_ADDRESS
TEAGRAM_QA_INVITE_STATE_DIR=$STATE_DIR
ENV
  mv -f -- "$temp" "$ENV_FILE"
  chmod 0600 "$ENV_FILE"
  chown root:root "$ENV_FILE"
}

compose() {
  docker compose \
    --project-name teagram-qa-invite \
    --project-directory "$REPO_ROOT/deploy/qa-invite" \
    --env-file "$ENV_FILE" \
    --file "$COMPOSE_FILE" \
    "$@"
}

# shellcheck source=deploy/qa-invite/rollback.sh
source "$REPO_ROOT/deploy/qa-invite/rollback.sh"

ensure_tls() {
  local key=$TLS_DIR/server.key cert=$TLS_DIR/server.crt key_tmp cert_tmp
  [[ ! -L "$TLS_DIR" ]] || fail 'TLS directory must not be a symlink'
  install -d -o root -g 101 -m 0750 "$TLS_DIR"
  if [[ -e "$key" || -e "$cert" || -L "$key" || -L "$cert" ]]; then
    [[ -f "$key" && ! -L "$key" && "$(stat -c '%u:%g:%a' "$key")" == '0:101:640' &&
      -f "$cert" && ! -L "$cert" && "$(stat -c '%u:%a' "$cert")" == '0:644' ]] ||
      fail 'TLS identity has unsafe type, owner, group, or mode'
    openssl x509 -in "$cert" -noout -checkhost "$HOST_NAME" >/dev/null || fail 'TLS certificate name does not match'
    return 0
  fi
  key_tmp=$(mktemp "$TLS_DIR/server.key.XXXXXXXX")
  cert_tmp=$(mktemp "$TLS_DIR/server.crt.XXXXXXXX")
  TEMP_FILES+=("$key_tmp" "$cert_tmp")
  if ! openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
    -keyout "$key_tmp" -out "$cert_tmp" -days 825 \
    -subj "/CN=$HOST_NAME" \
    -addext "subjectAltName=DNS:$HOST_NAME,IP:$HOST_ADDRESS" \
    -addext 'basicConstraints=critical,CA:FALSE' >/dev/null 2>&1; then
    fail 'could not create the private QA TLS identity'
  fi
  chown root:101 "$key_tmp"
  chmod 0640 "$key_tmp"
  chown root:root "$cert_tmp"
  chmod 0644 "$cert_tmp"
  mv -f -- "$key_tmp" "$key"
  mv -f -- "$cert_tmp" "$cert"
}

verify_image() {
  local sha=$1 digest=$2 ref revision digests
  valid_sha "$sha" || fail 'up requires a full main commit SHA'
  valid_digest "$digest" || fail 'up requires a SHA-256 image digest'
  ref=$IMAGE_REPOSITORY@$digest
  docker pull "$ref" >/dev/null || fail 'could not pull the tested main image'
  revision=$(docker image inspect --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}' "$ref") ||
    fail 'could not read the pulled image revision'
  [[ "$revision" == "$sha" ]] || fail 'image revision does not match the selected main commit'
  digests=$(docker image inspect --format '{{json .RepoDigests}}' "$ref") || fail 'could not read the pulled image digest'
  grep -Fq "$IMAGE_REPOSITORY@$digest" <<< "$digests" || fail 'pulled image digest does not match CI'
  printf '%s\n' "$ref"
}

ensure_identity() {
  local volume_path
  if ! volume_path=$(docker volume inspect --format '{{.Mountpoint}}' teagram-qa-invite-identity 2>/dev/null); then
    compose run --rm --no-deps teagramd bootstrap-identity >/dev/null || {
      printf '%s\n' 'qa-invite: could not create the persistent server identity' >&2
      return 1
    }
  fi
  volume_path=$(docker volume inspect --format '{{.Mountpoint}}' teagram-qa-invite-identity 2>/dev/null) ||
    {
      printf '%s\n' 'qa-invite: persistent identity volume is unavailable' >&2
      return 1
    }
  [[ -f "$volume_path/server_key.pem" && ! -L "$volume_path/server_key.pem" &&
    "$(stat -c '%u:%a' "$volume_path/server_key.pem")" == '65532:600' ]] ||
    {
      printf '%s\n' 'qa-invite: persistent server identity is missing or unsafe' >&2
      return 1
    }
}

backup_database() {
  local stamp dump list
  stamp=$(date -u +%Y%m%dT%H%M%SZ)-$$ || return 1
  dump=$BACKUP_DIR/postgres-$stamp.dump
  list=$BACKUP_DIR/postgres-$stamp.list
  if ! compose exec -T postgres pg_dump -U postgres -Fc teagram </dev/null > "$dump"; then
    rm -f -- "$dump" "$list"
    return 1
  fi
  if ! chown root:root "$dump" || ! chmod 0600 "$dump" || [[ ! -s "$dump" ]] ||
    ! compose exec -T postgres pg_restore --list < "$dump" > "$list"; then
    rm -f -- "$dump" "$list"
    return 1
  fi
  if ! grep -q 'TABLE DATA' "$list"; then
    rm -f -- "$dump" "$list"
    return 1
  fi
  rm -f -- "$list" || return 1
  printf '%s\n' "$dump"
}

probe_endpoint() {
  local document
  document=$(curl --fail --silent --show-error --insecure --max-time 10 \
    --resolve "$HOST_NAME:443:$HOST_ADDRESS" \
    "https://$HOST_NAME/.well-known/teagramd/client") || return 1
  jq -e --arg mtproto "$HOST_NAME:443" --arg websocket "wss://$HOST_NAME/apiws" \
    '.mtproto.endpoint == $mtproto and .websocket.endpoint == $websocket' \
    <<< "$document" >/dev/null
}

do_up() {
  local sha=$1 digest=$2 image_ref previous_ref='' dump='' active_file=$STATE_DIR/active-image temp
  image_ref=$(verify_image "$sha" "$digest")
  if [[ -e "$active_file" || -L "$active_file" ]]; then
    [[ -f "$active_file" && ! -L "$active_file" && "$(stat -c '%u:%a' "$active_file")" == '0:600' ]] ||
      fail 'active image record has unsafe type or mode'
    previous_ref=$(<"$active_file")
    [[ "$previous_ref" =~ ^$IMAGE_REPOSITORY@sha256:[0-9a-f]{64}$ ]] || fail 'active image record is invalid'
  fi
  ensure_tls
  write_env "$image_ref"
  if ! ensure_identity; then
    [[ -z "$previous_ref" ]] || write_env "$previous_ref"
    fail 'could not verify the persistent server identity'
  fi
  if ! compose config --quiet; then
    [[ -z "$previous_ref" ]] || write_env "$previous_ref"
    fail 'resolved QA Compose configuration is invalid'
  fi
  if [[ -f "$active_file" ]]; then
    compose up -d --wait postgres || {
      [[ -z "$previous_ref" ]] || write_env "$previous_ref"
      fail 'Postgres did not become healthy before the safety dump'
    }
    if ! dump=$(backup_database); then
      [[ -z "$previous_ref" ]] || write_env "$previous_ref"
      fail 'could not create a validated rollback database dump'
    fi
  fi
  if ! compose up -d --wait || ! probe_endpoint; then
    rollback_failed_up "$previous_ref" "$dump"
    fail 'QA stack rollout or endpoint check failed; previous state restored where possible'
  fi
  temp=$(mktemp "$STATE_DIR/active-image.XXXXXXXX")
  TEMP_FILES+=("$temp")
  printf '%s\n' "$image_ref" > "$temp"
  chown root:root "$temp"
  chmod 0600 "$temp"
  mv -f -- "$temp" "$active_file"
  printf 'qa-invite=up image=%s endpoint=%s:443 websocket=wss://%s/apiws tls=self-signed turnstile=absent\n' \
    "$digest" "$HOST_NAME" "$HOST_NAME"
}

do_status() {
  local containers
  if [[ ! -f "$ENV_FILE" ]]; then
    printf '%s\n' 'qa-invite=down'
    return 0
  fi
  [[ -f "$ENV_FILE" && ! -L "$ENV_FILE" && "$(stat -c '%u:%a' "$ENV_FILE")" == '0:600' ]] ||
    fail 'private env file has unsafe ownership or mode'
  validate_env
  containers=$(compose ps -q)
  if [[ -z "$containers" ]]; then
    printf '%s\n' 'qa-invite=down persistent volumes retained'
    return 0
  fi
  compose ps --format 'table {{.Name}}\t{{.Service}}\t{{.State}}\t{{.Health}}'
  probe_endpoint || fail 'private endpoint discovery check failed'
  if [[ -f "$STATE_DIR/active-image" && ! -L "$STATE_DIR/active-image" ]]; then
    [[ "$(stat -c '%u:%a' "$STATE_DIR/active-image")" == '0:600' ]] || fail 'active image record has unsafe ownership or mode'
    printf 'image=%s\n' "$(<"$STATE_DIR/active-image")"
  fi
  printf 'endpoint=%s:443 websocket=wss://%s/apiws turnstile=absent\n' "$HOST_NAME" "$HOST_NAME"
}

do_down() {
  [[ -f "$ENV_FILE" && ! -L "$ENV_FILE" ]] || {
    printf '%s\n' 'qa-invite=down'
    return 0
  }
  [[ "$(stat -c '%u:%a' "$ENV_FILE")" == '0:600' ]] || fail 'private env file has unsafe ownership or mode'
  validate_env
  compose down --remove-orphans
  printf '%s\n' 'qa-invite=down persistent volumes and secrets retained'
}

do_create_account() {
  local name=$1 password_file stderr_file containers
  valid_name "$name" || fail 'create-account requires an ASCII account name'
  [[ -f "$ENV_FILE" && ! -L "$ENV_FILE" ]] || fail 'QA stack has not been initialized'
  [[ "$(stat -c '%u:%a' "$ENV_FILE")" == '0:600' ]] || fail 'private env file has unsafe ownership or mode'
  validate_env
  containers=$(compose ps -q teagramd)
  [[ -n "$containers" ]] || fail 'QA server is not running'
  password_file=$(mktemp "$ACCOUNT_DIR/.password.XXXXXXXX")
  stderr_file=$(mktemp "$ACCOUNT_DIR/.create.XXXXXXXX")
  TEMP_FILES+=("$password_file" "$stderr_file")
  cat > "$password_file"
  chown root:root "$password_file" "$stderr_file"
  chmod 0600 "$password_file" "$stderr_file"
  grep -Eq '^[0-9a-f]{64}$' "$password_file" || fail 'password input must be a 64-character hex secret'
  if ! compose exec -T --user 0:0 teagramd /usr/local/bin/teagramd admin create-user \
    --username "$name" < "$password_file" > /dev/null 2> "$stderr_file"; then
    fail 'could not create the requested operator account'
  fi
  grep -Eq "^User created: ${name,,} \\(user id: [0-9]+\\)$" "$stderr_file" ||
    fail 'account creation confirmation was invalid'
  printf 'account-created=%s\n' "${name,,}"
}

case "${1:-}" in
  up)
    [[ $# -eq 3 ]] || fail 'usage: qa-invite up <main-sha> <image-digest>'
    do_up "$2" "$3"
    ;;
  down)
    [[ $# -eq 1 ]] || fail 'usage: qa-invite down'
    do_down
    ;;
  status)
    [[ $# -eq 1 ]] || fail 'usage: qa-invite status'
    do_status
    ;;
  create-account)
    [[ $# -eq 2 ]] || fail 'usage: qa-invite create-account <name>'
    do_create_account "$2"
    ;;
  *) fail 'unknown operation' ;;
esac
