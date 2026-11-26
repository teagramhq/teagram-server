#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
TMP_DIR=$(mktemp -d)
NETWORK_NAME="teagram-tailnet-source-$$"
TAILNET_NETWORK_NAME="$NETWORK_NAME-publish"
CONTAINER_NAME="teagram-tailnet-source-$$"
ALPINE_IMAGE=alpine:3.22
PUBLISHED_PORT=$((20000 + $$ % 40000))

cleanup() {
  docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
  docker network rm "$TAILNET_NETWORK_NAME" "$NETWORK_NAME" >/dev/null 2>&1 || true
  rm -rf -- "$TMP_DIR"
}
trap cleanup EXIT

[[ "$(docker info --format '{{.OSType}}')" == linux ]] || {
  printf 'tailnet source probe requires a Linux Docker engine\n' >&2
  exit 1
}
case "$(docker info --format '{{.Architecture}}')" in
  x86_64) GOARCH=amd64 ;;
  aarch64) GOARCH=arm64 ;;
  armv7l) GOARCH=arm ;;
  ppc64le) GOARCH=ppc64le ;;
  s390x) GOARCH=s390x ;;
  *)
    printf 'tailnet source probe does not support the Docker engine architecture\n' >&2
    exit 1
    ;;
esac

GOOS=linux CGO_ENABLED=0 GOARCH="$GOARCH" go build \
  -trimpath -o "$TMP_DIR/probe" "$SCRIPT_DIR/testdata/tailnet-source-probe.go"
docker network create --internal \
  --subnet 198.18.130.0/24 \
  --ip-range 198.18.130.128/26 \
  --gateway 198.18.130.1 \
  "$NETWORK_NAME" >/dev/null
docker network create \
  --subnet 198.18.134.0/24 \
  --ip-range 198.18.134.128/26 \
  --gateway 198.18.134.1 \
  --opt com.docker.network.bridge.enable_ip_masquerade=false \
  "$TAILNET_NETWORK_NAME" >/dev/null
docker run -d --name "$CONTAINER_NAME" --network "$NETWORK_NAME" \
  --expose 8081 \
  -p "127.0.0.1:$PUBLISHED_PORT:8081" \
  --entrypoint sh "$ALPINE_IMAGE" -c 'sleep 3600' >/dev/null
docker network connect "$TAILNET_NETWORK_NAME" "$CONTAINER_NAME"
docker cp "$TMP_DIR/probe" "$CONTAINER_NAME:/probe" >/dev/null
docker exec "$CONTAINER_NAME" chmod 0555 /probe
docker exec -d "$CONTAINER_NAME" /probe

published=$(docker port "$CONTAINER_NAME" 8081/tcp)
[[ "$published" == "127.0.0.1:$PUBLISHED_PORT" ]] || {
  printf 'tailnet source probe was not published on loopback\n' >&2
  exit 1
}
port=$PUBLISHED_PORT
actual=''
for _ in 1 2 3 4 5; do
  if actual=$(docker run --rm --network host --entrypoint sh "$ALPINE_IMAGE" \
    -c "wget -q -T 3 -O - http://127.0.0.1:$port/"); then
    break
  fi
  sleep 0.2
done
[[ "$actual" == 198.18.134.1 ]] || {
  printf 'loopback-published request reached the edge from %s, want listener gateway 198.18.134.1\n' "$actual" >&2
  exit 1
}
printf 'tailnet loopback-published connection source=%s\n' "$actual"
