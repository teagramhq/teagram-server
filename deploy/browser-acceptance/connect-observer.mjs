import { lookup as dnsLookup } from "node:dns/promises";
import http from "node:http";
import net from "node:net";

export const ALLOWED_HOST = "teagram.tailaa4918.ts.net";
export const MAX_HEADER_BYTES = 8 * 1024;
export const HEADERS_TIMEOUT_MS = 2_000;
export const REQUEST_TIMEOUT_MS = 5_000;
export const CONNECTIONS_CHECKING_INTERVAL_MS = 500;
export const DNS_TIMEOUT_MS = 2_000;
export const UPSTREAM_CONNECT_TIMEOUT_MS = 2_000;
const MAX_COUNTER_VALUE = 100;

// Let the per-socket timer send 408 before Node's parser timeout can fire.
const SERVER_HEADERS_TIMEOUT_MS = HEADERS_TIMEOUT_MS + CONNECTIONS_CHECKING_INTERVAL_MS;
const ALLOWED_PORT = 443;
const ALLOWED_HOSTS = Object.freeze([ALLOWED_HOST]);
const TELEGRAM_SUFFIXES = Object.freeze(["telegram.org", "t.me", "telegram.me", "telesco.pe"]);
const CONNECT_ATTEMPT_KEYS = Object.freeze([
  ...ALLOWED_HOSTS,
  "telegram-suffix",
  "other",
].sort());
const COUNTER_KEYS = Object.freeze([
  "allowed_connects",
  "blocked_requests",
  "telegram_attempts",
  "other_blocked_count",
  "dns_lookups",
  "upstream_connects",
  "upstream_failures",
]);
const SNAPSHOT_KEYS = Object.freeze([
  "status",
  "allowed_host",
  "connect_attempts",
  ...COUNTER_KEYS,
].sort());

function configurationError(code) {
  const error = new Error("observer configuration invalid");
  error.code = code;
  return error;
}

function toIpv6Words(address) {
  if (net.isIP(address) !== 6 || address.includes("%") || address.includes(".")) {
    return null;
  }

  const halves = address.toLowerCase().split("::");
  if (halves.length > 2) {
    return null;
  }

  const left = halves[0] === "" ? [] : halves[0].split(":");
  const right = halves.length === 1 || halves[1] === "" ? [] : halves[1].split(":");
  const missingWords = 8 - left.length - right.length;
  if ((halves.length === 1 && missingWords !== 0) || (halves.length === 2 && missingWords < 1)) {
    return null;
  }

  const words = [...left, ...Array(missingWords).fill("0"), ...right];
  if (words.length !== 8 || words.some((word) => !/^[0-9a-f]{1,4}$/u.test(word))) {
    return null;
  }
  return words.map((word) => Number.parseInt(word, 16));
}

export function isTailscaleAddress(address) {
  const family = typeof address === "string" ? net.isIP(address) : 0;
  if (family === 4) {
    const octets = address.split(".").map(Number);
    return octets[0] === 100 && octets[1] >= 64 && octets[1] <= 127;
  }
  if (family !== 6) {
    return false;
  }

  const words = toIpv6Words(address);
  return words !== null && words[0] === 0xfd7a && words[1] === 0x115c && words[2] === 0xa1e0;
}

