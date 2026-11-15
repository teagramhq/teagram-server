#!/usr/bin/env bash
set -Eeuo pipefail

root=$(cd -P "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/qa-bot-k8s.XXXXXXXX")
cleanup() {
  local status=$?
  trap - EXIT
  kubectl delete job qa-bot-operation -n teagram --ignore-not-found --cascade=foreground --wait=true --timeout=60s >/dev/null || status=1
  case "$tmp" in */qa-bot-k8s.*) rm -rf -- "$tmp" ;; *) status=1 ;; esac
  exit "$status"
}
trap cleanup EXIT

operation() {
  local mode=$1
  python3 "$root/deploy/k8s/render-qa-bot-job.py" ghcr.io/teagramhq/teagramd:ci "$mode" > "$tmp/job.json"
  kubectl create --dry-run=server -f "$tmp/job.json" >/dev/null
  kubectl create -f "$tmp/job.json" >/dev/null
  bash "$root/deploy/k8s/wait-qa-bot-job.sh"
  kubectl logs job/qa-bot-operation -c qa-bot -n teagram > "$tmp/$mode"
  if grep -Eq '[1-9][0-9]*:[A-Za-z0-9_-]{43}' "$tmp/$mode"; then
    printf '%s\n' 'QA bot job exposed a token in its logs' >&2
    return 1
  fi
  kubectl delete job qa-bot-operation -n teagram --cascade=foreground --wait=true --timeout=60s >/dev/null
}

operation create
grep -Eq '^action=create bot_id=[1-9][0-9]* username=teagram_qa_test_bot$' "$tmp/create"
grep -Fxq -- '-----BEGIN AGE ENCRYPTED FILE-----' "$tmp/create"
operation status
grep -Eq '^state=found bot_id=[1-9][0-9]* username=teagram_qa_test_bot token_active=true$' "$tmp/status"
operation probe
grep -Eq '^state=found bot_id=[1-9][0-9]* username=teagram_qa_test_bot token_active=true$' "$tmp/probe"
operation rotate
grep -Eq '^action=rotate bot_id=[1-9][0-9]* username=teagram_qa_test_bot$' "$tmp/rotate"
grep -Fxq -- '-----BEGIN AGE ENCRYPTED FILE-----' "$tmp/rotate"
operation revoke
grep -Eq '^state=revoked bot_id=[1-9][0-9]* username=teagram_qa_test_bot action=revoke$' "$tmp/revoke"
operation status
grep -Eq '^state=found bot_id=[1-9][0-9]* username=teagram_qa_test_bot token_active=false$' "$tmp/status"
python3 "$root/deploy/k8s/render-qa-bot-job.py" ghcr.io/teagramhq/teagramd:ci probe > "$tmp/job.json"
kubectl create -f "$tmp/job.json" >/dev/null
if bash "$root/deploy/k8s/wait-qa-bot-job.sh" > "$tmp/failed-probe" 2>&1; then
  printf '%s\n' 'QA bot probe unexpectedly succeeded after revocation' >&2
  exit 1
fi
grep -Fxq 'qa-bot: QA bot has no active token' "$tmp/failed-probe"
kubectl delete job qa-bot-operation -n teagram --cascade=foreground --wait=true --timeout=60s >/dev/null
printf '%s\n' 'QA bot job passed against disposable k3s'
