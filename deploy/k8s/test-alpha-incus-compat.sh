#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"

if grep -En '/dev/stdin|incus exec .*-- command -v' \
  "$repo_root/deploy/k8s/bootstrap-alpha.sh"; then
  printf 'alpha bootstrap still relies on an unsupported Incus command form\n' >&2
  exit 1
fi
if grep -En 'mktemp.*XXXXXX\.[[:alpha:]]' "$repo_root/deploy/k8s/bootstrap-alpha.sh"; then
  printf 'alpha bootstrap places a suffix after BusyBox mktemp XXXXXX\n' >&2
  exit 1
fi
grep --fixed-strings --quiet 'alpha_install_remote_file' \
  "$repo_root/deploy/k8s/bootstrap-alpha.sh"
grep --fixed-strings --quiet 'alpha_ensure_sshd_runtime_dir' \
  "$repo_root/deploy/k8s/bootstrap-alpha.sh"

readonly TEST_IMAGE=debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc
container_output=$(docker run --rm -i \
  --volume "$repo_root/deploy/k8s/alpha-incus.sh:/alpha-incus.sh:ro" \
  --env DEBIAN_FRONTEND=noninteractive \
  "$TEST_IMAGE" bash -s <<'CONTAINER_TEST'
set -euo pipefail
apt-get update -qq
apt-get install -y -qq --no-install-recommends openssh-server busybox-static util-linux >/dev/null
ssh-keygen -A >/dev/null
source /alpha-incus.sh

# Model the target LXC's denied reopen: fd 0 remains usable, while opening the
# /dev/stdin pathname fails even for the LXC's mapped root user.
rm -f /dev/stdin
: > /dev/stdin
chmod 000 /dev/stdin

mkdir -p /tmp/busybox-bin
ln -s /usr/bin/busybox /tmp/busybox-bin/mktemp
PATH=/tmp/busybox-bin:$PATH
export PATH
if mktemp /tmp/alpha-invalid.XXXXXX.yaml >/dev/null 2>&1; then
  printf 'BusyBox mktemp unexpectedly accepted a suffix after XXXXXX\n' >&2
  exit 1
fi
temporary=$(alpha_mktemp_file /tmp alpha-mktemp-check)
[[ -f $temporary ]]
rm -f -- "$temporary"

test_instance=alpha-compat
incus() {
  [[ ${1:-} == exec ]] || return 2
  shift
  while [[ ${1:-} == --mode=* ]]; do shift; done
  shift
  [[ ${1:-} == -- ]] || return 2
  shift
  # setpriv removes DAC_OVERRIDE to reproduce the path access denial from an
  # unprivileged LXC. The command still receives the actual inherited stdin.
  setpriv --bounding-set=-dac_override "$@"
}

if printf 'unreadable-by-path' | incus exec "$test_instance" -- \
  install -D -o root -g root -m 0640 /dev/stdin /tmp/old-transfer; then
  printf 'opening /dev/stdin unexpectedly worked in the unprivileged LXC fixture\n' >&2
  exit 1
fi

printf 'transferred over inherited stdin' > /tmp/alpha-transfer-source
alpha_install_remote_file "$test_instance" /tmp/alpha-compat/config 0640 \
  < /tmp/alpha-transfer-source
[[ $(cat /tmp/alpha-compat/config) == 'transferred over inherited stdin' ]]
[[ $(stat -c '%a:%u:%g' /tmp/alpha-compat/config) == 640:0:0 ]]
chmod 0700 /tmp/alpha-compat
alpha_install_remote_file "$test_instance" /tmp/alpha-compat/config 0640 \
  < /tmp/alpha-transfer-source
[[ $(stat -c '%a:%u:%g' /tmp/alpha-compat) == 700:0:0 ]]
alpha_remote_file_matches "$test_instance" /tmp/alpha-compat/config \
  /tmp/alpha-transfer-source
printf 'different input' > /tmp/alpha-transfer-source
if alpha_remote_file_matches "$test_instance" /tmp/alpha-compat/config \
  /tmp/alpha-transfer-source; then
  printf 'remote file comparison accepted different stdin\n' >&2
  exit 1
fi
if incus exec "$test_instance" -- sshd -t >/tmp/sshd-before.log 2>&1; then
  printf 'sshd -t unexpectedly passed without /run/sshd\n' >&2
  exit 1
fi
grep --fixed-strings --quiet 'Missing privilege separation directory: /run/sshd' \
  /tmp/sshd-before.log
alpha_ensure_sshd_runtime_dir "$test_instance"
[[ $(stat -c '%a:%u:%g' /run/sshd) == 755:0:0 ]]
incus exec "$test_instance" -- sshd -t
printf 'alpha Incus compatibility test passed\n'
CONTAINER_TEST
)
[[ $container_output == *'alpha Incus compatibility test passed'* ]]
