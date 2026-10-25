#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

script_dir=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
gate_args=()
if [[ $# -eq 1 && $1 == "--local-fixture-test" ]]; then
  gate_args=(--local-fixture-test)
elif [[ $# -ne 0 ]]; then
  printf '%s\n' 'usage: test-restore-readiness-foreign-fixture.sh [--local-fixture-test]' >&2
  exit 2
fi

docker_bin=/usr/bin/docker
inventory_tool="$script_dir/test-restore-readiness-inventory.py"
inventory_snapshot=""
foreign_container_id=""
foreign_volume_name=""
foreign_volume_created_at=""
foreign_network_id=""

cleanup_foreign_fixture() {
  local status=$?
  local current_volume_identity expected_volume_identity
  trap - EXIT
  if [[ -n $foreign_container_id ]] && ! "$docker_bin" rm --force "$foreign_container_id" >/dev/null 2>&1; then
    status=1
  fi
  if [[ -n $foreign_network_id ]] && ! "$docker_bin" network rm "$foreign_network_id" >/dev/null 2>&1; then
    status=1
  fi
  if [[ -n $foreign_volume_name ]]; then
    if current_volume_identity=$("$docker_bin" volume inspect --format '{{.CreatedAt}}{{"\t"}}{{index .Labels "com.docker.compose.project"}}{{"\t"}}{{index .Labels "restore-readiness.fixture"}}' "$foreign_volume_name" 2>/dev/null) &&
      expected_volume_identity="$foreign_volume_created_at"$'\t'"$foreign_project"$'\t'foreign &&
      [[ $current_volume_identity == "$expected_volume_identity" ]]; then
      if ! "$docker_bin" volume rm "$foreign_volume_name" >/dev/null 2>&1; then status=1; fi
    else
      status=1
    fi
  fi
  if [[ -n $inventory_snapshot ]] && ! /usr/bin/rm -f -- "$inventory_snapshot"; then
    status=1
  fi
  exit "$status"
}
trap cleanup_foreign_fixture EXIT

source_image=$(/usr/bin/python3 -B "$inventory_tool" source-image)
fixture_id=$(/usr/bin/python3 -c 'import uuid; print(uuid.uuid4().hex)')
foreign_container_name="restore-readiness-foreign-${fixture_id}"
foreign_volume_candidate="restore-readiness-foreign-${fixture_id}"
foreign_network_name="restore-readiness-foreign-${fixture_id}"
foreign_project="restore-readiness-foreign-fixture-${fixture_id}"

if "$docker_bin" container inspect "$foreign_container_name" >/dev/null 2>&1 ||
  "$docker_bin" volume inspect "$foreign_volume_candidate" >/dev/null 2>&1 ||
  "$docker_bin" network inspect "$foreign_network_name" >/dev/null 2>&1; then
  printf '%s\n' 'foreign Docker fixture identity already exists' >&2
  exit 1
fi

"$docker_bin" pull "$source_image" >/dev/null
"$docker_bin" volume create \
  --label "com.docker.compose.project=$foreign_project" \
  --label "restore-readiness.fixture=foreign" \
  "$foreign_volume_candidate" >/dev/null
foreign_volume_name=$foreign_volume_candidate
volume_identity=$("$docker_bin" volume inspect --format '{{.CreatedAt}}{{"\t"}}{{index .Labels "com.docker.compose.project"}}{{"\t"}}{{index .Labels "restore-readiness.fixture"}}' "$foreign_volume_candidate")
foreign_volume_created_at=${volume_identity%%$'\t'*}
if [[ $volume_identity != "$foreign_volume_created_at"$'\t'"$foreign_project"$'\t'foreign ]]; then
  printf '%s\n' 'foreign Docker volume fixture identity mismatch' >&2
  exit 1
fi
foreign_network_id=$("$docker_bin" network create \
  --label "com.docker.compose.project=$foreign_project" \
  --label "restore-readiness.fixture=foreign" \
  "$foreign_network_name")
foreign_container_id=$("$docker_bin" run --detach \
  --name "$foreign_container_name" \
  --network "$foreign_network_id" \
  --mount "type=volume,source=$foreign_volume_name,target=/foreign" \
  --publish '127.0.0.1::5432' \
  --label "com.docker.compose.project=$foreign_project" \
  --label "restore-readiness.fixture=foreign" \
  --entrypoint /bin/sh \
  "$source_image" \
  -c 'while :; do sleep 3600; done')

inventory_snapshot=$(/usr/bin/mktemp /tmp/restore-readiness-foreign.XXXXXXXX)
if ! sudo -n -- env -i \
  HOME=/root \
  LANG=C \
  LC_ALL=C \
  PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
  PYTHONDONTWRITEBYTECODE=1 \
  /usr/bin/python3 -B "$inventory_tool" snapshot >"$inventory_snapshot"; then
  printf '%s\n' 'could not snapshot foreign Docker fixture' >&2
  exit 1
fi

gate_status=0
inventory_status=0
if timeout --signal=TERM --kill-after=15s 35m bash "$script_dir/test-restore-readiness.sh" "${gate_args[@]}"; then
  gate_status=0
else
  gate_status=$?
fi
if ! sudo -n -- env -i \
  HOME=/root \
  LANG=C \
  LC_ALL=C \
  PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
  PYTHONDONTWRITEBYTECODE=1 \
  /usr/bin/python3 -B "$inventory_tool" compare "$inventory_snapshot"; then
  inventory_status=1
fi
if (( gate_status != 0 )); then
  exit "$gate_status"
fi
exit "$inventory_status"
