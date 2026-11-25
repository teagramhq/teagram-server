#!/usr/bin/env bash
set -Eeuo pipefail

# Immutable published demo image from the successful main CI run at 609ae698.
LIMA_SERVICE_IMAGE='ghcr.io/teagramhq/teagram-server-demo@sha256:65c6c2445df3a1e9e4a11160d78c9cc45dbaf927d309262e255165e2d4ad41e6'
CONTAINER=teagramd-lima-memory
KEY_VOLUME=tg-lima-memory-key
BLOB_VOLUME=tg-lima-memory-blobs
EXPECTED_LIMIT=$((40 * 1024 * 1024))
MEMORY_CGROUP_DIR=
MEMORY_PEAK_FILE=
MEMORY_EVENTS_FILE=
IMAGE_REVISION=
MEMORY_MAX_BYTES=unknown
PROBE_PHASE=server_startup
UPLOAD_OUTCOME=not_started

finish() {
  local status=$?
  trap - EXIT
  set +e

  if [[ -r $MEMORY_PEAK_FILE ]]; then
    local peak_bytes peak_mib events oom oom_kill max_events
    peak_bytes=$(<"$MEMORY_PEAK_FILE")
    peak_mib=$(awk -v bytes="$peak_bytes" 'BEGIN { printf "%.2f", bytes / 1048576 }')
    events=$(<"$MEMORY_EVENTS_FILE")
    oom=$(awk '$1 == "oom" { print $2 }' <<<"$events")
    oom_kill=$(awk '$1 == "oom_kill" { print $2 }' <<<"$events")
    max_events=$(awk '$1 == "max" { print $2 }' <<<"$events")
    printf 'LIMA_MEMORY_PROBE result=%s phase=%s uploads=%s image=%s revision=%s peak_mib=%s peak_bytes=%s memory_max_bytes=%s max_events=%s oom=%s oom_kill=%s\n' \
      "$([[ $status -eq 0 ]] && printf passed || printf failed)" "$PROBE_PHASE" "$UPLOAD_OUTCOME" \
      "$LIMA_SERVICE_IMAGE" "$IMAGE_REVISION" "$peak_mib" "$peak_bytes" "$MEMORY_MAX_BYTES" \
      "${max_events:-unknown}" "${oom:-unknown}" "${oom_kill:-unknown}"
    if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then
      {
        printf '\n### Lima service memory probe\n\n'
        printf '| Result | Phase | Uploads | Image | Revision | Peak cgroup memory | memory.max | max events | OOM events | OOM kills |\n'
        printf '| --- | --- | --- | --- | --- | ---: | ---: | ---: | ---: | ---: |\n'
        printf '| %s | %s | %s | %s | %s | %s MiB (%s bytes) | %s bytes | %s | %s | %s |\n' \
          "$([[ $status -eq 0 ]] && printf passed || printf failed)" "$PROBE_PHASE" "$UPLOAD_OUTCOME" \
          "$LIMA_SERVICE_IMAGE" "$IMAGE_REVISION" "$peak_mib" "$peak_bytes" "$MEMORY_MAX_BYTES" \
          "${max_events:-unknown}" "${oom:-unknown}" "${oom_kill:-unknown}"
      } >>"$GITHUB_STEP_SUMMARY"
    fi
  fi

  if [[ $status -ne 0 ]]; then
    docker inspect --format 'LIMA_MEMORY_CONTAINER state={{.State.Status}} exit_code={{.State.ExitCode}} oom_killed={{.State.OOMKilled}} error={{.State.Error}}' \
      "$CONTAINER" 2>&1 || true
    docker logs "$CONTAINER" 2>&1 | tail -80
  fi
  docker rm -f "$CONTAINER" >/dev/null 2>&1
  docker volume rm "$KEY_VOLUME" "$BLOB_VOLUME" >/dev/null 2>&1
  exit "$status"
}
trap finish EXIT

if [[ $(uname -m) != x86_64 ]]; then
  echo 'Lima service memory probe requires an x86_64 runner' >&2
  exit 1
