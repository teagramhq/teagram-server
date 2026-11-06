# Teagram ops scripts

Ops scripts run as root in `/tmp` through the forced command:

```sh
ssh teagram run <40-hex-main-sha> <target-failure-report|rollout-evidence|invite-web-signup-url|apply-user-invite-quota|start-server|encrypt-media|cleanup-media|backup-media-key|bot-api-qa> [args]
```

The runner fetches `origin/main`, checks the commit ancestry, and accepts only
the nine current operations: `target-failure-report`, `rollout-evidence`,
`invite-web-signup-url`, `apply-user-invite-quota`, `start-server`,
`encrypt-media`, `cleanup-media`, `backup-media-key`, and `bot-api-qa`. A
script's presence in an older main commit does not authorize a retired operation. The allowlist lives in
`teagram-ops.sh`; tests cover retired names
at an older main SHA. Keep every operation narrowly scoped to its named
purpose. Validate arguments before using them, fail closed, and never put
secrets in arguments, logs, or output.

## Bot API QA

The optional `bot-api-qa` Compose profile uses two pinned TCP relays. The
host publishes the ingress relay on `127.0.0.1:2447` on a dedicated bridge
network. Only the two relays mount the private socket volume; the inner relay
shares teagramd's network namespace and forwards that socket to the Bot API
listener on `127.0.0.1:2447`. Tailscale Serve is the only tailnet entry point
and supplies a PROXY v1 header. The relays pass the bytes unchanged.

After deploying the reviewed SHA, run `ssh teagram run <sha> bot-api-qa check`
to check the live environment and Serve port, followed by `ssh teagram run
<sha> bot-api-qa apply`. Apply enables the listener through the named rollout
path, starts the relays and creates the Serve mapping without overwriting a
foreign mapping. `ssh teagram run <sha> bot-api-qa verify` checks the mapping,
container topology, health and volume isolation. Repeating apply preserves the
original mapping record. `ssh teagram run <sha> bot-api-qa rollback` restores
the previous Serve mapping without touching other ports. When the previous port
was absent, the rollback takes effect as soon as Tailscale applies the command,
leaving the listener and relays reachable only on host loopback. The operation
does not remove the listener environment setting or the relay containers.

Enabling the listener runs the server rollout and can interrupt all connected
clients during replacement. A wrong relay or Serve mapping can also make the QA
endpoint unreachable or permit peer-address forgery. There is no staging LXC.
The CI fixture previews the resolved Compose isolation and exercises Serve
apply, repeat, refusal and rollback against a mock. The relays expose Docker
health status and bounded connection logs; inspect them and run `verify` after
deployment. The rollout runner recreates them after a server restart so the
inner relay joins the current network namespace. Production acceptance
additionally needs `getMe` and the cross-peer revoked-token isolation test.

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
therefore become host-root execution. The dispatcher limits `run` to the current operation names before fetching Git data, verifies the full SHA is on
`origin/main`, constructs the script path itself, and does not evaluate a
caller-provided shell command. The evidence operations validate their inputs
and emit bounded, sanitized output. `invite-web-signup-url` accepts only the
documented no-argument apply, `--check`, and `--rollback` modes; check is
read-only, and apply or rollback changes only its public URL assignment and the
matching rollout-environment digest record. It verifies the current record,
refuses unrelated or desktop URL changes, and stages the update under the
deployment lock. Dispatcher tests check every shipped operation at the `git
show` boundary, cover all three invite URL modes, and refuse attempts to select
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

## Restarting the existing server containers

`start-server.sh` recovers the existing S3-backed server stack after its
containers stop. It takes the deployment lock, validates both containers'
Compose project, service names and recorded file selection, then checks the
S3 authority and environment-file paths with the reviewed blob-transition
helper. Only the live `docker-compose.yml` plus `docker-compose.override.yml`
selection is accepted. It starts the existing `postgres` container, waits five
seconds and checks `pg_isready` for up to 60 seconds before starting only the
inspected `teagramd` container. It uses `docker start` on each existing
container, not Compose dependency startup, so this recovery cannot restart the
`migrate` container. It does not create or replace containers, run migrations,
or issue volume operations; the resumed server can make its normal database
writes after it is running.

Run the operation from a merged `main` commit:

```sh
ssh teagram run <reviewed-main-sha> start-server
```

