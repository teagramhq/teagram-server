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

`restore-drill.sh` accepts no arguments. It requires the deployed main revision
to descend from the operation commit, `aa40758600452b422cc10f99a662df8e39ac3ef3`,
and `8cc64750f467b782b81efaddd0077cc619c52113`; the deployed revision is the
green-CI gate because `Deploy.` only publishes a successful main run. It updates
the clean `/home/restore-drill/teagram-server` checkout to that revision, then
runs preparation and the local-dump drill as `restore-drill`. It selects the
newest timestamped Deploy dump by filename timestamp and mtime, inspects only
filesystem metadata, and emits a metadata-only dump inventory with the newest
ten marked to keep. The shared deploy lock covers the drill. Before and after,
it checks the live container identity, `teagram_*` volume identities, Serve
configuration, and the verified admin readiness response. It refuses to run
with less than 4 GiB free for preparation, less than the larger of 4 GiB or
twice the dump size free for the drill, less than 2816 MiB available memory, or
under 10% aggregate CPU idle. It never reads or prints dump contents and never
changes `/opt/teagram`.
