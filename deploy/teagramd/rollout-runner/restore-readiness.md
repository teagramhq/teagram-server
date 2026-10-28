# Restore readiness drill

## Synthetic mode

Run the gate only on a disposable Linux CI runner with Docker, Go modules already downloaded, Atlas v1.2.0, and passwordless `sudo`. From the repository root, run:

```sh
timeout --signal=TERM --kill-after=15s 35m bash deploy/teagramd/rollout-runner/test-restore-readiness.sh
```

The shell gate prepares its pinned Go binaries and Atlas input before it starts the 30-minute drill deadline. Preparation has a separate 45-minute bound, compiles with one Go build worker, and writes no database, Docker network, or volume. A root-only attestation binds the read-only artifacts to the checked-out commit, exact Go toolchain digest, and the two PostgreSQL image digests. The run verifies that attestation before taking the restore lock. On completion the gate explicitly tears down the prepared directory and verifies it is absent.

With no arguments the command accepts no dump, DSN, account, or service configuration. It creates a fresh fixture from the repository migrations with three synthetic auth keys, two dependent SRP challenges, three dependent notify cursors, and a binding-evidence row enrolled for the synthetic account and updated by the reviewed auth-key trigger. It restores that fixture into the pinned PostgreSQL 18 image and applies the pending migration.

Before starting the daemon, the gate re-attests the exact scratch database, verifies its post-restore marker and migration head, and compares its auth-key foreign-key inventory and triggers on `auth_keys`, `srp_challenges`, `notify_settings_difference_cursors`, and `browser_acceptance_binding_evidence` with `auth-key-clearing-inventory.json`. It confirms the daemon is off and no other database client is connected, then runs a schema-qualified, bounded `DELETE` for the generated key IDs in one transaction. The transaction checks the auth-key count before commit; aggregate counts verify the declared cascades and binding-evidence trigger effect. A second zero-key count runs immediately before daemon startup. Key values, rows, and identifiers are not read back or reported.

After clearing, the gate checks `help.getConfig` and sign-in for a newly generated account. It also runs disposable fault checks: malformed restore SQL, an injected error inside the clearing transaction, INT and TERM during clearing, and SIGKILL for supervisor recovery. The injected SQL failure is accepted only when psql reports the expected division-by-zero error after the deletion acknowledgment and the aggregate counts match their pre-clear values. INT and TERM are sent only after that same acknowledgment, while the transaction is still open. Every failure case must return its fixed phase/category and verified cleanup. The gate prints its success object only after all checks leave no owned containers, images, volumes, networks or run directories. A passing result includes `result=pass`, `migration_head=20261028000001`, both application assertions set to `pass`, `auth_keys_cleared=pass`, `cleanup=verified`, an integer `elapsed_ms`, and `exit=0`.

The restore tools, daemon, and application checker run as non-root with dropped capabilities, no-new-privileges, read-only root filesystems, bounded scratch storage, and no externally attached network. The host Docker CLI is pinned to `/usr/bin/docker` and checked for root ownership and non-writable path components before use. Atlas is copied as input and its version is checked only inside the restricted tools container. There is one scoped root exception for the two one-shot account provisioning steps: the fixture seeder and the checker's `admin create-user` provisioner. Each runs in its own owned, disposable container with capabilities dropped, no-new-privileges, a read-only root filesystem, no container logging, and the private scratch database network namespace. Their only bind mounts are the protected secret files needed by that operation. The fixture seeder's unchanged Unix-socket contract is supplied by a private in-container socket shim over the source database's loopback-only TCP listener; it receives no host socket or database-socket bind mount. The checker provisioner receives only its four protected secret files and the target database's private loopback namespace. Both containers are attested and removed before the non-root checker proceeds.

The checker consumes a protected identity artifact from the provisioner, has no database DSN or auth-encryption key, and performs `help.getConfig` plus sign-in as UID 65532. The `admin create-user` root gate in `cmd/teagramd/main.go` and the seeder binary's input contract are unchanged. Security must verify this scoped exception at the PR head.

If the run reports unverified cleanup and the runner is still available, use the protected ownership manifest path created under `/run/restore-readiness-*/ownership.json` with:

```sh
bash deploy/teagramd/rollout-runner/test-restore-readiness.sh --recover /run/restore-readiness-<run>/ownership.json
```

Recovery rechecks the manifest and global lock, then removes only objects whose immutable IDs and configuration still match. A recovery command always returns nonzero because it repairs cleanup state after a failed drill; rerun the synthetic gate to obtain fresh passing evidence. Never copy a manifest, dump, database files, or container diagnostics into artifacts or logs.

