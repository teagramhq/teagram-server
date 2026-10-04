import { lookup } from "node:dns/promises";
import http from "node:http";
import net from "node:net";

import { ALLOWED_HOST, createConnectObserver, isTailscaleAddress } from "./connect-observer.mjs";

const observer = createConnectObserver({ manifestHost: ALLOWED_HOST });

const targetAddressServer = http.createServer(async (request, response) => {
  if (request.method !== "GET" || request.url !== "/target-address") {
    response.writeHead(404, { connection: "close", "content-length": "0" });
    response.end();
    return;
  }

  try {
    const addresses = await lookup(ALLOWED_HOST, { all: true, verbatim: true });
    if (addresses.length === 0 || addresses.some((entry) => (
      !isTailscaleAddress(entry.address) || entry.family !== net.isIP(entry.address)
    ))) {
      throw new Error("target address policy failed");
    }
    const address = addresses[0];
    const body = JSON.stringify({ address: address.address, family: address.family });
    response.writeHead(200, {
      connection: "close",
      "cache-control": "no-store",
      "content-length": String(Buffer.byteLength(body)),
      "content-type": "application/json",
    });
    response.end(body);
  } catch {
    response.writeHead(503, { connection: "close", "content-length": "0" });
    response.end();
  }
});

for (const [server, port] of [
  [observer.server, 3128],
  [observer.controlServer, 3129],
  [targetAddressServer, 3130],
]) {
  server.listen(port, "0.0.0.0");
}
