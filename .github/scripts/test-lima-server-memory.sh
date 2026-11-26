#!/usr/bin/env bash
set -Eeuo pipefail

LIMA_SERVICE_IMAGE=${LIMA_SERVICE_IMAGE:-teagramd:ci}
CONTAINER=teagramd-lima-memory
KEY_VOLUME=tg-lima-memory-key
BLOB_VOLUME=tg-lima-memory-blobs
EXPECTED_LIMIT=$((46 * 1024 * 1024))
MIN_HEADROOM=$((2 * 1024 * 1024))
LIMA_GOMEMLIMIT=18MiB
MEMORY_CGROUP_DIR=
MEMORY_PEAK_FILE=
MEMORY_EVENTS_FILE=
MEMORY_SAMPLE_PEAK_FILE=
MEMORY_SAMPLE_EVENTS_FILE=
MEMORY_SAMPLER_PID=
IMAGE_REVISION=
IMAGE_ID=
MEMORY_MAX_BYTES=unknown
MEMORY_METRICS_SOURCE=unavailable
PROBE_PHASE=server_startup
UPLOAD_OUTCOME=not_started

finish() {
  local status=$?
  trap - EXIT
  set +e

  if [[ -n $MEMORY_SAMPLER_PID ]]; then
    kill "$MEMORY_SAMPLER_PID" 2>/dev/null
    wait "$MEMORY_SAMPLER_PID" 2>/dev/null
  fi

  local peak_bytes='' events=''
  if [[ -r $MEMORY_PEAK_FILE && -r $MEMORY_EVENTS_FILE ]]; then
    peak_bytes=$(<"$MEMORY_PEAK_FILE")
    events=$(<"$MEMORY_EVENTS_FILE")
    MEMORY_METRICS_SOURCE=live_cgroup
  elif [[ -r $MEMORY_SAMPLE_PEAK_FILE && -r $MEMORY_SAMPLE_EVENTS_FILE ]]; then
    peak_bytes=$(<"$MEMORY_SAMPLE_PEAK_FILE")
    events=$(<"$MEMORY_SAMPLE_EVENTS_FILE")
    MEMORY_METRICS_SOURCE=last_sample
  fi

  if [[ -n $peak_bytes && -n $events ]]; then
    local peak_mib oom oom_kill max_events
    peak_mib=$(awk -v bytes="$peak_bytes" 'BEGIN { printf "%.2f", bytes / 1048576 }')
    oom=$(awk '$1 == "oom" { print $2 }' <<<"$events")
    oom_kill=$(awk '$1 == "oom_kill" { print $2 }' <<<"$events")
    max_events=$(awk '$1 == "max" { print $2 }' <<<"$events")
    printf 'LIMA_MEMORY_PROBE result=%s attempt=%s phase=%s uploads=%s image=%s image_id=%s revision=%s gomemlimit=%s websocket_listener=:2446 websocket_origin=https://web.teagram.io websocket_browser_only=false peak_mib=%s peak_bytes=%s memory_max_bytes=%s metrics_source=%s max_events=%s oom=%s oom_kill=%s\n' \
      "$([[ $status -eq 0 ]] && printf passed || printf failed)" "${LIMA_MEMORY_PROBE_ATTEMPT:-1}/3" "$PROBE_PHASE" "$UPLOAD_OUTCOME" \
      "$LIMA_SERVICE_IMAGE" "${IMAGE_ID:-unknown}" "$IMAGE_REVISION" "$LIMA_GOMEMLIMIT" "$peak_mib" "$peak_bytes" "$MEMORY_MAX_BYTES" "$MEMORY_METRICS_SOURCE" \
      "${max_events:-unknown}" "${oom:-unknown}" "${oom_kill:-unknown}"
    if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then
      {
        printf '\n### Lima service memory probe\n\n'
        printf '| Attempt | Result | Phase | Uploads | Image | Image ID | Revision | GOMEMLIMIT | WebSocket settings | Peak cgroup memory | memory.max | Metrics source | max events | OOM events | OOM kills |\n'
        printf '| ---: | --- | --- | --- | --- | --- | --- | --- | --- | ---: | ---: | --- | ---: | ---: | ---: |\n'
        printf '| %s/3 | %s | %s | %s | %s | %s | %s | %s | :2446, https://web.teagram.io, browser_only=false | %s MiB (%s bytes) | %s bytes | %s | %s | %s | %s |\n' \
          "${LIMA_MEMORY_PROBE_ATTEMPT:-1}" "$([[ $status -eq 0 ]] && printf passed || printf failed)" "$PROBE_PHASE" "$UPLOAD_OUTCOME" \
          "$LIMA_SERVICE_IMAGE" "${IMAGE_ID:-unknown}" "$IMAGE_REVISION" "$LIMA_GOMEMLIMIT" "$peak_mib" "$peak_bytes" "$MEMORY_MAX_BYTES" \
          "$MEMORY_METRICS_SOURCE" "${max_events:-unknown}" "${oom:-unknown}" "${oom_kill:-unknown}"
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
# Each repeated probe uses the same runner temp directory. Keep the generator's
# exclusive-create guard, but remove this attempt's previous photo fixture.
rm -f "$RUNNER_TEMP/lima-memory-start-gate" "$RUNNER_TEMP/lima-memory-photo.jpg"
if ! docker image inspect "$LIMA_SERVICE_IMAGE" >/dev/null 2>&1; then
  printf 'Lima service image is not present in the local daemon: %s\n' "$LIMA_SERVICE_IMAGE" >&2
  exit 1
fi
IMAGE_ID=$(docker image inspect --format '{{.Id}}' "$LIMA_SERVICE_IMAGE")
image_platform=$(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$IMAGE_ID")
if [[ $image_platform != linux/amd64 ]]; then
  printf 'Lima service platform is %s, want linux/amd64\n' "$image_platform" >&2
  exit 1
fi
IMAGE_REVISION=$(docker image inspect --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}' "$IMAGE_ID")
test -n "$IMAGE_REVISION"
if [[ -z ${TEAGRAMD_SOURCE_REVISION:-} ]]; then
  echo 'TEAGRAMD_SOURCE_REVISION is required to bind the probe to the workflow commit' >&2
  exit 1
fi
if [[ $IMAGE_REVISION != "$TEAGRAMD_SOURCE_REVISION" ]]; then
  printf 'Lima service image revision is %s, want %s\n' "$IMAGE_REVISION" "$TEAGRAMD_SOURCE_REVISION" >&2
  exit 1
fi
docker volume create "$KEY_VOLUME" >/dev/null
docker volume create "$BLOB_VOLUME" >/dev/null
docker run --rm --volume "$KEY_VOLUME:/var/lib/teagramd" "$IMAGE_ID" bootstrap-identity >/dev/null

docker run --detach --name "$CONTAINER" \
  --network host \
  --memory 46m --memory-swap 46m --cpus 0.5 --pids-limit 128 \
  --read-only \
  --volume "$KEY_VOLUME:/var/lib/teagramd" \
  --volume "$BLOB_VOLUME:/var/lib/teagramd-blobs" \
  --env TG_POSTGRES_DSN --env TG_AUTHKEY_ENC_KEY \
  --env TG_PUBLIC_LINK_PREFIX=https://links.example.test/ \
  --env TG_ADVERTISE_ADDR=127.0.0.1:2443 --env TG_LISTEN_ADDR=:2443 \
  --env TG_WEBSOCKET_LISTEN_ADDR=:2446 \
  --env TG_WEBSOCKET_ALLOWED_ORIGINS=https://web.teagram.io \
  --env TG_WEBSOCKET_BROWSER_ONLY=false \
  --env TG_REGISTRATION=closed --env TG_PG_MAX_CONNS=3 \
  --env GOMAXPROCS=1 --env "GOMEMLIMIT=$LIMA_GOMEMLIMIT" \
  "$IMAGE_ID" >/dev/null

# Setup CLI processes do not belong in the server's measured 46 MiB cgroup.
run_setup_command() {
  local -a docker_flags=(
    --rm
    --read-only
    --network host
    --volume "$KEY_VOLUME:/var/lib/teagramd"
    --volume "$BLOB_VOLUME:/var/lib/teagramd-blobs"
    --env TG_POSTGRES_DSN
    --env TG_AUTHKEY_ENC_KEY
    --env TG_PUBLIC_LINK_PREFIX=https://links.example.test/
    --env TG_ADVERTISE_ADDR=127.0.0.1:2443
    --env TG_LISTEN_ADDR=:2443
    --env TG_WEBSOCKET_LISTEN_ADDR=:2446
    --env TG_WEBSOCKET_ALLOWED_ORIGINS=https://web.teagram.io
    --env TG_WEBSOCKET_BROWSER_ONLY=false
    --env TG_REGISTRATION=closed
    --env TG_PG_MAX_CONNS=3
  )
  if [[ ${1:-} == --interactive ]]; then
    docker_flags+=(--interactive --user 0)
    shift
  fi
  docker run "${docker_flags[@]}" "$IMAGE_ID" "$@"
}

sample_memory_cgroup() {
  while [[ -r $MEMORY_PEAK_FILE && -r $MEMORY_EVENTS_FILE ]]; do
    cat "$MEMORY_PEAK_FILE" >"$MEMORY_SAMPLE_PEAK_FILE" || break
    cat "$MEMORY_EVENTS_FILE" >"$MEMORY_SAMPLE_EVENTS_FILE" || break
    sleep 0.05
  done
}

container_pid=$(docker inspect --format '{{.State.Pid}}' "$CONTAINER")
if [[ $container_pid =~ ^[1-9][0-9]*$ ]]; then
  cgroup_path=$(awk -F: '$1 == "0" { print $3 }' "/proc/$container_pid/cgroup")
  if [[ -n $cgroup_path ]]; then
    MEMORY_CGROUP_DIR="/sys/fs/cgroup$cgroup_path"
    MEMORY_PEAK_FILE="$MEMORY_CGROUP_DIR/memory.peak"
    MEMORY_EVENTS_FILE="$MEMORY_CGROUP_DIR/memory.events"
    MEMORY_SAMPLE_PEAK_FILE="$RUNNER_TEMP/lima-memory-sampled-peak"
    MEMORY_SAMPLE_EVENTS_FILE="$RUNNER_TEMP/lima-memory-sampled-events"
    if [[ -r $MEMORY_PEAK_FILE && -r $MEMORY_EVENTS_FILE && -r $MEMORY_CGROUP_DIR/memory.max ]]; then
      MEMORY_MAX_BYTES=$(<"$MEMORY_CGROUP_DIR/memory.max")
    else
      echo 'service container cgroup does not expose memory.peak, memory.max, and memory.events' >&2
      exit 1
    fi

    sample_memory_cgroup &
    MEMORY_SAMPLER_PID=$!
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

websocket_listener_ready=false
for attempt in $(seq 30); do
  if curl --noproxy '*' --connect-timeout 1 --max-time 2 --silent --output /dev/null \
    http://127.0.0.1:2446/apiws; then
    websocket_listener_ready=true
    break
  fi
  sleep 1
done
if [[ $websocket_listener_ready != true ]]; then
  echo 'Lima WebSocket listener did not accept an HTTP connection on :2446' >&2
  exit 1
fi

run_setup_command client-config >"$RUNNER_TEMP/lima-memory-client.json"
chmod 600 "$RUNNER_TEMP/lima-memory-client.json"
go build -o "$RUNNER_TEMP/mtproto-smoke" ./cmd/mtproto-smoke
go build -o "$RUNNER_TEMP/generate-lima-memory-photo" ./.github/scripts/lima-profile-photo-fixture
(cd "$RUNNER_TEMP" && ./generate-lima-memory-photo)

password_file="$RUNNER_TEMP/lima-memory-password"
printf 'lima-memory-probe-only-password\n' >"$password_file"
chmod 600 "$password_file"
for suffix in a b; do
  # The three attempts share a PostgreSQL service, so each needs fresh users.
  username="limamem_${LIMA_MEMORY_PROBE_ATTEMPT:-1}_$suffix"
  username_file="$RUNNER_TEMP/lima-memory-username-$suffix"
  printf '%s\n' "$username" >"$username_file"
  chmod 600 "$username_file"
  run_setup_command --interactive admin create-user --username "$username" <"$password_file"
done

PROBE_PHASE=photo_client_setup
"$RUNNER_TEMP/mtproto-smoke" \
  --endpoint 127.0.0.1:2443 \
  --discovery-file "$RUNNER_TEMP/lima-memory-client.json" \
  --username-file "$RUNNER_TEMP/lima-memory-username-a" \
  --password-file "$password_file" \
  --profile-photo-upload-file "$RUNNER_TEMP/lima-memory-photo.jpg" \
  --profile-photo-start-gate-file "$RUNNER_TEMP/lima-memory-start-gate" \
  >"$RUNNER_TEMP/lima-memory-client-a.log" 2>&1 &
client_a_pid=$!

"$RUNNER_TEMP/mtproto-smoke" \
  --endpoint 127.0.0.1:2443 \
  --discovery-file "$RUNNER_TEMP/lima-memory-client.json" \
  --username-file "$RUNNER_TEMP/lima-memory-username-b" \
  --password-file "$password_file" \
  --media-photo-upload-file "$RUNNER_TEMP/lima-memory-photo.jpg" \
  --media-photo-start-gate-file "$RUNNER_TEMP/lima-memory-start-gate" \
  >"$RUNNER_TEMP/lima-memory-client-media.log" 2>&1 &
media_client_pid=$!

gate_ready=0
for attempt in $(seq 300); do
  if grep -q 'Profile photo upload ready' "$RUNNER_TEMP/lima-memory-client-a.log" 2>/dev/null &&
    grep -q 'Media photo upload ready' "$RUNNER_TEMP/lima-memory-client-media.log" 2>/dev/null; then
    gate_ready=1
    break
  fi
  if ! kill -0 "$client_a_pid" 2>/dev/null || ! kill -0 "$media_client_pid" 2>/dev/null; then
    cat "$RUNNER_TEMP/lima-memory-client-a.log" "$RUNNER_TEMP/lima-memory-client-media.log"
    echo 'a photo client exited before reaching the upload barrier' >&2
    exit 1
  fi
  sleep 0.1
done
if [[ $gate_ready -ne 1 ]]; then
  echo 'profile and media photo clients did not reach the upload barrier' >&2
  exit 1
fi
PROBE_PHASE=concurrent_profile_and_media_photo_uploads
touch "$RUNNER_TEMP/lima-memory-start-gate"

set +e
wait "$client_a_pid"
client_a_status=$?
wait "$media_client_pid"
media_client_status=$?
set -e
cat "$RUNNER_TEMP/lima-memory-client-a.log" "$RUNNER_TEMP/lima-memory-client-media.log"
if [[ $client_a_status -ne 0 || $media_client_status -ne 0 ]]; then
  UPLOAD_OUTCOME="failed(profile=$client_a_status,media_photo=$media_client_status)"
  printf 'concurrent profile and media photo uploads failed (client statuses %d, %d)\n' \
    "$client_a_status" "$media_client_status" >&2
  exit 1
fi
UPLOAD_OUTCOME=profile_passed,media_photo_passed

peak_bytes=$(<"$MEMORY_PEAK_FILE")
events=$(<"$MEMORY_EVENTS_FILE")
max_events=$(awk '$1 == "max" { print $2 }' <<<"$events")
oom=$(awk '$1 == "oom" { print $2 }' <<<"$events")
oom_kill=$(awk '$1 == "oom_kill" { print $2 }' <<<"$events")
if (( peak_bytes > EXPECTED_LIMIT - MIN_HEADROOM )); then
  printf 'memory probe did not retain 2 MiB headroom: peak=%s limit=%s max=%s oom=%s oom_kill=%s\n' \
    "$peak_bytes" "$EXPECTED_LIMIT" "${max_events:-unknown}" "${oom:-unknown}" "${oom_kill:-unknown}" >&2
  exit 1
fi
if [[ ${max_events:-0} != 0 || ${oom:-0} != 0 || ${oom_kill:-0} != 0 ]]; then
  printf 'memory probe recorded cgroup pressure: peak=%s max=%s oom=%s oom_kill=%s\n' \
    "$peak_bytes" "${max_events:-unknown}" "${oom:-unknown}" "${oom_kill:-unknown}" >&2
  exit 1
fi
