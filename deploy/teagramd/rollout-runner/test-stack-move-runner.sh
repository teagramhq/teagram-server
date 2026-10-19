#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
HELPER="$SCRIPT_DIR/stack-move-runner.py"
TMP=$(mktemp -d "${TMPDIR:-/tmp}/stack-move-runner.XXXXXXXX")
chmod 700 "$TMP"
SOURCE_DIR="$TMP/source"
TARGET_DIR="$TMP/target"
EVIDENCE_DIR="$TMP/evidence"
mkdir -m 700 "$SOURCE_DIR" "$TARGET_DIR" "$EVIDENCE_DIR"
SOURCE_ENV="$SOURCE_DIR/.env"
TARGET_ENV="$TARGET_DIR/.env"
SOURCE_COMPOSE="$SOURCE_DIR/docker-compose.yml"
SOURCE_OVERRIDE="$SOURCE_DIR/docker-compose.override.yml"
TARGET_COMPOSE="$TARGET_DIR/docker-compose.yml"
SOURCE_PROJECT=telegram-server
TARGET_PROJECT=teagram
SOURCE_VOLUMES=(telegram-server_pgdata telegram-server_tgkey telegram-server_tgblobs telegram-server_rustfsdata)
TARGET_VOLUMES=(teagram_pgdata teagram_tgkey teagram_tgblobs teagram_rustfsdata)
BUSYBOX_IMAGE=ghcr.io/teagramhq/teagram-server-busybox:1.37
BUSYBOX_IMAGE_DIGEST=sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e
SOURCE_POSTGRES_IMAGE=ghcr.io/teagramhq/teagram-server-postgres:16-alpine
SOURCE_POSTGRES_IMAGE_DIGEST=sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea
TARGET_POSTGRES_IMAGE=ghcr.io/teagramhq/teagram-server-postgres:18-alpine
TARGET_POSTGRES_IMAGE_DIGEST=sha256:77f585114c32fbca283dc835b0596f4e52b51b4c6662d7810b2f4084f60a1873
RUSTFS_IMAGE=ghcr.io/rustfs/rustfs@sha256:1803faef57627e2d9c2e7d89d655d712ddded5389040054987163043fecb6a3c
RUSTFS_ROOT_ACCESS_KEY=fixture-root-access
RUSTFS_ROOT_SECRET_KEY=fixture-root-secret
TARGET_SHA=1234567890abcdef1234567890abcdef12345678
SERVER_IMAGE_BACKUP=
CLEANUP_FIXTURE_PROJECTS=0
CLEANUP_FIXTURE_VOLUMES=0
CLEANUP_EXTERNAL_WRITER=0
CLEANUP_TARGET_IMAGE=0

CLEANUP_SERVER_IMAGE=0

fail() {
  printf 'stack move fixture failed: %s\n' "$1" >&2
  exit 1
}

command -v aws >/dev/null || fail 'AWS CLI is unavailable on the runner'
cat > "$TMP/aws-config" <<'AWS_CONFIG'
[default]
region = us-east-1
s3 =
    addressing_style = path
AWS_CONFIG
chmod 600 "$TMP/aws-config"

container_ids() {
  docker ps -aq --filter "label=com.docker.compose.project=$1"
}

volume_exists() {
  docker volume inspect "$1" >/dev/null 2>&1
}

cleanup() {
  local status=$? name project
  trap - EXIT
  if [ "$CLEANUP_EXTERNAL_WRITER" = 1 ]; then
    docker rm -f main1974-stack-move-external-writer >/dev/null 2>&1 || true
  fi
  if [ "$CLEANUP_FIXTURE_PROJECTS" = 1 ]; then
    for project in "$SOURCE_PROJECT" "$TARGET_PROJECT" unexpected-source; do
      docker compose --project-name "$project" --project-directory "$SOURCE_DIR" \
        --env-file "$SOURCE_ENV" --file "$SOURCE_COMPOSE" down --remove-orphans >/dev/null 2>&1 || true
      docker compose --project-name "$project" --project-directory "$TARGET_DIR" \
        --env-file "$TARGET_ENV" --file "$TARGET_COMPOSE" down --remove-orphans >/dev/null 2>&1 || true
    done
  fi
  if [ "$CLEANUP_FIXTURE_VOLUMES" = 1 ]; then
    for name in "${TARGET_VOLUMES[@]}" "${SOURCE_VOLUMES[@]}"; do
      if volume_exists "$name"; then docker volume rm "$name" >/dev/null 2>&1 || status=1; fi
    done
  fi
  if [ "$CLEANUP_SERVER_IMAGE" = 1 ]; then
    if [ -n "$SERVER_IMAGE_BACKUP" ]; then
      docker image tag "$SERVER_IMAGE_BACKUP" telegramd:local >/dev/null 2>&1 || status=1
    else
      docker image rm telegramd:local >/dev/null 2>&1 || true
    fi
  fi
  if [ "$CLEANUP_TARGET_IMAGE" = 1 ]; then
    docker image rm teagramd:local >/dev/null 2>&1 || true
  fi
  rm -rf -- "$TMP"
  exit "$status"
}
trap cleanup EXIT

python3 -B "$SCRIPT_DIR/test-stack-move-runner-timeout.py"

command -v docker >/dev/null || fail 'docker is unavailable'
docker compose version >/dev/null 2>&1 || fail 'Docker Compose v2 is unavailable'
python3 -B "$SCRIPT_DIR/test-stack-move-runner-inspect.py"
for project in "$SOURCE_PROJECT" "$TARGET_PROJECT"; do
  [ -z "$(container_ids "$project")" ] || fail "project $project already has containers"
done
for project in unexpected-source; do
  [ -z "$(container_ids "$project")" ] || fail "project $project already has containers"
done
! docker inspect --type container main1974-stack-move-external-writer >/dev/null 2>&1 || \
  fail 'fixture external-writer container name is already in use'
for name in "${SOURCE_VOLUMES[@]}" "${TARGET_VOLUMES[@]}"; do
  ! volume_exists "$name" || fail "volume $name already exists"
done
CLEANUP_FIXTURE_PROJECTS=1
CLEANUP_FIXTURE_VOLUMES=1
if docker image inspect telegramd:local >/dev/null 2>&1; then
  SERVER_IMAGE_BACKUP=$(docker image inspect --format '{{.Id}}' telegramd:local)
fi
if docker image inspect teagramd:local >/dev/null 2>&1; then
  fail 'teagramd:local already exists; refusing to replace a host image'
fi
CLEANUP_TARGET_IMAGE=1
CLEANUP_EXTERNAL_WRITER=1
CLEANUP_SERVER_IMAGE=1

