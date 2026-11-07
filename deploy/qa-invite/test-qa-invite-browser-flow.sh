#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -P "$SCRIPT_DIR/../.." && pwd)
WEB_ROOT=${REAL_CLIENT_WEB_ROOT:-}
WEB_REVISION=${REAL_CLIENT_WEB_REVISION:-}
PUBLIC_WEB_ROOT=${QA_WEB_SOURCE_DIR:-}
HOST_NAME=teagram-demo.incus
HOST_ADDRESS=127.0.0.1
CERT_ADDRESS=10.96.46.70
RUN_ID=$(openssl rand -hex 8)
PROJECT=qa-invite-smoke-$RUN_ID
IMAGE_REF=localhost/teagram-server-qa-smoke:$RUN_ID
TMP_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/qa-invite-smoke.XXXXXXXX")
STATE_DIR=$TMP_ROOT/state
WEB_CLIENT_DIR=$STATE_DIR/public-client
TLS_IDENTITY_DIR=$STATE_DIR/tls/identity
TLS_PRIVATE_DIR=$TLS_IDENTITY_DIR/private
TLS_PUBLIC_DIR=$TLS_IDENTITY_DIR/public
CLIENT_DIR=$STATE_DIR/client
TLS_DIR=$STATE_DIR/tls
CLIENT_CONFIG=$CLIENT_DIR/teagram-config.json
ENV_FILE=$TMP_ROOT/qa-invite.env
COMPOSE_FILE=$TMP_ROOT/compose.yaml
IMAGE_BUILD_LOG=$TMP_ROOT/server-image-build.log
TEST_HOME=$TMP_ROOT/home
ACCOUNT_FILE=$TMP_ROOT/account-password
COMPOSE_READY=0
IMAGE_BUILT=0

fail() {
  printf 'qa-invite browser flow: %s\n' "$1" >&2
  if ((COMPOSE_READY)); then compose logs --no-color --tail 100 >&2 || true; fi
  exit 1
}

compose() {
  docker compose \
    --project-name "$PROJECT" \
    --project-directory "$SCRIPT_DIR" \
    --env-file "$ENV_FILE" \
    --file "$COMPOSE_FILE" \
    "$@"
}

# shellcheck source=deploy/qa-invite/tls.sh
source "$SCRIPT_DIR/tls.sh"

cleanup() {
  local result=$?
  trap - EXIT
  if ((COMPOSE_READY)); then
    if ! compose down --volumes --remove-orphans >/dev/null; then
      printf 'qa-invite browser flow: isolated Compose cleanup failed\n' >&2
      result=1
    fi
  fi
  if ((IMAGE_BUILT)) && docker image inspect "$IMAGE_REF" >/dev/null 2>&1; then
    if ! docker image rm "$IMAGE_REF" >/dev/null; then result=1; fi
  fi
  if ! sudo rm -rf -- "$TMP_ROOT"; then
    printf 'qa-invite browser flow: isolated temporary state cleanup failed\n' >&2
    result=1
  fi
  exit "$result"
}
trap cleanup EXIT

[[ -n "$WEB_ROOT" && -d "$WEB_ROOT" && ! -L "$WEB_ROOT" ]] || fail 'pinned teagram-web checkout is unavailable'
[[ "$WEB_REVISION" =~ ^[0-9a-f]{40}$ ]] || fail 'pinned teagram-web revision is invalid'
[[ "$(git -C "$WEB_ROOT" rev-parse HEAD)" == "$WEB_REVISION" ]] || fail 'teagram-web checkout does not match its pinned revision'
[[ -n "$PUBLIC_WEB_ROOT" && -d "$PUBLIC_WEB_ROOT" && ! -L "$PUBLIC_WEB_ROOT" ]] || fail 'pinned demo Web checkout is unavailable'
PUBLIC_WEB_ROOT=$(cd -P "$PUBLIC_WEB_ROOT" && pwd)
PIN_SOURCE_SHA=${QA_WEB_SOURCE_SHA:-}
PIN_ARTIFACT_DIGEST=${QA_WEB_ARTIFACT_DIGEST:-}
[[ "$PIN_SOURCE_SHA" =~ ^[0-9a-f]{40}$ ]] || fail 'demo Web source identity is invalid'
[[ "$PIN_ARTIFACT_DIGEST" =~ ^sha256:[0-9a-f]{64}$ ]] || fail 'demo Web artifact identity is invalid'
WEB_IDENTITY=$(python3 "$REPO_ROOT/deploy/teagram-demo/resolve_web_artifact_pin.py" --verify-release-identity "$PIN_SOURCE_SHA" "$PIN_ARTIFACT_DIGEST") ||
  fail 'demo Web release identity does not match the published pin'
