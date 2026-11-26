#!/usr/bin/env bash
set -Eeuo pipefail

if [[ "$EUID" -ne 0 ]]; then
  if command -v sudo >/dev/null 2>&1; then
    exec sudo -n -- bash "$0" "$@"
  fi
  printf 'identity import tests require root or passwordless sudo\n' >&2
  exit 77
fi

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -P "$SCRIPT_DIR/../.." && pwd)
TMP_ROOT=$(mktemp -d /tmp/teagram-demo-identity-test.XXXXXXXX)
trap 'rm -rf -- "$TMP_ROOT"' EXIT
chmod 0700 "$TMP_ROOT"

TEST_REPO="$TMP_ROOT/repo"
TEST_SCRIPT_DIR="$TEST_REPO/deploy/teagram-demo"
TEST_INIT="$TEST_SCRIPT_DIR/init-demo.sh"
MOCK_BIN="$TMP_ROOT/mock-bin"
VOLUME_NAME=teagram-demo_tgkey
POSTGRES_PASSWORD_FIXTURE=$(printf 'b%.0s' {1..64})
AUTHKEY_FIXTURE=$(printf 'c%.0s' {1..64})
IMAGE_DIGEST=$(printf 'a%.0s' {1..64})
REAL_DOCKER=$(command -v docker)

mkdir -p "$TEST_SCRIPT_DIR/lima" "$MOCK_BIN"
cp "$SCRIPT_DIR/init-demo.sh" "$TEST_INIT"
cp "$SCRIPT_DIR/demo-common.sh" "$TEST_SCRIPT_DIR/demo-common.sh"
cp "$SCRIPT_DIR/validate-env.py" "$TEST_SCRIPT_DIR/validate-env.py"
cp "$SCRIPT_DIR/haproxy.cfg" "$TEST_SCRIPT_DIR/haproxy.cfg"
cp "$SCRIPT_DIR/render-haproxy-config.py" "$TEST_SCRIPT_DIR/render-haproxy-config.py"
mkdir -p "$TEST_SCRIPT_DIR/nginx-config"
cp "$SCRIPT_DIR/nginx-config/nginx.conf" "$TEST_SCRIPT_DIR/nginx-config/nginx.conf"
cp "$SCRIPT_DIR/lima/compose.yaml" "$TEST_SCRIPT_DIR/lima/compose.yaml"
chmod 0755 "$TEST_INIT"

cat > "$MOCK_BIN/docker" <<'BASH'
#!/usr/bin/env bash
set -Eeuo pipefail
printf '%s\n' "$*" >> "$TEAGRAM_TEST_DOCKER_LOG"

volume_dir="$TEAGRAM_TEST_VOLUME_ROOT/$TEAGRAM_TEST_VOLUME_NAME"
volume_json="$TEAGRAM_TEST_VOLUME_ROOT/volume.json"

