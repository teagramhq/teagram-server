#!/usr/bin/env bash
set -Eeuo pipefail
umask 077
ulimit -c 0

script_dir=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# Last release before the base-schema migrations; it seeds the fixture at its own schema head.
seed_commit=7b62b9ab1cad0c459627ad7a2eb736a40382939b

emit_preflight_failure() {
  printf '%s\n' '{"result":"fail","phase":"preflight","category":"preflight_failure","migration_head":"not_reached","help_getConfig":"not_run","synthetic_signin":"not_run","cleanup":"not_started","elapsed_ms":0,"exit":1}'
  exit 1
}

if [[ $# -eq 2 && $1 == "--recover" ]]; then
  exec sudo -n -- env -i \
    HOME=/root \
    LANG=C \
    LC_ALL=C \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    PYTHONDONTWRITEBYTECODE=1 \
    /usr/bin/python3 "$script_dir/restore-readiness.py" --recover "$2"
fi

if [[ $# -ne 0 || $(id -u) -eq 0 ]]; then
  printf '%s\n' '{"result":"fail","phase":"preflight","category":"preflight_failure","migration_head":"not_reached","help_getConfig":"not_run","synthetic_signin":"not_run","cleanup":"not_started","elapsed_ms":0,"exit":1}'
  exit 1
fi

build_uid=$(id -u)
build_env=(
  env -i
  "HOME=$HOME"
  "PATH=$PATH"
  GOENV=off
  GOTOOLCHAIN=local
  GOPROXY=off
  GOSUMDB=off
  CGO_ENABLED=0
)
if ! sudo -n true >/dev/null 2>&1 || ! command -v go >/dev/null 2>&1 ||
  ! command -v docker >/dev/null 2>&1 || ! command -v atlas >/dev/null 2>&1; then
  emit_preflight_failure
fi
if ! go_arch=$("${build_env[@]}" go env GOARCH 2>/dev/null); then
  emit_preflight_failure
fi
if ! docker_bin=$(readlink -f -- "$(command -v docker)" 2>/dev/null) ||
  ! atlas_bin=$(readlink -f -- "$(command -v atlas)" 2>/dev/null); then
  emit_preflight_failure
fi
build_parent=${TMPDIR:-/tmp}
if ! build_dir=$(mktemp -d "$build_parent/teagram-restore-readiness.XXXXXXXX" 2>/dev/null); then
  emit_preflight_failure
fi

cleanup_build() {
  status=$?
  trap - EXIT
  if ! rm -rf -- "$build_dir" 2>/dev/null; then
    status=1
  fi
  exit "$status"
}
trap cleanup_build EXIT

if ! chmod 700 "$build_dir" 2>/dev/null; then
  emit_preflight_failure
fi

if ! "${build_env[@]}" go build -trimpath -buildvcs=false -o "$build_dir/teagramd" ./cmd/teagramd >/dev/null 2>&1 ||
  ! "${build_env[@]}" go build -trimpath -buildvcs=false -o "$build_dir/restore-readiness-launcher" ./deploy/teagramd/rollout-runner/restore-readiness-launcher >/dev/null 2>&1 ||
  ! "${build_env[@]}" go build -trimpath -buildvcs=false -o "$build_dir/restore-readiness-seed" ./deploy/teagramd/rollout-runner/restore-readiness-seed >/dev/null 2>&1 ||
  ! "${build_env[@]}" go build -trimpath -buildvcs=false -o "$build_dir/restore-readiness-seed-shim" ./deploy/teagramd/rollout-runner/restore-readiness-seed-shim >/dev/null 2>&1 ||
  ! "${build_env[@]}" go build -trimpath -buildvcs=false -o "$build_dir/restore-readiness-provisioner" ./deploy/teagramd/rollout-runner/restore-readiness-provisioner >/dev/null 2>&1 ||
  ! "${build_env[@]}" go build -trimpath -buildvcs=false -o "$build_dir/restore-readiness-probe" ./deploy/teagramd/rollout-runner/restore-readiness-probe >/dev/null 2>&1 ||
  ! "${build_env[@]}" go build -trimpath -buildvcs=false -o "$build_dir/restore-drill-checker" ./deploy/teagramd/rollout-runner/restore-readiness-checker >/dev/null 2>&1; then
  emit_preflight_failure
fi

if ! seed_source=$(mktemp -d "$build_parent/teagram-restore-seed.XXXXXXXX" 2>/dev/null); then
  emit_preflight_failure
fi
if ! git archive "$seed_commit" 2>/dev/null | tar -x -C "$seed_source" 2>/dev/null ||
  ! (cd "$seed_source" && "${build_env[@]}" go build -trimpath -buildvcs=false -o "$build_dir/teagramd-seed" ./cmd/teagramd >/dev/null 2>&1); then
  rm -rf -- "$seed_source"
  emit_preflight_failure
fi
if ! rm -rf -- "$seed_source"; then
  emit_preflight_failure
fi

sudo -n -- env -i \
  HOME=/root \
  LANG=C \
  LC_ALL=C \
  PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
  PYTHONDONTWRITEBYTECODE=1 \
  /usr/bin/python3 "$script_dir/restore-readiness.py" \
  --docker-bin "$docker_bin" \
  --atlas-bin "$atlas_bin" \
  --build-dir "$build_dir" \
  --build-uid "$build_uid" \
  --go-arch "$go_arch"
