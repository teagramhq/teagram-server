#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 2 ]; then
  printf '%s\n' 'usage: perf-scope.sh BASE_SHA HEAD_SHA' >&2
  exit 64
fi

base_sha=$1
head_sha=$2
git cat-file -e "$base_sha^{commit}" 2>/dev/null || {
  printf '%s\n' 'performance base is not a commit in this checkout' >&2
  exit 1
}
git cat-file -e "$head_sha^{commit}" 2>/dev/null || {
  printf '%s\n' 'performance head is not a commit in this checkout' >&2
  exit 1
}

if git diff --quiet "$base_sha" "$head_sha" -- \
  ':(glob)**/*.go' go.mod go.sum migrations internal/store/queries test/perf; then
  printf '%s\n' false
else
  status=$?
  [ "$status" -eq 1 ] || exit "$status"
  printf '%s\n' true
fi
