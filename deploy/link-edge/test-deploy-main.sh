#!/usr/bin/env bash
set -euo pipefail
umask 077

script_dir="$(cd "$(dirname "$0")" && pwd)"
script="$script_dir/deploy-main.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/repo/deploy/link-edge" "$tmp/state"
cp "$script" "$tmp/repo/deploy/link-edge/deploy-main.sh"
cp "$script_dir/validate-discovery.py" "$tmp/repo/deploy/link-edge/validate-discovery.py"
: > "$tmp/repo/deploy/link-edge/compose.yaml"
script="$tmp/repo/deploy/link-edge/deploy-main.sh"

cat > "$tmp/bin/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$MOCK_DOCKER_LOG"
state=$MOCK_DOCKER_STATE

read_state() {
  cat "$state/$1"
}

write_state() {
  printf '%s\n' "$2" > "$state/$1"
}

case "$1" in
  compose)
    case " $* " in
      *' config --quiet '*) ;;
      *' ps --quiet selector '*) printf 'selector-fixture\n' ;;
      *' ps --quiet linklanding '*) printf 'landing-fixture\n' ;;
      *' build selector '*) write_state built_revision "$TEST_TARGET_SHA" ;;
      *' up '*selector*)
        if [[ -f "$state/use_rollback" ]]; then
          write_state current_revision "$(read_state rollback_revision)"
          rm "$state/use_rollback"
        elif [[ -f "$state/built_revision" ]]; then
          write_state current_revision "$(read_state built_revision)"
        fi
        ;;
      *) echo "unexpected docker compose call: $*" >&2; exit 97 ;;
    esac
    ;;
  inspect)
    format=$3
    container=$4
    case "$format" in
      *project.config_files*)
        if [[ ${MOCK_BAD_CONFIG:-0} == 1 ]]; then
          printf '/opt/teagram/other-compose.yaml\n'
        else
          printf '%s\n' "$MOCK_COMPOSE_FILE"
        fi
        ;;
      *com.docker.compose.project\"*) printf 'teagram-link-edge\n' ;;
      *com.docker.compose.service\"*)
        if [[ $container == selector-fixture ]]; then printf 'selector\n'; else printf 'linklanding\n'; fi
        ;;
      *State.Health.Status*) printf 'healthy\n' ;;
      *.Image*) printf 'sha256:selector-fixture\n' ;;
      *) echo "unexpected docker inspect format: $format" >&2; exit 98 ;;
    esac
    ;;
  image)
    case "$2" in
      inspect)
        format=$4
        image=$5
        case "$format" in
          *opencontainers.image.source*) printf 'https://github.com/teagramhq/teagram-server\n' ;;
          *opencontainers.image.revision*)
            if [[ $image == teagram-linkselector:local && -f "$state/built_revision" ]]; then
              read_state built_revision
            else
              read_state current_revision
            fi
            ;;
          *) echo "unexpected docker image inspect format: $format" >&2; exit 99 ;;
        esac
        ;;
      tag)
        source=$3
        destination=$4
        if [[ $destination == teagram-linkselector:rollback ]]; then
          write_state rollback_revision "$(read_state current_revision)"
        elif [[ $source == teagram-linkselector:rollback && $destination == teagram-linkselector:local ]]; then
          : > "$state/use_rollback"
        else
          echo "unexpected docker image tag: $*" >&2
          exit 100
        fi
        ;;
      *) echo "unexpected docker image call: $*" >&2; exit 101 ;;
    esac
    ;;
  *) echo "unexpected docker call: $*" >&2; exit 102 ;;
esac
DOCKER

cat > "$tmp/bin/curl" <<'CURL'
#!/usr/bin/env bash
set -euo pipefail
url=''
write_format=''
for arg in "$@"; do
  url=$arg
  if [[ $arg == *'%{http_code}'* || $arg == '%{content_type}' ]]; then
    write_format=$arg
  fi
