import { readdirSync, readFileSync, statSync } from "node:fs";
import { readFile } from "node:fs/promises";
import { connect } from "node:net";

import { ALLOWED_HOST, isReadySnapshot, isTailscaleAddress } from "./connect-observer.mjs";

export { ALLOWED_HOST };

const OBSERVER_URL = "http://browser-observer:3129/healthz";
const TARGET_ADDRESS_URL = "http://browser-observer:3130/target-address";
const PROXY_SERVER = "http://browser-observer:3128";
const BLOCKED_CONNECT = "CONNECT telegram.org:443 HTTP/1.1\r\nHost: telegram.org:443\r\n\r\n";
const OBSERVER_KEYS = [
  "allowed_connects",
  "allowed_host",
  "blocked_requests",
  "dns_lookups",
  "other_blocked_count",
  "status",
  "telegram_attempts",
  "upstream_connects",
  "upstream_failures",
].sort();

function adapterError(code) {
  const error = new Error("runtime adapter check failed");
  error.code = code;
  return error;
}

function exactKeys(value, keys) {
  return value !== null && typeof value === "object" && !Array.isArray(value) &&
    JSON.stringify(Object.keys(value).sort()) === JSON.stringify([...keys].sort());
}

export async function observerSnapshot() {
  const response = await fetch(OBSERVER_URL, { signal: AbortSignal.timeout(2_000) });
  const snapshot = await response.json();
  if (!response.ok || !exactKeys(snapshot, OBSERVER_KEYS) || snapshot.status !== "healthy" ||
      snapshot.allowed_host !== ALLOWED_HOST || OBSERVER_KEYS.some((key) => (
        key !== "status" && key !== "allowed_host" &&
        (!Number.isSafeInteger(snapshot[key]) || snapshot[key] < 0)
      ))) {
    throw adapterError("observer-unhealthy");
  }
  return snapshot;
}

export async function readReleaseRecord(path) {
  let record;
  try {
    record = JSON.parse(await readFile(path, "utf8"));
  } catch {
    throw adapterError("release-record-invalid");
  }
  if (!exactKeys(record, ["sourceCommit", "archiveSha256", "contentDigest", "url"]) ||
      typeof record.sourceCommit !== "string" || !/^[0-9a-f]{40}$/u.test(record.sourceCommit) ||
      typeof record.archiveSha256 !== "string" || !/^sha256:[0-9a-f]{64}$/u.test(record.archiveSha256) ||
      typeof record.contentDigest !== "string" || !/^sha256:[0-9a-f]{64}$/u.test(record.contentDigest) ||
      typeof record.url !== "string") {
    throw adapterError("release-record-invalid");
  }
  let url;
  try {
    url = new URL(record.url);
  } catch {
    throw adapterError("release-record-invalid");
  }
  if (url.protocol !== "https:" || url.hostname !== ALLOWED_HOST || (url.port && url.port !== "443") ||
      url.username || url.password || url.hash || url.href !== record.url ||
      JSON.stringify([...url.searchParams.keys()].sort()) !== JSON.stringify(["v"]) ||
      url.searchParams.get("v") !== record.sourceCommit) {
    throw adapterError("release-record-invalid");
  }
  return record;
}

export async function targetAddress() {
  const response = await fetch(TARGET_ADDRESS_URL, { signal: AbortSignal.timeout(2_000) });
  if (!response.ok) throw adapterError("target-address-unavailable");
  const value = await response.json();
  if (!exactKeys(value, ["address", "family"]) || !isTailscaleAddress(value.address) ||
      value.family !== (value.address.includes(":") ? 6 : 4)) {
    throw adapterError("target-address-invalid");
  }
  return value;
}

function sendBlockedConnect() {
  const socket = connect({ host: "browser-observer", port: 3128 });
  return new Promise((resolve, reject) => {
    const chunks = [];
    const timer = setTimeout(() => {
      socket.destroy();
      reject(adapterError("blocked-host-control-failed"));
    }, 2_000);
    socket.once("connect", () => socket.write(BLOCKED_CONNECT));
    socket.on("data", (chunk) => chunks.push(chunk));
    socket.once("end", () => {
      clearTimeout(timer);
      const status = /^HTTP\/1\.1 ([0-9]{3})/u.exec(Buffer.concat(chunks).toString("latin1"))?.[1];
      if (status !== "403") reject(adapterError("blocked-host-control-failed"));
      else resolve();
    });
    socket.once("error", () => {
      clearTimeout(timer);
      reject(adapterError("blocked-host-control-failed"));
    });
  });
}

export async function assertDirectTargetBlocked(address) {
  await new Promise((resolve, reject) => {
    const socket = connect({ host: address, port: 443 });
    let settled = false;
    const finish = (error) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      socket.destroy();
      if (error) reject(error);
      else resolve();
    };
    const timer = setTimeout(() => finish(), 1_500);
    socket.once("connect", () => finish(adapterError("direct-egress-open")));
    socket.once("error", () => finish());
  });
}

async function assertBlockedRequestDeltas(before, after) {
  if (after.blocked_requests !== before.blocked_requests + 1 ||
      after.telegram_attempts !== before.telegram_attempts + 1 ||
      after.other_blocked_count !== before.other_blocked_count ||
      after.dns_lookups !== before.dns_lookups ||
      after.upstream_connects !== before.upstream_connects) {
    throw adapterError("blocked-host-control-failed");
  }
}

export async function runDeniedObserverControl() {
  const before = await observerSnapshot();
  await sendBlockedConnect();
  const after = await observerSnapshot();
  await assertBlockedRequestDeltas(before, after);
  return after;
}

