import fs from 'node:fs';
import https from 'node:https';
import net from 'node:net';

const PROBE_CSP = "default-src 'self'; connect-src 'self' wss://telegramd.test/apiws; script-src 'self'; worker-src 'self'; object-src 'none'; base-uri 'none'";
const PRIVATE_CSP = "default-src 'self'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'; object-src 'none'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self' data:; media-src 'self' blob:; worker-src 'self' blob:; manifest-src 'self'; connect-src 'self' wss://telegramd.test/apiws;";
const PROBE_PREFIX = '/_fixture_probe/';
const probeTarget = fs.readFileSync('/run/mtproto-target.json');
const args = process.argv.slice(2);
const artifactIndex = args.indexOf('--artifact-dir');
const artifactRoot = artifactIndex === -1 ? null : args[artifactIndex + 1];
const artifactFiles = new Map();
const contentTypes = new Map([
  ['.avif', 'image/avif'],
  ['.css', 'text/css; charset=utf-8'],
  ['.gif', 'image/gif'],
  ['.html', 'text/html; charset=utf-8'],
  ['.ico', 'image/x-icon'],
  ['.jpeg', 'image/jpeg'],
  ['.jpg', 'image/jpeg'],
  ['.js', 'text/javascript; charset=utf-8'],
  ['.json', 'application/json; charset=utf-8'],
  ['.map', 'application/json; charset=utf-8'],
  ['.mjs', 'text/javascript; charset=utf-8'],
  ['.otf', 'font/otf'],
  ['.png', 'image/png'],
  ['.svg', 'image/svg+xml'],
  ['.ttf', 'font/ttf'],
  ['.wasm', 'application/wasm'],
  ['.webm', 'video/webm'],
  ['.webmanifest', 'application/manifest+json'],
  ['.webp', 'image/webp'],
  ['.woff', 'font/woff'],
  ['.woff2', 'font/woff2'],
]);

if (artifactRoot) {
  const visitArtifactDirectory = (directory, prefix = '') => {
    for (const entry of fs.readdirSync(directory, { withFileTypes: true }).sort((left, right) => left.name.localeCompare(right.name))) {
      const filePath = `${directory}/${entry.name}`;
      const relativePath = prefix ? `${prefix}/${entry.name}` : entry.name;
      const stats = fs.lstatSync(filePath);
      if (entry.isDirectory() && stats.isDirectory() && !stats.isSymbolicLink()) {
        visitArtifactDirectory(filePath, relativePath);
        continue;
      }
      if (!entry.isFile() || !stats.isFile() || stats.isSymbolicLink()) {
        throw new Error('staged artifact contains an unsupported file type');
      }
      const urlPath = `/${relativePath.split('/').map((segment) => encodeURIComponent(segment)).join('/')}`;
      if (urlPath === '/index.html') artifactFiles.set('/', filePath);
      artifactFiles.set(urlPath, filePath);
    }
  };
  visitArtifactDirectory(artifactRoot);
  if (!artifactFiles.has('/') || !artifactFiles.has('/mtproto-target.json')) {
    throw new Error('staged artifact is missing its production entry or target manifest');
  }
}

let websocket101Count = 0;
let websocketRouteErrors = 0;
const websocketRouteErrorDetails = [];

const server = https.createServer({
  key: fs.readFileSync('/run/tls.key'),
  cert: fs.readFileSync('/run/tls.crt'),
}, (request, response) => {
  if (request.headers.host !== 'telegramd.test') {
    response.writeHead(421);
    response.end();
    return;
  }

  if (request.url === '/healthz') {
    response.setHeader('content-security-policy', PROBE_CSP);
    response.setHeader('x-content-type-options', 'nosniff');
    response.writeHead(200, { 'content-type': 'application/json', 'cache-control': 'no-store' });
    response.end(JSON.stringify({ status: 'ready', websocket_101_count: websocket101Count, route_errors: websocketRouteErrors, route_error_details: websocketRouteErrorDetails }));
    return;
  }

  if (request.url === `${PROBE_PREFIX}mtproto-target.json`) {
    response.setHeader('content-security-policy', PROBE_CSP);
    response.setHeader('x-content-type-options', 'nosniff');
    response.writeHead(200, { 'content-type': 'application/json', 'cache-control': 'no-store' });
    response.end(probeTarget);
    return;
  }

  if (request.url === `${PROBE_PREFIX}`) {
    response.setHeader('content-security-policy', PROBE_CSP);
    response.setHeader('x-content-type-options', 'nosniff');
    response.writeHead(200, { 'content-type': 'text/html; charset=utf-8', 'cache-control': 'no-store' });
    response.end('<!doctype html><meta charset="utf-8"><title>real-server fixture</title><h1>ready</h1>');
    return;
  }

  if (request.url === `${PROBE_PREFIX}shared-worker.js`) {
    response.setHeader('content-security-policy', PROBE_CSP);
    response.setHeader('x-content-type-options', 'nosniff');
    response.writeHead(200, { 'content-type': 'text/javascript', 'cache-control': 'no-store' });
    response.end(sharedWorkerSource);
    return;
  }

  if (request.url === `${PROBE_PREFIX}service-worker.js`) {
    response.setHeader('content-security-policy', PROBE_CSP);
    response.setHeader('x-content-type-options', 'nosniff');
    response.writeHead(200, { 'content-type': 'text/javascript', 'service-worker-allowed': PROBE_PREFIX, 'cache-control': 'no-store' });
    response.end(serviceWorkerSource);
    return;
  }

  const artifactRequestPath = request.url.split('?', 1)[0];
  const artifactPath = artifactFiles.get(artifactRequestPath);
  if (artifactPath) {
    response.setHeader('content-security-policy', PRIVATE_CSP);
    response.setHeader('x-content-type-options', 'nosniff');
    response.setHeader('cache-control', 'no-store');
    const extension = artifactPath.slice(artifactPath.lastIndexOf('.')).toLowerCase();
    response.writeHead(200, { 'content-type': contentTypes.get(extension) || 'application/octet-stream' });
    fs.createReadStream(artifactPath).pipe(response);
    return;
  }

  response.setHeader('content-security-policy', PROBE_CSP);
  response.setHeader('x-content-type-options', 'nosniff');
  response.writeHead(404);
  response.end();
});

