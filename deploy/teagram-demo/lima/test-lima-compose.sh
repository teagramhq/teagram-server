#!/usr/bin/env bash
# Resolves the lima Compose stack against fixture values and asserts the lima
# acceptance criteria: no qa-invite and no build keys, digest-pinned images,
# memory limits within the 200 MiB budget, the blob bind, fixed subnets, and
# secrets named by check.sh. It also checks that lima's teagramd environment
# matches the charlie file except for GOMEMLIMIT, and that check.sh never
# prints a secret.
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
mkdir -p "$SECRETS_DIR"
mkdir -p "$MOCK_BIN"
chmod 700 "$SECRETS_DIR"
cat > "$ENV_FILE" <<EOF
POSTGRES_PASSWORD=$PASSWORD_FIXTURE
TG_PUBLIC_LINK_PREFIX=https://demo.teagram.io/
TG_AUTHKEY_ENC_KEY=$DIGEST
TEAGRAM_DEMO_TURNSTILE_SITE_KEY=fixture-site-key
TEAGRAM_DEMO_IMAGE_REF=teagramd@sha256:$DIGEST
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

cat > "$MOCK_BIN/docker" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
if [[ ${TEAGRAM_TEST_FAIL_NETWORK_LS:-0} == 1 && ${1:-} == network && ${2:-} == ls && ${3:-} == -q ]]; then
  exit 42
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
printf 'format\n' >> "$TEAGRAM_TEST_MKFS_LOG"
EOF
chmod 755 "$MOCK_BIN/mkfs.ext4"

run_check() {
  local bad_owner_path=${1:-} fail_network_ls=${2:-0}
  PATH="$MOCK_BIN:$PATH" \
    TEAGRAM_TEST_REAL_STAT="$REAL_STAT" \
    TEAGRAM_TEST_REAL_DOCKER="$REAL_DOCKER" \
    TEAGRAM_TEST_BAD_OWNER_PATH="$bad_owner_path" \
    TEAGRAM_TEST_FAIL_NETWORK_LS="$fail_network_ls" \
    TEAGRAM_DEMO_LIMA_ENV_FILE="$ENV_FILE" \
    TEAGRAM_DEMO_SECRETS_DIR="$SECRETS_DIR" \
    "$SCRIPT_DIR/check.sh"
}

# Compose reads the environment itself, so unset the variables that would
# change its file or profile selection.
compose() {
  env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
    -u POSTGRES_PASSWORD -u TG_PUBLIC_LINK_PREFIX -u TG_AUTHKEY_ENC_KEY \
    -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_IMAGE_REF \
    TEAGRAM_DEMO_SECRETS_DIR="$SECRETS_DIR" \
    docker compose --env-file "$ENV_FILE" --project-directory "$REPO_ROOT" "$@"
}

LIMA_JSON=$TMP_DIR/lima.json
compose -f "$LIMA_COMPOSE" --project-name teagram-demo config --format json > "$LIMA_JSON"
CANDIDATE_JSON=$TMP_DIR/lima-candidate.json
compose --profile candidate -f "$LIMA_COMPOSE" --project-name teagram-demo config --format json > "$CANDIDATE_JSON"

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
  '[.services[] | select(.restart != null) | .mem_limit | tonumber] | add <= 209715200'
expect_candidate 'criterion 2: candidate-enabled limits total exactly 199 MiB' \
  '[.services[] | select(.restart != null) | .mem_limit | tonumber] | add == 199 * 1048576'
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
# max_connections small enough to fit 56 MiB.
if grep -Eq '^shared_buffers = (1[0-6]|[1-9])MB$' "$SCRIPT_DIR/postgresql.conf" \
  && grep -Eq '^max_connections = (2[0-9]|3[0-2])$' "$SCRIPT_DIR/postgresql.conf"; then
  ok 'postgres config: shared_buffers at most 16 MB, max_connections at most 32'
else
  not_ok 'postgres config: shared_buffers at most 16 MB, max_connections at most 32'
fi

# The candidate teagramd-next exists only under its profile and has a limit.
if jq -e '.services["teagramd-next"] | .profiles == ["candidate"] and (.mem_limit | tonumber) == 41943040' \
  "$CANDIDATE_JSON" >/dev/null; then
  ok 'candidate teagramd-next is profile-gated and limited'
else
  not_ok 'candidate teagramd-next is profile-gated and limited'
fi

