#!/usr/bin/env bash
set -euo pipefail
umask 077

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
GATE="$SCRIPT_DIR/container-identity-gate.sh"
TMP_DIR="$(mktemp -d)"
MOCK_BIN="$TMP_DIR/bin"
mkdir -m 700 "$MOCK_BIN"
trap 'rm -rf -- "$TMP_DIR"' EXIT

[[ -x "$GATE" ]] || { printf 'FAIL container identity gate is missing or not executable\n' >&2; exit 1; }

cat > "$MOCK_BIN/docker" <<'MOCK_DOCKER'
#!/usr/bin/env bash
set -euo pipefail

[[ "$#" -eq 4 && "$1" == inspect && "$2" == --format && "$3" == '{{.Id}}' ]] || exit 64
printf '%s\n' "$4" >> "$MOCK_DOCKER_CALLS"
case "$4" in
  "$MOCK_REF_A")
    [[ "${MOCK_FAIL_A:-0}" == 0 ]] || exit 1
    printf '%s\n' "$MOCK_ID_A"
    ;;
  "$MOCK_REF_B")
    [[ "${MOCK_FAIL_B:-0}" == 0 ]] || exit 1
    printf '%s\n' "$MOCK_ID_B"
    ;;
  *) exit 1 ;;
esac
MOCK_DOCKER
chmod 700 "$MOCK_BIN/docker"

ID_A=0123456789abaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
ID_B=0123456789abbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
SHORT_A="${ID_A:0:12}"
BAD_REF=fe9876543210

pass() {
  printf 'PASS %s\n' "$1"
}

fail() {
  printf 'FAIL %s\n' "$1" >&2
  exit 1
}

run_pair() {
  local name=$1 expected=$2 expected_calls=$3 ref_a=$4 ref_b=$5 id_a=$6 id_b=$7 fail_a=$8 fail_b=$9 rc calls
  : > "$TMP_DIR/docker-calls"
  if PATH="$MOCK_BIN:$PATH" \
    MOCK_DOCKER_CALLS="$TMP_DIR/docker-calls" \
    MOCK_REF_A="$ref_a" MOCK_REF_B="$ref_b" \
    MOCK_ID_A="$id_a" MOCK_ID_B="$id_b" \
    MOCK_FAIL_A="$fail_a" MOCK_FAIL_B="$fail_b" \
    "$GATE" compare "$ref_a" "$ref_b" > "$TMP_DIR/stdout" 2> "$TMP_DIR/stderr"; then
    rc=0
  else
    rc=$?
  fi
  calls="$(wc -l < "$TMP_DIR/docker-calls")"
  [[ "$calls" -eq "$expected_calls" ]] || fail "$name unexpected inspect count (calls=$calls)"
  if [[ "$expected" == pass && "$rc" -eq 0 ]]; then
    [[ "$(cat "$TMP_DIR/stdout")" == "container_identity=unchanged id=$id_a" ]] \
      || fail "$name must report the complete inspected ID"
    pass "$name"
  elif [[ "$expected" == reject && "$rc" -ne 0 ]]; then
    pass "$name"
  else
    cat "$TMP_DIR/stdout" "$TMP_DIR/stderr" >&2
    fail "$name expected=$expected exit=$rc"
  fi
}

run_pair 'unchanged full identity resolved from short references' pass \
  2 "$SHORT_A" "$SHORT_A" "$ID_A" "$ID_A" 0 0
run_pair 'changed full identities sharing a 12-character prefix reject' reject \
  2 "$ID_A" "$ID_B" "$ID_A" "$ID_B" 0 0
run_pair 'malformed inspected ID rejects' reject \
  2 "$SHORT_A" "$BAD_REF" "$ID_A" short 0 0
run_pair 'inspected ID not matching its reference rejects' reject \
  1 "$ID_A" "$ID_A" "$ID_B" "$ID_A" 0 0
run_pair 'Docker inspect failure rejects' reject \
  1 "$SHORT_A" "$SHORT_A" "$ID_A" "$ID_A" 1 0
