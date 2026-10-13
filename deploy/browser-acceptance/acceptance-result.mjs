import { ALLOWED_HOST } from "./connect-observer.mjs";

const PHASES = new Set([
  "wss-not-opened",
  "wss-not-101",
  "req-pq",
  "req-dh-params",
  "set-client-dh",
  "init-connection",
  "password-challenge",
  "password-check",
  "signed-in",
]);

const CLIENT_EVENT_TRANSITIONS = new Map([
  ["req-pq", ["req-pq", "req-pq"]],
  ["req-dh-params", ["req-pq", "req-dh-params"]],
  ["set-client-dh", ["req-dh-params", "set-client-dh"]],
  ["init-connection", ["set-client-dh", "init-connection"]],
  ["password-challenge", ["init-connection", "password-challenge"]],
  ["password-check", ["password-challenge", "password-check"]],
]);

function rpcOutcome(error) {
  if (error === null || typeof error !== "object" || Array.isArray(error)) return "other";

  try {
    const { error_code: code, error_message: message } = error;
    if (code === 400 && message === "PASSWORD_HASH_INVALID") return "credential-rejected";
    if (code === 401 && message === "AUTH_KEY_UNREGISTERED") return "auth-key-unregistered";
    if (code === 420 && typeof message === "string" && /^FLOOD_WAIT_[0-9]{1,6}$/u.test(message)) {
      return "flood";
    }
    if (code === 500 && message === "INTERNAL") return "internal";
  } catch {
    return "other";
  }

  return "other";
}

export function acceptanceRpcOutcome(error) {
  return rpcOutcome(error);
}

function clientFailureOutcome(error) {
  if (error !== null && typeof error === "object" && !Array.isArray(error)) {
    try {
      if (error.kind === "timeout") return "timeout";
      if (error.kind === "closed") return "closed";
    } catch {
      return "other";
    }
  }
  return rpcOutcome(error);
}

function logoutFailureOutcome(error) {
  const outcome = clientFailureOutcome(error);
  return outcome === "credential-rejected" ? "other" : outcome;
}

function logoutReplyOutcome(reply) {
  if (reply === null || typeof reply !== "object" || Array.isArray(reply)) return "ok";

  try {
    if ("rpc_error" in reply) return logoutFailureOutcome(reply.rpc_error);
    if ("error_code" in reply || "error_message" in reply) return logoutFailureOutcome(reply);
  } catch {
    return "other";
  }

  return "ok";
}

function safeRevision(sourceCommit) {
  if (typeof sourceCommit !== "string" || !/^[0-9a-f]{40}$/u.test(sourceCommit)) {
    throw new TypeError("acceptance release revision is invalid");
  }
  return sourceCommit;
}

export function createAcceptanceProgress({ sourceCommit }) {
  const revision = safeRevision(sourceCommit);
  let phase = "wss-not-opened";
  let outcome = "other";
  let outcomeIsFinal = false;
  let wssConnections = 0;
  let logout = "not-attempted";
  let finishPromise;
  let finished = false;

  function recordFailure(value) {
    if (outcomeIsFinal || finishPromise) return false;
    outcome = value;
    outcomeIsFinal = true;
    return true;
  }

  function makeResult() {
    finished = true;
    const success = phase === "signed-in" && outcome === "ok" && logout === "ok" &&
      (wssConnections === 1 || wssConnections === 2);
    return {
      schemaVersion: 2,
      ok: success,
      phase,
      outcome,
      logout,
      targetHost: ALLOWED_HOST,
      sourceCommit: revision,
      wssConnections,
    };
  }

  return {
    observeWssConnection() {
      if (finished || (outcomeIsFinal && phase !== "signed-in")) return false;
      wssConnections = Math.min(3, wssConnections + 1);
      if (phase === "wss-not-opened") phase = "wss-not-101";
      return true;
    },
    observeWssHandshake(status) {
      if (finishPromise || outcomeIsFinal || phase !== "wss-not-101" || wssConnections === 0 || status !== 101) {
        return false;
      }
      phase = "req-pq";
      return true;
    },
    observeClientEvent(event) {
      if (finishPromise || outcomeIsFinal || !PHASES.has(event) || event === "signed-in") return false;
      const transition = CLIENT_EVENT_TRANSITIONS.get(event);
      if (!transition || phase !== transition[0]) return false;
      phase = transition[1];
      return true;
    },
    recordRpcError(error) {
      return recordFailure(rpcOutcome(error));
    },
    recordClientFailure(error) {
      return recordFailure(clientFailureOutcome(error));
    },
    markSignedIn() {
      if (finishPromise || outcomeIsFinal || phase !== "password-check") return false;
      phase = "signed-in";
      outcome = "ok";
      outcomeIsFinal = true;
      return true;
    },
    finish(invokeAuthLogOut, { logoutTimeoutMs = 10_000 } = {}) {
      if (finishPromise) return finishPromise;
      finishPromise = (async () => {
        if (phase !== "signed-in") {
          logout = "not-attempted";
          return makeResult();
        }

        if (typeof invokeAuthLogOut !== "function" || !Number.isSafeInteger(logoutTimeoutMs) || logoutTimeoutMs < 1) {
          logout = "other";
          return makeResult();
        }

        let timer;
        const timedOut = Symbol("logout-timeout");
        const timeout = new Promise((_, reject) => {
          timer = setTimeout(() => reject(timedOut), logoutTimeoutMs);
        });
        try {
          const reply = await Promise.race([
            Promise.resolve().then(() => invokeAuthLogOut("auth.logOut")),
            timeout,
          ]);
          logout = logoutReplyOutcome(reply);
        } catch (error) {
          logout = error === timedOut ? "timeout" : logoutFailureOutcome(error);
        } finally {
          clearTimeout(timer);
        }
        return makeResult();
      })();
      return finishPromise;
    },
  };
}
