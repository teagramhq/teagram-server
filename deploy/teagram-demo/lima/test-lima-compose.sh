#!/usr/bin/env bash
# Resolves the lima Compose stack against fixture values and asserts the lima
# acceptance criteria: no qa-invite and no build keys, digest-pinned images,
# memory limits within the 200 MiB budget, the blob bind, fixed subnets, and
# secrets named by check.sh. It also checks that lima's teagramd environment
# matches the charlie file except for the lima-specific memory and pool caps,
# and that check.sh never prints a secret.
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -P "$SCRIPT_DIR/../../.." && pwd)
LIMA_COMPOSE="$SCRIPT_DIR/compose.yaml"
ROOT_COMPOSE="$REPO_ROOT/docker-compose.yml"
TMP_DIR=$(mktemp -d)
trap 'rm -rf -- "$TMP_DIR"' EXIT

failures=0
ok() { printf 'ok - %s\n' "$1"; }
not_ok() {
  printf 'not ok - %s\n' "$1" >&2
  failures=$((failures + 1))
}

# Fixture values. The password is distinctive so the leak check can find it.
PASSWORD_FIXTURE=fixture-db-password-7f3a
DIGEST=$(printf 'a%.0s' {1..64})
ENV_FILE=$TMP_DIR/lima.env
SECRETS_DIR=$TMP_DIR/secrets
MOCK_BIN=$TMP_DIR/mock-bin
REAL_STAT=$(command -v stat)
REAL_DOCKER=$(command -v docker)
REAL_MOUNTPOINT=$(command -v mountpoint)
REAL_FINDMNT=$(command -v findmnt)
REAL_FALLOCATE=$(command -v fallocate)
REAL_MKFS_EXT4=$(command -v mkfs.ext4)
export TEAGRAM_TEST_REAL_STAT="$REAL_STAT"
mkdir -p "$SECRETS_DIR"
mkdir -p "$MOCK_BIN"
chmod 700 "$SECRETS_DIR"
cat > "$ENV_FILE" <<EOF
POSTGRES_PASSWORD=$PASSWORD_FIXTURE
TG_PUBLIC_LINK_PREFIX=https://demo.teagram.io/
TG_AUTHKEY_ENC_KEY=$DIGEST
TEAGRAM_DEMO_TURNSTILE_SITE_KEY=fixture-site-key
TEAGRAM_DEMO_IMAGE_REF=teagramd@sha256:$DIGEST
TG_RSA_KEY_FINGERPRINT=42
EOF
chmod 600 "$ENV_FILE"
printf 'fixture\n' > "$SECRETS_DIR/turnstile-secret-key"
printf 'fixture\n' > "$SECRETS_DIR/turnstile-qa-bypass-token"
chmod 440 "$SECRETS_DIR/turnstile-secret-key" "$SECRETS_DIR/turnstile-qa-bypass-token"
printf 'TUNNEL_TOKEN=fixture-tunnel-token\n' > "$SECRETS_DIR/cloudflared.env"
chmod 600 "$SECRETS_DIR/cloudflared.env"

# The runner's fixtures belong to the test user. Report the documented host
# ownership to check.sh, with a switch for exercising its ownership failures.
cat > "$MOCK_BIN/stat" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
if [[ ${1:-} == -f && ${2:-} == -c && ${3:-} == '%b %S' && ${4:-} == -- \
  && ${5:-} == "$TEAGRAM_DEMO_BLOB_MOUNT" ]]; then
  printf '%s %s\n' "${TEAGRAM_TEST_STATFS_BLOCKS:-983040}" "${TEAGRAM_TEST_STATFS_BLOCK_SIZE:-4096}"
  printf '%s %s %s\n' "$5" "${TEAGRAM_TEST_STATFS_BLOCKS:-983040}" "${TEAGRAM_TEST_STATFS_BLOCK_SIZE:-4096}" \
    >> "$TEAGRAM_TEST_STATFS_LOG"
  exit 0
fi
if [[ ${1:-} == -f && ${2:-} == -c && ${3:-} == '%a %S' && ${4:-} == -- \
  && -n ${TEAGRAM_TEST_AVAILABLE_BYTES:-} ]]; then
  printf '%s 1\n' "$TEAGRAM_TEST_AVAILABLE_BYTES"
  exit 0
fi
if [[ ${1:-} == -c && ${2:-} == '%u:%g %a' && ${3:-} == -- \
  && ${4:-} == "$TEAGRAM_DEMO_BLOB_MOUNT/data" ]]; then
  printf '65532:65532 700\n'
  exit 0
fi
if [[ ${1:-} == -c && ${2:-} == '%u:%g:%a' && ${3:-} == -- ]]; then
  path=$4
  owner=
  case "$path" in
    "$TEAGRAM_DEMO_LIMA_ENV_FILE"|"$TEAGRAM_DEMO_SECRETS_DIR/cloudflared.env") owner=0:0 ;;
    "$TEAGRAM_DEMO_SECRETS_DIR") owner=0:0 ;;
    "$TEAGRAM_DEMO_SECRETS_DIR/turnstile-secret-key"|"$TEAGRAM_DEMO_SECRETS_DIR/turnstile-qa-bypass-token") owner=0:65532 ;;
  esac
  if [[ -n $owner ]]; then
    [[ ${TEAGRAM_TEST_BAD_OWNER_PATH:-} != "$path" ]] || owner=1000:1000
    printf '%s:%s\n' "$owner" "$("$TEAGRAM_TEST_REAL_STAT" -c '%a' -- "$path")"
    exit 0
  fi
fi
exec "$TEAGRAM_TEST_REAL_STAT" "$@"
EOF
chmod 755 "$MOCK_BIN/stat"

cat > "$MOCK_BIN/mountpoint" <<'EOF'
#!/usr/bin/env bash
if [[ ${1:-} == -q && ${2:-} == "$TEAGRAM_DEMO_BLOB_MOUNT" ]]; then
  [[ ${TEAGRAM_TEST_MOUNTED:-0} == 1 ]]
  exit
fi
exec "$TEAGRAM_TEST_REAL_MOUNTPOINT" "$@"
EOF
chmod 755 "$MOCK_BIN/mountpoint"

cat > "$MOCK_BIN/findmnt" <<'EOF'
#!/usr/bin/env bash
if [[ ${1:-} == -n && ${2:-} == -o && ${3:-} == OPTIONS \
  && ${4:-} == --target && ${5:-} == "$TEAGRAM_DEMO_BLOB_MOUNT" ]]; then
  if [[ ${TEAGRAM_TEST_MOUNT_READONLY:-0} == 1 ]]; then
    printf 'ro,relatime\n'
  else
    printf 'rw,relatime\n'
  fi
  exit 0
