#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -P "$SCRIPT_DIR/../../.." && pwd)
# shellcheck source=deploy/teagram-demo/demo-common.sh
source "$SCRIPT_DIR/../demo-common.sh"
# shellcheck source=deploy/teagram-demo/image-retention.sh
source "$SCRIPT_DIR/../image-retention.sh"

readonly SHA=${DEPLOY_SHA:-}
readonly IMAGE_DIGEST=${DEPLOY_IMAGE_DIGEST:-}
readonly PREV_SHA=${PREVIOUS_SHA:-}
readonly ENV_FILE=${TEAGRAM_DEMO_LIMA_ENV_FILE:-/var/lib/teagram-demo/lima.env}
readonly HEALTH_URL=http://127.0.0.1:8080/healthz
readonly SERVER_IMAGE_REPOSITORY=$DEMO_IMAGE_REPOSITORY
ACTIVE_SERVER_ID=
PREV_IMAGE_ID=
PREV_IMAGE_REVISION=
PREV_IMAGE_REF=
CANDIDATE_IMAGE_ID=
CANDIDATE_IMAGE_REF=
BACKUP_DIR=
CANDIDATE_STARTED=0
OLD_SERVER_STOPPED=0
DEPLOY_COMMITTED=0
HISTORY_READY=0
ACTIVE_RELEASE_UPDATED=0

lima_compose() {
  local image_ref=$1
  shift
  env -i PATH="$PATH" HOME=/root TEAGRAM_DEMO_IMAGE_REF="$image_ref" \
    docker compose --env-file "$ENV_FILE" --project-directory "$REPO_ROOT" \
      --file "$SCRIPT_DIR/compose.yaml" --project-name teagram-demo "$@" </dev/null
}

lima_wait_health() {
  local attempt
  for ((attempt = 0; attempt < 60; attempt++)); do
    if wget -q -O /dev/null "$HEALTH_URL"; then
      return 0
    fi
    sleep 1
  done
  return 1
}

lima_backup_database() {
  local dump="$BACKUP_DIR/postgres.dump"
  lima_compose "$PREV_IMAGE_REF" exec -T postgres pg_dump -U postgres -Fc teagram > "$dump" || {
    demo_fail 'lima database dump failed before deployment'
    return 1
  }
  chown root:root "$dump"
  chmod 0600 "$dump"
  [[ -s "$dump" ]] || { demo_fail 'lima database dump is empty'; return 1; }
  lima_compose "$PREV_IMAGE_REF" exec -T postgres pg_restore --list < "$dump" > /dev/null || {
    demo_fail 'lima database dump failed its restore-list check'
    return 1
  }
  sha256sum "$dump" > "$BACKUP_DIR/postgres.dump.sha256"
  chown root:root "$BACKUP_DIR/postgres.dump.sha256"
  chmod 0600 "$BACKUP_DIR/postgres.dump.sha256"
}

lima_cleanup() {
  local rc=$? cleanup_failed=0 rollback_failed=0
  trap - EXIT
  if (( rc != 0 && OLD_SERVER_STOPPED == 1 && DEPLOY_COMMITTED == 0 )); then
    if (( CANDIDATE_STARTED == 1 )); then
      lima_compose "$CANDIDATE_IMAGE_REF" stop teagramd || cleanup_failed=1
      lima_compose "$CANDIDATE_IMAGE_REF" rm -f teagramd || cleanup_failed=1
    fi
    if (( ACTIVE_RELEASE_UPDATED == 1 )); then
      if [[ -n "$PREV_WEB_RELEASE" ]]; then
        demo_write_active_release teagramd "$PREV_IMAGE_REF" \
          "${PREV_WEB_RELEASE%% *}" "sha256:${PREV_WEB_RELEASE#* }" || cleanup_failed=1
      else
        demo_write_active_release teagramd "$PREV_IMAGE_REF" || cleanup_failed=1
      fi
    fi
    if (( HISTORY_READY == 1 )); then
      demo_prune_images "$PREV_IMAGE_ID" || cleanup_failed=1
    elif [[ -n "$CANDIDATE_IMAGE_ID" && "$CANDIDATE_IMAGE_ID" != "$PREV_IMAGE_ID" ]]; then
      docker image rm "$CANDIDATE_IMAGE_ID" || {
        printf 'could not remove failed lima candidate image %s\n' "$CANDIDATE_IMAGE_ID" >&2
        cleanup_failed=1
      }
    fi
    if ! lima_compose "$PREV_IMAGE_REF" up -d --no-deps --force-recreate \
      --wait --wait-timeout 120 teagramd || ! lima_wait_health; then
      rollback_failed=1
      printf 'lima rollback health failed requested_revision=%s previous_revision=%s; database was not restored or changed during rollback\n' \
        "$SHA" "$PREV_IMAGE_REVISION" >&2
    else
      printf 'lima rollout rolled back to revision=%s after requested_revision=%s failed; database backup=%s\n' \
        "$PREV_IMAGE_REVISION" "$SHA" "$BACKUP_DIR" >&2
    fi
    if (( rollback_failed == 0 && HISTORY_READY == 1 )); then
      demo_prune_images "$PREV_IMAGE_ID" || cleanup_failed=1
    fi
  elif (( rc != 0 && HISTORY_READY == 1 )); then
    demo_prune_images "$PREV_IMAGE_ID" || cleanup_failed=1
  fi
  if (( cleanup_failed != 0 )); then
    rc=1
  fi
  exit "$rc"
}
trap lima_cleanup EXIT

