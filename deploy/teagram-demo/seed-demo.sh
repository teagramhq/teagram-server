#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/teagram-demo/demo-common.sh
source "$SCRIPT_DIR/demo-common.sh"

MODE=
ENDPOINT=teagramd:2443
ENDPOINT_EXPLICIT=0
OWNER_USERNAME=demo_seed_owner
OWNER_PASSWORD_FILE="$DEMO_SEED_PASSWORD_FILE"
MESSAGE_ID=
TARGET_USERNAME=
MODERATION_SMOKE_MARKER=
MODERATION_SMOKE_MESSAGE_ID=
EXPECT_SIGNIN_ENABLED=

usage() {
  cat <<'EOF'
Usage:
  seed-demo.sh --check
  seed-demo.sh --seed [--endpoint teagramd:2443|teagramd-next:2443] [--owner-username demo_seed_owner] [--owner-password-file PATH]
  seed-demo.sh --verify-seed [--endpoint teagramd:2443|teagramd-next:2443]
  seed-demo.sh --verify-turnstile [--expect-signin-enabled=true|false] --endpoint haproxy:2443
  seed-demo.sh --moderate-delete-message --message-id ID [--endpoint haproxy:2443]
  seed-demo.sh --moderate-ban-user --username USERNAME [--endpoint haproxy:2443]
  seed-demo.sh --prepare-general-moderation-smoke --moderation-smoke-marker HEX [--endpoint teagramd:2443|teagramd-next:2443]
  seed-demo.sh --verify-general-moderation-smoke --moderation-smoke-marker HEX --moderation-smoke-message-id ID [--endpoint haproxy:2443]
  seed-demo.sh --cleanup-general-moderation-smoke --moderation-smoke-marker HEX [--endpoint haproxy:2443|teagramd:2443|teagramd-next:2443]
  Moderation commands route through the internal HAProxy listener.
EOF
}