WEB_EXPECTED_ENDPOINT=$(jq -er '.endpoint | strings' <<< "$WEB_IDENTITY") || fail 'demo Web endpoint is invalid'
WEB_EXPECTED_FINGERPRINT=$(jq -er '.fingerprint | strings' <<< "$WEB_IDENTITY") || fail 'demo Web fingerprint is invalid'
[[ "$(git -C "$PUBLIC_WEB_ROOT" rev-parse HEAD)" == "$PIN_SOURCE_SHA" ]] || fail 'demo Web checkout does not match the deployed pin'
[[ -d "$PUBLIC_WEB_ROOT/dist-private" && ! -L "$PUBLIC_WEB_ROOT/dist-private" ]] || fail 'pinned demo Web artifact is unavailable'
PUBLIC_TARGET=$PUBLIC_WEB_ROOT/ci/private-mtproto-target.json
PUBLIC_KEY_REL=$(jq -er '.MTPROTO_PRIVATE_RSA_PUBLIC_KEY_FILE | select(type == "string" and length > 0)' "$PUBLIC_TARGET") || fail 'demo Web target key path is invalid'
PUBLIC_KEY_PATH=$(realpath -e -- "$PUBLIC_WEB_ROOT/$PUBLIC_KEY_REL") || fail 'demo Web target key is unavailable'
[[ "$PUBLIC_KEY_PATH" == "$PUBLIC_WEB_ROOT/"* && -f "$PUBLIC_KEY_PATH" ]] || fail 'demo Web target key is outside the pinned checkout'
PUBLIC_KEY_SHA=$(openssl pkey -pubin -in "$PUBLIC_KEY_PATH" -outform DER | sha256sum | cut -d' ' -f1) || fail 'demo Web target key is invalid'
command -v certutil >/dev/null || fail 'NSS certutil is required for trusted browser TLS'
command -v sudo >/dev/null || fail 'sudo is required to prepare the read-only nginx TLS mount'

chmod 0755 "$TMP_ROOT"
mkdir -p "$TLS_PRIVATE_DIR" "$TLS_PUBLIC_DIR" "$CLIENT_DIR" "$WEB_CLIENT_DIR/web/releases" "$TEST_HOME/.pki/nssdb"
chmod 0755 "$STATE_DIR" "$STATE_DIR/tls" "$TLS_IDENTITY_DIR" "$TLS_PUBLIC_DIR" "$CLIENT_DIR" "$WEB_CLIENT_DIR" "$WEB_CLIENT_DIR/web" "$WEB_CLIENT_DIR/web/releases"
chmod 0700 "$TLS_PRIVATE_DIR" "$TEST_HOME"

tar -czf "$TMP_ROOT/demo-web.tar.gz" -C "$PUBLIC_WEB_ROOT/dist-private" . || fail 'could not archive the pinned demo Web artifact'
WEB_ARCHIVE_SHA=$(sha256sum "$TMP_ROOT/demo-web.tar.gz" | cut -d' ' -f1)
WEB_RELEASE=$(DEPLOY_WEB_ARCHIVE_SHA256="$WEB_ARCHIVE_SHA" \
  DEPLOY_WEB_EXPECTED_ENDPOINT="$WEB_EXPECTED_ENDPOINT" \
  DEPLOY_WEB_EXPECTED_FINGERPRINT="$WEB_EXPECTED_FINGERPRINT" \
  python3 "$REPO_ROOT/deploy/teagram-demo/install-web-release.py" stage \
  "$TMP_ROOT/demo-web.tar.gz" "$WEB_CLIENT_DIR/web/releases" "$PIN_SOURCE_SHA" "$PIN_ARTIFACT_DIGEST") ||
  fail 'pinned demo Web artifact did not pass deployment verification'
