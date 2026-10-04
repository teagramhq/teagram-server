import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import net from "node:net";
import test from "node:test";

const observerModule = await import("./connect-observer.mjs").catch((error) => {
  if (error.code === "ERR_MODULE_NOT_FOUND") {
    return null;
  }
  throw error;
});

function api(t) {
  assert.ok(observerModule, "the credential-free CONNECT observer module must exist");
  return observerModule;
}

async function startObserver(t, overrides = {}) {
  const observerApi = api(t);
  const observer = observerApi.createConnectObserver({
    manifestHost: observerApi.ALLOWED_HOST,
    ...overrides,
  });
  await new Promise((resolve, reject) => {
    observer.server.once("error", reject);
    observer.server.listen(0, "127.0.0.1", resolve);
  });
  t.after(async () => {
    await new Promise((resolve) => observer.server.close(resolve));
  });
  return {
    observer,
    port: observer.server.address().port,
  };
}

function exchange(port, request) {
  return new Promise((resolve, reject) => {
    const socket = net.connect(port, "127.0.0.1");
    const chunks = [];
    socket.on("connect", () => socket.write(request));
    socket.on("data", (chunk) => chunks.push(chunk));
    socket.on("end", () => resolve(Buffer.concat(chunks).toString("latin1")));
    socket.on("error", reject);
    socket.setTimeout(5000, () => socket.destroy(new Error("observer response timed out")));
  });
}

function connectRequest(authority, headers = "", hostAuthority = authority) {
  return `CONNECT ${authority} HTTP/1.1\r\nHost: ${hostAuthority}\r\n${headers}\r\n`;
}

function responseStatus(response) {
  return Number(response.match(/^HTTP\/1\.1 (\d{3})/u)?.[1]);
}

test("manifest host is checked against the single compiled allowlist", (t) => {
  const observerApi = api(t);
  assert.throws(
    () => observerApi.createConnectObserver({ manifestHost: "attacker.example" }),
    (error) => error.code === "ERR_OBSERVER_MANIFEST_MISMATCH" && !error.message.includes("attacker.example"),
  );
  assert.throws(
    () => observerApi.createConnectObserver({ manifestHost: undefined }),
    (error) => error.code === "ERR_OBSERVER_MANIFEST_MISMATCH",
  );
  assert.throws(
    () => observerApi.createConnectObserver(null),
    (error) => error.code === "ERR_OBSERVER_CONFIG",
  );
});

test("CONNECT to the pinned host resolves and connects only to a validated Tailscale address", async (t) => {
  const observerApi = api(t);
  let dnsLookups = 0;
  let upstreamAddress;
  let upstreamConnections = 0;
  const upstreamServer = net.createServer((socket) => {
    socket.once("data", (payload) => {
      assert.equal(payload.toString(), "synthetic-tunnel-payload");
      socket.end("synthetic-upstream-response");
    });
  });
  await new Promise((resolve) => upstreamServer.listen(0, "127.0.0.1", resolve));
  t.after(async () => {
    await new Promise((resolve) => upstreamServer.close(resolve));
  });

  const { observer, port } = await startObserver(t, {
    resolveHost: async (host) => {
      dnsLookups += 1;
      assert.equal(host, observerApi.ALLOWED_HOST);
      return [{ address: "100.64.1.2", family: 4 }];
    },
    openUpstream: (address) => {
      upstreamAddress = address;
      upstreamConnections += 1;
      return net.connect(upstreamServer.address().port, "127.0.0.1");
    },
  });
  const response = await exchange(
    port,
    connectRequest(
      `${observerApi.ALLOWED_HOST.toUpperCase()}:443`,
      "Proxy-Authorization: Bearer MUST_NOT_BE_RETAINED\r\n",
    ) + "synthetic-tunnel-payload",
  );

  assert.match(response, /^HTTP\/1\.1 200 Connection Established\r\n/u);
  assert.ok(response.includes("synthetic-upstream-response"));
  assert.equal(dnsLookups, 1);
  assert.equal(upstreamConnections, 1);
  assert.equal(upstreamAddress, "100.64.1.2");
  assert.equal(response.includes("MUST_NOT_BE_RETAINED"), false);
  assert.deepEqual(observer.snapshot(), {
    status: "healthy",
    allowed_host: observerApi.ALLOWED_HOST,
    allowed_connects: 1,
    blocked_requests: 0,
    telegram_attempts: 0,
    other_blocked_count: 0,
    dns_lookups: 1,
    upstream_connects: 1,
    upstream_failures: 0,
  });
});