while (($#)); do
  case "$1" in
    --check|--seed|--verify-seed|--verify-turnstile|--moderate-delete-message|--moderate-ban-user|\
      --prepare-general-moderation-smoke|--verify-general-moderation-smoke|--cleanup-general-moderation-smoke)
      [[ -z "$MODE" ]] || { usage >&2; demo_fail 'select exactly one mode'; }
      MODE="$1"
      shift
      ;;
    --endpoint)
      (($# >= 2)) || { usage >&2; demo_fail '--endpoint requires a value'; }
      ENDPOINT="$2"
      ENDPOINT_EXPLICIT=1
      shift 2
      ;;
    --owner-username)
      (($# >= 2)) || { usage >&2; demo_fail '--owner-username requires a value'; }
      OWNER_USERNAME="$2"
      shift 2
      ;;
    --owner-password-file)
      (($# >= 2)) || { usage >&2; demo_fail '--owner-password-file requires a path'; }
      OWNER_PASSWORD_FILE="$2"
      shift 2
      ;;
    --message-id)
      (($# >= 2)) || { usage >&2; demo_fail '--message-id requires a value'; }
      MESSAGE_ID="$2"
      shift 2
      ;;
    --username)
      (($# >= 2)) || { usage >&2; demo_fail '--username requires a value'; }
      TARGET_USERNAME="$2"
      shift 2
      ;;
    --moderation-smoke-marker)
      (($# >= 2)) || { usage >&2; demo_fail '--moderation-smoke-marker requires a value'; }
      MODERATION_SMOKE_MARKER="$2"
      shift 2
      ;;
    --moderation-smoke-message-id)
      (($# >= 2)) || { usage >&2; demo_fail '--moderation-smoke-message-id requires a value'; }
      MODERATION_SMOKE_MESSAGE_ID="$2"
      shift 2
      ;;
    --expect-signin-enabled)
      (($# >= 2)) || { usage >&2; demo_fail '--expect-signin-enabled requires true or false'; }
      EXPECT_SIGNIN_ENABLED="$2"
      [[ "$EXPECT_SIGNIN_ENABLED" == true || "$EXPECT_SIGNIN_ENABLED" == false ]] ||
        demo_fail '--expect-signin-enabled must be true or false'
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      usage >&2
      demo_fail "unsupported argument: $1"
      ;;
  esac
done

[[ -n "$MODE" ]] || { usage >&2; demo_fail 'select one mode'; }
if [[ "$MODE" != --verify-turnstile && -n "$EXPECT_SIGNIN_ENABLED" ]]; then
  demo_fail '--expect-signin-enabled applies only to --verify-turnstile'
fi
ENDPOINT=$(demo_resolve_seed_endpoint "$MODE" "$ENDPOINT_EXPLICIT" "$ENDPOINT") ||
  demo_fail 'resolve endpoint for demo seed operation'
if [[ "$MODE" == --verify-turnstile || "$MODE" == --moderate-delete-message || "$MODE" == --moderate-ban-user ||
  "$MODE" == --verify-general-moderation-smoke ]]; then
  [[ "$ENDPOINT" == haproxy:2443 ]] ||
    demo_fail "endpoint for $MODE must be the internal haproxy:2443 listener"
elif [[ "$MODE" == --prepare-general-moderation-smoke ]]; then
  [[ "$ENDPOINT" == teagramd:2443 || "$ENDPOINT" == teagramd-next:2443 ]] ||
    demo_fail 'General moderation smoke preparation must use a private teagramd endpoint'
elif [[ "$MODE" == --cleanup-general-moderation-smoke ]]; then
  [[ "$ENDPOINT" == haproxy:2443 || "$ENDPOINT" == teagramd:2443 || "$ENDPOINT" == teagramd-next:2443 ]] ||
    demo_fail 'General moderation smoke cleanup requires an internal endpoint'
else
  [[ "$ENDPOINT" == teagramd:2443 || "$ENDPOINT" == teagramd-next:2443 ]] ||
    demo_fail "endpoint for $MODE must be an internal teagramd service"
fi
if [[ "$MODE" == --moderate-delete-message ]]; then
  [[ "$MESSAGE_ID" =~ ^[1-9][0-9]*$ ]] || demo_fail '--message-id must be a positive integer'
  [[ -z "$TARGET_USERNAME" ]] || demo_fail '--username applies only to --moderate-ban-user'
elif [[ "$MODE" == --moderate-ban-user ]]; then
  [[ -z "$MESSAGE_ID" ]] || demo_fail '--message-id applies only to --moderate-delete-message'
  demo_valid_signup_username "$TARGET_USERNAME" || demo_fail '--username must follow signup rules and must not include @'
else
  [[ -z "$MESSAGE_ID" && -z "$TARGET_USERNAME" ]] || demo_fail '--message-id and --username require a moderation mode'
fi
if [[ "$MODE" == --prepare-general-moderation-smoke || "$MODE" == --verify-general-moderation-smoke ||
  "$MODE" == --cleanup-general-moderation-smoke ]]; then
  [[ "$MODERATION_SMOKE_MARKER" =~ ^[a-f0-9]{32}$ ]] ||
    demo_fail '--moderation-smoke-marker must be 32 lowercase hexadecimal characters'
  [[ -z "$MESSAGE_ID" && -z "$TARGET_USERNAME" ]] ||
    demo_fail '--message-id and --username do not apply to moderation smoke operations'
  if [[ "$MODE" == --verify-general-moderation-smoke ]]; then
    [[ "$MODERATION_SMOKE_MESSAGE_ID" =~ ^[1-9][0-9]*$ ]] ||
      demo_fail '--moderation-smoke-message-id must be a positive integer'
    python3 - "$MODERATION_SMOKE_MESSAGE_ID" <<'PY'
import sys

if int(sys.argv[1]) > 2_147_483_647:
    raise SystemExit("teagram-demo: moderation smoke message ID exceeds signed 32-bit MTProto range")
PY
  else
    [[ -z "$MODERATION_SMOKE_MESSAGE_ID" ]] ||
      demo_fail '--moderation-smoke-message-id applies only to verification'
  fi
else
  [[ -z "$MODERATION_SMOKE_MARKER" && -z "$MODERATION_SMOKE_MESSAGE_ID" ]] ||
    demo_fail 'moderation smoke arguments require a moderation smoke operation'
fi

check_dependencies() {
  local command_name version
  for command_name in bash curl docker env git grep jq openssl python3 flock install stat mktemp; do
    command -v "$command_name" >/dev/null 2>&1 || demo_fail "required command is missing: $command_name"
  done
  docker info >/dev/null 2>&1 || demo_fail 'Docker Engine is unavailable'
  docker compose version --short >/dev/null 2>&1 || demo_fail 'Docker Compose v2 is unavailable'
  version=$(docker compose version --short)
  python3 - "$version" <<'PY'
import re
import sys

match = re.fullmatch(r"v?(\d+)\.(\d+)\.(\d+)(?:[-+].*)?", sys.argv[1])
if not match or tuple(map(int, match.groups())) < (2, 39, 4):
    raise SystemExit("teagram-demo: Docker Compose v2.39.4 or later is required")
PY
  demo_require_env
}

check_source_wiring() {
  [[ -f "$DEMO_REPO_ROOT/cmd/demo-seed/main.go" ]] || demo_fail 'demo-seed source is missing'
  [[ -f "$DEMO_REPO_ROOT/cmd/demo-seed/seed.go" ]] || demo_fail 'demo-seed API implementation is missing'
  [[ -f "$DEMO_SCRIPT_DIR/compose.seed.yaml" ]] || demo_fail 'seed Compose overlay is missing'
  [[ -x "$DEMO_SCRIPT_DIR/moderate-general.sh" ]] || demo_fail 'General moderation command is missing or not executable'
  [[ -x "$DEMO_SCRIPT_DIR/test-seed-compose.sh" ]] || demo_fail 'seed convergence Compose API test is missing or not executable'
  [[ -x "$DEMO_SCRIPT_DIR/test-general-moderation-compose.sh" ]] || demo_fail 'General moderation Compose smoke test is missing or not executable'
  grep -Fq 'go build -o /demo-seed ./cmd/demo-seed' "$DEMO_REPO_ROOT/Dockerfile" ||
    demo_fail 'Dockerfile does not build the demo-seed binary'
  grep -Fq 'entrypoint: ["/usr/local/bin/demo-seed"]' "$DEMO_SCRIPT_DIR/compose.yaml" ||
    demo_fail 'Compose does not wire the demo-seed binary'
}

check_image_binary() {
  local image_ref
  image_ref=$(demo_compose_image_ref)
  if docker image inspect "$image_ref" >/dev/null 2>&1; then
    docker run --rm --network none --read-only --cap-drop ALL \
      --security-opt no-new-privileges --entrypoint /usr/local/bin/demo-seed \
      "$image_ref" --check --endpoint "$ENDPOINT" >/dev/null ||
      demo_fail 'the selected teagramd image cannot run demo-seed --check'
  fi
}

check_seed_secret_files() {
  [[ "$EUID" -eq 0 ]] || demo_fail 'seed and verify-seed modes must run as root on the demo LXC'
  [[ "$OWNER_USERNAME" == "$DEMO_SEED_OWNER_USERNAME" ]] || demo_fail 'owner username must be demo_seed_owner'
  [[ "$OWNER_PASSWORD_FILE" == "$DEMO_SEED_PASSWORD_FILE" ]] || demo_fail 'owner password must use the protected demo seed-owner file'
  [[ ! -L "$DEMO_SEED_SECRET_DIR" ]] || demo_fail 'seed secret directory must not be a symlink'
  [[ ! -L "$DEMO_SEED_ACCOUNT_DIR" ]] || demo_fail 'demo account directory must not be a symlink'
  [[ -f "$OWNER_PASSWORD_FILE" && ! -L "$OWNER_PASSWORD_FILE" ]] || demo_fail 'seed-owner password file is missing or unsafe'
  [[ "$(stat -c '%u:%a' "$OWNER_PASSWORD_FILE")" == '0:600' ]] || demo_fail 'seed-owner password file must be root-owned mode 0600'
  grep -Eq '^[0-9a-f]{64}$' "$OWNER_PASSWORD_FILE" || demo_fail 'seed-owner password file has an invalid value'
  demo_prepare_seed_owner_session
  [[ -f "$DEMO_SEED_OWNER_SESSION_FILE" && ! -L "$DEMO_SEED_OWNER_SESSION_FILE" ]] || demo_fail 'seed-owner session file is missing or unsafe'
  [[ "$(stat -c '%u:%a' "$DEMO_SEED_OWNER_SESSION_FILE")" == '0:600' ]] || demo_fail 'seed-owner session file must be root-owned mode 0600'
  [[ "$(stat -c '%u:%a' "$DEMO_SEED_SECRET_DIR")" == '0:700' ]] || demo_fail 'seed secret directory must be root-owned mode 0700'
  if [[ ! -e "$DEMO_SEED_ACCOUNT_DIR" ]]; then
    install -d -o root -g root -m 0700 "$DEMO_SEED_ACCOUNT_DIR"
  fi
  [[ -d "$DEMO_SEED_ACCOUNT_DIR" ]] || demo_fail 'demo account path is not a directory'
  [[ "$(stat -c '%u:%a' "$DEMO_SEED_ACCOUNT_DIR")" == '0:700' ]] || demo_fail 'demo account directory must be root-owned mode 0700'
  [[ ! -L "$DEMO_SEED_ACCOUNT_PASSWORD_FILE" ]] || demo_fail 'demo account password inventory must not be a symlink'
  if [[ -e "$DEMO_SEED_ACCOUNT_PASSWORD_FILE" ]]; then
    [[ -f "$DEMO_SEED_ACCOUNT_PASSWORD_FILE" && "$(stat -c '%u:%a' "$DEMO_SEED_ACCOUNT_PASSWORD_FILE")" == '0:600' ]] ||
      demo_fail 'demo account password inventory must be root-owned mode 0600'
  fi
}

run_seed_helper() {
  local service="$1"
  shift
  demo_seed_compose run --rm --no-deps --pull never "$service" "$@"
}

run_moderation_helper() {
  demo_compose --profile seed run --rm --no-deps --pull never demo-seed "$@"
}

case "$MODE" in
  --check)
    check_dependencies
    check_source_wiring
    check_image_binary
    printf 'seed-demo check=ok endpoint=%s\n' "$ENDPOINT"
    ;;
  --seed)
    demo_require_env
    check_seed_secret_files
    run_seed_helper demo-seed \
      --seed --endpoint "$ENDPOINT" \
      --owner-username "$OWNER_USERNAME"
    ;;
  --verify-seed)
    demo_require_env
    check_seed_secret_files
    run_seed_helper demo-seed \
      --verify-seed --endpoint "$ENDPOINT" \
      --owner-username "$OWNER_USERNAME"
    ;;
  --moderate-delete-message)
    demo_require_env
    check_seed_secret_files
    run_moderation_helper \
      --moderate-delete-message "$MESSAGE_ID" --endpoint "$ENDPOINT" \
      --owner-username "$OWNER_USERNAME"
    ;;
  --moderate-ban-user)
    demo_require_env
    check_seed_secret_files
    run_moderation_helper \
      --moderate-ban-user "$TARGET_USERNAME" --endpoint "$ENDPOINT" \
      --owner-username "$OWNER_USERNAME"
    ;;
  --prepare-general-moderation-smoke)
    demo_require_env
    check_seed_secret_files
    run_seed_helper demo-seed \
      --prepare-general-moderation-smoke --moderation-smoke-marker "$MODERATION_SMOKE_MARKER" \
      --endpoint "$ENDPOINT" --owner-username "$OWNER_USERNAME"
    ;;
  --verify-general-moderation-smoke)
    demo_require_env
    check_seed_secret_files
    run_moderation_helper \
      --verify-general-moderation-smoke --moderation-smoke-marker "$MODERATION_SMOKE_MARKER" \
      --moderation-smoke-message-id "$MODERATION_SMOKE_MESSAGE_ID" --endpoint "$ENDPOINT" \
      --owner-username "$OWNER_USERNAME"
    ;;
  --cleanup-general-moderation-smoke)
    demo_require_env
    check_seed_secret_files
    if [[ "$ENDPOINT" == haproxy:2443 ]]; then
      run_moderation_helper \
        --cleanup-general-moderation-smoke --moderation-smoke-marker "$MODERATION_SMOKE_MARKER" \
        --endpoint "$ENDPOINT" --owner-username "$OWNER_USERNAME"
    else
      run_seed_helper demo-seed \
        --cleanup-general-moderation-smoke --moderation-smoke-marker "$MODERATION_SMOKE_MARKER" \
        --endpoint "$ENDPOINT" --owner-username "$OWNER_USERNAME"
    fi
    ;;
  --verify-turnstile)
    demo_require_env
    if [[ -z "$EXPECT_SIGNIN_ENABLED" ]]; then
      EXPECT_SIGNIN_ENABLED=$(demo_turnstile_signin_enabled) || demo_fail 'read sign-in Turnstile setting'
    fi
    run_seed_helper demo-probe --verify-turnstile "--expect-signin-enabled=$EXPECT_SIGNIN_ENABLED" --endpoint "$ENDPOINT"
    ;;
esac
