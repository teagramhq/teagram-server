#!/usr/bin/env bash
set -Eeuo pipefail
set +x
umask 077

readonly BUSYBOX_IMAGE="mirror.gcr.io/library/node:24-alpine3.22@sha256:191c9f0080fcbbc6547a85dc0ff7988072214a355aabdc1d2ec55a7dae5eea8a"
readonly BUSYBOX_HUB_FALLBACK_IMAGE="docker.io/library/node:24-alpine3.22@sha256:191c9f0080fcbbc6547a85dc0ff7988072214a355aabdc1d2ec55a7dae5eea8a"
workspace="${GITHUB_WORKSPACE:-}"
phase_dir=""
lane=""
container_script=""
docker_image=""

report_failure() {
  local phase="$2" commit="unavailable" exit_value="$1"
  if [[ -n "$workspace" ]]; then
    commit="$(git -C "$workspace" rev-parse HEAD 2>/dev/null)" || commit="unavailable"
  fi
  [[ "$commit" =~ ^[0-9a-f]{40}$ ]] || commit="unavailable"
  [[ "$exit_value" =~ ^[0-9]{1,3}$ ]] || exit_value="unavailable"
  case "$phase" in
    container-launch|version-guard|package-setup|wrapper-tests|path-preflight|unavailable) ;;
    *) phase="unavailable" ;;
  esac
  printf '::error::%s failed (category: phase-failure; phase: %s; exit: %s; checked-out commit: %s; details redacted)\n' \
    "$lane" "$phase" "$exit_value" "$commit" || true
}

cleanup() {
  local status=$?
  trap - EXIT
  if [[ -n "$phase_dir" ]]; then
    rm -rf -- "$phase_dir" >/dev/null 2>&1 || true
  fi
  exit "$status"
}
trap cleanup EXIT

if [[ "$#" -ne 1 ]]; then
  printf 'one approved BusyBox lane is required\n' >&2
  exit 2
fi

case "$1" in
  browser-wrapper)
    lane="browser-wrapper"
    container_script=$(cat <<'SCRIPT'
set -eu
printf '%s' version-guard > /phase/current
busybox | grep -Fq "BusyBox v1.37.0"
printf '%s' package-setup > /phase/current
apk add --no-cache bash curl jq
printf '%s' wrapper-tests > /phase/current
node --test deploy/browser-acceptance/run-wrapper.test.mjs
SCRIPT
)
    ;;
  compose-path)
    lane="compose-path"
    container_script=$(cat <<'SCRIPT'
set -eu
printf '%s' version-guard > /phase/current
busybox realpath --help 2>&1 | grep -Fq "BusyBox v1.37.0"
printf '%s' package-setup > /phase/current
apk add --no-cache bash
printf '%s' path-preflight > /phase/current
bash deploy/teagramd/rollout-runner/test-compose-path-preflight.sh
SCRIPT
)
    ;;
  *)
    printf 'unsupported BusyBox lane\n' >&2
    exit 2
    ;;
esac

if [[ -z "${RUNNER_TEMP:-}" || ! -d "$RUNNER_TEMP" ]]; then
  report_failure 1 unavailable
  exit 1
fi
if [[ -z "$workspace" || "$workspace" != /* || ! -d "$workspace" ]]; then
  report_failure 1 unavailable
  exit 1
fi
if phase_dir="$(mktemp -d "$RUNNER_TEMP/phase.XXXXXX")"; then
  if ! chmod 700 "$phase_dir"; then
    report_failure 1 unavailable
    exit 1
  fi
else
  report_failure 1 unavailable
  exit 1
fi

if docker pull "$BUSYBOX_IMAGE" >/dev/null 2>&1; then
  docker_image="$BUSYBOX_IMAGE"
else
  echo "::warning::pinned image mirror pull failed; trying the same digest from Docker Hub"
  docker_image="$BUSYBOX_HUB_FALLBACK_IMAGE"
  if ! docker pull "$docker_image" >/dev/null 2>&1; then
    echo "::error::could not pull the pinned image from either registry"
    report_failure 1 container-launch
    exit 1
  fi
fi

docker_command=(
  docker run --rm
  --volume "$workspace:/workspace:ro"
  --volume "$phase_dir:/phase:rw"
  --workdir /workspace
  "$docker_image"
  sh -ec "$container_script"
)

command_token="$(python3 -c 'import secrets; print(secrets.token_hex(32))' 2>/dev/null)" \
  || command_token=""
stream_raw_output=0
if [[ "$command_token" =~ ^[0-9a-f]{64}$ ]] \
  && printf '::stop-commands::%s\n' "$command_token"; then
  stream_raw_output=1
fi

child_status=0
if ((stream_raw_output)); then
  if "${docker_command[@]}" 2>&1; then
    child_status=0
  else
    child_status=$?
  fi
  printf '\n::%s::\n' "$command_token" || true
elif "${docker_command[@]}" >/dev/null 2>&1; then
  child_status=0
else
  child_status=$?
fi
if ((child_status == 0)); then
  exit 0
fi

phase="unavailable"
if [[ -d "$phase_dir" && ! -L "$phase_dir" ]]; then
  shopt -s dotglob nullglob
  entries=("$phase_dir"/*)
  if ((${#entries[@]} == 0)); then
    phase="container-launch"
  elif ((${#entries[@]} == 1)) && [[ "${entries[0]}" == "$phase_dir/current" \
    && ! -L "${entries[0]}" && -f "${entries[0]}" ]]; then
    evidence_size="$(wc -c <"${entries[0]}" 2>/dev/null)" || evidence_size="invalid"
    evidence_size="${evidence_size//[[:space:]]/}"
    if [[ "$evidence_size" =~ ^[0-9]+$ ]] && ((evidence_size <= 32)); then
      evidence="$(cat -- "${entries[0]}" 2>/dev/null)" || evidence=""
      if ((${#evidence} == evidence_size)); then
        case "$lane:$evidence" in
          browser-wrapper:version-guard|browser-wrapper:package-setup|browser-wrapper:wrapper-tests|\
          compose-path:version-guard|compose-path:package-setup|compose-path:path-preflight)
            phase="$evidence"
            ;;
        esac
      fi
    fi
  fi
fi

report_failure "$child_status" "$phase"
exit "$child_status"
