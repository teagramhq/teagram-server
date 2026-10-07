# RustFS transition qualification

`qualify-rustfs-transition.sh check BUNDLE CHECKOUT` is a read-only gate. It
accepts a root-owned mode-0700 bundle and a candidate checkout, prints only the
approved aggregate counts, SHA-256 digests and verdict, and never calls Compose
or changes the bundle. A passing verdict does not stop or start containers,
apply migrations, copy media, or publish a blob-mode record. The transition
runner must hold the shared deploy lock and consume this verdict before any
replacement or copy.

The bundle contains these fixed inputs, each root-owned mode 0600:

- `baseline-compose.json` and `candidate-compose.json`: resolved Compose JSON
  for the inspected local baseline and proposed RustFS candidate.
- `baseline.env`, `candidate.env`, `baseline.override.yml`, and
  `candidate.override.yml`: private byte-exact configuration snapshots.
- `baseline-containers.json` and `frozen-containers.json`: complete Docker
  inspect inventories. The former records every running baseline replica; the
  latter records the all-host writer freeze.
- `postgres.dump` and `qualification.json`: the dump and its completion,
  isolated-restore, freeze and capture timestamps, plus the source volume name.
- `source-provisional.tsv`, `source-frozen.tsv`, `copy-pass-1.tsv`,
  `copy-pass-2.tsv`, and `destination-census.tsv`: sorted
  `key<TAB>size<TAB>sha256` manifests.
- `references.tsv`: the complete `files`/`upload_parts` candidate query as
  `ref_kind<TAB>stored<TAB>blob_key`; `active-links.tsv` records
  `messages|channel_messages<TAB>file_id<TAB>deleted`.
- `migrations.json`: baseline revisions, candidate revisions and the migration
  66 schema query result.
- `candidate-secrets/telegramd-blob-secret-key`: the candidate app key, inside
  a root-owned mode-0700 directory; the file is mode 0444.

The candidate checkout must be root-owned at the checked configuration paths:
`.env` is mode 0600 and byte-identical to `candidate.env`; the protected
override matches `candidate.override.yml`; `.secrets` is mode 0700 and contains
only the mode-0444 app key. `.state/blob-mode` must already exist as a real
directory. The bound RustFS policy must retain its exact bucket and
`telegramd/*` scope. Migration files and Atlas pins 60–66 must match the
reviewed set byte-for-byte.

The gate rejects any configuration change outside the approved RustFS
services, secrets and S3 settings, the read-only retained `tgblobs` mount, the
single read-only realpath `.state/blob-mode` bind on every `telegramd*`
service, and no such bind on any other service, plus the already-approved
replica/trust values. Existing `.env` bytes
and override bytes must remain identical; only the four appended RustFS
credential lines are accepted. It checks the baseline named volume against
each inspected replica, a complete freeze, a fresh in-freeze dump, an
unchanged source recensus, reference coverage, matching independent manifests,
nonempty reference coverage, matching independent manifests, and exactly
migrations 60–66 with the pinned unread-mark schema.

All keys, row output, environment values, secret contents and diagnostics stay
in the private bundle. On rejection the bundle and running containers are
unchanged. The gate is only a qualification input; it cannot authorize a
transition by itself.
