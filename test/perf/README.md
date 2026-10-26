# RPC performance harness

Measures every RPC desktop and web send at startup and in normal use against
an in-process teagramd on two fixtures: the scale account (10,000 contacts,
5,000 dialogs, a 1,000,000-message private chat, a 100,000-subscriber channel
and a 200,000-message group with photos) and the same account at one hundredth
of the data. Both are seeded through the store's write methods and the RPC
upload path.

The fixture owner can sign in with username `perftestowner` and password
`perf-fixture-password-2026`. These fixed credentials exist only in this test
fixture.

### Fixture credential threat model

These are public test credentials, not secrets. The performance workflow stores
the PostgreSQL dumps in the GitHub Actions cache on `main`, and pull-request
runs restore that cache. Anyone who obtains a dump can sign in as its owner on
a server restored from it and access or change the fixture data. Neither the
password nor cache visibility protects the owner account.

The fixture contains synthetic performance data only; production or user data
must never be seeded into it. The login test binds its in-process server to
loopback and must use disposable databases. Never restore these snapshots into
production or a shared service, and never reuse these credentials for another
account. Publishing or distributing a snapshot or a service restored from it
is out of scope and requires separate review. If live data enters a fixture,
treat it as exposed, remove it, rebuild the snapshot, and rotate any real
credentials it contained; deleting the cache cannot recall copies already
restored by PR runs.

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

`required_scenarios.json` lists the scenarios the gate must see measured on
both fixtures. It is kept apart from the harness on purpose: a scenario dropped
from `scenarios()` still fails as unmeasured, and a run limited by
`TG_PERF_ONLY` fails rather than passing on a subset.

A check `main` already fails is listed in `known_violations.json` with the issue
that fixes it. Base regressions cannot be listed. A sequential-scan entry names one scan by
the `table#fingerprint` the report prints (a hash of the full statement), so any
other scan in that scenario still fails.

## CI

`.github/workflows/perf.yml`. The fixture cache key includes
`fixture_test.go` and `server_test.go`. Main stores a v2 snapshot with a
`.fixture-metadata.json` provenance sidecar when that key is missing. PRs prefer
that snapshot, reuse the legacy cache for measurement only, or seed fixtures on
their branch when neither cache is available; they then measure the base and
head interleaved. If the fixture seeder changed, the head is measured without a
base comparison.

The provenance sidecar records the commit that seeded the dump. It is cache
metadata, not a fixture asset. Each `main` run also publishes the snapshot as a
public prerelease named
`perf-fixture-<fixture version>`, where the version is the cache key suffix
(the Actions `hashFiles` value for `test/perf/fixture_test.go` and
`test/perf/server_test.go`). The release includes every file under the snapshot
directory and a `manifest.json`. Snapshot paths are
flattened into asset names by replacing `/` with `__`; a dump larger than
1,900,000,000 bytes is split into `<asset name>.part000`, `.part001`, and so
on; join numbered parts in order to reconstruct the file. The manifest has
`fixture`, `server_commit` (the seeding commit), `migration_revision`, and an
`assets` array containing each asset's `name` and `sha256`. The manifest itself
is not listed in that array. Releases stay prereleases and are never marked
latest. Consumers can download the assets anonymously from the release URL.

## Locally

Needs Docker and `atlas`. Two empty databases, migrated:

```sh
atlas migrate apply --url "$DSN_1X" --dir file://migrations
atlas migrate apply --url "$DSN_100X" --dir file://migrations
export TG_PERF_DIR=/tmp/perf TG_PERF_DSN_1X=$DSN_1X TG_PERF_DSN_100X=$DSN_100X
TG_PERF_SHRINK=50 go test -tags performance -run '^TestSeedFixtures$' -timeout 2h ./test/perf
go test -tags performance -run '^TestOwnerUsernamePasswordLogin$' -timeout 5m ./test/perf
make perf
```

`TG_PERF_SHRINK` divides the large fixture so seeding takes minutes; drop it
for the real sizes (about an hour). `TG_PERF_ONLY=<substring>` runs matching
scenarios only, `TG_PERF_SQL=1` logs each scenario's statements and
`TG_PERF_LOG=1` the server's log.
