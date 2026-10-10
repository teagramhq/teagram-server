# RustFS transition qualification

`qualify-rustfs-transition.sh check BUNDLE CHECKOUT` is a read-only gate. It
accepts a root-owned mode-0700 bundle and a candidate checkout, renders the
candidate with `docker compose config` using a cleared environment and explicit
inputs, and compares that result with the bundled snapshot. Public output is
limited to the verdict, selected release set, applied revision IDs and the
`migrations.json` SHA-256 digest. The gate does not contact Docker or change the
bundle. A passing verdict does not stop or start containers, apply migrations,
copy media or publish a blob-mode record. The transition runner must hold the
shared deploy lock and consume this verdict before any replacement or copy.

`blob-migrate --manifest stdout` emits the canonical private TSV from each
verified copy or independent census. Callers store that output only in
root-owned mode-0600 evidence files. Its stderr contains aggregate counts and
manifest digests, and copy errors in this mode do not print object keys.

`qualify-rustfs-transition.sh pre-copy BUNDLE CHECKOUT` validates the private
baseline, candidate, freeze, dump, schema, source census and reference evidence
before the first transfer. It expects the four copy and destination-census
manifests to be absent. The locked runner runs it before starting RustFS or
invoking `blob-migrate`, then records copy and independent destination-census
output after each pass and runs the full `check` before publishing authority.

The transition runner freezes writers before relying on caller-supplied freeze
artifacts. While holding the shared lock with `telegramd*` stopped, it captures
a fresh database dump, queries the frozen database for applied Atlas revisions
and the migration schema, captures both exact reference queries, and takes a
fresh local-volume census. The private `migrations.json.live_capture` records
the observed schema, query digest, capture time and SHA-256 of the matching
dump; the gate compares that live observation to the pinned migration
attestation. R67 also captures schema before the dump and after it while writers
remain frozen, rejects a change between captures, and binds both observations
to the dump digest. Recovery repeats these captures after freezing and binds
them to its private recovery proof. The raw dump, query output, manifests and
Docker diagnostics remain in the root-owned private attempt directory.

The wrapper verifies the reviewed SHA-256 bundle over
`qualify-rustfs-transition.py`, `rustfs-schema-capture.sql`, and
`rustfs-inert-surfaces.sql` before invoking Python. Its approved digest is
`82b2e02d005b6162ca6179c374ad97086af94fc6332f5409e91c9e880c1415ae`; changing
any of these artifacts requires a reviewed pin update. The fixture suite
checks that obsolete digests reject each changed artifact.

The bundle contains these fixed inputs, each root-owned mode 0600:

- `baseline-compose.json` and `candidate-compose.json`: resolved Compose JSON
  for the inspected local baseline and proposed RustFS candidate.
- `baseline.env`, `candidate.env`, `baseline.override.yml` and
  `candidate.override.yml`: private byte-exact configuration snapshots.
  `baseline.env` is copied byte-for-byte from the pre-provision environment
  bound to MAIN-1387's recovery point and the inspected baseline. It is never
  derived by removing credentials from `candidate.env`. Its provenance digests
  and recovery-point identifier are recorded in
  `qualification.json.baseline_env_provenance` and the private
  `baseline-recovery-point.json` source record. Missing or mismatched
  provenance rejects qualification.
- `baseline-containers.json` and `frozen-containers.json`: complete Docker
  inspect inventories. The former records every running baseline service,
  including complete mounts, Compose-configured environment and normalized
  published ports; the latter records the all-host writer freeze. Baseline
  containers must match the resolved Compose model. Each normalized published
  port records `target`, `published`, `host_ip` and `protocol`.
- `postgres.dump` and `qualification.json`: the dump and its completion,
  isolated-restore, freeze and capture timestamps, source volume and candidate
  Compose binding.