fi
exec "$TEAGRAM_TEST_REAL_FINDMNT" "$@"
EOF
chmod 755 "$MOCK_BIN/findmnt"

cat > "$MOCK_BIN/docker" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
if [[ ${1:-} == network && ${2:-} == ls && ${3:-} == -q ]]; then
  if [[ ${TEAGRAM_TEST_FAIL_NETWORK_LS:-0} == 1 ]]; then
    printf 'mock network-list permission denied\n' >&2
    exit 42
  fi
  if [[ -n ${TEAGRAM_TEST_NETWORK_IDS_FILE:-} ]]; then
    cat "$TEAGRAM_TEST_NETWORK_IDS_FILE"
  fi
  exit 0
fi
if [[ ${1:-} == network && ${2:-} == inspect ]]; then
  if [[ ${TEAGRAM_TEST_FAIL_NETWORK_INSPECT:-0} == 1 ]]; then
    printf 'mock network-inspect permission denied\n' >&2
    exit 43
  fi
  if [[ -n ${TEAGRAM_TEST_NETWORK_INSPECT_FILE:-} ]]; then
    cat "$TEAGRAM_TEST_NETWORK_INSPECT_FILE"
    exit 0
  fi
fi
if [[ ${1:-} == compose && -n ${TEAGRAM_TEST_CONFIG_FILE:-} ]]; then
  cat "$TEAGRAM_TEST_CONFIG_FILE"
  exit 0
fi
exec "$TEAGRAM_TEST_REAL_DOCKER" "$@"
EOF
chmod 755 "$MOCK_BIN/docker"

cat > "$MOCK_BIN/blkid" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "${TEAGRAM_TEST_BLKID_TYPE:-}"
exit "${TEAGRAM_TEST_BLKID_STATUS:-0}"
EOF
chmod 755 "$MOCK_BIN/blkid"

cat > "$MOCK_BIN/mkfs.ext4" <<'EOF'
#!/usr/bin/env bash
printf 'format %s\n' "$*" >> "$TEAGRAM_TEST_MKFS_LOG"
if [[ ${TEAGRAM_TEST_MKFS_FAIL:-0} == 1 ]]; then
  exit 9
fi
EOF
chmod 755 "$MOCK_BIN/mkfs.ext4"

cat > "$MOCK_BIN/fallocate" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
if [[ ${TEAGRAM_TEST_FALLOCATE_FAIL:-0} == 1 ]]; then
  printf 'mock fallocate: No space left on device\n' >&2
  exit 1
fi
printf '%s\n' "$*" >> "$TEAGRAM_TEST_FALLOCATE_LOG"
exec "$TEAGRAM_TEST_REAL_FALLOCATE" "$@"
EOF
chmod 755 "$MOCK_BIN/fallocate"

run_check() {
  local bad_owner_path=${1:-} fail_network_ls=${2:-0} config_file=${3:-}
  local network_ids_file=${4:-} network_inspect_file=${5:-} fail_network_inspect=${6:-0}
  local env_file=${7:-$ENV_FILE}
  local blob_mount=${8:-/srv/teagram-demo-blobs} mounted=${9:-0}
  local read_only=${10:-0}
  PATH="$MOCK_BIN:$PATH" \
    TEAGRAM_TEST_REAL_STAT="$REAL_STAT" \
    TEAGRAM_TEST_REAL_DOCKER="$REAL_DOCKER" \
    TEAGRAM_TEST_REAL_MOUNTPOINT="$REAL_MOUNTPOINT" \
    TEAGRAM_TEST_REAL_FINDMNT="$REAL_FINDMNT" \
    TEAGRAM_TEST_BAD_OWNER_PATH="$bad_owner_path" \
    TEAGRAM_TEST_FAIL_NETWORK_LS="$fail_network_ls" \
    TEAGRAM_TEST_MOUNTED="$mounted" \
    TEAGRAM_TEST_MOUNT_READONLY="$read_only" \
    TEAGRAM_TEST_STATFS_BLOCKS=983040 \
    TEAGRAM_TEST_STATFS_BLOCK_SIZE=4096 \
    TEAGRAM_TEST_STATFS_LOG="$TMP_DIR/statfs.log" \
    TEAGRAM_TEST_CONFIG_FILE="$config_file" \
    TEAGRAM_TEST_NETWORK_IDS_FILE="$network_ids_file" \
    TEAGRAM_TEST_NETWORK_INSPECT_FILE="$network_inspect_file" \
    TEAGRAM_TEST_FAIL_NETWORK_INSPECT="$fail_network_inspect" \
    TEAGRAM_DEMO_LIMA_ENV_FILE="$env_file" \
    TEAGRAM_DEMO_SECRETS_DIR="$SECRETS_DIR" \
    TEAGRAM_DEMO_BLOB_MOUNT="$blob_mount" \
    "$SCRIPT_DIR/check.sh"
}

CHECK_OUT=$TMP_DIR/check.out
assert_config_failure() {
  local description=$1 expected=$2 config_file=$3 status
  if run_check "" 0 "$config_file" > "$CHECK_OUT" 2>&1; then
    status=0
  else
    status=$?
  fi
  if ((status == 1)) && grep -Fq -- "$expected" "$CHECK_OUT" \
    && grep -q '^would_reject' "$CHECK_OUT"; then
    ok "$description"
  else
    not_ok "$description"
    cat "$CHECK_OUT" >&2
  fi
}

# Compose reads the environment itself, so unset the variables that would
# change its file or profile selection.
compose() {
  env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
    -u POSTGRES_PASSWORD -u TG_PUBLIC_LINK_PREFIX -u TG_AUTHKEY_ENC_KEY \
    -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_IMAGE_REF \
    -u TG_RSA_KEY_FINGERPRINT \
    TEAGRAM_DEMO_SECRETS_DIR="$SECRETS_DIR" \
    docker compose --env-file "$ENV_FILE" --project-directory "$REPO_ROOT" "$@"
}

LIMA_JSON=$TMP_DIR/lima.json
compose -f "$LIMA_COMPOSE" --project-name teagram-demo config --format json > "$LIMA_JSON"
CANDIDATE_JSON=$TMP_DIR/lima-candidate.json
compose --profile candidate -f "$LIMA_COMPOSE" --project-name teagram-demo config --format json > "$CANDIDATE_JSON"