server.on('upgrade', (request, clientSocket, clientHead) => {
  if (request.headers.host !== 'telegramd.test' || request.headers.origin !== 'https://telegramd.test' || request.url !== '/apiws') {
    clientSocket.destroy();
    return;
  }

  const upstream = net.connect({ host: 'telegramd', port: 2444 });
  let upstreamHeader = Buffer.alloc(0);
  let counted = false;
  let upgraded = false;

  upstream.once('connect', () => {
    let requestHead = `${request.method} ${request.url} HTTP/${request.httpVersion}\r\n`;
    for (let index = 0; index < request.rawHeaders.length; index += 2) {
      requestHead += `${request.rawHeaders[index]}: ${request.rawHeaders[index + 1]}\r\n`;
    }
    upstream.write(`${requestHead}\r\n`);
    if (clientHead.length > 0) upstream.write(clientHead);
    clientSocket.pipe(upstream);
    upstream.pipe(clientSocket);
  });

  upstream.on('data', (chunk) => {
    if (!counted) {
      upstreamHeader = Buffer.concat([upstreamHeader, chunk]);
      const end = upstreamHeader.indexOf('\r\n\r\n');
      if (end >= 0) {
        const responseHead = upstreamHeader.toString('latin1', 0, end);
        if (/^HTTP\/1\.1 101(?: |$)/.test(responseHead)) {
          websocket101Count += 1;
          upgraded = true;
        }
        counted = true;
      }
    }
  });

  upstream.on('error', (error) => {
    if (!upgraded) {
      websocketRouteErrors += 1;
      websocketRouteErrorDetails.push({ side: 'upstream', code: error.code || 'unknown', upgraded });
    }
    clientSocket.destroy();
  });
  clientSocket.on('error', (error) => {
    if (!upgraded) {
      websocketRouteErrors += 1;
      websocketRouteErrorDetails.push({ side: 'client', code: error.code || 'unknown', upgraded });
    }
    upstream.destroy();
  });
});

const workerProbeSource = `
async function runProbeSet(targets, injectUnexpected, sourceContext) {
  const errorClass = (error) => ['TypeError', 'AbortError', 'TimeoutError'].includes(error?.name) ? error.name : 'UnexpectedError';
  const attempts = await Promise.all(targets.map(async (target) => {
    try {
      await fetch(target.url, {
        method: 'GET', mode: 'no-cors', credentials: 'omit', cache: 'no-store',
        signal: AbortSignal.timeout(1800),
      });
      return { class: target.class, blocked: false };
    } catch (error) {
      const failureClass = errorClass(error);
      return { class: target.class, blocked: failureClass !== 'UnexpectedError', error_class: failureClass };
    }
  }));

  if (injectUnexpected) {
    await issueUnannouncedEgress(sourceContext);
  }

  return { attempts };
}

async function issueUnannouncedEgress(sourceContext) {
  const fetchAttempt = fetch('https://unannounced.invalid/' + sourceContext + '/fetch', {
    method: 'GET', mode: 'no-cors', credentials: 'omit', cache: 'no-store',
    signal: AbortSignal.timeout(1800),
  }).catch(() => undefined);

  const websocketAttempt = new Promise((resolve) => {
    let settled = false;
    let timer;
    const finish = () => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      resolve();
    };
    try {
      const socket = new WebSocket('wss://unannounced.invalid/' + sourceContext + '/websocket');
      timer = setTimeout(finish, 1800);
      socket.addEventListener('open', () => { socket.close(); finish(); }, { once: true });
      socket.addEventListener('error', finish, { once: true });
    } catch {
      finish();
    }
  });
  await Promise.all([fetchAttempt, websocketAttempt]);
}
`;

const sharedWorkerSource = `
onconnect = (event) => {
  const port = event.ports[0];
  port.onmessage = async ({ data }) => {
    const result = await runProbeSet(data.targets, data.injectUnexpected, 'shared_worker');
    port.postMessage(result);
  };
  port.start();
};
${workerProbeSource}
`;

const serviceWorkerSource = `
self.addEventListener('message', (event) => {
  if (event.data?.type !== 'probe') return;
  const port = event.ports[0];
  event.waitUntil(runProbeSet(event.data.targets, event.data.injectUnexpected, 'service_worker').then((result) => port.postMessage(result)));
});
${workerProbeSource}
`;

server.listen(443, '0.0.0.0');
process.on('SIGTERM', () => server.close(() => process.exit(0)));
