# Isolated browser acceptance runtime

This is a one-shot Compose project named `teagram-browser-acceptance`. It is
separate from the server stack and owns two containers: an internal-only
Chromium runner and the dependency-free CONNECT observer. The browser can reach
only the observer. The observer is dual-homed and permits CONNECT to the one
pinned Tailscale target after validating the deployed artifact metadata.

The runner selects the architecture-specific Playwright image digest in
`run.sh`; the image adds only the integrity-locked `@playwright/test@1.61.1`
and `playwright@1.61.1` packages. The approved QA runner is pinned by its SHA-256
before it can be mounted. Both containers run as UID 1000 with all capabilities
dropped, `no-new-privileges`, a read-only root, no log driver, zero core dumps,
equal memory and memory-plus-swap limits, and bounded CPU and memory. Browser
profile, HOME, `/tmp`, and `/dev/shm` are bounded tmpfs mounts with `noexec`,
`nosuid`, and `nodev`. Chromium binaries and Node modules stay in the image.

The host runner requires curl 8.4.0 or newer so unknown-length responses are
stopped during transfer at 16 KiB plus one byte. Its project lock is held on a
mode-0700 directory under `XDG_RUNTIME_DIR`, or `$HOME/.local/run` when that
variable is unset.

Existing input files are checked as regular files with final-component symlinks
rejected, then canonicalized with plain `realpath` so both GNU and BusyBox
implementations work. Relative names are passed with a `./` prefix to prevent
option parsing; paths containing colons remain disallowed for safe bind mounts.

The custom seccomp profile is derived from Docker's default profile vendored by
Moby release `v28.5.2`, commit
`89c5e8fd66634b6128fc4c0e6f1236e2540e46e0`, SHA-256
`01536f1d1df938ae611eba20d6349e0de7a99b6ecdee1549427a0b01b8301e28`. The
profile test reconstructs the change and rejects any difference beyond the
Chromium namespace `clone` rule, `unshare`, and the unconditional `chroot`
allowance. The runtime profile does not add capabilities or disable the
Chromium sandbox.

## CONNECT observer contract

`connect-observer.mjs` has no startup side effects. The runtime integration
starts it through `observer-service.mjs`, which passes the compiled
`ALLOWED_HOST` to `createConnectObserver`; release-manifest data does not
configure the observer's host pin. The compiled pin is
`teagram.tailaa4918.ts.net`.

Separately, before any Docker command, `run.sh` validates the supplied release
record and fetches the served `mtproto-target.json` from the pinned HTTPS host.
It checks the source revision, content digest, fixed WSS hostname, and
fingerprint format. Those preflight checks validate release metadata; they do
not set the observer's compiled host pin.

The proxy accepts only CONNECT to that exact host on port 443 with exactly one
matching `Host` header. It resolves DNS only after those authority checks,
requires every answer to be in `100.64.0.0/10` or `fd7a:115c:a1e0::/48`, and
opens the upstream socket to a validated IP without resolving the name again.
Other CONNECT authorities are refused before DNS or socket access. Other HTTP
methods, including absolute-form GET, receive 405. Tunnel bytes pass through
without TLS inspection, and `Proxy-Authorization` is ignored.

Request headers are capped at 8 KiB. Header parsing has a two-second deadline,
the HTTP request timeout is five seconds, and DNS and upstream connection
attempts each have a two-second timeout. The observer never logs request
authorities, headers, payloads, or error details.

The separate `controlServer` listens only when started by the runtime
integration. Bind it to the private runtime network and do not publish its
port. `GET /healthz` returns a fixed snapshot with HTTP 200 when the observer
is healthy and 503 otherwise; other GET paths return 404 and non-GET methods
return 405. Health requests cannot become proxy traffic or change proxy
counters. The runtime may instead read `observer.snapshot()` in the owning
process.

The snapshot schema has exactly ten top-level fields: `status`, `allowed_host`,
`connect_attempts`, and the seven integer counters below. Every count is clamped
at 100. `connect_attempts` has exactly one key for each compiled-allowlist host
and the two buckets `telegram-suffix` and `other`. It never contains another
authority string.

- `allowed_connects`: established CONNECT tunnels.
- `connect_attempts`: CONNECT request-target authorities attempted, grouped by
  allowlisted hostname or privacy-preserving bucket. A conflicting `Host`
  header does not change the request's target bucket.
