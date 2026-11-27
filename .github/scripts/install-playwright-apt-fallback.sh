#!/usr/bin/env bash
set -Eeuo pipefail

apt_cache_dir=${1:?usage: install-playwright-apt-fallback.sh APT_CACHE_DIR}
script_dir=$(dirname -- "${BASH_SOURCE[0]}")

bash "$script_dir/prepare-playwright-apt-cache.sh" "$apt_cache_dir"
case "$apt_cache_dir" in
  /*) apt_archive_dir=$apt_cache_dir ;;
  *) apt_archive_dir="$PWD/$apt_cache_dir" ;;
esac
sudo apt-get update
# A failed cached dpkg install can leave the runner's package set deconfigured.
# Repair that state before resolving the dependency list against refreshed indexes.
sudo apt-get -f install -y --no-install-recommends \
  -o "Dir::Cache::archives=$apt_archive_dir"

status=0
pnpm exec playwright install-deps --dry-run chromium >deps-after-update.txt 2>&1 || status=$?
cat deps-after-update.txt
if grep -q 'All system dependencies are installed' deps-after-update.txt; then
  : >apt-install-packages.txt
elif grep -q 'Missing system dependencies' deps-after-update.txt; then
  sed -n 's/^  \([^ ]*\)$/\1/p' deps-after-update.txt | sort >apt-install-packages.txt
  if [[ ! -s apt-install-packages.txt ]]; then
    echo "::error::could not parse Playwright dependencies after apt-get update"
    exit 1
  fi
elif [[ "$status" -ne 0 ]]; then
  echo "::error::could not determine Playwright dependencies after apt-get update"
  exit "$status"
else
  echo "::error::Playwright did not report dependencies after apt-get update"
  exit 1
fi

if [[ -s apt-install-packages.txt ]]; then
  mapfile -t apt_packages <apt-install-packages.txt
  sudo apt-get install -y --no-install-recommends \
    -o "Dir::Cache::archives=$apt_archive_dir" \
    "${apt_packages[@]}"
fi

# Cache saving follows this helper, so verify the repaired package set here
# before allowing the workflow to publish it.
pnpm exec playwright install-deps --dry-run chromium
sudo chown -R "$(id -u):$(id -g)" "$apt_cache_dir"
