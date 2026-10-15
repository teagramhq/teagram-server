#!/usr/bin/env bash
# Pull every external base image of the given Dockerfiles, retrying transient
# registry failures, so a later `docker build` without --pull has nothing to
# fetch.
# Usage: pull-base-images.sh <Dockerfile>...
set -euo pipefail

readonly attempts=4
readonly delay="${PULL_RETRY_DELAY_SECONDS:-15}"

[ "$#" -gt 0 ] || { echo "usage: $0 <Dockerfile>..." >&2; exit 64; }
for dockerfile in "$@"; do
  [ -f "$dockerfile" ] || { echo "::error::$dockerfile not found"; exit 1; }
done

mapfile -t images < <(awk '
  FNR == 1 { delete stages }
  toupper($1) == "FROM" {
    image = ""
    for (i = 2; i <= NF; i++) if ($i !~ /^--/) { image = $i; break }
    if (!(image in stages)) print image
    if (toupper($(NF - 1)) == "AS") stages[$NF] = 1
  }
' "$@" | sort -u)
[ "${#images[@]}" -gt 0 ] || { echo "::error::no base images found in $*"; exit 1; }

for image in "${images[@]}"; do
  for attempt in $(seq "$attempts"); do
    docker pull "$image" && continue 2
    if [ "$attempt" -eq "$attempts" ]; then
      echo "::error::pull of $image failed after $attempts attempts"
      exit 1
    fi
    echo "::warning::pull of $image failed (attempt $attempt); retrying"
    sleep $((attempt * delay))
  done
done