TAMPERED_CONFIG_DIR=$TMP_DIR/tampered-config
mkdir -p "$TAMPERED_CONFIG_DIR"
jq '.services.postgres.image = "postgres:18-alpine"' "$CANDIDATE_JSON" > "$TAMPERED_CONFIG_DIR/digest.json"
jq '.services.postgres.build = "."' "$CANDIDATE_JSON" > "$TAMPERED_CONFIG_DIR/build.json"
jq '.services["qa-invite"] = (.services.migrate | .mem_limit = "1")' \
  "$CANDIDATE_JSON" > "$TAMPERED_CONFIG_DIR/qa-invite.json"
jq '.services.teagramd.environment.TG_PG_MAX_CONNS = "8" | .services["teagramd-next"].environment.TG_PG_MAX_CONNS = "8"' \
  "$CANDIDATE_JSON" > "$TAMPERED_CONFIG_DIR/postgres-pool.json"
jq '.services["budget-probe"] = {image: ("fixture@sha256:" + ("a" * 64)), mem_limit: "2097152"}' \
  "$CANDIDATE_JSON" > "$TAMPERED_CONFIG_DIR/memory.json"
jq '.networks.egress.ipam.config[0].subnet = "198.18.134.0/24"' \
  "$CANDIDATE_JSON" > "$TAMPERED_CONFIG_DIR/subnet.json"

# expect DESCRIPTION JQ-FILTER: the filter must evaluate to true on lima's config.
expect() {
  local description=$1 filter=$2
  if jq -e "$filter" "$LIMA_JSON" >/dev/null; then
    ok "$description"
  else
    not_ok "$description"
  fi
}

expect_candidate() {
  local description=$1 filter=$2
  if jq -e "$filter" "$CANDIDATE_JSON" >/dev/null; then
    ok "$description"
  else
    not_ok "$description"
  fi
}

# Criterion 1: no qa-invite service, no build key, every image pinned by digest.
expect 'criterion 1: no qa-invite service' \
  '[.services | keys[] | select(startswith("qa-invite"))] | length == 0'
expect 'criterion 1: no build key anywhere' \
  '[.services[] | select(has("build"))] | length == 0'
expect 'criterion 1: every image is pinned by sha256 digest' \
  '[.services[] | .image | select(test("@sha256:[0-9a-f]{64}$") | not)] | length == 0'

# Criterion 2: every service has a memory limit, and the long-running ones fit
# in 200 MiB. migrate is one-shot, so it is outside that sum.
# shellcheck disable=SC2016 # $n is a jq variable, not a shell expansion.
expect 'criterion 2: every service has a memory limit' \
  '(.services | length) as $n | [.services[] | select((.mem_limit // "0" | tonumber) > 0)] | length == $n'
expect 'criterion 2: long-running limits total at most 200 MiB' \
  '[.services | to_entries[] | select(.key != "migrate") | .value.mem_limit | tonumber] | add <= 209715200'
expect_candidate 'criterion 2: candidate-enabled limits total exactly 199 MiB' \
  '[.services | to_entries[] | select(.key != "migrate") | .value.mem_limit | tonumber] | add == 199 * 1048576'
expect_candidate 'criterion 2: both server pools are capped at three for rollout' \
  '.services.teagramd.environment.TG_PG_MAX_CONNS == "3" and .services["teagramd-next"].environment.TG_PG_MAX_CONNS == "3"'
expect_candidate 'criterion 2: both server slots have 44 MiB for concurrent photo uploads' \
  '(.services.teagramd.mem_limit | tonumber) == 46137344 and (.services["teagramd-next"].mem_limit | tonumber) == 46137344'
expect_candidate 'criterion 2: both server slots use a 20 MiB Go heap limit for memory headroom' \
  '.services.teagramd.environment.GOMEMLIMIT == "20MiB" and .services["teagramd-next"].environment.GOMEMLIMIT == "20MiB"'
expect_candidate 'criterion 2: postgres is capped at 49 MiB after rebalancing' \
  '(.services.postgres.mem_limit | tonumber) == 51380224'
expect 'criterion 2: postgres runs with the lima config file' \
  '.services.postgres.volumes | any(.target == "/etc/postgresql/postgresql.conf" and (.source | endswith("deploy/teagram-demo/lima/postgresql.conf")))'
expect 'criterion 2: only the edge publishes a host port' \
  '[.services | to_entries[] | select(.value.ports != null) | .key] == ["edge"]'

# Criterion 3: teagramd's blobs are a bind of the capped mount, which must
# already exist, and the bind refuses to create a missing host path.
expect 'criterion 3: teagramd binds the capped blob directory' \
  '.services.teagramd.volumes | any(.type == "bind" and .source == "/srv/teagram-demo-blobs/data" and .target == "/var/lib/teagramd-blobs" and .bind.create_host_path == false)'

# Criterion 4: the subnets are fixed and match the documented values.
expect 'criterion 4: fixed subnets are documented values' \
  '.networks.backend.ipam.config[0].subnet == "198.18.130.0/24" and .networks.egress.ipam.config[0].subnet == "198.18.132.0/24" and .networks.edge.ipam.config[0].subnet == "198.18.133.0/24"'

# Postgres sized for its limit: the config file must keep shared_buffers and
# max_connections small enough to fit 49 MiB.
if grep -Eq '^shared_buffers = ([1-8])MB$' "$SCRIPT_DIR/postgresql.conf" \
  && grep -Eq '^max_connections = (2[0-9]|3[0-2])$' "$SCRIPT_DIR/postgresql.conf"; then
  ok 'postgres config: shared_buffers at most 8 MB, max_connections at most 32'
else
  not_ok 'postgres config: shared_buffers at most 8 MB, max_connections at most 32'
fi

# The candidate teagramd-next exists only under its profile and has the raised
# limit needed for concurrent media-photo and profile-photo requests.
if jq -e '.services["teagramd-next"] | .profiles == ["candidate"] and (.mem_limit | tonumber) == 46137344' \
  "$CANDIDATE_JSON" >/dev/null; then
  ok 'candidate teagramd-next is profile-gated and limited'
else
  not_ok 'candidate teagramd-next is profile-gated and limited'
fi

