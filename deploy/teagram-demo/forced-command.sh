#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

readonly REPO=/opt/teagram-demo
readonly COMMAND=${SSH_ORIGINAL_COMMAND:-}
unset DEMO_DEPLOY_LOCK_HELD

if [[ "$EUID" -ne 0 ]]; then
  echo 'demo deploy key must run as root' >&2
  exit 1
fi
if [[ ! "$COMMAND" =~ ^([0-9a-f]{40})\ (sha256:[0-9a-f]{64})$ ]]; then
  echo 'demo deploy key accepts a full commit SHA and a sha256 image digest' >&2
  exit 1
fi
readonly SHA=${BASH_REMATCH[1]}
readonly IMAGE_DIGEST=${BASH_REMATCH[2]}
[[ -d "$REPO/.git" && ! -L "$REPO" ]] || { echo 'demo checkout is unavailable' >&2; exit 1; }
LOCK_PATH="$REPO/.state/deploy.lock"
mkdir -p "$(dirname "$LOCK_PATH")"
exec 9> "$LOCK_PATH"
flock -x 9
git -C "$REPO" diff --quiet HEAD -- || { echo 'demo checkout has tracked changes' >&2; exit 1; }
git -C "$REPO" fetch -q origin +refs/heads/main:refs/remotes/origin/main
git -C "$REPO" merge-base --is-ancestor "$SHA" origin/main || {
  echo "$SHA is not on origin/main; refusing" >&2
  exit 1
}

previous=$(git -C "$REPO" rev-parse HEAD)
if [[ "$SHA" != "$previous" ]] && git -C "$REPO" merge-base --is-ancestor "$SHA" "$previous"; then
  echo "live checkout is newer ($previous), skipped $SHA"
  exit 0
fi

git -C "$REPO" reset --hard "$SHA"
deploy_rc=0
DEMO_DEPLOY_LOCK_HELD=1 DEPLOY_SHA="$SHA" DEPLOY_IMAGE_DIGEST="$IMAGE_DIGEST" PREVIOUS_SHA="$previous" \
  bash "$REPO/deploy/teagram-demo/deploy-main.sh" || deploy_rc=$?
if (( deploy_rc != 0 )); then
  git -C "$REPO" reset --hard "$previous" || {
    echo "deploy failed and checkout restore failed; requested=$SHA previous=$previous" >&2
    exit 1
  }
fi
exit "$deploy_rc"
