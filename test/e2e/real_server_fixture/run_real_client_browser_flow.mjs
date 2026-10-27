import {randomBytes} from 'node:crypto';
import {execFileSync} from 'node:child_process';
import {appendFileSync, chmodSync, existsSync, lstatSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync} from 'node:fs';
import {createInterface} from 'node:readline';
import {join, resolve} from 'node:path';
import {tmpdir} from 'node:os';
import {FIXTURE_CLEANUP_TIMEOUT_MS, spawnTracked, terminate, terminateFixture} from './process_tree.mjs';

const SCENARIOS = 'sign-in,first-message,message,group,channel,poll';
const REVISION_PATTERN = /^[0-9a-f]{40}$/;
const EVENT_TIMEOUTS = {serverReady: 8 * 60_000, artifactReady: 3 * 60_000};
const TOTAL_BUDGET_MS = 12 * 60_000;
const CLEANUP_RESERVE_MS = 90_000;

let requestedSignal;
const commandChildren = new Set();
const signalWaiters = new Set();
const fixtureEvents = [];
let fixtureWaiter;
let fixtureProcess;
let fixtureExit;
let artifactAttached = false;
let fixtureCleanupVerified = false;
let runStage = 'input-validation';

class FlowError extends Error {
  constructor(stage, message) {
    super(message);
    this.stage = stage;
  }
}

function requiredRevision(name) {
  const value = process.env[name];
  if(!REVISION_PATTERN.test(value || '')) throw new FlowError('input-validation', `${name} must be a full lowercase SHA`);
  return value;
}

function stageTimeout(startedAt, maximumMs, stage) {
  const remainingMs = TOTAL_BUDGET_MS - (Date.now() - startedAt) - CLEANUP_RESERVE_MS;
  if(remainingMs <= 0) throw new FlowError(stage, 'real-client flow exceeded its time budget');
  return Math.min(maximumMs, remainingMs);
}

function repositoryHead(root, expected, name) {
  let head;
  try {
    head = execFileSync('git', ['-C', root, 'rev-parse', 'HEAD'], {encoding: 'utf8'}).trim();
  } catch {
    throw new FlowError('input-validation', `${name} checkout is unavailable`);
  }
  if(head !== expected) throw new FlowError('input-validation', `${name} checkout does not match its pinned revision`);
}

function cancelWaiter() {
  let rejectSignal;
  const promise = new Promise((_, reject) => {
    if(requestedSignal) {
      reject(new FlowError('cancelled', `workflow received ${requestedSignal}`));
      return;
    }
    rejectSignal = reject;
    signalWaiters.add(reject);
  });
  return {
    promise,
    remove: () => {
      if(rejectSignal) signalWaiters.delete(rejectSignal);
    }
  };
}

for(const signal of ['SIGINT', 'SIGTERM']) {
  process.on(signal, () => {
    if(requestedSignal) return;
    requestedSignal = signal;
    for(const child of commandChildren) terminate(child, signal);
    for(const reject of signalWaiters) reject(new FlowError('cancelled', `workflow received ${signal}`));
    signalWaiters.clear();
  });
}

function cleanEnvironment(extra = {}) {
  const env = {...process.env};
  for(const name of Object.keys(env)) {
    if(/^(TG_|MTPROTO_)/.test(name)) delete env[name];
  }
  return {...env, ...extra};
}

function runCommand(command, args, {cwd, env = cleanEnvironment(), stage, timeoutMs}) {
  if(requestedSignal) return Promise.reject(new FlowError('cancelled', `workflow received ${requestedSignal}`));
  const child = spawnTracked(command, args, {cwd, env, stdio: 'inherit'});
  commandChildren.add(child);
  return new Promise((resolvePromise, rejectPromise) => {
    let settled = false;
    let timer;
    let timedOut = false;
    const finish = (error) => {
      if(settled) return;
      settled = true;
      clearTimeout(timer);
      void (async () => {
        const processTreeStopped = await terminate(child, 'SIGTERM');
        commandChildren.delete(child);
        if(!processTreeStopped) {
          const message = `${command} process group did not stop`;
          error = new FlowError(error?.stage || stage, error ? `${error.message}; ${message}` : message);
        }
        if(error) rejectPromise(error);
        else resolvePromise();
      })();
    };

    child.once('error', () => finish(new FlowError(stage, `${command} could not start`)));
    child.once('close', (code) => {
      if(requestedSignal) finish(new FlowError('cancelled', `workflow received ${requestedSignal}`));
      else if(timedOut) finish(new FlowError(stage, `${command} exceeded its time limit`));
      else if(code !== 0) finish(new FlowError(stage, `${command} exited with status ${code}`));
      else finish();
    });
    if(timeoutMs) {
      timer = setTimeout(() => {
        timedOut = true;
        void terminate(child, 'SIGTERM');
      }, timeoutMs);
    }
  });
}