docker pull "$BUSYBOX_IMAGE@$BUSYBOX_IMAGE_DIGEST" >/dev/null
docker pull "$SOURCE_POSTGRES_IMAGE@$SOURCE_POSTGRES_IMAGE_DIGEST" >/dev/null
docker pull "$RUSTFS_IMAGE" >/dev/null
docker pull "$TARGET_POSTGRES_IMAGE@$TARGET_POSTGRES_IMAGE_DIGEST" >/dev/null
BUSYBOX_IMAGE_ID=$(docker image inspect --format '{{.Id}}' "$BUSYBOX_IMAGE@$BUSYBOX_IMAGE_DIGEST")
SOURCE_POSTGRES_IMAGE_ID=$(docker image inspect --format '{{.Id}}' "$SOURCE_POSTGRES_IMAGE@$SOURCE_POSTGRES_IMAGE_DIGEST")
TARGET_POSTGRES_IMAGE_ID=$(docker image inspect --format '{{.Id}}' "$TARGET_POSTGRES_IMAGE@$TARGET_POSTGRES_IMAGE_DIGEST")
docker image tag "$BUSYBOX_IMAGE_ID" "$BUSYBOX_IMAGE"
docker image tag "$SOURCE_POSTGRES_IMAGE_ID" "$SOURCE_POSTGRES_IMAGE"
docker image tag "$TARGET_POSTGRES_IMAGE_ID" "$TARGET_POSTGRES_IMAGE"
docker image tag "$BUSYBOX_IMAGE" telegramd:local

cat > "$SOURCE_COMPOSE" <<'YAML'
name: telegram-server
services:
  postgres:
    image: ghcr.io/teagramhq/teagram-server-postgres:16-alpine
    environment:
      POSTGRES_DB: telegram
      POSTGRES_PASSWORD: fixture-only
      POSTGRES_HOST_AUTH_METHOD: trust
    volumes:
      - pgdata:/var/lib/postgresql/data
  telegramd:
    image: telegramd:local
    command: ["sh", "-c", "sleep 3600"]
    environment:
      TG_BLOB_DIR: /var/lib/telegramd-blobs
    volumes:
      - tgkey:/var/lib/telegramd
      - tgblobs:/var/lib/telegramd-blobs
  rustfs:
    image: ghcr.io/rustfs/rustfs@sha256:1803faef57627e2d9c2e7d89d655d712ddded5389040054987163043fecb6a3c
    ports:
      - "127.0.0.1::9000"
    environment:
      RUSTFS_VOLUMES: /data
      RUSTFS_ADDRESS: 0.0.0.0:9000
      RUSTFS_CONSOLE_ENABLE: "false"
      RUSTFS_ACCESS_KEY: fixture-root-access
      RUSTFS_SECRET_KEY: fixture-root-secret
    volumes:
      - rustfsdata:/data
    healthcheck:
      test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:9000/health/ready"]
      interval: 2s
      timeout: 5s
      retries: 30
      start_period: 5s
volumes:
  pgdata:
    name: telegram-server_pgdata
  tgkey:
    name: telegram-server_tgkey
  tgblobs:
    name: telegram-server_tgblobs
  rustfsdata:
    name: telegram-server_rustfsdata
YAML

cat > "$TARGET_COMPOSE" <<'YAML'
name: teagram
services:
  postgres:
    image: ghcr.io/teagramhq/teagram-server-postgres:18-alpine
    environment:
      POSTGRES_DB: teagram
      POSTGRES_PASSWORD: fixture-only
      PGDATA: /var/lib/postgresql/18/docker
    volumes:
      - pgdata:/var/lib/postgresql
  teagramd:
    image: teagramd:local
    build:
      context: .
      dockerfile: teagramd.Dockerfile
      args:
        TEAGRAMD_SOURCE_REVISION: ${TEAGRAMD_SOURCE_REVISION:-}
    command: ["sh", "-c", "sleep 3600"]
    environment:
      TG_BLOB_DIR: /var/lib/telegramd-blobs
    volumes:
      - tgkey:/var/lib/teagramd
      - tgblobs:/var/lib/telegramd-blobs
  rustfs:
    image: ghcr.io/rustfs/rustfs@sha256:1803faef57627e2d9c2e7d89d655d712ddded5389040054987163043fecb6a3c
    ports:
      - "127.0.0.1::9000"
    environment:
      RUSTFS_VOLUMES: /data
      RUSTFS_ADDRESS: 0.0.0.0:9000
      RUSTFS_CONSOLE_ENABLE: "false"
      RUSTFS_ACCESS_KEY: fixture-root-access
      RUSTFS_SECRET_KEY: fixture-root-secret
    volumes:
      - rustfsdata:/data
    healthcheck:
      test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:9000/health/ready"]
      interval: 2s
      timeout: 5s
      retries: 30
      start_period: 5s
volumes:
  pgdata:
    name: teagram_pgdata
  tgkey:
    name: teagram_tgkey
  tgblobs:
    name: teagram_tgblobs
  rustfsdata:
    name: teagram_rustfsdata
YAML

cat > "$TMP/s3-source.yml" <<'YAML'
name: telegram-server
services:
  postgres:
    image: ghcr.io/teagramhq/teagram-server-postgres:16-alpine
    environment:
      POSTGRES_DB: telegram
      POSTGRES_PASSWORD: fixture-only
      POSTGRES_HOST_AUTH_METHOD: trust
    volumes:
      - pgdata:/var/lib/postgresql/data
  telegramd:
    image: telegramd:local
    environment:
      TG_BLOB_DIR: /var/lib/telegramd-blobs
      TG_BLOB_S3_ENDPOINT: http://rustfs:9000
      TG_BLOB_S3_BUCKET: telegram
      TG_BLOB_S3_PREFIX: telegramd/
      TG_BLOB_S3_REGION: us-east-1
      TG_BLOB_S3_ACCESS_KEY_ID: fixture-access
      TG_BLOB_S3_SECRET_ACCESS_KEY: fixture-secret
    volumes:
      - tgblobs:/var/lib/telegramd-blobs:ro
volumes:
  pgdata:
    name: telegram-server_pgdata
  tgblobs:
    name: telegram-server_tgblobs
YAML

cat > "$SOURCE_OVERRIDE" <<'YAML'
name: telegram-server
services:
  telegramd:
    ports:
      - target: 8080
        host_ip: 127.0.0.1
YAML

