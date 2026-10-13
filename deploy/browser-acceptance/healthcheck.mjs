import { isValidSnapshot } from "./connect-observer.mjs";

try {
  const response = await fetch("http://127.0.0.1:3129/healthz", { signal: AbortSignal.timeout(1_000) });
  const snapshot = await response.json();
  if (!response.ok || !isValidSnapshot(snapshot) || snapshot.status !== "healthy") {
    process.exitCode = 1;
  }
} catch {
  process.exitCode = 1;
}
