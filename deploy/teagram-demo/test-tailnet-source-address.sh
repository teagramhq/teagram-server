#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
TMP_DIR=$(mktemp -d)
NETWORK_NAME="teagram-tailnet-source-$$"
TAILNET_NETWORK_NAME="$NETWORK_NAME-publish"
ECHO_CONTAINER_NAME="teagram-tailnet-echo-$$"
NGINX_CONTAINER_NAME="teagram-tailnet-nginx-$$"
CLIENT_CONTAINER_NAME="teagram-tailnet-client-$$"
PEER_CONTAINER_NAME="teagram-tailnet-peer-$$"
ALPINE_IMAGE=alpine:3.22
NGINX_IMAGE=${TEAGRAM_TEST_NGINX_IMAGE:-mirror.gcr.io/library/nginx:1.28.0-alpine@sha256:09ab424a8c788f8d0fe3a64429f6d19dfa526885c8609b748d0943a75dcb9f8c}

cleanup() {
  docker rm -f "$NGINX_CONTAINER_NAME" "$ECHO_CONTAINER_NAME" "$CLIENT_CONTAINER_NAME" "$PEER_CONTAINER_NAME" >/dev/null 2>&1 || true
  docker network rm "$TAILNET_NETWORK_NAME" "$NETWORK_NAME" >/dev/null 2>&1 || true
  rm -rf -- "$TMP_DIR"
}
trap cleanup EXIT

[[ "$(docker info --format '{{.OSType}}')" == linux ]] || {
  printf 'tailnet source probe requires a Linux Docker engine\n' >&2
  exit 1
}
case "$(docker info --format '{{.Architecture}}')" in
  x86_64) GOARCH=amd64; PLATFORM=linux/amd64 ;;
  aarch64) GOARCH=arm64; PLATFORM=linux/arm64 ;;
  armv7l) GOARCH=arm; PLATFORM=linux/arm/v7 ;;
  ppc64le) GOARCH=ppc64le; PLATFORM=linux/ppc64le ;;
  s390x) GOARCH=s390x; PLATFORM=linux/s390x ;;
  *)
    printf 'tailnet source probe does not support the Docker engine architecture\n' >&2
    exit 1
    ;;
esac

GOOS=linux CGO_ENABLED=0 GOARCH="$GOARCH" go build \
  -trimpath -o "$TMP_DIR/echo" "$SCRIPT_DIR/testdata/tailnet-source-echo/main.go"
chmod 0555 "$TMP_DIR/echo"
GOOS=linux CGO_ENABLED=0 GOARCH="$GOARCH" go build \
  -trimpath -o "$TMP_DIR/client" "$SCRIPT_DIR/testdata/tailnet-source-client/main.go"
chmod 0555 "$TMP_DIR/client"

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

docker create --name "$ECHO_CONTAINER_NAME" --platform "$PLATFORM" \
  --network "$NETWORK_NAME" --network-alias haproxy \
  --entrypoint sh "$ALPINE_IMAGE" -c 'sleep 3600' >/dev/null
docker cp "$TMP_DIR/echo" "$ECHO_CONTAINER_NAME:/echo" >/dev/null
docker start "$ECHO_CONTAINER_NAME" >/dev/null
docker exec "$ECHO_CONTAINER_NAME" chmod 0555 /echo
docker exec -d "$ECHO_CONTAINER_NAME" /echo

docker create --name "$NGINX_CONTAINER_NAME" --platform "$PLATFORM" \
  --network "$TAILNET_NETWORK_NAME" --network-alias tailnet-edge --user 101:101 \
  --tmpfs /tmp:rw,nosuid,nodev,noexec,size=16m,mode=1777 \
  -p '127.0.0.1::8081' --entrypoint sh "$NGINX_IMAGE" -c 'sleep 3600' >/dev/null
docker cp "$SCRIPT_DIR/nginx-tailnet.conf" \
  "$NGINX_CONTAINER_NAME:/etc/nginx/nginx.conf" >/dev/null
docker cp "$SCRIPT_DIR/nginx-config" "$NGINX_CONTAINER_NAME:/etc/nginx/demo" >/dev/null
docker start "$NGINX_CONTAINER_NAME" >/dev/null
docker network connect "$NETWORK_NAME" "$NGINX_CONTAINER_NAME"
docker exec "$NGINX_CONTAINER_NAME" nginx -c /etc/nginx/nginx.conf -t
docker exec -d "$NGINX_CONTAINER_NAME" nginx -c /etc/nginx/nginx.conf -g 'daemon off;'

published=$(docker port "$NGINX_CONTAINER_NAME" 8081/tcp)
[[ "$published" =~ ^127\.0\.0\.1:([0-9]+)$ ]] || {
  printf 'tailnet source probe was not published on loopback: %s\n' "$published" >&2
  docker logs "$NGINX_CONTAINER_NAME" >&2 || true
  exit 1
}
port=${BASH_REMATCH[1]}
docker create --name "$CLIENT_CONTAINER_NAME" --platform "$PLATFORM" --network host \
  --entrypoint sh "$ALPINE_IMAGE" -c 'sleep 3600' >/dev/null
docker cp "$TMP_DIR/client" "$CLIENT_CONTAINER_NAME:/client" >/dev/null
docker start "$CLIENT_CONTAINER_NAME" >/dev/null
docker exec "$CLIENT_CONTAINER_NAME" chmod 0555 /client
docker exec "$CLIENT_CONTAINER_NAME" /client host "$port" || {
  docker logs "$NGINX_CONTAINER_NAME" >&2 || true
  exit 1
}
docker create --name "$PEER_CONTAINER_NAME" --platform "$PLATFORM" --network "$TAILNET_NETWORK_NAME" \
  --entrypoint sh "$ALPINE_IMAGE" -c 'sleep 3600' >/dev/null
docker cp "$TMP_DIR/client" "$PEER_CONTAINER_NAME:/client" >/dev/null
docker start "$PEER_CONTAINER_NAME" >/dev/null
docker exec "$PEER_CONTAINER_NAME" chmod 0555 /client
docker exec "$PEER_CONTAINER_NAME" /client peer || {
  docker logs "$NGINX_CONTAINER_NAME" >&2 || true
  exit 1
}
printf 'tailnet Nginx forwarding policy holds on 127.0.0.1:%s\n' "$port"
