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

Migrations `20261101000001_message_search_postings.sql` and
`20261101000002_message_search_postings_readiness.sql` add an ordered lexeme
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
