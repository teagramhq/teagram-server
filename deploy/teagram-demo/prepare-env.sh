#!/usr/bin/env bash
set -euo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ENV_FILE="$SCRIPT_DIR/.env"
SECRET_DIR="/var/lib/teagram-demo/secrets"
SECRET_FILE="$SECRET_DIR/turnstile-secret-key"
# shellcheck source=deploy/teagram-demo/demo-common.sh
source "$SCRIPT_DIR/demo-common.sh"

if [[ "$EUID" -ne 0 ]]; then
  echo 'prepare-env.sh must run as root on the demo LXC' >&2
  exit 1
fi
if [[ -e "$ENV_FILE" || -L "$ENV_FILE" ]]; then
  echo 'deploy/teagram-demo/.env already exists; refusing to replace secrets' >&2
  exit 1
fi
if [[ -e "$SECRET_FILE" || -L "$SECRET_FILE" ]]; then
  echo 'Turnstile secret file already exists; refusing to replace it' >&2
  exit 1
fi

printf 'Demo Turnstile site key: ' >&2
IFS= read -r turnstile_site_key
printf 'Demo Turnstile secret key: ' >&2
IFS= read -r -s turnstile_secret_key
printf '\n' >&2
[[ "$turnstile_site_key" =~ ^[A-Za-z0-9_-]+$ ]] || {
  echo 'Turnstile site key has an unsupported format' >&2
  exit 1
}
[[ "$turnstile_secret_key" =~ ^[A-Za-z0-9_-]+$ ]] || {
  echo 'Turnstile secret key has an unsupported format' >&2
  exit 1
}
[[ "$turnstile_site_key" != "$turnstile_secret_key" ]] || {
  echo 'Turnstile site and secret keys must be different' >&2
  exit 1
}

postgres_password=$(openssl rand -hex 32)
authkey_encryption_key=$(openssl rand -hex 32)
temporary="$ENV_FILE.tmp.$$"
secret_temporary=
secret_installed=0
env_installed=0
cleanup() {
  [[ -z "$temporary" ]] || rm -f -- "$temporary"
  [[ -z "$secret_temporary" ]] || rm -f -- "$secret_temporary"
  if (( secret_installed == 1 && env_installed == 0 )); then
    rm -f -- "$SECRET_FILE"
  fi
}
trap cleanup EXIT
[[ ! -L "$SECRET_DIR" ]] || {
  echo 'Turnstile secret directory must not be a symlink' >&2
  exit 1
}
install -d -o root -g root -m 0700 "$SECRET_DIR"
demo_ensure_qa_bypass_token
secret_temporary=$(mktemp "$SECRET_DIR/turnstile-secret-key.XXXXXXXX")
printf '%s\n' "$turnstile_secret_key" > "$secret_temporary"
chown root:65532 "$secret_temporary"
chmod 0440 "$secret_temporary"
cat > "$temporary" <<ENV
POSTGRES_PASSWORD=$postgres_password
TG_AUTHKEY_ENC_KEY=$authkey_encryption_key
TG_PUBLIC_LINK_PREFIX=https://demo.teagram.io/
TG_ADVERTISE_ADDR=demo.teagram.io:443
TG_ADVERTISE_DEMO_GUEST_SIGN_UP=true
TG_WEBSOCKET_LISTEN_ADDR=:2446
TG_WEBSOCKET_ALLOWED_ORIGINS=https://web.teagram.io,https://demo.teagram.io
TG_REGISTRATION=open
TG_LOG_LOGIN_CODES=false
TG_MAX_FILE_BYTES=16777216
TG_MAX_USER_STORAGE_BYTES=268435456
TG_UPLOADS_DISABLED=true
TEAGRAM_DEMO_BLOB_PATH=/srv/teagram-demo-blobs/data
TEAGRAM_DEMO_TURNSTILE_SITE_KEY=$turnstile_site_key
TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE=$SECRET_FILE
ENV
chmod 0600 "$temporary"
chown root:root "$temporary"
mv -- "$secret_temporary" "$SECRET_FILE"
secret_temporary=
secret_installed=1
mv -- "$temporary" "$ENV_FILE"
temporary=
env_installed=1
unset postgres_password authkey_encryption_key turnstile_site_key turnstile_secret_key
trap - EXIT
cleanup
python3 "$SCRIPT_DIR/validate-env.py" "$ENV_FILE"
echo 'created deploy/teagram-demo/.env and runtime-readable Turnstile secret and QA bypass files under its root-only directory; secrets are not displayed'
