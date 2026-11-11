#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
runner="$repo_root/deploy/k8s/initialize-channel-post-expiring-summaries.sh"
work="$(mktemp -d)"
trap 'rm -rf -- "$work"' EXIT
bin="$work/bin"
mkdir -p "$bin"

cat > "$bin/kubectl" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail

command="$1"
shift
printf '%s %s\n' "$command" "$*" >> "$MOCK_CALLS"
case "$command" in
  create)
    manifest=''
    while (($#)); do
      if [[ "$1" == --filename ]]; then
        manifest="$2"
        shift 2
      else
        shift
      fi
    done
    [[ -s "$manifest" ]]
    grep --fixed-strings --quiet 'image: ghcr.io/teagramhq/teagramd:ci' "$manifest"
    grep --fixed-strings --quiet 'initialize-channel-post-expiring-summaries' "$manifest"
    grep --fixed-strings --quiet 'key: TG_POSTGRES_DSN' "$manifest"
    grep --fixed-strings --quiet 'claimName: key-data-teagramd-0' "$manifest"
    ! grep --fixed-strings --quiet 'image: ghcr.io/teagramhq/teagramd:unreleased' "$manifest"
    count="$(cat "$MOCK_STATE")"
    count=$((count + 1))
    printf '%s' "$count" > "$MOCK_STATE"
    printf 'expiring-summary-init-%03d\n' "$count"
    ;;
  get)
    if [[ "${MOCK_MODE:-success}" == failure ]]; then
      printf 'Failed=True\n'
    else
      printf 'Complete=True\n'
    fi
    ;;
  logs)
    job_name="${1#job/}"
    batch="${job_name##*-}"
    case "${MOCK_MODE:-success}" in
      success)
        if [[ "$batch" == 001 ]]; then
          printf 'Channel post expiring summaries initialized: 100 channels, 1 remaining\n'
        else
          printf 'Channel post expiring summaries initialized: 1 channels, 0 remaining\n'
        fi
        ;;
      no-progress)
        printf 'Channel post expiring summaries initialized: 0 channels, 1 remaining\n'
        ;;
      malformed)
        printf 'maintenance completed without a remaining count\n'
        ;;
      failure)
        printf 'fixture job failure\n'
        ;;
      *)
        printf 'unexpected mock mode: %s\n' "$MOCK_MODE" >&2
        exit 2
        ;;
    esac
    ;;
  describe)
    printf 'mock job description\n'
    ;;
  patch)
    printf 'mock suspended job\n'
    ;;
  *)
    printf 'unexpected kubectl command: %s\n' "$command" >&2
    exit 2
    ;;
esac
MOCK
chmod 0755 "$bin/kubectl"

fail() {
  printf 'expiring-summary maintenance test: %s\n' "$1" >&2
  exit 1
}

image=ghcr.io/teagramhq/teagramd:ci
export PATH="$bin:$PATH"
export TMPDIR="$work"
export MOCK_CALLS="$work/calls.log"

MOCK_STATE="$work/success-count" MOCK_MODE=success
export MOCK_STATE MOCK_MODE
printf '0' > "$MOCK_STATE"
if ! "$runner" "$image" > "$work/success.out" 2>&1; then
  cat "$work/success.out" >&2
  fail 'the runner did not complete two maintenance batches'
fi
[[ "$(cat "$MOCK_STATE")" == 2 ]] || fail 'the runner stopped before the remaining count reached zero'
grep --fixed-strings --quiet \
  'expiring-summary maintenance=verified remaining=0 batches=2' "$work/success.out" ||
  fail 'the runner did not report the zero-remaining completion state'

if "$runner" 'ghcr.io/teagramhq/teagramd:untrusted' > "$work/invalid-image.out" 2>&1; then
  fail 'the runner accepted an unverified image reference'
fi

for mode in no-progress malformed failure; do
  MOCK_STATE="$work/$mode-count" MOCK_MODE="$mode"
  export MOCK_STATE MOCK_MODE
  printf '0' > "$MOCK_STATE"
  if "$runner" "$image" > "$work/$mode.out" 2>&1; then
    fail "$mode response unexpectedly succeeded"
  fi
  [[ "$(cat "$MOCK_STATE")" == 1 ]] || fail "$mode response did not stop after its first batch"
done

printf 'Expiring channel-post summary maintenance contract passed\n'