done
if [[ $url == *'/.well-known/teagramd/client' ]]; then
  if [[ $write_format == *'%{http_code}'* ]]; then
    printf '%s\n%s' "$TEST_CLIENT_JSON" "$TEST_CLIENT_STATUS"
  elif [[ -z $write_format ]]; then
    printf '%s\n' "$TEST_CLIENT_JSON"
  fi
  exit 0
fi
if [[ $url == *'/.well-known/teagram-web/version.txt' ]]; then
  if [[ $write_format == '%{content_type}' ]]; then printf 'text/plain\n';
  elif [[ $write_format == '%{http_code}' ]]; then printf '200\n'; fi
  exit 0
fi
if [[ $url == *'/.well-known/telegramd/client' ]]; then
  [[ $write_format == '%{http_code}' ]] && printf '404\n'
  exit 0
fi
if [[ $url == *'/.well-known/telegram-web/version.txt' ]]; then
  [[ $write_format == '%{http_code}' ]] && printf '%s\n' "$TEST_RETIRED_STATUS"
  exit 0
fi
echo "unexpected curl URL: $url" >&2
exit 103
CURL

chmod +x "$tmp/bin/docker" "$tmp/bin/curl"
sha=0123456789abcdef0123456789abcdef01234567
valid_rsa_spki='MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEArEJt3/8EjP15mNmKXrysrVgX1ZnbQeQnaS/VA4k4sbyWIqnwbYly5namiP8OZZIB9lwaHQSNNOP9lFMgqsHE5eF8xRMEDYGtqoVqI27vxi8KozBGKt4KTmQgq24ykfLreAjZpwrE/bzOWCzpcjq0ZwblouIDSUQv2r2/hpanj1tFJud4IOeWYxNH6wTBOyDQA6MebsQOcah+cj+n6cQBezy8w4SJDSzwvNod3jr0rX5vDVa6N1x51/G/TR4gFykVR/jjuTxNM/cRT+rYSqa1DYWqEUAGifiGHscdRES5wqAMGWLl9kdsOiH4zje81ZmvVybCIkWZiZcXUIzXZwUKLwIDAQAB'
small_rsa_spki='MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDZlFOztREchWtSDKHNQDUeMz8zHHnWwFS5ZmNVOxpLdTL5WmJg/6rsRRSeVabkvu7WPIbWzxYb3NnCUDW/yOVcudflcrOLiNxjUFJdPX/uGf1aFPlaMCznlwMfZbk6bkYQmbnejRlrMENA+Uy6czv+Lvdcoe/cDV/LKFDDrwILMwIDAQAB'
valid_client_json="{\"version\":1,\"mtproto\":{\"endpoint\":\"mtproto.example.test:2443\",\"dc_id\":2,\"rsa_spki\":\"$valid_rsa_spki\"},\"websocket\":{\"endpoint\":\"wss://mtproto.example.test/apiws\"}}"
export TEST_TARGET_SHA=$sha
export TEST_CLIENT_JSON="$valid_client_json"
export TEST_CLIENT_STATUS=200
export TG_INVITE_WEB_SIGNUP_URL=https://web.example.test/signup
export TG_INVITE_DESKTOP_URL=
export MOCK_DOCKER_STATE="$tmp/state"
export MOCK_DOCKER_LOG="$tmp/docker.log"
export MOCK_COMPOSE_FILE="$tmp/repo/deploy/link-edge/compose.yaml"
export PATH="$tmp/bin:$PATH"

if env -u TG_INVITE_WEB_SIGNUP_URL bash "$script" 0123456789abcdef0123456789abcdef01234567 >"$tmp/missing-target.log" 2>&1; then
  echo 'FAIL deploy accepted a missing invite sign-up target' >&2
  exit 1
fi
[[ ! -s "$tmp/docker.log" ]] || {
  echo 'FAIL deploy contacted Docker before rejecting a missing invite sign-up target' >&2
  exit 1
}

reset_state() {
  rm -f "$tmp/state/"*
  printf '%s\n' "$1" > "$tmp/state/current_revision"
  : > "$tmp/docker.log"
}