# Every rendered-config guard must reject a deliberately tampered candidate
# config instead of ending with would_apply.
assert_config_failure 'check.sh rejects a tag-only image' 'FAIL image-digests:' "$TAMPERED_CONFIG_DIR/digest.json"
assert_config_failure 'check.sh rejects a build key' 'FAIL config-build:' "$TAMPERED_CONFIG_DIR/build.json"
assert_config_failure 'check.sh rejects qa-invite' 'FAIL config-qa-invite:' "$TAMPERED_CONFIG_DIR/qa-invite.json"
assert_config_failure 'check.sh rejects an uncapped Postgres pool' 'FAIL postgres-pool:' "$TAMPERED_CONFIG_DIR/postgres-pool.json"
assert_config_failure 'check.sh rejects a memory sum over 200 MiB' 'FAIL mem-limits:' "$TAMPERED_CONFIG_DIR/memory.json"
assert_config_failure 'check.sh rejects an unexpected subnet' 'FAIL subnets:' "$TAMPERED_CONFIG_DIR/subnet.json"

# Drift guard: images shared with the charlie file are the same digests, and
# teagramd's environment matches it apart from the lima-specific caps.
ROOT_JSON=$TMP_DIR/root.json
compose -f "$ROOT_COMPOSE" --project-name teagram-demo --profile demo config --format json > "$ROOT_JSON"
for service in postgres migrate turnstile-egress haproxy edge; do
  if [[ "$(jq -r --arg s "$service" '.services[$s].image' "$ROOT_JSON")" \
    == "$(jq -r --arg s "$service" '.services[$s].image' "$LIMA_JSON")" ]]; then
    ok "image parity with charlie: $service"
  else
    not_ok "image parity with charlie: $service"
  fi
done
if diff <(jq -S '.services.teagramd.environment | del(.GOMEMLIMIT, .TG_PG_MAX_CONNS, .TG_RSA_KEY_FINGERPRINT)' "$ROOT_JSON") \
  <(jq -S '.services.teagramd.environment | del(.GOMEMLIMIT, .TG_PG_MAX_CONNS, .TG_RSA_KEY_FINGERPRINT)' "$LIMA_JSON") >/dev/null; then
  ok 'teagramd environment matches the charlie file apart from lima memory and pool caps'
else
  not_ok 'teagramd environment matches the charlie file apart from lima memory and pool caps'
fi
expect_candidate 'criterion 6: both lima server slots pin the imported RSA fingerprint' \
  '.services.teagramd.environment.TG_RSA_KEY_FINGERPRINT == "42" and .services["teagramd-next"].environment.TG_RSA_KEY_FINGERPRINT == "42"'

# The image helper rejects unknown existing images and blkid errors without
# calling mkfs, accepts a recognized ext4 image unchanged, and formats only an
# image created by this invocation.
# shellcheck source=deploy/teagram-demo/lima/prepare-blobs.sh
# shellcheck disable=SC1091
source "$SCRIPT_DIR/prepare-blobs.sh"
TEAGRAM_TEST_MKFS_LOG=$TMP_DIR/mkfs.log
TEAGRAM_TEST_FALLOCATE_LOG=$TMP_DIR/fallocate.log
PREPARE_ERR=$TMP_DIR/prepare.err
export TEAGRAM_TEST_MKFS_LOG TEAGRAM_TEST_FALLOCATE_LOG
: > "$TEAGRAM_TEST_MKFS_LOG"
: > "$TEAGRAM_TEST_FALLOCATE_LOG"
FSTAB_FIXTURE=$TMP_DIR/fstab
: > "$FSTAB_FIXTURE"
if ensure_fstab_entry "$FSTAB_FIXTURE" "$TMP_DIR/blobs.img" "$TMP_DIR/blobs" >/dev/null \
  && grep -Fqx "$TMP_DIR/blobs.img $TMP_DIR/blobs ext4 loop,nodev,nosuid,noexec,nofail 0 0" "$FSTAB_FIXTURE"; then
  ok 'prepare-blobs writes a nofail fstab entry'
else
  not_ok 'prepare-blobs writes a nofail fstab entry'
fi
printf '%s %s ext4 loop,nodev,nosuid,noexec 0 0\n' "$TMP_DIR/legacy.img" "$TMP_DIR/legacy" > "$FSTAB_FIXTURE"
if ensure_fstab_entry "$FSTAB_FIXTURE" "$TMP_DIR/legacy.img" "$TMP_DIR/legacy" >/dev/null \
  && [[ $(wc -l < "$FSTAB_FIXTURE") == 1 ]] \
  && grep -Fqx "$TMP_DIR/legacy.img $TMP_DIR/legacy ext4 loop,nodev,nosuid,noexec,nofail 0 0" "$FSTAB_FIXTURE"; then
  ok 'prepare-blobs upgrades an existing fstab entry with nofail'
else
  not_ok 'prepare-blobs upgrades an existing fstab entry with nofail'
fi
if PATH="$MOCK_BIN:$PATH" TEAGRAM_TEST_BLKID_TYPE=ext4 TEAGRAM_TEST_BLKID_STATUS=0 \
  prepare_blob_filesystem "$TMP_DIR/existing-ext4.img" 0 >/dev/null; then
  ok 'prepare-blobs accepts an identified ext4 image'
else
  not_ok 'prepare-blobs accepts an identified ext4 image'
fi
if PATH="$MOCK_BIN:$PATH" TEAGRAM_TEST_BLKID_TYPE='' TEAGRAM_TEST_BLKID_STATUS=2 \
  prepare_blob_filesystem "$TMP_DIR/unknown.img" 0 >/dev/null 2>"$PREPARE_ERR"; then
  not_ok 'prepare-blobs rejects an unknown existing image'
elif grep -Fq 'could not identify existing' "$PREPARE_ERR" && grep -Fq 'refusing to format' "$PREPARE_ERR"; then
  ok 'prepare-blobs rejects an unknown existing image with a diagnostic'
else
  not_ok 'prepare-blobs diagnoses an unknown existing image'
  cat "$PREPARE_ERR" >&2
fi
if PATH="$MOCK_BIN:$PATH" TEAGRAM_TEST_BLKID_TYPE=ext4 TEAGRAM_TEST_BLKID_STATUS=8 \
  prepare_blob_filesystem "$TMP_DIR/blkid-error.img" 0 >/dev/null 2>"$PREPARE_ERR"; then
  not_ok 'prepare-blobs rejects blkid errors'
elif grep -Fq 'could not identify existing' "$PREPARE_ERR" && grep -Fq 'refusing to format' "$PREPARE_ERR"; then
  ok 'prepare-blobs rejects blkid errors with a diagnostic'
else
  not_ok 'prepare-blobs diagnoses blkid errors'
  cat "$PREPARE_ERR" >&2
