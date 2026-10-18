-- Index the secret-chat lifecycle replay by party and date.
--
-- getDifference replays lifecycle transitions with the predicate
-- (admin_id = $1 OR participant_id = $1) AND date > $2. Neither index that
-- ships with the secret_chats schema can serve it: both
-- secret_chats_admin_state_idx and secret_chats_participant_state_idx lead
-- with the party but carry state, not date, so neither side of the OR is
-- indexable and every poll re-reads the whole table to return a handful of
-- rows. Read work then grows with a user's lifetime chat count rather than
-- with what changed since their cursor.
--
-- These two party-and-date indexes give each side of the OR its own index
-- scan with the date bound inside the Index Cond, so the planner combines two
-- bounded scans and reads only the pages holding the changed rows.
--
-- Plain CREATE INDEX, not CONCURRENTLY, following the messages_file_idx
-- precedent: it takes SHARE on secret_chats for the duration of the build,
-- which pauses writes but never blocks reads, and it keeps this file inside
-- Atlas's per-file transaction. A build that fails or times out therefore
-- rolls back completely and leaves no INVALID index for the planner to ignore
-- in silence, which is what a CONCURRENTLY build would risk. Both timeouts are
-- SET LOCAL, so they cover this migration's transaction and nothing else:
-- lock_timeout bounds the wait for the SHARE lock, statement_timeout bounds
-- the build itself.
--
-- Additive only: no rewrite, no backfill, no removal. Old code ignores both
-- indexes, so a code revert leaves them safely in place and any eventual
-- removal is a separate forward DROP INDEX. The accepted cost is that date
-- stops being HOT-updatable: AcceptSecretChat and DiscardSecretChat now write
-- a new index entry per transition.
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '120s';

CREATE INDEX secret_chats_admin_date_idx ON secret_chats (admin_id, date);
CREATE INDEX secret_chats_participant_date_idx ON secret_chats (participant_id, date);
