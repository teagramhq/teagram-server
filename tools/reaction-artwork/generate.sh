#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
npm ci --prefix "$script_dir"
node "$script_dir/generate.mjs"
