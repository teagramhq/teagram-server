#!/bin/sh
set -eu
umask 077

if [ "$#" -gt 1 ]; then
	printf '%s\n' 'usage: migrate-local-blobs.sh [report-file]' >&2
	exit 2
fi

report=${1:-"/root/blob-migration-$(date -u +%Y%m%dT%H%M%SZ).jsonl"}
if ! command -v jq >/dev/null 2>&1; then
	printf '%s\n' 'jq is required to validate the blob migration report' >&2
	exit 1
fi

printf 'Blob migration report: %s\n' "$report"
docker compose stop telegramd
docker compose run --rm --no-deps blob-migrate >"$report"
summary=$(tail -n 1 "$report")
if ! printf '%s\n' "$summary" | jq -e '
	.type == "summary" and
	(.source_manifest_sha256 | type == "string" and test("^[0-9a-f]{64}$")) and
	.destination_manifest_sha256 == .source_manifest_sha256
' >/dev/null; then
	printf '%s\n' 'blob migration did not report matching verified manifests' >&2
	exit 1
fi

install -d -m 0700 .state
printf '%s\n' "$summary" > .state/blob-migration-complete
chmod 600 .state/blob-migration-complete
docker compose up -d telegramd