demo_require_root
[[ "$SHA" =~ ^[0-9a-f]{40}$ ]] || demo_fail 'DEPLOY_SHA must be a full 40-hex commit'
[[ "$IMAGE_DIGEST" =~ ^sha256:[0-9a-f]{64}$ ]] || demo_fail 'DEPLOY_IMAGE_DIGEST must be a trusted sha256 digest'
[[ "$PREV_SHA" =~ ^[0-9a-f]{40}$ ]] || demo_fail 'PREVIOUS_SHA must be a full 40-hex commit'
[[ -d "$REPO_ROOT/.git" && ! -L "$REPO_ROOT" ]] || demo_fail 'lima demo checkout is unavailable'
[[ "$(stat -c '%u:%a' "$REPO_ROOT")" == '0:755' || "$(stat -c '%u:%a' "$REPO_ROOT")" == '0:750' ]] ||
  demo_fail 'lima demo checkout must be root-owned and not writable by the deploy user'
[[ "$(git -C "$REPO_ROOT" rev-parse HEAD)" == "$SHA" ]] || demo_fail 'checkout is not the requested deployment SHA'
git -C "$REPO_ROOT" fetch -q origin +refs/heads/main:refs/remotes/origin/main
git -C "$REPO_ROOT" merge-base --is-ancestor "$SHA" origin/main || demo_fail 'deployment SHA is no longer on origin/main'
git -C "$REPO_ROOT" diff --quiet HEAD -- || demo_fail 'lima demo checkout has tracked changes'

if [[ "${DEMO_DEPLOY_LOCK_HELD:-0}" != 1 ]]; then
  LOCK_PATH="$REPO_ROOT/.state/deploy.lock"
  [[ ! -L "$REPO_ROOT/.state" && ! -L "$LOCK_PATH" ]] || demo_fail 'lima deploy state path is unsafe'
  mkdir -p "$(dirname "$LOCK_PATH")"
  exec 9> "$LOCK_PATH"
  flock -x 9
fi

"$SCRIPT_DIR/check.sh"
if [[ -e "$DEMO_ACTIVE_RELEASE_FILE" || -L "$DEMO_ACTIVE_RELEASE_FILE" ]]; then
  [[ "$(jq -er '.server' "$DEMO_ACTIVE_RELEASE_FILE")" == teagramd ]] ||
    demo_fail 'lima active release must name the single teagramd service'
  PREV_IMAGE_REF=$(demo_active_image_ref)
