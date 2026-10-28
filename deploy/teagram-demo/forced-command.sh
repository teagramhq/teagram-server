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
elif [[ "$COMMAND" =~ ^web\ ([0-9a-f]{40})\ ([0-9a-f]{40})\ (sha256:[0-9a-f]{64})\ ([0-9a-f]{64})$ ]]; then
  ACTION=web
  readonly DEMO_SHA=${BASH_REMATCH[1]}
  readonly WEB_SOURCE_SHA=${BASH_REMATCH[2]}
  readonly WEB_ARTIFACT_DIGEST=${BASH_REMATCH[3]}
  readonly WEB_ARCHIVE_SHA256=${BASH_REMATCH[4]}
else
  echo 'demo deploy key accepts a server image release or a pinned web artifact release' >&2
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

demo_restore_legacy_edge_after_checkout_rollback() {
  local compose_file="$REPO/deploy/teagram-demo/compose.yaml"
  local legacy_config="$REPO/deploy/teagram-demo/nginx.conf"
  local mounts legacy_hash haproxy_hash
  local -a compose=(
    docker compose
    --env-file "$REPO/deploy/teagram-demo/.env"
    --project-directory "$REPO"
    --project-name teagram-demo
    --file "$compose_file"
  )
  local -a edge_ids=()

  # The first directory-mount rollout can fail after replacing edge. Restore
  # that container with the previous checkout's file-mount Compose config
  # before the directory containing its bind source is removed by git reset.
  [[ ! -e "$REPO/deploy/teagram-demo/nginx-config/nginx.conf" ]] || return 0
  [[ -f "$legacy_config" && -f "$compose_file" ]] || {
    echo 'legacy demo edge configuration is unavailable after checkout rollback' >&2
    return 1
  }
  if ! grep -Fq 'source: ./deploy/teagram-demo/nginx.conf' "$compose_file" ||
    ! grep -Fq 'target: /etc/nginx/nginx.conf' "$compose_file"; then
      echo 'restored demo Compose file does not define the supported legacy edge mount' >&2
      return 1
  fi

  mapfile -t edge_ids < <("${compose[@]}" ps -q edge)
  ((${#edge_ids[@]} == 1)) || {
    echo 'demo edge is missing or ambiguous after checkout rollback' >&2
    return 1
  }
  mounts=$(docker inspect --format '{{range .Mounts}}{{printf "%s|%s|%s\n" .Type .Source .Destination}}{{end}}' "${edge_ids[0]}") || {
    echo 'demo edge mounts could not be read after checkout rollback' >&2
    return 1
  }
  if grep -Fqx -- "bind|$REPO/deploy/teagram-demo/nginx.conf|/etc/nginx/nginx.conf" <<< "$mounts"; then
    return 0
  fi
  grep -Fqx -- "bind|$REPO/deploy/teagram-demo/nginx-config|/etc/nginx/demo" <<< "$mounts" || {
    echo 'demo edge has an unsupported config mount after checkout rollback' >&2
    return 1
  }

  legacy_hash=$(sha256sum -- "$legacy_config" | awk '{print $1}') || return 1
  haproxy_hash=$(
    sha256sum -- "$REPO/deploy/teagram-demo/haproxy.cfg" \
      "$REPO/deploy/teagram-demo/render-haproxy-config.py" |
      awk '{print $1}' | sha256sum | awk '{print $1}'
  ) || return 1
  [[ "$legacy_hash" =~ ^[0-9a-f]{64}$ ]] || {
    echo 'restored legacy edge configuration hash is invalid' >&2
    return 1
  }
  [[ "$haproxy_hash" =~ ^[0-9a-f]{64}$ ]] || {
    echo 'restored legacy HAProxy configuration hash is invalid' >&2
    return 1
  }
  TEAGRAM_DEMO_EDGE_CONFIG_SHA256="$legacy_hash" \
    TEAGRAM_DEMO_HAPROXY_CONFIG_SHA256="$haproxy_hash" "${compose[@]}" \
    up -d --no-deps --force-recreate edge || {
      echo 'demo edge could not be recreated with the restored legacy Compose file' >&2
      return 1
    }
  mapfile -t edge_ids < <("${compose[@]}" ps -q edge)
  ((${#edge_ids[@]} == 1)) || {
    echo 'restored legacy demo edge is missing or ambiguous' >&2
    return 1
  }
  mounts=$(docker inspect --format '{{range .Mounts}}{{printf "%s|%s|%s\n" .Type .Source .Destination}}{{end}}' "${edge_ids[0]}") || {
    echo 'restored legacy demo edge mounts could not be read' >&2
    return 1
  }
  grep -Fqx -- "bind|$REPO/deploy/teagram-demo/nginx.conf|/etc/nginx/nginx.conf" <<< "$mounts" || {
    echo 'restored demo edge does not have the legacy config file mount' >&2
    return 1
  }
  docker exec "${edge_ids[0]}" nginx -t >/dev/null || {
    echo 'restored legacy demo edge configuration failed its syntax check' >&2
    return 1
  }
}

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
    demo_restore_legacy_edge_after_checkout_rollback || {
      echo "deploy failed and legacy edge restore failed; requested=$SHA previous=$previous" >&2
      exit 1
    }
  fi
  exit "$deploy_rc"
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
