#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
readonly APPROVED_QUALIFIER_ARTIFACT_SHA256=df0488b779227034e6e47643d5255d7d0155c687600b1a3f0c7ce4909c5b5d86
readonly -a QUALIFIER_ARTIFACTS=(
  qualify-rustfs-transition.py
  rustfs-schema-capture.sql
  rustfs-inert-surfaces.sql
  rustfs-r70-schema-capture.sql
  rustfs-r70-inert-surfaces.sql
)

qualifier_artifact_digest() {
  local artifact digest
  for artifact in "${QUALIFIER_ARTIFACTS[@]}"; do
    [ -f "$SCRIPT_DIR/$artifact" ] && [ ! -L "$SCRIPT_DIR/$artifact" ] || return 1
  done
  digest=$(
    cd -P "$SCRIPT_DIR" &&
      sha256sum -- "${QUALIFIER_ARTIFACTS[@]}" 2>/dev/null | sha256sum 2>/dev/null
  ) || return 1
  printf '%s\n' "${digest%% *}"
}

if [ "$(id -u)" != 0 ]; then
  printf '%s\n' 'RustFS qualification requires uid 0 for root-owned evidence checks' >&2
  exit 77
fi

artifact_digest=$(qualifier_artifact_digest) || {
  printf '%s\n' 'gate_result=reject reason=qualifier_artifact_digest' >&2
  exit 1
}
[ "$artifact_digest" = "$APPROVED_QUALIFIER_ARTIFACT_SHA256" ] || {
  printf '%s\n' 'gate_result=reject reason=qualifier_artifact_digest' >&2
  exit 1
}

if [ "$#" -ne 3 ] || { [ "$1" != check ] && [ "$1" != pre-copy ]; }; then
  printf '%s\n' 'usage: qualify-rustfs-transition.sh check|pre-copy PRIVATE_BUNDLE_DIR CANDIDATE_CHECKOUT' >&2
  exit 64
fi

exec python3 "$SCRIPT_DIR/qualify-rustfs-transition.py" "$@"