cat > "$TARGET_DIR/teagramd.Dockerfile" <<'DOCKERFILE'
FROM ghcr.io/teagramhq/teagram-server-busybox:1.37
ARG TEAGRAMD_SOURCE_REVISION
LABEL org.opencontainers.image.revision="${TEAGRAMD_SOURCE_REVISION}"
DOCKERFILE
cp -- "$TARGET_DIR/teagramd.Dockerfile" "$SOURCE_DIR/teagramd.Dockerfile"

mkdir -p "$TARGET_DIR/deploy/teagramd/rollout-runner"
cat > "$TARGET_DIR/deploy/teagramd/rollout-runner/rollout-verifier.sh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[ "$#" -eq 4 ] && [ "$1" = ready ] && [ "$2" = target ] || exit 64
if [ "${STACK_MOVE_TEST_STOP_TARGET:-}" = 1 ]; then
  # Stop the target only once the move reaches readiness, after every earlier
  # runtime check has seen it running.
  "$REAL_DOCKER" exec "$3" touch /tmp/stack-move-exit || true
  "$REAL_DOCKER" wait "$3" >/dev/null
fi
state=$("$REAL_DOCKER" inspect --format '{{.State.Status}}' "$3") || exit 1
if [ "$state" != running ]; then
  printf 'target server readiness failed: state=%s\n' "$state" >&2
  exit 1
fi
printf '%s\n' 'role=target result=ready'
SH
chmod 700 "$TARGET_DIR/deploy/teagramd/rollout-runner/rollout-verifier.sh"

printf 'POSTGRES_PASSWORD=fixture-only\n' > "$SOURCE_ENV"
printf 'POSTGRES_PASSWORD=fixture-only\n' > "$TARGET_ENV"
chmod 600 "$SOURCE_ENV" "$TARGET_ENV"
mkdir -m 700 "$TMP/bin"
REAL_GIT=$(command -v git)
cat > "$TMP/bin/git" <<'SH'
#!/bin/sh
if [ "${3:-}" = show ] && [ "${4:-}" = "$TARGET_SHA:docker-compose.yml" ]; then
  cat "$STACK_MOVE_TARGET_COMPOSE"
  exit $?
fi
exec "$REAL_GIT" "$@"
SH
chmod 700 "$TMP/bin/git"
REAL_DOCKER=$(command -v docker)
export REAL_DOCKER BUSYBOX_IMAGE
cat > "$TMP/bin/docker" <<'SH'
#!/bin/sh
if [ "${STACK_MOVE_FAIL_SOURCE_ROLLBACK:-}" = 1 ] && [ "${1:-}" = compose ]; then
  case " $* " in
    *" --project-name telegram-server "*" up -d "*)
      printf '%s\n' 'ERROR: injected source Compose rollback failure' >&2
      exit 44
      ;;
  esac
fi
if [ "${STACK_MOVE_START_TARGET_WRITER_AFTER_CREATE:-}" = 1 ] \
  && [ "${1:-}" = volume ] && [ "${2:-}" = create ] \
  && [ "${3:-}" = teagram_tgblobs ]; then
  created=$("$REAL_DOCKER" "$@") || exit $?
  "$REAL_DOCKER" run --detach --name main1974-stack-move-external-writer --network none \
    --mount type=volume,source=teagram_tgblobs,target=/data \
    --entrypoint sh "$BUSYBOX_IMAGE" -ec 'sleep 3600' >/dev/null || exit $?
  printf '%s\n' "$created"
  exit 0
fi
if [ "${STACK_MOVE_FAIL_DUMP:-}" = 1 ] \
  && [ "${1:-}" = exec ] && [ "${3:-}" = pg_dump ]; then
  cat >/dev/null
  printf '%s\n' 'ERROR: injected pg_dump failure' >&2
  exit 42
fi
if [ "${STACK_MOVE_FAIL_RESTORE:-}" = 1 ] \
  && [ "${1:-}" = exec ] && [ "${2:-}" = -i ]; then
  case "${3:-}" in
    main1974-move-restore-*)
      previous=
      for argument in "$@"; do
        if [ "$previous" = -d ] && [ "$argument" = teagram ]; then
          cat >/dev/null
          printf '%s\n' 'ERROR: injected restore SQL failure' >&2
          exit 3
        fi
        previous=$argument
      done
      ;;
  esac
fi
exec "$REAL_DOCKER" "$@"
SH
chmod 700 "$TMP/bin/docker"

inspect_layout() {
  local target_compose=$1
  TARGET_SHA="$TARGET_SHA" STACK_MOVE_TARGET_COMPOSE="$target_compose" REAL_GIT="$REAL_GIT" \
    PATH="$TMP/bin:$PATH" python3 "$HELPER" inspect \
      --checkout "$SOURCE_DIR" --target-sha "$TARGET_SHA" \
      --env-file "$SOURCE_ENV" --override "$SOURCE_OVERRIDE"
}

inspect_result=$(inspect_layout "$SOURCE_COMPOSE") || fail 'normal target Compose inspection failed'
printf '%s' "$inspect_result" | jq -e '.changed == false' >/dev/null || \
  fail 'unchanged Compose project and volume layout was rejected'
inspect_result=$(inspect_layout "$TARGET_COMPOSE") || fail 'renamed target Compose inspection failed'
printf '%s' "$inspect_result" | jq -e '.changed == true and .target.project == "teagram"' >/dev/null || \
  fail 'renamed Compose project and volume layout was not detected'
sed 's#      - rustfsdata:/data$#      - rustfsdata:/changed-data#' "$SOURCE_COMPOSE" > "$TMP/rustfs-mount-only.yml"
inspect_result=$(inspect_layout "$TMP/rustfs-mount-only.yml") || fail 'RustFS mount-only Compose inspection failed'
printf '%s' "$inspect_result" | jq -e '
  .changed == true
  and .source.project == .target.project
  and .source.server == .target.server
  and .source.server_mounts == .target.server_mounts
  and .source.postgres_mounts == .target.postgres_mounts
  and .source.volumes == .target.volumes
  and (.source.persistent_service_mounts | keys) == ["postgres", "rustfs", "telegramd"]
  and .source.persistent_service_mounts.postgres == .target.persistent_service_mounts.postgres
  and .source.persistent_service_mounts.telegramd == .target.persistent_service_mounts.telegramd
  and .source.persistent_service_mounts.rustfs[0].target == "/data"
  and .target.persistent_service_mounts.rustfs[0].target == "/changed-data"
' >/dev/null || fail 'mount-only RustFS layout change was not detected'
printf '%s\n' 'PASS layout inspection detects persistent mount-only changes on any service'