export async function runBlockedHostControl() {
  const target = await targetAddress();
  const after = await runDeniedObserverControl();
  await assertDirectTargetBlocked(target.address);
  return after;
}

export async function runBrowserNetworkControls() {
  const after = await runDeniedObserverControl();
  await assertDirectTargetBlocked("1.1.1.1");
  return after;
}

export async function snapshot() {
  // These host-only checks need protected LXC access and remain a separate
  // deployment prerequisite. Never substitute constants or container state.
  throw adapterError("runtime-snapshot-unavailable");
}

export async function startIndependentCapture() {
  // CONNECT metadata is available here; independent WSS capture and the
  // target-reported fingerprint require the separately reviewed host adapter.
  throw adapterError("independent-capture-unavailable");
}

export async function cleanupNewBinding() {
  return { performed: false };
}

export async function browserLaunchPolicy({ targetHost }) {
  if (targetHost !== ALLOWED_HOST) throw adapterError("proxy-policy-invalid");
  for (const name of ["DEBUG", "PWDEBUG", "DEBUG_FILE", "PLAYWRIGHT_DISABLE_FORCED_CHROMIUM_PROXIED_LOOPBACK"]) {
    if (process.env[name] !== undefined) throw adapterError("proxy-policy-invalid");
  }
  const initial = await observerSnapshot();
  await runBlockedHostControl();
  const after = await observerSnapshot();
  if (after.telegram_attempts !== initial.telegram_attempts + 1 ||
      after.blocked_requests !== initial.blocked_requests + 1 ||
      after.dns_lookups !== initial.dns_lookups ||
      after.upstream_connects !== initial.upstream_connects) {
    throw adapterError("blocked-host-control-failed");
  }
  return {
    proxyServer: PROXY_SERVER,
    proxyBypassHosts: [],
    observerAllowedTargets: [{ host: ALLOWED_HOST, port: 443 }],
    observerConnectOnly: true,
    observerHeaderLimitBytes: 8192,
    observerConnectTimeoutEnforced: true,
    observerHealthy: true,
    browserNetworkInternal: true,
    observerOnlyEgress: true,
    directTargetTcpBlocked: true,
    allowedTargetResolvedToTailnet: true,
    blockedHostControlPassed: true,
    blockedHostDnsLookups: 0,
    blockedHostUpstreamConnects: 0,
    nonProxiedUdpBlocked: true,
  };
}

export async function verifySandbox({ expectedProxyServer }) {
  const pids = readdirSync("/proc").filter((name) => /^[0-9]+$/u.test(name));
  const processes = [];
  for (const pid of pids) {
    try {
      let argv = readFileSync(`/proc/${pid}/cmdline`).toString("utf8").split("\0").filter(Boolean);
      // Chromium rewrites child cmdlines as one space-separated process title.
      if (argv.length === 1) argv = argv[0].split(" ").filter(Boolean);
      if (argv.length === 0 || !/(?:^|\/)(?:(?:chrome|chromium)(?:-browser)?|chrome-headless-shell|headless_shell)$/u.test(argv[0])) continue;
      processes.push({
        argv,
        userNamespaceInode: statSync(`/proc/${pid}/ns/user`).ino,
        pidNamespaceInode: statSync(`/proc/${pid}/ns/pid`).ino,
      });
    } catch {
      // Chromium processes may exit during inspection; no process details are retained.
    }
  }
  const browser = processes.find((entry) => !entry.argv.some((arg) => arg.startsWith("--type=")));
  const renderer = processes.find((entry) => entry.argv.some((arg) => arg === "--type=renderer"));
  if (!browser || !renderer) throw adapterError("sandbox-proof-invalid");
  const args = browser.argv.slice(1);
  const proxyServers = args.filter((arg) => arg.startsWith("--proxy-server="));
  const proxyBypasses = args.filter((arg) => arg.startsWith("--proxy-bypass-list="));
  const forbiddenSandboxFlags = processes.flatMap((entry) => entry.argv.slice(1)).filter((arg) => (
    arg === "--no-sandbox" || arg.startsWith("--no-sandbox=") ||
    arg === "--disable-setuid-sandbox" || arg.startsWith("--disable-setuid-sandbox=")
  ));
  const missingRequiredNetworkArgs = [
    "--disable-background-networking",
    "--disable-crash-reporter",
    "--disable-breakpad",
    "--disable-quic",
    "--dns-over-https-mode=off",
    "--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
  ].filter((required) => !args.includes(required));
  const conflictingNetworkArgs = [
    ...(proxyServers.some((arg) => arg !== `--proxy-server=${expectedProxyServer}`) ? ["proxy-server"] : []),
    ...(proxyBypasses.some((arg) => arg !== "--proxy-bypass-list=<-loopback>") ? ["proxy-bypass-list"] : []),
  ];
  return {
    chromiumSandboxEnabled: forbiddenSandboxFlags.length === 0,
    forbiddenSandboxFlags,
    observedProxyServer: proxyServers.length === 1 ? proxyServers[0].slice("--proxy-server=".length) : "",
    observedProxyBypassList: proxyBypasses.length === 1 ? proxyBypasses[0].slice("--proxy-bypass-list=".length) : "",
    proxyServerArgCount: proxyServers.length,
    proxyBypassArgCount: proxyBypasses.length,
    missingRequiredNetworkArgs,
    conflictingNetworkArgs,
    browserUserNamespaceInode: browser.userNamespaceInode,
    browserPidNamespaceInode: browser.pidNamespaceInode,
    rendererUserNamespaceInode: renderer.userNamespaceInode,
    rendererPidNamespaceInode: renderer.pidNamespaceInode,
  };
}

export function isReadyObserverSnapshot(snapshotValue) {
  return isReadySnapshot(snapshotValue);
}
