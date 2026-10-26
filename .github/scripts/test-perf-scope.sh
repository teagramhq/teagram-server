#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPT="$SCRIPT_DIR/perf-scope.sh"
readonly TEMP_PREFIX=/tmp/teagram-perf-scope-test.
TMP=$(mktemp -d "${TEMP_PREFIX}XXXXXXXXXX")

cleanup() {
  local suffix
  case "$TMP" in
    "$TEMP_PREFIX"*)
      suffix=${TMP#"$TEMP_PREFIX"}
      case "$suffix" in
        ''|*/*|*[!A-Za-z0-9]*) return 1 ;;
        *) rm -rf -- "$TMP" ;;
      esac
      ;;
    *) return 1 ;;
  esac
}
trap cleanup EXIT

REPO="$TMP/repo"
git init -q -b main "$REPO"
git -C "$REPO" config user.name fixture
git -C "$REPO" config user.email fixture@example.invalid
mkdir -p "$REPO/.github/scripts"
printf 'ops script\n' > "$REPO/.github/scripts/perf-run.sh"
printf 'notes\n' > "$REPO/README.md"
git -C "$REPO" add .
git -C "$REPO" commit -q -m base
BASE=$(git -C "$REPO" rev-parse HEAD)

printf 'ops script updated\n' > "$REPO/.github/scripts/perf-run.sh"
git -C "$REPO" add .
git -C "$REPO" commit -q -m ops
[[ "$(cd "$REPO" && bash "$SCRIPT" "$BASE" HEAD)" == false ]] || {
  printf '%s\n' 'FAIL operational shell changes should not trigger RPC measurements' >&2
  exit 1
}

mkdir -p "$REPO/internal/example"
printf 'package example\n' > "$REPO/internal/example/example.go"
git -C "$REPO" add .
git -C "$REPO" commit -q -m go
[[ "$(cd "$REPO" && bash "$SCRIPT" "$BASE" HEAD)" == true ]] || {
  printf '%s\n' 'FAIL Go source changes should trigger RPC measurements' >&2
  exit 1
}

mkdir -p "$REPO/migrations"
printf 'SELECT 1;\n' > "$REPO/migrations/20260101000001_test.sql"
git -C "$REPO" add .
git -C "$REPO" commit -q -m migration
[[ "$(cd "$REPO" && bash "$SCRIPT" "$BASE" HEAD)" == true ]] || {
  printf '%s\n' 'FAIL migration changes should trigger RPC measurements' >&2
  exit 1
}

printf '%s\n' 'PASS RPC measurements run only for performance-affecting changes'
