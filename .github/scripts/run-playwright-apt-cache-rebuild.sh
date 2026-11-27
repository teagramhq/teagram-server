#!/usr/bin/env bash
set -Eeuo pipefail

apt_cache_dir=${1:?usage: run-playwright-apt-cache-rebuild.sh APT_CACHE_DIR PACKAGE_LIST}
apt_package_list=${2:?usage: run-playwright-apt-cache-rebuild.sh APT_CACHE_DIR PACKAGE_LIST}
github_output=${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}
script_dir=$(dirname -- "${BASH_SOURCE[0]}")

reap_rebuild() {
  local process_group=$1
  local fuser_path
  local lock_released=false
  local probe_status
  local probe_output

  # The timeout command can exit while a privileged apt-get child remains in
  # its process group. Kill that group as root before allowing later apt work.
  sudo kill -KILL -- "-$process_group" 2>/dev/null || true

  fuser_path=$(command -v fuser 2>/dev/null) || {
    echo "::error::fuser is required to release the apt cache rebuild lock"
    return 1
  }
  probe_status=0
  sudo "$fuser_path" -V >/dev/null 2>&1 || probe_status=$?
  if [[ "$probe_status" -ne 0 ]]; then
    echo "::error::fuser could not be executed to check the apt lock (exit $probe_status)"
    return 1
  fi

  for _ in $(seq 1 30); do
    probe_status=0
    probe_output=$(sudo "$fuser_path" /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock 2>&1) || probe_status=$?
    if [[ "$probe_status" -eq 0 ]]; then
      sleep 1
    elif [[ "$probe_status" -eq 1 && -z "$probe_output" ]]; then
      lock_released=true
      break
    else
      echo "::error::fuser failed while checking the apt lock (exit $probe_status): $probe_output"
      return 1
    fi
  done

  if [[ "$lock_released" != true ]]; then
    echo "::error::dpkg lock remains after the apt cache rebuild was terminated"
    return 1
  fi

  sudo dpkg --configure -a || true
}

status=0
# Keep descendants in this session's process group so a timeout cannot strand
# sudo apt-get with the dpkg lock after the unprivileged shell is stopped.
setsid timeout --foreground --signal=TERM --kill-after=10s 4m \
  bash "$script_dir/rebuild-playwright-apt-cache.sh" \
  "$apt_cache_dir" "$apt_package_list" &
rebuild_pid=$!
wait "$rebuild_pid" || status=$?
if [[ "$status" -eq 0 ]]; then
  exit 0
fi

echo "::warning::APT cache rebuild stopped (exit $status); skipping cache publish"
echo "cache_ready=false" >>"$github_output"
reap_rebuild "$rebuild_pid"
