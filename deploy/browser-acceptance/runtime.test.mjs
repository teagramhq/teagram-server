import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import {
  ALLOWED_HOST,
  isReadyObserverSnapshot,
  readReleaseRecord,
  snapshot,
  startIndependentCapture,
  verifySandbox,
} from "./qa-runtime-adapter.mjs";

const sourceCommit = "a".repeat(40);
const digest = `sha256:${"b".repeat(64)}`;

test("runtime adapter exports the pinned host used by the runtime entrypoint", () => {
  assert.equal(ALLOWED_HOST, "telegram-server.tailaa4918.ts.net");
});

async function withRecord(record, run) {
  const directory = await mkdtemp(join(tmpdir(), "browser-acceptance-test-"));
  const path = join(directory, "release-record.json");
  try {
    await writeFile(path, JSON.stringify(record), { mode: 0o600 });
    await run(path);
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
}

function releaseRecord(overrides = {}) {
  return {
    sourceCommit,
    archiveSha256: digest,
    contentDigest: digest,
    url: `https://telegram-server.tailaa4918.ts.net/client/?v=${sourceCommit}`,
    ...overrides,
  };
}

test("verified release input accepts only the exact pinned host and typed digests", async () => {
  await withRecord(releaseRecord(), async (path) => {
    const result = await readReleaseRecord(path);
    assert.equal(result.sourceCommit, sourceCommit);
    assert.equal(new URL(result.url).hostname, "telegram-server.tailaa4918.ts.net");
  });
  await withRecord(releaseRecord({ url: `https://attacker.example/client/?v=${sourceCommit}` }), async (path) => {
    await assert.rejects(readReleaseRecord(path), (error) => error.code === "release-record-invalid");
  });
  await withRecord(releaseRecord({ contentDigest: "sha256:wrong" }), async (path) => {
    await assert.rejects(readReleaseRecord(path), (error) => error.code === "release-record-invalid");
  });
  await withRecord({ ...releaseRecord(), unexpected: "must-not-widen-policy" }, async (path) => {
    await assert.rejects(readReleaseRecord(path), (error) => error.code === "release-record-invalid");
  });
});

test("readiness requires the observer's exact healthy zero-block contract", () => {
  const healthy = {
    status: "healthy",
    allowed_host: "telegram-server.tailaa4918.ts.net",
    allowed_connects: 1,
    blocked_requests: 0,
    telegram_attempts: 0,
    other_blocked_count: 0,
    dns_lookups: 1,
    upstream_connects: 1,
    upstream_failures: 0,
  };
  assert.equal(isReadyObserverSnapshot(healthy), true);
  assert.equal(isReadyObserverSnapshot({ ...healthy, other_blocked_count: 1 }), false);
  assert.equal(isReadyObserverSnapshot({ ...healthy, extra: "not an observer field" }), false);
  assert.equal(isReadyObserverSnapshot({ ...healthy, dns_lookups: 0 }), false);
});

test("host-only acceptance observations fail closed without a reviewed provider", async () => {
  await assert.rejects(snapshot("before"), (error) => error.code === "runtime-snapshot-unavailable");
  await assert.rejects(startIndependentCapture({}), (error) => error.code === "independent-capture-unavailable");
});

test("sandbox proof cannot be inferred when Chromium processes are absent", async () => {
  await assert.rejects(
    verifySandbox({ expectedProxyServer: "http://browser-observer:3128" }),
    (error) => error.code === "sandbox-proof-invalid",
  );
});

for (const executable of ["chrome-headless-shell", "headless_shell"]) {
  test(`sandbox inspection records Playwright's ${executable} processes`, async () => {
    const children = [];
    try {
      for (const args of [
        ["--proxy-server=http://browser-observer:3128", "--proxy-bypass-list=<-loopback>",
          "--disable-background-networking", "--disable-crash-reporter", "--disable-breakpad",
          "--disable-quic", "--dns-over-https-mode=off",
          "--force-webrtc-ip-handling-policy=disable_non_proxied_udp"],
        ["--type=renderer"],
      ]) {
        // Real /proc entries exercise argv discovery; these fixture processes
        // share namespaces and therefore cannot prove sandbox isolation.
        const child = spawn(process.execPath, [
          "-e", "process.send('ready'); setInterval(() => {}, 1000)", "--", ...args,
        ], { argv0: `/ms-playwright/chromium/${executable}`, stdio: ["ignore", "ignore", "ignore", "ipc"] });
        children.push(child);
        await once(child, "message");
      }
      const proof = await verifySandbox({ expectedProxyServer: "http://browser-observer:3128" });
      assert.equal(proof.observedProxyServer, "http://browser-observer:3128");
      assert.equal(proof.observedProxyBypassList, "<-loopback>");
      assert.deepEqual(proof.missingRequiredNetworkArgs, []);
      assert.deepEqual(proof.conflictingNetworkArgs, []);
      assert.deepEqual(proof.forbiddenSandboxFlags, []);
      assert.ok(proof.browserUserNamespaceInode > 0);
      assert.ok(proof.rendererPidNamespaceInode > 0);
      assert.equal(proof.browserUserNamespaceInode, proof.rendererUserNamespaceInode);
      assert.equal(proof.browserPidNamespaceInode, proof.rendererPidNamespaceInode);
    } finally {
      await Promise.all(children.map(async (child) => {
        const exited = once(child, "exit");
        child.kill();
        await exited;
      }));
    }
  });
}

test("sandbox proof rejects disable-sandbox flags in renderer argv", async () => {
  const children = [];
  try {
    for (const args of [
      ["--proxy-server=http://browser-observer:3128", "--proxy-bypass-list=<-loopback>",
        "--disable-background-networking", "--disable-crash-reporter", "--disable-breakpad",
        "--disable-quic", "--dns-over-https-mode=off",
        "--force-webrtc-ip-handling-policy=disable_non_proxied_udp"],
      ["--type=renderer", "--disable-setuid-sandbox", "--no-sandbox=1"],
    ]) {
      const child = spawn(process.execPath, [
        "-e", "process.send('ready'); setInterval(() => {}, 1000)", "--", ...args,
      ], { argv0: "/ms-playwright/chromium/chrome-headless-shell", stdio: ["ignore", "ignore", "ignore", "ipc"] });
      children.push(child);
      await once(child, "message");
    }

    const proof = await verifySandbox({ expectedProxyServer: "http://browser-observer:3128" });
    assert.equal(proof.chromiumSandboxEnabled, false);
    assert.deepEqual(proof.forbiddenSandboxFlags, ["--disable-setuid-sandbox", "--no-sandbox=1"]);
  } finally {
    await Promise.all(children.map(async (child) => {
      const exited = once(child, "exit");
      child.kill();
      await exited;
    }));
  }
});