else
  mapfile -t image_refs < <(sed -n 's/^TEAGRAM_DEMO_IMAGE_REF=//p' "$ENV_FILE")
  ((${#image_refs[@]} == 1)) || demo_fail 'lima env file must have one image reference'
  PREV_IMAGE_REF=${image_refs[0]}
fi
demo_valid_image_ref "$PREV_IMAGE_REF" || demo_fail 'active lima image reference is invalid'

mapfile -t server_ids < <(lima_compose "$PREV_IMAGE_REF" ps --status running -q teagramd)
((${#server_ids[@]} == 1)) || demo_fail 'running lima teagramd is missing or ambiguous'
ACTIVE_SERVER_ID=${server_ids[0]}
PREV_IMAGE_ID=$(docker inspect --format '{{.Image}}' "$ACTIVE_SERVER_ID")
[[ "$PREV_IMAGE_ID" =~ ^sha256:[0-9a-f]{64}$ ]] || demo_fail 'running lima image ID is invalid'
PREV_IMAGE_REVISION=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$PREV_IMAGE_ID")
[[ "$PREV_IMAGE_REVISION" =~ ^[0-9a-f]{40}$ ]] || demo_fail 'running lima image revision label is invalid'
[[ "$(docker image inspect --format '{{.Id}}' "$PREV_IMAGE_REF")" == "$PREV_IMAGE_ID" ]] ||
  demo_fail 'active lima image reference does not match the running server'
[[ "$(docker inspect --format '{{.State.Health.Status}}' "$ACTIVE_SERVER_ID")" == healthy ]] ||
  demo_fail 'running lima server is not healthy before deployment'
wget -q -O /dev/null "$HEALTH_URL" || demo_fail 'lima /healthz is not passing before deployment'
mapfile -t postgres_ids < <(lima_compose "$PREV_IMAGE_REF" ps --status running -q postgres)
((${#postgres_ids[@]} == 1)) || demo_fail 'running lima postgres is missing or ambiguous'
[[ "$(docker inspect --format '{{.State.Health.Status}}' "${postgres_ids[0]}")" == healthy ]] ||
  demo_fail 'running lima postgres is not healthy before deployment'

demo_initialize_image_history "$PREV_IMAGE_REVISION" "$PREV_IMAGE_ID" "$PREV_IMAGE_REF"
HISTORY_READY=1
PREV_WEB_RELEASE=$(demo_active_web_release)
TS=$(date -u +%Y%m%dT%H%M%SZ)
BACKUP_DIR="$DEMO_STATE_DIR/backups/lima-deploy-${SHA:0:12}-$TS"
install -d -o root -g root -m 0700 "$DEMO_STATE_DIR/backups" "$BACKUP_DIR"
[[ "$(stat -c '%u:%a' "$BACKUP_DIR")" == '0:700' ]] || demo_fail 'lima backup directory must be root-owned mode 0700'
lima_backup_database

# Pull and verify the new image only after the read-only preflight, old-image
# rollback check, health checks, and verified database backup have succeeded.
CANDIDATE_IMAGE_REF=$(demo_pull_image_by_sha "$SHA" "$IMAGE_DIGEST")
CANDIDATE_IMAGE_ID=$(docker image inspect --format '{{.Id}}' "$CANDIDATE_IMAGE_REF")
[[ "$CANDIDATE_IMAGE_ID" =~ ^sha256:[0-9a-f]{64}$ ]] || demo_fail 'lima candidate image ID is invalid'

OLD_SERVER_STOPPED=1
lima_compose "$PREV_IMAGE_REF" stop teagramd
lima_compose "$PREV_IMAGE_REF" rm -f teagramd

# Keep migrations separate from rollback. If the candidate fails, rollback
# changes only the server image and never restores or otherwise touches data.
lima_compose "$CANDIDATE_IMAGE_REF" run --rm -T --no-deps migrate
CANDIDATE_STARTED=1
lima_compose "$CANDIDATE_IMAGE_REF" up -d --no-deps --force-recreate \
  --wait --wait-timeout 120 teagramd
lima_wait_health || demo_fail 'new lima server did not pass /healthz within 60 seconds'
mapfile -t candidate_ids < <(lima_compose "$CANDIDATE_IMAGE_REF" ps --status running -q teagramd)
((${#candidate_ids[@]} == 1)) || demo_fail 'new lima server is missing or ambiguous after health check'
CANDIDATE_RUNNING_IMAGE_ID=$(docker inspect --format '{{.Image}}' "${candidate_ids[0]}")
[[ "$CANDIDATE_RUNNING_IMAGE_ID" == "$CANDIDATE_IMAGE_ID" ]] || demo_fail 'running lima server image differs from the verified candidate'
[[ "$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$CANDIDATE_RUNNING_IMAGE_ID")" == "$SHA" ]] ||
  demo_fail 'running lima server revision does not match the requested SHA'

if [[ -n "$PREV_WEB_RELEASE" ]]; then
  demo_write_active_release teagramd "$CANDIDATE_IMAGE_REF" \
    "${PREV_WEB_RELEASE%% *}" "sha256:${PREV_WEB_RELEASE#* }"
else
  demo_write_active_release teagramd "$CANDIDATE_IMAGE_REF"
fi
ACTIVE_RELEASE_UPDATED=1
demo_record_successful_image "$SHA" "$CANDIDATE_IMAGE_ID" "$CANDIDATE_IMAGE_REF" \
  "$PREV_IMAGE_REVISION" "$PREV_IMAGE_ID" "$PREV_IMAGE_REF"
DEPLOY_COMMITTED=1
demo_prune_images "$CANDIDATE_IMAGE_ID"
printf 'lima demo deployed revision=%s previous_revision=%s backup=%s healthz=passed\n' \
  "$SHA" "$PREV_IMAGE_REVISION" "$BACKUP_DIR"