fi
if [[ ! -s $TEAGRAM_TEST_MKFS_LOG ]]; then
  ok 'prepare-blobs does not format an existing image'
else
  not_ok 'prepare-blobs does not format an existing image'
fi
GNU_COREUTILS_BIN=$TMP_DIR/mock-gnu-coreutils
mkdir -p "$GNU_COREUTILS_BIN"
for tool in chown chmod stat; do
  cat > "$GNU_COREUTILS_BIN/$tool" <<'EOF'
#!/usr/bin/env bash
printf '%s (GNU coreutils) 9.1\n' "${0##*/}"
EOF
  chmod 755 "$GNU_COREUTILS_BIN/$tool"
done
if PATH="$GNU_COREUTILS_BIN:$PATH" require_gnu_coreutils >/dev/null 2>"$PREPARE_ERR"; then
  ok 'prepare-blobs accepts GNU coreutils'
else
  not_ok 'prepare-blobs accepts GNU coreutils'
  cat "$PREPARE_ERR" >&2
fi
cat > "$MOCK_BIN/chown" <<'EOF'
#!/usr/bin/env bash
printf 'chown (BusyBox) 1.36.1\n'
EOF
chmod 755 "$MOCK_BIN/chown"
if PATH="$MOCK_BIN:$PATH" require_gnu_coreutils >/dev/null 2>"$PREPARE_ERR"; then
  not_ok 'prepare-blobs rejects BusyBox chown before setup'
elif grep -Fq 'GNU coreutils chown is required' "$PREPARE_ERR"; then
  ok 'prepare-blobs rejects BusyBox chown before setup'
else
  not_ok 'prepare-blobs diagnoses an incompatible chown'
  cat "$PREPARE_ERR" >&2
fi
rm -f "$MOCK_BIN/chown"

LOW_SPACE_IMAGE=$TMP_DIR/low-space.img
create_blob_image "$LOW_SPACE_IMAGE"
LOW_SPACE_AVAILABLE=$((IMAGE_BYTES + MIN_HOST_FREE_BYTES - 1))
if PATH="$MOCK_BIN:$PATH" TEAGRAM_TEST_AVAILABLE_BYTES="$LOW_SPACE_AVAILABLE" \
  reserve_blob_space "$LOW_SPACE_IMAGE" "$IMAGE_BYTES" 1 >/dev/null 2>"$PREPARE_ERR"; then
  not_ok 'prepare-blobs rejects a reservation that would leave less than 100 MiB free'
elif grep -Fq 'insufficient host disk space' "$PREPARE_ERR" \
  && [[ ! -e $LOW_SPACE_IMAGE ]] \
  && [[ ! -s $TEAGRAM_TEST_FALLOCATE_LOG ]]; then
  ok 'prepare-blobs rejects insufficient host space before allocating the image'
else
  not_ok 'prepare-blobs diagnoses insufficient host space before allocating the image'
  cat "$PREPARE_ERR" >&2
fi

ALLOCATED_IMAGE=$TMP_DIR/allocated.img
create_blob_image "$ALLOCATED_IMAGE"
if PATH="$MOCK_BIN:$PATH" TEAGRAM_TEST_REAL_FALLOCATE="$REAL_FALLOCATE" \
  TEAGRAM_TEST_AVAILABLE_BYTES=1073741824 \
  reserve_blob_space "$ALLOCATED_IMAGE" 4096 1 >/dev/null; then
  if [[ $(stat -c '%s' -- "$ALLOCATED_IMAGE") == 4096 ]] \
    && grep -Fq -- '-l 4096 --' "$TEAGRAM_TEST_FALLOCATE_LOG"; then
    ok 'prepare-blobs reserves backing blocks before use'
  else
    not_ok 'prepare-blobs reserves backing blocks before use'
  fi
else
  not_ok 'prepare-blobs reserves backing blocks before use'
fi

SPARSE_IMAGE=$TMP_DIR/existing-sparse.img
truncate -s 4096 "$SPARSE_IMAGE"
if PATH="$MOCK_BIN:$PATH" TEAGRAM_TEST_REAL_FALLOCATE="$REAL_FALLOCATE" \
  TEAGRAM_TEST_AVAILABLE_BYTES=1073741824 \
  reserve_blob_space "$SPARSE_IMAGE" 4096 0 >/dev/null; then
  if [[ $(stat -c '%s' -- "$SPARSE_IMAGE") == 4096 ]] \
    && [[ $(stat -c '%b' -- "$SPARSE_IMAGE") -gt 0 ]] \
    && grep -Fq -- '-l 4096 --' "$TEAGRAM_TEST_FALLOCATE_LOG"; then
    ok 'prepare-blobs reserves holes in an existing sparse image'
  else
    not_ok 'prepare-blobs reserves holes in an existing sparse image'
  fi
else
  not_ok 'prepare-blobs reserves holes in an existing sparse image'
fi

PARTIAL_IMAGE=$TMP_DIR/partial.img
create_blob_image "$PARTIAL_IMAGE"
if PATH="$MOCK_BIN:$PATH" TEAGRAM_TEST_REAL_FALLOCATE="$REAL_FALLOCATE" \
  TEAGRAM_TEST_AVAILABLE_BYTES=1073741824 TEAGRAM_TEST_FALLOCATE_FAIL=1 \
  reserve_blob_space "$PARTIAL_IMAGE" 4096 1 >/dev/null 2>"$PREPARE_ERR"; then
  not_ok 'prepare-blobs rejects a partial image after fallocate failure'
elif grep -Fq "$PARTIAL_IMAGE" "$PREPARE_ERR" \
  && grep -Fq 'No space left on device' "$PREPARE_ERR" \
  && grep -Fq 'remove it before rerunning' "$PREPARE_ERR"; then
  ok 'prepare-blobs diagnoses and identifies a partial image after fallocate failure'
else
  not_ok 'prepare-blobs diagnoses allocation failure and partial image recovery'
  cat "$PREPARE_ERR" >&2
fi
if PATH="$MOCK_BIN:$PATH" TEAGRAM_TEST_BLKID_TYPE='' TEAGRAM_TEST_BLKID_STATUS=2 \
  prepare_blob_filesystem "$TMP_DIR/created-here.img" 1 >/dev/null; then
  if [[ $(wc -l < "$TEAGRAM_TEST_MKFS_LOG") == 1 ]] \
    && grep -Fq -- '-E nodiscard' "$TEAGRAM_TEST_MKFS_LOG"; then
    ok 'prepare-blobs formats new images without discarding reserved blocks'
  else
    not_ok 'prepare-blobs formats new images without discarding reserved blocks'
  fi
