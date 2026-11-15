#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

readonly ACCOUNTS_FILE=/home/qa/.qa-accounts-invite
readonly LOCK_FILE=/home/qa/.qa-accounts-invite.lock
readonly BOT_TOKEN_FILE=/home/qa/.telegram-bot-token
readonly BOT_LOCK_FILE=/home/qa/.telegram-bot-token.lock
readonly AGE_IDENTITY=/home/qa/.config/age/keys.txt
readonly AGE_BIN=/home/qa/.local/bin/age
readonly ORIGINAL_COMMAND=${SSH_ORIGINAL_COMMAND:-}

fail() {
  printf '%s\n' 'qa-invite account writer rejected the request' >&2
  exit 1
}

valid_handle() { [[ "$1" =~ ^[a-z][a-z0-9_]{1,31}$ ]]; }
valid_password() { [[ "$1" =~ ^[0-9a-f]{64}$ ]]; }

[[ "$(id -un)" == qa && "$(id -u)" -eq 1000 ]] || fail
[[ "$ORIGINAL_COMMAND" == health || "$ORIGINAL_COMMAND" == store || "$ORIGINAL_COMMAND" =~ ^remove\ [a-z][a-z0-9_]{1,31}$ ||
   "$ORIGINAL_COMMAND" == bot-health || "$ORIGINAL_COMMAND" == bot-store || "$ORIGINAL_COMMAND" == bot-probe ]] || fail

bot_probe() {
  python3 - "$1" <<'PY'
import json
import pathlib
import sys
import urllib.error
import urllib.request

try:
    token = pathlib.Path(sys.argv[1]).read_text().strip()
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    with opener.open("http://teagram.tailaa4918.ts.net:2447/bot" + token + "/getMe", timeout=5) as response:
        result = json.load(response)
        if response.status != 200 or result.get("ok") is not True or result.get("result", {}).get("username") != "teagram_qa_test_bot":
            raise ValueError("unexpected bot")
except urllib.error.HTTPError as error:
    print(f"qa-bot probe failed: HTTP {error.code}", file=sys.stderr)
    raise SystemExit(1)
except Exception as error:
    if isinstance(error, urllib.error.URLError) and isinstance(error.reason, BaseException):
        error = error.reason
    print(f"qa-bot probe failed: {type(error).__name__}", file=sys.stderr)
    raise SystemExit(1)
print("getMe=200 username=teagram_qa_test_bot")
PY
}

if [[ "$ORIGINAL_COMMAND" == bot-* ]]; then
  [[ -f "$AGE_BIN" && ! -L "$AGE_BIN" && -x "$AGE_BIN" && "$(stat -c '%u:%a' "$AGE_BIN")" == '1000:700' ]] || fail
  [[ -f "$AGE_IDENTITY" && ! -L "$AGE_IDENTITY" && "$(stat -c '%u:%a' "$AGE_IDENTITY")" == '1000:600' ]] || fail
  if [[ -e "$BOT_TOKEN_FILE" || -L "$BOT_TOKEN_FILE" ]]; then
    [[ -f "$BOT_TOKEN_FILE" && ! -L "$BOT_TOKEN_FILE" && "$(stat -c '%u:%a' "$BOT_TOKEN_FILE")" == '1000:600' ]] || fail
  fi
  if [[ -e "$BOT_LOCK_FILE" || -L "$BOT_LOCK_FILE" ]]; then
    [[ -f "$BOT_LOCK_FILE" && ! -L "$BOT_LOCK_FILE" && "$(stat -c '%u:%a' "$BOT_LOCK_FILE")" == '1000:600' ]] || fail
  else
    install -m 0600 -o qa -g qa /dev/null "$BOT_LOCK_FILE"
  fi
  exec 9>> "$BOT_LOCK_FILE"
  flock -x 9
  if [[ "$ORIGINAL_COMMAND" == bot-health ]]; then
    check=$(head -c 4097 | "$AGE_BIN" --decrypt --identity "$AGE_IDENTITY" 2>/dev/null) || fail
    [[ "$check" == '1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA' ]] || fail
    printf '%s\n' 'qa-bot-writer=ready'
    exit 0
  fi
  if [[ "$ORIGINAL_COMMAND" == bot-probe ]]; then
    [[ -f "$BOT_TOKEN_FILE" ]] || fail
    bot_probe "$BOT_TOKEN_FILE"
    exit 0
  fi
  temp=$(mktemp /home/qa/.telegram-bot-token.XXXXXXXX)
  trap 'rm -f -- "$temp"' EXIT
  head -c 4097 | "$AGE_BIN" --decrypt --identity "$AGE_IDENTITY" > "$temp" 2>/dev/null || fail
  [[ "$(wc -c < "$temp")" -le 64 ]] || fail
  [[ "$(wc -l < "$temp")" == 0 ]] || fail
  LC_ALL=C grep -Eq '^[1-9][0-9]*:[A-Za-z0-9_-]{43}$' "$temp" || fail
  bot_probe "$temp" >/dev/null || fail
  chmod 0600 "$temp"
  mv -f -- "$temp" "$BOT_TOKEN_FILE"
  printf '%s\n' 'qa-bot-token=stored'
  exit 0
fi

if [[ ! -e "$ACCOUNTS_FILE" && ! -L "$ACCOUNTS_FILE" ]]; then
  install -m 0600 -o qa -g qa /dev/null "$ACCOUNTS_FILE"
else
  [[ -f "$ACCOUNTS_FILE" && ! -L "$ACCOUNTS_FILE" && "$(stat -c '%u:%a' "$ACCOUNTS_FILE")" == '1000:600' ]] || fail
fi
if [[ -e "$LOCK_FILE" || -L "$LOCK_FILE" ]]; then
  [[ -f "$LOCK_FILE" && ! -L "$LOCK_FILE" && "$(stat -c '%u:%a' "$LOCK_FILE")" == '1000:600' ]] || fail
else
  install -m 0600 -o qa -g qa /dev/null "$LOCK_FILE"
fi
exec 9>> "$LOCK_FILE"
flock -x 9

if [[ "$ORIGINAL_COMMAND" == health ]]; then
  printf '%s\n' 'qa-account-writer=ready'
  exit 0
fi

if [[ "$ORIGINAL_COMMAND" == store ]]; then
  IFS=$'\t' read -r handle password extra || fail
  [[ -z "${extra:-}" ]] || fail
  if ! valid_handle "$handle" || ! valid_password "$password"; then
    fail
  fi
  ! awk -F '\t' -v handle="$handle" '$1 == handle { found=1 } END { exit !found }' "$ACCOUNTS_FILE" || fail
  temp=$(mktemp /home/qa/.qa-accounts-invite.XXXXXXXX)
  trap 'rm -f -- "$temp"' EXIT
  cat -- "$ACCOUNTS_FILE" > "$temp"
  printf '%s\t%s\n' "$handle" "$password" >> "$temp"
  chown qa:qa "$temp"
  chmod 0600 "$temp"
  mv -f -- "$temp" "$ACCOUNTS_FILE"
  printf 'stored=%s\n' "$handle"
  exit 0
fi

handle=${ORIGINAL_COMMAND#remove }
valid_handle "$handle" || fail
temp=$(mktemp /home/qa/.qa-accounts-invite.XXXXXXXX)
trap 'rm -f -- "$temp"' EXIT
awk -F '\t' -v handle="$handle" '$1 != handle { print $0 }' "$ACCOUNTS_FILE" > "$temp"
chown qa:qa "$temp"
chmod 0600 "$temp"
mv -f -- "$temp" "$ACCOUNTS_FILE"
printf 'removed=%s\n' "$handle"