[[ "$WEB_RELEASE" == "$PIN_SOURCE_SHA-${PIN_ARTIFACT_DIGEST#sha256:}" ]] || fail 'staged demo Web release identity changed'
ln -s -- "releases/$WEB_RELEASE" "$WEB_CLIENT_DIR/web/current"
jq -e --arg host "$HOST_NAME" --slurpfile target "$PUBLIC_TARGET" \
  '.mode == "private" and .endpoint == $target[0].MTPROTO_PRIVATE_ENDPOINT and .fingerprint == $target[0].fingerprint and .endpoint != ("wss://" + $host + "/apiws")' \
  "$WEB_CLIENT_DIR/web/current/mtproto-target.json" >/dev/null || fail 'demo Web artifact does not retain its different embedded target'

openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$TLS_PRIVATE_DIR/ca.key" >/dev/null 2>&1 || fail 'could not create the isolated QA root CA'
openssl req -x509 -new -key "$TLS_PRIVATE_DIR/ca.key" -sha256 -days 2 \
  -subj '/CN=Teagram QA Invite Browser Test Root CA' -out "$TLS_PUBLIC_DIR/ca.crt" \
  -addext 'basicConstraints=critical,CA:TRUE,pathlen:0' \
  -addext 'keyUsage=critical,keyCertSign,cRLSign' \
  -addext 'extendedKeyUsage=critical,serverAuth' \
  -addext 'subjectKeyIdentifier=hash' \
  -addext "$(qa_tls_name_constraints "$HOST_NAME" "$CERT_ADDRESS")" >/dev/null 2>&1 || fail 'could not issue the isolated QA root CA'
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$TLS_PUBLIC_DIR/server.key" >/dev/null 2>&1 || fail 'could not create the isolated QA server key'
openssl req -new -key "$TLS_PUBLIC_DIR/server.key" -subj "/CN=$HOST_NAME" -out "$TMP_ROOT/server.csr" >/dev/null 2>&1 || fail 'could not create the QA server certificate request'
cat > "$TMP_ROOT/server.ext" <<EXT
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectAltName=DNS:$HOST_NAME,IP:$CERT_ADDRESS
subjectKeyIdentifier=hash
authorityKeyIdentifier=keyid,issuer
EXT
openssl x509 -req -in "$TMP_ROOT/server.csr" -CA "$TLS_PUBLIC_DIR/ca.crt" -CAkey "$TLS_PRIVATE_DIR/ca.key" \
  -CAcreateserial -CAserial "$TLS_PRIVATE_DIR/ca.srl" -days 2 -sha256 -extfile "$TMP_ROOT/server.ext" \
  -out "$TLS_PUBLIC_DIR/server-leaf.crt" >/dev/null 2>&1 || fail 'could not issue the QA server certificate'
cat "$TLS_PUBLIC_DIR/server-leaf.crt" "$TLS_PUBLIC_DIR/ca.crt" > "$TLS_PUBLIC_DIR/server.crt"
rm -f -- "$TLS_PRIVATE_DIR/ca.srl" "$TMP_ROOT/server.csr" "$TMP_ROOT/server.ext"
chmod 0600 "$TLS_PRIVATE_DIR/ca.key"
chmod 0644 "$TLS_PUBLIC_DIR/ca.crt" "$TLS_PUBLIC_DIR/server-leaf.crt" "$TLS_PUBLIC_DIR/server.crt"
chmod 0640 "$TLS_PUBLIC_DIR/server.key"
sudo chown root:101 "$TLS_DIR" "$TLS_PUBLIC_DIR" "$TLS_PUBLIC_DIR/server.key"
sudo chown root:root "$TLS_PUBLIC_DIR/ca.crt" "$TLS_PUBLIC_DIR/server-leaf.crt" "$TLS_PUBLIC_DIR/server.crt"
openssl verify -CAfile "$TLS_PUBLIC_DIR/ca.crt" -purpose sslserver -verify_hostname "$HOST_NAME" "$TLS_PUBLIC_DIR/server-leaf.crt" >/dev/null ||
  fail 'isolated QA TLS chain does not verify for the configured hostname'