write_volume_file() {
  local volume=$1 path=$2 contents=$3
  docker run --rm --network none --mount "type=volume,source=$volume,target=/data" \
    --entrypoint sh "$BUSYBOX_IMAGE" -ec \
    'mkdir -p "/data/$(dirname "$1")"; printf "%s" "$2" > "/data/$1"' sh "$path" "$contents"
}

wait_rustfs() {
  local project=$1 project_dir compose_file env_file rustfs_id health
  if [ "$project" = "$TARGET_PROJECT" ]; then
    project_dir=$TARGET_DIR
    compose_file=$TARGET_COMPOSE
    env_file=$TARGET_ENV
  else
    project_dir=$SOURCE_DIR
    compose_file=$SOURCE_COMPOSE
    env_file=$SOURCE_ENV
  fi
  rustfs_id=$(docker compose --project-name "$project" --project-directory "$project_dir" \
    --env-file "$env_file" --file "$compose_file" ps -q rustfs)
  [ -n "$rustfs_id" ] || fail "RustFS service is missing from $project"
  for ((attempt=0; attempt<60; attempt++)); do
    health=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$rustfs_id")
    [ "$health" = healthy ] && return 0
    sleep 1
  done
  docker logs "$rustfs_id" >&2 || true
  fail "RustFS service in $project did not become healthy"
}

rustfs_endpoint() {
  local project=$1 project_dir compose_file env_file endpoint
  if [ "$project" = "$TARGET_PROJECT" ]; then
    project_dir=$TARGET_DIR
    compose_file=$TARGET_COMPOSE
    env_file=$TARGET_ENV
  else
    project_dir=$SOURCE_DIR
    compose_file=$SOURCE_COMPOSE
    env_file=$SOURCE_ENV
  fi
  endpoint=$(docker compose --project-name "$project" --project-directory "$project_dir" \
    --env-file "$env_file" --file "$compose_file" port rustfs 9000) || \
    fail "could not resolve RustFS endpoint for $project"
  [[ "$endpoint" =~ ^127\.0\.0\.1:[0-9]+$ ]] || \
    fail "RustFS endpoint is not loopback-only for $project: $endpoint"
  printf 'http://%s' "$endpoint"
}

aws_cli() {
  local project=$1 endpoint
  shift
  endpoint=$(rustfs_endpoint "$project")
  AWS_ACCESS_KEY_ID="$RUSTFS_ROOT_ACCESS_KEY" \
    AWS_SECRET_ACCESS_KEY="$RUSTFS_ROOT_SECRET_KEY" \
    AWS_DEFAULT_REGION=us-east-1 AWS_CONFIG_FILE="$TMP/aws-config" \
    aws --endpoint-url "$endpoint" "$@"
}

aws_cli_upload_stdin() {
  local project=$1 target=$2
  aws_cli "$project" s3 cp - "$target"
}

capture_rustfs_objects() {
  local project=$1 output_dir=$2 mode=${3:-read}
  mkdir -m 700 "$output_dir"
  if [ "$mode" = upload ]; then
    aws_cli "$project" s3api create-bucket --bucket telegram >/dev/null
    cat "$TMP/rustfs-upload/object-a" | \
      aws_cli_upload_stdin "$project" s3://telegram/objects/object-a
  fi
  aws_cli "$project" s3api list-objects-v2 --bucket telegram --output json \
    > "$output_dir/listing.json"
  aws_cli "$project" s3 cp s3://telegram/objects/object-a - > "$output_dir/object-a"
  jq '[.Contents[]? | {Key, Size}] | sort_by(.Key)' "$output_dir/listing.json" \
    > "$output_dir/listing-normalized.json" || fail "RustFS object listing is invalid for $project"
  sha256sum "$output_dir/object-a" | cut -d ' ' -f1 > "$output_dir/object.sha256" || \
    fail "RustFS object download is missing for $project"
}

start_source() {
  local project=${1:-$SOURCE_PROJECT}
  docker compose --project-name "$project" --project-directory "$SOURCE_DIR" \
    --env-file "$SOURCE_ENV" --file "$SOURCE_COMPOSE" up -d >/dev/null
  local postgres_id postgres_ready=0
  postgres_id=$(docker compose --project-name "$project" --project-directory "$SOURCE_DIR" \
    --env-file "$SOURCE_ENV" --file "$SOURCE_COMPOSE" ps -q postgres)
  for ((attempt=0; attempt<60; attempt++)); do
    if docker exec "$postgres_id" pg_isready -h 127.0.0.1 -U postgres -d telegram >/dev/null 2>&1; then
      postgres_ready=1
      break
    fi
    sleep 1
  done
  [ "$postgres_ready" = 1 ] || fail 'source PostgreSQL 16 did not become ready'
  wait_rustfs "$project"
}

run_move() {
  local compose_file=$1 output_file=$2
  rm -f -- "$TARGET_DIR/docker-compose.override.yml"
  python3 "$HELPER" move \
    --source-dir "$SOURCE_DIR" --target-dir "$TARGET_DIR" \
    --source-compose "$SOURCE_COMPOSE" --target-compose "$compose_file" \
    --target-sha "$TARGET_SHA" \
    --override "$SOURCE_OVERRIDE" \
    --env-file "$SOURCE_ENV" --target-env-file "$TARGET_ENV" \
    --evidence-dir "$EVIDENCE_DIR" >"$output_file" 2>&1
}

run_move_restore_failure() {
  local output_file=$1
  rm -f -- "$TARGET_DIR/docker-compose.override.yml"
  REAL_DOCKER="$REAL_DOCKER" STACK_MOVE_FAIL_RESTORE=1 PATH="$TMP/bin:$PATH" \
    python3 "$HELPER" move \
      --source-dir "$SOURCE_DIR" --target-dir "$TARGET_DIR" \
      --source-compose "$SOURCE_COMPOSE" --target-compose "$TARGET_COMPOSE" \
      --target-sha "$TARGET_SHA" \
      --override "$SOURCE_OVERRIDE" \
      --env-file "$SOURCE_ENV" --target-env-file "$TARGET_ENV" \
      --evidence-dir "$EVIDENCE_DIR" >"$output_file" 2>&1
}

run_move_with_source() {
  local source_compose=$1 target_compose=$2 output_file=$3
  rm -f -- "$TARGET_DIR/docker-compose.override.yml"
  python3 "$HELPER" move \
    --source-dir "$SOURCE_DIR" --target-dir "$TARGET_DIR" \
    --source-compose "$source_compose" --target-compose "$target_compose" \
    --target-sha "$TARGET_SHA" \
    --override "$SOURCE_OVERRIDE" \
    --env-file "$SOURCE_ENV" --target-env-file "$TARGET_ENV" \
    --evidence-dir "$EVIDENCE_DIR" >"$output_file" 2>&1
}

