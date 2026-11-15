#!/bin/sh
set -eu
set +x
umask 077

readonly TEAGRAMD=/usr/local/bin/teagramd
readonly BOT_USERNAME=teagram_qa_test_bot
readonly BOT_FIRST_NAME='Teagram QA Test'
readonly AGE_RECIPIENT=age1p5s5le72wnxu465pe2xz5a3qv8mdykwp5leezpjk0zm6p0m3e5hs36fydn

fail() {
  printf 'qa-bot: %s\n' "$1" >&2
  exit 1
}

[ "$#" -eq 1 ] || { printf '%s\n' 'usage: qa-bot-operation <create|rotate|revoke|status|probe>' >&2; exit 64; }
mode=$1
case "$mode" in create|rotate|revoke|status|probe) ;; *) exit 64 ;; esac
[ "$(id -u)" = 0 ] || fail 'operation requires root in the isolated job'

lookup() {
  lookup_error='bot lookup command failed'
  json=$("$TEAGRAMD" bot lookup "$BOT_USERNAME" 2>/dev/null) || return 1
  if [ "$json" = '{"state":"missing"}' ]; then
    bot_state=missing
    bot_id=
    bot_active=false
    return
  fi
  # The CLI encodes its lookup map with these sorted keys. Accept only the
  # complete, expected bot record; an unexpected account fails closed.
  lookup_error='pinned username is not the expected QA bot'
  record=$(printf '%s\n' "$json" | sed -nE \
    's/^\{"first_name":"Teagram QA Test","id":([1-9][0-9]*),"kind":"bot","owner":"user","state":"found","token_active":(true|false)\}$/\1 \2/p')
  [ -n "$record" ] || return 1
  bot_id=${record% *}
  bot_active=${record#* }
  bot_state=found
}

revoke_issued() {
  if ! "$TEAGRAMD" bot revoke "$1" >/dev/null 2>&1; then
    fail "new token could not be delivered or revoked; reconcile bot id $1"
  fi
  fail "new token could not be delivered; bot id $1 was revoked"
}

if [ "$mode" = create ] || [ "$mode" = rotate ]; then
  "$TEAGRAMD" bot encrypt --check-recipient "$AGE_RECIPIENT" >/dev/null 2>&1 || fail 'QA age recipient is unavailable'
fi
lookup || fail "$lookup_error"
case "$mode" in
  status)
    if [ "$bot_state" = missing ]; then
      printf 'state=missing username=%s\n' "$BOT_USERNAME"
    else
      printf 'state=found bot_id=%s username=%s token_active=%s\n' "$bot_id" "$BOT_USERNAME" "$bot_active"
    fi
    exit 0
    ;;
  probe)
    if [ "$bot_state" != found ] || [ "$bot_active" != true ]; then
      fail 'QA bot has no active token'
    fi
    printf 'state=found bot_id=%s username=%s token_active=true\n' "$bot_id" "$BOT_USERNAME"
    exit 0
    ;;
  create)
    if [ "$bot_state" = found ]; then
      [ "$bot_active" = true ] || fail 'existing QA bot has no active token; use rotate'
      printf 'state=exists bot_id=%s username=%s token_active=%s\n' "$bot_id" "$BOT_USERNAME" "$bot_active"
      exit 0
    fi
    token=$("$TEAGRAMD" bot create "$BOT_USERNAME" "$BOT_FIRST_NAME" 2>/dev/null) || fail 'create failed'
    ;;
  rotate)
    [ "$bot_state" = found ] || fail 'QA bot does not exist'
    expected_id=$bot_id
    token=$("$TEAGRAMD" bot rotate "$expected_id" 2>/dev/null) || fail 'rotation failed'
    ;;
  revoke)
    if [ "$bot_state" = missing ]; then
      printf 'state=missing username=%s action=revoke\n' "$BOT_USERNAME"
    elif [ "$bot_active" = false ]; then
      printf 'state=revoked bot_id=%s username=%s action=revoke\n' "$bot_id" "$BOT_USERNAME"
    else
      "$TEAGRAMD" bot revoke "$bot_id" >/dev/null 2>&1 || fail 'revocation failed'
      printf 'state=revoked bot_id=%s username=%s action=revoke\n' "$bot_id" "$BOT_USERNAME"
    fi
    exit 0
    ;;
esac

if ! printf '%s\n' "$token" | grep -Eq '^[1-9][0-9]*:[A-Za-z0-9_-]{43}$'; then
  unset token
  if [ "$mode" = rotate ]; then revoke_issued "$expected_id"; fi
  fail 'create returned an invalid token; reconcile the pinned bot'
fi
token_id=${token%%:*}
if [ "$mode" = rotate ] && [ "$token_id" != "$expected_id" ]; then
  unset token
  revoke_issued "$expected_id"
fi

# The create command returns the new bot's ID in its token. A failed lookup
# must revoke that newly issued token; a successful lookup must agree with it.
if ! lookup; then
  unset token
  printf 'qa-bot: %s\n' "$lookup_error" >&2
  if [ "$mode" = rotate ]; then revoke_issued "$expected_id"; fi
  revoke_issued "$token_id"
fi
if [ "$bot_state" != found ] || [ "$bot_id" != "$token_id" ] || [ "$bot_active" != true ]; then
  unset token
  if [ "$mode" = rotate ]; then revoke_issued "$expected_id"; fi
  fail 'issued token does not match the pinned bot; reconcile its state'
fi
if ! encrypted=$(printf '%s\n' "$token" | "$TEAGRAMD" bot encrypt "$AGE_RECIPIENT" 2>/dev/null); then
  unset token
  revoke_issued "$token_id"
fi
unset token
case "$encrypted" in
  *'-----BEGIN AGE ENCRYPTED FILE-----'*'-----END AGE ENCRYPTED FILE-----'*) ;;
  *) unset encrypted; revoke_issued "$token_id" ;;
esac
if ! { printf 'action=%s bot_id=%s username=%s\n' "$mode" "$token_id" "$BOT_USERNAME" && printf '%s\n' "$encrypted"; }; then
  unset encrypted
  revoke_issued "$token_id"
fi
unset encrypted