The synthetic-mode claim is limited to restored synthetic schema compatibility after scratch auth-key clearing, `help.getConfig`, sign-in by the newly created synthetic account, and verified teardown. Restored sessions, SRP challenges, and notify cursors are discarded. The gate covers configuration and new-account sign-in only; it makes no claim about restoring existing sessions, keys, challenges, cursors, or media.

## Local dump mode

To run the drill against one local PostgreSQL plain-SQL dump, set the target commit SHA and use the protected invocation from the repository root:

```sh
target_sha=0123456789abcdef0123456789abcdef01234567
dump_timestamp=20261022T211333Z
timeout --signal=TERM --kill-after=15s 35m bash deploy/teagramd/rollout-runner/test-restore-readiness.sh --local-dump "/root/pgdump-pre-${target_sha:0:7}-${dump_timestamp}.sql"
```

On the `teagram` LXC, prepare and run from the separate checkout as the `restore-drill` account, never from `/opt/teagram`. Preparation can run on its own and returns a private prepared-directory path with aggregate elapsed time:

```sh
bash deploy/teagramd/rollout-runner/test-restore-readiness.sh --prepare-only
```

Pass that directory to the local-dump invocation. The drill re-attests the source commit, toolchain, image pins, and artifact hashes before acquiring `/run/restore-readiness.lock`. It also takes a nonblocking shared lock on `/tmp/teagram-deploy.lock`; an active `Deploy.` rollout returns `phase=preflight`, `category=rollout_in_progress`, and creates no scratch resource. The shared lock remains held through cleanup so a rollout cannot overlap the drill. A normal run removes the prepared directory and verifies its absence. For an interrupted or standalone preparation, tear it down explicitly and verify absence:

```sh
bash deploy/teagramd/rollout-runner/test-restore-readiness.sh --teardown-prepared /tmp/teagram-restore-readiness.XXXXXXXX
test ! -e /tmp/teagram-restore-readiness.XXXXXXXX
```

Use the `/root/pgdump-pre-<7-hex>-<UTC timestamp>.sql` path printed by the LXC `Deploy.` run; that deploy producer is authoritative for this invocation. The accepted filename also allows `pgdump-pre-<7-to-40-lowercase-hex>.sql` for the rollout runner's backup producer. The file must be regular and non-symlink. Preflight checks metadata only: root ownership, mode `0600`, one hard link, a nonzero size no larger than 512 MiB, and root-owned parent directories that are not group- or world-writable. A metadata mismatch returns the fixed `isolation_refusal` category before creating the run lock or scratch resources. The invocation accepts no DSN, URL, standard input, globals dump, or second input. The host does not read, hash, print, copy, or upload the dump bytes; only the confined restore client reads the read-only bind mount.

The local dump is restored transactionally by a confined PostgreSQL client inside the scratch database's no-egress namespace. The client runs as container UID 0 so it can read the root-owned `0600` bind mount, with GID 70 so it can reach the scratch socket directory; it has no Linux capabilities, a read-only root filesystem, and only the dump and scratch socket mounts. A random psql restricted-mode key blocks dump-supplied psql commands; the gate strips the expected `pg_dump` restriction framing and refuses unexpected `\restrict` or `\unrestrict` commands. Scratch clusters use a separate bootstrap superuser, so the dump's non-bootstrap `postgres` owner role stays demoted while the gate checks catalog inventories, roles, database ACLs and settings with an unprivileged session whose search path is pinned. The migration-history relation's shape is checked before reading its head; the head must be a repository migration at or after the supported baseline. The source baseline is migrated to that exact head before the dump is compared. The target role is promoted only after schema comparison and any pending migrations pass. Relation ACL checks ignore only the owner's own privileges, which differ between PostgreSQL 16 and 18; every grant to another role or PUBLIC is compared. The auth-key clearing transaction is mandatory and refuses more than 100,000 rows. Application checks run only after the zero-key check. A pass adds `input_mode=local` to the enumerated success object; failure output contains only fixed categories.

CI qualifies this mode with a harness-generated, synthetic SQL dump written with the accepted production filename and metadata. The source fixture is migrated to the newest repository migration, which also exercises a dump head newer than the previous fixed head. Its gate follows the same prepare-then-run path, then runs local auth-key INT/TERM cancellation and SIGKILL supervisor recovery while the local dump descriptor remains open:

```sh
timeout --signal=TERM --kill-after=15s 35m bash deploy/teagramd/rollout-runner/test-restore-readiness.sh --local-fixture-test
```

Local mode uses the same protected recovery command documented above. The claim is limited to schema compatibility after scratch auth-key clearing, `help.getConfig`, sign-in by a newly created synthetic account, and verified teardown. Existing sessions, auth keys, challenges, cursors, and media are not recovered. Preparing the runtime does not read a real dump; MAIN-2397 owns the separate LXC real-dump drill.