run_move_late_target_writer() {
  local output_file=$1
  rm -f -- "$TARGET_DIR/docker-compose.override.yml"
  REAL_DOCKER="$REAL_DOCKER" BUSYBOX_IMAGE="$BUSYBOX_IMAGE" \
    STACK_MOVE_START_TARGET_WRITER_AFTER_CREATE=1 PATH="$TMP/bin:$PATH" \
    python3 "$HELPER" move \
      --source-dir "$SOURCE_DIR" --target-dir "$TARGET_DIR" \
      --source-compose "$SOURCE_COMPOSE" --target-compose "$TARGET_COMPOSE" \
      --target-sha "$TARGET_SHA" \
      --override "$SOURCE_OVERRIDE" \
      --env-file "$SOURCE_ENV" --target-env-file "$TARGET_ENV" \
      --evidence-dir "$EVIDENCE_DIR" >"$output_file" 2>&1
}

run_move_dump_failure() {
  local output_file=$1
  rm -f -- "$TARGET_DIR/docker-compose.override.yml"
  REAL_DOCKER="$REAL_DOCKER" STACK_MOVE_FAIL_DUMP=1 PATH="$TMP/bin:$PATH" \
    python3 "$HELPER" move \
      --source-dir "$SOURCE_DIR" --target-dir "$TARGET_DIR" \
      --source-compose "$SOURCE_COMPOSE" --target-compose "$TARGET_COMPOSE" \
      --target-sha "$TARGET_SHA" \
      --override "$SOURCE_OVERRIDE" \
      --env-file "$SOURCE_ENV" --target-env-file "$TARGET_ENV" \
      --evidence-dir "$EVIDENCE_DIR" >"$output_file" 2>&1
}

run_move_delayed_target_exit() {
  local compose_file=$1 output_file=$2
  rm -f -- "$TARGET_DIR/docker-compose.override.yml"
  STACK_MOVE_TEST_STOP_TARGET=1 REAL_DOCKER="$REAL_DOCKER" PATH="$TMP/bin:$PATH" \
    python3 "$HELPER" move \
      --source-dir "$SOURCE_DIR" --target-dir "$TARGET_DIR" \
      --source-compose "$SOURCE_COMPOSE" --target-compose "$compose_file" \
      --target-sha "$TARGET_SHA" \
      --override "$SOURCE_OVERRIDE" \
      --env-file "$SOURCE_ENV" --target-env-file "$TARGET_ENV" \
      --evidence-dir "$EVIDENCE_DIR" >"$output_file" 2>&1
}

run_move_rollback_failure() {
  local compose_file=$1 output_file=$2
  rm -f -- "$TARGET_DIR/docker-compose.override.yml"
  STACK_MOVE_TEST_STOP_TARGET=1 STACK_MOVE_FAIL_SOURCE_ROLLBACK=1 \
    REAL_DOCKER="$REAL_DOCKER" PATH="$TMP/bin:$PATH" \
    python3 "$HELPER" move \
      --source-dir "$SOURCE_DIR" --target-dir "$TARGET_DIR" \
      --source-compose "$SOURCE_COMPOSE" --target-compose "$compose_file" \
      --target-sha "$TARGET_SHA" \
      --override "$SOURCE_OVERRIDE" \
      --env-file "$SOURCE_ENV" --target-env-file "$TARGET_ENV" \
      --evidence-dir "$EVIDENCE_DIR" >"$output_file" 2>&1
}

start_source unexpected-source
if run_move "$TARGET_COMPOSE" "$TMP/wrong-project.out"; then
  fail 'move accepted a source stack under an unexpected Compose project'
fi
grep -q 'expected old telegram-server and postgres containers are not running' "$TMP/wrong-project.out" || \
  fail 'wrong-project refusal did not identify the unexpected source stack'
[ -n "$(container_ids unexpected-source)" ] || fail 'wrong-project refusal stopped the unexpected source stack'
docker compose --project-name unexpected-source --project-directory "$SOURCE_DIR" \
  --env-file "$SOURCE_ENV" --file "$SOURCE_COMPOSE" down >/dev/null

start_source
if run_move_with_source "$TMP/s3-source.yml" "$TARGET_COMPOSE" "$TMP/s3-source.out"; then
  fail 'move accepted an S3-authorized source lane'
fi
grep -q 'source blob mode must not select S3 authority' \
  "$TMP/s3-source.out" || fail 'S3-lane refusal did not identify the unsupported authority'
local_server=$(docker compose --project-name "$SOURCE_PROJECT" --project-directory "$SOURCE_DIR" \
  --env-file "$SOURCE_ENV" --file "$SOURCE_COMPOSE" ps -q telegramd)
[ -n "$local_server" ] && [ "$(docker inspect --format '{{.State.Running}}' "$local_server")" = true ] || \
  fail 'S3-lane refusal stopped the live local source server'
docker compose --project-name "$SOURCE_PROJECT" --project-directory "$SOURCE_DIR" \
  --env-file "$SOURCE_ENV" --file "$SOURCE_COMPOSE" down >/dev/null
printf '%s\n' 'PASS move rejects S3 authority before stopping the live local source'

start_source
source_postgres=$(docker compose --project-name "$SOURCE_PROJECT" --project-directory "$SOURCE_DIR" \
  --env-file "$SOURCE_ENV" --file "$SOURCE_COMPOSE" ps -q postgres)
docker exec "$source_postgres" psql -X -v ON_ERROR_STOP=1 -U postgres -d telegram \
  -c 'CREATE TABLE public.stack_move_probe (id integer PRIMARY KEY, value text NOT NULL); INSERT INTO public.stack_move_probe VALUES (1, '\''one'\''), (2, '\''two'\'');' >/dev/null
docker exec "$source_postgres" psql -X -v ON_ERROR_STOP=1 -U postgres -d postgres \
  -c "CREATE ROLE browser_acceptance_probe LOGIN PASSWORD 'fixture-role-password' NOSUPERUSER NOCREATEDB NOCREATEROLE INHERIT NOREPLICATION NOBYPASSRLS" >/dev/null
write_volume_file telegram-server_tgkey enc_key.hex 'fixture-key-material'
write_volume_file telegram-server_tgblobs objects/blob-a 'fixture-blob-object'
write_volume_file telegram-server_rustfsdata fixture.marker 'rustfs-volume-preserved'
mkdir -m 700 "$TMP/rustfs-upload"
printf '%s' 'rustfs-object-preserved' > "$TMP/rustfs-upload/object-a"
capture_rustfs_objects "$SOURCE_PROJECT" "$TMP/rustfs-source" upload

