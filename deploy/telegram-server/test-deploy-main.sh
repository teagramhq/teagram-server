#!/usr/bin/env bash
# Fixture test for deploy-main.sh's target checks: a fake origin and live
# checkout, and a fake docker that reports a pinned compose and image id.
set -euo pipefail
umask 077

script="$(cd "$(dirname "$0")" && pwd)/deploy-main.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

export GIT_AUTHOR_NAME=fixture GIT_AUTHOR_EMAIL=fixture@example.invalid
export GIT_COMMITTER_NAME=fixture GIT_COMMITTER_EMAIL=fixture@example.invalid
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

git init -q -b main "$tmp/work"
mkdir -p "$tmp/work/deploy/telegramd/rollout-runner"
printf 'tracked fixture\n' > "$tmp/work/tracked.txt"
printf '#!/usr/bin/env bash\n' > "$tmp/work/tracked-script.sh"
cat > "$tmp/work/deploy/telegramd/rollout-runner/rollout-runner.sh" <<'RUNNER'
#!/usr/bin/env bash
set -euo pipefail
git -C "$DEPLOY_DIR" merge --ff-only -q "$2"
printf 'source_sha=%s image_id=sha256:fixture built_at=fixture\n' "$2" \
  > "$DEPLOY_ROOT/main1238-fixture.build/built-image.identity"
RUNNER
for file in rollout-verifier.sh schema-result-gate.sh schema-result-gate.py blob-mode-state.py stack-move-runner.py; do
  printf 'fixture\n' > "$tmp/work/deploy/telegramd/rollout-runner/$file"
done
chmod 644 "$tmp/work/tracked.txt"
chmod 755 "$tmp/work/tracked-script.sh"
chmod 644 "$tmp/work/deploy/telegramd/rollout-runner/"*
git -C "$tmp/work" add tracked.txt tracked-script.sh deploy/telegramd/rollout-runner
commit() {
  git -C "$tmp/work" commit -q --allow-empty -m "$1"
  git -C "$tmp/work" rev-parse HEAD
}
first=$(commit first)
second=$(commit second)
git -C "$tmp/work" checkout -q -b side "$first"
side=$(commit side)
git -C "$tmp/work" checkout -q main
git clone -q --bare "$tmp/work" "$tmp/origin.git"
git clone -q "$tmp/origin.git" "$tmp/live"
git -C "$tmp/work" push -q "$tmp/origin.git" main
printf 'third revision\n' > "$tmp/work/tracked.txt"
printf 'new tracked fixture\n' > "$tmp/work/new-tracked.txt"
chmod 644 "$tmp/work/new-tracked.txt"
git -C "$tmp/work" add new-tracked.txt
third=$(commit third)
git -C "$tmp/work" push -q "$tmp/origin.git" main
git -C "$tmp/live" reset -q --hard "$second"
printf 'TG_RSA_KEY_FINGERPRINT=12345\n' > "$tmp/live/.env"
printf 'fixture dump\n' > "$tmp/live/pgdump-fixture.sql"
printf 'fixture evidence\n' > "$tmp/live/rollout-evidence.txt"
chmod 640 "$tmp/live/.env"
chmod 600 "$tmp/live/pgdump-fixture.sql" "$tmp/live/rollout-evidence.txt"
local_files_before=$(stat -c '%n:%u:%g:%a' \
  "$tmp/live/.env" "$tmp/live/pgdump-fixture.sql" "$tmp/live/rollout-evidence.txt")

mkdir -p "$tmp/bin" "$tmp/root/main1238-fixture.build"
cat > "$tmp/bin/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  *config_files*) echo "/opt/telegram-server/.rollout-compose.local-abc.yml,/opt/telegram-server/docker-compose.override.yml" ;;
  *'{{.Image}}'*) echo "sha256:fixture" ;;
  *'pg_dump'*)
    [[ "${FIXTURE_DEPLOY_SUCCESS:-0}" == 1 ]] || { echo "unexpected docker call: $*" >&2; exit 99; }
    printf 'fixture dump complete\n'
    ;;
  *org.opencontainers.image.revision*) echo "$DEPLOY_SHA" ;;
  *'{{range .Config.Env}}'*) echo 'TG_RSA_KEY_FINGERPRINT=12345' ;;
  *'compose ps -a'*) ;;
  *'compose logs --since 3m telegramd'*) echo 'advertise=teagram.tailaa4918.ts.net:2443' ;;
  *) echo "unexpected docker call: $*" >&2; exit 99 ;;
