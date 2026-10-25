#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
npm ci --prefix "$script_dir"
python3 "$script_dir/generate.py" "$@"
