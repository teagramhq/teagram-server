#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
DEMO_REPO_ROOT=$(cd -P "$SCRIPT_DIR/../.." && pwd)

IMPORT_MODE=0
IMAGE_DIGEST=
IMPORT_KEY_SOURCE=
EXPECTED_RSA_SPKI=
if [[ "$#" -eq 1 && "${1:-}" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  IMAGE_DIGEST=$1
elif [[ "$#" -eq 4 && "${1:-}" == --import-key && "${3:-}" == --expected-rsa-spki && -n "${2:-}" && -n "${4:-}" ]]; then
  IMPORT_MODE=1
  IMPORT_KEY_SOURCE=$2
  EXPECTED_RSA_SPKI=$4
  TEAGRAM_DEMO_VARIANT=lima
else
  printf 'teagram-demo: usage: init-demo.sh <trusted-ci-sha256-digest> | init-demo.sh --import-key <copied-server_key.pem> --expected-rsa-spki <value>\n' >&2
  exit 1
fi
readonly IMPORT_MODE IMAGE_DIGEST IMPORT_KEY_SOURCE EXPECTED_RSA_SPKI
export TEAGRAM_DEMO_VARIANT=${TEAGRAM_DEMO_VARIANT:-charlie}
if (( IMPORT_MODE == 0 )) && [[ "$TEAGRAM_DEMO_VARIANT" != charlie ]]; then
  printf 'teagram-demo: unknown Compose variant; refusing identity bootstrap\n' >&2
  exit 1
fi

LIMA_ENV_CANDIDATE=${TEAGRAM_DEMO_LIMA_ENV_FILE:-/var/lib/teagram-demo/lima.env}
if (( IMPORT_MODE == 0 )) && { [[ -v TEAGRAM_DEMO_LIMA_ENV_FILE ]] || [[ -e "$LIMA_ENV_CANDIDATE" || -L "$LIMA_ENV_CANDIDATE" ]]; }; then
  printf 'teagram-demo: Lima installs require identity import; refusing generated identity\n' >&2
  exit 1
fi
if (( IMPORT_MODE == 0 )) && [[ -n "${COMPOSE_FILE:-}" ]] &&
  [[ "$COMPOSE_FILE" != "$DEMO_REPO_ROOT/docker-compose.yml" && "$COMPOSE_FILE" != docker-compose.yml ]]; then
  printf 'teagram-demo: unknown Compose variant; refusing identity bootstrap\n' >&2
  exit 1
fi

demo_lock_init() {
  LOCK_PATH="$DEMO_REPO_ROOT/.state/deploy.lock"
  if [[ "${DEMO_DEPLOY_LOCK_HELD:-0}" == 1 ]]; then
    [[ "$(readlink -e "/proc/$$/fd/9")" == "$LOCK_PATH" ]] || {
      printf 'teagram-demo: deployment caller did not pass the deploy lock\n' >&2
      return 1
    }
    flock -n 9 || {
      printf 'teagram-demo: deployment caller does not hold the deploy lock\n' >&2
      return 1
    }
    return 0
  fi
  mkdir -p "$(dirname "$LOCK_PATH")"
  exec 9> "$LOCK_PATH"
  flock -x 9
}

if [[ "$EUID" -ne 0 ]]; then
  echo 'teagram-demo: must run as root on the demo LXC' >&2
  exit 1
fi
if (( IMPORT_MODE == 0 )); then
  demo_lock_init
fi

# shellcheck source=deploy/teagram-demo/demo-common.sh
source "$SCRIPT_DIR/demo-common.sh"

TMP_DIR=
IMPORT_ENV_NEXT=
IMPORT_CREATED_KEY_VOLUME=0
IMPORT_KEY_VOLUME=
IMPORT_VOLUME_TOKEN=
PUBLIC_SERVICES_MAY_BE_RUNNING=0

lima_compose() {
  env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
    -u POSTGRES_PASSWORD -u TG_PUBLIC_LINK_PREFIX -u TG_ADVERTISE_ADDR \
    -u TG_ADVERTISE_DEMO_GUEST_SIGN_UP -u TG_DEMO_GENERAL_CHAT_ENABLED \
    -u TG_WEBSOCKET_LISTEN_ADDR -u TG_WEBSOCKET_ALLOWED_ORIGINS \
    -u TG_WEBSOCKET_BROWSER_ONLY -u TG_LOG_LOGIN_CODES -u TG_REGISTRATION \
    -u TG_TURNSTILE_SIGNIN_ENABLED -u TG_MAX_FILE_BYTES \
    -u TG_MAX_USER_STORAGE_BYTES -u TG_UPLOAD_MAX_BYTES \
    -u TG_RATE_LIMIT_UPLOAD_BYTES -u TG_RSA_KEY_FINGERPRINT \
    -u TG_AUTHKEY_ENC_KEY -u TG_AUTHKEY_ENC_KEY_FILE \
    -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_TURNSTILE_SECRET_KEY_FILE \
    -u TEAGRAM_DEMO_EDGE_BIND -u TEAGRAM_DEMO_IMAGE_REF \
    "TG_RSA_KEY_FINGERPRINT=$IMPORT_FINGERPRINT" \
    "TEAGRAM_DEMO_SECRETS_DIR=$LIMA_SECRETS_DIR" \
    docker compose --env-file "$DEMO_ENV_FILE" \
      --project-directory "$DEMO_REPO_ROOT" \
      --file "$LIMA_COMPOSE_FILE" --project-name teagram-demo "$@" </dev/null
}

validate_import_key() {
  local key_path=$1 prefix=$2 key_copy key_copy_metadata key_copy_uid key_copy_mode key_copy_size
  key_copy="$TMP_DIR/$prefix-server_key.pem"
  if ! python3 - "$key_path" "$key_copy" \
    >"$TMP_DIR/$prefix-copy.stdout" 2>"$TMP_DIR/$prefix-copy.stderr" <<'PY'
import os
import stat
import sys


source_path, destination_path = sys.argv[1:]
maximum_size = 16384
source_fd = os.open(
    source_path,
    os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW | os.O_NONBLOCK,
)
destination_fd = -1
try:
    before = os.fstat(source_fd)
    mode = stat.S_IMODE(before.st_mode)
    if (
        not stat.S_ISREG(before.st_mode)
        or before.st_uid != 0
        or before.st_size <= 0
        or before.st_size > maximum_size
        or not mode & 0o400
        or mode & 0o077
        or mode & 0o111
    ):
        raise ValueError("unsafe RSA import key")

    destination_fd = os.open(
        destination_path,
        os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_CLOEXEC,
        0o600,
    )
    os.fchown(destination_fd, 0, 0)
    os.fchmod(destination_fd, 0o600)
    remaining = before.st_size
    while remaining:
        block = os.read(source_fd, min(4096, remaining))
        if not block:
            raise ValueError("RSA import key changed while being copied")
        view = memoryview(block)
        while view:
            written = os.write(destination_fd, view)
            view = view[written:]
        remaining -= len(block)
    if os.read(source_fd, 1):
        raise ValueError("RSA import key grew while being copied")

    after = os.fstat(source_fd)
    before_state = (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns, before.st_ctime_ns)
    after_state = (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns, after.st_ctime_ns)
    if before_state != after_state:
        raise ValueError("RSA import key changed while being copied")
    os.fsync(destination_fd)
finally:
    os.close(source_fd)
    if destination_fd >= 0:
        os.close(destination_fd)
PY
  then
    demo_fail 'RSA import key is missing or unsafe'
    return 1
  fi
  if ! key_copy_metadata=$(stat -c '%u:%a:%s' -- "$key_copy" 2>/dev/null); then
    demo_fail 'RSA import key copy metadata could not be read'
    return 1
  fi
  IFS=: read -r key_copy_uid key_copy_mode key_copy_size <<< "$key_copy_metadata"
  if [[ "$key_copy_uid" != 0 || "$key_copy_mode" != 600 || ! "$key_copy_size" =~ ^[0-9]+$ ]] ||
    (( key_copy_size <= 0 || key_copy_size > 16384 )); then
    demo_fail 'RSA import key copy could not be secured'
    return 1
  fi
  if ! python3 - "$key_copy" >"$TMP_DIR/pem-format.stdout" 2>"$TMP_DIR/pem-format.stderr" <<'PY'
import pathlib
import re
import sys

raw = pathlib.Path(sys.argv[1]).read_bytes().strip(b" \t\r\n")
match = re.fullmatch(
    rb"-----BEGIN RSA PRIVATE KEY-----\s+(.*?)\s+-----END RSA PRIVATE KEY-----",
    raw,
    flags=re.DOTALL,
)
if match is None or re.search(rb"(?m)^(?:Proc-Type|DEK-Info):", match.group(1)):
    raise SystemExit(1)
PY
  then
    demo_fail 'RSA import key must be an unencrypted PKCS#1 PEM'
    return 1
  fi
  if ! openssl rsa -in "$key_copy" -passin pass: -check -noout \
    >/dev/null 2>"$TMP_DIR/openssl.stderr"; then
    demo_fail 'RSA import key is not a valid PKCS#1 key'
    return 1
  fi
  if ! python3 - "$key_copy" "$TMP_DIR/$prefix-fingerprint" \
    >"$TMP_DIR/metadata.stdout" 2>"$TMP_DIR/metadata.stderr" <<'PY'
import base64
import hashlib
import pathlib
import re
import sys


def read_tlv(data: bytes, offset: int, tag: int) -> tuple[bytes, int]:
    if offset >= len(data) or data[offset] != tag or offset + 1 >= len(data):
        raise ValueError("invalid DER")
    first = data[offset + 1]
    cursor = offset + 2
    if first & 0x80:
        count = first & 0x7f
        if count == 0 or count > 4 or cursor + count > len(data):
            raise ValueError("invalid DER length")
        if data[cursor] == 0:
            raise ValueError("non-canonical DER length")
        length = int.from_bytes(data[cursor : cursor + count], "big")
        if length < 128:
            raise ValueError("non-canonical DER length")
        cursor += count
    else:
        length = first
    end = cursor + length
    if end > len(data):
        raise ValueError("truncated DER")
    return data[cursor:end], end


def tl_bytes(value: bytes) -> bytes:
    length = len(value)
    if length <= 253:
        encoded = bytes((length,)) + value
    else:
        encoded = b"\xfe" + length.to_bytes(3, "little") + value
    return encoded + b"\x00" * ((-len(encoded)) % 4)


raw = pathlib.Path(sys.argv[1]).read_bytes()
raw = raw.strip(b" \t\r\n")
match = re.fullmatch(
    rb"-----BEGIN RSA PRIVATE KEY-----\s+(.*?)\s+-----END RSA PRIVATE KEY-----",
    raw,
    flags=re.DOTALL,
)
if match is None:
    raise SystemExit(1)
encoded = re.sub(rb"\s+", b"", match.group(1))
der = base64.b64decode(encoded, validate=True)
sequence, sequence_end = read_tlv(der, 0, 0x30)
if sequence_end != len(der):
    raise SystemExit(1)
_, offset = read_tlv(sequence, 0, 0x02)
modulus_bytes, offset = read_tlv(sequence, offset, 0x02)
exponent_bytes, _ = read_tlv(sequence, offset, 0x02)
modulus = int.from_bytes(modulus_bytes, "big")
if modulus.bit_length() != 2048:
    raise SystemExit(1)
modulus_bytes = modulus.to_bytes((modulus.bit_length() + 7) // 8, "big")
exponent = int.from_bytes(exponent_bytes, "big")
exponent_bytes = exponent.to_bytes((exponent.bit_length() + 7) // 8, "big")
digest = hashlib.sha1(tl_bytes(modulus_bytes) + tl_bytes(exponent_bytes)).digest()
fingerprint = int.from_bytes(digest[12:], "little", signed=True)
pathlib.Path(sys.argv[2]).write_text(f"{fingerprint}\n", encoding="ascii")
PY
  then
    demo_fail 'RSA import key is not a single 2048-bit PKCS#1 key'
    return 1
  fi
  if ! { openssl pkey -in "$key_copy" -passin pass: -pubout -outform DER |
    base64 | tr -d '\n' > "$TMP_DIR/$prefix-spki"; } 2>"$TMP_DIR/openssl.stderr"; then
    demo_fail 'RSA import key public identity could not be read'
    return 1
  fi
  local actual_spki
  actual_spki=$(<"$TMP_DIR/$prefix-spki")
  [[ "$actual_spki" == "$EXPECTED_RSA_SPKI" ]] || {
    demo_fail 'RSA import key does not match the expected server identity'
    return 1
  }
  IMPORT_FINGERPRINT=$(<"$TMP_DIR/$prefix-fingerprint")
  [[ "$IMPORT_FINGERPRINT" =~ ^-?[0-9]+$ ]] || {
    demo_fail 'RSA import key fingerprint could not be calculated'
    return 1
  }
}

ensure_lima_image() {
  local image=$1
  if docker image inspect "$image" >/dev/null 2>"$TMP_DIR/image-inspect.stderr"; then
    return 0
  fi
  if ! docker pull --quiet "$image" >"$TMP_DIR/image-pull.stdout" 2>"$TMP_DIR/image-pull.stderr"; then
    demo_fail 'required Lima image is unavailable'
    return 1
  fi
}

inspect_import_volume() {
  local expected_run_label=${1:-}
  if ! docker volume inspect "$IMPORT_KEY_VOLUME" >"$TMP_DIR/key-volume.json" \
    2>"$TMP_DIR/key-volume.stderr"; then
    demo_fail 'Lima key volume could not be inspected'
    return 1
  fi
  if [[ -n "$expected_run_label" ]]; then
    jq -e --arg name "$IMPORT_KEY_VOLUME" --arg token "$expected_run_label" '
      length == 1
      and .[0].Name == $name
      and .[0].Driver == "local"
      and ((.[0].Options // {}) | length == 0)
      and .[0].Labels["com.teagram.demo.identity-import-run"] == $token
    ' "$TMP_DIR/key-volume.json" >/dev/null || {
      demo_fail 'new Lima key volume ownership could not be verified'
      return 1
    }
  else
    jq -e --arg name "$IMPORT_KEY_VOLUME" '
      length == 1
      and .[0].Name == $name
      and .[0].Driver == "local"
      and ((.[0].Options // {}) | length == 0)
    ' "$TMP_DIR/key-volume.json" >/dev/null || {
      demo_fail 'existing Lima key volume is not a plain local volume'
      return 1
    }
  fi
}

read_existing_volume_key() {
  if ! docker run --pull=never --rm --log-driver none --network none --read-only \
    --user 65532:65532 --cap-drop ALL --security-opt no-new-privileges \
    --mount "type=volume,src=$IMPORT_KEY_VOLUME,dst=/key,readonly" \
    --entrypoint /bin/sh "$LIMA_HELPER_IMAGE" -ec '
      set -eu
      [ "$(stat -c "%u:%g:%a" /key)" = 65532:65532:700 ]
      for item in /key/.[!.]* /key/..?* /key/*; do
        [ -e "$item" ] || [ -L "$item" ] || continue
        [ "$item" = /key/server_key.pem ] || exit 41
      done
      if [ ! -f /key/server_key.pem ] || [ -L /key/server_key.pem ]; then
        exit 41
      fi
      [ "$(stat -c "%u:%g:%a" /key/server_key.pem)" = 65532:65532:600 ]
      cat /key/server_key.pem
    ' >"$TMP_DIR/volume-copy-server_key.pem" 2>"$TMP_DIR/volume-read.stderr"; then
    demo_fail 'existing Lima key volume does not contain one secure server key'
    return 1
  fi
  chmod 0600 -- "$TMP_DIR/volume-copy-server_key.pem" || {
    demo_fail 'existing Lima key copy could not be secured'
    return 1
  }
}

publish_imported_volume_key() {
  if ! docker run --pull=never --rm --log-driver none --network none --read-only \
    --user 0:0 --cap-drop ALL --cap-add CHOWN --cap-add DAC_OVERRIDE --cap-add FOWNER \
    --security-opt no-new-privileges \
    --mount "type=bind,src=$TMP_DIR/imported-server_key.pem,dst=/tmp/server_key.pem,readonly" \
    --mount "type=volume,src=$IMPORT_KEY_VOLUME,dst=/key" \
    --entrypoint /bin/sh "$LIMA_HELPER_IMAGE" -ec '
      set -eu
      for item in /key/.[!.]* /key/..?* /key/*; do
        [ -e "$item" ] || [ -L "$item" ] || continue
        exit 41
      done
      chown 65532:65532 /key
      chmod 0700 /key
      temporary=/key/.server_key.pem.import-$$
      cat /tmp/server_key.pem > "$temporary"
      chmod 0600 "$temporary"
      chown 65532:65532 "$temporary"
      ln "$temporary" /key/server_key.pem
      rm "$temporary"
    ' >"$TMP_DIR/publish.stdout" 2>"$TMP_DIR/publish.stderr"; then
    demo_fail 'RSA import key could not be published without replacement'
    return 1
  fi
}

write_lima_fingerprint() {
  IMPORT_ENV_NEXT="$DEMO_ENV_FILE.init.$$"
  [[ ! -e "$IMPORT_ENV_NEXT" && ! -L "$IMPORT_ENV_NEXT" ]] || {
    demo_fail 'Lima environment update path already exists'
    return 1
  }
  if ! python3 - "$DEMO_ENV_FILE" "$IMPORT_ENV_NEXT" "$IMPORT_FINGERPRINT" \
    >"$TMP_DIR/env-write.stdout" 2>"$TMP_DIR/env-write.stderr" <<'PY'
import os
import pathlib
import sys

source = pathlib.Path(sys.argv[1])
destination = pathlib.Path(sys.argv[2])
fingerprint = sys.argv[3]
contents = source.read_bytes()
if any(line.startswith(b"TG_RSA_KEY_FINGERPRINT=") for line in contents.splitlines()):
    raise SystemExit(1)
if contents and not contents.endswith(b"\n"):
    contents += b"\n"
contents += b"TG_RSA_KEY_FINGERPRINT=" + fingerprint.encode("ascii") + b"\n"
fd = os.open(destination, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
try:
    os.fchown(fd, 0, 0)
    os.fchmod(fd, 0o600)
    with os.fdopen(fd, "wb", closefd=False) as output:
        output.write(contents)
        output.flush()
        os.fsync(fd)
finally:
    os.close(fd)
PY
  then
    demo_fail 'Lima RSA fingerprint could not be prepared safely'
    return 1
  fi
  if ! python3 "$DEMO_SCRIPT_DIR/validate-env.py" --lima "$IMPORT_ENV_NEXT" \
    >"$TMP_DIR/env-validate.stdout" 2>"$TMP_DIR/env-validate.stderr"; then
    demo_fail 'Lima environment failed validation after fingerprint update'
    return 1
  fi
  if ! mv -f -- "$IMPORT_ENV_NEXT" "$DEMO_ENV_FILE"; then
    demo_fail 'Lima RSA fingerprint could not be committed'
    return 1
  fi
  IMPORT_ENV_NEXT=
}

run_lima_identity_import() {
  local existing_volume=0 current_fingerprints=()
  LIMA_COMPOSE_FILE="$SCRIPT_DIR/lima/compose.yaml"
  LIMA_SECRETS_DIR=${TEAGRAM_DEMO_SECRETS_DIR:-/var/lib/teagram-demo/secrets}
  [[ -f "$LIMA_COMPOSE_FILE" && ! -L "$LIMA_COMPOSE_FILE" ]] || {
    demo_fail 'Lima Compose variant is missing or unsafe'
    return 1
  }
  [[ "$EXPECTED_RSA_SPKI" =~ ^[A-Za-z0-9+/]+={0,2}$ && ${#EXPECTED_RSA_SPKI} -le 4096 ]] || {
    demo_fail 'expected Lima RSA identity is invalid'
    return 1
  }
  TMP_DIR=$(mktemp -d /tmp/teagram-demo-identity.XXXXXXXX) || {
    demo_fail 'temporary identity directory could not be created'
    return 1
  }
  chmod 0700 "$TMP_DIR" || {
    demo_fail 'temporary identity directory could not be secured'
    return 1
  }
  validate_import_key "$IMPORT_KEY_SOURCE" imported || return 1

  mapfile -t current_fingerprints < <(sed -n 's/^TG_RSA_KEY_FINGERPRINT=//p' "$DEMO_ENV_FILE")
  ((${#current_fingerprints[@]} <= 1)) || {
    demo_fail 'Lima environment has duplicate RSA fingerprint settings'
    return 1
  }
  if ((${#current_fingerprints[@]} == 1)) &&
    [[ "${current_fingerprints[0]}" != "$IMPORT_FINGERPRINT" ]]; then
    demo_fail 'Lima environment fingerprint differs from the imported key'
    return 1
  fi

  if ! lima_compose config --format json \
    >"$TMP_DIR/lima-compose.json" 2>"$TMP_DIR/lima-compose.stderr"; then
    demo_fail 'Lima Compose configuration is invalid'
    return 1
  fi
  chmod 0600 "$TMP_DIR/lima-compose.json"
  jq -e --arg fingerprint "$IMPORT_FINGERPRINT" '
    .name == "teagram-demo"
    and (.volumes.tgkey.driver // "local") == "local"
    and ((.volumes.tgkey.driver_opts // {}) | length == 0)
    and (.volumes.tgkey.name // "teagram-demo_tgkey") == "teagram-demo_tgkey"
    and .services.teagramd.environment.TG_RSA_KEY_FINGERPRINT == $fingerprint
    and .services.teagramd.environment.TG_RSA_KEY_PATH == "/var/lib/teagramd/server_key.pem"
    and (.services | has("teagramd-next") | not)
    and (.services.teagramd.image | test("@sha256:[0-9a-f]{64}$"))
    and (.services.postgres.image | test("@sha256:[0-9a-f]{64}$"))
    and ([.services.teagramd.volumes[] | select(.type == "volume" and .target == "/var/lib/teagramd" and .source == "tgkey")] | length == 1)
  ' "$TMP_DIR/lima-compose.json" >/dev/null || {
    demo_fail 'Compose configuration is not the pinned Lima identity variant'
    return 1
  }
  IMPORT_KEY_VOLUME=$(jq -er '.volumes.tgkey.name // "teagram-demo_tgkey"' "$TMP_DIR/lima-compose.json") || {
    demo_fail 'Lima key volume name could not be resolved'
    return 1
  }
  LIMA_HELPER_IMAGE=$(jq -er '.services.postgres.image' "$TMP_DIR/lima-compose.json") || {
    demo_fail 'Lima helper image could not be resolved'
    return 1
  }
  LIMA_SERVER_IMAGE=$(jq -er '.services.teagramd.image' "$TMP_DIR/lima-compose.json") || {
    demo_fail 'Lima server image could not be resolved'
    return 1
  }
  demo_lock_init || return 1
  if ! docker volume ls --quiet --filter "name=$IMPORT_KEY_VOLUME" \
    >"$TMP_DIR/key-volume-list" 2>"$TMP_DIR/key-volume-list.stderr"; then
    demo_fail 'Lima key volume list could not be read'
    return 1
  fi
  if grep -Fxq "$IMPORT_KEY_VOLUME" "$TMP_DIR/key-volume-list"; then
    existing_volume=1
    inspect_import_volume || return 1
    ensure_lima_image "$LIMA_HELPER_IMAGE" || return 1
    read_existing_volume_key || return 1
    validate_import_key "$TMP_DIR/volume-copy-server_key.pem" existing || return 1
    local existing_spki
    existing_spki=$(<"$TMP_DIR/existing-spki")
    [[ "$existing_spki" == "$EXPECTED_RSA_SPKI" ]] || {
      demo_fail 'existing Lima key volume holds a different identity'
      return 1
    }
  fi

  ensure_lima_image "$LIMA_SERVER_IMAGE" || return 1
  if (( existing_volume == 0 )); then
    ensure_lima_image "$LIMA_HELPER_IMAGE" || return 1
    IMPORT_VOLUME_TOKEN="init-$$-$RANDOM"
    IMPORT_CREATED_KEY_VOLUME=1
    if ! docker volume create --driver local \
      --label com.docker.compose.project=teagram-demo \
      --label com.docker.compose.volume=tgkey \
      --label "com.teagram.demo.identity-import-run=$IMPORT_VOLUME_TOKEN" \
      "$IMPORT_KEY_VOLUME" >"$TMP_DIR/volume-create.stdout" 2>"$TMP_DIR/volume-create.stderr"; then
      demo_fail 'Lima key volume could not be created'
      return 1
    fi
    inspect_import_volume "$IMPORT_VOLUME_TOKEN" || return 1
    publish_imported_volume_key || return 1
  fi

  if ! jq -er '
    .services.teagramd.environment
    | with_entries(select(.key as $key |
        ["TG_RSA_KEY_PATH", "TG_ADVERTISE_ADDR", "TG_WEBSOCKET_LISTEN_ADDR",
         "TG_WEBSOCKET_BROWSER_ONLY", "TG_TURNSTILE_SIGNIN_ENABLED",
         "TG_TURNSTILE_TEST_MODE", "TG_TURNSTILE_HOSTNAMES"] | index($key)))
    | .TG_ADVERTISE_DEMO_GUEST_SIGN_UP = "false"
    | .TG_TURNSTILE_SITE_KEY = "fixture-site-key"
    | .TG_TURNSTILE_SECRET_KEY = "fixture-verification-secret"
    | .TG_TURNSTILE_SECRET_KEY_FILE = ""
    | .TG_TURNSTILE_QA_BYPASS_TOKEN_FILE = ""
    | to_entries[] | "\(.key)=\(.value)"
  ' \
    "$TMP_DIR/lima-compose.json" >"$TMP_DIR/client-config.env" 2>"$TMP_DIR/client-config-env.stderr"; then
    demo_fail 'Lima client-config environment could not be prepared'
    return 1
  fi
  chmod 0600 "$TMP_DIR/client-config.env"
  if ! docker run --pull=never --rm --network none --read-only \
    --user 65532:65532 --cap-drop ALL --security-opt no-new-privileges \
    --env-file "$TMP_DIR/client-config.env" \
    --mount "type=volume,src=$IMPORT_KEY_VOLUME,dst=/var/lib/teagramd,readonly" \
    "$LIMA_SERVER_IMAGE" client-config \
    >"$TMP_DIR/client-config.json" 2>"$TMP_DIR/client-config.stderr"; then
    demo_fail 'Lima client-config verification failed'
    return 1
  fi
  jq -e --arg expected "$EXPECTED_RSA_SPKI" \
    '.mtproto.rsa_spki == $expected' "$TMP_DIR/client-config.json" >/dev/null || {
    demo_fail 'Lima client-config reported a different RSA identity'
    return 1
  }
  if ((${#current_fingerprints[@]} == 0)); then
    write_lima_fingerprint || return 1
  fi
  printf 'teagram-demo: Lima identity imported and verified\n'
}

cleanup() {
  local rc=$?
  local import_env_next=${IMPORT_ENV_NEXT:-}
  local import_created_volume=${IMPORT_CREATED_KEY_VOLUME:-0}
  local import_key_volume=${IMPORT_KEY_VOLUME:-}
  local import_volume_token=${IMPORT_VOLUME_TOKEN:-}
  trap - EXIT
  if (( rc != 0 && PUBLIC_SERVICES_MAY_BE_RUNNING == 1 )); then
    if ! demo_compose stop edge haproxy; then
      printf 'teagram-demo: failed to stop public ingress during install cleanup\n' >&2
    fi
  fi
  if [[ -n "$TMP_DIR" && -d "$TMP_DIR" ]]; then
    if (( rc != 0 && import_created_volume == 1 )) && [[ -n "$import_key_volume" && -n "$import_volume_token" ]]; then
      local cleanup_attempt cleanup_verified=0
      for cleanup_attempt in 1 2 3; do
        if docker volume inspect "$import_key_volume" >"$TMP_DIR/cleanup-volume.json" \
          2>"$TMP_DIR/cleanup-volume.stderr"; then
          if ! jq -e --arg name "$import_key_volume" --arg token "$import_volume_token" '
            length == 1
            and .[0].Name == $name
            and .[0].Driver == "local"
            and ((.[0].Options // {}) | length == 0)
            and .[0].Labels["com.teagram.demo.identity-import-run"] == $token
          ' "$TMP_DIR/cleanup-volume.json" >/dev/null; then
            printf 'teagram-demo: rollback refused to remove a key volume whose ownership changed\n' >&2
            break
          fi
          docker volume rm "$import_key_volume" >"$TMP_DIR/cleanup-volume-rm.stdout" \
            2>"$TMP_DIR/cleanup-volume-rm.stderr" || true
        fi
        if docker volume ls --quiet --filter "name=$import_key_volume" \
          >"$TMP_DIR/cleanup-volume-list" 2>"$TMP_DIR/cleanup-volume-list.stderr" &&
          ! grep -Fxq "$import_key_volume" "$TMP_DIR/cleanup-volume-list"; then
          cleanup_verified=1
          break
        fi
        if (( cleanup_attempt < 3 )); then
          sleep 1
        fi
      done
      if (( cleanup_verified == 0 )); then
        printf 'teagram-demo: rollback could not verify removal of the key volume created by this import\n' >&2
        rc=1
      fi
    fi
    if ! rm -rf -- "$TMP_DIR"; then
      printf 'teagram-demo: failed to remove temporary install state\n' >&2
      (( rc != 0 )) || rc=1
    fi
  fi
  if [[ -n "$import_env_next" && ( -e "$import_env_next" || -L "$import_env_next" ) ]]; then
    if ! rm -f -- "$import_env_next"; then
      printf 'teagram-demo: failed to remove temporary Lima environment state\n' >&2
      (( rc != 0 )) || rc=1
    fi
  fi
  exit "$rc"
}
trap cleanup EXIT

demo_require_env
if (( IMPORT_MODE == 1 )); then
  run_lima_identity_import
  exit 0
fi
ACTIVE_SERVER=$(demo_active_server)
ACTIVE_ENDPOINT="$ACTIVE_SERVER:2443"
SEED_SCRIPT="$SCRIPT_DIR/seed-demo.sh"
git -C "$DEMO_REPO_ROOT" ls-files --error-unmatch deploy/teagram-demo/seed-demo.sh >/dev/null 2>&1 ||
  demo_fail 'initial install is disabled until the tracked API seed-demo.sh is supplied'
[[ -f "$SEED_SCRIPT" && ! -L "$SEED_SCRIPT" && -x "$SEED_SCRIPT" ]] ||
  demo_fail 'tracked API seed-demo.sh must be executable'
"$SEED_SCRIPT" --check

git -C "$DEMO_REPO_ROOT" diff --quiet HEAD -- || demo_fail 'checkout has tracked changes'
[[ "$(git -C "$DEMO_REPO_ROOT" branch --show-current)" == main ]] || demo_fail 'checkout must be on main'
git -C "$DEMO_REPO_ROOT" fetch -q origin main
SHA=$(git -C "$DEMO_REPO_ROOT" rev-parse HEAD)
git -C "$DEMO_REPO_ROOT" merge-base --is-ancestor "$SHA" origin/main || demo_fail 'checkout is not on origin/main'

mkdir -p "$DEMO_STATE_DIR"
chown root:root "$DEMO_STATE_DIR"
chmod 0755 "$DEMO_STATE_DIR"
TMP_DIR=$(mktemp -d "$DEMO_REPO_ROOT/.state/teagram-demo/init.XXXXXXXX")
chmod 0700 "$TMP_DIR"
TARGET_RENDER="$TMP_DIR/target-render.json"
INITIAL_RENDER="$TMP_DIR/initial-render.json"
INITIAL_INVENTORY="$TMP_DIR/initial-inventory.json"
MODE_HELPER="$DEMO_SCRIPT_DIR/blob-mode/blob-mode-state.py"

demo_render_config "$TARGET_RENDER"
demo_compose --profile maintenance --profile bootstrap config --format json \
  > "$TMP_DIR/charlie-identity-render.json"
jq -e '
  .services.teagramd != null
  and .services.teagramd.environment.TG_RSA_KEY_PATH == "/var/lib/teagramd/server_key.pem"
  and .services["teagramd-maintenance"].image == .services.teagramd.image
  and .services["teagramd-maintenance"].environment.TG_RSA_KEY_PATH == "/var/lib/teagramd/server_key.pem"
  and .services["teagramd-bootstrap"].image == .services.teagramd.image
  and .services["teagramd-bootstrap"].environment.TG_RSA_KEY_PATH == "/var/lib/teagramd/server_key.pem"
  and ([.services["teagramd-maintenance"].volumes[] | select(.type == "volume" and .source == "tgkey" and .target == "/var/lib/teagramd")] | length == 1)
  and ([.services["teagramd-bootstrap"].volumes[] | select(.type == "volume" and .source == "tgkey" and .target == "/var/lib/teagramd")] | length == 1)
' "$TMP_DIR/charlie-identity-render.json" >/dev/null 2>"$TMP_DIR/charlie-variant.stderr" ||
  demo_fail 'identity bootstrap is not enabled by the Charlie Compose model'
demo_check_blob_mount "$TARGET_RENDER"
BLOB_PATH=$(jq -er '.volumes.tgblobs.driver_opts.device' "$TARGET_RENDER")
demo_seed_compose config --format json > "$INITIAL_RENDER"
chmod 0600 "$INITIAL_RENDER"
demo_check_blob_mount "$INITIAL_RENDER"
[[ "$(jq -er '.volumes.tgblobs.driver_opts.device' "$INITIAL_RENDER")" == "$BLOB_PATH" ]] ||
  demo_fail 'seed and production Compose renders use different blob paths'
python3 "$MODE_HELPER" compose --checkout "$DEMO_REPO_ROOT" \
  < "$INITIAL_RENDER" > "$INITIAL_INVENTORY"

MODE_FILE="$DEMO_AUTHORITY_DIR/mode.json"
JOURNAL_DIR="$DEMO_AUTHORITY_DIR/journal"
if [[ -e "$MODE_FILE" || -L "$MODE_FILE" ]]; then
  # Resume a valid, already-published first-install authority after an
  # interrupted switch from the unguarded bootstrap container.
  demo_compose stop edge haproxy turnstile-egress
  mapfile -t CONTAINER_IDS < <(demo_bootstrap_compose ps -q)
  if ((${#CONTAINER_IDS[@]})); then
    docker inspect "${CONTAINER_IDS[@]}" > "$TMP_DIR/containers.raw.json"
  else
    printf '[]\n' > "$TMP_DIR/containers.raw.json"
  fi
  python3 "$MODE_HELPER" containers \
    --checkout "$DEMO_REPO_ROOT" < "$TMP_DIR/containers.raw.json" > "$TMP_DIR/containers.json"
  cp "$INITIAL_INVENTORY" "$TMP_DIR/target-preflight.json"
  python3 "$MODE_HELPER" validate \
    --state-dir "$DEMO_AUTHORITY_DIR" \
    --report-root "$DEMO_STATE_DIR/reports" \
    --containers "$TMP_DIR/containers.json" \
    --compose "$INITIAL_INVENTORY" \
    --override /dev/null \
    --checkout "$DEMO_REPO_ROOT" \
    --allow-empty-containers \
    --allow-unguarded-initial-local \
    --allow-unguarded-initial-local-containers

  IMAGE_REF=$(demo_pull_image_by_sha "$SHA" "$IMAGE_DIGEST")
  export TEAGRAM_DEMO_IMAGE_REF="$IMAGE_REF"
  # Recreate the project networks from the current IPAM config, including the
  # backend dynamic-address pool. Compose down leaves named volumes intact.
  demo_compose down --remove-orphans
  demo_bootstrap_compose up -d migrate
  demo_wait_migration demo_bootstrap_compose 'resume migrations'
  demo_compose run --rm --no-deps teagramd-maintenance client-config > "$TMP_DIR/client.json"
  jq -e '
    .version == 1
    and .mtproto.endpoint == "demo.teagram.io:443"
    and (.mtproto.rsa_spki | type == "string" and length > 0)
    and .websocket.endpoint == "wss://demo.teagram.io/apiws"
    and .demo.guest_sign_up == true
    and (.demo.wipe_generation | type == "string" and test("^[0-9a-f]{64}$"))
  ' "$TMP_DIR/client.json" >/dev/null || demo_fail 'client-config lacks the approved demo wipe generation binding'
  install -o root -g root -m 0644 "$TMP_DIR/client.json" "$DEMO_CLIENT_CONFIG.next"
  mv -f -- "$DEMO_CLIENT_CONFIG.next" "$DEMO_CLIENT_CONFIG"
  demo_prepare_tailnet_client_config
  demo_write_active_release "$ACTIVE_SERVER" "$IMAGE_REF"
  demo_seed_compose up -d "$ACTIVE_SERVER"
  demo_prepare_seed_owner "$TMP_DIR"
  "$SEED_SCRIPT" --seed \
    --endpoint "$ACTIVE_ENDPOINT" \
    --owner-username "$DEMO_SEED_OWNER_USERNAME" \
    --owner-password-file "$DEMO_SEED_PASSWORD_FILE"
  "$SEED_SCRIPT" --verify-seed --endpoint "$ACTIVE_ENDPOINT"
  "$SCRIPT_DIR/test-seed-compose.sh"

  # The seed and convergence checks exercise auth flows with relaxed rate
  # limits. Remove only the limiter state they created before the private
  # production-config Turnstile probe runs.
  demo_compose exec -T postgres psql -v ON_ERROR_STOP=1 -U postgres -d teagram \
    -c 'DELETE FROM rate_limits; DELETE FROM send_code_ip_calls; DELETE FROM send_code_ip_phones; DELETE FROM public_send_code_ip_calls; DELETE FROM public_send_code_ip_phones;'

  PUBLIC_SERVICES_MAY_BE_RUNNING=1
  demo_compose up -d --wait --wait-timeout 120 "$ACTIVE_SERVER" haproxy
  "$SEED_SCRIPT" --verify-turnstile --endpoint haproxy:2443
  demo_compose up -d edge
  TAILNET_SMOKE_ARGS=()
  TAILNET_ENABLED=$(demo_tailnet_enabled)
  if [[ "$TAILNET_ENABLED" == true ]]; then
    TAILNET_SMOKE_ARGS+=(--tailnet-port 8081)
  fi
  python3 "$SCRIPT_DIR/smoke.py" --bootstrap "${TAILNET_SMOKE_ARGS[@]}" "$DEMO_CLIENT_CONFIG"
  demo_persist_tailnet_enabled "$TAILNET_ENABLED"
  echo "demo install resumed and seeded at $SHA"
  exit 0
fi
[[ "$ACTIVE_SERVER" == teagramd ]] || demo_fail 'fresh installs must begin in the teagramd slot'
if [[ -e "$JOURNAL_DIR" || -L "$JOURNAL_DIR" ]]; then
  demo_fail 'blob-mode journal exists without mode.json; preserve state and reconcile it before retrying'
fi

for volume in teagram-demo_pgdata teagram-demo_tgkey teagram-demo_tgblobs; do
  if docker volume inspect "$volume" >/dev/null 2>&1; then
    demo_fail "persistent volume $volume already exists without a published mode record"
  fi
done
python3 - "$BLOB_PATH" <<'PY'
import os
import sys

try:
    entries = list(os.scandir(sys.argv[1]))
except OSError as error:
    raise SystemExit(f"demo blob path cannot be listed: {error}")
if entries:
    raise SystemExit("demo blob path is not empty; refusing fresh installation")
PY

IMAGE_REF=$(demo_pull_image_by_sha "$SHA" "$IMAGE_DIGEST")
export TEAGRAM_DEMO_IMAGE_REF="$IMAGE_REF"
demo_write_active_release "$ACTIVE_SERVER" "$IMAGE_REF"
"$SEED_SCRIPT" --check

demo_compose run --rm --no-deps teagramd-maintenance bootstrap-identity > "$TMP_DIR/bootstrap.stdout" 2> "$TMP_DIR/bootstrap.stderr"
FINGERPRINT=$(sed -n 's/.*fingerprint=\(-\{0,1\}[0-9][0-9]*\).*/\1/p' "$TMP_DIR/bootstrap.stdout" | head -n 1)
[[ "$FINGERPRINT" =~ ^-?[0-9]+$ ]] || demo_fail 'identity bootstrap did not report an RSA fingerprint'

demo_bootstrap_compose up -d migrate
demo_wait_migration demo_bootstrap_compose 'bootstrap migrations'
demo_compose run --rm --no-deps teagramd-maintenance client-config > "$TMP_DIR/client.json"
jq -e '
  .version == 1
  and .mtproto.endpoint == "demo.teagram.io:443"
  and (.mtproto.rsa_spki | type == "string" and length > 0)
  and .websocket.endpoint == "wss://demo.teagram.io/apiws"
  and .demo.guest_sign_up == true
  and (.demo.wipe_generation | type == "string" and test("^[0-9a-f]{64}$"))
' "$TMP_DIR/client.json" >/dev/null || demo_fail 'client-config lacks the approved demo wipe generation binding; wait for MAIN-2196'
install -o root -g root -m 0644 "$TMP_DIR/client.json" "$DEMO_CLIENT_CONFIG.next"
mv -f -- "$DEMO_CLIENT_CONFIG.next" "$DEMO_CLIENT_CONFIG"
demo_prepare_tailnet_client_config
demo_bootstrap_compose up -d teagramd

mapfile -t CONTAINER_IDS < <(demo_bootstrap_compose ps -q)
((${#CONTAINER_IDS[@]} >= 2)) || demo_fail 'bootstrap server and database containers are not both running'
docker inspect "${CONTAINER_IDS[@]}" > "$TMP_DIR/baseline-containers.raw.json"
python3 "$MODE_HELPER" containers --checkout "$DEMO_REPO_ROOT" \
  < "$TMP_DIR/baseline-containers.raw.json" > "$TMP_DIR/baseline-containers.json"
docker inspect "${CONTAINER_IDS[@]}" > "$TMP_DIR/current-containers.raw.json"
python3 "$MODE_HELPER" containers --checkout "$DEMO_REPO_ROOT" \
  < "$TMP_DIR/current-containers.raw.json" > "$TMP_DIR/current-containers.json"
cp "$INITIAL_INVENTORY" "$TMP_DIR/target-preflight.json"
TARGET_ARTIFACT_SHA=$(sha256sum "$DEMO_REPO_ROOT/docker-compose.yml")
TARGET_ARTIFACT_SHA=${TARGET_ARTIFACT_SHA%% *}
LOCK_PATH=$(realpath -m "$LOCK_PATH")

python3 "$MODE_HELPER" initialize-local \
  --state-dir "$DEMO_AUTHORITY_DIR" \
  --report-root "$DEMO_STATE_DIR/reports" \
  --baseline-containers "$TMP_DIR/baseline-containers.json" \
  --current-containers "$TMP_DIR/current-containers.json" \
  --preflight-target-compose "$TMP_DIR/target-preflight.json" \
  --target-compose "$INITIAL_INVENTORY" \
  --target-artifact-sha256 "$TARGET_ARTIFACT_SHA" \
  --override /dev/null \
  --checkout "$DEMO_REPO_ROOT" \
  --target-sha "$SHA" \
  --baseline-sha "$SHA" \
  --lock-path "$LOCK_PATH"

demo_reset_seed_owner_marker
demo_seed_compose up -d teagramd
demo_prepare_seed_owner "$TMP_DIR"
"$SEED_SCRIPT" --seed \
  --endpoint teagramd:2443 \
  --owner-username "$DEMO_SEED_OWNER_USERNAME" \
  --owner-password-file "$DEMO_SEED_PASSWORD_FILE"
"$SEED_SCRIPT" --verify-seed --endpoint teagramd:2443
"$SCRIPT_DIR/test-seed-compose.sh"

# The seed and convergence checks exercise auth flows with relaxed rate
# limits. Remove only the limiter state they created before the private
# production-config Turnstile probe runs.
demo_compose exec -T postgres psql -v ON_ERROR_STOP=1 -U postgres -d teagram \
  -c 'DELETE FROM rate_limits; DELETE FROM send_code_ip_calls; DELETE FROM send_code_ip_phones; DELETE FROM public_send_code_ip_calls; DELETE FROM public_send_code_ip_phones;'

PUBLIC_SERVICES_MAY_BE_RUNNING=1
demo_compose up -d --wait --wait-timeout 120 "$ACTIVE_SERVER" haproxy
"$SEED_SCRIPT" --verify-turnstile --endpoint haproxy:2443
demo_compose up -d edge
TAILNET_SMOKE_ARGS=()
TAILNET_ENABLED=$(demo_tailnet_enabled)
if [[ "$TAILNET_ENABLED" == true ]]; then
  TAILNET_SMOKE_ARGS+=(--tailnet-port 8081)
fi
python3 "$SCRIPT_DIR/smoke.py" --bootstrap "${TAILNET_SMOKE_ARGS[@]}" "$DEMO_CLIENT_CONFIG"
demo_persist_tailnet_enabled "$TAILNET_ENABLED"
echo "demo install ready sha=$SHA rsa_fingerprint=$FINGERPRINT seeded=true"
