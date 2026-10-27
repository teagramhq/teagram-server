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

docker_hub_fallback() {
  case "$1" in
    *@sha256:*) return 1 ;; # docker tag cannot create a repository-digest alias.
    mirror.gcr.io/library/*) printf '%s\n' "${1#mirror.gcr.io/library/}" ;;
    mirror.gcr.io/*) printf '%s\n' "${1#mirror.gcr.io/}" ;;
    *) return 1 ;;
  esac
}

pull_with_retry() {
  local image=$1 attempt
  for attempt in $(seq "$attempts"); do
    if docker pull "$image"; then
      return 0
    fi
    if [ "$attempt" -eq "$attempts" ]; then
      return 1
    fi
    echo "::warning::pull of $image failed (attempt $attempt); retrying"
    sleep $((attempt * delay))
  done
}

for image in "${images[@]}"; do
  pull_with_retry "$image" && continue
  if fallback=$(docker_hub_fallback "$image"); then
    echo "::warning::mirror pull of $image failed; trying Docker Hub source $fallback"
    if pull_with_retry "$fallback"; then
      docker tag "$fallback" "$image" || {
        echo "::error::could not tag Docker Hub fallback $fallback as $image"
        exit 1
      }
      continue
    fi
    echo "::error::Docker Hub fallback $fallback also failed"
  fi
  echo "::error::pull of $image failed after $attempts attempts"
  exit 1
done