fi
docker pull "$LIMA_SERVICE_IMAGE"
image_platform=$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$LIMA_SERVICE_IMAGE")
if [[ $image_platform != linux/amd64 ]]; then
  printf 'pinned service platform is %s, want linux/amd64\n' "$image_platform" >&2
  exit 1
fi
IMAGE_REVISION=$(docker image inspect --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}' "$LIMA_SERVICE_IMAGE")
test -n "$IMAGE_REVISION"
docker volume create "$KEY_VOLUME" >/dev/null
docker volume create "$BLOB_VOLUME" >/dev/null
docker run --rm --volume "$KEY_VOLUME:/var/lib/teagramd" "$LIMA_SERVICE_IMAGE" bootstrap-identity >/dev/null

docker run --detach --name "$CONTAINER" \
  --network host \
  --memory 40m --memory-swap 40m --cpus 0.5 --pids-limit 128 \
  --read-only \
  --volume "$KEY_VOLUME:/var/lib/teagramd" \
  --volume "$BLOB_VOLUME:/var/lib/teagramd-blobs" \
  --env TG_POSTGRES_DSN --env TG_AUTHKEY_ENC_KEY \
  --env TG_PUBLIC_LINK_PREFIX=https://links.example.test/ \
  --env TG_ADVERTISE_ADDR=127.0.0.1:2443 --env TG_LISTEN_ADDR=:2443 \
  --env TG_REGISTRATION=closed --env TG_PG_MAX_CONNS=3 \
  --env GOMAXPROCS=1 --env GOMEMLIMIT=24MiB \
  "$LIMA_SERVICE_IMAGE" >/dev/null

container_pid=$(docker inspect --format '{{.State.Pid}}' "$CONTAINER")
if [[ $container_pid =~ ^[1-9][0-9]*$ ]]; then
  cgroup_path=$(awk -F: '$1 == "0" { print $3 }' "/proc/$container_pid/cgroup")
  if [[ -n $cgroup_path ]]; then
    MEMORY_CGROUP_DIR="/sys/fs/cgroup$cgroup_path"
    MEMORY_PEAK_FILE="$MEMORY_CGROUP_DIR/memory.peak"
    MEMORY_EVENTS_FILE="$MEMORY_CGROUP_DIR/memory.events"
    if [[ -r $MEMORY_PEAK_FILE && -r $MEMORY_EVENTS_FILE && -r $MEMORY_CGROUP_DIR/memory.max ]]; then
      MEMORY_MAX_BYTES=$(<"$MEMORY_CGROUP_DIR/memory.max")
    else
      echo 'service container cgroup does not expose memory.peak, memory.max, and memory.events' >&2
      exit 1
    fi
  else
    echo 'runner does not expose the service container cgroup v2 path' >&2
    exit 1
  fi
else
  echo "service container exited before measurement (pid=$container_pid)" >&2
  exit 1
fi

for attempt in $(seq 30); do
  if docker logs "$CONTAINER" 2>&1 | grep -q 'msg=listening'; then
    break
  fi
  if [[ $(docker inspect --format '{{.State.Running}}' "$CONTAINER") != true ]]; then
    echo 'pinned Lima service container exited before reaching msg=listening' >&2
    exit 1
  fi
  if [[ $attempt -eq 30 ]]; then
    echo 'pinned Lima service image did not reach msg=listening' >&2
    exit 1
  fi
  sleep 1
done

memory_limit=$(<"$MEMORY_CGROUP_DIR/memory.max")
if [[ $memory_limit != "$EXPECTED_LIMIT" ]]; then
  printf 'service memory.max is %s bytes, want %s\n' "$memory_limit" "$EXPECTED_LIMIT" >&2
  exit 1
fi

docker exec "$CONTAINER" /usr/local/bin/teagramd client-config >"$RUNNER_TEMP/lima-memory-client.json"
chmod 600 "$RUNNER_TEMP/lima-memory-client.json"
go build -o "$RUNNER_TEMP/mtproto-smoke" ./cmd/mtproto-smoke
go build -o "$RUNNER_TEMP/generate-lima-memory-photo" ./.github/scripts/lima-profile-photo-fixture
(cd "$RUNNER_TEMP" && ./generate-lima-memory-photo)

