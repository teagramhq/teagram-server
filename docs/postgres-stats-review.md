# PostgreSQL statistics review

Use this short review for the Compose PostgreSQL 18 instance after the live
move. Run the first review one week after the move, then review weekly for the
rest of the first month and monthly after that. Execution and I/O counters are
cumulative since their last reset; tuple counts are estimates and size queries
are current snapshots. Compare results with the prior review before resetting
the execution and I/O counters for the next interval.

## Run the review

On the host, open `psql` in the Compose Postgres container as its trusted
operator role:

```sh
docker compose exec postgres psql -X -v ON_ERROR_STOP=1 -U postgres -d teagram
```

Paste each SQL query below at the `psql` prompt. The statistics views and
`pg_stat_statements` query text and identifiers are sensitive; access them only
as a trusted operator. The top-statement queries return representative SQL
text. Keep query results in the trusted operator session. For cross-review
comparisons, use one private snapshot at
`~/.local/state/teagram/postgres-stats-review/previous-review.txt` on the
Compose host. Keep the directory accessible only to the operator (`0700`) and
the snapshot readable and writable only by that operator (`0600`). Store only
the total-time top-five `queryid` values and per-table dead/live ratios needed
for comparison; never store SQL text. At each review, compare current results
with the snapshot, replace it with the current values, and remove any older
copies before resetting the counters. Retain only this latest snapshot until
the next review. See the
[statistics-reader boundary](security.md#statistics-reader-boundary).

### Top five by total execution time

```sql
SELECT queryid,
       calls,
       rows,
       round(total_exec_time::numeric, 1) AS total_ms,
       round(mean_exec_time::numeric, 1) AS mean_ms,
       query
FROM pg_stat_statements
WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
  AND calls > 0
ORDER BY total_exec_time DESC
LIMIT 5;
```

### Top five by mean execution time

```sql
SELECT queryid,
       calls,
       rows,
       round(total_exec_time::numeric, 1) AS total_ms,
       round(mean_exec_time::numeric, 1) AS mean_ms,
       query
FROM pg_stat_statements
WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
  AND calls > 0
ORDER BY mean_exec_time DESC
LIMIT 5;
```

### Relation read time against query execution time

`pg_stat_io` is cluster-wide. This ratio compares cluster-wide relation read
time with cluster-wide `pg_stat_statements` execution time, in milliseconds;
it is an aggregate signal, not elapsed wall-clock time, and can exceed 100% with
concurrent work. It includes I/O by background workers as well as queries. Read
time is collected because `track_io_timing` is enabled in the server
configuration. Check that the two `*_since` values cover the same review
interval before using the ratio.

```sql
WITH io AS (
    SELECT min(stats_reset) AS stats_since,
           coalesce(
               sum(read_time) FILTER (WHERE object IN ('relation', 'temp relation')),
               0
           )::numeric AS read_ms
    FROM pg_stat_io
), query_exec AS (
    SELECT info.stats_reset AS stats_since,
           coalesce(sum(statements.total_exec_time), 0)::numeric AS query_ms
    FROM pg_stat_statements AS statements
    CROSS JOIN pg_stat_statements_info AS info
    GROUP BY info.stats_reset
)
SELECT io.stats_since AS io_since,
       query_exec.stats_since AS query_time_since,
       round(io.read_ms, 1) AS relation_read_ms,
       round(query_exec.query_ms, 1) AS query_exec_ms,
       round(100 * io.read_ms / nullif(query_exec.query_ms, 0), 2) AS read_pct
FROM io
CROSS JOIN query_exec;
```

### Database size against `shared_buffers`

For this review, “near `shared_buffers`” means the database is at least 80% of
the configured buffer size. The query reports the percentage so the threshold
can be checked directly.

```sql
WITH sizes AS (
    SELECT current_database() AS database_name,
           pg_database_size(current_database()) AS database_bytes,
           pg_size_bytes(current_setting('shared_buffers')) AS shared_buffers_bytes
)
SELECT database_name,
       pg_size_pretty(database_bytes) AS database_size,
       pg_size_pretty(shared_buffers_bytes) AS shared_buffers,
       round(100.0 * database_bytes / nullif(shared_buffers_bytes, 0), 1)
           AS database_pct_of_shared_buffers
FROM sizes;
```

### Dead and live tuples by table

These tuple counts are estimates. Keep the full table list so the same table
can be checked across consecutive reviews.

```sql
SELECT schemaname,
       relname,
       n_live_tup,
       n_dead_tup,
       round(n_dead_tup::numeric / nullif(n_live_tup, 0), 3) AS dead_live_ratio,
       last_autovacuum
FROM pg_stat_user_tables
ORDER BY dead_live_ratio DESC NULLS LAST, schemaname, relname;
```

### `message_events` total size

`pg_total_relation_size` includes the table, indexes, and TOAST data.

```sql
SELECT pg_size_pretty(pg_total_relation_size('public.message_events')) AS total_size,
       pg_total_relation_size('public.message_events') AS total_bytes;
```

### Cache hit ratio and temporary files

These database counters are cumulative since `stats_reset`; use that timestamp
to interpret them and compare the counts with the previous review.

```sql
SELECT datname,
       stats_reset,
       round(100.0 * blks_hit / nullif(blks_hit + blks_read, 0), 2)
           AS shared_buffer_hit_pct,
       temp_files,
       pg_size_pretty(temp_bytes) AS temp_bytes_written
FROM pg_stat_database
WHERE datname = current_database();
```

## Triggers and follow-up

- If the database is at least 80% of `shared_buffers` and relation read time is
  above 5% of query execution time for the same interval, open an `io_method`
  review ticket. Keep `worker` unless the A/B review justifies a change. Use
  MAIN-2001's method: on an isolated copy seeded with MAIN-1987's scale data,
  set `shared_buffers` below the data size, then compare `worker` with
  `io_uring` using `EXPLAIN (ANALYZE, BUFFERS)` for sequential and bitmap scans
  and `VACUUM` duration over three runs each. A 20% p95 improvement is the
  evidence to reopen the setting; review the required seccomp change separately.
- If one table's estimated dead/live tuple ratio is above 0.2 in two consecutive
  reviews, open a ticket to review its autovacuum settings.
- If `message_events` exceeds 1 GiB (1,073,741,824 bytes), open an event-log
  retention ticket.
- If a statement newly enters the top five by total execution time compared
  with the prior review, open a query ticket. Identify it without copying SQL
  text into the general review issue.
- Record cache hit ratio and temporary-file counts as trends; no additional
  trigger is set for those measures.

End each review with one summary line on its tracking issue containing only
aggregate findings (database size versus `shared_buffers`, read-time percentage,
highest dead/live ratio, `message_events` size, cache hit ratio, and
temporary-file count), the triggers met, and any follow-up tickets opened. Do
not include query IDs, per-table names, database or role identifiers, or SQL
text in the issue note.

## Reset for the next interval

After recording the review, reset both counter sets so the next read-time ratio
covers the same interval as the query execution totals:

```sql
SELECT pg_stat_statements_reset();
SELECT pg_stat_reset_shared('io');
```

Both resets affect the whole PostgreSQL instance, not only `teagram`. Coordinate
with any other operator measuring these counters before resetting them.

See the PostgreSQL 18 documentation for
[monitoring statistics](https://www.postgresql.org/docs/18/monitoring-stats.html)
and [`pg_stat_statements`](https://www.postgresql.org/docs/18/pgstatstatements.html).