- `source-provisional.tsv`, `source-frozen.tsv`, `copy-pass-1.tsv`,
  `copy-pass-2.tsv`, `destination-census-pass-1.tsv` and
  `destination-census-pass-2.tsv`: sorted `key<TAB>size<TAB>sha256` manifests.
  Each destination census follows its own checksum-copy pass.
- `references-provisional.tsv` and `active-links-provisional.tsv`: the
  pre-freeze query results. `references.tsv` and `active-links.tsv` are the
  same queries captured after writers are frozen; the gate rejects either
  byte-level change. Reference rows use
  `ref_kind<TAB>stored<TAB>blob_key`; active links use
  `messages|channel_messages<TAB>file_id<TAB>deleted`.
- `migrations.json`: baseline and applied revisions, successful Atlas revision
  details, pinned migration 66 and, for R67, migration 67 schema results, plus
  the live frozen-database observation bound to the fresh dump digest.
- `candidate-secrets/telegramd-blob-secret-key`: the candidate app key inside
  a root-owned mode-0700 directory; the file is mode 0444.

`qualification.json.baseline_env_provenance` binds the source issue and
recovery-point identifier, recovery-point digest, raw `baseline.env` digest,
inspected baseline inventory digest, inspected environment digest, baseline
Compose snapshot digest and inspection timestamp. The recovery-point source
record carries the same baseline environment, inventory and Compose digests.
Environment digests must equal the raw bytes of `baseline.env`; inventory and
Compose digests must match their bundled snapshots. The bundle and each file
remain root-owned and private.

`qualification.json.candidate_compose_binding` records the canonical candidate
snapshot SHA-256 and raw SHA-256 digests for `.env`,
`docker-compose.override.yml` and `docker-compose.yml`. The gate verifies the
input digests, independently renders the current checkout with a fixed system
`PATH`, and accepts the snapshot only when the resolved JSON matches. Recorded
hashes alone are not resolution evidence.

The candidate checkout must be root-owned at the checked configuration paths:
`.env` is mode 0600 and byte-identical to `candidate.env`; the protected
override matches `candidate.override.yml`; `docker-compose.yml` is a regular,
non-group/world-writable file; `.secrets` is mode 0700 and contains only the
mode-0444 app key. `.state` and `.state/blob-mode` must already exist as
root-owned real directories with no group or world write permission. The bound
RustFS policy must retain its exact bucket and `telegramd/*` scope.

The exact target revision list in `migrations.json` selects one pinned release:
R66 (`60-66`), R67 (`60-67`), R69 (`60-69`) or R70 (`60-70`);
`migrations.json.release_set` only confirms that selection. The validator checks
the vendored release snapshot, including its exact file inventory, complete
`atlas.sum` digest, per-file digests and selected Atlas rows. The candidate
checkout must have a one-to-one inventory between its SQL files and Atlas rows,
and the validator recomputes every cumulative Atlas file hash. R66 and R67
overlays may retain later SQL files only when their bytes match their Atlas
hashes. The live migration overlay passes only when its complete set equals an
approved release, and the frozen live database must report exactly that
release's applied revisions. Vendored fixture inputs are checked against the
same pins before test bundles are constructed. R67 pins come from merged commit
`9139dd19222d002a2dfe83ae7fd261e0e0134d9e`, including the whole `atlas.sum`
SHA-256 `b2c094461a8224de2adde980a7c510e8254d5c0fae2d1e39a0dd9125cae8e9d9`
and migration 67 file SHA-256
`eb94b35a5303dd6ef3d22c8d3164b13284b9c7529071800af6592ff160573388` with Atlas
row `h1:Lux8heOMbxuuDRHoHwo61qwT/B+Nm05v6jFNbXvz2EE=`.

