#!/usr/bin/env bash
set -Eeuo pipefail

readonly WEB_SOURCE_SHA=${WEB_SOURCE_SHA:-}
readonly WEB_ARTIFACT_DIGEST=${WEB_ARTIFACT_DIGEST:-}
readonly BUILD_DIR=dist-private

[[ -f package.json && -f ci/private-mtproto-target.json ]] || {
  echo 'pinned teagram-web source checkout is unavailable' >&2
  exit 1
}
[[ "$WEB_SOURCE_SHA" =~ ^[0-9a-f]{40}$ ]] || {
  echo 'WEB_SOURCE_SHA must be a full commit SHA' >&2
  exit 1
}
[[ "$WEB_ARTIFACT_DIGEST" =~ ^sha256:[0-9a-f]{64}$ ]] || {
  echo 'WEB_ARTIFACT_DIGEST must be a sha256 digest' >&2
  exit 1
}

reviewed_mode=$(jq -er '.MTPROTO_TARGET_MODE | select(type == "string")' ci/private-mtproto-target.json)
reviewed_endpoint=$(jq -er '.MTPROTO_PRIVATE_ENDPOINT | select(type == "string")' ci/private-mtproto-target.json)
reviewed_key=$(jq -er '.MTPROTO_PRIVATE_RSA_PUBLIC_KEY_FILE | select(type == "string")' ci/private-mtproto-target.json)
[[ "$reviewed_mode" == private && "$reviewed_endpoint" =~ ^wss:// ]] || {
  echo 'pinned teagram-web source has an invalid reviewed private target' >&2
  exit 1
}

pnpm install --frozen-lockfile
MTPROTO_TARGET_MODE="$reviewed_mode" \
  MTPROTO_PRIVATE_ENDPOINT="$reviewed_endpoint" \
  MTPROTO_PRIVATE_RSA_PUBLIC_KEY_FILE="$reviewed_key" \
  pnpm exec vite build --outDir "$BUILD_DIR"
MTPROTO_TARGET_MODE="$reviewed_mode" \
  MTPROTO_PRIVATE_ENDPOINT="$reviewed_endpoint" \
  MTPROTO_PRIVATE_RSA_PUBLIC_KEY_FILE="$reviewed_key" \
  node scripts/private-artifact-release.mjs verify \
    --dist "$BUILD_DIR" --commit "$WEB_SOURCE_SHA" --ref refs/heads/master
jq -e --arg sha "$WEB_SOURCE_SHA" --arg digest "$WEB_ARTIFACT_DIGEST" \
  '.sourceCommit == $sha and .artifactDigest == $digest' \
  "$BUILD_DIR/mtproto-target.json" >/dev/null || {
  echo 'rebuilt web output differs from the pinned artifact identity' >&2
  exit 1
}
