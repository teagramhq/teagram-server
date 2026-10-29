#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/teagram-demo/demo-common.sh
source "$SCRIPT_DIR/demo-common.sh"

demo_require_root
demo_require_env
EXPECTED_PREVIOUS_GENERATION=${1:-}
if [[ "$#" -gt 1 || ( -n "$EXPECTED_PREVIOUS_GENERATION" && ! "$EXPECTED_PREVIOUS_GENERATION" =~ ^[0-9a-f]{64}$ ) ]]; then
  demo_fail 'expected at most one previous wipe generation with 64 lowercase hexadecimal characters'
fi

if [[ -n "$(demo_compose ps --status running -q edge haproxy)" ]]; then
  demo_fail 'seed convergence test requires edge and HAProxy to be stopped'
fi

ACTIVE_SERVER=$(demo_active_server)
ACTIVE_ENDPOINT="$ACTIVE_SERVER:2443"
mapfile -t SERVER_IDS < <(demo_seed_compose ps --status running -q "$ACTIVE_SERVER")
((${#SERVER_IDS[@]} == 1)) || demo_fail 'seed convergence test requires one running teagramd service'
docker inspect "${SERVER_IDS[0]}" | jq -e '
  .[0].Config.Env as $env
  | ($env | any(. == "TG_CLIENT_ADDR_TRUST=socket"))
    and ($env | any(. == "TG_UPLOADS_DISABLED=false"))
    and ($env | any(. == "TG_TURNSTILE_SITE_KEY="))
    and ($env | any(. == "TG_TURNSTILE_SECRET_KEY_FILE="))
' >/dev/null || demo_fail 'seed convergence test requires the private seed-only server configuration'

SEED_SCRIPT="$SCRIPT_DIR/seed-demo.sh"
OWNER_USERNAME="$DEMO_SEED_OWNER_USERNAME"
OWNER_PASSWORD_FILE="$DEMO_SEED_PASSWORD_FILE"
EXPECTED_REPORT='demo seed verified accounts=10 welcome_posts=1 broadcast_channels=3 posts_per_channel=32 public_groups=3 posts_per_group=12 searchable_spaces=7 markers=133 search_account=demo_searcher reaction_examples=1'

capture_seed_report() {
  local output report line
  output=$("$SEED_SCRIPT" --verify-seed --endpoint "$ACTIVE_ENDPOINT" \
    --owner-username "$OWNER_USERNAME" --owner-password-file "$OWNER_PASSWORD_FILE")
  report=
  while IFS= read -r line; do
    if [[ "$line" == "demo seed verified "* ]]; then
      report="$line"
    fi
  done <<< "$output"
  [[ -n "$report" ]] || demo_fail 'API verification did not report seeded content counts'
  printf '%s\n' "$report"
}

capture_wipe_generation() {
  local output
  output=$(demo_compose run --rm --no-deps "$ACTIVE_SERVER" client-config)
  jq -er '.demo.wipe_generation | select(type == "string" and test("^[0-9a-f]{64}$"))' <<< "$output"
}

before_generation=$(capture_wipe_generation)
if [[ -n "$EXPECTED_PREVIOUS_GENERATION" && "$before_generation" == "$EXPECTED_PREVIOUS_GENERATION" ]]; then
  demo_fail 'fresh database reused the previous wipe generation'
fi
before=$(capture_seed_report)
[[ "$before" == "$EXPECTED_REPORT" ]] || demo_fail 'first API verification did not match the expected content counts and search account'

"$SEED_SCRIPT" --seed --endpoint "$ACTIVE_ENDPOINT" \
  --owner-username "$OWNER_USERNAME" --owner-password-file "$OWNER_PASSWORD_FILE"

after=$(capture_seed_report)
[[ "$after" == "$EXPECTED_REPORT" ]] || demo_fail 'second API verification did not match the expected content counts and search account'
[[ "$before" == "$after" ]] || demo_fail 're-seeding changed the verified content counts or search result'
after_generation=$(capture_wipe_generation)
[[ "$after_generation" == "$before_generation" ]] || demo_fail 're-seeding changed the stored wipe generation'

demo_seed_compose up -d --force-recreate --wait --wait-timeout 120 "$ACTIVE_SERVER"
after_restart_generation=$(capture_wipe_generation)
[[ "$after_restart_generation" == "$before_generation" ]] || demo_fail 'server restart changed the stored wipe generation'
after_restart=$(capture_seed_report)
[[ "$after_restart" == "$EXPECTED_REPORT" ]] || demo_fail 'server restart changed the verified content counts or search account'

printf 'seed Compose API convergence=verified %s wipe_generation=%s\n' "$after" "$after_restart_generation"