esac
DOCKER
chmod 755 "$tmp/bin/docker"

failures=0
# expect <exit code> <last line> [VAR=value...]
expect() {
  local want_rc="$1" want_last="$2" want_contains='' out rc=0
  shift 2
  if [[ "${1:-}" == --contains ]]; then
    want_contains="$2"
    shift 2
  fi
  out=$(env "$@" PATH="$tmp/bin:$PATH" DEPLOY_DIR="$tmp/live" DEPLOY_ROOT="$tmp/root" bash "$script" 2>&1) || rc=$?
  if [[ "$rc" != "$want_rc" || "${out##*$'\n'}" != "$want_last" || \
      ( -n "$want_contains" && "$out" != *"$want_contains"* ) ]]; then
    printf 'FAIL %s: want rc=%s last=%q, got rc=%s output:\n%s\n' "$*" "$want_rc" "$want_last" "$rc" "$out" >&2
    failures=$((failures + 1))
  fi
}

bad='DEPLOY_SHA must be a full 40-hex commit SHA'
expect 1 "$bad"
expect 1 "$bad" DEPLOY_SHA=
expect 1 "$bad" DEPLOY_SHA="${second:0:7}"
expect 1 "$bad" DEPLOY_SHA="${second}0"
expect 1 "$bad" DEPLOY_SHA="$(tr a-f A-F <<<"$second")"
expect 1 "$side is not on origin/main; refusing" DEPLOY_SHA="$side"
expect 1 "0000000000000000000000000000000000000000 is not on origin/main; refusing" DEPLOY_SHA=0000000000000000000000000000000000000000
chmod 600 "$tmp/live/tracked.txt" "$tmp/live/tracked-script.sh"
expect 0 "live is newer ($second), skipped $first" --contains \
  'tracked file modes: normalized=2 mismatches=0' DEPLOY_SHA="$first"
if [[ "$(stat -c '%a' "$tmp/live/tracked.txt")" != 644 || \
    "$(stat -c '%a' "$tmp/live/tracked-script.sh")" != 755 ]]; then
  echo 'FAIL tracked file modes were not restored from git' >&2
  failures=$((failures + 1))
fi
local_files_after=$(stat -c '%n:%u:%g:%a' \
  "$tmp/live/.env" "$tmp/live/pgdump-fixture.sql" "$tmp/live/rollout-evidence.txt")
if [[ "$local_files_after" != "$local_files_before" ]]; then
  echo 'FAIL untracked host-local file owners or modes changed' >&2
  failures=$((failures + 1))
fi

echo "source_sha=$second image_id=sha256:fixture built_at=fixture" > "$tmp/root/main1238-fixture.build/built-image.identity"
expect 0 "already at $second with runner provenance" DEPLOY_SHA="$second"

# Without a provenance record, an equal SHA must reach the deploy path
# (the fake docker refuses the pg_dump call).
echo "source_sha=$first image_id=sha256:other " > "$tmp/root/main1238-fixture.build/built-image.identity"
expect 99 "unexpected docker call: compose exec -T postgres pg_dump -U postgres telegram" DEPLOY_SHA="$second"
# A newer commit on main deploys.
expect 99 "unexpected docker call: compose exec -T postgres pg_dump -U postgres telegram" DEPLOY_SHA="$third"
expect 0 "deployed $third" --contains \
  'tracked file modes: normalized=1 mismatches=0' FIXTURE_DEPLOY_SUCCESS=1 DEPLOY_SHA="$third"
if [[ "$(stat -c '%a' "$tmp/live/tracked.txt")" != 644 || \
    "$(stat -c '%a' "$tmp/live/tracked-script.sh")" != 755 || \
    "$(stat -c '%a' "$tmp/live/new-tracked.txt")" != 644 ]]; then
  echo 'FAIL tracked modes were not restored after the rollout updated the checkout' >&2
  failures=$((failures + 1))
fi
local_files_after=$(stat -c '%n:%u:%g:%a' \
  "$tmp/live/.env" "$tmp/live/pgdump-fixture.sql" "$tmp/live/rollout-evidence.txt")
if [[ "$local_files_after" != "$local_files_before" ]]; then
  echo 'FAIL untracked host-local file owners or modes changed after rollout' >&2
  failures=$((failures + 1))
fi

if [[ "$failures" != 0 ]]; then
  echo "$failures deploy-main.sh fixture case(s) failed" >&2
  exit 1
fi
echo "deploy-main.sh fixture cases pass"