- `blocked_requests`: requests rejected by policy or failed before a tunnel
  was established.
- `telegram_attempts`: blocked authorities ending in the `telegram.org`,
  `t.me`, `telegram.me`, or `telesco.pe` DNS suffixes.
- `other_blocked_count`: blocked requests outside those official suffixes.
- `dns_lookups`: lookups attempted for the pinned host after authority checks.
- `upstream_connects`: upstream socket attempts after address validation.
- `upstream_failures`: DNS, address-policy, or upstream connection failures.

`status: healthy` means the proxy listener is active and neither listener has
reported an internal server error; it does not establish readiness. Consumers
must call `isReadySnapshot(snapshot)`, which rejects missing or extra fields,
wrong types, an unhealthy status, blocked requests, and upstream failures. It
also rejects unknown attempt buckets and counts above the clamp, requires zero
attempts in both non-allowlist buckets, at least one established allowed tunnel,
and consistent attempted, DNS, upstream, and successful-tunnel counts. This
check does not cover the browser's HTTP or WSS readiness.

`resolveHost` and `openUpstream` are unit-test seams. Runtime code must use the
default resolver and connector and must not override them.

## Credential-free commands

Run from `/opt/teagram`. `VERIFIED_MANIFEST` is the nonsecret release
record supplied by the reviewed deployment. It must have exactly these fields:

```json
{
  "sourceCommit": "<40 lowercase hex characters>",
  "archiveSha256": "sha256:<64 lowercase hex characters>",
  "contentDigest": "sha256:<64 lowercase hex characters>",
  "url": "https://teagram.tailaa4918.ts.net/<path>?v=<sourceCommit>"
}
```

The wrapper checks this schema, both digest formats, the source revision, and
the fixed HTTPS hostname. Before any Docker command, it fetches the served
`mtproto-target.json` from that pinned host and checks its source revision,
content digest, fixed WSS hostname, and fingerprint format. The browser then
fetches the manifest through Chromium and rechecks its source, artifact digest,
endpoint hostname, and fingerprint format before reporting ready. `run.sh`
closes stdin automatically for readiness, blocked-control and
acceptance-preflight; acceptance keeps
stdin attached to the separately approved host-local credential pipe. Host
preconditions pass before the QA process receives that pipe.

```sh
cd /opt/teagram
./deploy/browser-acceptance/run.sh readiness --manifest "$VERIFIED_MANIFEST" </dev/null
./deploy/browser-acceptance/run.sh blocked-control --manifest "$VERIFIED_MANIFEST" </dev/null
```

Readiness first runs the blocked-host control, verifies that the denied request
caused no DNS lookup or upstream connection and that direct browser-container
TCP to the target Tailnet address on port 443 fails. It then restarts the
observer to reset its counters and opens a fresh sandboxed browser context for
the positive origin and WSS checks. The readiness command prints one fixed
metadata JSON line only when both phases pass. The separate blocked-control
command prints:

```json
{"status":"blocked_expected","blocked_telegram_org_attempts":1,"telegram_org_upstream_connects":0,"payloads_retained":0}
```

Readiness prints:

```json
{"status":"ready","http_status":200,"asset_502_count":0,"browser_wss_status":101,"browser_wss_unique_targets":1,"observer_success_hosts":1,"observer_target_host":"teagram.tailaa4918.ts.net","telegram_org_attempts":0,"payloads_retained":0}
```

The wrapper rejects extra or malformed output and maps failures to
`{"status":"error","code":"<fixed-enum>"}`. Readiness-mode
`websocket-not-ready` errors may use the fixed ordered diagnostic form below;
all other modes and errors keep the two-field form:

```json
{"status":"error","code":"websocket-not-ready","wss_diagnostic":"handshake-not-101","wss_targets":1,"wss_handshakes":1,"wss_target_match":true,"wss_status":503}
```

