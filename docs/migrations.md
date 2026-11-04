# Database migrations

Schema changes are managed with [Atlas](https://atlasgo.io). Migration files live
in `migrations/` and are tracked by `migrations/atlas.sum` (an integrity hash).
Atlas records which files it has applied, so migrations run exactly once — no
`IF NOT EXISTS` guards.

Config is in `atlas.hcl` under `env "local"`:

- `url` — the target database, read from `TG_POSTGRES_DSN`.
- `dev` — a throwaway Postgres 18 container Atlas spins up to parse/execute SQL
  for `validate` and `diff` (`docker://postgres/18`). Requires Docker.

## Add a migration

Either hand-write a file `migrations/<version>_<name>.sql` (version is a sortable
timestamp, e.g. `20260721000001`), then re-hash:

```bash
atlas migrate hash --env local
```

Or diff against a desired schema to generate one:

```bash
make migrate-new name=add_sessions   # atlas migrate diff ... --env local
```

## Validate

```bash
atlas migrate validate --env local                 # checksum only
atlas migrate validate --env local \
  --dev-url "docker://postgres/18/dev?search_path=public"   # + SQL semantics
```

## Apply

```bash
export TG_POSTGRES_DSN="postgres://user:pass@host:5432/db?sslmode=disable"
make migrate   # atlas migrate apply --env local
```

The channel post summary migration adds hooks and derived-state tables without
backfilling existing channels. Initialize one channel explicitly with:

```bash
teagramd maintenance initialize-channel-post-summaries --channel-id <id>
```

The command rebuilds only that channel's summary nodes and readiness in a
separate transaction from the Atlas migration. It serializes with that
channel's posts; draining writers is an operational performance precaution,
not a correctness requirement.

The basic-group read-receipt migration adds only `chat_read_receipts`; it does
not alter messages or read markers and does not fabricate dates for earlier
reads. If the application rollout is reverted, leave this additive table in
place. The existing message and dialog read state remains intact, and the
pre-receipt application ignores the table. Reapplying the application later
continues to use any receipts already captured. The schema expansion is
reversible with loss of the feature-only receipt rows: an Atlas down migration
removes this table and its index, but does not change messages or dialog read
markers. Receipt dates removed this way cannot be reconstructed or backfilled.

The test harness (`internal/pgtest`) and production both apply these same
migrations to reach the current schema. (Wiring pgtest to apply them lands in a
later task.)

### Ordered message-search postings

Migrations `20261102000001_message_search_postings.sql` and
`20261102000002_message_search_postings_readiness.sql` add an ordered lexeme
posting table, per-dialog lexeme counts, and a marker table for dialogs whose
existing history has not been initialized. The first migration installs and
commits the message-write triggers; the second takes the readiness snapshot in
a separate transaction so its full-table read does not extend the lock needed
to install those triggers. Message inserts, text edits, soft deletes, and hard
deletes keep postings and counts in sync transactionally. The migrations do
not copy historical lexemes into the postings table. Existing dialogs with
messages remain on the original `messages.search` query until the separate
history initializer runs; dialogs with no messages are ready immediately, and
new dialogs use postings from their first message. Ready-dialog searches use
the per-dialog counts to walk postings for the least frequent query lexeme. The
initializer must build counts from the same history before clearing a dialog's
unready marker.

Initialize the existing dialogs with:

```bash
teagramd maintenance initialize-message-search-postings [--batch-size <n>]
```

The default batch size is 100 messages per dialog transaction; `n` can be from
1 to 1000 and bounds the message rows locked and rebuilt in each chunk. The
initializer scans dialog keys in owner, peer type, and peer ID order. For each
dialog it locks the unready marker, then up to `n` message rows by `local_id`,
then that chunk's postings and posting-count rows. Each chunk has its own
transaction. Its commit advances the marker's `last_local_id`; after the final
empty chunk, the marker is deleted and the dialog becomes ready.

The source rows are locked before their postings are cleared and rebuilt from
the same statement snapshot. A chunk only clears postings for its selected
message IDs, so inserts outside that chunk keep the postings maintained by the
message triggers. Edits and deletes of selected rows wait for the chunk to
commit; later chunks see their committed state. The marker remains until all
chunks complete, so searches use the legacy query during a partial rebuild.
If the process stops or a chunk fails, that chunk's postings and cursor update
roll back together. Earlier chunks remain committed and the marker stays
unready; a rerun resumes at its saved cursor.

Sends, edits, and deletes in the dialog can wait for the active chunk to commit.
A send with an overlapping lexeme can wait on the chunk's posting-count row
while holding its owner advisory locks, which queues that owner's other sends
until the chunk commits. This stall is limited to one chunk, rather than the
dialog's full history; choose `--batch-size` for the write delay the deployment
can tolerate. The initializer omits the configured Postgres statement timeout
so it can finish a chunk; message writers still use their normal request
timeout. Run it in a low-traffic window when large dialogs are expected.

Existing send, edit, and delete transactions acquire their owner or chat locks
before changing message rows, then their message triggers update postings and
counts. Some message mutations can touch rows in multiple dialogs in one
transaction. The initializer holds rows for only one dialog in one chunk and
releases them before starting another chunk, so it cannot form a cross-dialog
message-row lock cycle with those mutations.

Reverting the application leaves these additive tables, index, and triggers in
place. The old search query ignores the postings, so its results remain
unchanged; the database triggers still maintain postings on message writes.
Application rollback alone does not stop collection or erase existing rows.

The lexemes are sensitive message-derived data. Their access, retention,
erasure, and rollback boundaries are documented in the
[message-search postings threat model](security.md#threat-model-private-message-search-postings).

Browser acceptance binding evidence is an additive, unenrolled expansion.
Its versioned read API, owner-only enrollment transaction, retention and rollback
contract are documented in [browser-acceptance-binding-evidence.md](browser-acceptance-binding-evidence.md).
