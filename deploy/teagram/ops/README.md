# Teagram ops scripts

Ops scripts run as root in `/tmp` through the forced command:

```sh
ssh teagram run <40-hex-main-sha> <target-failure-report|rollout-evidence|invite-web-signup-url> [args]
```

The runner fetches `origin/main`, checks the commit ancestry, and accepts only
`target-failure-report`, `rollout-evidence`, and `invite-web-signup-url`. A
script's presence in an older main commit does not authorize a retired
operation. The allowlist lives in
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
therefore become host-root execution. The dispatcher limits `run` to the three
current operation names before fetching Git data, verifies the full SHA is on
`origin/main`, constructs the script path itself, and does not evaluate a
caller-provided shell command. The evidence operations validate their inputs
and emit bounded, sanitized output. `invite-web-signup-url` accepts only the
documented no-argument apply, `--check`, and `--rollback` modes; check is
read-only, and apply or rollback changes only its public URL assignment and the
matching rollout-environment digest record. It verifies the current record,
refuses unrelated or desktop URL changes, and stages the update under the
deployment lock. Dispatcher tests cover all three modes and attempts to select
deleted scripts from an older main commit.

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

`apply-user-invite-quota.sh` adds `TG_USER_INVITE_QUOTA=5` through the named
rollout environment path. It requires the invite-mode listener and unset
account-age override, saves the prior `.env` under `/opt/teagram/.state`, and
verifies `help.getAppConfig` from the deployed image after the runner applies
the change. Run it only after the invite RPC release is live:

```sh
ssh teagram run <reviewed-main-sha> apply-user-invite-quota
```

If the rollout succeeds but the final `help.getAppConfig` probe fails, retry
with the read-only verification path:

```sh
ssh teagram run <reviewed-main-sha> apply-user-invite-quota verify
```

It checks that the recorded environment is exactly the saved `.env` plus the
quota setting, then reruns the probe against the current live image without
applying another rollout.

`rollback-public-websocket.sh` is the pre-revert step for a deployed public
WebSocket route. Run it through `teagram-ops` with the current deployed SHA;
it takes `/tmp/teagram-deploy.lock`, verifies the live checkout and base image
match that SHA, removes only the `teagram` project's `public-haproxy`,
`teagramd-public-1`, and `teagramd-public-2` containers, then confirms those
containers are gone and no container publishes host port 2446. After it
succeeds, revert the route commit and let `Deploy.` recreate base `teagramd`
with its original `127.0.0.1:2446` publish. The operation is repeatable and
does not touch volumes or the database.

## User-invite quota threat model

The protected asset is the account-admission boundary. A user-issued invite is
a bearer link that lets its holder create an account; it does not grant access
to the inviter's account. The relevant actors are an abusive or compromised
authenticated account, someone who obtains a live invite link, and an operator
misconfiguration.

- An authenticated inviter can issue links only on an invite-mode listener
  after the inviter account is seven days old. The configured limit is five
  issued invites per inviter in a trailing seven-day window. Links expire after
  seven days, can be revoked by their owner, and are redeemed once. Secrets are
  generated from `crypto/rand`; only their SHA-256 digests are stored.
- A leaked link can be used by whoever holds it before it expires or is
  revoked. Expiry, revocation, single-use consumption, and the issuance limit
  reduce exposure but do not verify that invitees are trustworthy.
- Applying the quota could expose the feature on the wrong listener or change
  the account-age policy. The operation verifies the invite-mode listener,
  rejects additional WebSocket listeners and an account-age override, backs up
  `.env`, and checks `help.getAppConfig` after the rollout. Inspection failures
  reject the operation rather than being treated as absent settings.

An eligible account can still invite up to five new accounts each week. The
quota limits the rate of expansion; it does not replace signup abuse
monitoring, account enforcement, or revocation of exposed links.

`invite-web-signup-url.sh` adds only the public Web signup target to `.env` and
updates `.state/rollout-env` to keep the next verified rollout's environment
baseline consistent. Run `ssh teagram run <main-sha> invite-web-signup-url
--check` before the no-argument apply. It refuses a mismatched rollout record,
an existing different or duplicate target, or any `TG_INVITE_DESKTOP_URL`
assignment. The target is the live Web app root at
`https://web.teagram.io/`; the `/signup` path is not the app route. To undo the
addition, run `ssh teagram run <main-sha> invite-web-signup-url --rollback`.