else
  not_ok 'prepare-blobs formats new images without discarding reserved blocks'
fi
if PATH="$MOCK_BIN:$PATH" TEAGRAM_TEST_MKFS_FAIL=1 TEAGRAM_TEST_BLKID_TYPE='' TEAGRAM_TEST_BLKID_STATUS=2 \
  prepare_blob_filesystem "$TMP_DIR/partial-format.img" 1 >/dev/null 2>"$PREPARE_ERR"; then
  not_ok 'prepare-blobs rejects mkfs failure'
elif grep -Fq "$TMP_DIR/partial-format.img" "$PREPARE_ERR" \
  && grep -Fq 'remove it before rerunning' "$PREPARE_ERR"; then
  ok 'prepare-blobs diagnoses and identifies a partial image after mkfs failure'
else
  not_ok 'prepare-blobs diagnoses mkfs failure and partial image recovery'
  cat "$PREPARE_ERR" >&2
fi

# A real ext4 format must keep fallocate's backing blocks allocated. The test
# exercises the production formatter against a small file-backed filesystem.
REAL_FORMAT_BYTES=$((64 * 1024 * 1024))
REAL_FORMAT_IMAGE=$TMP_DIR/real-formatted.img
REAL_MKFS_BIN=$TMP_DIR/real-mkfs-bin
mkdir -p "$REAL_MKFS_BIN"
cat > "$REAL_MKFS_BIN/mkfs.ext4" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
exec "$TEAGRAM_TEST_REAL_MKFS_EXT4" "$@"
EOF
chmod 755 "$REAL_MKFS_BIN/mkfs.ext4"
"$REAL_FALLOCATE" -l "$REAL_FORMAT_BYTES" -- "$REAL_FORMAT_IMAGE"
if PATH="$REAL_MKFS_BIN:$PATH" TEAGRAM_TEST_REAL_MKFS_EXT4="$REAL_MKFS_EXT4" \
  prepare_blob_filesystem "$REAL_FORMAT_IMAGE" 1 >/dev/null; then
  read -r formatted_blocks formatted_block_size < <(stat -c '%b %B' -- "$REAL_FORMAT_IMAGE")
  if ((formatted_blocks * formatted_block_size >= REAL_FORMAT_BYTES)); then
    ok 'prepare-blobs retains allocated blocks after real ext4 formatting'
  else
    not_ok 'prepare-blobs retains allocated blocks after real ext4 formatting'
  fi
else
  not_ok 'prepare-blobs formats a real ext4 image for the allocation regression'
fi

# A failed reservation on an existing ext4 image must preserve its contents
# and tell the operator to free host space and rerun, never to delete the image.
REAL_FORMAT_SHA_BEFORE=$(sha256sum "$REAL_FORMAT_IMAGE")
if PATH="$MOCK_BIN:$PATH" TEAGRAM_TEST_AVAILABLE_BYTES=1073741824 TEAGRAM_TEST_FALLOCATE_FAIL=1 \
  reserve_blob_space "$REAL_FORMAT_IMAGE" "$REAL_FORMAT_BYTES" 0 >/dev/null 2>"$PREPARE_ERR"; then
  not_ok 'prepare-blobs preserves an existing image when reservation fails'
elif grep -Fq "$REAL_FORMAT_IMAGE" "$PREPARE_ERR" \
  && grep -Fq 'existing image data is preserved' "$PREPARE_ERR" \
  && grep -Fq 'free host disk space and rerun' "$PREPARE_ERR" \
  && grep -Fq 'do not remove it' "$PREPARE_ERR" \
  && ! grep -Fq 'remove it before rerunning' "$PREPARE_ERR" \
  && [[ -f $REAL_FORMAT_IMAGE ]] \
  && [[ $(sha256sum "$REAL_FORMAT_IMAGE") == "$REAL_FORMAT_SHA_BEFORE" ]]; then
  ok 'prepare-blobs preserves an existing image and explains retry after reservation failure'
else
  not_ok 'prepare-blobs gives safe guidance after existing-image reservation failure'
  cat "$PREPARE_ERR" >&2
fi

# Criterion 6 and the check: config, network discovery and filesystem state are
# fixture-backed. The unmounted case must reject; the mounted case uses stat -f
# values from the fixture. The output must not leak values.
CHECK_OUT=$TMP_DIR/check.out
STATFS_LOG=$TMP_DIR/statfs.log
: > "$STATFS_LOG"
set +e
run_check > "$CHECK_OUT" 2>&1
check_status=$?
set -e
if grep -qxF 'FAIL blob-cap: /srv/teagram-demo-blobs is not a mount point' "$CHECK_OUT" \
  && ((check_status == 1)) \
  && grep -qx 'would_reject (1 failing)' "$CHECK_OUT" \
  && [[ $(grep -c '^FAIL ' "$CHECK_OUT") == 1 ]]; then
  ok 'check.sh rejects an unmounted blob cap'
else
  not_ok 'check.sh rejects an unmounted blob cap'
  cat "$CHECK_OUT" >&2
fi
for name in env-file secret-files tunnel-token compose-config config-build config-qa-invite postgres-pool image-digests mem-limits subnets; do
  if grep -q "^PASS $name" "$CHECK_OUT"; then
    ok "check.sh passes $name on the fixture"
  else
    not_ok "check.sh passes $name on the fixture"
  fi
done
if grep -q '^PASS mem-limits .*with candidate enabled)$' "$CHECK_OUT"; then
  ok 'check.sh validates the memory ceiling with the candidate enabled'
else
  not_ok 'check.sh validates the memory ceiling with the candidate enabled'
fi
if grep -qF -- "$PASSWORD_FIXTURE" "$CHECK_OUT" || grep -qF -- "fixture-tunnel-token" "$CHECK_OUT"; then
  not_ok 'check.sh prints no secret values'
else
  ok 'check.sh prints no secret values'
fi

BLOB_MOUNT_FIXTURE=$TMP_DIR/blob-mount
mkdir -p "$BLOB_MOUNT_FIXTURE/data"
chmod 700 "$BLOB_MOUNT_FIXTURE/data"
BLOB_MOUNT_CONFIG=$TMP_DIR/blob-mount-config.json
jq --arg src "$BLOB_MOUNT_FIXTURE/data" \
  '.services.teagramd.volumes |= map(if .type == "bind" and .target == "/var/lib/teagramd-blobs" then .source = $src else . end)' \
  "$CANDIDATE_JSON" > "$BLOB_MOUNT_CONFIG"