test("CONNECT rejects mixed DNS answers before opening any upstream connection", async (t) => {
  const observerApi = api(t);
  let upstreamConnections = 0;
  const { observer, port } = await startObserver(t, {
    resolveHost: async (host) => {
      assert.equal(host, observerApi.ALLOWED_HOST);
      return [
        { address: "100.64.1.2", family: 4 },
        { address: "192.0.2.10", family: 4 },
      ];
    },
    openUpstream: () => {
      upstreamConnections += 1;
      return assert.fail("mixed DNS answers must not connect upstream");
    },
  });

  const response = await exchange(port, connectRequest(`${observerApi.ALLOWED_HOST}:443`));
  const snapshot = observer.snapshot();

  assert.equal(responseStatus(response), 502);
  assert.equal(upstreamConnections, 0);
  assert.equal(snapshot.upstream_connects, 0);
  assert.equal(snapshot.blocked_requests, 1);
  assert.equal(snapshot.upstream_failures, 1);
});

test("the official Telegram domain deny case is counted once without DNS or upstream access", async (t) => {
  const { observer, port } = await startObserver(t, {
    resolveHost: async () => assert.fail("denied authority must not trigger DNS"),
    openUpstream: () => assert.fail("denied authority must not connect upstream"),
  });
  const response = await exchange(port, connectRequest("telegram.org:443"));
  const snapshot = observer.snapshot();

  assert.equal(responseStatus(response), 403);
  assert.equal(snapshot.blocked_requests, 1);
  assert.equal(snapshot.telegram_attempts, 1);
  assert.equal(snapshot.other_blocked_count, 0);
  assert.equal(snapshot.dns_lookups, 0);
  assert.equal(snapshot.upstream_connects, 0);
  assert.equal(JSON.stringify(snapshot).includes("telegram.org"), false);
});

test("a mismatched or duplicate Host authority is denied before DNS", async (t) => {
  const observerApi = api(t);
  let dnsLookups = 0;
  let upstreamConnections = 0;
  const { observer, port } = await startObserver(t, {
    resolveHost: async () => {
      dnsLookups += 1;
      return [{ address: "100.64.1.2", family: 4 }];
    },
    openUpstream: () => {
      upstreamConnections += 1;
      return assert.fail("conflicting Host authority must not connect upstream");
    },
  });

  const mismatch = await exchange(
    port,
    connectRequest(`${observerApi.ALLOWED_HOST}:443`, "", "telegram.org:443"),
  );
  const duplicate = await exchange(
    port,
    `CONNECT ${observerApi.ALLOWED_HOST}:443 HTTP/1.1\r\nHost: ${observerApi.ALLOWED_HOST}:443\r\nHost: telegram.org:443\r\n\r\n`,
  );

  assert.equal(responseStatus(mismatch), 403);
  assert.notEqual(responseStatus(duplicate), 200);
  assert.equal(observer.snapshot().telegram_attempts, 2);
  assert.equal(observer.snapshot().blocked_requests, 2);
  assert.equal(dnsLookups, 0);
  assert.equal(upstreamConnections, 0);
});

test("a disconnected client is not forwarded after DNS completes", async (t) => {
  const observerApi = api(t);
  let releaseLookup;
  let lookupStarted;
  const started = new Promise((resolve) => {
    lookupStarted = resolve;
  });
  let upstreamAttempts = 0;
  const { observer, port } = await startObserver(t, {
    resolveHost: () => {
      lookupStarted();
      return new Promise((resolve) => {
        releaseLookup = resolve;
      });
    },
    openUpstream: () => {
      upstreamAttempts += 1;
      return assert.fail("disconnected clients must not open an upstream socket");
    },
  });
  const serverSideClose = new Promise((resolve) => {
    observer.server.once("connection", (socket) => socket.once("close", resolve));
  });
  const client = net.connect(port, "127.0.0.1");
  client.on("error", () => {});
  const clientClosed = new Promise((resolve) => client.once("close", resolve));
  client.write(connectRequest(`${observerApi.ALLOWED_HOST}:443`));
  await started;
  client.destroy();
  await clientClosed;
  await serverSideClose;
  releaseLookup([{ address: "100.64.1.2", family: 4 }]);
  await new Promise((resolve) => setImmediate(resolve));

  assert.equal(upstreamAttempts, 0);
  assert.equal(observer.snapshot().upstream_connects, 0);
});

