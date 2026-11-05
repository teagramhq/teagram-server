#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
[[ $(id -u) == 0 ]] || { printf '%s\n' 'QA TLS test requires root in an isolated fixture' >&2; exit 1; }
TEST_ROOT=$(mktemp -d)
TEST_SERVER_PID=
cleanup() {
  if [[ -n "$TEST_SERVER_PID" ]]; then
    kill "$TEST_SERVER_PID" 2>/dev/null || true
    wait "$TEST_SERVER_PID" 2>/dev/null || true
  fi
  rm -rf -- "$TEST_ROOT"
}
trap cleanup EXIT
TLS_DIR=$TEST_ROOT/tls
TLS_IDENTITY_DIR=$TLS_DIR/identity
TLS_PRIVATE_DIR=$TLS_IDENTITY_DIR/private
TLS_PUBLIC_DIR=$TLS_IDENTITY_DIR/public
HOST_NAME=teagram-demo.incus
HOST_ADDRESS=10.96.46.70
TEMP_DIRS=()

fail() { printf 'qa-invite TLS test: %s\n' "$1" >&2; exit 1; }
# shellcheck source=deploy/qa-invite/tls.sh
source "$SCRIPT_DIR/tls.sh"

compose() { printf '%s\n' "$*" >> "$TEST_ROOT/reloads"; return "${COMPOSE_RESULT:-0}"; }

start_test_server() {
  TEST_PORT=$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')
  openssl s_server -quiet -accept "127.0.0.1:$TEST_PORT" -cert "$TLS_PUBLIC_DIR/server.crt" \
    -key "$TLS_PUBLIC_DIR/server.key" -www > "$TEST_ROOT/server.log" 2>&1 &
  TEST_SERVER_PID=$!
  wait_for_qa_tls "127.0.0.1:$TEST_PORT" || fail 'test TLS server did not present the current leaf'
}

stop_test_server() {
  kill "$TEST_SERVER_PID"
  wait "$TEST_SERVER_PID" 2>/dev/null || true
  TEST_SERVER_PID=
}

issue_fixture_leaf() {
  local san=$1 output=$2
  openssl req -new -key "$TLS_PUBLIC_DIR/server.key" -subj '/CN=fixture' -out "$TEST_ROOT/fixture.csr" >/dev/null 2>&1
  printf 'basicConstraints=critical,CA:FALSE\nextendedKeyUsage=serverAuth\nsubjectAltName=%s\n' "$san" > "$TEST_ROOT/fixture.ext"
  openssl x509 -req -in "$TEST_ROOT/fixture.csr" -CA "$TLS_PUBLIC_DIR/ca.crt" -CAkey "$TLS_PRIVATE_DIR/ca.key" \
    -set_serial 42 -days 1 -sha256 -extfile "$TEST_ROOT/fixture.ext" -out "$output" >/dev/null 2>&1
}

ensure_tls
CA_SHA=$(sha256sum "$TLS_PUBLIC_DIR/ca.crt" | cut -d' ' -f1)
CA_KEY_SHA=$(sha256sum "$TLS_PRIVATE_DIR/ca.key" | cut -d' ' -f1)
SERVER_KEY_SHA=$(sha256sum "$TLS_PUBLIC_DIR/server.key" | cut -d' ' -f1)
CERT_SHA=$(sha256sum "$TLS_PUBLIC_DIR/server.crt" | cut -d' ' -f1)
cp "$TLS_PUBLIC_DIR/ca.crt" "$TEST_ROOT/original-ca.crt"
cp "$TLS_PRIVATE_DIR/ca.key" "$TEST_ROOT/original-ca.key"
ensure_tls
[[ "$(sha256sum "$TLS_PUBLIC_DIR/server.crt" | cut -d' ' -f1)" == "$CERT_SHA" ]] ||
  fail 'a healthy certificate was unnecessarily replaced'
reload_qa_tls
[[ "$(<"$TEST_ROOT/reloads")" == 'exec -T tls-edge nginx -s reload' ]] || fail 'up did not reconcile the TLS edge'

openssl req -x509 -new -key "$TLS_PRIVATE_DIR/ca.key" -sha256 -days 2 -subj '/CN=Teagram QA Invite Root CA' \
  -addext 'basicConstraints=critical,CA:TRUE,pathlen:0' -addext 'keyUsage=critical,keyCertSign,cRLSign' \
  -addext 'subjectKeyIdentifier=hash' -out "$TEST_ROOT/control-ca.crt" >/dev/null 2>&1