docker run --detach --name main1974-stack-move-external-writer --network none \
  --mount type=volume,source=telegram-server_tgkey,target=/data \
  --entrypoint sh "$BUSYBOX_IMAGE" -ec 'sleep 3600' >/dev/null
if run_move "$TARGET_COMPOSE" "$TMP/external-writer.out"; then
  fail 'move copied while an external source-volume writer was running'
fi
grep -q 'a running container outside telegram-server writes a source volume' "$TMP/external-writer.out" || \
  fail 'external-writer refusal did not identify the live writer'
[ -n "$(docker compose --project-name "$SOURCE_PROJECT" --project-directory "$SOURCE_DIR" \
  --env-file "$SOURCE_ENV" --file "$SOURCE_COMPOSE" ps -q telegramd)" ] || \
  fail 'external-writer refusal stopped the source stack'
[ "$(docker inspect --format '{{.State.Running}}' main1974-stack-move-external-writer)" = true ] || \
  fail 'external-writer refusal stopped the outside writer'
docker rm -f main1974-stack-move-external-writer >/dev/null
printf '%s\n' 'PASS move refuses to copy source volumes while an external writer is running'

docker volume create teagram_tgkey >/dev/null
docker run --detach --name main1974-stack-move-external-writer --network none \
  --mount type=volume,source=teagram_tgkey,target=/data \
  --entrypoint sh "$BUSYBOX_IMAGE" -ec 'sleep 3600' >/dev/null
if run_move "$TARGET_COMPOSE" "$TMP/target-writer.out"; then
  fail 'move accepted an empty target volume with an external writer'
fi
grep -q 'a running container writes target volume teagram_tgkey' "$TMP/target-writer.out" || \
  fail 'target-writer refusal did not identify the destination volume'
[ -n "$(docker compose --project-name "$SOURCE_PROJECT" --project-directory "$SOURCE_DIR" \
  --env-file "$SOURCE_ENV" --file "$SOURCE_COMPOSE" ps -q telegramd)" ] || \
  fail 'target-writer refusal stopped the source stack'
[ "$(docker inspect --format '{{.State.Running}}' main1974-stack-move-external-writer)" = true ] || \
  fail 'target-writer refusal stopped the outside writer'
docker rm -f main1974-stack-move-external-writer >/dev/null
docker volume rm teagram_tgkey >/dev/null
printf '%s\n' 'PASS move refuses an outside writer on an empty target volume before stopping the source'

docker volume create teagram_tgkey >/dev/null
write_volume_file teagram_tgkey existing 'must-not-be-replaced'
if run_move "$TARGET_COMPOSE" "$TMP/nonempty-target.out"; then
  fail 'move accepted a target volume with content'
fi
grep -q 'target volume teagram_tgkey already contains data' "$TMP/nonempty-target.out" || \
  fail 'nonempty-target refusal did not identify the volume'
[ -n "$(docker compose --project-name "$SOURCE_PROJECT" --project-directory "$SOURCE_DIR" \
  --env-file "$SOURCE_ENV" --file "$SOURCE_COMPOSE" ps -q telegramd)" ] || \
  fail 'nonempty-target refusal stopped the source stack'
docker volume rm teagram_tgkey >/dev/null