There is no staging LXC. The preflight and fixture suite are the preview gate;
the operation reports success only after Postgres is ready and `teagramd` is
running. If both containers were stopped before recovery and it must be rolled
back, the Steward can stop them from alpha with this command. It preserves the
named volumes; `teagramd` can take up to 120 seconds to finish its configured
graceful stop.

```sh
incus exec teagram -- docker stop teagram-teagramd-1 teagram-postgres-1
```

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

## Backing up the media key

After deploying the reviewed SHA, run `ssh teagram run <deployed-main-sha>
backup-media-key`. The operation copies the mounted version 1 key to
`/etc/teagramd/media-key-backups/1.key`, verifies it without displaying key
material or digests, and refuses to overwrite an existing backup. Retain the
backup for recovery; investigate any mismatch before proceeding. The operation
does not restart services.

## Encrypting and cleaning up media

`encrypt-media` converts the accepted S3 media authority to encrypted media.
It requires the deployed image and checkout to match, the live stack to use
the verified S3 Compose selection, the current media key to match the recorded
root-only backup, and healthy Postgres and RustFS containers. It stops the
existing `teagramd` writer, creates a custom-format `pg_dump`, and restores
that archive into an isolated PostgreSQL container with no network before
accepting the backup. It then migrates and reads back every referenced object,
publishes encrypted authority, restarts RustFS, repeats encrypted readback,
starts `teagramd`, and runs the deployed image's MTProto smoke probe.
The operation fails closed if the full-tree inventory reports an object with no
database reference; its key is retained in the private inventory report for
review, and the script does not claim migration completion.

Run it only from the exact deployed `main` SHA after the reviewed migration
code is live:

```sh
ssh teagram run <deployed-main-sha> encrypt-media
```

The private evidence directory `/root/teagram-media-encryption` stores the
verified database dump and manifest, migration JSON results, and smoke output.
The command reports reference counts, the backup path, and the recorded media
key backup path without printing key material. If interrupted before encrypted
authority is published, the operation restarts the existing server under the
still-authoritative S3 mode. After publication, the plaintext source objects
remain intact and a repeated invocation verifies/resumes encrypted copies;
keep encrypted authority and use that forward-recovery path rather than
restoring an older application image.

Plaintext deletion is a separate operation and requires an explicit
`--acceptance-passed` argument. Before invoking it, verify old and new media
downloads, including a range read, after restarting `teagramd` and RustFS, and
complete the requested Linux QA media probe. The cleanup operation independently
checks encrypted authority, the verified database dump and key backup, the
stopped-writer progress journal, and every encrypted reference. It removes only
verified plaintext copies, runs a second cleanup pass that must remove zero
objects, repeats encrypted readback across a RustFS restart, restarts
`teagramd`, and runs the MTProto smoke probe. Before deleting referenced
plaintext, it also rejects any unaccounted objects reported by the current
full-tree inventory. These objects have no database reference and remain
untouched for explicit operator review; cleanup cannot report success while
they remain.

```sh
ssh teagram run <deployed-main-sha> cleanup-media --acceptance-passed
```

The encrypted copies and encrypted authority remain in place after cleanup.
If cleanup stops partway through, retain encrypted authority and rerun it only
after the acceptance checks still pass; cleanup revalidates the encrypted
copies before removing any remaining source. The verified pre-migration dump
remains in `/root` as recovery evidence; the media key backup is at
`/etc/teagramd/media-key-backups/1.key`.

The transition's threat boundary is the reviewed `main` SHA, the single
verified Compose stack, the root-owned media key and database backup, the
Postgres reference inventory, and the private RustFS network. Migration stops
the server writer and preserves plaintext until encrypted references pass
readback. Cleanup is destructive: it requires the explicit acceptance flag,
revalidates encrypted copies, and removes only database-referenced or
journaled plaintext objects; an unaccounted object stops the operation for
review. If interrupted, authority remains encrypted after publication and the
script restores service under the recorded authority; rerun the operation to
resume from its journal. The SSH forced command executes only an allowlisted
script from a commit on `origin/main`; privileged work runs as root under the
shared deployment lock, with secrets read from protected files and excluded
from operation output. The private dump, key backup, journal, and inventory
reports are the recovery evidence; do not roll back encrypted authority or
restore an older server image.
