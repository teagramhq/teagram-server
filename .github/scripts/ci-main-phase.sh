#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
script_path="$script_dir/ci-main-phase.sh"
source "$script_dir/smoke-diagnostics.sh"

run_phase_command() {
  local phase="$1" image
  case "$phase" in
    build)
      env -u EDGE_IMAGE_SOURCE -u EDGE_IMAGE_REVISION docker compose --env-file /dev/null --project-directory deploy/link-edge --file deploy/link-edge/compose.yaml build
      ;;
    label-check)
      for image in telegram-linklanding:local telegram-linkselector:local; do
        docker image inspect "$image" --format '{{json .Config.Labels}}' \
          | jq -e '(.["org.opencontainers.image.source"] // "") == "" and (.["org.opencontainers.image.revision"] // "") == ""'
      done
      ;;
    *)
      return 2
      ;;
  esac
}

report_phase_failure() {
  local phase="$1" status="$2" commit=""
  case "$phase" in
    build|label-check) ;;
    *) phase=unavailable ;;
  esac
  if [[ ! "$status" =~ ^[0-9]{1,3}$ ]]; then
    status=unavailable
  fi
  commit=$(git rev-parse --verify 'HEAD^{commit}' 2>/dev/null) || commit=""
  if [[ ! "$commit" =~ ^[0-9a-f]{40}$ ]]; then
    commit=unavailable
  fi
  printf '::error::ci-main failed (category: phase-failure; phase: %s; exit: %s; checked-out commit: %s; details redacted)\n' \
    "$phase" "$status" "$commit"
}

if [[ "${1:-}" == --execute ]]; then
  if [[ "$#" -ne 2 ]]; then
    exit 2
  fi
  run_phase_command "$2"
  exit 0
fi

if [[ "$#" -ne 1 ]]; then
  printf 'ci-main phase invocation is invalid\n' >&2
  exit 2
fi

phase="$1"
case "$phase" in
  build|label-check) ;;
  *)
    printf 'ci-main phase invocation is invalid\n' >&2
    exit 2
    ;;
esac

status=0
command_token=""
if generated_token=$(smoke_generate_command_token 2>/dev/null) \
  && [[ "$generated_token" =~ ^[0-9a-f]{64}$ ]] \
  && printf '::stop-commands::%s\n' "$generated_token"; then
  command_token="$generated_token"
fi

if [[ -n "$command_token" ]]; then
  if bash "$script_path" --execute "$phase"; then
    status=0
  else
    status=$?
  fi
  printf '::%s::\n' "$command_token" || true
else
  if bash "$script_path" --execute "$phase" >/dev/null 2>&1; then
    status=0
  else
    status=$?
  fi
fi

if [[ "$status" -ne 0 ]]; then
  report_phase_failure "$phase" "$status" || true
fi
exit "$status"
