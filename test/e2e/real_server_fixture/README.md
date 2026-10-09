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

The build creates `$ARTIFACT_DIR/mtproto-target.json` with the private mode, endpoint, fingerprint, source commit, and completed artifact digest. The fixture requires that `sourceCommit` to equal the requested web revision and recomputes the digest itself. Return to the fixture terminal and enter exactly one attachment command using the absolute output path:

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
