#!/usr/bin/env bash
set -Eeuo pipefail

apt_cache_dir=${1:?usage: discard-playwright-apt-cache.sh APT_CACHE_DIR}

# A failed cached install may leave stale or corrupt archives. Start the
# network repair from an empty package cache so the next saved entry is clean.
shopt -s nullglob
cached_debs=("$apt_cache_dir"/*.deb)
if ((${#cached_debs[@]})); then
  sudo rm -f -- "${cached_debs[@]}"
fi
