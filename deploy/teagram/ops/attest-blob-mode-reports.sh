#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

readonly REPO=/opt/teagram

fail() {
  printf 'blob report attestation rejected: %s\n' "$1" >&2
  exit 1
}

if [[ "$#" != 0 ]]; then
  printf '%s\n' 'usage: attest-blob-mode-reports.sh' >&2
  exit 64
fi
[[ "$(id -u)" == 0 ]] || fail 'root is required'
[[ "${OPS_SHA:-}" =~ ^[0-9a-f]{40}$ ]] || fail 'reviewed source SHA is invalid'

TEMP_DIR=$(mktemp -d /tmp/teagram-blob-report-attestation.XXXXXXXX) || fail 'cannot create the private helper directory'
[[ -d "$TEMP_DIR" && ! -L "$TEMP_DIR" && "$(stat -c '%a' -- "$TEMP_DIR")" == 700 ]] || {
  fail 'private helper directory failed validation'
}
cleanup() {
  local status=$?
  trap - EXIT
  case "${TEMP_DIR:-}" in
    /tmp/teagram-blob-report-attestation.????????)
      if [[ -d "$TEMP_DIR" && ! -L "$TEMP_DIR" ]]; then
        rm -rf -- "$TEMP_DIR" || status=1
      else
        status=1
      fi
      ;;
    *)
      printf '%s\n' 'refusing to remove an unexpected helper path' >&2
      status=1
      ;;
  esac
  exit "$status"
}
trap cleanup EXIT

git -C "$REPO" show "$OPS_SHA:deploy/teagramd/rollout-runner/attest-blob-mode-reports.py" \
  > "$TEMP_DIR/attest-blob-mode-reports.py" || fail 'cannot stage the reviewed report attestation operation'
git -C "$REPO" show "$OPS_SHA:deploy/teagramd/rollout-runner/blob-mode-state.py" \
  > "$TEMP_DIR/blob-mode-state.py" || fail 'cannot stage the reviewed blob authority validator'
git -C "$REPO" show "$OPS_SHA:deploy/teagramd/rollout-runner/blob-transition-runner.py" \
  > "$TEMP_DIR/blob-transition-runner.py" || fail 'cannot stage the reviewed deployment authority validator'
chmod 600 "$TEMP_DIR/attest-blob-mode-reports.py" "$TEMP_DIR/blob-mode-state.py" \
  "$TEMP_DIR/blob-transition-runner.py" || fail 'cannot protect the staged report attestation helpers'

cd "$REPO"
python3 "$TEMP_DIR/attest-blob-mode-reports.py"
