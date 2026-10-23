#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/teagram-demo/demo-common.sh
source "$SCRIPT_DIR/demo-common.sh"

MODE=
ENDPOINT=teagramd:2443
OWNER_USERNAME=demo_seed_owner
OWNER_PASSWORD_FILE="$DEMO_SEED_PASSWORD_FILE"

usage() {
  cat <<'EOF'
Usage:
  seed-demo.sh --check
  seed-demo.sh --seed [--endpoint teagramd:2443|teagramd-next:2443] [--owner-username demo_seed_owner] [--owner-password-file PATH]
  seed-demo.sh --verify-seed [--endpoint teagramd:2443|teagramd-next:2443]
  seed-demo.sh --verify-turnstile --endpoint haproxy:2443
EOF
}

while (($#)); do
  case "$1" in
    --check|--seed|--verify-seed|--verify-turnstile)
      [[ -z "$MODE" ]] || { usage >&2; demo_fail 'select exactly one mode'; }
      MODE="$1"
      shift
      ;;
    --endpoint)
      (($# >= 2)) || { usage >&2; demo_fail '--endpoint requires a value'; }
      ENDPOINT="$2"
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
if [[ "$MODE" == --verify-turnstile ]]; then
  [[ "$ENDPOINT" == haproxy:2443 ]] ||
    demo_fail "endpoint for $MODE must be the internal haproxy:2443 service"
else
  [[ "$ENDPOINT" == teagramd:2443 || "$ENDPOINT" == teagramd-next:2443 ]] ||
    demo_fail "endpoint for $MODE must be an internal teagramd service"
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
  [[ -x "$DEMO_SCRIPT_DIR/test-seed-compose.sh" ]] || demo_fail 'seed convergence Compose API test is missing or not executable'
  grep -Fq 'go build -o /demo-seed ./cmd/demo-seed' "$DEMO_REPO_ROOT/Dockerfile" ||
    demo_fail 'Dockerfile does not build the demo-seed binary'
  grep -Fq 'entrypoint: ["/usr/local/bin/demo-seed"]' "$DEMO_SCRIPT_DIR/compose.yaml" ||
    demo_fail 'Compose does not wire the demo-seed binary'
}

check_image_binary() {
  if docker image inspect teagramd:local >/dev/null 2>&1; then
    docker run --rm --network none --read-only --cap-drop ALL \
      --security-opt no-new-privileges --entrypoint /usr/local/bin/demo-seed \
      teagramd:local --check --endpoint "$ENDPOINT" >/dev/null ||
      demo_fail 'the built teagramd image cannot run demo-seed --check'
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
  --verify-turnstile)
    demo_require_env
    run_seed_helper demo-probe --verify-turnstile --endpoint "$ENDPOINT"
    ;;
esac