case "${1:-}" in
  compose)
    if [[ " $* " == *" config "* ]]; then
      exec "$TEAGRAM_TEST_REAL_DOCKER" "$@"
    fi
    exit 0
    ;;
  image)
    [[ "${2:-}" == inspect ]]
    exit 0
    ;;
  pull)
    exit 0
    ;;
  volume)
    case "${2:-}" in
      ls)
        if [[ -f "$volume_json" ]]; then
          printf '%s\n' "$TEAGRAM_TEST_VOLUME_NAME"
        fi
        ;;
      inspect)
        inspect_count=0
        if [[ -f "$TEAGRAM_TEST_INSPECT_COUNT" ]]; then
          inspect_count=$(<"$TEAGRAM_TEST_INSPECT_COUNT")
        fi
        inspect_count=$((inspect_count + 1))
        printf '%s\n' "$inspect_count" > "$TEAGRAM_TEST_INSPECT_COUNT"
        if [[ "${TEAGRAM_TEST_FAIL_CLEANUP_INSPECT:-0}" == once && "$inspect_count" == 2 ]] ||
          [[ "${TEAGRAM_TEST_FAIL_CLEANUP_INSPECT:-0}" == always && "$inspect_count" -ge 2 ]]; then
          exit 1
        fi
        [[ -f "$volume_json" ]] || exit 1
        cat "$volume_json"
        ;;
      create)
        name=
        run_token=
        while (($#)); do
          case "$1" in
            --label)
              shift
              [[ "$1" != com.teagram.demo.identity-import-run=* ]] || run_token=${1#*=}
              ;;
            --driver)
              shift
              ;;
            *) name=$1 ;;
          esac
          shift
        done
        [[ "$name" == "$TEAGRAM_TEST_VOLUME_NAME" && ! -e "$volume_json" ]]
        mkdir -m 0700 "$volume_dir"
        if [[ "${TEAGRAM_TEST_SEED_STRAY:-0}" == 1 ]]; then
          printf 'unexpected\n' > "$volume_dir/unexpected"
        fi
        jq -n --arg name "$name" --arg token "$run_token" \
          '[{Name:$name,Driver:"local",Options:{},Labels:{"com.teagram.demo.identity-import-run":$token}}]' \
          > "$volume_json"
        printf '%s\n' "$name"
        ;;
      rm)
        [[ "${3:-}" == "$TEAGRAM_TEST_VOLUME_NAME" && -f "$volume_json" ]]
        rm -rf -- "$volume_dir" "$volume_json"
        ;;
      *) exit 1 ;;
    esac
    ;;
  run)
    shift
    entrypoint=
    network=
    log_driver=
    read_only=0
    env_file=
    mounts=()
    while (($#)); do
      case "$1" in
        --pull=*) shift ;;
        --rm) shift ;;
        --read-only) read_only=1; shift ;;
        --network)
          network=$2
          shift 2
          ;;
        --log-driver)
          log_driver=$2
          shift 2
          ;;
        --env-file)
          env_file=$2
          shift 2
          ;;
        --user|--cap-drop|--cap-add|--security-opt)
          shift 2
          ;;
        --mount)
          mounts+=("$2")
          shift 2
          ;;
        --entrypoint)
          entrypoint=$2
          shift 2
          ;;
        *)
          image=$1
          shift
          break
          ;;
      esac
    done
    if [[ "$entrypoint" == /bin/sh && "${1:-}" == -ec && "$#" -ge 2 ]]; then
      script=$2
      source_key=
      has_volume_mount=0
      for mount in "${mounts[@]}"; do
        case "$mount" in
          "type=volume,src=$TEAGRAM_TEST_VOLUME_NAME,dst=/key"|"type=volume,src=$TEAGRAM_TEST_VOLUME_NAME,dst=/key,readonly")
            has_volume_mount=1
            ;;
          type=bind,src=*,dst=/tmp/server_key.pem,readonly)
            source_key=${mount#type=bind,src=}
            source_key=${source_key%,dst=/tmp/server_key.pem,readonly}
            ;;
        esac
      done
      [[ "$has_volume_mount" == 1 && "$network" == none && "$read_only" == 1 && "$log_driver" == none ]]
      if [[ -n "$source_key" ]]; then
        [[ -f "$source_key" ]]
        rewritten_script=$(printf '%s\n' "$script" | sed \
          -e "s|/tmp/server_key\\.pem|$source_key|g" \
          -e "s|/key|$volume_dir|g")
      else
        rewritten_script=$(printf '%s\n' "$script" | sed \
          -e "s|/key|$volume_dir|g")
      fi
      sh -ec "$rewritten_script"
    else
      [[ "$entrypoint" == '' && "${1:-}" == client-config && "$network" == none &&
        "$read_only" == 1 && -f "$env_file" ]]
      grep -Fxq 'TG_ADVERTISE_DEMO_GUEST_SIGN_UP=false' "$env_file"
      grep -Fxq 'TG_TURNSTILE_SECRET_KEY=fixture-verification-secret' "$env_file"
      ! grep -Eq '^(TG_AUTHKEY_ENC_KEY|TG_POSTGRES_DSN)=' "$env_file"
      [[ " ${mounts[*]} " == *"type=volume,src=$TEAGRAM_TEST_VOLUME_NAME,dst=/var/lib/teagramd,readonly"* ]]
      if [[ "${TEAGRAM_TEST_RUN_REAL_CLIENT_CONFIG:-0}" == 1 ]]; then
        [[ -n "${TEAGRAM_TEST_SERVER_IMAGE:-}" ]]
        client_config=$("$TEAGRAM_TEST_REAL_DOCKER" run --pull=never --rm --network none --read-only \
          --user 65532:65532 --cap-drop ALL --security-opt no-new-privileges \
          --env-file "$env_file" \
          --mount "type=bind,src=$volume_dir,dst=/var/lib/teagramd,readonly" \
          "$TEAGRAM_TEST_SERVER_IMAGE" client-config)
        printf '%s\n' "$client_config" > "$TEAGRAM_TEST_REAL_CLIENT_CONFIG_OUTPUT"
        printf '%s\n' "$client_config"
      else
        printf '{"mtproto":{"rsa_spki":"%s"}}\n' "$TEAGRAM_TEST_CLIENT_SPKI"
      fi
    fi
    ;;
  *) exit 1 ;;
