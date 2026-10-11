# Real server fixture

The fixture starts the requested immutable `telegramd` revision from an owned detached server worktree, applies that revision's migrations to a fresh PostgreSQL database, and exposes the service only at `https://telegramd.test` and `wss://telegramd.test/apiws`. Each run has a fresh identity, RSA key, auth-key encryption key, and two synthetic accounts. The fixture authenticates both accounts and completes its existing browser, worker, and egress checks before reporting `server-ready`. Harness, server, and web revisions are reported separately as full verified SHAs; the requested server revision must be resolvable in the checkout and resolves to a detached worktree owned by the run.

The browser image is selected from the Docker daemon architecture using the corresponding digest pinned in the CI workflow. Unsupported architectures fail before the fixture creates resources.

Start it from the server checkout with full lowercase SHAs and a fresh run ID:

```sh
RUN_ID="$(openssl rand -hex 16)"
bash test/e2e/real_server_fixture/run.sh \
  --server-revision "$(git rev-parse HEAD)" \
  --web-revision 16f12b9f4e0a42b20c3fe3aa340b6b8f8b2e8861 \
  --run-id "$RUN_ID"
```

Keep this command in the foreground. Its first JSON line is `server-ready`, emitted only after the authentication and egress checks pass. It includes this run's public build inputs, the TLS leaf SPKI pin for browser trust, the harness/server/web revisions, the existing fixture security evidence, and each synthetic username with the absolute path to its protected mode-0400 password file. It never includes password contents or private-key material.

## Fixture-only auth progress

The opt-in `--auth-progress` mode is for the pinned auth-progress fixture contract only. It requires web source `569529c1f36923c098760ef727808254dcb6a662` and a new absolute output path. The fixture binds the full digest over sorted relative paths and file contents, including every byte of `mtproto-target.json`, into the run identity and verifies that frozen tree before and after each parity leg. It separately audits the manifest's producer digest, which excludes the manifest itself, and supplies that digest to the pinned browser runner when it checks the manifest. The fresh target key makes both digests experiment-specific. The historical digest `sha256:f352e4ef6163855cdbbcc4022386a306b6c768f56e1fa2aa1e35f434ba272f3f` from MAIN-1660/MAIN-1315 is a historical reference only, not a digest attested for a current experiment.

```sh
bash test/e2e/real_server_fixture/run.sh \
  --server-revision "$(git rev-parse HEAD)" \
  --web-revision 569529c1f36923c098760ef727808254dcb6a662 \
  --run-id "$RUN_ID" \
  --auth-progress \
  --auth-progress-output "$(pwd)/auth-progress.json"
```

The runner builds the requested clean server worktree with `fixtureauthprogress` and passes its confined `/run/log` path and immutable source references only to that server process. Ordinary runs and the production Dockerfiles keep using the untagged build. The fixture records the server and front-counter baselines after `artifact-ready`, records `runner_exit` after browser contexts close, requests bounded server shutdown, then copies the versioned report while owned-resource cleanup is still in progress. The report file can remain if later cleanup fails, so its presence alone does not establish a valid observation. The consumer accepts validity only after owned cleanup succeeds for both parity legs and the shared key is removed; partial cleanup is `unavailable_teardown_failure`. The report carries fixed method/result enums, bounded counters, front-counter source, and full run/source references; it carries no RPC payloads or raw errors. The fixture rejects a different artifact source or digest.

`--auth-controls` runs the same fixed positive and closed-signup checks against the ordinary untagged build for the CI parity case. It does not enable server instrumentation.

## Paired browser auth-progress observation

`TestRealServerFixtureAuthProgressBrowserObservation` owns a tagged and an untagged fixture leg for one experiment. It generates one target key, builds one artifact from web source `569529c1f36923c098760ef727808254dcb6a662`, audits and binds the same full digest in both legs, separately validates the manifest digest supplied to the browser runner, settles the positive A/B and closed-signup controls before the tagged baseline, and invokes the reviewed one-submission runner once per leg. It reports only validated stages, counts, control outcomes, and bound source references after fixture, credential, artifact, readiness-file, and shared-key cleanup.

The entrypoint is opt-in so the regular CI fixture lane does not need a local runner checkout. To run it, have `rtk`, `pnpm`, Docker, and Go available, and use a clean `teagram-web` checkout at runner revision `877aeffca544728d415946d8e7ccb4aeae12df28`. The test checks both the exact HEAD and clean worktree; setting the environment variable makes an invalid checkout or missing runner dependency fail instead of skip.

