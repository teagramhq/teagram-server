#!/usr/bin/env bash
# Compatibility entrypoint for the teagram LXC's forced-command wrapper.
set -euo pipefail

SHA=${DEPLOY_SHA:-}
case "$SHA" in *[!0-9a-f]*|"") echo "DEPLOY_SHA must be a full 40-hex commit SHA"; exit 1;; esac
[ "${#SHA}" = 40 ] || { echo "DEPLOY_SHA must be a full 40-hex commit SHA"; exit 1; }

git fetch -q origin main
if ! git merge-base --is-ancestor "$SHA" origin/main 2>/dev/null; then
  echo "$SHA is not on origin/main; refusing"
  exit 1
fi

git show "$SHA:deploy/teagram/deploy-main.sh" | bash