openssl verify -CAfile "$TLS_PUBLIC_DIR/ca.crt" -purpose sslserver -verify_ip "$CERT_ADDRESS" "$TLS_PUBLIC_DIR/server-leaf.crt" >/dev/null ||
  fail 'isolated QA TLS chain does not verify for the configured IP'

openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$TMP_ROOT/outside.key" >/dev/null 2>&1 || fail 'could not create the constraint probe key'
openssl req -new -key "$TMP_ROOT/outside.key" -subj '/CN=outside-qa.invalid' -out "$TMP_ROOT/outside.csr" >/dev/null 2>&1 ||
  fail 'could not create the constraint probe request'
printf 'basicConstraints=critical,CA:FALSE\nextendedKeyUsage=serverAuth\nsubjectAltName=DNS:outside-qa.invalid\n' > "$TMP_ROOT/outside.ext"
openssl x509 -req -in "$TMP_ROOT/outside.csr" -CA "$TLS_PUBLIC_DIR/ca.crt" -CAkey "$TLS_PRIVATE_DIR/ca.key" \
  -CAcreateserial -CAserial "$TLS_PRIVATE_DIR/outside.srl" -days 2 -sha256 -extfile "$TMP_ROOT/outside.ext" \
  -out "$TMP_ROOT/outside.crt" >/dev/null 2>&1 || fail 'could not create the constraint probe certificate'
openssl req -x509 -new -key "$TLS_PRIVATE_DIR/ca.key" -sha256 -days 2 \
  -subj '/CN=Teagram QA Invite Browser Test Root CA' -out "$TMP_ROOT/unconstrained.crt" \
  -addext 'basicConstraints=critical,CA:TRUE,pathlen:0' -addext 'keyUsage=critical,keyCertSign,cRLSign' -addext 'extendedKeyUsage=critical,serverAuth' \
  -addext 'subjectKeyIdentifier=hash' >/dev/null 2>&1 || fail 'could not create the constraint control root'
openssl verify -CAfile "$TMP_ROOT/unconstrained.crt" -purpose sslserver -verify_hostname outside-qa.invalid "$TMP_ROOT/outside.crt" >/dev/null ||
  fail 'the constraint probe certificate is otherwise invalid'
if LC_ALL=C openssl verify -CAfile "$TLS_PUBLIC_DIR/ca.crt" -purpose sslserver -verify_hostname outside-qa.invalid "$TMP_ROOT/outside.crt" > "$TMP_ROOT/outside.verify" 2>&1; then
  fail 'the QA root accepted an out-of-scope DNS name'
fi
grep -Fq 'permitted subtree violation' "$TMP_ROOT/outside.verify" || fail 'the constraint probe failed for an unexpected reason'

certutil -d "sql:$TEST_HOME/.pki/nssdb" -N --empty-password >/dev/null 2>&1 || fail 'could not create the isolated Chromium trust database'
certutil -d "sql:$TEST_HOME/.pki/nssdb" -A -t 'C,,' -n 'Teagram QA Invite Test Root' -i "$TLS_PUBLIC_DIR/ca.crt" ||
  fail 'could not trust the isolated QA root in Chromium'