The harness passes `pnpm --silent` before the pinned runner script so pnpm status text cannot contaminate its strict JSON report on stdout. The runner script and one-submission arguments remain the accepted command contract.

From the `teagram-server` checkout, run:

```sh
TEAGRAM_FIXTURE_AUTH_PROGRESS_RUNNER_CHECKOUT=/absolute/path/to/teagram-web \
  go test ./test/e2e -run '^TestRealServerFixtureAuthProgressBrowserObservation$' -count=1 -v
```

The broader focused auth-progress command also selects this entrypoint. Without the environment variable, it is the only fixture case allowed to skip; the CI JSON gate rejects that skip when the variable is configured:

```sh
go test ./test/e2e -run '^TestRealServerFixtureAuthProgress' -count=1
```

Build the matching production bundle for that run from a clean web checkout at the reported `webRevision`, using the run's own public inputs. Copy the `mtprotoPublicKeyPEM` value from the readiness JSON to a temporary public-key file outside the output directory, then run:

```sh
cd ../teagram-web
test "$(git rev-parse HEAD)" = 16f12b9f4e0a42b20c3fe3aa340b6b8f8b2e8861
test -z "$(git status --porcelain --untracked-files=all)"
ARTIFACT_DIR="$(mktemp -d)/dist-private"
KEY_FILE="$(mktemp)"
chmod 600 "$KEY_FILE"
trap 'rm -f -- "$KEY_FILE"' EXIT
# Write the mtprotoPublicKeyPEM value from server-ready to "$KEY_FILE".
MTPROTO_TARGET_MODE=private \
MTPROTO_PRIVATE_ENDPOINT=wss://telegramd.test/apiws \
MTPROTO_PRIVATE_RSA_PUBLIC_KEY_FILE="$KEY_FILE" \
  corepack pnpm exec vite build --outDir "$ARTIFACT_DIR"
MTPROTO_TARGET_MODE=private node scripts/check-bundle-mangling.mjs "$ARTIFACT_DIR"
```

The build creates `$ARTIFACT_DIR/mtproto-target.json` with the private mode, endpoint, fingerprint, source commit, and the producer's digest report. That producer-defined digest excludes the manifest itself, so the fixture independently recomputes it and supplies it to the runner's manifest check. The fixture also computes the per-experiment full digest over the frozen file set, including every manifest byte; `artifactDigest`, `frozenArtifactDigest`, and the before/after leg measurements bind and verify that full digest. The fixture requires `sourceCommit` to equal the requested web revision. Return to the fixture terminal and enter exactly one attachment command using the absolute output path:

```text
attach /absolute/path/to/dist-private
```

The fixture copies the caller-owned bundle into a private immutable stage, audits every staged file independently, then serves only the staged URL map from a restricted tmpfs inside the TLS front. The audit never reads its rules from the artifact or from the web tree: it pins the trusted Telegram RSA fingerprints and moduli, the official datacenter host and route forms including dynamically assembled ones, the official IPv4 ranges and IPv6 DC prefixes, and the run's exact WSS endpoint. Cleartext `ws://`, any other `wss://`, private-key blocks, production identifiers, and every run secret are unconditional rejections. Ordinary HTTPS product references, such as `https://web.telegram.org/a/`, `https://telegram.org/android`, and `https://t.me/botfather`, are permitted bundle content: they are recorded in the audit evidence with their staged-file location and never become allowed destinations. Any page, SharedWorker, or ServiceWorker attempt to a host other than this run's endpoint fails the run, including attempts the CSP blocks.

The fixture never strips links, drops files, or substitutes a revision. It emits `artifact-ready` only after a fresh Chromium process and context verify the entry and manifest hashes, the private CSP on every artifact response, script/asset and worker loads, the bundle's own service-worker controller, and zero unexpected attempts. This is a bundle bridge check; it does not assert login or SRP regression coverage.

Close the fixture by sending EOF or `stop` on stdin, or by sending `SIGTERM` to the foreground process. Cleanup removes only resources carrying the run's ownership label. Startup, attachment, audit, browser, or cleanup failures return nonzero, have no retry, and still run owned cleanup. The caller's artifact is never modified or removed.

Run the focused integration gate with:

```sh
bash .github/scripts/run-real-server-fixture-gate.sh
```

It runs the artifact boundary tests and focused Go fixture cases, then checks Go's JSON test stream for every required case. CI checks the same fixture cases in its end-to-end result stream.
