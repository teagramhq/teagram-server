#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

readonly REPO=/opt/teagram-demo
readonly CONTRACT="$REPO/deploy/teagram-demo/lima/command-contract.sh"

if [[ "$EUID" -ne 0 ]]; then
  echo 'lima deploy wrapper must run as root' >&2
  exit 1
fi
if [[ $# -gt 1 ]]; then
  echo 'lima deploy wrapper accepts at most one legacy command argument' >&2
  exit 1
fi
[[ -d "$REPO/.git" && ! -L "$REPO" && ! -L "$REPO/.git" ]] || {
  echo 'lima deploy checkout is unavailable' >&2
  exit 1
}
[[ "$(stat -c '%u' "$REPO")" == 0 && "$(stat -c '%u' "$REPO/.git")" == 0 &&
  "$(stat -c '%u' "$REPO/deploy/teagram-demo/forced-command.sh")" == 0 &&
  "$(stat -c '%u' "$CONTRACT")" == 0 ]] || {
  echo 'lima deploy execution paths must be root-owned' >&2
  exit 1
}

# Revalidate the SSH client command against the same fixed verbs as charlie.
# shellcheck source=deploy/teagram-demo/lima/command-contract.sh
source "$CONTRACT"
# The old listener forwards the command as an argument through sudo during cutover.
if [[ $# == 1 ]]; then
  COMMAND=$1
  if [[ -n ${SSH_ORIGINAL_COMMAND:-} && "$COMMAND" != "$SSH_ORIGINAL_COMMAND" ]]; then
    echo 'legacy deploy command does not match SSH_ORIGINAL_COMMAND' >&2
    exit 1
  fi
else
  COMMAND=${SSH_ORIGINAL_COMMAND:-}
fi
demo_deploy_command_valid "$COMMAND" || {
  echo 'lima deploy command is not in the reviewed command contract' >&2
  exit 1
}

exec env -i PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
  HOME=/root DEMO_DEPLOY_TARGET=lima SSH_ORIGINAL_COMMAND="$COMMAND" \
  /bin/bash "$REPO/deploy/teagram-demo/forced-command.sh"