cat > "$CLIENT_CONFIG" <<CONFIG
{"server":"$HOST_NAME","autoGuest":false}
CONFIG
chmod 0644 "$CLIENT_CONFIG"
openssl rand -hex 32 > "$TMP_ROOT/postgres-password"
openssl rand -hex 32 > "$TMP_ROOT/authkey-encryption-key"
openssl rand -hex 32 > "$ACCOUNT_FILE"
chmod 0600 "$TMP_ROOT/postgres-password" "$TMP_ROOT/authkey-encryption-key" "$ACCOUNT_FILE"
cat > "$ENV_FILE" <<ENV
POSTGRES_PASSWORD=$(<"$TMP_ROOT/postgres-password")
TG_AUTHKEY_ENC_KEY=$(<"$TMP_ROOT/authkey-encryption-key")
TEAGRAM_QA_INVITE_IMAGE_REF=$IMAGE_REF
TEAGRAM_QA_INVITE_BIND_ADDRESS=$HOST_ADDRESS
TEAGRAM_QA_INVITE_STATE_DIR=$STATE_DIR
ENV
chmod 0600 "$ENV_FILE"

sed \
  -e "s/^name: teagram-qa-invite$/name: $PROJECT/" \
  -e "s/name: teagram-qa-invite-postgres/name: $PROJECT-postgres/" \
  -e "s/name: teagram-qa-invite-identity/name: $PROJECT-identity/" \
  -e "s/name: teagram-qa-invite-blobs/name: $PROJECT-blobs/" \
  -e "s#source: /opt/teagram-demo/.state/teagram-demo/public-client#source: $WEB_CLIENT_DIR#" \
  "$SCRIPT_DIR/compose.yaml" > "$COMPOSE_FILE"
grep -Fq "name: $PROJECT" "$COMPOSE_FILE" || fail 'isolated Compose project name was not set'
grep -Fq "source: $WEB_CLIENT_DIR" "$COMPOSE_FILE" || fail 'QA Web artifact mount was not redirected to isolated state'
[[ -s "$REPO_ROOT/build/sticker-bundle/manifest.json" ]] || fail 'validated sticker bundle is unavailable'

if ! docker buildx build --progress=plain \
  --build-context sticker-bundle="$REPO_ROOT/build" \
  --build-arg STICKER_BUNDLE_SOURCE=sticker-bundle \
  --tag "$IMAGE_REF" --load "$REPO_ROOT" >"$IMAGE_BUILD_LOG" 2>&1; then
  cat "$IMAGE_BUILD_LOG" >&2
  fail 'could not build the server image for the isolated invite stack'
fi
IMAGE_BUILT=1
compose pull postgres migrate tls-edge haproxy >/dev/null || fail 'could not pull the pinned QA stack support images'
COMPOSE_READY=1
compose run --rm --no-deps teagramd bootstrap-identity >/dev/null || fail 'could not initialize the isolated server identity'
compose up -d --wait || fail 'isolated invite stack did not become healthy'

DISCOVERY_FILE=$WEB_CLIENT_DIR/client
compose exec -T teagramd /usr/local/bin/teagramd client-config > "$DISCOVERY_FILE" || fail 'QA discovery configuration could not be read from the invite stack'
jq -e --arg host "$HOST_NAME" '.mtproto.endpoint == ($host + ":443") and .websocket.endpoint == ("wss://" + $host + "/apiws")' \
  "$DISCOVERY_FILE" >/dev/null || fail 'QA invite stack advertised unexpected MTProto or WebSocket endpoints'
jq -er '.mtproto.rsa_spki' "$DISCOVERY_FILE" | base64 -d > "$TMP_ROOT/server-public-key.der" ||
  fail 'QA discovery did not provide a usable server public key'
openssl pkey -pubin -inform DER -in "$TMP_ROOT/server-public-key.der" -noout >/dev/null 2>&1 ||
  fail 'QA discovery did not provide a usable server public key'
QA_KEY_SHA=$(sha256sum "$TMP_ROOT/server-public-key.der" | cut -d' ' -f1)
[[ "$QA_KEY_SHA" != "$PUBLIC_KEY_SHA" ]] || fail 'QA server key unexpectedly matches the demo Web embedded key'