password_file="$RUNNER_TEMP/lima-memory-password"
printf 'lima-memory-probe-only-password\n' >"$password_file"
chmod 600 "$password_file"
for suffix in a b; do
  username="limamem_$suffix"
  username_file="$RUNNER_TEMP/lima-memory-username-$suffix"
  printf '%s\n' "$username" >"$username_file"
  chmod 600 "$username_file"
  docker exec --interactive --user 0 "$CONTAINER" \
    /usr/local/bin/teagramd admin create-user --username "$username" <"$password_file"
done

PROBE_PHASE=photo_client_setup
for suffix in a b; do
  "$RUNNER_TEMP/mtproto-smoke" \
    --endpoint 127.0.0.1:2443 \
    --discovery-file "$RUNNER_TEMP/lima-memory-client.json" \
    --username-file "$RUNNER_TEMP/lima-memory-username-$suffix" \
    --password-file "$password_file" \
    --profile-photo-upload-file "$RUNNER_TEMP/lima-memory-photo.jpg" \
    --profile-photo-start-gate-file "$RUNNER_TEMP/lima-memory-start-gate" \
    >"$RUNNER_TEMP/lima-memory-client-$suffix.log" 2>&1 &
  if [[ $suffix == a ]]; then
    client_a_pid=$!
  else
    client_b_pid=$!
  fi
done

gate_ready=0
for attempt in $(seq 300); do
  if grep -q 'Profile photo upload ready' "$RUNNER_TEMP/lima-memory-client-a.log" 2>/dev/null &&
    grep -q 'Profile photo upload ready' "$RUNNER_TEMP/lima-memory-client-b.log" 2>/dev/null; then
    gate_ready=1
    break
  fi
  if ! kill -0 "$client_a_pid" 2>/dev/null || ! kill -0 "$client_b_pid" 2>/dev/null; then
    cat "$RUNNER_TEMP/lima-memory-client-a.log" "$RUNNER_TEMP/lima-memory-client-b.log"
    echo 'a profile photo client exited before reaching the upload barrier' >&2
    exit 1
  fi
  sleep 0.1
done
if [[ $gate_ready -ne 1 ]]; then
  echo 'profile photo clients did not reach the upload barrier' >&2
  exit 1
fi
PROBE_PHASE=concurrent_profile_photo_uploads
touch "$RUNNER_TEMP/lima-memory-start-gate"

set +e
wait "$client_a_pid"
client_a_status=$?
wait "$client_b_pid"
client_b_status=$?
set -e
cat "$RUNNER_TEMP/lima-memory-client-a.log" "$RUNNER_TEMP/lima-memory-client-b.log"
if [[ $client_a_status -ne 0 || $client_b_status -ne 0 ]]; then
  UPLOAD_OUTCOME="failed(client_a=$client_a_status,client_b=$client_b_status)"
  printf 'concurrent profile photo uploads failed (client statuses %d, %d)\n' \
    "$client_a_status" "$client_b_status" >&2
  exit 1
fi
UPLOAD_OUTCOME=2_passed

peak_bytes=$(<"$MEMORY_PEAK_FILE")
events=$(<"$MEMORY_EVENTS_FILE")
max_events=$(awk '$1 == "max" { print $2 }' <<<"$events")
oom=$(awk '$1 == "oom" { print $2 }' <<<"$events")
oom_kill=$(awk '$1 == "oom_kill" { print $2 }' <<<"$events")
if (( peak_bytes >= EXPECTED_LIMIT )) || [[ ${max_events:-0} != 0 || ${oom:-0} != 0 || ${oom_kill:-0} != 0 ]]; then
  printf 'memory probe exceeded safe headroom: peak=%s limit=%s max=%s oom=%s oom_kill=%s\n' \
    "$peak_bytes" "$EXPECTED_LIMIT" "${max_events:-unknown}" "${oom:-unknown}" "${oom_kill:-unknown}" >&2
  exit 1
fi
