# Synthetic restore readiness drill

Run the gate only on a disposable Linux CI runner with Docker, Go modules already downloaded, Atlas v1.2.0, and passwordless `sudo`. From the repository root, run:

```sh
timeout --signal=TERM --kill-after=15s 35m bash deploy/teagramd/rollout-runner/test-restore-readiness.sh
```

The command accepts no dump, DSN, account, or service configuration. It creates a fresh fixture from the repository migrations, restores it into the pinned PostgreSQL 18 image, applies the pending migration, and checks configuration plus sign-in for a newly generated account. After that pass it runs two disposable failure checks: generated malformed SQL must produce a sanitized restore failure with verified cleanup, and SIGKILL must let the supervisor remove its container, image, build inputs and run files. The gate prints its success object only after both checks leave no owned containers, images, volumes, networks or run directories. A passing result is one JSON object with `result=pass`, `migration_head=20261021000004`, both application assertions set to `pass`, `cleanup=verified`, an integer `elapsed_ms`, and `exit=0`. Any failed assertion or unverified cleanup returns nonzero and a fixed phase/category report.

The restore tools, daemon, and application checker run as non-root with dropped capabilities, no-new-privileges, read-only root filesystems, bounded scratch storage, and no externally attached network. The host Docker CLI is pinned to `/usr/bin/docker` and checked for root ownership and non-writable path components before use. Atlas is copied as input and its version is checked only inside the restricted tools container. There is one scoped root exception for the two one-shot account provisioning steps: the fixture seeder and the checker's `admin create-user` provisioner. Each runs in its own owned, disposable container with capabilities dropped, no-new-privileges, a read-only root filesystem, no container logging, and the private scratch database network namespace. Their only bind mounts are the protected secret files needed by that operation. The fixture seeder's unchanged Unix-socket contract is supplied by a private in-container socket shim over the source database's loopback-only TCP listener; it receives no host socket or database-socket bind mount. The checker provisioner receives only its four protected secret files and the target database's private loopback namespace. Both containers are attested and removed before the non-root checker proceeds.

The checker consumes a protected identity artifact from the provisioner, has no database DSN or auth-encryption key, and performs `help.getConfig` plus sign-in as UID 65532. The `admin create-user` root gate in `cmd/teagramd/main.go` and the seeder binary's input contract are unchanged. Security must verify this scoped exception at the PR head.

If the run reports unverified cleanup and the runner is still available, use the protected ownership manifest path created under `/run/restore-readiness-*/ownership.json` with:

```sh
bash deploy/teagramd/rollout-runner/test-restore-readiness.sh --recover /run/restore-readiness-<run>/ownership.json
```

Recovery rechecks the manifest and global lock, then removes only objects whose immutable IDs and configuration still match. A recovery command always returns nonzero because it repairs cleanup state after a failed drill; rerun the synthetic gate to obtain fresh passing evidence. Never copy a manifest, dump, database files, or container diagnostics into artifacts or logs.

The claim is limited to restored synthetic schema compatibility, `help.getConfig`, sign-in by the newly created synthetic account, and verified teardown. It says nothing about restoring existing sessions, auth keys, or media.
