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

ACTION=server
if [[ "$COMMAND" =~ ^([0-9a-f]{40})\ (sha256:[0-9a-f]{64})$ ]]; then
  readonly SHA=${BASH_REMATCH[1]}
  readonly IMAGE_DIGEST=${BASH_REMATCH[2]}
elif [[ "$COMMAND" =~ ^qa-invite\ ([0-9a-f]{40})\ (up)\ (sha256:[0-9a-f]{64})$ ]]; then
  ACTION=qa-invite
  readonly SHA=${BASH_REMATCH[1]}
  readonly QA_ACTION=${BASH_REMATCH[2]}
  readonly QA_IMAGE_DIGEST=${BASH_REMATCH[3]}
elif [[ "$COMMAND" =~ ^qa-invite\ ([0-9a-f]{40})\ (down|status)$ ]]; then
  ACTION=qa-invite
  readonly SHA=${BASH_REMATCH[1]}
  readonly QA_ACTION=${BASH_REMATCH[2]}
elif [[ "$COMMAND" =~ ^qa-invite\ ([0-9a-f]{40})\ (create-account)\ ([A-Za-z][A-Za-z0-9_]{1,31})$ ]]; then
  ACTION=qa-invite
  readonly SHA=${BASH_REMATCH[1]}
  readonly QA_ACTION=${BASH_REMATCH[2]}
  readonly QA_ACCOUNT_NAME=${BASH_REMATCH[3]}
elif [[ "$COMMAND" =~ ^web\ ([0-9a-f]{40})\ ([0-9a-f]{40})\ (sha256:[0-9a-f]{64})\ ([0-9a-f]{64})$ ]]; then
  ACTION=web
  readonly DEMO_SHA=${BASH_REMATCH[1]}
  readonly WEB_SOURCE_SHA=${BASH_REMATCH[2]}
  readonly WEB_ARTIFACT_DIGEST=${BASH_REMATCH[3]}
  readonly WEB_ARCHIVE_SHA256=${BASH_REMATCH[4]}
else
  echo 'demo deploy key accepts server/web releases or a reviewed qa-invite operation' >&2
  exit 1
fi

[[ -d "$REPO/.git" && ! -L "$REPO" ]] || { echo 'demo checkout is unavailable' >&2; exit 1; }
[[ ! -L "$REPO/.state" && ! -L "$REPO/.state/teagram-demo" ]] || {
  echo 'demo state path is unsafe' >&2
  exit 1
}
LOCK_PATH="$REPO/.state/deploy.lock"
mkdir -p "$(dirname "$LOCK_PATH")"
exec 9> "$LOCK_PATH"
flock -x 9
git -C "$REPO" diff --quiet HEAD -- || { echo 'demo checkout has tracked changes' >&2; exit 1; }
git -C "$REPO" fetch -q origin +refs/heads/main:refs/remotes/origin/main


if [[ "$ACTION" == server ]]; then
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
fi