R67 `migrations.json` has a closed key set: `release_set`,
`baseline_revisions`, `revision_rows`, `target_revisions`,
`approved_revision_set_exact`, `migration_66_present`,
`migration_67_present`, `migration_66_schema`, `migration_67_schema` and
`revision_detail`, with only the runner's two dump-bound live-capture records
added during a frozen transition. `revision_detail` is keyed by all eight
version IDs; each entry has only `applied`, `total`, `error` and `hash`, with
equal positive counts, an empty error and that file's pinned Atlas hash. The
live Atlas query independently captures those fields from the database and
checks them against the pinned rows in both cutover and recovery freezes. The
R67 schema object has only `table`, `index_validity` and `party_date_indexes`;
the table is `public.secret_chats`, the validity map contains exactly the six
approved indexes with `true` values, and the date-index map contains the two
pinned index definitions. `qualification.json.freeze.baseline_schema_captured_at`
records the live baseline query before the dump; `schema_captured_at` records
the applied query in the same freeze.

The candidate `.env` must equal `candidate.env` byte-for-byte. `candidate.env`
must preserve every `baseline.env` byte and append only the four approved
credential lines in the validator's fixed order. No normalization or digest
waiver is accepted. The gate rejects configuration changes outside the
approved RustFS services, secrets and S3 settings, the read-only retained
`tgblobs` mount, the single read-only realpath `.state/blob-mode` bind on every
`telegramd*` service and no such bind on any other service, plus the approved
replica and trust values. Existing override bytes must remain identical.

The gate checks the baseline named volume against each inspected replica, a
host-wide writer freeze, a fresh in-freeze dump, an unchanged source recensus, unchanged
reference snapshots, complete reference coverage, matching copy and destination
manifests from both passes, nonempty reference coverage and exactly one
approved migration set. R66 preserves its 60–62 baseline and 60–66 applied
revisions. R67 requires both baseline and applied revisions to equal 60–67,
with a complete successful revision-detail row for every migration. Its
baseline schema capture must be in the freeze before the dump, and its applied
schema capture must follow it in the same freeze. The R67 live schema query must
report exactly the six approved `secret_chats` indexes, all valid, and both
migration 67 indexes with their pinned btree, column-order, validity,
uniqueness, predicate, expression, key-count and ordering-option properties. A
checkout change never changes database revisions or rolls them back. Ordinary
rollout remains the only authority that applies migrations. The transition and
recovery paths neither apply migrations 63–67 nor drop indexes.

R69 pins are from the immutable merge `c6353f205aa286a8c83513eabb88ee5c2049b94f`.
Its whole `atlas.sum` SHA-256 is
`c54c4c43a1941519fb5ea7a62e56b5286853496c42d4238420bcf761f9577e77`; migration
68 is pinned to SHA-256
`e4d3aed863bc6bec9feec53c89859ace763a3832a203fbbab8a7ba777b284ae0` and Atlas
`h1:wpeh1DEL6OoF7lR2m5RPeMVAC/maP9l+mXGeHsTZnXc=`; migration 69 is pinned to
SHA-256 `4972fad76892ac89bf8529b16b9ec1679773fd9257f037c621397f59383ca490`
and Atlas `h1:gX6I/YcbJQsvERf2mEbtNolprFNg8fVzx/05XGa6ESM=`. The exact ten-file
60–69 set is required; changed bytes, an extra file, or any 70+ revision
rejects.

R69 `migrations.json` has a closed key set. It retains the R67 fields and
`migration_66_schema`/`migration_67_schema`, then adds only
`migration_68_present`, `migration_68_schema`, `migration_69_present`,
`migration_69_schema`, and `inert_surfaces`. Its baseline and applied revision
sets are both exactly 60–69, with ten complete successful `revision_detail`
entries. The 68 snapshot checks the direct `files_id_uploader_id_key` btree
catalog properties and exact `files` index set. The 69 snapshot checks all four
tables' columns, defaults, constraints, ordered keys, referenced keys, foreign
key actions and `conindid` targets, exact index sets and index validity.
Constraint check expressions remain matched by name and count.

