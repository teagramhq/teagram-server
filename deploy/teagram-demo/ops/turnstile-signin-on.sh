#!/bin/sh
set -eu

[ "${OPS_NAME:-}" = turnstile-signin-on ] || {
  printf '%s\n' 'demo Turnstile rollout: unexpected operation name' >&2
  exit 1
}
exec /bin/bash /opt/teagram-demo/deploy/teagram-demo/ops/turnstile-signin-rollout.sh true
