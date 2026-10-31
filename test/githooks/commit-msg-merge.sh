#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root"
test_root="$(mktemp -d)"
trap 'rm -rf -- "$test_root"' EXIT
repo="$test_root/repo"
message_file="$test_root/commit-message"
error_file="$test_root/commit-msg-error"
commit_msg_hook="$repo_root/.githooks/commit-msg"

check_rejected_message() {
  local expected_rule="$1"
  shift
  printf '%s\n' "$@" > "$message_file"
  if "$commit_msg_hook" "$message_file" 2> "$error_file"; then
    printf 'expected commit-msg to reject: %s\n' "$1" >&2
    exit 1
  fi
  if [[ "$(wc -l < "$error_file")" -ne 1 ]] || ! grep -Fq "$expected_rule" "$error_file"; then
    printf 'commit-msg did not report one line for rule: %s\n' "$expected_rule" >&2
    cat "$error_file" >&2
    exit 1
  fi
}

for subject in \
  'Merge branch '\''main'\'' into feature' \
  'Revert "feat(api): add endpoint"' \
  'fixup! feat(api): add endpoint' \
  'squash! feat(api): add endpoint' \
  'amend! feat(api): add endpoint'; do
  printf '%s\n' "$subject" > "$message_file"
  "$commit_msg_hook" "$message_file"
done

check_rejected_message 'subject must match conventional format' 'not a conventional subject'
check_rejected_message 'subject must not contain an issue key' 'chore(hooks): close MAIN-123'
check_rejected_message 'message must not include a Co-Authored-By trailer' \
  'chore(hooks): update hooks' 'Co-Authored-By: Example <example@example.invalid>'
check_rejected_message 'message must not include a Generated with line' \
  'chore(hooks): update hooks' 'Generated with Example'

git -C "$test_root" init --quiet "$repo"
git -C "$repo" checkout --quiet -b main
git -C "$repo" config user.name 'Githooks Test'
git -C "$repo" config user.email 'githooks-test@example.invalid'
git -C "$repo" config commit.gpgsign false
git -C "$repo" config core.hooksPath "$repo_root/.githooks"

git -C "$repo" commit --quiet --allow-empty -m 'chore(test): create base'
base_commit="$(git -C "$repo" rev-parse HEAD)"

git -C "$repo" checkout --quiet -b feature
printf 'feature branch\n' > "$repo/feature.txt"
git -C "$repo" add feature.txt
git -C "$repo" commit --quiet -m 'chore(test): add feature change'

git -C "$repo" checkout --quiet main
printf 'main branch\n' > "$repo/main.txt"
git -C "$repo" add main.txt
git -C "$repo" commit --quiet -m 'chore(test): add main change'

git -C "$repo" checkout --quiet feature
git -C "$repo" merge --quiet --no-edit main
parent_count="$(git -C "$repo" rev-list --parents -n 1 HEAD | awk '{print NF}')"
if [[ "$parent_count" -ne 3 ]]; then
  printf 'expected git merge to create a merge commit with hooks enabled\n' >&2
  exit 1
fi

git -C "$repo" checkout --quiet -b merge-head-check "$base_commit"
printf 'feature value\n' > "$repo/conflict.txt"
git -C "$repo" add conflict.txt
git -C "$repo" commit --quiet -m 'chore(test): add feature value'

git -C "$repo" checkout --quiet main
printf 'main value\n' > "$repo/conflict.txt"
git -C "$repo" add conflict.txt
git -C "$repo" commit --quiet -m 'chore(test): add main value'

git -C "$repo" checkout --quiet merge-head-check
if git -C "$repo" merge --no-commit --no-edit main >/dev/null 2>&1; then
  printf 'expected the merge-head check setup to conflict\n' >&2
  exit 1
fi
printf 'resolved value\n' > "$repo/conflict.txt"
git -C "$repo" add conflict.txt
git -C "$repo" commit --quiet -m 'generated merge message'

printf 'commit-msg merge hook checks passed\n'
