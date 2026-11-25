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
mkdir -p "$SECRETS_DIR"
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
chmod 444 "$SECRETS_DIR/turnstile-secret-key" "$SECRETS_DIR/turnstile-qa-bypass-token"
printf 'TUNNEL_TOKEN=fixture-tunnel-token\n' > "$SECRETS_DIR/cloudflared.env"
chmod 600 "$SECRETS_DIR/cloudflared.env"

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

# expect DESCRIPTION JQ-FILTER: the filter must evaluate to true on lima's config.
expect() {
  local description=$1 filter=$2
  if jq -e "$filter" "$LIMA_JSON" >/dev/null; then
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
# max_connections small enough to fit 64 MiB.
if grep -Eq '^shared_buffers = (1[0-6]|[1-9])MB$' "$SCRIPT_DIR/postgresql.conf" \
  && grep -Eq '^max_connections = (2[0-9]|3[0-2])$' "$SCRIPT_DIR/postgresql.conf"; then
  ok 'postgres config: shared_buffers at most 16 MB, max_connections at most 32'
else
  not_ok 'postgres config: shared_buffers at most 16 MB, max_connections at most 32'
fi

# The candidate teagramd-next exists only under its profile and has a limit.
if compose -f "$LIMA_COMPOSE" --project-name teagram-demo --profile candidate config --format json \
  | jq -e '.services["teagramd-next"] | .profiles == ["candidate"] and .mem_limit > 0' >/dev/null; then
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

# Criterion 6 and the check: a complete fixture passes everything except the
# blob cap, which only the host can provide. The output must not leak values.
CHECK_OUT=$TMP_DIR/check.out
set +e
TEAGRAM_DEMO_LIMA_ENV_FILE=$ENV_FILE TEAGRAM_DEMO_SECRETS_DIR=$SECRETS_DIR \
  "$SCRIPT_DIR/check.sh" > "$CHECK_OUT" 2>&1
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
if grep -qF -- "$PASSWORD_FIXTURE" "$CHECK_OUT" || grep -qF -- "fixture-tunnel-token" "$CHECK_OUT"; then
  not_ok 'check.sh prints no secret values'
else
  ok 'check.sh prints no secret values'
fi

# A missing tunnel token is named by key, and the check rejects.
mv "$SECRETS_DIR/cloudflared.env" "$TMP_DIR/cloudflared.env.moved"
set +e
TEAGRAM_DEMO_LIMA_ENV_FILE=$ENV_FILE TEAGRAM_DEMO_SECRETS_DIR=$SECRETS_DIR \
  "$SCRIPT_DIR/check.sh" > "$CHECK_OUT" 2>&1
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
