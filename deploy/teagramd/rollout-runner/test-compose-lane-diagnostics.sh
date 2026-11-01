#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(git -C "$SCRIPT_DIR" rev-parse --show-toplevel)
TMP=$(mktemp -d "${TMPDIR:-/tmp}/teagramd-compose-lane-diagnostics.XXXXXXXX")
chmod 700 "$TMP"
trap 'rm -rf -- "$TMP"' EXIT

pass() {
  printf 'PASS %s\n' "$1"
}

fail_test() {
  printf 'FAIL %s\n' "$1" >&2
  exit 1
}

# The stub stands in for the Docker CLI. A quiet build that fails reports only
# its exit code, as BuildKit does; a full build reports the step that broke.
# Compose cannot interpolate the stack without .env, as on a real runner. The
# helpers run Docker under env -i, so the call log path is written in.
readonly STUB_DOCKER_CALLS="$TMP/docker-calls"
mkdir "$TMP/bin"
{
  printf '#!/usr/bin/env bash\n'
  printf 'calls=%q\n' "$STUB_DOCKER_CALLS"
  cat <<'SH'
printf '%s\n' "$*" >> "$calls"
case " $* " in
  *" build "*)
    case " $* " in
      *" -q "*) printf '%s\n' 'failed to solve: exit code: 7' >&2 ;;
      *)
        printf '%s\n' '#12 [builder 4/8] RUN go mod download'
        printf '%s\n' '#12 ERROR: go: stub module download failure' >&2
        ;;
    esac
    exit 7
    ;;
  *" down "*)
    [ -e .env ] || {
      printf '%s\n' 'required variable POSTGRES_PASSWORD is missing a value' >&2
      exit 1
    }
    ;;
esac
SH
} > "$TMP/bin/docker"
chmod 700 "$TMP/bin/docker"

for helper in test-local-compose-5602c7c.sh test-local-compose-cce00d1.sh; do
  definition=$(sed -n '/^compose_build() {$/,/^}$/p' "$SCRIPT_DIR/$helper")
  [ -n "$definition" ] || fail_test "$helper defines no compose_build"
  : > "$STUB_DOCKER_CALLS"
  status=0
  # The 5602c7c helper reads the worktree from WORKTREE, cce00d1 from its
  # first argument; each ignores the variable it does not use.
  PATH="$TMP/bin:$PATH" TMP="$TMP" DOCKER_HOME="$TMP" DOCKER_CONFIG="$TMP" DOCKER_ENDPOINT= \
    WORKTREE="$TMP" ARTIFACT_NAME=artifact.yml bash -c "
      set -Eeuo pipefail
      $definition
      if [ $helper = test-local-compose-5602c7c.sh ]; then
        compose_build override.yml REVISION=1
      else
        compose_build \"\$TMP\" override.yml REVISION=1
      fi
      printf '%s\n' 'build failure did not stop the helper'
    " > "$TMP/stdout" 2> "$TMP/stderr" || status=$?
  [ "$status" = 7 ] || fail_test "$helper compose_build exited $status, not the build's 7"
  grep -Fq 'ERROR: go: stub module download failure' "$TMP/stderr" \
    || fail_test "$helper did not report the failed build's own error"
  grep -Fq 'RUN go mod download' "$TMP/stderr" \
    || fail_test "$helper did not report which build step failed"
  [ ! -s "$TMP/stdout" ] || fail_test "$helper continued or wrote to stdout after a failed build"
  grep -Fq ' build telegramd' "$STUB_DOCKER_CALLS" || fail_test "$helper did not build telegramd"
  pass "$helper reports the failed build's error and exit status"
done

# The compose job's Tear down step, read from the workflow so the test covers
# what CI runs.
teardown=$(awk '
  /^  test-compose:$/ { job = 1; next }
  job && /^  [^ ]/ { exit }
  job && /^      - name: Tear down$/ { step = 1; next }
  step && /^        run: \|$/ { body = 1; next }
  body && /^          / { print substr($0, 11); next }
  body { exit }
' "$REPO_ROOT/.github/workflows/ci.yml")
grep -Fq 'docker compose down -v' <<< "$teardown" || fail_test 'compose job Tear down step not found'

mkdir "$TMP/no-env" "$TMP/with-env"
: > "$TMP/with-env/.env"

: > "$STUB_DOCKER_CALLS"
status=0
(cd "$TMP/no-env" && PATH="$TMP/bin:$PATH" bash -e -c "$teardown") > "$TMP/stdout" 2> "$TMP/stderr" || status=$?
[ "$status" = 0 ] || fail_test "teardown without .env exited $status: $(cat "$TMP/stderr")"
[ ! -s "$TMP/stderr" ] || fail_test "teardown without .env reported an error: $(cat "$TMP/stderr")"
[ ! -s "$STUB_DOCKER_CALLS" ] || fail_test 'teardown without .env invoked Docker'
pass 'teardown without .env skips Compose without an error'

: > "$STUB_DOCKER_CALLS"
status=0
(cd "$TMP/with-env" && PATH="$TMP/bin:$PATH" bash -e -c "$teardown") > "$TMP/stdout" 2> "$TMP/stderr" || status=$?
[ "$status" = 0 ] || fail_test "teardown with .env exited $status: $(cat "$TMP/stderr")"
[ "$(cat "$STUB_DOCKER_CALLS")" = 'compose down -v' ] \
  || fail_test "teardown with .env ran <$(cat "$STUB_DOCKER_CALLS")>, not compose down -v"
pass 'teardown with .env runs compose down -v'