function receiveFixtureEvent(line) {
  let event;
  try {
    event = JSON.parse(line);
  } catch {
    fixtureWaiter?.reject(new FlowError(runStage, 'fixture emitted invalid readiness data'));
    fixtureWaiter = undefined;
    return;
  }
  if(!event || !['server-ready', 'artifact-ready'].includes(event.event)) {
    fixtureWaiter?.reject(new FlowError(runStage, 'fixture emitted an unexpected readiness event'));
    fixtureWaiter = undefined;
    return;
  }
  if(fixtureWaiter?.event === event.event) {
    const waiter = fixtureWaiter;
    fixtureWaiter = undefined;
    waiter.resolve(event);
  } else {
    fixtureEvents.push(event);
  }
}

function waitForFixtureEvent(eventName, timeoutMs) {
  const existingIndex = fixtureEvents.findIndex((event) => event.event === eventName);
  if(existingIndex >= 0) return Promise.resolve(fixtureEvents.splice(existingIndex, 1)[0]);
  if(!fixtureProcess) return Promise.reject(new FlowError(runStage, 'fixture is not running'));

  let timer;
  const eventPromise = new Promise((resolvePromise, rejectPromise) => {
    fixtureWaiter = {event: eventName, resolve: resolvePromise, reject: rejectPromise};
  });
  const timeoutPromise = new Promise((_, reject) => {
    timer = setTimeout(() => reject(new FlowError(runStage, `fixture did not report ${eventName} before timeout`)), timeoutMs);
  });
  const exitPromise = fixtureExit.then(({code}) => {
    throw new FlowError(runStage, `fixture exited with status ${code} before ${eventName}`);
  });
  const cancellation = cancelWaiter();
  return Promise.race([eventPromise, timeoutPromise, exitPromise, cancellation.promise]).finally(() => {
    cancellation.remove();
    clearTimeout(timer);
    if(fixtureWaiter?.event === eventName) fixtureWaiter = undefined;
  });
}

function appendReadiness(readinessPath, event) {
  appendFileSync(readinessPath, `${JSON.stringify(event)}\n`, {mode: 0o600});
}

function writeFixtureCommand(command) {
  if(!fixtureProcess || fixtureProcess.stdin.destroyed || !fixtureProcess.stdin.writable) {
    throw new FlowError('fixture-attachment', 'fixture input is unavailable');
  }
  fixtureProcess.stdin.write(`${command}\n`);
}

async function stopFixture({allowNonzeroExit = false} = {}) {
  if(!fixtureProcess) return;
  if(fixtureProcess.exitCode !== null || fixtureProcess.signalCode !== null) {
    const previous = await fixtureExit;
    const processTreeStopped = await terminateFixture(fixtureProcess, 'SIGTERM');
    if(!processTreeStopped) throw new FlowError('fixture-cleanup', 'fixture process group did not stop');
    if((previous.code !== 0 && !allowNonzeroExit) || !fixtureCleanupVerified) {
      throw new FlowError('fixture-cleanup', `fixture exited with status ${previous.code ?? previous.signal}`);
    }
    return;
  }
  const orderlyStop = artifactAttached;
  if(orderlyStop) {
    try {
      writeFixtureCommand('stop');
    } catch {
      void terminateFixture(fixtureProcess, 'SIGTERM');
    }
  } else {
    void terminateFixture(fixtureProcess, 'SIGTERM');
  }

  let timer;
  const outcome = await Promise.race([
    fixtureExit.then(async (result) => ({
      ...result,
      processTreeStopped: await terminateFixture(fixtureProcess, 'SIGTERM')
    })),
    new Promise((resolvePromise) => { timer = setTimeout(() => resolvePromise(null), FIXTURE_CLEANUP_TIMEOUT_MS); })
  ]).finally(() => clearTimeout(timer));
  if(!outcome) {
    await terminate(fixtureProcess, 'SIGKILL');
    throw new FlowError('fixture-cleanup', 'fixture cleanup exceeded its time limit');
  }
  if(!outcome.processTreeStopped) throw new FlowError('fixture-cleanup', 'fixture process group did not stop');
  if(!fixtureCleanupVerified || (orderlyStop && outcome.code !== 0 && !allowNonzeroExit)) {
    throw new FlowError('fixture-cleanup', `fixture cleanup exited with status ${outcome.code ?? outcome.signal}`);
  }
}