rm -f -- "$EVIDENCE_DIR"/*
start_source
if run_move_late_target_writer "$TMP/late-target-writer.out"; then
  fail 'move copied after an external writer attached to a newly created target volume'
fi
grep -q 'a running container writes target volume teagram_tgblobs' "$TMP/late-target-writer.out" || {
  cat "$TMP/late-target-writer.out" >&2
  fail 'late target-writer refusal did not identify the destination volume'
}
jq -e '.result == "failed" and .rollback == "old-project-restored" and (.reason | contains("teagram_tgblobs"))' \
  "$EVIDENCE_DIR/stack-move-evidence.json" >/dev/null || fail 'late target-writer failure did not record rollback'
source_server=$(docker compose --project-name "$SOURCE_PROJECT" --project-directory "$SOURCE_DIR" \
  --env-file "$SOURCE_ENV" --file "$SOURCE_COMPOSE" ps -q telegramd)
[ "$(docker inspect --format '{{.State.Running}}' "$source_server")" = true ] || \
  fail 'late target-writer refusal did not restart the source stack'
[ "$(docker inspect --format '{{.State.Running}}' main1974-stack-move-external-writer)" = true ] || \
  fail 'late target writer was unexpectedly stopped'
docker rm -f main1974-stack-move-external-writer >/dev/null
for name in "${TARGET_VOLUMES[@]}"; do docker volume rm "$name" >/dev/null; done
rm -f -- "$EVIDENCE_DIR"/*
printf '%s\n' 'PASS move rechecks destination writers after volume creation and rolls back'

if ! run_move "$TARGET_COMPOSE" "$TMP/success.out"; then
  cat "$TMP/success.out" >&2
  fail 'verified stack move did not complete'
fi
evidence_file="$EVIDENCE_DIR/stack-move-evidence.json"
jq -e '
  .result == "verified"
  and .target_sha == "1234567890abcdef1234567890abcdef12345678"
  and .source_project == "telegram-server"
  and .target_project == "teagram"
  and .source_database == "telegram"
  and .target_database == "teagram"
  and .runtime_row_counts_allow_only_atlas_revision_growth == true
  and .roles_equal == true
  and .volumes.old_volumes_retained == true
  and .volumes.source_pgdata_unchanged == true
  and .isolated_restore.row_counts_equal == true
  and .isolated_restore.roles_equal == true
' "$evidence_file" >/dev/null || fail 'move evidence is missing required database and volume results'
cmp -s "$EVIDENCE_DIR/table-row-counts.source.tsv" "$EVIDENCE_DIR/table-row-counts.target-runtime.tsv" || \
  fail 'per-table counts differ after PostgreSQL 18 container recreation'
cmp -s "$EVIDENCE_DIR/pgdata.source-stopped.sha256" "$EVIDENCE_DIR/pgdata.source-retained.sha256" || \
  fail 'old PostgreSQL 16 volume changed during the move'
jq -e 'any(.[]; .name == "browser_acceptance_probe" and .login == true and .password_set == true)' \
  "$EVIDENCE_DIR/roles.target-runtime.json" >/dev/null || fail 'browser acceptance role was not preserved'
! grep -q 'fixture-role-password' "$EVIDENCE_DIR/roles.target-runtime.json" || \
  fail 'role password was exposed in evidence'
target_postgres=$(docker compose --project-name "$TARGET_PROJECT" --project-directory "$TARGET_DIR" \
  --env-file "$TARGET_ENV" --file "$TARGET_COMPOSE" \
  --file "$TARGET_DIR/docker-compose.override.yml" ps -q postgres)
target_server=$(docker compose --project-name "$TARGET_PROJECT" --project-directory "$TARGET_DIR" \
  --env-file "$TARGET_ENV" --file "$TARGET_COMPOSE" \
  --file "$TARGET_DIR/docker-compose.override.yml" ps -q teagramd)
wait_rustfs "$TARGET_PROJECT"
capture_rustfs_objects "$TARGET_PROJECT" "$TMP/rustfs-target"
cmp -s "$TMP/rustfs-source/listing-normalized.json" "$TMP/rustfs-target/listing-normalized.json" || \
  fail 'RustFS S3 object listing differs after the move'
cmp -s "$TMP/rustfs-source/object.sha256" "$TMP/rustfs-target/object.sha256" || \
  fail 'RustFS downloaded object checksum differs after the move'
docker inspect "$target_server" | jq -e '.[0].HostConfig.PortBindings | has("8080/tcp")' >/dev/null || \
  fail 'target override did not move telegramd service settings onto teagramd'
docker exec "$target_postgres" grep -Eq '^[[:space:]]*host[[:space:]]+all[[:space:]]+all[[:space:]]+all[[:space:]]+scram-sha-256([[:space:]]|$)' \
  /var/lib/postgresql/18/docker/pg_hba.conf || fail 'isolated restore left PostgreSQL with a weaker host authentication rule'
docker exec "$target_postgres" sh -ec \
  "! grep -E '^[[:space:]]*host[[:space:]].*[[:space:]]trust([[:space:]]|$)' /var/lib/postgresql/18/docker/pg_hba.conf | \
    grep -Evq '[[:space:]](127\.0\.0\.1/32|::1/128)[[:space:]]'" || \
  fail 'target PostgreSQL host authentication unexpectedly trusts network clients'
for logical in tgkey tgblobs rustfsdata; do
  cmp -s "$EVIDENCE_DIR/$logical.source.sha256" "$EVIDENCE_DIR/$logical.target.sha256" || \
    fail "$logical file inventory differs after the move"
done
grep -q 'objects/blob-a' "$EVIDENCE_DIR/tgblobs.target.sha256" || fail 'blob object listing is missing its object'
grep -q 'fixture.marker' "$EVIDENCE_DIR/rustfsdata.target.sha256" || \
  fail 'RustFS volume file inventory is missing its fixture file'
for name in "${SOURCE_VOLUMES[@]}" "${TARGET_VOLUMES[@]}"; do
  volume_exists "$name" || fail "persistent volume $name was removed"
done
for path in "$EVIDENCE_DIR"/*; do
  [ "$(stat -c %a "$path")" = 600 ] || fail "evidence file is not private: $(basename "$path")"
done
printf '%s\n' 'PASS stack move copies persistent volumes and restores PostgreSQL 16 telegram into PostgreSQL 18 teagram'

docker compose --project-name "$TARGET_PROJECT" --project-directory "$TARGET_DIR" \
  --env-file "$TARGET_ENV" --file "$TARGET_COMPOSE" down >/dev/null
docker compose --project-name "$SOURCE_PROJECT" --project-directory "$SOURCE_DIR" \
  --env-file "$SOURCE_ENV" --file "$SOURCE_COMPOSE" down >/dev/null
for name in "${TARGET_VOLUMES[@]}" "${SOURCE_VOLUMES[@]}"; do docker volume rm "$name" >/dev/null; done

rm -f -- "$EVIDENCE_DIR"/*
start_source
if run_move_restore_failure "$TMP/restore-failure.out"; then
  fail 'move accepted an injected PostgreSQL restore failure'
fi
grep -Eq 'PostgreSQL database restore failed with exit 3; diagnostic saved to .*/restore-database\.stderr' "$TMP/restore-failure.out" || {
  cat "$TMP/restore-failure.out" >&2
  fail 'restore failure did not identify its operation and diagnostic'
}
grep -q 'ERROR: injected restore SQL failure' "$EVIDENCE_DIR/restore-database.stderr" || fail 'restore SQL diagnostic was not preserved in private evidence'
[ "$(stat -c %a "$EVIDENCE_DIR/restore-database.stderr")" = 600 ] || fail 'restore SQL diagnostic evidence is not private'
jq -e '.result == "failed" and (.reason | contains("PostgreSQL database restore")) and (.reason | contains("restore-database.stderr")) and .rollback == "old-project-restored"' "$EVIDENCE_DIR/stack-move-evidence.json" >/dev/null || fail 'restore failure evidence omitted the operation or rollback result'
source_server=$(docker compose --project-name "$SOURCE_PROJECT" --project-directory "$SOURCE_DIR" \
  --env-file "$SOURCE_ENV" --file "$SOURCE_COMPOSE" ps -q telegramd)
source_postgres=$(docker compose --project-name "$SOURCE_PROJECT" --project-directory "$SOURCE_DIR" \
  --env-file "$SOURCE_ENV" --file "$SOURCE_COMPOSE" ps -q postgres)
[ "$(docker inspect --format '{{.State.Running}}' "$source_server")" = true ] || fail 'restore failure did not restart telegramd on the old project'
[ "$(docker inspect --format '{{.State.Running}}' "$source_postgres")" = true ] || fail 'restore failure did not restart PostgreSQL 16 on the old project'
[ -z "$(container_ids "$TARGET_PROJECT")" ] || fail 'restore failure left the target project running'
printf '%s\n' 'PASS restore SQL diagnostics are private and survive rollback evidence'
docker compose --project-name "$SOURCE_PROJECT" --project-directory "$SOURCE_DIR" \
  --env-file "$SOURCE_ENV" --file "$SOURCE_COMPOSE" down >/dev/null
for name in "${TARGET_VOLUMES[@]}" "${SOURCE_VOLUMES[@]}"; do docker volume rm "$name" >/dev/null; done

rm -f -- "$EVIDENCE_DIR"/*
start_source
if run_move_dump_failure "$TMP/dump-failure.out"; then
  fail 'move accepted an injected PostgreSQL dump failure'
fi
grep -Eq 'PostgreSQL database dump failed with exit 42; diagnostic saved to .*/pgdump\.stderr' \
  "$TMP/dump-failure.out" || fail 'dump failure did not identify its operation and diagnostic'
grep -q 'ERROR: injected pg_dump failure' "$EVIDENCE_DIR/pgdump.stderr" || \
  fail 'pg_dump diagnostic was not preserved in private evidence'