test("all four official Telegram suffix families are counted without exposing hosts", async (t) => {
  const { observer, port } = await startObserver(t, {
    resolveHost: async () => assert.fail("denied authority must not trigger DNS"),
    openUpstream: () => assert.fail("denied authority must not connect upstream"),
  });
  const authorities = [
    "telegram.org:443",
    "api.telegram.org:443",
    "t.me:443",
    "sub.t.me:443",
    "telegram.me:443",
    "sub.telegram.me:443",
    "telesco.pe:443",
    "sub.telesco.pe:443",
  ];
  for (const authority of authorities) {
    assert.equal(responseStatus(await exchange(port, connectRequest(authority))), 403);
  }

  const snapshot = observer.snapshot();
  assert.equal(snapshot.blocked_requests, authorities.length);
  assert.equal(snapshot.telegram_attempts, authorities.length);
  assert.equal(snapshot.other_blocked_count, 0);
  assert.equal(snapshot.dns_lookups, 0);
  assert.equal(snapshot.upstream_connects, 0);
  for (const suffix of ["telegram.org", "t.me", "telegram.me", "telesco.pe"]) {
    assert.equal(JSON.stringify(snapshot).includes(suffix), false);
  }
});

test("hostile authority forms and non-CONNECT methods fail closed", async (t) => {
  const { observer, port } = await startObserver(t, {
    resolveHost: async () => assert.fail("denied authority must not trigger DNS"),
    openUpstream: () => assert.fail("denied authority must not connect upstream"),
  });
  const authorities = [
    "127.0.0.1:443",
    "2130706433:443",
    "0177.0.0.1:443",
    "0x7f000001:443",
    "[::1]:443",
    "telegram-server.tailaa4918.ts.net.:443",
    "user@telegram-server.tailaa4918.ts.net:443",
    "%74elegram-server.tailaa4918.ts.net:443",
    "telegram-server.tailaa4918.ts.net:444",
    "telegram-server.tailaa4918.ts.net",
    "telegram-server.tailaa4918.ts.net:0443",
    "telegram-server.tailaa4918.ts.net/path:443",
    "telegraм-server.tailaa4918.ts.net:443",
    "eviltelegram.org:443",
  ];
  for (const authority of authorities) {
    const response = await exchange(port, connectRequest(authority));
    assert.notEqual(responseStatus(response), 200, authority);
  }
  const getResponse = await exchange(
    port,
    "GET http://telegram.org/private/path?token=URL_CANARY HTTP/1.1\r\nHost: telegram.org\r\nX-Canary: HEADER_CANARY\r\n\r\n",
  );

  assert.equal(responseStatus(getResponse), 405);
  const snapshot = observer.snapshot();
  assert.equal(snapshot.blocked_requests, authorities.length + 1);
  assert.equal(snapshot.telegram_attempts, 1);
  assert.equal(snapshot.other_blocked_count, authorities.length);
  assert.equal(snapshot.dns_lookups, 0);
  assert.equal(snapshot.upstream_connects, 0);
  const retainedOutput = JSON.stringify(snapshot) + getResponse;
  for (const canary of ["URL_CANARY", "HEADER_CANARY", "private/path", "telegram.org"]) {
    assert.equal(retainedOutput.includes(canary), false);
  }
});

test("headers are capped at 8 KiB and incomplete headers time out", async (t) => {
  const observerApi = api(t);
  const { observer, port } = await startObserver(t, {
    resolveHost: async () => assert.fail("oversized or incomplete headers must not trigger DNS"),
    openUpstream: () => assert.fail("oversized or incomplete headers must not connect upstream"),
  });
  assert.equal(observerApi.MAX_HEADER_BYTES, 8192);
  assert.ok(observer.server.headersTimeout > observerApi.HEADERS_TIMEOUT_MS);
  assert.ok(observer.controlServer.headersTimeout > observerApi.HEADERS_TIMEOUT_MS);
  assert.ok(observer.server.requestTimeout > 0);

  const oversized = await exchange(
    port,
    `CONNECT telegram-server.tailaa4918.ts.net:443 HTTP/1.1\r\nHost: telegram-server.tailaa4918.ts.net:443\r\nX-Pad: ${"x".repeat(9000)}\r\n\r\n`,
  );
  assert.equal(responseStatus(oversized), 431);

  const socket = net.connect(port, "127.0.0.1");
  let closedByObserver = false;
  let timeoutResponse = "";
  const closed = new Promise((resolve) => socket.once("close", () => {
    closedByObserver = true;
    resolve();
  }));
  socket.on("data", (chunk) => {
    timeoutResponse += chunk.toString("latin1");
  });
  socket.write("CONNECT telegram-server.tailaa4918.ts.net:443 HTTP/1.1\r\nHost: ");
  let timeout;
  await Promise.race([closed, new Promise((resolve) => {
    timeout = setTimeout(() => {
      socket.destroy();
      resolve();
    }, observer.server.headersTimeout + 2500);
  })]);
  clearTimeout(timeout);
  assert.equal(closedByObserver, true);
  assert.equal(responseStatus(timeoutResponse), 408);
  assert.equal(observer.snapshot().blocked_requests, 2);
  assert.equal(observer.snapshot().dns_lookups, 0);
  assert.equal(observer.snapshot().upstream_connects, 0);
});