: > "$STATFS_LOG"
set +e
run_check "" 0 "$BLOB_MOUNT_CONFIG" "" "" 0 "$ENV_FILE" "$BLOB_MOUNT_FIXTURE" 1 > "$CHECK_OUT" 2>&1
check_status=$?
set -e
if ((check_status == 0)) \
  && grep -qx 'PASS blob-cap' "$CHECK_OUT" \
  && grep -qx 'would_apply' "$CHECK_OUT" \
  && grep -qx "$BLOB_MOUNT_FIXTURE 983040 4096" "$STATFS_LOG"; then
  ok 'check.sh accepts a mounted blob cap using the stat -f fixture'
else
  not_ok 'check.sh accepts a mounted blob cap using the stat -f fixture'
  cat "$CHECK_OUT" >&2
  cat "$STATFS_LOG" >&2
fi

: > "$STATFS_LOG"
set +e
run_check "" 0 "$BLOB_MOUNT_CONFIG" "" "" 0 "$ENV_FILE" "$BLOB_MOUNT_FIXTURE" 1 1 \
  > "$CHECK_OUT" 2>&1
check_status=$?
set -e
if ((check_status == 1)) \
  && grep -qx "FAIL blob-cap: $BLOB_MOUNT_FIXTURE is mounted read-only" "$CHECK_OUT" \
  && grep -qx "$BLOB_MOUNT_FIXTURE 983040 4096" "$STATFS_LOG" \
  && grep -q '^would_reject' "$CHECK_OUT"; then
  ok 'check.sh rejects a read-only mounted blob filesystem'
else
  not_ok 'check.sh rejects a read-only mounted blob filesystem'
  cat "$CHECK_OUT" >&2
  cat "$STATFS_LOG" >&2
fi

# Both documented Turnstile modes must let runtime gid 65532 read the mounted
# files. Owner-only or unreadable modes are rejected by the preflight.
for mode in 400 0; do
  chmod "$mode" "$SECRETS_DIR/turnstile-secret-key"
  set +e
  run_check > "$CHECK_OUT" 2>&1
  check_status=$?
  set -e
  if ((check_status == 1)) && grep -q '^FAIL secrets: wrong ownership or mode on:.*turnstile-secret-key' "$CHECK_OUT"; then
    ok "check.sh rejects unreadable Turnstile mode $mode"
  else
    not_ok "check.sh rejects unreadable Turnstile mode $mode"
    cat "$CHECK_OUT" >&2
  fi
done
chmod 440 "$SECRETS_DIR/turnstile-secret-key"

# An existing but empty Turnstile key is not a usable secret.
chmod 600 "$SECRETS_DIR/turnstile-secret-key"
: > "$SECRETS_DIR/turnstile-secret-key"
set +e
run_check > "$CHECK_OUT" 2>&1
check_status=$?
set -e
printf 'fixture\n' > "$SECRETS_DIR/turnstile-secret-key"
chmod 440 "$SECRETS_DIR/turnstile-secret-key"
if ((check_status == 1)) && grep -q '^FAIL secrets: empty: turnstile-secret-key$' "$CHECK_OUT" \
  && grep -q '^would_reject' "$CHECK_OUT"; then
  ok 'check.sh rejects an empty Turnstile secret'
else
  not_ok 'check.sh rejects an empty Turnstile secret'
  cat "$CHECK_OUT" >&2
fi

# Root-only paths with otherwise-correct modes still fail when owned by a
# non-root account.
for path in "$ENV_FILE" "$SECRETS_DIR" "$SECRETS_DIR/turnstile-secret-key" "$SECRETS_DIR/cloudflared.env"; do
  set +e
  run_check "$path" > "$CHECK_OUT" 2>&1
  check_status=$?
  set -e
  if ((check_status == 1)) && grep -q '^FAIL env-file:\|^FAIL secrets:' "$CHECK_OUT"; then
    ok "check.sh rejects non-root ownership for $path"
  else
    not_ok "check.sh rejects non-root ownership for $path"
    cat "$CHECK_OUT" >&2
  fi
done

# docker network ls errors must not look like an empty, overlap-free network set,
# and the operator gets the first diagnostic line.
set +e
run_check "" 1 > "$CHECK_OUT" 2>&1
check_status=$?
set -e
if ((check_status == 1)) \
  && grep -q '^FAIL subnets-host: docker network ls failed: mock network-list permission denied$' "$CHECK_OUT" \
  && ! grep -q '^PASS subnets-host$' "$CHECK_OUT"; then
  ok 'check.sh rejects docker network ls failures with a diagnostic'
else
  not_ok 'check.sh rejects docker network ls failures with a diagnostic'
  cat "$CHECK_OUT" >&2
fi

# The overlap check inspects existing networks, but ignores the demo's own
# networks so an earlier deployment does not block a preflight rerun.
NETWORK_IDS_FILE=$TMP_DIR/network-ids
NETWORK_INSPECT_FILE=$TMP_DIR/network-inspect.json
printf 'fixture-network-id\n' > "$NETWORK_IDS_FILE"
cat > "$NETWORK_INSPECT_FILE" <<'EOF'
[{"Name":"other-stack_default","IPAM":{"Config":[{"Subnet":"198.18.132.0/24"}]}}]
EOF
set +e
run_check "" 0 "" "$NETWORK_IDS_FILE" "$NETWORK_INSPECT_FILE" > "$CHECK_OUT" 2>&1
check_status=$?
set -e
if ((check_status == 1)) \
  && grep -q '^FAIL subnets-host: overlaps existing network: 198\.18\.132\.0/24' "$CHECK_OUT" \
  && grep -q '^would_reject' "$CHECK_OUT"; then
  ok 'check.sh rejects an overlapping network from another stack'
else
  not_ok 'check.sh rejects an overlapping network from another stack'
  cat "$CHECK_OUT" >&2
fi

cat > "$NETWORK_INSPECT_FILE" <<'EOF'
[{"Name":"teagram-demo_egress","IPAM":{"Config":[{"Subnet":"198.18.132.0/24"}]}}]
EOF
set +e
run_check "" 0 "" "$NETWORK_IDS_FILE" "$NETWORK_INSPECT_FILE" > "$CHECK_OUT" 2>&1
check_status=$?
set -e
if grep -q '^PASS subnets-host$' "$CHECK_OUT" && ! grep -q '^FAIL subnets-host:' "$CHECK_OUT"; then
  ok 'check.sh ignores an overlapping teagram-demo network'
