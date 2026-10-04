const keys = [
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

try {
  const response = await fetch("http://127.0.0.1:3129/healthz", { signal: AbortSignal.timeout(1_000) });
  const snapshot = await response.json();
  if (!response.ok || JSON.stringify(Object.keys(snapshot).sort()) !== JSON.stringify(keys) ||
      snapshot.status !== "healthy" || snapshot.allowed_host !== "telegram-server.tailaa4918.ts.net") {
    process.exitCode = 1;
  }
} catch {
  process.exitCode = 1;
}