compose exec -T --user 0:0 teagramd /usr/local/bin/teagramd admin create-user --username qa_browser_smoke < "$ACCOUNT_FILE" >/dev/null 2>&1 ||
  fail 'could not create the isolated QA browser account'

curl --fail --silent --show-error --cacert "$TLS_PUBLIC_DIR/ca.crt" --resolve "$HOST_NAME:443:$HOST_ADDRESS" \
  "https://$HOST_NAME/.well-known/teagramd/client" > "$TMP_ROOT/served-discovery.json" || fail 'trusted TLS discovery request failed'
jq -e --arg host "$HOST_NAME" '.mtproto.endpoint == ($host + ":443") and .websocket.endpoint == ("wss://" + $host + "/apiws")' \
  "$TMP_ROOT/served-discovery.json" >/dev/null || fail 'TLS edge served an unexpected discovery document'
curl --fail --silent --show-error --cacert "$TLS_PUBLIC_DIR/ca.crt" --resolve "$HOST_NAME:443:$HOST_ADDRESS" \
  "https://$HOST_NAME/teagram-config.json" > "$TMP_ROOT/served-web-config.json" || fail 'trusted TLS Web configuration request failed'
jq -e --arg host "$HOST_NAME" '.server == $host and .autoGuest == false' "$TMP_ROOT/served-web-config.json" >/dev/null ||
  fail 'TLS edge served an unexpected Web configuration'
curl --fail --silent --show-error --cacert "$TLS_PUBLIC_DIR/ca.crt" --resolve "$HOST_NAME:443:$HOST_ADDRESS" \
  "https://$HOST_NAME/" > "$TMP_ROOT/served-index.html" || fail 'trusted TLS request for the built Web artifact failed'
grep -q '<html' "$TMP_ROOT/served-index.html" || fail 'TLS edge did not serve the built Web artifact'
cmp -s "$TMP_ROOT/served-index.html" "$WEB_CLIENT_DIR/web/current/index.html" || fail 'TLS edge served a different Web artifact'
curl --fail --silent --show-error --cacert "$TLS_PUBLIC_DIR/ca.crt" --resolve "$HOST_NAME:443:$HOST_ADDRESS" \
  "https://$HOST_NAME/mtproto-target.json" > "$TMP_ROOT/served-target.json" || fail 'TLS edge did not serve the demo Web target manifest'
cmp -s "$TMP_ROOT/served-target.json" "$WEB_CLIENT_DIR/web/current/mtproto-target.json" || fail 'TLS edge served a different Web target manifest'

REAL_CLIENT_WEB_ROOT="$WEB_ROOT" \
QA_INVITE_SITE="https://$HOST_NAME/" \
QA_INVITE_USERNAME=qa_browser_smoke \
QA_INVITE_PASSWORD="$(<"$ACCOUNT_FILE")" \
QA_INVITE_PROFILE_ROOT="$TMP_ROOT/profiles" \
QA_INVITE_PROBE_CERT="$TMP_ROOT/outside.crt" \
QA_INVITE_PROBE_KEY="$TMP_ROOT/outside.key" \
HOME="$TEST_HOME" \
  node "$SCRIPT_DIR/test-qa-invite-browser-flow.cjs" || fail 'trusted browser sign-in or repeated WebSocket flow failed'

compose logs --no-color tls-edge > "$TMP_ROOT/tls-edge.log" 2>&1 || fail 'could not collect QA edge WebSocket upgrade logs'
UPGRADES=$(grep -Ec '"GET /apiws HTTP/[0-9.]+" 101([[:space:]]|$)' "$TMP_ROOT/tls-edge.log" || true)
if [[ ! "$UPGRADES" =~ ^[0-9]+$ ]] || ((UPGRADES < 3)); then
  fail "QA edge accepted only $UPGRADES API WebSocket connections; expected at least three"
fi

printf 'qa-invite browser flow passed: trusted TLS, discovery, Web configuration, three sign-ins, and %s accepted API WebSocket connections\n' "$UPGRADES"
