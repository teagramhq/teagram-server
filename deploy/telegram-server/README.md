# telegram-server LXC media storage

The normal `docker-compose.yml` stack runs RustFS on the LXC's private Compose
network. The service publishes no ports, uses the pinned multi-platform RustFS
image, and stores objects in the named `rustfsdata` volume. The private
`telegram` bucket is initialized idempotently; `telegramd` receives a separate
key scoped to its `telegramd/` prefix. Credential values live in
`/opt/telegram-server/.env` (mode 0600). The bootstrap also writes the app
secret to `.secrets/telegramd-blob-secret-key`; its parent is mode 0700 and the
file is mode 0444 so the non-root server can read its read-only secret mount.

## First provisioning

Run this once on the box after checking out the merged revision:

```sh
cd /opt/telegram-server
./deploy/bootstrap-rustfs-secrets.sh
```

The script generates credentials only when all four values are empty. It
refuses a partially configured set, so rerunning it cannot rotate a key for an
initialized RustFS volume. It never prints credential values. Do not copy the
generated `.env` or `.secrets` off the box or commit them.

## Local blob migration and cutover

Before the first cutover, take and verify a backup or snapshot of the entire
telegram-server LXC. The restore point must include Docker's named volumes,
especially `rustfsdata` and `tgblobs`. Record the restore identifier and path
on the deployment issue. The project repository does not define the alpha host's
backup schedule; verify the live host procedure before proceeding.

The deploy ticket still requires the normal Postgres dump before updating the
checkout. After the code is updated, keep Postgres running and perform the
media cutover in this order:

```sh
set -eu
cd /opt/telegram-server
./deploy/bootstrap-rustfs-secrets.sh
docker compose up -d rustfs
docker compose run --rm --no-deps rustfs-init
./deploy/telegram-server/migrate-local-blobs.sh
docker compose ps -a
docker compose logs --since 5m telegramd rustfs
```

Do not start `telegramd` with S3 until `blob-migrate` exits successfully and
the final JSONL line reports equal source and destination manifest checksums.
Every preceding `object` line records the same key, byte count, and source and
destination SHA-256. The command checks the full destination key set and count;
it exits non-zero on any unreadable source path, extra destination key, missing
object, short write, or checksum mismatch. It does not delete or modify the
source volume, so a failed or interrupted copy can be retried while the server
remains stopped. Keep the report with the deployment record.

The successful command's final JSON line is the local completion marker, so
the helper stops `telegramd`, validates and writes it, then starts the service.
It checks for `jq` before stopping the service. The marker is local to
`/opt/telegram-server` and is ignored by Git. Standard deploys skip the
migration while it exists. If you roll back to filesystem storage, remove the
marker after the rollback succeeds and record that decision; the next cutover
must reconcile local writes with the existing S3 namespace.

Verify RustFS is healthy, `telegramd` is running without S3 errors, and no RustFS
port is published (`docker compose port rustfs 9000` returns no mapping). Then
verify a fresh document upload and download through the client acceptance path.
The `tgblobs` volume remains mounted read-only in the normal service until a
separate cleanup removes it.

## Rollback

If the S3 startup or media check fails, stop `telegramd` to freeze media writes,
copy the current RustFS namespace back into `tgblobs`, and only then start the
filesystem rollback override. The restore overwrites matching keys with
checksum-verified S3 bytes and preserves local-only legacy objects; it does not
delete or modify RustFS objects. The Postgres metadata therefore continues to
resolve files uploaded after cutover:

```sh
set -eu
umask 077
cd /opt/telegram-server
if [ "$#" -gt 1 ]; then
	printf '%s\n' 'usage: rollback [restore-report-file]' >&2
	exit 2
fi
restore_report=${1:-".state/blob-restore-$(date -u +%Y%m%dT%H%M%SZ).jsonl"}
command -v jq >/dev/null 2>&1 || {
	printf '%s\n' 'jq is required to validate the blob restore report' >&2
	exit 1
}
docker compose stop telegramd
docker compose run --rm --no-deps blob-restore >"$restore_report"
summary=$(tail -n 1 "$restore_report")
if ! printf '%s\n' "$summary" | jq -e '
	.type == "summary" and
	(.source_manifest_sha256 | type == "string" and test("^[0-9a-f]{64}$")) and
	.destination_manifest_sha256 == .source_manifest_sha256
' >/dev/null; then
	printf '%s\n' 'blob restore did not report matching verified manifests' >&2
	exit 1
fi
docker compose -f docker-compose.yml -f docker-compose.local-blobs.yml up -d --no-deps telegramd
test -n "$(docker compose -f docker-compose.yml -f docker-compose.local-blobs.yml ps --status running --quiet telegramd)"
docker compose ps -a
docker compose logs --since 5m telegramd
rm -f .state/blob-migration-complete
```

The restore report records a source and destination SHA-256 for every object;
the command fails before the backend switch if any copy or checksum fails. Keep
the report with the deployment record. Marker removal happens only after the
local-backend container is running, so a failed restore or startup leaves the
S3 mode marker in place. Never remove `tgblobs` or run `docker compose down -v`
as part of rollback. A later cutover must reconcile local writes with the
existing RustFS namespace before retrying; the migration tool fails closed if
it finds destination keys absent from the source. Restore the pre-cutover LXC
snapshot only if the volume or container state itself needs recovery, and record
the restore path on the issue.
