# RPC performance harness

Measures every RPC desktop and web send at startup and in normal use against
an in-process teagramd on two fixtures: the scale account (10,000 contacts,
5,000 dialogs, a 1,000,000-message private chat, a 100,000-subscriber channel
and a 200,000-message group with photos) and the same account at one hundredth
of the data. Both are seeded through the store's write methods and the RPC
upload path.

Per scenario and fixture it records p50 and p95 latency, statements per call
(including the delivery work the call triggers), rows read (from `EXPLAIN
ANALYZE` of every statement), allocated bytes and every sequential scan over a
table above 10,000 rows.

`cmd/perfreport` judges the reports:

- Blocking from the first run: statement count and rows read against the base
  branch, the same statement count at 1x and 100x, rows read at 100x within 1.5x
  of 1x, no sequential scan, no statement Postgres cannot EXPLAIN, no failed
  scenario.
- Reported on every run, blocking once 20 `main` runs have given the scenario a
  noise band: p95 within budget (reads 20 ms, writes 50 ms, fan-out 2 s),
  p50 at 100x within 1.5x of 1x, and a median no slower than base by more than
  max(15%, 3 standard deviations) and 2 ms. A timing failure is measured once
  more before it counts.

A check `main` already fails is listed in `known_violations.json` with the issue
that fixes it. Base regressions cannot be listed. A sequential-scan entry names one scan by
the `table#fingerprint` the report prints (a hash of the full statement), so any
other scan in that scenario still fails.

## CI

`.github/workflows/perf.yml`. A `main` run seeds the fixtures when
`fixture_test.go` changed and stores the dump as a cache entry; every run
restores it, applies the branch's migrations on top, and measures base and head
interleaved. A PR that changes the fixture is not measured until it merges; a
missing snapshot for an unchanged fixture fails the gate.

## Locally

Needs Docker and `atlas`. Two empty databases, migrated:

```sh
atlas migrate apply --url "$DSN_1X" --dir file://migrations
atlas migrate apply --url "$DSN_100X" --dir file://migrations
export TG_PERF_DIR=/tmp/perf TG_PERF_DSN_1X=$DSN_1X TG_PERF_DSN_100X=$DSN_100X
TG_PERF_SHRINK=50 go test -tags performance -run '^TestSeedFixtures$' -timeout 2h ./test/perf
make perf
```

`TG_PERF_SHRINK` divides the large fixture so seeding takes minutes; drop it
for the real sizes (about an hour). `TG_PERF_ONLY=<substring>` runs matching
scenarios only, `TG_PERF_SQL=1` logs each scenario's statements and
`TG_PERF_LOG=1` the server's log.
