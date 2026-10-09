#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
readonly ARTIFACT_NAME=.rollout-compose.initial-local.yml
readonly ARTIFACT_SHA=3a4f158c6e1f2ead6676fba85d8d95cfb15557a0fbd8e82230361e0af988e0f7
TMP=$(mktemp -d "${TMPDIR:-/tmp}/telegramd-compose-path-preflight.XXXXXXXX")
chmod 700 "$TMP"
trap 'rm -rf -- "$TMP"' EXIT

export ROLLOUT_RUNNER_SOURCE_ONLY=1
source "$SCRIPT_DIR/rollout-runner.sh"
export ROLLOUT_VERIFIER_SOURCE_ONLY=1
source "$SCRIPT_DIR/rollout-verifier.sh"

pass() {
  printf 'PASS %s\n' "$1"
}

fail_test() {
  printf 'FAIL %s\n' "$1" >&2
  exit 1
}

new_fixture() {
  local name=$1
  FIXTURE="$TMP/$name checkout with spaces"
  mkdir -m 700 "$FIXTURE"
  cp "$SCRIPT_DIR/initial-local-compose-777742.yml" "$FIXTURE/$ARTIFACT_NAME"
  chmod 600 "$FIXTURE/$ARTIFACT_NAME"
  cat > "$FIXTURE/docker-compose.override.yml" <<'YAML'
services:
  telegramd:
    ports:
      - "127.0.0.1:2444:2444"
YAML
  chmod 600 "$FIXTURE/docker-compose.override.yml"
  mkdir "$FIXTURE/normalized"
  CHECKOUT=$FIXTURE
  OVERRIDE_FILE="$FIXTURE/docker-compose.override.yml"
  INITIALIZE_LOCAL=1
  cd "$FIXTURE"
}

assert_selection_passes() {
  local description=$1
  COMPOSE_FILE=$2
  require_compose_override || fail_test "$description was rejected while checking override inclusion"
  verify_initial_local_compose || fail_test "$description was rejected while checking the pinned artifact"
  pass "$description handles the existing artifact and override"
}

assert_selection_rejected() {
  local description=$1 expected=$2
  if ( require_compose_override && verify_initial_local_compose ) 2>"$TMP/$description.stderr"; then
    fail_test "$description was accepted"
  fi
  if ! grep -Fq "$expected" "$TMP/$description.stderr"; then
    cat "$TMP/$description.stderr" >&2
    fail_test "$description failed for an unexpected reason"
  fi
  pass "$description is rejected before rollout work"
}

new_fixture canonical-selection
artifact_relative="$ARTIFACT_NAME"
override_relative=docker-compose.override.yml
assert_selection_passes 'relative paths with spaces' "$artifact_relative:$override_relative"
assert_selection_passes 'absolute paths with spaces' "$FIXTURE/$ARTIFACT_NAME:$OVERRIDE_FILE"
assert_selection_passes 'equivalent normalized paths with spaces' \
  "normalized/../$ARTIFACT_NAME:normalized/../docker-compose.override.yml"

ROLLOUT_CHECKOUT_PATH=$FIXTURE
mode_source=$(canonical_blob_mode_source) || fail_test 'missing blob-mode path could not be canonicalized'
expected_mode_source="$(pwd -P)/.state/blob-mode"
[ "$mode_source" = "$expected_mode_source" ] || fail_test 'blob-mode source did not preserve canonical path semantics'
pass 'initial-local snapshot canonicalizes its not-yet-created state path'
mkdir -m 700 "$TMP/state-target"
ln -s "$TMP/state-target" "$FIXTURE/.state"
mode_source=$(canonical_blob_mode_source) || fail_test 'symlinked state path could not be canonicalized'
expected_mode_source="$(readlink -f "$TMP/state-target")/blob-mode"
[ "$mode_source" = "$expected_mode_source" ] || fail_test 'blob-mode source did not resolve its existing state parent'
pass 'initial-local snapshot resolves an existing state-parent symlink'

COMPOSE_FILE=$artifact_relative
assert_selection_rejected 'omitted-existing-override' 'COMPOSE_FILE omits the existing docker-compose.override.yml'

COMPOSE_FILE="$artifact_relative:$override_relative:missing-compose.yml"
assert_selection_rejected 'missing-compose-input' 'cannot resolve a COMPOSE_FILE entry'

new_fixture broken-override
assert_selection_passes 'broken override passing base' "$ARTIFACT_NAME:docker-compose.override.yml"
rm "$OVERRIDE_FILE"
ln -s missing-override.yml "$OVERRIDE_FILE"
COMPOSE_FILE="$ARTIFACT_NAME:docker-compose.override.yml"
assert_selection_rejected 'broken-override-input' 'cannot resolve the Compose override path'

new_fixture missing-artifact
assert_selection_passes 'missing artifact passing base' "$ARTIFACT_NAME:docker-compose.override.yml"
rm "$FIXTURE/$ARTIFACT_NAME"
COMPOSE_FILE="$ARTIFACT_NAME:docker-compose.override.yml"
assert_selection_rejected 'missing-artifact-input' 'cannot resolve a COMPOSE_FILE entry'

new_fixture symlink-artifact
assert_selection_passes 'symlink artifact passing base' "$ARTIFACT_NAME:docker-compose.override.yml"
mv "$FIXTURE/$ARTIFACT_NAME" "$FIXTURE/artifact-target.yml"
ln -s artifact-target.yml "$FIXTURE/$ARTIFACT_NAME"
COMPOSE_FILE="$ARTIFACT_NAME:docker-compose.override.yml"
assert_selection_rejected 'symlinked-artifact' 'initial-local Compose artifact must be a root-owned mode-0600 regular file'

actual_sha=$(sha256sum "$SCRIPT_DIR/initial-local-compose-777742.yml")
actual_sha=${actual_sha%% *}
[ "$actual_sha" = "$ARTIFACT_SHA" ] || fail_test 'fixture source artifact no longer matches its approved digest'
pass 'approved artifact digest remains the fixture source'
