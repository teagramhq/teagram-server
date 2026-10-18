#!/usr/bin/env bash
set -euo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ENV_FILE="$SCRIPT_DIR/.env"

if [[ "$EUID" -ne 0 ]]; then
  echo 'prepare-env.sh must run as root on the demo LXC' >&2
  exit 1
fi
if [[ -e "$ENV_FILE" || -L "$ENV_FILE" ]]; then
  echo 'deploy/teagram-demo/.env already exists; refusing to replace secrets' >&2
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
trap 'rm -f -- "$temporary"' EXIT
cat > "$temporary" <<ENV
POSTGRES_PASSWORD=$postgres_password
TG_AUTHKEY_ENC_KEY=$authkey_encryption_key
TG_PUBLIC_LINK_PREFIX=https://demo.teagram.io/
TG_ADVERTISE_ADDR=demo.teagram.io:443
TG_WEBSOCKET_LISTEN_ADDR=:2446
TG_WEBSOCKET_ALLOWED_ORIGINS=https://web.teagram.io
TG_REGISTRATION=open
TG_LOG_LOGIN_CODES=false
TG_MAX_FILE_BYTES=16777216
TG_MAX_USER_STORAGE_BYTES=268435456
TEAGRAM_DEMO_BLOB_PATH=/srv/teagram-demo-blobs/data
TEAGRAM_DEMO_TURNSTILE_SITE_KEY=$turnstile_site_key
TEAGRAM_DEMO_TURNSTILE_SECRET_KEY=$turnstile_secret_key
ENV
chmod 0600 "$temporary"
chown root:root "$temporary"
mv -- "$temporary" "$ENV_FILE"
unset postgres_password authkey_encryption_key turnstile_site_key turnstile_secret_key
trap - EXIT
python3 "$SCRIPT_DIR/validate-env.py" "$ENV_FILE"
echo 'created deploy/teagram-demo/.env (mode 0600); secrets are not displayed'
