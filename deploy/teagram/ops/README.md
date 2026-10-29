# Teagram ops scripts

Ops scripts run as root in `/tmp` through the forced command:

```sh
ssh teagram run <40-hex-main-sha> <target-failure-report|rollout-evidence> [args]
```

The runner fetches `origin/main`, checks the commit ancestry, and accepts only
`target-failure-report` and `rollout-evidence`. A script's presence in an older
main commit does not authorize a retired operation. The allowlist lives in
`teagram-ops.sh`; tests cover both retired names at an older main SHA. Keep
every operation narrowly scoped to its named purpose. Validate arguments
before using them, fail closed, and never put secrets in arguments, logs, or
output.

## Threat model

Assets at risk include the host filesystem and root-owned evidence,
`/opt/teagram` configuration and secrets, the Docker daemon, and persistent
service data. The caller supplies `SSH_ORIGINAL_COMMAND`; `run` executes a
selected Git blob as root. An ancestor SHA from `origin/main` proves reviewed
lineage, but does not authorize every path present in that historical tree.
The host, Git origin,
protected `main`, review process, and custody of the forced-command key are
trusted. This command menu is not a sandbox.

The main threat is a caller using the privileged key to select an unintended
operation or arguments, or a bug in a root operation exposing secrets or
changing the host. A compromised key or maliciously merged operation can
therefore become host-root execution. The dispatcher limits `run` to the two
current operation names before fetching Git data, verifies the full SHA is on
`origin/main`, constructs the script path itself, and does not evaluate a
caller-provided shell command. The operations validate their inputs and emit
only bounded, sanitized evidence; their tests exercise the command boundary,
including attempts to select deleted scripts from an older main commit.

The allowlist blocks retired operation names at every SHA, while older
versions of an allowlisted operation remain callable if present on `main`.
Keep the list narrow and review each operation as root code. Adding an
operation requires updating the allowlist and its boundary tests.

Do not `source` the deploy runner or rollout verifier. Invoke reviewed tools as
child processes with explicit arguments and check their exit status. Access
files under `/root` only inside a narrowly scoped, reviewed operation exposed
by this forced-command CLI. Never accept a caller-supplied root path or expose
generic read, write, copy, or delete access there.

Prefer read-only and repeatable operations. Avoid recursive deletion. If an
operation needs `rm -rf`, first verify the canonical target is a real directory
inside the exact temporary directory owned by that invocation; pass that exact
path to `rm -rf`, never a parent or a glob. Keep temporary evidence in private,
invocation-owned directories and print a concise result, including the target
and evidence path where relevant. A repeated invocation must not change the
live service state. The forced-command runner removes only the `/tmp/teagram-ops.*`
directory it created with `mktemp`.

`rollout-evidence.sh` is read-only and takes no arguments. Run it through the
same CLI as `ssh teagram run <main-sha> rollout-evidence`. It reports the
latest failed target gate, the bounded readiness sample tail when present, and
allowlisted summary lines from the matching runner log's final 80 lines. The
runner log captures child-process stdout and stderr and may contain secrets or
configuration values, so this operation emits only the validated failure-gate
and rollback summary shapes; every other line is omitted. If the target failure
marker could not be persisted, it uses the matching rollback warning and
identifies the target failure marker and capture time as unavailable. It reads
only validated root-owned mode-0600 files from mode-0700 evidence directories.

`invite-web-signup-url.sh` adds only the public Web signup target to `.env` and
updates `.state/rollout-env` to keep the next verified rollout's environment
baseline consistent. Run `ssh teagram run <main-sha> invite-web-signup-url
--check` before the no-argument apply. It refuses a mismatched rollout record,
an existing different or duplicate target, or any `TG_INVITE_DESKTOP_URL`
assignment. The target is the live Web app root at
`https://web.teagram.io/`; the `/signup` path is not the app route. To undo the
addition, run `ssh teagram run <main-sha> invite-web-signup-url --rollback`.
