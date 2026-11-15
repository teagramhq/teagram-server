#!/usr/bin/env bash
set -Eeuo pipefail

report_failure() {
  local job=$1 reason logs line found=false
  reason=$(jq -r '[.status.conditions[]? | select((.type == "Failed" or .type == "FailureTarget") and .status == "True") | .reason][0] // ""' <<< "$job")
  case "$reason" in
    BackoffLimitExceeded|DeadlineExceeded|PodFailurePolicy) printf 'qa-bot: job failed (%s)\n' "$reason" >&2 ;;
    *) printf '%s\n' 'qa-bot: job did not complete' >&2 ;;
  esac
  if ! logs=$(kubectl logs job/qa-bot-operation -c qa-bot -n teagram --tail=40 --limit-bytes=16384 --request-timeout=10s 2>/dev/null); then
    printf '%s\n' 'qa-bot: operation logs unavailable' >&2
    return
  fi
  while IFS= read -r line; do
    case "$line" in
      'qa-bot: operation requires root in the isolated job'|\
      'qa-bot: QA age recipient is unavailable'|\
      'qa-bot: lookup failed or pinned username is not the expected QA bot'|\
      'qa-bot: QA bot has no active token'|\
      'qa-bot: create failed'|\
      'qa-bot: QA bot does not exist'|\
      'qa-bot: rotation failed'|\
      'qa-bot: revocation failed'|\
      'qa-bot: create returned an invalid token; reconcile the pinned bot'|\
      'qa-bot: could not verify the issued token; reconcile the pinned bot'|\
      'qa-bot: issued token does not match the pinned bot; reconcile its state')
        printf '%s\n' "$line" >&2
        found=true
        ;;
      *)
        if [[ "$line" =~ ^qa-bot:\ new\ token\ could\ not\ be\ delivered(\ or\ revoked\;\ reconcile\ bot\ id|\;\ bot\ id)\ [1-9][0-9]*(\ was\ revoked)?$ ]]; then
          printf '%s\n' "$line" >&2
          found=true
        fi
        ;;
    esac
  done <<< "$logs"
  if [[ "$found" == false ]]; then
    printf '%s\n' 'qa-bot: no recognized operation diagnostic' >&2
  fi
}

deadline=$((SECONDS + 240))
while (( SECONDS < deadline )); do
  if ! job=$(kubectl get job qa-bot-operation -n teagram -o json --request-timeout=10s 2>/dev/null); then
    printf '%s\n' 'qa-bot: could not query job status' >&2
    exit 1
  fi
  if ! state=$(jq -er '
    if any(.status.conditions[]?; (.type == "Failed" or .type == "FailureTarget") and .status == "True") or ((.status.failed // 0) > 0) then "failed"
    elif any(.status.conditions[]?; .type == "Complete" and .status == "True") then "complete"
    else "pending" end
  ' <<< "$job" 2>/dev/null); then
    printf '%s\n' 'qa-bot: invalid job status' >&2
    exit 1
  fi
  case "$state" in
    complete) exit 0 ;;
    failed) report_failure "$job"; exit 1 ;;
  esac
  sleep 2
done
printf '%s\n' 'qa-bot: job timed out' >&2
report_failure "$job"
exit 1