reset_state 1111111111111111111111111111111111111111
export TEST_RETIRED_STATUS=404
if ! output=$(bash "$script" "$sha"); then
  cat "$tmp/docker.log" >&2
  exit 1
fi
[[ $output == *"retired path=404"* ]] || { printf 'FAIL successful rollout was not verified: %s\n' "$output" >&2; exit 1; }
[[ $(cat "$tmp/state/current_revision") == "$sha" ]] || { echo 'FAIL selector was not advanced to the tested SHA' >&2; exit 1; }
grep -Fq 'build selector' "$tmp/docker.log" || { echo 'FAIL selector image was not built' >&2; exit 1; }
grep -Fq 'up -d --no-deps --wait --wait-timeout 60 selector' "$tmp/docker.log" || { echo 'FAIL only the selector was not refreshed with a health wait' >&2; exit 1; }
if grep -Eq 'build linklanding|up .*linklanding' "$tmp/docker.log"; then
  echo 'FAIL linklanding was rebuilt or restarted' >&2
  exit 1
fi

assert_invalid_discovery_status() {
  local status=$1 previous=5555555555555555555555555555555555555555
  reset_state "$previous"
  if TEST_CLIENT_STATUS="$status" bash "$script" "$sha" >"$tmp/discovery-status.log" 2>&1; then
    printf 'FAIL discovery document status %s passed the deployment gate\n' "$status" >&2
    exit 1
  fi
  [[ $(cat "$tmp/state/current_revision") == "$previous" ]] || {
    printf 'FAIL status %s did not restore the previous selector\n' "$status" >&2
    exit 1
  }
  grep -Fq "returned $status, expected 200" "$tmp/discovery-status.log" || {
    printf 'FAIL status %s was not reported as a discovery response failure\n' "$status" >&2
    exit 1
  }
}

assert_invalid_discovery_status 201
assert_invalid_discovery_status 302

valid_ipv6_client_json=$(jq -c '.mtproto.endpoint = "[2001:db8::1]:2443" | .mtproto.dc_id = 2147483647 | .websocket.endpoint = "wss://[2001:db8::1]/apiws"' <<<"$valid_client_json")
reset_state 1212121212121212121212121212121212121212
if ! output=$(TEST_CLIENT_JSON="$valid_ipv6_client_json" bash "$script" "$sha"); then
  cat "$tmp/docker.log" >&2
  exit 1
fi
[[ $output == *"retired path=404"* ]] || { printf 'FAIL valid IPv6 discovery document was rejected: %s\n' "$output" >&2; exit 1; }

valid_optional_client_json=$(jq -c '.demo = {guest_sign_up:true} | .turnstile = {site_key:("a" * 256)}' <<<"$valid_client_json")
reset_state 1313131313131313131313131313131313131313
if ! output=$(TEST_CLIENT_JSON="$valid_optional_client_json" bash "$script" "$sha"); then
  cat "$tmp/docker.log" >&2
  exit 1
fi
[[ $output == *"retired path=404"* ]] || { printf 'FAIL valid optional discovery fields were rejected: %s\n' "$output" >&2; exit 1; }

reset_state 2222222222222222222222222222222222222222
export TEST_RETIRED_STATUS=200
if bash "$script" "$sha" >"$tmp/rollback.log" 2>&1; then
  echo 'FAIL retired path regression did not fail the deploy' >&2
  exit 1
fi
[[ $(cat "$tmp/state/current_revision") == 2222222222222222222222222222222222222222 ]] || {
  echo 'FAIL failed route check did not restore the previous selector image' >&2
  exit 1
}
grep -Fq 'previous selector image was restored' "$tmp/rollback.log" || {
  echo 'FAIL failed route check did not report rollback' >&2
  exit 1
}

reset_state "$sha"
export TEST_RETIRED_STATUS=404
output=$(bash "$script" "$sha")
if grep -Fq 'build selector' "$tmp/docker.log"; then
  echo 'FAIL matching selector revision was rebuilt unnecessarily' >&2
  exit 1
fi
[[ $output == *"retired path=404"* ]] || { echo 'FAIL matching revision skipped route verification' >&2; exit 1; }

