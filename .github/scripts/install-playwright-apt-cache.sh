#!/usr/bin/env bash
set -Eeuo pipefail

apt_cache_dir=${1:?usage: install-playwright-apt-cache.sh APT_CACHE_DIR}
github_output=${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}

if sudo dpkg --auto-deconfigure -i "$apt_cache_dir"/*.deb &&
  sudo dpkg --configure -a &&
  pnpm exec playwright install-deps --dry-run chromium; then
  echo "installed=true" >>"$github_output"
  exit 0
fi

echo "::warning::cached debs did not satisfy the dependencies; falling back to the network install"
echo "installed=false" >>"$github_output"