if [[ "$ACTION" == qa-invite ]]; then
  git -C "$REPO" merge-base --is-ancestor "$SHA" origin/main || {
    echo "$SHA is not on origin/main; refusing" >&2
    exit 1
  }
  STATE_DIR=$REPO/.state/qa-invite
  RELEASES_DIR=$STATE_DIR/releases
  RELEASE_DIR=$RELEASES_DIR/$SHA
  WORK_DIR=''

  ensure_private_dir() {
    local path=$1
    [[ ! -L "$path" ]] || { echo "unsafe QA state path: $path" >&2; return 1; }
    if [[ -e "$path" ]]; then
      [[ -d "$path" && "$(stat -c '%u:%a' "$path")" == '0:700' ]] || {
        echo "QA state directory has unsafe ownership or mode: $path" >&2
        return 1
      }
    else
      install -d -o root -g root -m 0700 "$path"
      [[ -d "$path" && ! -L "$path" && "$(stat -c '%u:%a' "$path")" == '0:700' ]] || {
        echo "could not create a private QA state directory: $path" >&2
        return 1
      }
    fi
  }

  verify_release() {
    [[ -d "$RELEASE_DIR" && ! -L "$RELEASE_DIR" &&
      "$(stat -c '%u:%a' "$RELEASE_DIR")" == '0:700' &&
      -d "$RELEASE_DIR/deploy" && ! -L "$RELEASE_DIR/deploy" &&
      -d "$RELEASE_DIR/deploy/qa-invite" && ! -L "$RELEASE_DIR/deploy/qa-invite" &&
      -f "$RELEASE_DIR/deploy/qa-invite/compose.yaml" && ! -L "$RELEASE_DIR/deploy/qa-invite/compose.yaml" &&
      -f "$RELEASE_DIR/deploy/qa-invite/remote.sh" && ! -L "$RELEASE_DIR/deploy/qa-invite/remote.sh" &&
      -f "$RELEASE_DIR/deploy/qa-invite/postgresql.conf" && ! -L "$RELEASE_DIR/deploy/qa-invite/postgresql.conf" &&
      -f "$RELEASE_DIR/deploy/qa-invite/nginx.conf" && ! -L "$RELEASE_DIR/deploy/qa-invite/nginx.conf" &&
      -f "$RELEASE_DIR/deploy/qa-invite/haproxy.cfg" && ! -L "$RELEASE_DIR/deploy/qa-invite/haproxy.cfg" &&
      -d "$RELEASE_DIR/migrations" && ! -L "$RELEASE_DIR/migrations" ]]
  }

  ensure_private_dir "$STATE_DIR"
  ensure_private_dir "$RELEASES_DIR"
  # shellcheck disable=SC2016 # Expand the staging path when EXIT runs.
  trap '[[ -z "$WORK_DIR" ]] || rm -rf -- "$WORK_DIR"' EXIT
  if [[ -e "$RELEASE_DIR" || -L "$RELEASE_DIR" ]]; then
    verify_release || { echo 'existing reviewed QA snapshot is unsafe or incomplete' >&2; exit 1; }
  else
    WORK_DIR=$(mktemp -d "$RELEASES_DIR/.${SHA}.XXXXXXXX")
    git -C "$REPO" archive --format=tar "$SHA" deploy/qa-invite migrations |
      tar -xf - -C "$WORK_DIR" || {
        echo 'could not extract the reviewed QA invite project' >&2
        exit 1
      }
    chown root:root "$WORK_DIR"
    chmod 0700 "$WORK_DIR"
    [[ ! -e "$RELEASE_DIR" && ! -L "$RELEASE_DIR" ]] || {
      echo 'reviewed QA snapshot path appeared during extraction' >&2
      exit 1
    }
    mv -T -- "$WORK_DIR" "$RELEASE_DIR" || {
      echo 'could not atomically publish the reviewed QA snapshot' >&2
      exit 1
    }
    WORK_DIR=''
    verify_release || { echo 'published reviewed QA snapshot is incomplete' >&2; exit 1; }
  fi
  args=("$QA_ACTION")
  case "$QA_ACTION" in
    up) args+=("$SHA" "$QA_IMAGE_DIGEST") ;;
    create-account) args+=("$QA_ACCOUNT_NAME") ;;
  esac
  env -i PATH="$PATH" HOME=/root \
    QA_INVITE_REPO_ROOT="$RELEASE_DIR" \
    QA_INVITE_STATE_DIR="$STATE_DIR" \
    bash "$RELEASE_DIR/deploy/qa-invite/remote.sh" "${args[@]}"
  exit "$?"
fi

[[ "$(git -C "$REPO" rev-parse HEAD)" == "$DEMO_SHA" ]] || {
  echo "web release targets demo checkout $DEMO_SHA, but the live checkout changed" >&2
  exit 1
}
git -C "$REPO" merge-base --is-ancestor "$DEMO_SHA" origin/main || {
  echo "$DEMO_SHA is not on origin/main; refusing web release" >&2
  exit 1
}
python3 "$REPO/deploy/teagram-demo/resolve_web_artifact_pin.py" \
  --verify-release-identity "$WEB_SOURCE_SHA" "$WEB_ARTIFACT_DIGEST"

incoming_dir="$REPO/.state/teagram-demo/incoming"
if [[ -e "$incoming_dir" || -L "$incoming_dir" ]]; then
  [[ -d "$incoming_dir" && ! -L "$incoming_dir" && "$(stat -c '%u:%a' "$incoming_dir")" == '0:700' ]] || {
    echo 'demo web incoming directory has unsafe ownership or mode' >&2
    exit 1
  }
else
  install -d -o root -g root -m 0700 "$incoming_dir"
fi
archive="$incoming_dir/web-${WEB_SOURCE_SHA}-${WEB_ARTIFACT_DIGEST#sha256:}-$$.tar.gz"
python3 "$REPO/deploy/teagram-demo/receive-web-artifact.py" "$archive" "$WEB_ARCHIVE_SHA256"
trap 'rm -f -- "$archive"' EXIT
deploy_rc=0
DEMO_DEPLOY_LOCK_HELD=1 \
  DEPLOY_WEB_SOURCE_SHA="$WEB_SOURCE_SHA" \
  DEPLOY_WEB_ARTIFACT_DIGEST="$WEB_ARTIFACT_DIGEST" \
  DEPLOY_WEB_ARCHIVE_SHA256="$WEB_ARCHIVE_SHA256" \
  DEPLOY_WEB_ARCHIVE="$archive" \
  bash "$REPO/deploy/teagram-demo/publish-web-release.sh" || deploy_rc=$?
exit "$deploy_rc"
