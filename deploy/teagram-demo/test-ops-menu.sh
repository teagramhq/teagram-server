#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
WORK_DIR=$(mktemp -d /tmp/teagram-demo-ops-test.XXXXXXXX)
cleanup() {
  status=$?
  trap - EXIT HUP INT TERM
  case "$WORK_DIR" in
    /tmp/teagram-demo-ops-test.????????) rm -rf -- "$WORK_DIR" || status=1 ;;
    *) printf '%s\n' 'test refused to remove an unexpected temporary path' >&2; status=1 ;;
  esac
  exit "$status"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM

ORIGIN=$WORK_DIR/origin.git
CHECKOUT=$WORK_DIR/checkout
git init --bare --quiet "$ORIGIN"
git clone --quiet "$ORIGIN" "$CHECKOUT" 2>/dev/null
git -C "$CHECKOUT" checkout --quiet -b main
git -C "$CHECKOUT" config user.name 'Ops Menu Test'
git -C "$CHECKOUT" config user.email 'ops-menu-test@example.invalid'
mkdir -p "$CHECKOUT/deploy/teagram-demo/ops"
cat >"$CHECKOUT/deploy/teagram-demo/ops/probe.sh" <<'SH'
#!/bin/sh
set -eu
printf 'ops-script sha=%s name=%s\n' "$OPS_SHA" "$OPS_NAME"
SH
chmod 0755 "$CHECKOUT/deploy/teagram-demo/ops/probe.sh"
git -C "$CHECKOUT" add deploy/teagram-demo/ops/probe.sh
git -C "$CHECKOUT" commit --quiet -m 'test ops fixture'
git -C "$CHECKOUT" push --quiet --set-upstream origin main
MAIN_SHA=$(git -C "$CHECKOUT" rev-parse HEAD)

git -C "$CHECKOUT" checkout --quiet -b unreviewed
printf '%s\n' 'unreviewed' >"$CHECKOUT/unreviewed.txt"
git -C "$CHECKOUT" add unreviewed.txt
git -C "$CHECKOUT" commit --quiet -m 'unreviewed fixture'
UNREVIEWED_SHA=$(git -C "$CHECKOUT" rev-parse HEAD)
mkdir -p "$WORK_DIR/home"

sed \
  -e "s|^REPO=/opt/teagram-demo$|REPO=$CHECKOUT|" \
  -e "s|^HOME=/root$|HOME=$WORK_DIR/home|" \
  -e 's|^ROOT_UID=.*|ROOT_UID=0|' \
  "$SCRIPT_DIR/ops/teagram-demo-ops" >"$WORK_DIR/teagram-demo-ops"
chmod 0755 "$WORK_DIR/teagram-demo-ops"

HOSTNAME_OUTPUT=$(SSH_ORIGINAL_COMMAND=hostname "$WORK_DIR/teagram-demo-ops")
[ -n "$HOSTNAME_OUTPUT" ] || {
  printf '%s\n' 'hostname verb returned no output' >&2
  exit 1
}

RUN_OUTPUT=$(SSH_ORIGINAL_COMMAND="run $MAIN_SHA probe" "$WORK_DIR/teagram-demo-ops")
[ "$RUN_OUTPUT" = "ops-script sha=$MAIN_SHA name=probe" ] || {
  printf 'reviewed script output mismatch: %s\n' "$RUN_OUTPUT" >&2
  exit 1
}

if output=$(SSH_ORIGINAL_COMMAND="run $UNREVIEWED_SHA probe" "$WORK_DIR/teagram-demo-ops" 2>&1); then
  printf '%s\n' 'run accepted a commit outside origin/main' >&2
  exit 1
fi
case "$output" in *'not on origin/main'*) ;; *) printf '%s\n' "$output" >&2; exit 1 ;; esac

if output=$(SSH_ORIGINAL_COMMAND='bash -c id' "$WORK_DIR/teagram-demo-ops" 2>&1); then
  printf '%s\n' 'ops menu accepted an arbitrary shell command' >&2
  exit 1
fi
case "$output" in *'usage: teagram-demo-ops'*) ;; *) printf '%s\n' "$output" >&2; exit 1 ;; esac

INJECT_MARKER=$WORK_DIR/injected
if output=$(SSH_ORIGINAL_COMMAND="run $MAIN_SHA probe; touch $INJECT_MARKER" "$WORK_DIR/teagram-demo-ops" 2>&1); then
  printf '%s\n' 'ops menu accepted a command with shell syntax' >&2
  exit 1
fi
[ ! -e "$INJECT_MARKER" ] || { printf '%s\n' 'shell syntax executed through the ops menu' >&2; exit 1; }
case "$output" in *'usage: teagram-demo-ops'*) ;; *) printf '%s\n' "$output" >&2; exit 1 ;; esac

printf '%s\n' 'demo ops menu limits SSH commands and runs only executable scripts from main'
