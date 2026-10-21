#!/usr/bin/env bash
# Pull external base images from Dockerfiles and explicitly named images,
# retrying transient registry failures. A base named by a build argument
# (FROM ${IMAGE}) is left to the caller, since only the build knows its value.
# Usage: pull-base-images.sh [--image <image>]... <Dockerfile>...
set -euo pipefail

readonly attempts=4
readonly delay="${PULL_RETRY_DELAY_SECONDS:-15}"

images=()
dockerfiles=()
while [ "$#" -gt 0 ]; do
  case "$1" in
    --image)
      if [ "$#" -lt 2 ] || [ -z "$2" ] || [[ "$2" == -* ]]; then
        echo "usage: $0 [--image <image>]... <Dockerfile>..." >&2
        exit 64
      fi
      images+=("$2")
      shift 2
      ;;
    --)
      shift
      dockerfiles+=("$@")
      break
      ;;
    -*)
      echo "::error::unknown option $1"
      exit 64
      ;;
    *)
      dockerfiles+=("$1")
      shift
      ;;
  esac
done

if [ "${#dockerfiles[@]}" -eq 0 ] && [ "${#images[@]}" -eq 0 ]; then
  echo "usage: $0 [--image <image>]... <Dockerfile>..." >&2
  exit 64
fi

for dockerfile in "${dockerfiles[@]}"; do
  [ -f "$dockerfile" ] || { echo "::error::$dockerfile not found"; exit 1; }
done

if [ "${#dockerfiles[@]}" -gt 0 ]; then
  mapfile -t dockerfile_images < <(awk '
    FNR == 1 { delete stages }
    toupper($1) == "FROM" {
      image = ""
      for (i = 2; i <= NF; i++) if ($i !~ /^--/) { image = $i; break }
      if (!(image in stages) && image !~ /\$/) print image
      if (toupper($(NF - 1)) == "AS") stages[$NF] = 1
  }' "${dockerfiles[@]}" | sort -u)
  images+=("${dockerfile_images[@]}")
fi

[ "${#images[@]}" -gt 0 ] || { echo "::error::no base images found in ${dockerfiles[*]}"; exit 1; }
mapfile -t images < <(printf '%s\n' "${images[@]}" | sort -u)

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
