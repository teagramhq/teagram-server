# Teagram ops scripts

Ops scripts run as root in `/tmp` through the forced command:

```sh
ssh teagram run <40-hex-main-sha> <script-name> [args]
```

The runner fetches `origin/main`, checks the commit ancestry and script name,
and executes the script recorded in that commit. Keep every operation narrowly
scoped to its named purpose. Validate arguments before using them, fail closed,
and never put secrets in arguments, logs, or output.

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

`replay-check.sh` is read-only. It requires a full SHA on `origin/main` that
matches the live image revision, runs that commit's rollout verifier as a child
process, and stores its bounded readiness evidence under `/tmp`. The verifier's
required private directory under `/root` is created and cleaned by this CLI
operation; only the two validated readiness output files are copied to the
private `/tmp` evidence directory. For the S3 Compose selection, the script
accepts exactly one parked environment file at the approved path and passes
that path to Compose without printing its values.

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

`main2612-live-compose-replay.sh` requires bubblewrap with working user, network,
and process namespaces; it fails before reading live inputs if those namespaces
are unavailable. Candidate transition, verifier, and runner programs execute as
UID 0 inside that namespace, with `/root`, `/home`, `/opt`, `/var`, `/run`, and
`/tmp` isolated from the host, no network, and no Docker socket. They receive
read-only staged Compose inputs, synthetic environment and secret files, and a
redacted container inspection. The staged override and pinned Compose files
may contain operator settings, so candidate output is accepted only when it
matches the trusted twelve-field result set byte for byte; the host prints its
own canonical rows after guarded cleanup. The entire writable replay tree,
including evidence, fixtures, and captured logs, is an invocation-owned tmpfs
limited to 128 MiB and 8,192 inodes. Candidate processes run in a systemd scope
limited to 1 GiB of memory, one CPU, and 64 tasks, with additional per-process
time, address-space, file-size, and descriptor limits. The host process uses the
deployed live transition code to validate the real authority state and chooses
the environment-file paths, but never copies credential values into the
replay. Candidate-produced pass rows are diagnostic output only and do not
authorize a deployment.
