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
ID_B=0123456789abbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
SHORT_A="${ID_A:0:12}"
SHORT_C=cafe0123abcd
BAD_REF=fe9876543210

pass() {
  printf 'PASS %s\n' "$1"
}

fail() {
  printf 'FAIL %s\n' "$1" >&2
  exit 1
}

# The before side is a captured identity and must never be resolved, so the
# expected inspect count is the after side only.
run_compare() {
  local name=$1 expected=$2 expected_calls=$3 before_ref=$4 before_id=$5 after_ref=$6 after_id=$7
  local fail_after=${8:-0} expected_stderr=${9:-}
  local rc calls
  : > "$TMP_DIR/docker-calls"
  if PATH="$MOCK_BIN:$PATH" \
    MOCK_DOCKER_CALLS="$TMP_DIR/docker-calls" \
    MOCK_REF_A="$before_ref" MOCK_ID_A="$before_id" MOCK_FAIL_A=0 \
    MOCK_REF_B="$after_ref" MOCK_ID_B="$after_id" MOCK_FAIL_B="$fail_after" \
    "$GATE" compare "$before_ref" "$after_ref" > "$TMP_DIR/stdout" 2> "$TMP_DIR/stderr"; then
    rc=0
  else
    rc=$?
  fi
  calls="$(wc -l < "$TMP_DIR/docker-calls")"
  [[ "$calls" -eq "$expected_calls" ]] \
    || { cat "$TMP_DIR/docker-calls" >&2; fail "$name unexpected inspect count (calls=$calls want=$expected_calls)"; }
  if [[ "$expected" == pass && "$rc" -eq 0 ]]; then
    [[ "$(cat "$TMP_DIR/stdout")" == "container_identity=unchanged id=$before_ref" ]] \
      || fail "$name must report the captured complete ID"
    pass "$name"
  elif [[ "$expected" == reject && "$rc" -ne 0 ]]; then
    if [[ -n "$expected_stderr" ]]; then
      grep -qF -- "$expected_stderr" "$TMP_DIR/stderr" \
        || { cat "$TMP_DIR/stderr" >&2; fail "$name missing rejection reason"; }
    fi
    pass "$name"
  else
    cat "$TMP_DIR/stdout" "$TMP_DIR/stderr" >&2
    fail "$name expected=$expected exit=$rc"
  fi
}

run_capture() {
  local name=$1 reference=$2 full_id=$3
  local rc calls
  : > "$TMP_DIR/docker-calls"
  if PATH="$MOCK_BIN:$PATH" \
    MOCK_DOCKER_CALLS="$TMP_DIR/docker-calls" \
    MOCK_REF_A="$reference" MOCK_ID_A="$full_id" MOCK_FAIL_A=0 \
    MOCK_REF_B=unused MOCK_ID_B=unused MOCK_FAIL_B=1 \
    "$GATE" id "$reference" > "$TMP_DIR/stdout" 2> "$TMP_DIR/stderr"; then
    rc=0
  else
    rc=$?
  fi
  calls="$(wc -l < "$TMP_DIR/docker-calls")"
  if [[ "$rc" -eq 0 && "$calls" -eq 1 && "$(cat "$TMP_DIR/stdout")" == "$full_id" ]]; then
    pass "$name"
  else
    cat "$TMP_DIR/stdout" "$TMP_DIR/stderr" >&2
    fail "$name expected one inspect and the complete ID (exit=$rc calls=$calls)"
  fi
}

run_capture 'id prints the complete ID for a short reference' "$SHORT_A" "$ID_A"
run_capture 'id prints the complete ID for a full reference' "$ID_A" "$ID_A"

run_compare 'unchanged identity captured as a full ID and resolved after reference' pass \
  1 "$ID_A" "$ID_A" "$SHORT_A" "$ID_A"
run_compare 'changed full identities sharing a 12-character prefix reject' reject \
  1 "$ID_A" "$ID_A" "$ID_B" "$ID_B" 0 'container identity changed'
run_compare 'malformed inspected ID rejects' reject \
  1 "$ID_A" "$ID_A" "$BAD_REF" short 0 'did not return a complete container ID'
run_compare 'inspected ID not matching its reference rejects' reject \
  1 "$ID_A" "$ID_A" "$SHORT_C" "$ID_A" 0 'inspected container ID does not match its reference'
run_compare 'Docker inspect failure rejects' reject \
  1 "$ID_A" "$ID_A" "$SHORT_A" "$ID_A" 1 'cannot resolve complete Docker container ID'
run_compare 'saved short before reference that now resolves to a replacement container rejects' reject \
  0 "$SHORT_A" "$ID_B" "$ID_B" "$ID_B" 0 'complete 64-character Docker container ID'
run_compare 'truncated saved before identity rejects without any inspect' reject \
  0 "${ID_A:0:63}" "$ID_A" "$ID_A" "$ID_A" 0 'complete 64-character Docker container ID'
run_compare 'malformed saved before identity rejects without any inspect' reject \
  0 "zz${ID_A:2}" "$ID_A" "$ID_A" "$ID_A" 0 'complete 64-character Docker container ID'

printf 'container_identity_fixture=passed\n'
