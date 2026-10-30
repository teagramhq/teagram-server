#!/bin/sh
# Forced command of the multica-daemon@delta key (Steward, 2026-10-26, after MAIN-2610: an agent's ad-hoc root
# script deleted /root). Agents get read-only verbs. Anything that changes this box runs a reviewed script from a
# commit on origin/main (deploy/teagram/ops/<name>.sh), the same trust model as teagram-deploy. Break-glass root is
# alpha's `incus exec teagram`, Steward or Adam only.
set -eu
set -f
CMD=${SSH_ORIGINAL_COMMAND:-}
logger -t teagram-ops "request: $CMD client=${SSH_CLIENT:-}"
# shellcheck disable=SC2086
set -- $CMD
VERB=${1:-help}
[ $# -gt 0 ] && shift
REPO=/opt/teagram

usage() {
  cat >&2 <<'EOF'
teagram-ops verbs (read-only unless noted):
  hostname
  live                      live checkout SHA, branch, dirty count, teagramd image revision and compose files
  ps                        container names, images and status
  logs <since>              teagramd logs since an RFC3339 UTC time (same as teagram-logs)
  env-names                 names of keys in /opt/teagram/.env, never values
  fingerprints              sha256 of .env, override, rollout-env record and compose files
  run <40-hex-sha> <target-failure-report|rollout-evidence|invite-web-signup-url> [args]
                            runs an allowlisted script from that commit only if
                            the commit is on origin/main
                            invite-web-signup-url default (apply) and --rollback
                            mutate the live .env; --check is read-only
EOF
}

case "$VERB" in
  hostname) hostname ;;
  live)
    printf 'live_sha=%s\n' "$(git -C "$REPO" rev-parse HEAD)"
    printf 'branch=%s\n' "$(git -C "$REPO" branch --show-current)"
    printf 'dirty_paths=%s\n' "$(git -C "$REPO" status --porcelain | wc -l | tr -d ' ')"
    printf 'image_revision=%s\n' "$(docker inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' teagram-teagramd-1)"
    printf 'compose_files=%s\n' "$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project.config_files"}}' teagram-teagramd-1)"
    printf 'container_id=%s\n' "$(docker inspect --format '{{.Id}}' teagram-teagramd-1)"
    printf 'started_at=%s\n' "$(docker inspect --format '{{.State.StartedAt}}' teagram-teagramd-1)"
    ;;
  ps) docker ps --format '{{.Names}}\t{{.Image}}\t{{.Status}}' ;;
  logs)
    [ $# -eq 1 ] || { usage; exit 64; }
    SSH_ORIGINAL_COMMAND=$1 exec /usr/local/sbin/teagram-logs
    ;;
  env-names) sed -n 's/^\([A-Za-z_][A-Za-z0-9_]*\)=.*/\1/p' "$REPO/.env" | sort -u ;;
  fingerprints)
    for f in "$REPO/.env" "$REPO/docker-compose.override.yml" "$REPO/.state/rollout-env" "$REPO/docker-compose.yml" "$REPO/docker-compose.local-blobs.yml"; do
      [ -f "$f" ] && printf '%s  %s\n' "$(sha256sum "$f" | cut -d' ' -f1)" "$f"
    done
    ;;
  run)
    [ $# -ge 2 ] || { usage; exit 64; }
    SHA=$1; NAME=$2; shift 2
    case "$SHA" in *[!0-9a-f]*|'') echo "teagram-ops: need a 40-character hex SHA" >&2; exit 64 ;; esac
    [ "${#SHA}" -eq 40 ] || { echo "teagram-ops: need a 40-character hex SHA" >&2; exit 64; }
    case "$NAME" in [a-z0-9]*) ;; *) echo "teagram-ops: bad script name" >&2; exit 64 ;; esac
    case "$NAME" in *[!a-z0-9-]*) echo "teagram-ops: bad script name" >&2; exit 64 ;; esac
    case "$NAME" in
      target-failure-report|rollout-evidence|invite-web-signup-url) ;;
      *)
        logger -t teagram-ops "refused operation: $NAME"
        echo "teagram-ops: operation is not allowlisted" >&2
        exit 64
        ;;
    esac
    git -C "$REPO" fetch -q origin main
    git -C "$REPO" merge-base --is-ancestor "$SHA" origin/main || { logger -t teagram-ops "refused: $SHA not on main"; echo "teagram-ops: $SHA is not on origin/main" >&2; exit 65; }
    P="deploy/teagram/ops/$NAME.sh"
    git -C "$REPO" cat-file -e "$SHA:$P" 2>/dev/null || { echo "teagram-ops: $P does not exist at $SHA" >&2; exit 66; }
    W=$(mktemp -d /tmp/teagram-ops.XXXXXX)
    cleanup() { case "$W" in /tmp/teagram-ops.*) rm -rf -- "$W" ;; esac; }
    trap cleanup EXIT
    git -C "$REPO" show "$SHA:$P" > "$W/op.sh"
    logger -t teagram-ops "running $P from $SHA args: $*"
    (cd /tmp && OPS_SHA=$SHA bash "$W/op.sh" "$@")
    ;;
  help|-h|--help) usage ;;
  *) usage; logger -t teagram-ops "refused verb: $VERB"; exit 64 ;;
esac
