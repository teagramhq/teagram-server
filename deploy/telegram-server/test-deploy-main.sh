#!/usr/bin/env bash
# Fixture test for deploy-main.sh's target checks: a fake origin and live
# checkout, and a fake docker that reports a pinned compose and image id.
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/deploy-main.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

export GIT_AUTHOR_NAME=fixture GIT_AUTHOR_EMAIL=fixture@example.invalid
export GIT_COMMITTER_NAME=fixture GIT_COMMITTER_EMAIL=fixture@example.invalid
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

git init -q -b main "$tmp/work"
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
third=$(commit third)
git -C "$tmp/work" push -q "$tmp/origin.git" main
git -C "$tmp/live" reset -q --hard "$second"

mkdir -p "$tmp/bin" "$tmp/root/main1238-fixture.build"
cat > "$tmp/bin/docker" <<'DOCKER'
#!/usr/bin/env bash
case "$*" in
  *config_files*) echo "/opt/telegram-server/.rollout-compose.local-abc.yml,/opt/telegram-server/docker-compose.override.yml" ;;
  *'{{.Image}}'*) echo "sha256:fixture" ;;
  *) echo "unexpected docker call: $*" >&2; exit 99 ;;
esac
DOCKER
chmod 755 "$tmp/bin/docker"

failures=0
# expect <exit code> <last line> [VAR=value...]
expect() {
  local want_rc="$1" want_last="$2" out rc=0
  shift 2
  out=$(env "$@" PATH="$tmp/bin:$PATH" DEPLOY_DIR="$tmp/live" DEPLOY_ROOT="$tmp/root" bash "$script" 2>&1) || rc=$?
  if [[ "$rc" != "$want_rc" || "${out##*$'\n'}" != "$want_last" ]]; then
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
expect 0 "live is newer ($second), skipped $first" DEPLOY_SHA="$first"

echo "source_sha=$second image_id=sha256:fixture built_at=fixture" > "$tmp/root/main1238-fixture.build/built-image.identity"
expect 0 "already at $second with runner provenance" DEPLOY_SHA="$second"

# Without a provenance record, an equal SHA must reach the deploy path
# (the fake docker refuses the pg_dump call).
echo "source_sha=$first image_id=sha256:other " > "$tmp/root/main1238-fixture.build/built-image.identity"
expect 99 "unexpected docker call: compose exec -T postgres pg_dump -U postgres telegram" DEPLOY_SHA="$second"
# A newer commit on main deploys.
expect 99 "unexpected docker call: compose exec -T postgres pg_dump -U postgres telegram" DEPLOY_SHA="$third"

if [[ "$failures" != 0 ]]; then
  echo "$failures deploy-main.sh fixture case(s) failed" >&2
  exit 1
fi
echo "deploy-main.sh fixture cases pass"