reset_state 3333333333333333333333333333333333333333
export MOCK_BAD_CONFIG=1
if bash "$script" "$sha" >"$tmp/unapproved-config.log" 2>&1; then
  echo 'FAIL unapproved live Compose config was accepted' >&2
  exit 1
fi
if grep -Fq 'build selector' "$tmp/docker.log"; then
  echo 'FAIL selector was built before rejecting the live Compose config' >&2
  exit 1
fi
unset MOCK_BAD_CONFIG

assert_invalid_discovery() {
  local mutation=$1 invalid_json previous=4444444444444444444444444444444444444444
  invalid_json=$(jq -c "$mutation" <<<"$valid_client_json")
  reset_state "$previous"
  if TEST_CLIENT_JSON="$invalid_json" bash "$script" "$sha" >"$tmp/discovery-validation.log" 2>&1; then
    printf 'FAIL malformed discovery document passed validation: %s\n' "$mutation" >&2
    exit 1
  fi
  [[ $(cat "$tmp/state/current_revision") == "$previous" ]] || {
    printf 'FAIL malformed discovery document did not roll back selector: %s\n' "$mutation" >&2
    exit 1
  }
  grep -Fq 'previous selector image was restored' "$tmp/discovery-validation.log" || {
    printf 'FAIL malformed discovery document did not report selector rollback: %s\n' "$mutation" >&2
    exit 1
  }
}

for mutation in \
  'del(.mtproto)' \
  '.mtproto = []' \
  'del(.mtproto.endpoint)' \
  '.mtproto.endpoint = 2443' \
  '.mtproto.endpoint = ""' \
  '.mtproto.endpoint = "mtproto.example.test"' \
  '.mtproto.endpoint = "mtproto.example.test:not-a-port"' \
  '.mtproto.endpoint = "mtproto.example.test:0"' \
  '.mtproto.endpoint = "mtproto.example.test:65536"' \
  '.mtproto.endpoint = "bad host:2443"' \
  '.mtproto.endpoint = "[2001:db8::1]:not-a-port"' \
  'del(.mtproto.dc_id)' \
  '.mtproto.dc_id = "2"' \
  '.mtproto.dc_id = 2.5' \
  '.mtproto.dc_id = 0' \
  '.mtproto.dc_id = 2147483648' \
  '.mtproto.dc_id = -1' \
  'del(.mtproto.rsa_spki)' \
  '.mtproto.rsa_spki = 2' \
  '.mtproto.rsa_spki = ""' \
  '.mtproto.rsa_spki = "c3BraQ=="' \
  '.mtproto.rsa_spki = "not base64!"' \
  ".mtproto.rsa_spki = \"$small_rsa_spki\"" \
  'del(.websocket)' \
  '.websocket = []' \
  'del(.websocket.endpoint)' \
  '.websocket.endpoint = false' \
  '.websocket.endpoint = ""' \
  '.websocket.endpoint = "https://mtproto.example.test/apiws"' \
  '.websocket.endpoint = "wss://mtproto.example.test:443/apiws"' \
  '.websocket.endpoint = "wss://mtproto.example.test/other"' \
  '.websocket.endpoint = "wss://bad host/apiws"' \
  '.websocket.endpoint = "wss://mtproto.example.test/apiws?host=other.example.test"' \
  '.demo = null' \
  '.demo = []' \
  '.demo = {}' \
  '.demo.guest_sign_up = false' \
  '.demo.guest_sign_up = "true"' \
  '.turnstile = null' \
  '.turnstile = []' \
  '.turnstile = {}' \
  '.turnstile.site_key = null' \
  '.turnstile.site_key = 123' \
  '.turnstile.site_key = ""' \
  '.turnstile.site_key = "bad site key"' \
  '.turnstile.site_key = ("a" * 257)'; do
  assert_invalid_discovery "$mutation"
done

if bash "$script" short >"$tmp/invalid-sha.log" 2>&1; then
  echo 'FAIL invalid commit SHA was accepted' >&2
  exit 1
fi
echo 'link-edge deployment fixtures pass'