for san in 'DNS:example.com' 'DNS:child.teagram-demo.incus' 'IP:10.96.46.71'; do
  issue_fixture_leaf "$san" "$TEST_ROOT/outside.crt"
  if [[ "$san" == 'DNS:example.com' ]]; then
    openssl verify -CAfile "$TEST_ROOT/control-ca.crt" -purpose sslserver -verify_hostname example.com "$TEST_ROOT/outside.crt" >/dev/null ||
      fail 'constraint control certificate is otherwise invalid'
  fi
  if openssl verify -CAfile "$TLS_PUBLIC_DIR/ca.crt" "$TEST_ROOT/outside.crt" >/dev/null 2>&1; then
    fail "CA accepted an out-of-scope name: $san"
  fi
done

openssl req -new -key "$TLS_PUBLIC_DIR/server.key" -subj '/CN=fixture' -out "$TEST_ROOT/mail.csr" >/dev/null 2>&1
printf 'basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=emailProtection\nsubjectAltName=email:qa@example.com\n' > "$TEST_ROOT/mail.ext"
openssl x509 -req -in "$TEST_ROOT/mail.csr" -CA "$TLS_PUBLIC_DIR/ca.crt" -CAkey "$TLS_PRIVATE_DIR/ca.key" \
  -set_serial 43 -days 1 -sha256 -extfile "$TEST_ROOT/mail.ext" -out "$TEST_ROOT/mail.crt" >/dev/null 2>&1
openssl verify -CAfile "$TEST_ROOT/control-ca.crt" -purpose smimesign "$TEST_ROOT/mail.crt" >/dev/null || fail 'mail-purpose control certificate is otherwise invalid'
if openssl verify -CAfile "$TLS_PUBLIC_DIR/ca.crt" -purpose smimesign "$TEST_ROOT/mail.crt" >/dev/null 2>&1; then
  fail 'QA root accepted a non-TLS certificate purpose'
fi

start_test_server
issue_fixture_leaf "DNS:$HOST_NAME,IP:$HOST_ADDRESS" "$TEST_ROOT/short.crt"
cat "$TEST_ROOT/short.crt" "$TLS_PUBLIC_DIR/ca.crt" > "$TLS_PUBLIC_DIR/server.crt"
chmod 0644 "$TLS_PUBLIC_DIR/server.crt"
if verify_served_qa_tls "127.0.0.1:$TEST_PORT"; then fail 'stale served certificate was accepted'; fi
stop_test_server
cp "$TLS_PUBLIC_DIR/server.crt" "$TEST_ROOT/short-chain.crt"
mkdir -p "$TEST_ROOT/bin"
cat > "$TEST_ROOT/bin/openssl" <<'WRAPPER'
#!/usr/bin/env bash
if [[ "$1" == x509 && "${2:-}" == -req ]]; then exit 1; fi
exec /usr/bin/openssl "$@"
WRAPPER
chmod 0755 "$TEST_ROOT/bin/openssl"
if (PATH="$TEST_ROOT/bin:$PATH"; ensure_tls) >/dev/null 2>&1; then
  fail 'failed leaf issuance was reported as successful'
fi
cmp -s "$TEST_ROOT/short-chain.crt" "$TLS_PUBLIC_DIR/server.crt" || fail 'failed renewal changed the served certificate'
ensure_tls
reload_qa_tls
[[ "$(wc -l < "$TEST_ROOT/reloads")" == 2 ]] || fail 'renewal did not reload the TLS edge'
COMPOSE_RESULT=1
if reload_qa_tls; then fail 'TLS edge reload failure was ignored'; fi
unset COMPOSE_RESULT
[[ "$(sha256sum "$TLS_PUBLIC_DIR/ca.crt" | cut -d' ' -f1)" == "$CA_SHA" &&
   "$(sha256sum "$TLS_PRIVATE_DIR/ca.key" | cut -d' ' -f1)" == "$CA_KEY_SHA" &&
   "$(sha256sum "$TLS_PUBLIC_DIR/server.key" | cut -d' ' -f1)" == "$SERVER_KEY_SHA" ]] || fail 'renewal rotated a trusted key or root'
