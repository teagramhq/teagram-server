# Real server fixture

`run.sh` starts a disposable production `telegramd`, a freshly migrated PostgreSQL database, and a local TLS front for `https://telegramd.test`. Each run has a fresh 128-bit identity, RSA key, auth-key encryption key, and two ordinary synthetic accounts. The accounts authenticate through the matching `wss://telegramd.test/apiws` endpoint and then the real browser, worker, and egress checks run before readiness is returned.

Run from the server checkout with a full server revision matching `HEAD`, the accepted web revision, and a fresh lowercase 32-character hex run ID:

```sh
bash test/e2e/real_server_fixture/run.sh \
  --server-revision "$(git rev-parse HEAD)" \
  --web-revision 84961bf77003a1bdb582d1096d988f1d304e3d1f \
  --run-id "$(openssl rand -hex 16)"
```

Standard output contains one JSON readiness record only after every check passes. It includes the test origin, WSS endpoint, public MTProto key and fingerprint, TLS leaf SPKI, protected password-file references, and a cleanup handle. Passwords and private keys are never printed. Keep the command in the foreground while using the fixture; close stdin or send `SIGTERM` to stop it and remove only resources carrying that run's ownership label. Any startup, target, verification, or cleanup error returns nonzero.

The focused integration gate is:

```sh
bash .github/scripts/run-real-server-fixture-gate.sh
```

The gate validates Go's JSON test stream and fails if any required fixture case is missing,
skipped, or not passing. CI applies the same check to its full e2e result stream.
