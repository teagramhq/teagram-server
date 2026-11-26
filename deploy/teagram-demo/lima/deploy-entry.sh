#!/bin/sh
set -eu

PATH=/usr/sbin:/usr/bin:/sbin:/bin
export PATH

if [ "$(id -u)" -eq 0 ]; then
  echo 'deploy entry must run as teagram-deploy' >&2
  exit 1
fi

exec sudo -n /usr/local/sbin/teagram-demo-deploy "${SSH_ORIGINAL_COMMAND:-}"