[ "$(stat -c %a "$EVIDENCE_DIR/pgdump.stderr")" = 600 ] || fail 'pg_dump diagnostic evidence is not private'
jq -e '.result == "failed" and (.reason | contains("PostgreSQL database dump")) and (.reason | contains("pgdump.stderr")) and .rollback == "old-project-restored"' \
  "$EVIDENCE_DIR/stack-move-evidence.json" >/dev/null || fail 'dump failure evidence omitted the operation or rollback result'
source_server=$(docker compose --project-name "$SOURCE_PROJECT" --project-directory "$SOURCE_DIR" \
  --env-file "$SOURCE_ENV" --file "$SOURCE_COMPOSE" ps -q telegramd)
[ "$(docker inspect --format '{{.State.Running}}' "$source_server")" = true ] || \
  fail 'dump failure did not restart the old project'
printf '%s\n' 'PASS PostgreSQL dump diagnostics are private and survive rollback evidence'
docker compose --project-name "$SOURCE_PROJECT" --project-directory "$SOURCE_DIR" \
  --env-file "$SOURCE_ENV" --file "$SOURCE_COMPOSE" down >/dev/null
for name in "${TARGET_VOLUMES[@]}" "${SOURCE_VOLUMES[@]}"; do
  if volume_exists "$name"; then docker volume rm "$name" >/dev/null; fi
done

rm -f -- "$EVIDENCE_DIR"/*
sed 's/sleep 3600/until [ -e \/tmp\/stack-move-exit ]; do sleep 1; done; exit 23/' "$TARGET_COMPOSE" > "$TMP/target-failing.yml"
start_source
write_volume_file telegram-server_tgkey enc_key.hex 'rollback-key-material'
write_volume_file telegram-server_tgblobs objects/blob-a 'rollback-blob-object'
write_volume_file telegram-server_rustfsdata rollback.marker 'rustfs-rollback-volume-preserved'
if run_move_delayed_target_exit "$TMP/target-failing.yml" "$TMP/rollback.out"; then
  fail 'move accepted a target server that exits after startup'
fi
if ! grep -q 'target teagramd readiness check failed with exit 1' "$TMP/rollback.out"; then
  cat "$TMP/rollback.out" >&2
  fail 'delayed target exit was not rejected by the readiness gate'
fi
grep -q 'rollback=old-project-restored' "$TMP/rollback.out" || fail 'post-stop failure did not report source rollback'
grep -q 'target server readiness failed: state=exited' "$EVIDENCE_DIR/target-readiness.stderr" || \
  fail 'target readiness failure diagnostic was not preserved'
[ "$(stat -c %a "$EVIDENCE_DIR/target-readiness.stderr")" = 600 ] || \
  fail 'target readiness diagnostic evidence is not private'
jq -e '.result == "failed" and (.reason | contains("target teagramd readiness check")) and .rollback == "old-project-restored"' \
  "$EVIDENCE_DIR/stack-move-evidence.json" >/dev/null || fail 'readiness failure did not trigger rollback evidence'
source_server=$(docker compose --project-name "$SOURCE_PROJECT" --project-directory "$SOURCE_DIR" \
  --env-file "$SOURCE_ENV" --file "$SOURCE_COMPOSE" ps -q telegramd)
source_postgres=$(docker compose --project-name "$SOURCE_PROJECT" --project-directory "$SOURCE_DIR" \
  --env-file "$SOURCE_ENV" --file "$SOURCE_COMPOSE" ps -q postgres)
wait_rustfs "$SOURCE_PROJECT"
[ "$(docker inspect --format '{{.State.Running}}' "$source_server")" = true ] || \
  fail 'rollback did not restart telegramd on the old project'
[ "$(docker inspect --format '{{.State.Running}}' "$source_postgres")" = true ] || \
  fail 'rollback did not restart PostgreSQL 16 on the old project'
[ "$(docker inspect --format '{{.State.Health.Status}}' "$(docker compose \
  --project-name "$SOURCE_PROJECT" --project-directory "$SOURCE_DIR" \
  --env-file "$SOURCE_ENV" --file "$SOURCE_COMPOSE" ps -q rustfs)")" = healthy ] || \
  fail 'rollback did not restart a healthy RustFS service'
[ -z "$(container_ids "$TARGET_PROJECT")" ] || fail 'failed target project remains running after rollback'
for name in "${SOURCE_VOLUMES[@]}"; do volume_exists "$name" || fail "rollback volume $name is missing"; done
grep -q 'rollback.marker' "$EVIDENCE_DIR/rustfsdata.source.sha256" || \
  fail 'RustFS volume state was not retained through rollback'
printf '%s\n' 'PASS readiness rejects a target that exits after startup and restores the old project'

docker compose --project-name "$TARGET_PROJECT" --project-directory "$TARGET_DIR" \
  --env-file "$TARGET_ENV" --file "$TARGET_COMPOSE" down >/dev/null
for name in "${TARGET_VOLUMES[@]}"; do docker volume rm "$name" >/dev/null; done
rm -f -- "$EVIDENCE_DIR"/*
if run_move_rollback_failure "$TMP/target-failing.yml" "$TMP/rollback-command-failure.out"; then
  fail 'move accepted an injected target readiness failure with a broken old-project restart'
fi
if ! grep -q 'rollback=old-project-restart-failed' "$TMP/rollback-command-failure.out"; then
  cat "$TMP/rollback-command-failure.out" >&2
  fail 'failed old-project restart was not reported'
fi
grep -q 'restart old Compose project during rollback failed with exit 44; diagnostic saved to .*/rollback-source-up.stderr' \
  "$TMP/rollback-command-failure.out" || fail 'rollback output omitted the failed operation and evidence path'
grep -q 'ERROR: injected source Compose rollback failure' "$EVIDENCE_DIR/rollback-source-up.stderr" || \
  fail 'old-project restart stderr was not preserved in private evidence'
[ "$(stat -c %a "$EVIDENCE_DIR/rollback-source-up.stderr")" = 600 ] || \
  fail 'old-project restart diagnostic evidence is not private'
jq -e '
  .result == "failed"
  and .rollback == "old-project-restart-failed"
  and any(.rollback_diagnostics[]; contains("restart old Compose project during rollback") and contains("rollback-source-up.stderr"))
' "$EVIDENCE_DIR/stack-move-evidence.json" >/dev/null || fail 'rollback evidence omitted source restart diagnostics'
printf '%s\n' 'PASS failed old-project restart keeps the Compose diagnostic in private evidence'