if cmp -s "$TEST_ROOT/short-chain.crt" "$TLS_PUBLIC_DIR/server.crt"; then fail 'renewal retained the expiring leaf'; fi
openssl x509 -in "$TLS_PUBLIC_DIR/server.crt" -noout -checkend 2592000 >/dev/null || fail 'renewed leaf expires too soon'
openssl verify -CAfile "$TLS_PUBLIC_DIR/ca.crt" -purpose sslserver -verify_hostname "$HOST_NAME" "$TLS_PUBLIC_DIR/server.crt" >/dev/null
openssl verify -CAfile "$TLS_PUBLIC_DIR/ca.crt" -purpose sslserver -verify_ip "$HOST_ADDRESS" "$TLS_PUBLIC_DIR/server.crt" >/dev/null
start_test_server
stop_test_server

CERT_SHA=$(sha256sum "$TLS_PUBLIC_DIR/server.crt" | cut -d' ' -f1)
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "$TLS_PRIVATE_DIR/ca.key" >/dev/null 2>&1
chmod 0600 "$TLS_PRIVATE_DIR/ca.key"
if (ensure_tls) >/dev/null 2>&1; then fail 'mismatched CA key was accepted'; fi
[[ "$(sha256sum "$TLS_PUBLIC_DIR/ca.crt" | cut -d' ' -f1)" == "$CA_SHA" &&
   "$(sha256sum "$TLS_PUBLIC_DIR/server.crt" | cut -d' ' -f1)" == "$CERT_SHA" ]] || fail 'CA key rejection changed certificates'
cp "$TEST_ROOT/original-ca.key" "$TLS_PRIVATE_DIR/ca.key"
chmod 0600 "$TLS_PRIVATE_DIR/ca.key"

openssl req -x509 -new -key "$TLS_PRIVATE_DIR/ca.key" -sha256 -days 3650 -subj '/CN=Teagram QA Invite Root CA' \
  -addext 'basicConstraints=critical,CA:TRUE,pathlen:0' -addext 'keyUsage=critical,keyCertSign,cRLSign' \
  -out "$TLS_PUBLIC_DIR/ca.crt" >/dev/null 2>&1
chmod 0644 "$TLS_PUBLIC_DIR/ca.crt"
UNCONSTRAINED_SHA=$(sha256sum "$TLS_PUBLIC_DIR/ca.crt" | cut -d' ' -f1)
if (ensure_tls) >/dev/null 2>&1; then fail 'unconstrained existing root was accepted'; fi
[[ "$(sha256sum "$TLS_PUBLIC_DIR/ca.crt" | cut -d' ' -f1)" == "$UNCONSTRAINED_SHA" &&
   "$(sha256sum "$TLS_PUBLIC_DIR/server.crt" | cut -d' ' -f1)" == "$CERT_SHA" ]] || fail 'unsafe root rejection changed the identity'

openssl req -x509 -new -key "$TLS_PRIVATE_DIR/ca.key" -sha256 -days 1 -subj '/CN=Teagram QA Invite Root CA' \
  -addext 'basicConstraints=critical,CA:TRUE,pathlen:0' -addext 'keyUsage=critical,keyCertSign,cRLSign' \
  -addext 'extendedKeyUsage=critical,serverAuth' \
  -addext "$(qa_tls_name_constraints "$HOST_NAME" "$HOST_ADDRESS")" \
  -out "$TLS_PUBLIC_DIR/ca.crt" >/dev/null 2>&1
chmod 0644 "$TLS_PUBLIC_DIR/ca.crt"
EXPIRING_CA_SHA=$(sha256sum "$TLS_PUBLIC_DIR/ca.crt" | cut -d' ' -f1)
if (ensure_tls) >/dev/null 2>&1; then fail 'expiring root was accepted'; fi
[[ "$(sha256sum "$TLS_PUBLIC_DIR/ca.crt" | cut -d' ' -f1)" == "$EXPIRING_CA_SHA" &&
   "$(sha256sum "$TLS_PUBLIC_DIR/server.crt" | cut -d' ' -f1)" == "$CERT_SHA" ]] || fail 'expiring root rejection changed the identity'

printf '%s\n' 'QA TLS constraints and leaf renewal checks passed'
