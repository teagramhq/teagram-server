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

The wrapper verifies the reviewed SHA-256 bundle over
`qualify-rustfs-transition.py`, `rustfs-schema-capture.sql`, and
`rustfs-inert-surfaces.sql` before invoking Python. Its approved digest is
`4f825040166168318c1b708f150910e5dd9ef834109f013ca37b6ed45cf8f028`; changing
any of these artifacts requires a reviewed pin update. The fixture suite
checks that obsolete digests reject each changed artifact.

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
(`60-66`), R67 (`60-67`), or R69 (`60-69`). The `migrations.json.release_set`
value can only confirm that selection; it cannot choose a release. Migration
files and Atlas pins must match the selected set byte-for-byte. R67 pins come from merged
commit `9139dd19222d002a2dfe83ae7fd261e0e0134d9e`, including the whole
`atlas.sum` SHA-256 `b2c094461a8224de2adde980a7c510e8254d5c0fae2d1e39a0dd9125cae8e9d9`
and migration 67 file SHA-256
`eb94b35a5303dd6ef3d22c8d3164b13284b9c7529071800af6592ff160573388` with Atlas
row `h1:Lux8heOMbxuuDRHoHwo61qwT/B+Nm05v6jFNbXvz2EE=`. The live migration
overlay passes only when its complete set equals R66, R67 or R69; a future
migration therefore rejects. The vendored release fixture is checked against
these pins before a test bundle is constructed.

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
it cannot authorize a transition by itself.

The PostgreSQL 16 CI proof applies the actual immutable migration directory with
Atlas 1.2.0, runs the two capture SQL files, validates the empty catalog, and
checks isolated SQL mutations for ordered keys, foreign-key actions and MATCH
types, `SET NULL` column subsets, and `conindid` targets. Each mutation runs in a
transaction from the passing catalog and rolls back. Run it locally with:

```sh
sudo env "PATH=$PATH" TMPDIR=/root bash deploy/telegramd/rollout-runner/test-rustfs-schema-postgres.sh
```