The inert query returns four `EXISTS` booleans for `user_photos`,
`profile_photo_state`, `profile_upload_receipt`, and `profile_delete_operation`.
Its SHA-256 is
`2d0c108eb69b0cab431f01837a649e5e7f14d33483aae677be1032d5aa32cfe3`; the
R69 evidence must attest this query and all four results must be false. The
capture command, run inside the freeze, writes root-only evidence:

```sh
sudo install -d -m 700 /root/telegramd-rollout-runner/r69-capture
sudo env COMPOSE_FILE="$COMPOSE_FILE" bash -c '
  set -eu
  docker compose exec -T postgres psql -X -qAt -v ON_ERROR_STOP=1 -U postgres -d telegram \
    < /opt/telegram-server/deploy/telegramd/rollout-runner/rustfs-schema-capture.sql \
    > /root/telegramd-rollout-runner/r69-capture/catalog.json
  docker compose exec -T postgres psql -X -qAt -v ON_ERROR_STOP=1 -U postgres -d telegram \
    < /opt/telegram-server/deploy/telegramd/rollout-runner/rustfs-inert-surfaces.sql \
    > /root/telegramd-rollout-runner/r69-capture/inert-surfaces.json
  chmod 600 /root/telegramd-rollout-runner/r69-capture/catalog.json \
    /root/telegramd-rollout-runner/r69-capture/inert-surfaces.json
'
sha256sum deploy/telegramd/rollout-runner/rustfs-inert-surfaces.sql
```

Merge the catalog objects into the closed `migrations.json` fields and record
the inert query digest under `qualification.json.references` before running the
read-only gate. Any row in one of the four successor tables rejects with
`reference_coverage` and exhausts R69. R69 also rejects a nonempty `files`
table; no migration or successor catalog is widened to handle that state.

R70 pins are from immutable merge `a3613f888b1c3320aa74484c369d76c987333d30`
and its migration tree `9c5b55176f2a0e1bb6705e979213b0337d2d3204`. The whole
`atlas.sum` SHA-256 is
`e16da8e46119290cac52762235a47f47efd796ab2847db56a368a36d3b2ec608`;
`20261008000070_erasure_outbox_epoch_markers.sql` is pinned to SHA-256
`cf7bc135c5df5a539cf5b77d76136ab321fc63e6d0a787566b8e052779a2c` and Atlas
`h1:3ZPWNySt9YWg9s+xi+aQVAFrcSkeym+fGlL4PgkIayk=`. Exactly eleven filenames
from 60 through 70 are approved. Any change to the pinned files or `atlas.sum`,
an extra migration file, or a 71+ revision rejects. R70 retains the R69 pins
unchanged.

R70 `migrations.json` retains the R69 fields and adds only
`migration_70_present` and `migration_70_schema`. Baseline and applied revisions
must both equal 60–70. The eleven `revision_detail` entries each require a
positive `applied == total`, the pinned Atlas hash, empty `error`,
`error_stmt_empty: true`, and `partial_hashes_empty: true`. The capture includes
only those empty-state booleans for error statements and partial hashes. Both
live revision captures must occur during the freeze before the verified dump.
S3-to-local recovery repeats the R70 baseline and applied captures before its
verified dump and runs the seven-surface query during that same freeze. The
recovery bundle records the query pin in `recovery.json.references` and the
capture time in `recovery.json.freeze`; its `migrations.json` booleans come
from that fresh query before the schema evidence digest is written.

The R70 catalog capture checks every column type, NOT NULL, no default, no
identity and no sequence for `erasure_outbox`, `erasure_epoch`, and
`erasure_epoch_completion`. It checks the exact check names and counts, ordered
primary and unique keys, exact valid index sets, validated foreign-key columns,
referenced columns and index, delete/update actions, MATCH type and
deferrability. The sole allowed foreign key is
`erasure_epoch_completion_marker_exists` from
`erasure_epoch_completion(epoch, lineage_id)` to `erasure_epoch(epoch,
lineage_id)` with `RESTRICT`, `NO ACTION`, `MATCH SIMPLE`, validation enabled,
and `erasure_epoch_pkey`. There may be no other inbound or outbound foreign key
and no user trigger on these tables.