function authorityHostCandidate(authority) {
  if (typeof authority !== "string" || authority.length === 0 || /[^\x00-\x7f]/u.test(authority)) {
    return "";
  }

  let candidate = authority;
  const absoluteForm = /^[a-z][a-z0-9+.-]*:\/\/([^/?#]*)/iu.exec(candidate);
  if (absoluteForm) {
    candidate = absoluteForm[1];
  }
  candidate = candidate.split(/[/?#]/u, 1)[0];
  candidate = candidate.slice(candidate.lastIndexOf("@") + 1);
  if (candidate.startsWith("[")) {
    return "";
  }
  const portSeparator = candidate.lastIndexOf(":");
  if (portSeparator >= 0) {
    candidate = candidate.slice(0, portSeparator);
  }

  for (let pass = 0; pass < 2; pass += 1) {
    if (candidate.endsWith(".")) {
      candidate = candidate.slice(0, -1);
    }
    const normalized = candidate.toLowerCase();
    if (!candidate.includes("%")) {
      return normalized;
    }
    try {
      candidate = decodeURIComponent(candidate);
    } catch {
      return "";
    }
  }
  return "";
}

function officialHostCandidate(authority) {
  const candidate = authorityHostCandidate(authority);
  return TELEGRAM_SUFFIXES.some((suffix) => (
    candidate === suffix || candidate.endsWith(`.${suffix}`)
  )) ? candidate : "";
}

function isTelegramAttempt(authority) {
  return officialHostCandidate(authority) !== "";
}

function classifyConnectAuthority(authority) {
  const host = authorityHostCandidate(authority);
  if (ALLOWED_HOSTS.includes(host)) return host;
  if (TELEGRAM_SUFFIXES.some((suffix) => host === suffix || host.endsWith(`.${suffix}`))) {
    return "telegram-suffix";
  }
  return "other";
}

function parseAllowedAuthority(authority) {
  if (typeof authority !== "string" || authority.length > 260 || /[^\x21-\x7e]/u.test(authority)) {
    return null;
  }
  const match = /^([a-z0-9.-]+):([0-9]+)$/iu.exec(authority);
  if (!match || match[2] !== String(ALLOWED_PORT)) {
    return null;
  }
  if (match[1].toLowerCase() !== ALLOWED_HOST) {
    return null;
  }
  return { host: ALLOWED_HOST, port: ALLOWED_PORT };
}

function requestHostAuthorities(request) {
  const authorities = [];
  for (let index = 0; index < request.rawHeaders.length; index += 2) {
    if (request.rawHeaders[index].toLowerCase() === "host") {
      authorities.push(request.rawHeaders[index + 1]);
    }
  }
  return authorities;
}

function requestHasTelegramAttempt(request) {
  return isTelegramAttempt(request.url)
    || requestHostAuthorities(request).some((authority) => isTelegramAttempt(authority));
}

function requestHasPinnedHostAuthority(request) {
  const authorities = requestHostAuthorities(request);
  return authorities.length === 1 && parseAllowedAuthority(authorities[0]) !== null;
}

function increment(counter, counters) {
  counters[counter] = Math.min(counters[counter] + 1, MAX_COUNTER_VALUE);
}

function defaultResolveHost(host) {
  return dnsLookup(host, { all: true, verbatim: true });
}

function defaultOpenUpstream(address, port, family) {
  return net.createConnection({ host: address, port, family });
}

function withTimeout(promise, timeoutMs, code) {
  let timer;
  const timeout = new Promise((resolve, reject) => {
    timer = setTimeout(() => {
      const error = new Error("observer operation timed out");
      error.code = code;
      reject(error);
    }, timeoutMs);
  });
  return Promise.race([promise, timeout]).finally(() => clearTimeout(timer));
}

function waitForConnect(socket) {
  return new Promise((resolve, reject) => {
    let settled = false;
    const timer = setTimeout(() => finish(configurationError("ERR_OBSERVER_UPSTREAM_TIMEOUT")), UPSTREAM_CONNECT_TIMEOUT_MS);
    const finish = (error) => {
      if (settled) {
        return;
      }
      settled = true;
      clearTimeout(timer);
      socket.removeListener("connect", onConnect);
      if (error) {
        reject(error);
      } else {
        resolve();
      }
    };
    const onConnect = () => finish();
    const onError = () => finish(configurationError("ERR_OBSERVER_UPSTREAM_CONNECT"));
    socket.once("connect", onConnect);
    socket.once("error", onError);
  });
}

function writeFixedResponse(socket, statusCode) {
  const phrases = new Map([
    [400, "Bad Request"],
    [403, "Forbidden"],
    [405, "Method Not Allowed"],
    [408, "Request Timeout"],
    [417, "Expectation Failed"],
    [404, "Not Found"],
    [431, "Request Header Fields Too Large"],
    [502, "Bad Gateway"],
    [503, "Service Unavailable"],
  ]);
  const phrase = phrases.get(statusCode) ?? "Bad Request";
  if (socket.destroyed || !socket.writable) {
    return;
  }
  socket.end(`HTTP/1.1 ${statusCode} ${phrase}\r\nConnection: close\r\nContent-Length: 0\r\n\r\n`);
}

export function isValidSnapshot(snapshot) {
  try {
    if (snapshot === null || typeof snapshot !== "object" || Array.isArray(snapshot)) {
      return false;
    }
    const keys = Object.keys(snapshot).sort();
    if (keys.length !== SNAPSHOT_KEYS.length || keys.some((key, index) => key !== SNAPSHOT_KEYS[index])) {
      return false;
    }
    if ((snapshot.status !== "healthy" && snapshot.status !== "unhealthy") ||
        snapshot.allowed_host !== ALLOWED_HOST) {
      return false;
    }
    const connectAttempts = snapshot.connect_attempts;
    if (connectAttempts === null || typeof connectAttempts !== "object" || Array.isArray(connectAttempts)) {
      return false;
    }
    const attemptKeys = Object.keys(connectAttempts).sort();
    if (attemptKeys.length !== CONNECT_ATTEMPT_KEYS.length ||
        attemptKeys.some((key, index) => key !== CONNECT_ATTEMPT_KEYS[index])) {
      return false;
    }
    if (COUNTER_KEYS.some((key) => (
      !Number.isSafeInteger(snapshot[key]) || snapshot[key] < 0 || snapshot[key] > MAX_COUNTER_VALUE
    )) || CONNECT_ATTEMPT_KEYS.some((key) => (
      !Number.isSafeInteger(connectAttempts[key]) || connectAttempts[key] < 0 ||
      connectAttempts[key] > MAX_COUNTER_VALUE
    ))) {
      return false;
    }
    return true;
  } catch {
    return false;
  }
}

export function isReadySnapshot(snapshot) {
  if (!isValidSnapshot(snapshot)) return false;
  try {
    return snapshot.status === "healthy"
      && snapshot.blocked_requests === 0
      && snapshot.telegram_attempts === 0
      && snapshot.other_blocked_count === 0
      && snapshot.connect_attempts["telegram-suffix"] === 0
      && snapshot.connect_attempts.other === 0
      && snapshot.upstream_failures === 0
      && snapshot.allowed_connects > 0
      && snapshot.connect_attempts[ALLOWED_HOST] >= snapshot.allowed_connects
      && snapshot.dns_lookups >= snapshot.upstream_connects
      && snapshot.upstream_connects >= snapshot.allowed_connects;
  } catch {
    return false;
  }
}

export function createConnectObserver(options = {}) {
  if (options === null || typeof options !== "object" || Array.isArray(options)) {
    throw configurationError("ERR_OBSERVER_CONFIG");
  }
  const {
    manifestHost,
    resolveHost = defaultResolveHost,
    openUpstream = defaultOpenUpstream,
  } = options;
  if (manifestHost !== ALLOWED_HOST) {
    throw configurationError("ERR_OBSERVER_MANIFEST_MISMATCH");
  }
  if (typeof resolveHost !== "function" || typeof openUpstream !== "function") {
    throw configurationError("ERR_OBSERVER_CONFIG");
  }

  const counters = Object.fromEntries(COUNTER_KEYS.map((key) => [key, 0]));
  const connectAttempts = Object.fromEntries(CONNECT_ATTEMPT_KEYS.map((key) => [key, 0]));
  let listenerErrors = 0;
  const headerTimers = new WeakMap();
  const timedOutSockets = new WeakSet();
  const clearHeaderTimer = (socket) => {
    const timer = headerTimers.get(socket);
    if (timer !== undefined) {
      clearTimeout(timer);
      headerTimers.delete(socket);
    }
  };
  const recordBlocked = (telegramAttempt) => {
    increment("blocked_requests", counters);
    increment(telegramAttempt ? "telegram_attempts" : "other_blocked_count", counters);
  };
  const recordConnectAttempt = (request) => {
    increment(classifyConnectAuthority(request.url), connectAttempts);
  };

  const server = http.createServer({
    maxHeaderSize: MAX_HEADER_BYTES,
    headersTimeout: SERVER_HEADERS_TIMEOUT_MS,
    requestTimeout: REQUEST_TIMEOUT_MS,
    connectionsCheckingInterval: CONNECTIONS_CHECKING_INTERVAL_MS,
    keepAliveTimeout: 1_000,
  }, (request, response) => {
    clearHeaderTimer(request.socket);
    recordBlocked(requestHasTelegramAttempt(request));
    response.writeHead(405, {
      connection: "close",
      "content-length": "0",
    });
    response.end();
  });
  server.on("connection", (socket) => {
    const timer = setTimeout(() => {
      headerTimers.delete(socket);
      timedOutSockets.add(socket);
      recordBlocked(false);
      writeFixedResponse(socket, 408);
    }, HEADERS_TIMEOUT_MS);
    headerTimers.set(socket, timer);
    socket.once("close", () => clearHeaderTimer(socket));
  });
  server.on("connect", (request, client, head) => {
    clearHeaderTimer(client);
    recordConnectAttempt(request);
    const telegramAttempt = requestHasTelegramAttempt(request);
    let requestSettled = false;
    let upstream;
    client.on("error", () => {
      if (upstream) {
        upstream.destroy();
      }
    });
    client.once("close", () => {
      if (upstream) {
        upstream.destroy();
      }
    });
    const deny = (statusCode) => {
      if (requestSettled) {
        return;
      }
      requestSettled = true;
      recordBlocked(telegramAttempt);
      writeFixedResponse(client, statusCode);
    };

    const handle = async () => {
      const allowed = parseAllowedAuthority(request.url);
      if (!allowed || !requestHasPinnedHostAuthority(request)) {
        deny(403);
        return;
      }

      increment("dns_lookups", counters);
      let addresses;
      try {
        addresses = await withTimeout(
          Promise.resolve().then(() => resolveHost(ALLOWED_HOST)),
          DNS_TIMEOUT_MS,
          "ERR_OBSERVER_DNS_TIMEOUT",
        );
      } catch {
        increment("upstream_failures", counters);
        deny(502);
        return;
      }

      if (!Array.isArray(addresses) || addresses.length === 0) {
        increment("upstream_failures", counters);
        deny(502);
        return;
      }
      const validatedAddresses = addresses.filter((entry) => (
        entry
        && typeof entry.address === "string"
        && isTailscaleAddress(entry.address)
        && entry.family === net.isIP(entry.address)
      ));
      if (validatedAddresses.length !== addresses.length) {
        increment("upstream_failures", counters);
        deny(502);
        return;
      }
      if (client.destroyed || requestSettled) {
        return;
      }

      const address = validatedAddresses[0];
      increment("upstream_connects", counters);
      try {
        upstream = openUpstream(address.address, ALLOWED_PORT, address.family);
        if (!upstream || typeof upstream.once !== "function" || typeof upstream.removeListener !== "function") {
          throw configurationError("ERR_OBSERVER_UPSTREAM_SOCKET");
        }
        await waitForConnect(upstream);
      } catch {
        if (upstream) {
          upstream.destroy();
        }
        increment("upstream_failures", counters);
        deny(502);
        return;
      }

      if (client.destroyed || requestSettled) {
        upstream.destroy();
        return;
      }
      requestSettled = true;
      increment("allowed_connects", counters);
      client.write("HTTP/1.1 200 Connection Established\r\n\r\n");
      if (head.length > 0) {
        upstream.write(head);
      }
      upstream.on("error", () => client.destroy());
      upstream.once("close", () => {
        if (!client.destroyed) {
          client.end();
        }
      });
      client.pipe(upstream);
      upstream.pipe(client);
    };

    void handle().catch(() => deny(502));
  });

  server.on("checkContinue", (request, response) => {
    clearHeaderTimer(request.socket);
    if (request.method === "CONNECT") recordConnectAttempt(request);
    recordBlocked(requestHasTelegramAttempt(request));
    response.writeHead(405, { connection: "close", "content-length": "0" });
    response.end();
  });
  server.on("checkExpectation", (request, response) => {
    clearHeaderTimer(request.socket);
    if (request.method === "CONNECT") recordConnectAttempt(request);
    recordBlocked(requestHasTelegramAttempt(request));
    response.writeHead(417, { connection: "close", "content-length": "0" });
    response.end();
  });
  server.on("upgrade", (request, socket) => {
    clearHeaderTimer(socket);
    if (request.method === "CONNECT") recordConnectAttempt(request);
    recordBlocked(requestHasTelegramAttempt(request));
    writeFixedResponse(socket, 405);
  });
  server.on("clientError", (error, socket) => {
    clearHeaderTimer(socket);
    if (timedOutSockets.has(socket)) {
      return;
    }
    recordBlocked(false);
    writeFixedResponse(socket, error.code === "HPE_HEADER_OVERFLOW" ? 431 : 400);
  });
  server.on("error", () => {
    listenerErrors = Math.min(listenerErrors + 1, Number.MAX_SAFE_INTEGER);
  });
  server.sendDate = false;

  const snapshot = () => {
    const current = {
      status: server.listening && listenerErrors === 0 ? "healthy" : "unhealthy",
      allowed_host: ALLOWED_HOST,
      connect_attempts: Object.freeze({ ...connectAttempts }),
    };
    for (const key of COUNTER_KEYS) {
      current[key] = counters[key];
    }
    return Object.freeze(current);
  };
  const controlServer = http.createServer({
    maxHeaderSize: MAX_HEADER_BYTES,
    headersTimeout: SERVER_HEADERS_TIMEOUT_MS,
    requestTimeout: REQUEST_TIMEOUT_MS,
    connectionsCheckingInterval: CONNECTIONS_CHECKING_INTERVAL_MS,
    keepAliveTimeout: 1_000,
  }, (request, response) => {
    clearControlHeaderTimer(request.socket);
    if (request.method !== "GET") {
      response.writeHead(405, { connection: "close", "content-length": "0" });
      response.end();
      return;
    }
    if (request.url !== "/healthz") {
      response.writeHead(404, { connection: "close", "content-length": "0" });
      response.end();
      return;
    }
    const current = snapshot();
    const body = JSON.stringify(current);
    response.writeHead(current.status === "healthy" ? 200 : 503, {
      connection: "close",
      "cache-control": "no-store",
      "content-length": String(Buffer.byteLength(body)),
      "content-type": "application/json",
    });
    response.end(body);
  });
  const controlHeaderTimers = new WeakMap();
  const clearControlHeaderTimer = (socket) => {
    const timer = controlHeaderTimers.get(socket);
    if (timer !== undefined) {
      clearTimeout(timer);
      controlHeaderTimers.delete(socket);
    }
  };
  controlServer.on("connection", (socket) => {
    const timer = setTimeout(() => {
      controlHeaderTimers.delete(socket);
      writeFixedResponse(socket, 408);
    }, HEADERS_TIMEOUT_MS);
    controlHeaderTimers.set(socket, timer);
    socket.once("close", () => clearControlHeaderTimer(socket));
  });
  controlServer.on("clientError", (error, socket) => {
    clearControlHeaderTimer(socket);
    writeFixedResponse(socket, error.code === "HPE_HEADER_OVERFLOW" ? 431 : 400);
  });
  controlServer.on("error", () => {
    listenerErrors = Math.min(listenerErrors + 1, Number.MAX_SAFE_INTEGER);
  });
  controlServer.sendDate = false;

  return {
    server,
    controlServer,
    snapshot,
  };
}
