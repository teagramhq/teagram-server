#!/usr/bin/env bash
set -euo pipefail

: "${TEAGRAMD_IMAGE:?TEAGRAMD_IMAGE must be the image digest that was deployed}"
: "${MT_PROTO_ENDPOINT:?MT_PROTO_ENDPOINT must address the public or forwarded MTProto listener}"
: "${DISCOVERY_URL:?DISCOVERY_URL must address the teagramd client discovery endpoint}"
: "${LINK_SMOKE_URL:?LINK_SMOKE_URL must address the public or forwarded link selector}"
: "${RUNNER_TEMP:?RUNNER_TEMP must point to a writable temporary directory}"

tmp_dir="$(mktemp -d "$RUNNER_TEMP/teagram-k3s-smoke.XXXXXX")"
cleanup() {
  rm -rf -- "$tmp_dir"
}
trap cleanup EXIT

curl --fail --silent --show-error --location --max-time 20 \
  "$DISCOVERY_URL" --output "$tmp_dir/client.json"
jq --exit-status '.mtproto.rsa_spki | strings | select(length > 0)' \
  "$tmp_dir/client.json" >/dev/null
curl --fail --silent --show-error --location --max-time 20 \
  "$LINK_SMOKE_URL" --output "$tmp_dir/link.html"
grep --fixed-strings --quiet 'Open this in Teagram' "$tmp_dir/link.html"

timeout --signal=TERM --kill-after=5s 90s docker run --rm --network host \
  --volume "$tmp_dir/client.json:/client.json:ro" \
  --entrypoint /usr/local/bin/mtproto-smoke \
  "$TEAGRAMD_IMAGE" \
  --endpoint "$MT_PROTO_ENDPOINT" \
  --discovery-file /client.json
printf 'smoke=verified discovery=true links=true mtproto=true\n'