R70 uses a separately pinned seven-table inert query. Its SHA-256 is
`ade88675d3c578cc42eea5ca7250a224c05cd68a3cf814c045dec488bc60cee5`; all seven
`EXISTS` results must be false. The existing R69 four-table query and its
`REFERENCE_QUERY` and `ACTIVE_LINKS_QUERY` bytes and hashes remain unchanged.
Capture both R70 queries during the writer freeze and store them root-only:

```sh
sudo install -d -m 700 /root/telegramd-rollout-runner/r70-capture
sudo env COMPOSE_FILE="$COMPOSE_FILE" bash -c '
  set -eu
  docker compose exec -T postgres psql -X -qAt -v ON_ERROR_STOP=1 -U postgres -d telegram \
    < /opt/telegram-server/deploy/telegramd/rollout-runner/rustfs-r70-schema-capture.sql \
    > /root/telegramd-rollout-runner/r70-capture/catalog.json
  docker compose exec -T postgres psql -X -qAt -v ON_ERROR_STOP=1 -U postgres -d telegram \
    < /opt/telegram-server/deploy/telegramd/rollout-runner/rustfs-r70-inert-surfaces.sql \
    > /root/telegramd-rollout-runner/r70-capture/inert-surfaces.json
  chmod 600 /root/telegramd-rollout-runner/r70-capture/catalog.json \
    /root/telegramd-rollout-runner/r70-capture/inert-surfaces.json
'
sha256sum deploy/telegramd/rollout-runner/rustfs-r70-inert-surfaces.sql
```

Put `migration_70_schema` in the closed `migrations.json`, the seven booleans
in `inert_surfaces`, and the inert query digest in
`qualification.json.references`. Record
`qualification.json.freeze.inert_surfaces_captured_at` within the same held
freeze. Any row in one of the seven tables or in `files` returns
`reference_coverage` and exhausts R70. R70 applies equally to cutover and
S3-to-local recovery; recovery restores blobs only and never applies, repairs,
or drops database schema or rows. Ordinary rollout remains the only schema
authority.

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

R69 requires both its in-freeze baseline and applied revision captures to equal
60–69. It checks the full successor catalog and rejects `files` or any successor
table row. RustFS never applies migrations 68 or 69, builds the concurrent
index, repairs schema, or changes ordinary rollout's schema authority. Its
README exhaustion contract is explicit: any successor row, nonempty `files`,
or migration 70+ requires a new Security-reviewed release.

All keys, row output, environment values, secret contents and detailed
diagnostics stay in the private bundle. Public output contains no counts, row
facts, plan text, configuration digests or secret data. On rejection the bundle
and running containers are unchanged. The gate is only a qualification input;
it cannot authorize a transition by itself. MAIN-1332's client/photo
prerequisites continue to gate media acceptance. Live post-67
transitions remain gated by MAIN-1418/1419/1420: the separate 60–67 batch
contract and ordinary rollout must land, R67 qualification must land, and both
baseline and applied state must be exactly 60–67. This runner does not apply
migrations 63–67 or drop 67 indexes on rollback.

The PostgreSQL 16 CI proof applies the actual immutable migration directory with
Atlas 1.2.0, runs the two capture SQL files, validates the empty catalog, and
checks isolated SQL mutations for ordered keys, foreign-key actions and MATCH
types, `SET NULL` column subsets, and `conindid` targets. Each mutation runs in a
transaction from the passing catalog and rolls back. Run it locally with:

```sh
sudo env "PATH=$PATH" TMPDIR=/root bash deploy/telegramd/rollout-runner/test-rustfs-schema-postgres.sh
```