else
  not_ok 'check.sh ignores an overlapping teagram-demo network'
  cat "$CHECK_OUT" >&2
fi

set +e
run_check "" 0 "" "$NETWORK_IDS_FILE" "" 1 > "$CHECK_OUT" 2>&1
check_status=$?
set -e
if ((check_status == 1)) \
  && grep -q '^FAIL subnets-host: docker network inspect failed: mock network-inspect permission denied$' "$CHECK_OUT" \
  && grep -q '^would_reject' "$CHECK_OUT"; then
  ok 'check.sh reports docker network inspect diagnostics'
else
  not_ok 'check.sh reports docker network inspect diagnostics'
  cat "$CHECK_OUT" >&2
fi

# Missing env keys are reported even when Compose's :? guard also rejects the
# render. The suggested command uses placeholders and does not expose secrets.
MISSING_ENV_FILE=$TMP_DIR/missing-postgres-password.env
grep -v '^POSTGRES_PASSWORD=' "$ENV_FILE" > "$MISSING_ENV_FILE"
chmod 600 "$MISSING_ENV_FILE"
set +e
run_check "" 0 "" "" "" 0 "$MISSING_ENV_FILE" > "$CHECK_OUT" 2>&1
check_status=$?
set -e
if ((check_status == 1)) \
  && grep -q '^FAIL env-file: missing keys: POSTGRES_PASSWORD$' "$CHECK_OUT" \
  && grep -q '^FAIL compose-config: render failed;' "$CHECK_OUT" \
  && grep -q '^compose-config diagnostic: Docker Compose config failed; details withheld' "$CHECK_OUT" \
  && grep -q 'command: docker compose --env-file <lima.env> --project-directory <checkout>' "$CHECK_OUT" \
  && grep -q '^would_reject' "$CHECK_OUT"; then
  ok 'check.sh reports a missing env key and Compose diagnostic'
else
  not_ok 'check.sh reports a missing env key and Compose diagnostic'
  cat "$CHECK_OUT" >&2
fi

# The effective env_file value can be empty even when the file contains a
# TUNNEL_TOKEN assignment, so inspect Compose's resolved environment.
printf 'TUNNEL_TOKEN=""\n' > "$SECRETS_DIR/cloudflared.env"
chmod 600 "$SECRETS_DIR/cloudflared.env"
set +e
run_check > "$CHECK_OUT" 2>&1
check_status=$?
set -e
printf 'TUNNEL_TOKEN=fixture-tunnel-token\n' > "$SECRETS_DIR/cloudflared.env"
chmod 600 "$SECRETS_DIR/cloudflared.env"
if ((check_status == 1)) \
  && grep -q '^FAIL tunnel-token: cloudflared.env:TUNNEL_TOKEN is missing or empty$' "$CHECK_OUT" \
  && grep -q '^would_reject' "$CHECK_OUT"; then
  ok 'check.sh rejects an empty effective tunnel token'
else
  not_ok 'check.sh rejects an empty effective tunnel token'
  cat "$CHECK_OUT" >&2
fi

# Compose dotenv parse errors can echo the offending value. Verify the
# preflight withholds the parser text from both stdout and stderr.
CHECK_ERR=$TMP_DIR/check.err
MALFORMED_DB_SECRET=fixture-malformed-db-secret-must-not-leak
MALFORMED_DB_ENV=$TMP_DIR/malformed-db.env
sed "s/^POSTGRES_PASSWORD=.*/POSTGRES_PASSWORD=\"$MALFORMED_DB_SECRET/" "$ENV_FILE" > "$MALFORMED_DB_ENV"
chmod 600 "$MALFORMED_DB_ENV"
set +e
run_check "" 0 "" "" "" 0 "$MALFORMED_DB_ENV" > "$CHECK_OUT" 2> "$CHECK_ERR"
check_status=$?
set -e
if ((check_status == 1)) \
  && grep -q '^FAIL compose-config: render failed;' "$CHECK_OUT" \
  && grep -q '^compose-config diagnostic: Docker Compose config failed; details withheld' "$CHECK_ERR" \
  && ! grep -Fq "$MALFORMED_DB_SECRET" "$CHECK_OUT" \
  && ! grep -Fq "$MALFORMED_DB_SECRET" "$CHECK_ERR"; then
  ok 'check.sh withholds malformed database dotenv values from both streams'
else
  not_ok 'check.sh withholds malformed database dotenv values from both streams'
fi

MALFORMED_TUNNEL_SECRET=fixture-malformed-tunnel-secret-must-not-leak
cp "$SECRETS_DIR/cloudflared.env" "$TMP_DIR/cloudflared.env.saved"
printf 'TUNNEL_TOKEN="%s\n' "$MALFORMED_TUNNEL_SECRET" > "$SECRETS_DIR/cloudflared.env"
chmod 600 "$SECRETS_DIR/cloudflared.env"
set +e
run_check > "$CHECK_OUT" 2> "$CHECK_ERR"
check_status=$?
set -e
mv "$TMP_DIR/cloudflared.env.saved" "$SECRETS_DIR/cloudflared.env"
if ((check_status == 1)) \
  && grep -q '^FAIL compose-config: render failed;' "$CHECK_OUT" \
  && grep -q '^compose-config diagnostic: Docker Compose config failed; details withheld' "$CHECK_ERR" \
  && ! grep -Fq "$MALFORMED_TUNNEL_SECRET" "$CHECK_OUT" \
  && ! grep -Fq "$MALFORMED_TUNNEL_SECRET" "$CHECK_ERR"; then
  ok 'check.sh withholds malformed tunnel dotenv values from both streams'
else
  not_ok 'check.sh withholds malformed tunnel dotenv values from both streams'
fi

# A missing tunnel token is named by key, and the check rejects.
mv "$SECRETS_DIR/cloudflared.env" "$TMP_DIR/cloudflared.env.moved"
set +e
run_check > "$CHECK_OUT" 2>&1
check_status=$?
set -e
mv "$TMP_DIR/cloudflared.env.moved" "$SECRETS_DIR/cloudflared.env"
if ((check_status == 1)) && grep -q '^FAIL secrets: missing: cloudflared.env' "$CHECK_OUT"; then
  ok 'check.sh names a missing tunnel token file and rejects'
else
  not_ok 'check.sh names a missing tunnel token file and rejects'
  cat "$CHECK_OUT" >&2
fi

if ((failures > 0)); then
  printf '%d lima compose check(s) failed\n' "$failures" >&2
  exit 1
fi
printf 'lima compose contract holds\n'
