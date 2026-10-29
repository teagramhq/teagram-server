#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=deploy/teagram-demo/demo-common.sh
source "$SCRIPT_DIR/demo-common.sh"

readonly WEB_SOURCE_SHA=${DEPLOY_WEB_SOURCE_SHA:-}
readonly WEB_ARTIFACT_DIGEST=${DEPLOY_WEB_ARTIFACT_DIGEST:-}
readonly WEB_ARCHIVE_SHA256=${DEPLOY_WEB_ARCHIVE_SHA256:-}
readonly WEB_ARCHIVE=${DEPLOY_WEB_ARCHIVE:-}
TMP_LINK=
PREVIOUS_LINK_TARGET=
PREVIOUS_WEB_RELEASE=
WEB_ACTIVATED=0
RELEASE_COMMITTED=0

cleanup() {
  local rc=$?
  local rollback_failed=0 restore_link
  trap - EXIT
  if (( rc != 0 && WEB_ACTIVATED == 1 && RELEASE_COMMITTED == 0 )); then
    if [[ -n "$TMP_LINK" ]]; then
      rm -f -- "$TMP_LINK"
    fi
    if [[ -n "$PREVIOUS_LINK_TARGET" ]]; then
      restore_link="$DEMO_WEB_CURRENT_LINK.rollback.$$"
      rm -f -- "$restore_link"
      if ! ln -s -- "$PREVIOUS_LINK_TARGET" "$restore_link" ||
        ! mv -Tf -- "$restore_link" "$DEMO_WEB_CURRENT_LINK"; then
        rollback_failed=1
      fi
    else
      rm -f -- "$DEMO_WEB_CURRENT_LINK" || rollback_failed=1
    fi
    if (( rollback_failed == 0 )); then
      printf 'demo web release reverted to %s\n' "${PREVIOUS_WEB_RELEASE:-bootstrap status page}" >&2
    else
      printf 'demo web release rollback could not be confirmed; inspect %s\n' "$DEMO_WEB_CURRENT_LINK" >&2
      rc=1
    fi
  fi
  exit "$rc"
}
trap cleanup EXIT

demo_require_root
demo_require_env
[[ "$WEB_SOURCE_SHA" =~ ^[0-9a-f]{40}$ ]] || demo_fail 'web source SHA must be a full 40-hex commit'
[[ "$WEB_ARTIFACT_DIGEST" =~ ^sha256:[0-9a-f]{64}$ ]] || demo_fail 'web artifact digest must be sha256'
[[ "$WEB_ARCHIVE_SHA256" =~ ^[0-9a-f]{64}$ ]] || demo_fail 'web archive digest must be a full sha256'
[[ -f "$WEB_ARCHIVE" && ! -L "$WEB_ARCHIVE" ]] || demo_fail 'received web artifact archive is missing or unsafe'
[[ ! -L "$DEMO_STATE_DIR" && ! -L "$DEMO_CLIENT_CONFIG_DIR" && ! -L "$DEMO_WEB_ROOT" ]] ||
  demo_fail 'public web release path must not contain a symlinked parent'
demo_prepare_client_config_dir

ACTIVE_SERVER=$(demo_active_server)
ACTIVE_IMAGE_REF=$(demo_active_image_ref)
if ! PREVIOUS_WEB_RELEASE=$(demo_reconcile_web_release); then
  demo_fail 'active web release metadata could not be verified'
fi
if [[ -n "$PREVIOUS_WEB_RELEASE" ]]; then
  PREVIOUS_LINK_TARGET="releases/${PREVIOUS_WEB_RELEASE// /-}"
fi

demo_require_proxy_configuration_current edge
STAGED_WEB_RELEASE=$(DEPLOY_WEB_ARCHIVE_SHA256="$WEB_ARCHIVE_SHA256" \
  python3 "$SCRIPT_DIR/install-web-release.py" stage \
    "$WEB_ARCHIVE" "$DEMO_WEB_RELEASES_DIR" "$WEB_SOURCE_SHA" "$WEB_ARTIFACT_DIGEST")
readonly STAGED_WEB_RELEASE
[[ "$STAGED_WEB_RELEASE" == "$WEB_SOURCE_SHA-${WEB_ARTIFACT_DIGEST#sha256:}" ]] ||
  demo_fail 'staged web release directory does not match its artifact identity'

TMP_LINK="$DEMO_WEB_CURRENT_LINK.next.$$"
[[ ! -e "$TMP_LINK" && ! -L "$TMP_LINK" ]] || demo_fail 'temporary web release link already exists'
ln -s -- "releases/$STAGED_WEB_RELEASE" "$TMP_LINK"
mv -Tf -- "$TMP_LINK" "$DEMO_WEB_CURRENT_LINK"
TMP_LINK=
WEB_ACTIVATED=1

python3 "$SCRIPT_DIR/smoke.py" \
  --web-source-commit "$WEB_SOURCE_SHA" \
  --web-artifact-digest "$WEB_ARTIFACT_DIGEST" \
  "$DEMO_CLIENT_CONFIG"

demo_write_active_release "$ACTIVE_SERVER" "$ACTIVE_IMAGE_REF" "$WEB_SOURCE_SHA" "$WEB_ARTIFACT_DIGEST"
RELEASE_COMMITTED=1
printf 'demo web deployed source_commit=%s artifact_digest=%s\n' "$WEB_SOURCE_SHA" "$WEB_ARTIFACT_DIGEST"