esac
BASH
chmod 0755 "$MOCK_BIN/docker"

cat > "$MOCK_BIN/ln" <<'BASH'
#!/usr/bin/env bash
set -Eeuo pipefail
if [[ "${TEAGRAM_TEST_FORCE_LN_COLLISION:-0}" == 1 &&
  "${2:-}" == "$TEAGRAM_TEST_VOLUME_ROOT/$TEAGRAM_TEST_VOLUME_NAME/server_key.pem" ]]; then
  printf 'sentinel\n' > "$2"
fi
exec /usr/bin/ln "$@"
BASH
chmod 0755 "$MOCK_BIN/ln"

openssl genrsa -traditional -out "$TMP_ROOT/server_key.pem" 2048 >/dev/null 2>&1
chmod 0600 "$TMP_ROOT/server_key.pem"
openssl pkey -in "$TMP_ROOT/server_key.pem" -pubout -outform DER \
  2>/dev/null | base64 | tr -d '\n' > "$TMP_ROOT/server-spki"
VALID_SPKI=$(<"$TMP_ROOT/server-spki")
openssl genrsa -traditional -out "$TMP_ROOT/other_key.pem" 2048 >/dev/null 2>&1
chmod 0600 "$TMP_ROOT/other_key.pem"

cat > "$TMP_ROOT/fingerprint.go" <<'GO'
package main

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"

	"github.com/gotd/td/crypto"
)

func main() {
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	block, _ := pem.Decode(data)
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		panic(err)
	}
	fmt.Println(crypto.RSAFingerprint(&key.PublicKey))
}
GO
if command -v go >/dev/null 2>&1; then
  EXPECTED_FINGERPRINT=$(cd "$REPO_ROOT" && go run "$TMP_ROOT/fingerprint.go" "$TMP_ROOT/server_key.pem")
