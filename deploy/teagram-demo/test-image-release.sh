#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/teagram-demo/demo-common.sh
source "$SCRIPT_DIR/demo-common.sh"

TMP_DIR=$(mktemp -d)
trap 'rm -rf -- "$TMP_DIR"' EXIT

SHA=0123456789abcdef0123456789abcdef01234567
DIGEST=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
PULLS="$TMP_DIR/pulls"
MOCK_REVISION="$SHA"
MOCK_REPO_DIGESTS="[\"$DEMO_IMAGE_REPOSITORY@$DIGEST\"]"

docker() {
  case "$1 $2" in
    'pull ghcr.io/teagramhq/teagram-server-demo:sha-'*)
      printf '%s\n' "$2" >> "$PULLS"
      ;;
    'image inspect')
      case "$4" in
        *org.opencontainers.image.revision*) printf '%s\n' "$MOCK_REVISION" ;;
        *RepoDigests*) printf '%s\n' "$MOCK_REPO_DIGESTS" ;;
        *) return 2 ;;
      esac
      ;;
    *)
      printf 'unexpected docker command: %s\n' "$*" >&2
      return 2
      ;;
  esac
}

image_ref=$(demo_pull_image_by_sha "$SHA" 2>"$TMP_DIR/pull.log")
[[ "$image_ref" == "$DEMO_IMAGE_REPOSITORY@$DIGEST" ]]
[[ "$(cat "$PULLS")" == "$DEMO_IMAGE_REPOSITORY:sha-$SHA" ]]

DEMO_STATE_DIR="$TMP_DIR/state"
DEMO_ACTIVE_RELEASE_FILE="$DEMO_STATE_DIR/active-release.json"
install() {
  if [[ "$1" == -d ]]; then
    shift
    mkdir -p "${@: -1}"
  else
    command install "$@"
  fi
}
chown() { :; }
stat() {
  if [[ "$1" == -c && "$2" == '%u:%a' ]]; then
    printf '0:600\n'
  else
    command stat "$@"
  fi
}

demo_write_active_release teagramd "$image_ref"
[[ "$(demo_active_server)" == teagramd ]]
[[ "$(demo_active_image_ref)" == "$image_ref" ]]
demo_write_active_release teagramd-next "$DEMO_IMAGE_REPOSITORY@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
[[ "$(demo_active_server)" == teagramd-next ]]
[[ "$(command stat -c '%a' "$DEMO_ACTIVE_RELEASE_FILE")" == 600 ]]
if demo_write_active_release teagramd "$DEMO_IMAGE_REPOSITORY:latest" 2>/dev/null; then
  printf '%s\n' 'active release accepted a mutable registry tag' >&2
  exit 1
fi
[[ "$(demo_active_server)" == teagramd-next ]]

if demo_pull_image_by_sha short 2>/dev/null; then
  printf '%s\n' 'image pull accepted a non-full commit SHA' >&2
  exit 1
fi

MOCK_REVISION=$(printf '%040d' 7)
if demo_pull_image_by_sha "$SHA" 2>/dev/null; then
  printf '%s\n' 'image pull accepted a revision label for a different commit' >&2
  exit 1
fi
MOCK_REVISION="$SHA"

MOCK_REPO_DIGESTS="[\"$DEMO_IMAGE_REPOSITORY@$DIGEST\",\"$DEMO_IMAGE_REPOSITORY@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\"]"
if demo_pull_image_by_sha "$SHA" 2>/dev/null; then
  printf '%s\n' 'image pull accepted ambiguous repository digests' >&2
  exit 1
fi

printf '%s\n' 'demo image release pinning holds'
