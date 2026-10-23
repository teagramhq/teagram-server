#!/usr/bin/env bash
set -Eeuo pipefail

apt_cache_dir=${1:?usage: prepare-playwright-apt-cache.sh APT_CACHE_DIR}

# Keep complete packages from a timed-out attempt so the retry does not
# download the same files again. Apt stores incomplete downloads in partial/.
sudo rm -rf -- "$apt_cache_dir/partial"
sudo mkdir -p -- "$apt_cache_dir/partial"
sudo chown -R _apt:root "$apt_cache_dir"