The diagnostic enum is `ambiguous`, `no-target`, `target-mismatch`,
`no-handshake`, or `handshake-not-101`. Counts are clamped to `0`, `1`, or
`2` (two or more). Target comparison is a boolean against the validated full
endpoint; observed URL text is discarded. The observation starts at app
`DOMContentLoaded`, waits at least one second, then stops when all observed
connections have one handshake response or the observations are ambiguous,
with a ten-second total limit. Readiness uses only the created and handshake
response events; it does not infer a rejection status from server output.
Those events come from the page and from every worker it starts: shared and
service workers are auto-attached at the browser target, dedicated workers at
the page target, and the workers any worker starts, at any depth, before that
worker runs. Dedicated workers are held at start until their Network domain is
enabled. Playwright resumes shared and service workers itself, so a worker
whose script finished loading before its Network domain was enabled is
unobservable. A worker's descendants are watched before its own session is
attached, so its script only counts as loaded after both, and children it
already started are reported when the watch begins. A worker stays pending
until its hold is released and its script has loaded. A worker that cannot be attached and enabled, or is
still attaching when the window closes, makes the observation `ambiguous`.
The QA runner's WSS proof observes the same targets. It accepts one to four
connections, every one matching the endpoint with exactly one 101 handshake
response. Four is the web client's ceiling: its MTProto SharedWorker keeps four
account slots, and on an unauthenticated load each opens one client socket to
its base DC. A fifth connection fails the proof. `run.sh` and the progress
helper use the same ceiling. The host probe's CONNECT window caps allowed tunnels
only at the counter clamp of 100.

The wrapper suppresses Compose and container stderr. The one-shot lock
serializes this fixed project; preflight requires at least 2 GiB free under
Docker storage. An exit trap removes only this project's containers, network,
and local image with `down --rmi local`. It never prunes shared images or
volumes.

## Compose and image verification

Inspect the resolved Compose policy from the repository root:

```sh
docker compose --env-file /dev/null --project-directory deploy/browser-acceptance --file deploy/browser-acceptance/compose.yaml config --format json
```

`compose-policy.test.mjs` asserts the resolved service, network, mount,
logging, image pin, memory, swap, CPU, seccomp, and capability policy. On
native amd64 and arm64 builders, pass the matching pinned digest from `run.sh`
as `PLAYWRIGHT_IMAGE` and `linux/amd64` or `linux/arm64` as
`PLAYWRIGHT_PLATFORM`, then build the `browser` service. Each CI image job runs
the sandbox and egress smoke plus Chromium readiness against an ephemeral,
credential-free HTTPS/WSS origin through the CONNECT observer. The readiness
smoke covers immediate and three-second delayed success, multiple successful
connections, no target, path and query mismatch, held and destroyed handshakes,
a rejected 503, mixed observations, manifest-before-app precedence, and asset
502. It also covers a socket opened from a SharedWorker, including one started
from a blob URL as the web client does, with a matching target, a mismatched
target and no socket, a dedicated worker started from a blob URL, and a
worker started by a dedicated worker, alone and beside a valid page socket, for both readiness and the mounted QA runner's WSS proof. The rejected-503 fixture records whether the browser exposes an associated
handshake response and its exact error line for each architecture; the pinned
arm64 image must report the exact no-handshake line. Runtime and wrapper unit
fixtures cover malformed IDs and statuses, associated 503 classification,
orphans, duplicates, overflow, post-close events, output leaks, strict allowlist
validation, and cleanup override. The LXC deploy ticket uses the same wrapper on
its native arm64 platform.

The expected verification set for this directory is:

```sh
npm ci --prefix deploy/browser-acceptance --ignore-scripts --no-audit --no-fund
node --test deploy/browser-acceptance/*.test.mjs
for file in deploy/browser-acceptance/*.mjs; do node --check "$file"; done
docker compose --env-file /dev/null --project-directory deploy/browser-acceptance --file deploy/browser-acceptance/compose.yaml config --format json
```

On-LXC browser sandbox, MagicDNS/Serve reachability, direct-egress, and
attached-stdout checks belong to the reviewed deployment ticket. No staging
LXC is available for this runtime; the PR's Compose policy and synthetic tests
are the preview gate. Roll back by reverting the PR and using the normal
deployment path, then run:

```sh
docker compose --env-file /dev/null --project-directory deploy/browser-acceptance --file deploy/browser-acceptance/compose.yaml --project-name teagram-browser-acceptance down --rmi local
```

## QA adapter contract