# Drift guard: images shared with the charlie file are the same digests, and
# teagramd's environment matches it apart from GOMEMLIMIT.
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
if diff <(jq -S '.services.teagramd.environment | del(.GOMEMLIMIT)' "$ROOT_JSON") \
  <(jq -S '.services.teagramd.environment | del(.GOMEMLIMIT)' "$LIMA_JSON") >/dev/null; then
  ok 'teagramd environment matches the charlie file apart from GOMEMLIMIT'
else
  not_ok 'teagramd environment matches the charlie file apart from GOMEMLIMIT'
fi

# The image helper rejects unknown existing images and blkid errors without
# calling mkfs, accepts a recognized ext4 image unchanged, and formats only an
# image created by this invocation.
# shellcheck source=deploy/teagram-demo/lima/prepare-blobs.sh
source "$SCRIPT_DIR/prepare-blobs.sh"
TEAGRAM_TEST_MKFS_LOG=$TMP_DIR/mkfs.log
export TEAGRAM_TEST_MKFS_LOG
: > "$TEAGRAM_TEST_MKFS_LOG"
if PATH="$MOCK_BIN:$PATH" TEAGRAM_TEST_BLKID_TYPE=ext4 TEAGRAM_TEST_BLKID_STATUS=0 \
  prepare_blob_filesystem "$TMP_DIR/existing-ext4.img" 0 >/dev/null; then
  ok 'prepare-blobs accepts an identified ext4 image'
else
  not_ok 'prepare-blobs accepts an identified ext4 image'
fi
if PATH="$MOCK_BIN:$PATH" TEAGRAM_TEST_BLKID_TYPE='' TEAGRAM_TEST_BLKID_STATUS=2 \
  prepare_blob_filesystem "$TMP_DIR/unknown.img" 0 >/dev/null 2>&1; then
  not_ok 'prepare-blobs rejects an unknown existing image'
else
  ok 'prepare-blobs rejects an unknown existing image'
fi
if PATH="$MOCK_BIN:$PATH" TEAGRAM_TEST_BLKID_TYPE=ext4 TEAGRAM_TEST_BLKID_STATUS=8 \
  prepare_blob_filesystem "$TMP_DIR/blkid-error.img" 0 >/dev/null 2>&1; then
  not_ok 'prepare-blobs rejects blkid errors'
else
  ok 'prepare-blobs rejects blkid errors'
fi
if [[ ! -s $TEAGRAM_TEST_MKFS_LOG ]]; then
  ok 'prepare-blobs does not format an existing image'
else
  not_ok 'prepare-blobs does not format an existing image'
fi
if PATH="$MOCK_BIN:$PATH" TEAGRAM_TEST_BLKID_TYPE='' TEAGRAM_TEST_BLKID_STATUS=2 \
  prepare_blob_filesystem "$TMP_DIR/created-here.img" 1 >/dev/null; then
  if [[ $(wc -l < "$TEAGRAM_TEST_MKFS_LOG") == 1 ]]; then
    ok 'prepare-blobs formats an image created in this invocation'
  else
    not_ok 'prepare-blobs formats an image created in this invocation'
  fi
else
  not_ok 'prepare-blobs formats an image created in this invocation'
fi

# Criterion 6 and the check: a complete fixture passes everything except the
# blob cap, which only the host can provide. The output must not leak values.
CHECK_OUT=$TMP_DIR/check.out
set +e
run_check > "$CHECK_OUT" 2>&1
check_status=$?
set -e
if ((check_status == 1)) && grep -qx 'would_reject (1 failing)' "$CHECK_OUT" \
  && grep -q '^FAIL blob-cap:' "$CHECK_OUT" \
  && [[ $(grep -c '^FAIL ' "$CHECK_OUT") == 1 ]]; then
  ok 'check.sh rejects only on the blob cap for a complete fixture'
else
  not_ok 'check.sh rejects only on the blob cap for a complete fixture'
  cat "$CHECK_OUT" >&2
fi
for name in env-file secrets compose-config config-build config-qa-invite image-digests mem-limits subnets; do
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

# docker network ls errors must not look like an empty, overlap-free network set.
set +e
run_check "" 1 > "$CHECK_OUT" 2>&1
check_status=$?
set -e
if ((check_status == 1)) && grep -q '^FAIL subnets-host: docker network ls failed$' "$CHECK_OUT" \
  && ! grep -q '^PASS subnets-host$' "$CHECK_OUT"; then
  ok 'check.sh rejects docker network ls failures'
else
  not_ok 'check.sh rejects docker network ls failures'
  cat "$CHECK_OUT" >&2
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