function createDirectory(parent, prefix) {
  const directory = mkdtempSync(join(parent, prefix));
  chmodSync(directory, 0o700);
  return directory;
}

function formatDuration(milliseconds) {
  const seconds = Math.floor(milliseconds / 1000);
  return `${Math.floor(seconds / 60)}m ${String(seconds % 60).padStart(2, '0')}s`;
}

function writeOutputs(values) {
  const outputPath = process.env.GITHUB_OUTPUT;
  if(!outputPath) return;
  appendFileSync(outputPath, `${Object.entries(values).map(([key, value]) => `${key}=${String(value).replace(/[\r\n]/g, '')}`).join('\n')}\n`);
}

function writeSummary({status, serverRevision, webRevision, headRevision, testedRevision, runId, elapsedMs, cleanupVerified, traceReady, traceDirectory, summaryPath}) {
  const result = {
    status,
    runId,
    serverRevision,
    webRevision,
    pullRequestHead: headRevision,
    testedRevision,
    scenarios: SCENARIOS.split(','),
    elapsedMs,
    fixtureCleanup: cleanupVerified ? 'verified' : 'failed',
    traceArtifact: traceReady ? 'uploaded on this workflow run' : 'not produced'
  };
  writeFileSync(summaryPath, `${JSON.stringify(result, null, 2)}\n`, {mode: 0o600});

  const summaryPathForOutput = summaryPath;
  writeOutputs({
    status,
    trace_dir: traceDirectory,
    trace_ready: traceReady,
    summary_path: summaryPathForOutput,
    elapsed_ms: elapsedMs
  });

  if(process.env.GITHUB_STEP_SUMMARY) {
    const artifactLink = `${process.env.GITHUB_SERVER_URL}/${process.env.GITHUB_REPOSITORY}/actions/runs/${process.env.GITHUB_RUN_ID}#artifacts`;
    const markdown = [
      '## Real-client browser flow',
      '',
      `Result: **${status}**`,
      `Server revision: \`${serverRevision}\``,
      `Web revision: \`${webRevision}\``,
      `Pull request head: \`${headRevision}\``,
      `Tested revision: \`${testedRevision}\``,
      `Scenarios: \`${SCENARIOS}\``,
      `Elapsed through fixture cleanup: **${formatDuration(elapsedMs)}**`,
      `Fixture cleanup: **${cleanupVerified ? 'verified' : 'failed'}**`,
      traceReady ? `Playwright trace artifact: [open workflow artifacts](${artifactLink})` : 'Playwright trace artifact: not produced',
      ''
    ].join('\n');
    appendFileSync(process.env.GITHUB_STEP_SUMMARY, markdown);
  }
}