test("late upstream errors after timeout stay out of process output", async (t) => {
  const observerApi = api(t);
  let errorListenersAtDestroy = 0;

  const { observer, port } = await startObserver(t, {
    resolveHost: async () => [{ address: "100.64.1.2", family: 4 }],
    openUpstream: () => {
      const socket = new EventEmitter();
      socket.destroy = () => {
        errorListenersAtDestroy = socket.listenerCount("error");
        if (errorListenersAtDestroy > 0) {
          queueMicrotask(() => socket.emit("error", new Error("UPSTREAM_ERROR_CANARY")));
        }
      };
      return socket;
    },
  });
  const response = await exchange(
    port,
    connectRequest(`${observerApi.ALLOWED_HOST}:443`),
  );

  assert.equal(responseStatus(response), 502);
  assert.equal(errorListenersAtDestroy, 1);
  assert.equal(observer.snapshot().upstream_failures, 1);
  assert.equal(JSON.stringify(observer.snapshot()).includes("UPSTREAM_ERROR_CANARY"), false);
});

test("only Tailscale IPv4 and IPv6 addresses pass the resolved-address boundary", (t) => {
  const observerApi = api(t);
  for (const address of ["100.64.0.0", "100.64.1.2", "100.127.255.255", "fd7a:115c:a1e0::1"]) {
    assert.equal(observerApi.isTailscaleAddress(address), true, address);
  }
  for (const address of [
    "100.63.255.255",
    "100.128.0.0",
    "10.0.0.1",
    "127.0.0.1",
    "fd7a:115c:a1df::1",
    "fd7a:115c:a1e1::1",
    "fe80::1",
    "::ffff:100.64.1.2",
    "not-an-address",
  ]) {
    assert.equal(observerApi.isTailscaleAddress(address), false, address);
  }
});

test("unhealthy, incomplete, or blocked observer counts cannot be ready", (t) => {
  const observerApi = api(t);
  const clean = {
    status: "healthy",
    allowed_host: observerApi.ALLOWED_HOST,
    allowed_connects: 1,
    blocked_requests: 0,
    telegram_attempts: 0,
    other_blocked_count: 0,
    dns_lookups: 1,
    upstream_connects: 1,
    upstream_failures: 0,
  };
  assert.equal(observerApi.isReadySnapshot(clean), true);
  assert.equal(observerApi.isReadySnapshot({
    ...clean,
    allowed_connects: 0,
    dns_lookups: 0,
    upstream_connects: 0,
  }), false);
  assert.equal(observerApi.isReadySnapshot({ ...clean, status: "unhealthy" }), false);
  assert.equal(observerApi.isReadySnapshot({ ...clean, blocked_requests: 1 }), false);
  assert.equal(observerApi.isReadySnapshot({ ...clean, telegram_attempts: 1 }), false);
  assert.equal(observerApi.isReadySnapshot({ ...clean, other_blocked_count: 1 }), false);
  assert.equal(observerApi.isReadySnapshot({ ...clean, upstream_failures: 1 }), false);
  const missing = { ...clean };
  delete missing.upstream_connects;
  assert.equal(observerApi.isReadySnapshot(missing), false);
  assert.equal(observerApi.isReadySnapshot({ ...clean, dns_lookups: "1" }), false);
});

test("health counts use a fixed separate endpoint that does not touch proxy counters", async (t) => {
  const { observer } = await startObserver(t, {
    resolveHost: async () => assert.fail("health requests must not trigger DNS"),
    openUpstream: () => assert.fail("health requests must not connect upstream"),
  });
  assert.ok(observer.controlServer, "the observer must expose a separate health/count server");
  assert.equal(observer.controlServer.listening, false);
  await new Promise((resolve, reject) => {
    observer.controlServer.once("error", reject);
    observer.controlServer.listen(0, "127.0.0.1", resolve);
  });
  t.after(async () => {
    await new Promise((resolve) => observer.controlServer.close(resolve));
  });
  const port = observer.controlServer.address().port;

  const health = await exchange(port, "GET /healthz HTTP/1.1\r\nHost: observer\r\n\r\n");
  const body = health.slice(health.indexOf("\r\n\r\n") + 4);
  assert.equal(responseStatus(health), 200);
  assert.deepEqual(JSON.parse(body), observer.snapshot());

  const unknown = await exchange(
    port,
    "GET /secret?token=CONTROL_CANARY HTTP/1.1\r\nHost: observer\r\nX-Canary: HEADER_CANARY\r\n\r\n",
  );
  assert.equal(responseStatus(unknown), 404);
  assert.equal(unknown.includes("CONTROL_CANARY"), false);
  assert.equal(unknown.includes("HEADER_CANARY"), false);
  assert.equal(observer.snapshot().blocked_requests, 0);
});
