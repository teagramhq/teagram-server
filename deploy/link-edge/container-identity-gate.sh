#!/usr/bin/env bash
set -Eeuo pipefail
set +x
umask 077

fail() {
  printf '%s\n' "$1" >&2
  return 1
}

resolve_full_container_id() {
  local reference=$1 full_id
  [[ "$reference" =~ ^[0-9a-f]{12,64}$ ]] \
    || { fail 'container reference must be a hexadecimal Docker ID'; return 1; }

  full_id="$(docker inspect --format '{{.Id}}' "$reference" 2>/dev/null)" \
    || { fail 'cannot resolve complete Docker container ID'; return 1; }
  [[ "$full_id" =~ ^[0-9a-f]{64}$ ]] \
    || { fail 'Docker inspect did not return a complete container ID'; return 1; }
  [[ "${full_id:0:${#reference}}" == "$reference" ]] \
    || { fail 'inspected container ID does not match its reference'; return 1; }

  printf '%s\n' "$full_id"
}

usage() {
  printf '%s\n' 'usage: container-identity-gate.sh id CONTAINER_REF | compare BEFORE_REF AFTER_REF' >&2
  return 64
}

case "${1:-}" in
  id)
    [[ "$#" -eq 2 ]] || usage
    resolve_full_container_id "$2"
    ;;
  compare)
    [[ "$#" -eq 3 ]] || usage
    before_id="$(resolve_full_container_id "$2")" || exit 1
    after_id="$(resolve_full_container_id "$3")" || exit 1
    [[ "$before_id" == "$after_id" ]] \
      || { fail 'container identity changed'; exit 1; }
    printf 'container_identity=unchanged id=%s\n' "$before_id"
    ;;
  *) usage ;;
esac