async function main() {
  const startedAt = Date.now();
  const serverRevision = requiredRevision('REAL_CLIENT_SERVER_REVISION');
  const webRevision = requiredRevision('REAL_CLIENT_WEB_REVISION');
  const headRevision = requiredRevision('REAL_CLIENT_PR_HEAD');
  const testedRevision = requiredRevision('REAL_CLIENT_TESTED_REVISION');
  const serverRoot = resolve(process.env.REAL_CLIENT_SERVER_ROOT || process.cwd());
  const webRoot = resolve(process.env.REAL_CLIENT_WEB_ROOT || process.cwd());
  const runId = randomBytes(16).toString('hex');
  const runTemp = createDirectory('/dev/shm', 'real-client-run-');
  const artifactParent = createDirectory(process.env.RUNNER_TEMP || tmpdir(), 'real-client-artifact-');
  const traceDirectory = join(process.env.RUNNER_TEMP || tmpdir(), `real-client-traces-${runId}`);
  const summaryPath = join(process.env.RUNNER_TEMP || tmpdir(), `real-client-summary-${runId}.json`);
  const readinessPath = join(runTemp, 'readiness.jsonl');
  const keyPath = join(runTemp, 'public-key.pem');
  const artifactDirectory = join(artifactParent, 'dist-private');
  mkdirSync(traceDirectory, {mode: 0o700});
  writeFileSync(readinessPath, '', {mode: 0o600});

  let status = 'failed';
  let failure;
  let cleanupVerified = false;
  let traceReady = false;
  let fixtureStarted = false;
  try {
    repositoryHead(serverRoot, serverRevision, 'server');
    repositoryHead(webRoot, webRevision, 'web');
    runStage = 'fixture-startup';

    const fixtureScript = join(serverRoot, 'test/e2e/real_server_fixture/run.sh');
    if(!existsSync(fixtureScript)) throw new FlowError(runStage, 'real-server fixture entrypoint is missing');
    fixtureProcess = spawnTracked('bash', [fixtureScript,
      '--server-revision', serverRevision,
      '--web-revision', webRevision,
      '--run-id', runId
    ], {cwd: serverRoot, env: cleanEnvironment(), stdio: ['pipe', 'pipe', 'pipe']});
    fixtureStarted = true;
    const output = createInterface({input: fixtureProcess.stdout});
    output.on('line', receiveFixtureEvent);
    let stderrBuffer = '';
    fixtureProcess.stderr.setEncoding('utf8');
    fixtureProcess.stderr.on('data', (chunk) => {
      process.stderr.write(chunk);
      stderrBuffer += chunk;
      const lines = stderrBuffer.split(/\r?\n/);
      stderrBuffer = lines.pop() || '';
      if(lines.includes('cleanup=verified')) fixtureCleanupVerified = true;
    });
    fixtureExit = new Promise((resolvePromise) => {
      fixtureProcess.once('close', (code, signal) => {
        fixtureWaiter?.reject(new FlowError(runStage, `fixture exited with status ${code ?? signal}`));
        fixtureWaiter = undefined;
        resolvePromise({code, signal});
      });
    });
    fixtureProcess.once('error', () => {
      fixtureWaiter?.reject(new FlowError(runStage, 'fixture process could not start'));
      fixtureWaiter = undefined;
    });

    const serverReady = await waitForFixtureEvent('server-ready', stageTimeout(startedAt, EVENT_TIMEOUTS.serverReady, 'fixture-startup'));
    if(serverReady.status !== 'ready' || serverReady.runId !== runId || serverReady.harnessRevision !== serverRevision ||
        serverReady.serverRevision !== serverRevision || serverReady.webRevision !== webRevision ||
        typeof serverReady.mtprotoPublicKeyPEM !== 'string' || !Array.isArray(serverReady.credentials) || serverReady.credentials.length !== 2) {
      throw new FlowError('fixture-startup', 'fixture server readiness did not match the requested revisions');
    }
    appendReadiness(readinessPath, serverReady);
    writeFileSync(keyPath, serverReady.mtprotoPublicKeyPEM, {mode: 0o600});

    runStage = 'private-artifact-build';
    const buildEnvironment = cleanEnvironment({
      MTPROTO_TARGET_MODE: 'private',
      MTPROTO_PRIVATE_ENDPOINT: 'wss://teagramd.test/apiws',
      MTPROTO_PRIVATE_RSA_PUBLIC_KEY_FILE: keyPath
    });
    await runCommand('corepack', ['pnpm', 'exec', 'vite', 'build', '--outDir', artifactDirectory], {
      cwd: webRoot,
      env: buildEnvironment,
      stage: runStage,
      timeoutMs: stageTimeout(startedAt, 4 * 60_000, runStage)
    });
    await runCommand('node', ['scripts/check-bundle-mangling.mjs', artifactDirectory], {
      cwd: webRoot,
      env: cleanEnvironment({MTPROTO_TARGET_MODE: 'private'}),
      stage: 'private-artifact-audit',
      timeoutMs: stageTimeout(startedAt, 60_000, 'private-artifact-audit')
    });

    runStage = 'fixture-artifact-readiness';
    writeFixtureCommand(`attach ${artifactDirectory}`);
    const artifactReady = await waitForFixtureEvent('artifact-ready', stageTimeout(startedAt, EVENT_TIMEOUTS.artifactReady, runStage));
    if(artifactReady.status !== 'ready' || artifactReady.runId !== runId || artifactReady.harnessRevision !== serverRevision ||
        artifactReady.serverRevision !== serverRevision || artifactReady.webRevision !== webRevision) {
      throw new FlowError(runStage, 'fixture artifact readiness did not match the requested revisions');
    }
    appendReadiness(readinessPath, artifactReady);
    artifactAttached = true;

    runStage = 'browser-scenarios';
    const runnerEnvironment = cleanEnvironment({REAL_CLIENT_TRACE_DIR: traceDirectory});
    let runnerError;
    try {
      await runCommand('corepack', ['pnpm', 'run', 'test:real-client', '--',
        '--readiness-file', readinessPath,
        '--harness-revision', serverRevision,
        '--server-revision', serverRevision,
        '--web-revision', webRevision,
        '--run-id', runId,
        '--scenarios', SCENARIOS
      ], {
        cwd: webRoot,
        env: runnerEnvironment,
        stage: runStage,
        timeoutMs: stageTimeout(startedAt, TOTAL_BUDGET_MS, runStage)
      });
    } catch(error) {
      runnerError = error;
    }

    const traceManifest = join(traceDirectory, 'trace-manifest.json');
    if(existsSync(traceManifest)) {
      const manifest = JSON.parse(readFileSync(traceManifest, 'utf8'));
      const traceFiles = readdirSync(traceDirectory).filter((name) => /^[a-z]+\.zip$/.test(name));
      if(manifest.status !== 'browser-assertion-failed' || manifest.runId !== runId ||
          manifest.harnessRevision !== serverRevision || manifest.serverRevision !== serverRevision ||
          manifest.webRevision !== webRevision || traceFiles.length === 0 ||
          JSON.stringify([...traceFiles].sort()) !== JSON.stringify(manifest.traces)) {
        throw new FlowError('browser-trace-validation', 'browser failure trace metadata is invalid');
      }
      traceReady = true;
    }
    if(runnerError) throw runnerError;
    status = 'passed';
  } catch(error) {
    failure = error instanceof FlowError ? error : new FlowError(runStage, 'real-client browser flow failed');
    process.stderr.write(`${failure.stage}: ${failure.message}\n`);
  } finally {
    if(fixtureStarted) {
      try {
        await stopFixture({allowNonzeroExit: Boolean(failure) || Boolean(requestedSignal)});
        cleanupVerified = true;
      } catch(error) {
        const cleanupError = error instanceof FlowError ? error : new FlowError('fixture-cleanup', 'fixture cleanup failed');
        if(!failure) failure = cleanupError;
        status = 'failed';
        process.stderr.write(`${cleanupError.stage}: ${cleanupError.message}\n`);
      }
    }
    if(requestedSignal) {
      status = 'failed';
      failure ||= new FlowError('cancelled', `workflow received ${requestedSignal}`);
    }
    const elapsedMs = Date.now() - startedAt;
    try {
      writeSummary({status, serverRevision, webRevision, headRevision, testedRevision, runId, elapsedMs,
        cleanupVerified, traceReady, traceDirectory, summaryPath});
    } catch {
      process.stderr.write('report: could not write the real-client run summary\n');
      status = 'failed';
    }
    rmSync(runTemp, {recursive: true, force: true});
    rmSync(artifactParent, {recursive: true, force: true});
  }

  if(status !== 'passed') process.exitCode = 1;
}

main().catch((error) => {
  process.stderr.write(`${error instanceof FlowError ? error.stage : 'real-client'}: ${error?.message || 'real-client browser flow failed'}\n`);
  process.exitCode = 1;
});