else
  EXPECTED_FINGERPRINT=$(python3 - "$TMP_ROOT/server_key.pem" <<'PY'
import base64
import hashlib
import pathlib
import re
import sys


def read_tlv(data, offset, tag):
    if data[offset] != tag:
        raise ValueError("invalid DER")
    first = data[offset + 1]
    cursor = offset + 2
    if first & 0x80:
        count = first & 0x7f
        length = int.from_bytes(data[cursor : cursor + count], "big")
        cursor += count
    else:
        length = first
    end = cursor + length
    return data[cursor:end], end


def tl_bytes(value):
    length = len(value)
    if length <= 253:
        encoded = bytes((length,)) + value
    else:
        encoded = b"\xfe" + length.to_bytes(3, "little") + value
    return encoded + b"\x00" * ((-len(encoded)) % 4)


pem_data = pathlib.Path(sys.argv[1]).read_bytes()
body = re.fullmatch(
    rb"\s*-----BEGIN RSA PRIVATE KEY-----\s+(.*?)\s+-----END RSA PRIVATE KEY-----\s*",
    pem_data,
    flags=re.DOTALL,
).group(1)
der = base64.b64decode(re.sub(rb"\s+", b"", body), validate=True)
sequence, _ = read_tlv(der, 0, 0x30)
_, offset = read_tlv(sequence, 0, 0x02)
modulus, offset = read_tlv(sequence, offset, 0x02)
exponent, _ = read_tlv(sequence, offset, 0x02)
modulus = int.from_bytes(modulus, "big").to_bytes(256, "big")
exponent = int.from_bytes(exponent, "big").to_bytes((int.from_bytes(exponent, "big").bit_length() + 7) // 8, "big")
digest = hashlib.sha1(tl_bytes(modulus) + tl_bytes(exponent)).digest()
print(int.from_bytes(digest[12:], "little", signed=True))
PY
  )
fi

new_case() {
  CASE_ROOT="$TMP_ROOT/$1"
  CASE_ENV="$CASE_ROOT/lima.env"
  CASE_VOLUME_ROOT="$CASE_ROOT/volumes"
  CASE_LOG="$CASE_ROOT/docker.log"
  CASE_OUTPUT="$CASE_ROOT/output.log"
  CASE_INSPECT_COUNT="$CASE_ROOT/inspect-count"
  mkdir -p "$CASE_ROOT" "$CASE_VOLUME_ROOT" "$CASE_ROOT/secrets"
  cat > "$CASE_ENV" <<EOF
POSTGRES_PASSWORD=$POSTGRES_PASSWORD_FIXTURE
TG_PUBLIC_LINK_PREFIX=https://demo.teagram.io/
TG_AUTHKEY_ENC_KEY=$AUTHKEY_FIXTURE
TEAGRAM_DEMO_TURNSTILE_SITE_KEY=fixture-site-key
TEAGRAM_DEMO_IMAGE_REF=ghcr.io/teagramhq/teagram-server-demo@sha256:$IMAGE_DIGEST
EOF
  chmod 0600 "$CASE_ENV"
  printf '%s\n' 'TUNNEL_TOKEN=fixture-tunnel-token' > "$CASE_ROOT/secrets/cloudflared.env"
  printf '%s\n' 'fixture-turnstile-secret' > "$CASE_ROOT/secrets/turnstile-secret-key"
  printf '%s\n' 'fixture-qa-bypass-token-0123456789abcdef' > "$CASE_ROOT/secrets/turnstile-qa-bypass-token"
  chmod 0600 "$CASE_ROOT/secrets/cloudflared.env" \
    "$CASE_ROOT/secrets/turnstile-secret-key" "$CASE_ROOT/secrets/turnstile-qa-bypass-token"
  : > "$CASE_LOG"
}

run_import() {
  local key_path=$1 expected_spki=$2 client_spki=${3:-$VALID_SPKI}
  local run_real_client_config=0
  local import_status
  if [[ -n "${TEAGRAM_TEST_SERVER_IMAGE:-}" && "$client_spki" == "$VALID_SPKI" ]]; then
    run_real_client_config=1
  fi
  if timeout 15s env PATH="$MOCK_BIN:$PATH" \
    TEAGRAM_DEMO_LIMA_ENV_FILE="$CASE_ENV" \
    TEAGRAM_DEMO_SECRETS_DIR="$CASE_ROOT/secrets" \
    TEAGRAM_TEST_DOCKER_LOG="$CASE_LOG" \
    TEAGRAM_TEST_VOLUME_ROOT="$CASE_VOLUME_ROOT" \
    TEAGRAM_TEST_VOLUME_NAME="$VOLUME_NAME" \
    TEAGRAM_TEST_REAL_DOCKER="$REAL_DOCKER" \
    TEAGRAM_TEST_INSPECT_COUNT="$CASE_INSPECT_COUNT" \
    TEAGRAM_TEST_SERVER_IMAGE="${TEAGRAM_TEST_SERVER_IMAGE:-}" \
    TEAGRAM_TEST_RUN_REAL_CLIENT_CONFIG="$run_real_client_config" \
    TEAGRAM_TEST_REAL_CLIENT_CONFIG_OUTPUT="$CASE_ROOT/real-client-config.json" \
    TEAGRAM_TEST_CLIENT_SPKI="$client_spki" \
    TEAGRAM_TEST_SEED_STRAY="${TEAGRAM_TEST_SEED_STRAY:-0}" \
    TEAGRAM_TEST_FORCE_LN_COLLISION="${TEAGRAM_TEST_FORCE_LN_COLLISION:-0}" \
    TEAGRAM_TEST_FAIL_CLEANUP_INSPECT="${TEAGRAM_TEST_FAIL_CLEANUP_INSPECT:-0}" \
    "$TEST_INIT" --import-key "$key_path" --expected-rsa-spki "$expected_spki" \
    >"$CASE_OUTPUT" 2>&1; then
    import_status=0
  else
    import_status=$?
  fi
  printf '%s\n' "$import_status" > "$CASE_ROOT/import.exit"
  return "$import_status"
}

assert_no_secret_output() {
  if grep -Fq "$POSTGRES_PASSWORD_FIXTURE" "$CASE_OUTPUT" "$CASE_LOG" \
    || grep -Fq "$AUTHKEY_FIXTURE" "$CASE_OUTPUT" "$CASE_LOG" \
    || grep -Fq 'BEGIN RSA PRIVATE KEY' "$CASE_OUTPUT" "$CASE_LOG"; then
    printf 'identity import exposed a test secret or key marker\n' >&2
    return 1
  fi
}

assert_not_contains() {
  local value=$1 file=$2
  if grep -Fq -- "$value" "$file"; then
    printf 'identity import test found an unexpected value in %s\n' "$file" >&2
    return 1
  fi
}

if [[ ! -x "$(command -v openssl)" ]]; then
  printf 'identity import tests require OpenSSL\n' >&2
  exit 1
fi

new_case success
if ! run_import "$TMP_ROOT/server_key.pem" "$VALID_SPKI"; then
  cat "$CASE_OUTPUT" >&2
  exit 1
fi
[[ -f "$CASE_VOLUME_ROOT/$VOLUME_NAME/server_key.pem" ]]
cmp -s "$TMP_ROOT/server_key.pem" "$CASE_VOLUME_ROOT/$VOLUME_NAME/server_key.pem"
[[ "$(stat -c '%u:%g:%a' "$CASE_VOLUME_ROOT/$VOLUME_NAME")" == 65532:65532:700 ]]
[[ "$(stat -c '%u:%g:%a' "$CASE_VOLUME_ROOT/$VOLUME_NAME/server_key.pem")" == 65532:65532:600 ]]
grep -Fxq "TG_RSA_KEY_FINGERPRINT=$EXPECTED_FINGERPRINT" "$CASE_ENV"
grep -Fq -- '--network none' "$CASE_LOG"
grep -Fq 'dst=/tmp/server_key.pem,readonly' "$CASE_LOG"
grep -Fq 'dst=/var/lib/teagramd,readonly' "$CASE_LOG"
grep -Fq -- '--log-driver none' "$CASE_LOG"
grep -Fq -- '--env-file ' "$CASE_LOG"
grep -Fq 'client-config' "$CASE_LOG"
if [[ -n "${TEAGRAM_TEST_SERVER_IMAGE:-}" ]]; then
  jq -e --arg expected "$VALID_SPKI" '.mtproto.rsa_spki == $expected' \
    "$CASE_ROOT/real-client-config.json" >/dev/null
fi
assert_no_secret_output

new_case stray-volume-entry
if TEAGRAM_TEST_SEED_STRAY=1 run_import "$TMP_ROOT/server_key.pem" "$VALID_SPKI"; then
  printf 'identity import accepted a stray file in the new volume\n' >&2
  exit 1
fi
[[ ! -e "$CASE_VOLUME_ROOT/$VOLUME_NAME" ]]
grep -Fq 'volume rm teagram-demo_tgkey' "$CASE_LOG"
assert_no_secret_output

new_case no-clobber
if TEAGRAM_TEST_FORCE_LN_COLLISION=1 run_import "$TMP_ROOT/server_key.pem" "$VALID_SPKI"; then
  printf 'identity import overwrote a key published during the no-clobber race\n' >&2
  exit 1
fi
[[ ! -e "$CASE_VOLUME_ROOT/$VOLUME_NAME" ]]
grep -Fq 'volume rm teagram-demo_tgkey' "$CASE_LOG"
assert_no_secret_output

new_case mismatch
cp "$CASE_ENV" "$CASE_ROOT/env.before"
if run_import "$TMP_ROOT/server_key.pem" 'AAAA'; then
  printf 'identity import accepted a mismatched rsa_spki\n' >&2
  exit 1
fi
[[ ! -e "$CASE_VOLUME_ROOT/$VOLUME_NAME" ]]
assert_not_contains 'volume create' "$CASE_LOG"
cmp -s "$CASE_ENV" "$CASE_ROOT/env.before"
assert_no_secret_output

new_case missing
if run_import "$CASE_ROOT/not-a-key.pem" "$VALID_SPKI"; then
  printf 'identity import accepted a missing key\n' >&2
  exit 1
fi
[[ ! -e "$CASE_VOLUME_ROOT/$VOLUME_NAME" ]]
assert_not_contains 'volume create' "$CASE_LOG"
assert_no_secret_output

new_case symlink
ln -s "$TMP_ROOT/server_key.pem" "$CASE_ROOT/symlink.pem"
if run_import "$CASE_ROOT/symlink.pem" "$VALID_SPKI"; then
  printf 'identity import accepted a symlink to the valid RSA key\n' >&2
  exit 1
fi
[[ ! -e "$CASE_VOLUME_ROOT/$VOLUME_NAME" ]]
assert_no_secret_output

new_case fifo-source
mkfifo "$CASE_ROOT/key.fifo"
if run_import "$CASE_ROOT/key.fifo" "$VALID_SPKI"; then
  printf 'identity import accepted a named pipe as the source RSA key\n' >&2
  exit 1
fi
[[ ! -e "$CASE_VOLUME_ROOT/$VOLUME_NAME" ]]
grep -Fq 'RSA import key is missing or unsafe' "$CASE_OUTPUT"
[[ "$(<"$CASE_ROOT/import.exit")" != 124 ]]
assert_not_contains 'volume create' "$CASE_LOG"
assert_no_secret_output

new_case unreadable
cp "$TMP_ROOT/server_key.pem" "$CASE_ROOT/unreadable.pem"
chmod 0000 "$CASE_ROOT/unreadable.pem"
if run_import "$CASE_ROOT/unreadable.pem" "$VALID_SPKI"; then
  printf 'identity import accepted a key without owner read permission\n' >&2
  exit 1
fi
[[ ! -e "$CASE_VOLUME_ROOT/$VOLUME_NAME" ]]
assert_no_secret_output

new_case non-root-owner
cp "$TMP_ROOT/server_key.pem" "$CASE_ROOT/non-root-key.pem"
chown 65534:65534 "$CASE_ROOT/non-root-key.pem"
chmod 0600 "$CASE_ROOT/non-root-key.pem"
if run_import "$CASE_ROOT/non-root-key.pem" "$VALID_SPKI"; then
  printf 'identity import accepted an RSA key not owned by root\n' >&2
  exit 1
fi
[[ ! -e "$CASE_VOLUME_ROOT/$VOLUME_NAME" ]]
assert_no_secret_output

new_case group-readable
cp "$TMP_ROOT/server_key.pem" "$CASE_ROOT/group-readable.pem"
chmod 0640 "$CASE_ROOT/group-readable.pem"
if run_import "$CASE_ROOT/group-readable.pem" "$VALID_SPKI"; then
  printf 'identity import accepted a group-readable key\n' >&2
  exit 1
fi
[[ ! -e "$CASE_VOLUME_ROOT/$VOLUME_NAME" ]]
assert_no_secret_output

new_case oversize-valid-key
cp "$TMP_ROOT/server_key.pem" "$CASE_ROOT/oversize-key.pem"
head -c 16384 /dev/zero | tr '\000' ' ' >> "$CASE_ROOT/oversize-key.pem"
chmod 0600 "$CASE_ROOT/oversize-key.pem"
if run_import "$CASE_ROOT/oversize-key.pem" "$VALID_SPKI"; then
  printf 'identity import accepted a valid key padded past 16 KiB\n' >&2
  exit 1
fi
[[ ! -e "$CASE_VOLUME_ROOT/$VOLUME_NAME" ]]
assert_no_secret_output

new_case non-pkcs1
openssl pkcs8 -topk8 -nocrypt -in "$TMP_ROOT/server_key.pem" -out "$CASE_ROOT/pkcs8.pem" 2>/dev/null
chmod 0600 "$CASE_ROOT/pkcs8.pem"
if run_import "$CASE_ROOT/pkcs8.pem" "$VALID_SPKI"; then
  printf 'identity import accepted a non-PKCS#1 RSA key\n' >&2
  exit 1
fi
[[ ! -e "$CASE_VOLUME_ROOT/$VOLUME_NAME" ]]
assert_no_secret_output

new_case encrypted
openssl rsa -in "$TMP_ROOT/server_key.pem" -traditional -aes256 \
  -passout pass:fixture-encrypted-key-passphrase -out "$CASE_ROOT/encrypted-key.pem" \
  >/dev/null 2>&1
chmod 0600 "$CASE_ROOT/encrypted-key.pem"
if run_import "$CASE_ROOT/encrypted-key.pem" "$VALID_SPKI"; then
  printf 'identity import accepted an encrypted RSA key\n' >&2
  exit 1
fi
[[ ! -e "$CASE_VOLUME_ROOT/$VOLUME_NAME" ]]
grep -Fq 'RSA import key must be an unencrypted PKCS#1 PEM' "$CASE_OUTPUT"
assert_not_contains 'volume create' "$CASE_LOG"
assert_no_secret_output

new_case wrong-size
openssl genrsa -traditional -out "$CASE_ROOT/large-key.pem" 4096 >/dev/null 2>&1
chmod 0600 "$CASE_ROOT/large-key.pem"
if run_import "$CASE_ROOT/large-key.pem" "$VALID_SPKI"; then
  printf 'identity import accepted an RSA key that was not 2048 bits\n' >&2
  exit 1
fi
[[ ! -e "$CASE_VOLUME_ROOT/$VOLUME_NAME" ]]
assert_no_secret_output

new_case trailing-data
cp "$TMP_ROOT/server_key.pem" "$CASE_ROOT/trailing-key.pem"
printf '\ntrailing data\n' >> "$CASE_ROOT/trailing-key.pem"
chmod 0600 "$CASE_ROOT/trailing-key.pem"
if run_import "$CASE_ROOT/trailing-key.pem" "$VALID_SPKI"; then
  printf 'identity import accepted trailing data after the PEM block\n' >&2
  exit 1
fi
[[ ! -e "$CASE_VOLUME_ROOT/$VOLUME_NAME" ]]
assert_no_secret_output

new_case existing-different
mkdir -m 0700 "$CASE_VOLUME_ROOT/$VOLUME_NAME"
cp "$TMP_ROOT/other_key.pem" "$CASE_VOLUME_ROOT/$VOLUME_NAME/server_key.pem"
chown 65532:65532 "$CASE_VOLUME_ROOT/$VOLUME_NAME" "$CASE_VOLUME_ROOT/$VOLUME_NAME/server_key.pem"
chmod 0700 "$CASE_VOLUME_ROOT/$VOLUME_NAME"
chmod 0600 "$CASE_VOLUME_ROOT/$VOLUME_NAME/server_key.pem"
jq -n --arg name "$VOLUME_NAME" \
  '[{Name:$name,Driver:"local",Options:{},Labels:{}}]' > "$CASE_VOLUME_ROOT/volume.json"
before_hash=$(sha256sum "$CASE_VOLUME_ROOT/$VOLUME_NAME/server_key.pem")
if run_import "$TMP_ROOT/server_key.pem" "$VALID_SPKI"; then
  printf 'identity import accepted a different key in the existing volume\n' >&2
  exit 1
fi
[[ "$before_hash" == "$(sha256sum "$CASE_VOLUME_ROOT/$VOLUME_NAME/server_key.pem")" ]]
assert_not_contains 'volume create' "$CASE_LOG"
assert_no_secret_output

new_case existing-stray
mkdir -m 0700 "$CASE_VOLUME_ROOT/$VOLUME_NAME"
cp "$TMP_ROOT/server_key.pem" "$CASE_VOLUME_ROOT/$VOLUME_NAME/server_key.pem"
printf 'unexpected\n' > "$CASE_VOLUME_ROOT/$VOLUME_NAME/unexpected"
chown 65532:65532 "$CASE_VOLUME_ROOT/$VOLUME_NAME" \
  "$CASE_VOLUME_ROOT/$VOLUME_NAME/server_key.pem" \
  "$CASE_VOLUME_ROOT/$VOLUME_NAME/unexpected"
chmod 0700 "$CASE_VOLUME_ROOT/$VOLUME_NAME"
chmod 0600 "$CASE_VOLUME_ROOT/$VOLUME_NAME/server_key.pem" \
  "$CASE_VOLUME_ROOT/$VOLUME_NAME/unexpected"
jq -n --arg name "$VOLUME_NAME" \
  '[{Name:$name,Driver:"local",Options:{},Labels:{}}]' > "$CASE_VOLUME_ROOT/volume.json"
before_hash=$(sha256sum "$CASE_VOLUME_ROOT/$VOLUME_NAME/server_key.pem")
if run_import "$TMP_ROOT/server_key.pem" "$VALID_SPKI"; then
  printf 'identity import accepted a stray file in the existing volume\n' >&2
  exit 1
fi
[[ "$before_hash" == "$(sha256sum "$CASE_VOLUME_ROOT/$VOLUME_NAME/server_key.pem")" ]]
[[ -f "$CASE_VOLUME_ROOT/$VOLUME_NAME/unexpected" ]]
assert_not_contains 'volume create' "$CASE_LOG"
assert_no_secret_output

new_case existing-key-fifo
mkdir -m 0700 "$CASE_VOLUME_ROOT/$VOLUME_NAME"
mkfifo "$CASE_VOLUME_ROOT/$VOLUME_NAME/server_key.pem"
chown 65532:65532 "$CASE_VOLUME_ROOT/$VOLUME_NAME" "$CASE_VOLUME_ROOT/$VOLUME_NAME/server_key.pem"
chmod 0700 "$CASE_VOLUME_ROOT/$VOLUME_NAME"
chmod 0600 "$CASE_VOLUME_ROOT/$VOLUME_NAME/server_key.pem"
jq -n --arg name "$VOLUME_NAME" \
  '[{Name:$name,Driver:"local",Options:{},Labels:{}}]' > "$CASE_VOLUME_ROOT/volume.json"
before_volume_hash=$(sha256sum "$CASE_VOLUME_ROOT/volume.json")
if run_import "$TMP_ROOT/server_key.pem" "$VALID_SPKI"; then
  printf 'identity import accepted a non-regular existing key-volume entry\n' >&2
  exit 1
fi
[[ -p "$CASE_VOLUME_ROOT/$VOLUME_NAME/server_key.pem" ]]
[[ "$before_volume_hash" == "$(sha256sum "$CASE_VOLUME_ROOT/volume.json")" ]]
grep -Fq 'existing Lima key volume does not contain one secure server key' "$CASE_OUTPUT"
[[ "$(<"$CASE_ROOT/import.exit")" != 124 ]]
assert_not_contains 'volume create' "$CASE_LOG"
assert_no_secret_output

new_case existing-same
mkdir -m 0700 "$CASE_VOLUME_ROOT/$VOLUME_NAME"
cp "$TMP_ROOT/server_key.pem" "$CASE_VOLUME_ROOT/$VOLUME_NAME/server_key.pem"
chown 65532:65532 "$CASE_VOLUME_ROOT/$VOLUME_NAME" "$CASE_VOLUME_ROOT/$VOLUME_NAME/server_key.pem"
chmod 0700 "$CASE_VOLUME_ROOT/$VOLUME_NAME"
chmod 0600 "$CASE_VOLUME_ROOT/$VOLUME_NAME/server_key.pem"
jq -n --arg name "$VOLUME_NAME" \
  '[{Name:$name,Driver:"local",Options:{},Labels:{}}]' > "$CASE_VOLUME_ROOT/volume.json"
if ! run_import "$TMP_ROOT/server_key.pem" "$VALID_SPKI"; then
  cat "$CASE_OUTPUT" >&2
  exit 1
fi
assert_not_contains 'volume create' "$CASE_LOG"
assert_not_contains 'dst=/tmp/server_key.pem,readonly' "$CASE_LOG"
grep -Fxq "TG_RSA_KEY_FINGERPRINT=$EXPECTED_FINGERPRINT" "$CASE_ENV"
grep -Fq -- '--log-driver none' "$CASE_LOG"
if [[ -n "${TEAGRAM_TEST_SERVER_IMAGE:-}" ]]; then
  jq -e --arg expected "$VALID_SPKI" '.mtproto.rsa_spki == $expected' \
    "$CASE_ROOT/real-client-config.json" >/dev/null
fi
assert_no_secret_output

new_case postcheck-failure
if TEAGRAM_TEST_FAIL_CLEANUP_INSPECT=once run_import "$TMP_ROOT/server_key.pem" "$VALID_SPKI" 'different-public-identity'; then
  printf 'identity import accepted a client-config SPKI mismatch\n' >&2
  exit 1
fi
[[ ! -e "$CASE_VOLUME_ROOT/$VOLUME_NAME" ]]
assert_not_contains "TG_RSA_KEY_FINGERPRINT=$EXPECTED_FINGERPRINT" "$CASE_ENV"
grep -Fq 'volume rm teagram-demo_tgkey' "$CASE_LOG"
assert_no_secret_output

new_case rollback-inspect-unverified
if TEAGRAM_TEST_FAIL_CLEANUP_INSPECT=always run_import "$TMP_ROOT/server_key.pem" "$VALID_SPKI" 'different-public-identity'; then
  printf 'identity import accepted a client-config SPKI mismatch with unverified rollback\n' >&2
  exit 1
fi
[[ -e "$CASE_VOLUME_ROOT/$VOLUME_NAME" ]]
grep -Fq 'rollback could not verify removal of the key volume' "$CASE_OUTPUT"
assert_no_secret_output

new_case no-import
if env PATH="$MOCK_BIN:$PATH" TEAGRAM_DEMO_LIMA_ENV_FILE="$CASE_ENV" \
  TEAGRAM_TEST_DOCKER_LOG="$CASE_LOG" "$TEST_INIT" "sha256:$IMAGE_DIGEST" \
  >"$CASE_OUTPUT" 2>&1; then
  printf 'Lima init accepted a start without identity import\n' >&2
  exit 1
fi
grep -Fq 'Lima installs require identity import' "$CASE_OUTPUT"
assert_not_contains 'bootstrap-identity' "$CASE_LOG"
assert_no_secret_output

printf 'demo identity import contract holds\n'
