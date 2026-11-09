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

The expiring-post summary migrations (`20261108000001` to `20261108000003`) add two
summary scopes for live posts with an expiry (by local ID, and by expiry time in
microseconds), an `expiring_ready` flag on `channel_post_summary_state`, and a
concurrently built expiry index. Group history reads the channel-wide expiring
total and expired-unswept backlog in O(log n) and runs the cheaper exact scan,
so per-open cost follows the smaller of the backlog and the live expiring posts.
The migrations read and rewrite no source rows. Existing channels start with
`expiring_ready = false` and history keeps the old direct expired-row count for
them, so the fix does not take effect until the rollout below finishes. New
channels are created ready. The scope check on `channel_post_summaries` is added
`NOT VALID` and validated in the next migration so no scan runs under an
exclusive lock.

Rollout, after the migrations are applied and the new binary is serving:

```bash
teagramd maintenance initialize-channel-post-expiring-summaries [--batch-size <1-1000>]
```

Each run rebuilds the expiring nodes of up to `--batch-size` (default 100)
ready channels, one transaction per channel, serialized with that channel's
posts, and prints `Channel post expiring summaries initialized: N channels, M
remaining`. Re-run until `M` is `0`; that is the completion signal, and it is
safe to interrupt and resume. A channel that fails is skipped for that run and named in the error together with the rebuilt and pending counts, so one bad channel does not hide progress. While any channel is pending, `teagramd` logs a startup warning with the pending count. Channels still waiting for
`initialize-channel-post-summaries` are rebuilt in full by that command, which
also sets the flag. Reversal: drop the index, the column and the scope-2 and
scope-3 rows, and restore the previous function bodies.

The group history page read needs no schema change or backfill. On a channel
whose expiring nodes are initialized and whose expired backlog is larger than
its unexpired expiring posts, it reads the page from the existing history TTL,
service and expiry indexes instead of walking past the expired rows; every other
channel keeps the primary-key walk.

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

While a dialog has an unready marker, the initializer and message triggers
serialize posting changes through one of 1024 transaction advisory lock
buckets derived from the affected dialog. A trigger invocation that touches
old and new dialogs acquires the buckets for marked dialogs in ascending bucket
order. Hash collisions only serialize unrelated dialogs; the buckets are
scoped by database. The initializer locks the unready marker, message rows by
`local_id`, dialog bucket, then posting and count rows.

Every application writer locks each affected owner before its first message-copy
write, including for ready dialogs. These owner advisory locks serialize
posting and count changes for ready dialogs, which do not take posting-dialog
buckets; a chat lock alone is not a substitute. Before a user-to-user send
inserts its first copy, a trigger locks the buckets for any unready reciprocal
dialogs in ascending order; the second copy reuses those locks. Message updates
and hard deletes lock any operation-specific poll rows, then the complete
affected message-copy set in `(owner_id, local_id)` order, then the buckets for
affected unready dialogs in ascending bucket order, and finally posting and
count rows. This transaction-wide row-before-bucket order covers revokes and
batches that touch several dialogs; their triggers reuse the already-held bucket
set. The initializer does not take owner or chat locks, and each chunk holds
message rows for only one dialog.

Before a basic-group send, chat forward, or album writes its first message
copy, it locks the buckets for recipients whose dialogs have unready markers as
one set in ascending bucket order. Its insert triggers reuse that set, so
disjoint group fan-outs with colliding buckets cannot acquire them in opposite
per-copy orders.

Sends, edits, and deletes in the dialog can wait for the active chunk to commit.
A send into that dialog can wait on its advisory bucket while holding its owner
locks, which queues other sends from that owner until the chunk commits. An
edit or delete that needs a row locked by the chunk waits before acquiring any
posting bucket, so it does not hold a bucket needed by the initializer. This
stall is limited to one chunk, rather than the dialog's full history; choose
`--batch-size` for the write delay the deployment can tolerate. The initializer
omits the configured Postgres statement timeout so it can finish a chunk;
message writers still use their normal request timeout. Run it in a low-traffic
window when large dialogs are expected.

Existing send, edit, and delete transactions acquire the affected owner locks
before changing message rows, then their message triggers update postings and
counts. A batch may touch rows in multiple dialogs: it locks the complete row
set by `(owner_id, local_id)` before taking any required unready-dialog bucket,
so it cannot hold a bucket while waiting for a row in the initializer's
current chunk. The initializer holds rows for only one dialog in one chunk
and releases them before starting another chunk.

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
