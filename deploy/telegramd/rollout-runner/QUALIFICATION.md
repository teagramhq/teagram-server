# RustFS transition qualification

`qualify-rustfs-transition.sh check BUNDLE CHECKOUT` is a read-only gate. It
accepts a root-owned mode-0700 bundle and a candidate checkout, renders the
candidate with `docker compose config` using a cleared environment and explicit
inputs, and compares that result with the bundled snapshot. Public output is
limited to the verdict, selected release set, applied revision IDs and the
`migrations.json` SHA-256 digest. It does not contact the Docker daemon or
change the bundle. A passing verdict does not stop or start containers, apply
migrations, copy media, or publish a blob-mode record. The transition runner
must hold the shared deploy lock and consume this verdict before any replacement
or copy.

The bundle contains these fixed inputs, each root-owned mode 0600:

- `baseline-compose.json` and `candidate-compose.json`: resolved Compose JSON
  for the inspected local baseline and proposed RustFS candidate.
- `baseline.env`, `candidate.env`, `baseline.override.yml`, and
  `candidate.override.yml`: private byte-exact configuration snapshots.
- `baseline-containers.json` and `frozen-containers.json`: complete Docker
  inspect inventories. The former records every running baseline service,
  including each container's complete mounts, Compose-configured environment,
  and normalized published ports; the latter records the all-host writer
  freeze. Baseline containers must match the baseline resolved Compose model
  exactly for those fields. Every non-profiled baseline service except the
  one-shot `migrate` job must have a running container; migration revision and
  schema evidence validates its completed work. Each normalized published port
  records `target`, `published`, `host_ip`, and `protocol`.
- `postgres.dump` and `qualification.json`: the dump and its completion,
  isolated-restore, freeze and capture timestamps, source volume name, and
  candidate Compose binding.
- `source-provisional.tsv`, `source-frozen.tsv`, `copy-pass-1.tsv`,
  `copy-pass-2.tsv`, and `destination-census.tsv`: sorted
  `key<TAB>size<TAB>sha256` manifests.
- `references.tsv`: the complete `files`/`upload_parts` candidate query as
  `ref_kind<TAB>stored<TAB>blob_key`; `active-links.tsv` records
  `messages|channel_messages<TAB>file_id<TAB>deleted`.
- `migrations.json`: baseline and applied revisions, successful Atlas revision
  details, and the migration 66 and, for R67, migration 67 schema query results.
- `candidate-secrets/telegramd-blob-secret-key`: the candidate app key, inside
  a root-owned mode-0700 directory; the file is mode 0444.

`qualification.json.candidate_compose_binding` records the canonical candidate
snapshot SHA-256 and raw SHA-256 digests for `.env`,
`docker-compose.override.yml`, and `docker-compose.yml` in the candidate
checkout. Capture `candidate-compose.json` with the same explicit Compose files
and `.env` under a cleared inherited environment. The gate verifies the input
digests, independently renders the current checkout with only a fixed system
`PATH` in its environment, and accepts the snapshot only when the resolved
JSON matches. The recorded hashes alone are not resolution evidence.

The candidate checkout must be root-owned at the checked configuration paths:
`.env` is mode 0600 and byte-identical to `candidate.env`; the protected
override matches `candidate.override.yml`; `docker-compose.yml` is a regular,
non-group/world-writable file; `.secrets` is mode 0700 and contains only the
mode-0444 app key. `.state` and `.state/blob-mode` must already exist as
root-owned real directories with no group or world write permission.
The bound RustFS policy must retain its exact bucket and `telegramd/*` scope.
The complete `atlas.sum` bytes select exactly one approved release: R66
(`60-66`) or R67 (`60-67`). The `migrations.json.release_set` value can only
confirm that selection; it cannot choose a release. Migration files and Atlas
pins must match the selected set byte-for-byte. R67 pins come from merged
commit `9139dd19222d002a2dfe83ae7fd261e0e0134d9e`, including the whole
`atlas.sum` SHA-256 `b2c094461a8224de2adde980a7c510e8254d5c0fae2d1e39a0dd9125cae8e9d9`
and migration 67 file SHA-256
`eb94b35a5303dd6ef3d22c8d3164b13284b9c7529071800af6592ff160573388` with Atlas
row `h1:Lux8heOMbxuuDRHoHwo61qwT/B+Nm05v6jFNbXvz2EE=`. The live migration
overlay passes only when its complete set equals R66 or R67; a future migration
therefore rejects. The vendored release fixture is checked against these pins
before a test bundle is constructed.

R67 `migrations.json` has a closed key set: `release_set`,
`baseline_revisions`, `revision_rows`, `target_revisions`,
`approved_revision_set_exact`, `migration_66_present`,
`migration_67_present`, `migration_66_schema`, `migration_67_schema`, and
`revision_detail`. `revision_detail` is keyed by all eight version IDs; each
entry has only `applied`, `total`, `error`, and `hash`, with equal positive
counts, an empty error, and that file's pinned Atlas hash. The R67 schema object
has only `table`, `index_validity`, and `party_date_indexes`; the first is
`public.secret_chats`, the validity map contains exactly the six approved index
names with `true` values, and the date-index map contains the two exact index
definitions. `qualification.json.freeze.baseline_schema_captured_at` records
the live baseline query in the freeze before the dump; `schema_captured_at`
records the applied query in that freeze.

The gate rejects any configuration change outside the approved RustFS
services, secrets and S3 settings, the read-only retained `tgblobs` mount, the
single read-only realpath `.state/blob-mode` bind on every `telegramd*`
service, and no such bind on any other service, plus the already-approved
replica/trust values. Existing `.env` bytes
and override bytes must remain identical; only the four appended RustFS
credential lines are accepted. It checks the baseline named volume against
each inspected replica, a complete freeze, a fresh in-freeze dump, an
unchanged source recensus, reference coverage, matching independent manifests,
nonempty reference coverage, and exactly one approved migration set. R66
preserves its 60–62 baseline and 60–66 applied revisions. R67 requires both its
baseline and applied revisions to equal 60–67,
with a complete successful revision-detail row for every migration. Its
baseline revision capture must be in the freeze and precede the dump; its
applied revision capture uses `schema_captured_at` in the same freeze. The R67
schema query must report exactly the six approved `secret_chats` indexes, all
valid, and both migration 67 indexes with their pinned btree, column-order,
validity, uniqueness, predicate, expression, key-count and ordering-option
properties. A checkout change never changes the database revisions or rolls
them back. Ordinary rollout remains the only authority that applies migrations.
The R67 indexes are already in its baseline; the RustFS transition and its
recovery paths neither apply migrations 63–67 nor drop indexes. The baseline
`migrate` job remains a no-op and `telegramd` still depends on its successful
completion. The gate checks the locked runner's `migrations.json` attestation;
it does not rederive schema facts from the dump.

All keys, row output, environment values, secret contents and detailed
diagnostics stay in the private bundle. Public output contains no counts, row
facts, plan text, configuration digests or secret data. On rejection the bundle
and running containers are unchanged. The gate is only a qualification input;
it cannot authorize a transition by itself.