The corrected QA-owned script is a read-only bind mount whose SHA-256 must
match the approved runner. Its fresh Chromium profile and page use the same
explicit observer proxy, strict empty bypass policy, sandbox argument checks,
and live observer controls as readiness. The adapter exports `snapshot`,
`startIndependentCapture`, `cleanupNewBinding`, `browserLaunchPolicy`, and
`verifySandbox`, following the exact typed schemas in the approved runner
instructions. Reports may contain only normalized target metadata and typed
counts; the browser must not retain frames, request/response bodies, console
messages, HAR, traces, video, screenshots, raw errors, credentials, or session
material. It also exports `createAcceptanceProgress` and
`acceptanceRpcOutcome` for producing a bounded sign-in result from client
events. The helper exposes only these phase values: `wss-not-opened`,
`wss-not-101`, `req-pq`, `req-dh-params`, `set-client-dh`, `init-connection`,
`password-challenge`, `password-check`, and `signed-in`. Its RPC outcome map
matches the `(error_code, error_message)` pair exactly: `400/PASSWORD_HASH_INVALID`
is `credential-rejected`, `401/AUTH_KEY_UNREGISTERED` is
`auth-key-unregistered`, `420/FLOOD_WAIT_<1 to 6 digits>` is `flood`, and
`500/INTERNAL` is `internal`; other pairs are `other`. Timeout and
closed-connection signals map to `timeout` and `closed`.

The result is one JSON line with exactly `schemaVersion`, `ok`, `phase`,
`outcome`, `logout`, `targetHost`, `sourceCommit`, and `wssConnections`.
Version 2 binds the compiled host and mounted source revision, and clamps the
WSS count at five. A successful sign-in requires one to four WSS connections
and waits for its own `auth.logOut` call before teardown. The pinned QA v2
script still emits this version 2 shape. Acceptance mode now validates QA v3;
until the v3 script and pin are installed, a QA v2 line is marked invalid in
the wrapper result below. This does not change the host probe code or process
exit status: the wrapper reports `status: "error"` while preserving a
successful probe's exit status of zero.

The QA v3 result retains the version 2 fields and adds `qaFailureCode`
(`SAFE_FAILURES` or `none`), `submission` (`none`, `username`, or `password`),
`chatList` (`not-reached`, `missing`, or `rendered`), `chatListRows` (integer
0–10, where 10 means ten or more), and `wssEndpointMatched` (boolean). A
rendered chat list includes the visible empty state, which reports zero rows.
A v3 success requires `qaFailureCode: "none"`, a password submission, a
rendered chat list, a matching WSS endpoint, and the existing successful
sign-in and logout proof.
Failed QA results keep a non-`none` failure code and their bounded diagnostics.
The wrapper accepts the exact v3 schema only: a non-success result must have a
non-`none` failure code, and `password-check` or `signed-in` requires a
password submission. Any schema, enum, range, or consistency failure makes
the QA result invalid.

The adapter's `snapshot`, `startIndependentCapture`, and `cleanupNewBinding`
remain unprivileged and fail closed. Protected checks live in `host-probe.mjs`,
which is neither copied into the image nor mounted into either container. The
QA script cannot invoke it or provide its inputs. An approved QA script must
use the bounded result contract and log out its own session. The pinned runner
is the QA-owned `private-browser-acceptance-v2.mjs`, tracked byte-identical in
this directory with its credential-free fixture. It never calls the three
stubs, and its `--mode preflight` runs every pre-stdin check without reading
stdin. It prints a fixed readiness line on success and a one-line JSON error
with a code from its fixed failure allow-list on failure.

The credential-bearing invocation remains a host-local provider pipe. The
wrapper forwards it through `docker exec --interactive` only after the host
checks pass; account credentials never enter environment variables, argv,
files, SSH, or logs:

```sh
./deploy/browser-acceptance/run.sh acceptance --manifest "$VERIFIED_MANIFEST" --script "$APPROVED_QA_SCRIPT" < <(host-local-credential-provider)
```

Before reading the account credential, prove readiness through the same
entrypoint with stdin closed:

```sh
./deploy/browser-acceptance/run.sh acceptance-preflight --manifest "$VERIFIED_MANIFEST" --script "$APPROVED_QA_SCRIPT" </dev/null
```

It runs the acceptance probe path in order: lock, both manifests, the QA pin,
server provenance, consistent registration mode and disabled login-code
logging, topology, fingerprint, probe-credential file checks, one complete
binding cut and the account sequence high-water read, the host observer read,
QA `--mode preflight` through a non-interactive `docker exec` without stdin,
and the post-cut stability and network-window checks. This credential-free
preflight reads the account sequence high-water once, takes no second binding
cut, and never forwards a credential. The protected acceptance run reads the
high-water again after its launch. Success prints exactly:

```json
{"status":"acceptance-ready","web_source_commit":"<40 hex>","server_source_commit":"<40 hex>","registration_mode":"open","login_codes_disabled":true,"server_revision_proven":true,"runtime_stable":true,"network_isolated":true,"fingerprint_matched":true,"binding_evidence_complete":true,"account_evidence_complete":true,"qa_preflight_ready":true}
```

Failures use the probe's fixed codes, `browser-failed` for a failed QA
preflight, or the wrapper's fixed errors. `database-unavailable` also includes
`database_failure_category`, one of `config`, `connect`, `auth`, `query`,
`evidence-invalid`, or `unknown`. A failed QA preflight also includes
`qa_failure_code`; missing or unrecognized child codes become `unknown`. If a
post-run host check also fails, its code remains top-level and the QA code is
retained. Wrapper cleanup failure replaces the top-level code with
`cleanup-failed` and retains the QA code. Preflight leaves no state: acceptance
reruns every check and a previous ready line authorizes nothing.

`run.sh` accepts only the runner's pinned SHA-256 and mounts the manifest and QA
script read-only. The script lands at `/opt/browser-acceptance/approved-qa.mjs`,
inside the image's package tree, so its `playwright` import resolves to the
pinned install. The probe starts an idle browser container before checking
the internal network, then starts the QA process without a TTY. Its stdout
goes directly to the wrapper on a separate descriptor, never through the
probe; a fixed start marker distinguishes a child failure from checks that
stop before the child runs. The wrapper validates browser and probe output
independently, rejects extra lines, duplicate keys and unknown fields, and
requires both results for success. Its exit trap tears down this project's
containers and local image.

## Host probe

The host needs Node 24 or newer, `psql`, and the wrapper's existing tools.
Provision these two host-local files separately, owned by the invoking UID,
with mode exactly 0600, one hard link, and no final-component symlink:

