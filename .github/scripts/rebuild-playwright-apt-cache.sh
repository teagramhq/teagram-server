#!/usr/bin/env bash
set -Eeuo pipefail

apt_cache_dir=${1:?usage: rebuild-playwright-apt-cache.sh APT_CACHE_DIR PACKAGE_LIST}
apt_package_list=${2:?usage: rebuild-playwright-apt-cache.sh APT_CACHE_DIR PACKAGE_LIST}
github_output=${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}

skip_cache() {
  echo "::warning::$1"
  echo "cache_ready=false" >>"$github_output"
  exit 0
}

if [[ ! -s "$apt_package_list" ]]; then
  skip_cache "Playwright apt package list is missing; skipping cache publish"
fi
mapfile -t apt_packages <"$apt_package_list"
if ((${#apt_packages[@]} == 0)); then
  skip_cache "Playwright apt package list is empty; skipping cache publish"
fi

case "$apt_cache_dir" in
  /*) apt_archive_dir=$apt_cache_dir ;;
  *) apt_archive_dir="$PWD/$apt_cache_dir" ;;
esac

# This optional step runs with its own timeout, after the required install and
# verification have finished. A stale name or mirror failure must not fail the
# Playwright job or publish a partial cache.
if sudo apt-get install -y --no-install-recommends --download-only --reinstall \
  -o "Dir::Cache::archives=$apt_archive_dir" \
  "${apt_packages[@]}"; then
  :
else
  status=$?
  skip_cache "APT cache rebuild failed (exit $status); skipping cache publish"
fi

shopt -s nullglob
cached_debs=("$apt_cache_dir"/*.deb)
declare -A cached_packages=()
for archive in "${cached_debs[@]}"; do
  if ! package=$(dpkg-deb --field "$archive" Package); then
    skip_cache "could not inspect $archive; skipping cache publish"
  fi
  cached_packages["$package"]=1
done

for package in "${apt_packages[@]}"; do
  if [[ -z "${cached_packages[$package]:-}" ]]; then
    skip_cache "APT cache is missing $package; skipping cache publish"
  fi
done

sudo chown -R "$(id -u):$(id -g)" "$apt_cache_dir"
echo "cache_ready=true" >>"$github_output"
