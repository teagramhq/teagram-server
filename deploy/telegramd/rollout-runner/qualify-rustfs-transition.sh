#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
readonly APPROVED_QUALIFIER_ARTIFACT_SHA256=4f825040166168318c1b708f150910e5dd9ef834109f013ca37b6ed45cf8f028
readonly -a QUALIFIER_ARTIFACTS=(
  qualify-rustfs-transition.py
  rustfs-schema-capture.sql
  rustfs-inert-surfaces.sql
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

if [ "$#" -ne 3 ] || [ "$1" != check ]; then
  printf '%s\n' 'usage: qualify-rustfs-transition.sh check PRIVATE_BUNDLE_DIR CANDIDATE_CHECKOUT' >&2
  exit 64
fi

exec python3 "$SCRIPT_DIR/qualify-rustfs-transition.py" "$@"