- `/etc/teagram-browser-acceptance/probe.pgpass`: one
  [Postgres password-file entry](https://www.postgresql.org/docs/current/libpq-pgpass.html),
  `host:port:database:browser_acceptance_probe:password`. Escape colons and
  backslashes with backslashes. Wildcards, additional entries and other roles
  are rejected. The host may be loopback, an RFC1918 address on the LXC's
  private Docker network, or an absolute Unix socket directory.
- `/etc/teagram-browser-acceptance/probe-config.json`: exactly
  `{"userId":"<positive decimal bigint>"}` for the existing test account.

Acceptance-only options `--probe-credential` and `--probe-config` override
these paths. Neither file is mounted into a container. The password file is
opened without following a symlink, checked, and held open for the run. `psql`
receives that checked descriptor as its password file, with no password in
argv or environment and no server DSN fallback. The account ID goes through
the host-private query pipe. The probe calls only
`public.browser_acceptance_binding_snapshot_v1(bigint)` and
`public.browser_acceptance_account_high_water_v1()`, with read-only
transactions and a statement timeout. It has only `EXECUTE` on those functions,
with no table, sequence, enrollment, trigger-function or write privileges. The
probe never writes to the database or revokes a key.

Before credential forwarding, every running teagramd process must have
`TG_REGISTRATION=closed` or `TG_REGISTRATION=open`, with the same setting on
every replica, and `TG_LOG_LOGIN_CODES=false`. Docker's projection returns only
those two effective flags and
`TG_RSA_KEY_FINGERPRINT`; no `.env` or full environment is read. Server source
provenance comes only from the rollout runner's `/root/*.build/built-image.identity`
records. Production has no evidence-root override. Candidate directories are
root-owned mode 0700; candidate records are root-owned regular files with mode
0600, one hard link, no-follow opening, and a 1 KiB limit. Each record must be
the runner's exact one-line `captured_at_utc`, `source_sha`, `image_id`, and
`revision_label_json` format in that order, followed by exactly one LF. A
malformed candidate fails closed.
For every running replica, at least one record must match its immutable image
ID. All selected records must agree, and their source SHA must equal the
recorded revision label and the inspected image and container revision labels.
All replicas must share that source. The web release and server source remain
separate identities; their only join is the target fingerprint. Each
`TG_RSA_KEY_FINGERPRINT` must be the canonical signed decimal that teagramd
parses for the same 64 bits as the served hex fingerprint. The image is
inspected by its immutable image ID. The probe requires
`teagram-browser-acceptance-internal` to be internal and contain exactly the
browser and observer, and independently fetches the served manifest over
verified HTTPS without a proxy or redirects to compare fingerprints.

The supported evidence API is pinned to version 1. It provides one atomic cut
per call: completeness and version, enrollment epoch, cumulative additions and
removals, the trigger-recorded creation-time witness, the bound key IDs and
their original creation times, pending count, and DB cut time. Before
forwarding browser credentials, the probe requires a complete supported cut
and retains it only in memory. Once the browser exits, it repeats the same
single-function read after rechecking container IDs, image IDs, start times,
running state, complete container and image labels, effective flags, the
runner-record resolution, and observer identity and start time. It also
re-reads the web release record and served manifest. Any change yields
`runtime-changed` without a retry.

The probe also reads the observer counters itself at both cuts. It runs the
fixed in-image `observer-snapshot.mjs` through `docker exec` on the observer
container without stdin, caps its stdout at 16 KiB and validates the snapshot
with `isValidSnapshot` on the host; the browser cannot supply or redirect this
evidence. The pre-cut read happens after the first binding cut, just before the
QA process starts, and the post-cut
read happens before the observer identity recheck, so a restart stays
`runtime-changed`. Across the window, `connect_attempts.other` and
`other_blocked_count` must change by 0; `telegram_attempts`,
`connect_attempts.telegram-suffix` and `blocked_requests` by exactly 1 (the
single `browserLaunchPolicy` control); `upstream_failures` by 0; and
`allowed_connects` by at least 1. No counter may decrease and every post-cut
counter must be below the clamp of 100. A missing, invalid, unhealthy or
oversized snapshot, a regression or any other delta yields `network-unproven`
without a retry. Snapshots never leave probe memory.

Pass requires both complete cuts at version 1 and the same epoch, exact
addition/removal deltas of one each, a monotonic final DB cut, and a non-null
witness within the inclusive interval from the first DB cut through the final
DB cut. Bound key IDs and creation times must be identical, and pending count
must be no higher than baseline. Thus one newly created key can bind and log
out entirely between cuts and still pass; two such keys fail from deltas
`(2, 2)` even when both visible key sets equal B. Restoring or reassigning a
baseline key fails through its old creation-time witness. Missing trigger
coverage, malformed or unsupported evidence, an epoch change, a clock
straddle, or an excess pending count yields `cleanup-unproven`; there is no
retry or host revocation.
Counters, witness, epoch and DB cut time never leave probe memory.

Probe stdout is one flat JSON line with exactly `schemaVersion` (4),
`serverSourceCommit`, `ok`, `code`, `registrationMode`, `accountEvidence`,
`accountsUnchanged`,
`loginCodesDisabled`, `revisionMatched`,
`containersStable`, `networkIsolated`, `fingerprintMatched`,
`newBindingProven`, `cleanupProven`, `browserExitOk`, `boundBefore`,
`boundAfter`, `pendingBefore`, and `pendingAfter`, plus these v3 fields:
`bindingEvidence` is `not-reached`, `unavailable`, `epoch-changed`, or
`complete`. `unavailable` means the final read failed, was incomplete, or had
a mismatched evidence version; `epoch-changed` reports a changed epoch.
`bindingAdditions` and `bindingRemovals` are integers 0–3, with 3 meaning
three or more. `witnessInWindow` and `baselineIntact` are booleans,
`topologyIsolated` (boolean), `captureWindow` (`not-reached`, `invalid`, or
`complete`), and the integer deltas `captureAllowedConnects`,
`captureTelegramSuffix`, `captureOther`, `captureBlocked`, and
`captureUpstreamFailures` (each 0–99). They report deltas for `allowed_connects`,
`connect_attempts.telegram-suffix`, `connect_attempts.other`,
`blocked_requests`, and `upstream_failures`. An invalid capture window means a
counter read failed, decreased, or hit its clamp. Fields for unreached steps
retain their fixed defaults. Unless `bindingEvidence` is `complete`, binding
deltas are zero and `witnessInWindow` and `baselineIntact` are false. Unless
`captureWindow` is `complete`, every capture delta is zero. `registrationMode`
is `unknown`, `closed`, `open`, or `changed`: `unknown` means the pre-cut was
unreached or rejected, and `changed` means the post-cut did not match. Every
replica must report the same exact `closed` or `open` value at each cut.
`accountEvidence` is `not-reached`, `unavailable`, or `complete`;
`accountsUnchanged` is false unless the pre and post sequence reads are
complete, and is true only when their high-water values match. Preflight makes
one high-water read, reports complete account evidence, and leaves
`accountsUnchanged` false because it has no second value to compare. The raw
high-water values never leave probe memory. `loginCodesDisabled` and
`fingerprintMatched` reflect both pre-cut and post-cut values;
`containersStable` covers runtime identity, and `networkIsolated` retains its
v2 capture-window meaning while `topologyIsolated` records the network topology
checks. `serverSourceCommit` is the proven 40-hex server revision or
an empty string when provenance is unproven. Raw binding additions and
removals, DB time, key IDs and creation times, raw pending counts above the
clamp, container/image/observer IDs, `StartedAt`, epoch, witness, and absolute
observer counters stay in memory. Evidence fields are booleans; bound and
pending counts clamp at 100.

Code values are `ok`,
`arguments-invalid`, `runtime-unavailable`, `environment-unproven`,
`revision-unproven`, `runtime-changed`, `network-unproven`,
`fingerprint-unproven`, `database-unavailable`, `cleanup-unproven`,
`browser-failed`, `binding-not-created`, `account-created`, and
`account-unproven`. The account codes take precedence over other post-launch
codes: a higher post high-water reports `account-created`; a failed or backward
post read reports `account-unproven`. Both fail with a non-zero exit. The
`binding-not-created` code is emitted only when
the final binding evidence is complete, additions and removals are both zero,
the baseline is intact, pending count did not rise, and the browser exited
unsuccessfully. Every other post-launch failure without proven cleanup remains
`cleanup-unproven`. Failures exit non-zero. No account IDs, key IDs, passwords,
fingerprints or raw Docker, DB, server or browser errors are emitted or saved.

Once the wrapper validates a v5 probe line in acceptance mode, it prints one
flat JSON line with exactly these keys: `evidenceVersion` (2), `status`,
`code`, `teardown`, `targetHost`, `sourceCommit`, `serverSourceCommit`,
`registrationMode`, `accountEvidence`, `accountsUnchanged`,
`loginCodesDisabled`, `revisionMatched`,
`containersStable`, `networkIsolated`, `fingerprintMatched`,
`newBindingProven`, `cleanupProven`, `browserExitOk`, `boundBefore`,
`boundAfter`, `pendingBefore`, `pendingAfter`, `bindingEvidence`,
`bindingAdditions`, `bindingRemovals`, `witnessInWindow`, `baselineIntact`,
`topologyIsolated`, `captureWindow`, `captureAllowedConnects`,
`captureTelegramSuffix`, `captureOther`, `captureBlocked`,
`captureUpstreamFailures`, `qaResult`, `phase`, `outcome`, `logout`,
`wssConnections`, `wssEndpointMatched`, `submission`, `chatList`,
`chatListRows`, and `qaFailureCode`. `databaseFailureCategory` is also present
only when `code` is `database-unavailable`. `sourceCommit` comes from the manifest;
all probe fields except `schemaVersion`, `ok`, and `code` are retained. Invalid
QA fields use `unknown`, zero, or false defaults; malformed, absent, and v2 QA
lines cannot erase probe evidence. QA v2 is invalid under this contract.

`status: "accepted"` requires probe code `ok`, unchanged account evidence, a
valid QA v3 result with `ok: true`, probe exit status zero, and
`teardown: "ok"`. Otherwise the record
has `status: "error"`. QA diagnostics do not change the probe `code` or
process exit status: a successful probe with invalid or failed QA therefore
has `status: "error"` and exits zero. A compose teardown failure sets
`teardown: "failed"`, changes status to error, preserves the probe code and
evidence, and exits non-zero. Code-only output remains for `runtime-failed`
when no valid probe line exists and for failures before the probe runs.
Readiness output is unchanged, and wrapper stderr remains suppressed. The
preflight line reports the proven `registration_mode` and
`account_evidence_complete`; database failures also report their fixed
`database_failure_category`.
