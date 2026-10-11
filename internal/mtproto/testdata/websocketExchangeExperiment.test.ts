import {readFileSync, renameSync, unlinkSync, writeFileSync} from 'node:fs';
import {retainFirstAbort, runBoundedCase} from './websocketExchangeCaseLifecycle.mjs';
import {afterAll, expect, it, vi} from 'vitest';

const experimentState = vi.hoisted(() => ({
  target: undefined as any,
  randomState: 0x1a2b3c4d
}));

vi.mock('@config/mtprotoTarget', () => ({
  MTProtoTarget: experimentState.target,
  getMtprotoTarget: () => experimentState.target,
  isPrivateMtprotoTarget: () => experimentState.target?.mode === 'private',
  validateMtprotoTarget: (target: unknown) => target
}));

vi.mock('@helpers/array/randomize', () => ({
  default: (array: ArrayBufferView) => {
    const bytes = new Uint8Array(array.buffer, array.byteOffset, array.byteLength);
    for(let i = 0; i < bytes.length; ++i) {
      let value = experimentState.randomState >>> 0;
      value ^= value << 13;
      value ^= value >>> 17;
      value ^= value << 5;
      experimentState.randomState = value >>> 0;
      bytes[i] = value & 0xff;
    }
    return array;
  }
}));

vi.mock('@lib/accounts/accountController', () => ({
  default: {
    get: vi.fn(async() => ({})),
    update: vi.fn()
  }
}));

vi.mock('@lib/sessionStorage', () => ({
  default: {
    get: vi.fn(async() => undefined),
    set: vi.fn(async() => undefined)
  }
}));

type ClientStage = 'req_pq_sent' | 'res_pq_ok' | 'req_dh_sent' | 'server_dh_received' | 'server_dh_validated' | 'set_client_dh_sent' | 'dh_gen_verified' | 'unknown';
type AbortReason = 'server_dh_validation' | 'response_mismatch' | 'transport_closed' | 'timeout' | 'none' | 'unknown';
type Constructor = 'set_client_dh_params' | 'req_pq_multi' | 'req_pq' | 'req_dh_params' | 'other' | 'unknown';
type PreRequestFailureCategory = 'client_initialization' | 'transport_selection' | 'transport_construction' | 'unknown';
type ServerDhBoundaryResult = 'decrypt_entered' | 'not_received' | 'envelope_rejected' | 'constructor_rejected' | 'nonce_mismatch' | 'unknown';
type NodeWebSocketConstructor = new (
  address: string | URL,
  protocols?: string | string[],
  options?: {headers?: Record<string, string>}
) => WebSocket;
const WEB_EXCHANGE_TEST_ONLY_MARKER = 'teagram-web-authorizer-exchange-experiment-test-only';
const WEB_EXCHANGE_PRE_REQUEST_FAILURE_MARKER = 'synthetic_pre_request_failure';
const WEB_EXCHANGE_SERVER_DH_BOUNDARY_MARKER = 'synthetic_server_dh_boundary';

type ClientRow = {
  case: string,
  client_stage: ClientStage,
  client_abort_reason: AbortReason,
  plain_requests_on_socket: number,
  retry_reused_socket: boolean,
  third_constructor: Constructor,
  proto_error_404: boolean
};

type Config = {
  origin: string,
  public_key: {modulus: string, exponent: string, fingerprint: string},
  cases: Record<string, {endpoint: string}>
};

type SocketObservation = {
  transport: any,
  connection: object,
  requests: number,
  third: Constructor,
  closed: boolean,
  closeListener?: () => void,
};

type Observation = {
  stage: ClientStage,
  abort: AbortReason,
  abortObserved: boolean,
  retryReused: boolean,
  proto404: boolean,
  inClientDh: boolean,
  currentConstructor: Constructor,
  sockets: Map<object, SocketObservation>,
  retrySocket?: object,
};

type CaseScope = {
  name: string,
  observation: Observation,
  active: boolean,
  pending: Set<Promise<void>>,
  transports: Set<any>,
  timers: Map<any, () => void>,
  cleanup: Array<() => boolean | void>,
  record: () => void,
  track: <T>(promise: T) => T,
  addCleanup: (cleanup: () => boolean | void) => void,
  installTimerTracking: () => void,
  teardown: () => Promise<void>,
  isQuiescent: () => boolean
};

type ServerDhBoundaryMaterial = {
  received: boolean,
  complete: boolean,
  response?: Uint8Array,
  nonce?: Uint8Array,
  serverNonce?: Uint8Array,
  newNonce?: Uint8Array
};

type ServerDhBoundaryAttempt = {
  started: boolean,
  activeAuth?: any,
  liveDecryptEntered: boolean,
  randomStateBeforeDh?: number,
  material: ServerDhBoundaryMaterial
};

type ServerDhBoundaryResults = {
  actual: ServerDhBoundaryResult,
  control: ServerDhBoundaryResult
};

const CASES = ['A', 'B', 'C', 'N1', 'N2', 'N3'];
const SHARED_INITIALIZATION_BUDGET_MS = 10_000;
const CASE_EXECUTION_BUDGET_MS = 7_000;
const CASE_TEARDOWN_BUDGET_MS = 2_000;
// Setup plus six execution/teardown budgets total 64 seconds, within the 75-second client ceiling.
const nativeSetTimeout = globalThis.setTimeout.bind(globalThis);
const nativeClearTimeout = globalThis.clearTimeout.bind(globalThis);
let transportsToClose: any[] = [];

afterAll(() => {
  let cleanupFailed = false;
  for(const transport of transportsToClose) {
    try {
      transport.destroy();
    } catch{
      cleanupFailed = true;
    }
  }
  transportsToClose = [];
  vi.unstubAllGlobals();
  if(cleanupFailed) throw new Error('synthetic client fallback cleanup could not establish quiescence');
});

it('runs isolated shipped-Web exchanges against synthetic server instances', async() => {
  const rows: ClientRow[] = [];
  const failureMarkers = new Map<string, PreRequestFailureCategory>();
  const serverDhBoundary: ServerDhBoundaryResults = {actual: 'unknown', control: 'unknown'};
  const configPath = process.env.WEB_EXCHANGE_CONFIG_PATH;
  const clientRowsPath = process.env.WEB_EXCHANGE_CLIENT_ROWS_PATH;
  if(clientRowsPath) writeClientRowsSnapshot(clientRowsPath, rows);

  try {
    const initialization = await runBoundedCase({
      execute: async() => {
        let config: Config | undefined;
        let sharedDependencies: HarnessDependencies | undefined;
        try {
          expect(WEB_EXCHANGE_TEST_ONLY_MARKER).toBe('teagram-web-authorizer-exchange-experiment-test-only');
          expect(typeof configPath).toBe('string');
          expect(typeof clientRowsPath).toBe('string');
          const loadedConfig = JSON.parse(readFileSync(configPath!, 'utf8')) as Config;
          expect(loadedConfig.origin).toBe('https://exchange.synthetic.invalid');
          expect(Object.keys(loadedConfig.cases).sort()).toEqual(CASES.slice().sort());
          for(const name of CASES) assertLoopbackEndpoint(loadedConfig.cases[name].endpoint);

          const {installNodeEnv} = await import('./api/nodeEnv');
          installNodeEnv();
          setTarget(loadedConfig, loadedConfig.cases.A.endpoint);

          const NodeWebSocket = globalThis.WebSocket as unknown as NodeWebSocketConstructor;
          class OriginWebSocket extends NodeWebSocket {
            constructor(address: string | URL, protocols?: string | string[]) {
              super(address, protocols, {headers: {Origin: loadedConfig.origin}});
            }
          }
          vi.stubGlobal('WebSocket', OriginWebSocket);

          await import('../lib/crypto/crypto.worker');
          const [authorizerModule, configuratorModule, timeManagerModule, tlModule, cryptoModule, apiManagerModule] = await Promise.all([
            import('@lib/mtproto/authorizer'),
            import('@lib/mtproto/dcConfigurator'),
            import('@lib/mtproto/timeManager'),
            import('@lib/mtproto/tl_utils'),
            import('@lib/crypto/cryptoMessagePort'),
            import('@lib/appManagers/apiManager')
          ]);
          const Authorizer = authorizerModule.Authorizer;
          const DcConfigurator = configuratorModule.DcConfigurator;
          const TimeManager = timeManagerModule.TimeManager;
          const CryptoWorker = cryptoModule.default as any;
          const ApiManager = apiManagerModule.ApiManager;
          const prefixes = await requestPrefixes(tlModule.TLSerialization);
          config = loadedConfig;
          sharedDependencies = {Authorizer, DcConfigurator, TimeManager, CryptoWorker, ApiManager, prefixes};
        } catch{
          for(const name of CASES) failureMarkers.set(name, 'client_initialization');
        }
        return {config, sharedDependencies};
      },
      onTimeout: () => {},
      teardown: () => {},
      isQuiescent: () => transportsToClose.length === 0,
      executionBudgetMs: SHARED_INITIALIZATION_BUDGET_MS,
      teardownBudgetMs: 1_000
    });
    if(initialization.status !== 'completed') throw new Error('synthetic shared client initialization exceeded its bounded lifecycle');
    const {config, sharedDependencies} = initialization.value;

    if(config && sharedDependencies) {
      await runClientCase('A', rows, clientRowsPath, (scope) => runAuthorizerCase(scope, config, config.cases.A.endpoint, {
        ...sharedDependencies,
        recordServerDhBoundary: (actual, control) => {
          if(scope.active) {
            serverDhBoundary.actual = actual;
            serverDhBoundary.control = control;
          }
        }
      }, failureMarkers));
      await runClientCase('B', rows, clientRowsPath, (scope) => runAuthorizerCase(scope, config, config.cases.B.endpoint, {...sharedDependencies, injectRetryAbort: true}, failureMarkers));
      await runClientCase('C', rows, clientRowsPath, (scope) => runApiManagerCase(scope, config, config.cases.C.endpoint, sharedDependencies, failureMarkers));
      await runClientCase('N1', rows, clientRowsPath, (scope) => runAuthorizerCase(scope, config, config.cases.N1.endpoint, {...sharedDependencies, nextReqPQ: true}, failureMarkers));
      await runClientCase('N2', rows, clientRowsPath, (scope) => runAuthorizerCase(scope, config, config.cases.N2.endpoint, {...sharedDependencies, wrongTemporaryAes: true}, failureMarkers));
      await runClientCase('N3', rows, clientRowsPath, (scope) => runAuthorizerCase(scope, config, config.cases.N3.endpoint, {...sharedDependencies, corruptClientHash: true}, failureMarkers));
    }

    expect(rows).toHaveLength(6);
    expect(failureMarkers.size).toBe(0);
    expect(rowFor(rows, 'B').retry_reused_socket).toBe(true);
    expect(rowFor(rows, 'B').third_constructor).toBe('req_pq_multi');
    expect(rowFor(rows, 'N1').third_constructor).toBe('req_pq_multi');
    expect(rowFor(rows, 'N1').proto_error_404).toBe(false);
    expect(rowFor(rows, 'N2').proto_error_404).toBe(true);
    expect(rowFor(rows, 'N3').proto_error_404).toBe(true);
    expect(serverDhBoundary.control).toBe('decrypt_entered');
  } finally {
    try {
      writeClientRowsSnapshot(clientRowsPath, rows);
    } finally {
      process.stdout.write(`${WEB_EXCHANGE_SERVER_DH_BOUNDARY_MARKER} subject=actual result=${serverDhBoundary.actual}\n`);
      process.stdout.write(`${WEB_EXCHANGE_SERVER_DH_BOUNDARY_MARKER} subject=control result=${serverDhBoundary.control}\n`);
      for(const name of CASES) {
        const category = failureMarkers.get(name);
        if(category) process.stdout.write(`${WEB_EXCHANGE_PRE_REQUEST_FAILURE_MARKER} case=${name} category=${category}\n`);
      }
    }
  }
}, 72_000);

async function runClientCase(
  name: string,
  rows: ClientRow[],
  rowsPath: string | undefined,
  execute: (scope: CaseScope) => Promise<ClientRow>
): Promise<ClientRow> {
  const scope = newCaseScope(name, rows, rowsPath);
  const result = await runBoundedCase({
    execute: () => {
      scope.installTimerTracking();
      return execute(scope);
    },
    onTimeout: () => {
      scope.active = false;
      retainFirstAbort(scope.observation, 'timeout');
      scope.record();
    },
    teardown: () => scope.teardown(),
    isQuiescent: () => scope.isQuiescent(),
    executionBudgetMs: CASE_EXECUTION_BUDGET_MS,
    teardownBudgetMs: CASE_TEARDOWN_BUDGET_MS
  });
  if(result.status !== 'completed') {
    scope.record();
    const failure = result.status === 'timed_out' ? 'synthetic client case exceeded its execution deadline' :
      result.status === 'teardown_failed' ? 'synthetic client case teardown did not establish quiescence' :
        'synthetic client case failed before completion';
    throw new Error(failure);
  }
  const row = result.value as ClientRow;
  const existingIndex = rows.findIndex((current) => current.case === name);
  if(existingIndex >= 0) rows[existingIndex] = row;
  else rows.push(row);
  writeClientRowsSnapshot(rowsPath, rows);
  return row;
}

function newCaseScope(name: string, rows: ClientRow[], rowsPath: string | undefined): CaseScope {
  const scope: CaseScope = {
    name,
    observation: newObservation(),
    active: true,
    pending: new Set<Promise<void>>(),
    transports: new Set<any>(),
    timers: new Map<any, () => void>(),
    cleanup: [] as Array<() => boolean | void>,
    record: () => {},
    track: <T>(promise: T): T => promise,
    addCleanup: (_cleanup: () => boolean | void) => {},
    installTimerTracking: () => {},
    teardown: async(): Promise<void> => {},
    isQuiescent: () => false
  } satisfies CaseScope;

  scope.record = () => {
    const row = toClientRow(name, scope.observation);
    const index = rows.findIndex((current) => current.case === name);
    if(index >= 0) rows[index] = row;
    else rows.push(row);
    writeClientRowsSnapshot(rowsPath, rows);
  };
  scope.track = <T>(promise: T): T => {
    if(!promise || typeof (promise as any).then !== 'function') return promise;
    const pending: Promise<void> = Promise.resolve(promise).then((): void => undefined, (): void => undefined);
    scope.pending.add(pending);
    pending.then(() => scope.pending.delete(pending));
    return promise;
  };
  scope.addCleanup = (cleanup) => { scope.cleanup.push(cleanup); };
  scope.installTimerTracking = () => {
    const originalSetTimeout = globalThis.setTimeout;
    const originalClearTimeout = globalThis.clearTimeout;
    const originalSetInterval = globalThis.setInterval;
    const originalClearInterval = globalThis.clearInterval;
    const trackedSetTimeout = (callback: (...args: any[]) => any, delay?: number, ...args: any[]) => {
      const handle = originalSetTimeout((...callbackArgs: any[]) => {
        scope.timers.delete(handle);
        if(scope.active) {
          const result = callback(...(callbackArgs.length ? callbackArgs : args));
          if(result && typeof result.then === 'function') scope.track(result);
        }
      }, delay);
      scope.timers.set(handle, () => originalClearTimeout(handle));
      return handle;
    };
    const trackedSetInterval = (callback: (...args: any[]) => any, delay?: number, ...args: any[]) => {
      const handle = originalSetInterval((...callbackArgs: any[]) => {
        if(scope.active) {
          const result = callback(...(callbackArgs.length ? callbackArgs : args));
          if(result && typeof result.then === 'function') scope.track(result);
        }
      }, delay);
      scope.timers.set(handle, () => originalClearInterval(handle));
      return handle;
    };
    const trackedClear = (handle: any, originalClear: (handle: any) => void) => {
      const clear = scope.timers.get(handle);
      if(clear) {
        scope.timers.delete(handle);
        clear();
      } else {
        originalClear(handle);
      }
    };
    (globalThis as any).setTimeout = trackedSetTimeout;
    (globalThis as any).clearTimeout = (handle: any) => trackedClear(handle, originalClearTimeout);
    (globalThis as any).setInterval = trackedSetInterval;
    (globalThis as any).clearInterval = (handle: any) => trackedClear(handle, originalClearInterval);
    scope.addCleanup(() => {
      (globalThis as any).setTimeout = originalSetTimeout;
      (globalThis as any).clearTimeout = originalClearTimeout;
      (globalThis as any).setInterval = originalSetInterval;
      (globalThis as any).clearInterval = originalClearInterval;
      let success = true;
      for(const clear of scope.timers.values()) {
        try {
          clear();
        } catch{
          success = false;
        }
      }
      scope.timers.clear();
      return success;
    });
  };
  scope.teardown = async() => {
    scope.active = false;
    let success = true;
    for(const cleanup of scope.cleanup.splice(0).reverse()) {
      try {
        if(cleanup() === false) success = false;
      } catch{
        success = false;
      }
    }
    const closeResults = await Promise.all([...scope.transports].map((transport) => closeObservedTransport(scope, transport)));
    if(closeResults.some((result) => !result)) success = false;
    await Promise.all([...scope.pending]);
    if(!success) throw new Error('synthetic client resources did not close cleanly');
  };
  scope.isQuiescent = () => scope.pending.size === 0 && scope.transports.size === 0 && scope.timers.size === 0;
  scope.record();
  return scope;
}

async function closeObservedTransport(scope: CaseScope, transport: any): Promise<boolean> {
  const connection = transport.connection;
  const observedSockets = [...scope.observation.sockets.values()].filter((candidate) => candidate.transport === transport);
  const socket = observedSockets.find((candidate) => candidate.connection === connection);
  let closed = connection ? socket?.closed === true || connection.readyState === 3 : observedSockets.length > 0 && observedSockets.every((candidate) => candidate.closed);
  let closeListener: (() => void) | undefined;
  let closeTimer: ReturnType<typeof setTimeout> | undefined;
  let closeResolve: (() => void) | undefined;
  let canObserveClose = closed;
  const closePromise = new Promise<void>((resolve) => { closeResolve = resolve; });
  if(connection && !closed && typeof connection.addEventListener === 'function') {
    canObserveClose = true;
    closeListener = () => {
      closed = true;
      closeResolve?.();
    };
    connection.addEventListener('close', closeListener, {once: true});
  } else if(connection && !closed && typeof connection.on === 'function') {
    canObserveClose = true;
    closeListener = () => {
      closed = true;
      closeResolve?.();
    };
    connection.once('close', closeListener);
  }

  let destroySucceeded = true;
  try {
    transport.destroy();
  } catch{
    destroySucceeded = false;
  }
  if(canObserveClose && connection && !closed) {
    closeTimer = nativeSetTimeout(() => closeResolve?.(), 1_500);
    await closePromise;
    nativeClearTimeout(closeTimer);
  }
  for(const observedSocket of observedSockets) {
    const observedConnection = observedSocket.connection as any;
    if(!observedSocket.closeListener) continue;
    if(typeof observedConnection.removeEventListener === 'function') observedConnection.removeEventListener('close', observedSocket.closeListener);
    else if(typeof observedConnection.off === 'function') observedConnection.off('close', observedSocket.closeListener);
    else if(typeof observedConnection.removeListener === 'function') observedConnection.removeListener('close', observedSocket.closeListener);
    observedSocket.closeListener = undefined;
  }
  if(closeListener) {
    if(typeof connection.removeEventListener === 'function') connection.removeEventListener('close', closeListener);
    else if(typeof connection.off === 'function') connection.off('close', closeListener);
    else if(typeof connection.removeListener === 'function') connection.removeListener('close', closeListener);
  }
  if(closed && destroySucceeded) {
    scope.transports.delete(transport);
    transportsToClose = transportsToClose.filter((current) => current !== transport);
  }
  return closed && destroySucceeded;
}

function writeClientRowsSnapshot(path: string | undefined, rows: ClientRow[]) {
  if(!path) return;
  const tempPath = `${path}.tmp`;
  try {
    writeFileSync(tempPath, JSON.stringify(rows), {mode: 0o600});
    renameSync(tempPath, path);
  } catch{
    try {
      unlinkSync(tempPath);
    } catch{}
    throw new Error('synthetic client observation snapshot could not be persisted');
  }
}

type HarnessDependencies = {
  Authorizer: any,
  DcConfigurator: any,
  TimeManager: any,
  CryptoWorker: any,
  ApiManager?: any,
  prefixes: Record<string, Uint8Array>,
  injectRetryAbort?: boolean,
  nextReqPQ?: boolean,
  wrongTemporaryAes?: boolean,
  corruptClientHash?: boolean
  recordServerDhBoundary?: (actual: ServerDhBoundaryResult, control: ServerDhBoundaryResult) => void
};

async function runAuthorizerCase(
  scope: CaseScope,
  config: Config,
  endpoint: string,
  dependencies: HarnessDependencies,
  failureMarkers: Map<string, PreRequestFailureCategory>
): Promise<ClientRow> {
  const name = scope.name;
  const observation = scope.observation;
  const boundaryAttempt = name === 'A' && dependencies.recordServerDhBoundary ? newServerDhBoundaryAttempt() : undefined;
  if(boundaryAttempt) scope.addCleanup(() => disposeServerDhBoundaryMaterial(boundaryAttempt.material));
  let authorizer: any;
  try {
    setTarget(config, endpoint);
    const configurator = new dependencies.DcConfigurator();
    const timeManager = new dependencies.TimeManager();
    authorizer = new dependencies.Authorizer({timeManager, dcConfigurator: configurator}) as any;
    instrumentAuthorizer(authorizer, scope, dependencies.prefixes, failureMarkers, boundaryAttempt);

    if(dependencies.injectRetryAbort) {
      const originalSendClientDhParams = authorizer.sendSetClientDhParams.bind(authorizer);
      let injected = false;
      authorizer.sendSetClientDhParams = async(auth: any) => {
        if(!scope.active) return Promise.reject(undefined);
        if(!injected) {
          injected = true;
          if(observation.stage !== 'server_dh_validated') {
            throw new Error('synthetic retry injection preceded server-DH validation');
          }
          retainFirstAbort(observation, 'unknown');
          throw new Error('synthetic post-validation abort');
        }
        return originalSendClientDhParams(auth);
      };
      scope.addCleanup(() => { authorizer.sendSetClientDhParams = originalSendClientDhParams; });
    }
    if(dependencies.nextReqPQ) {
      authorizer.sendSetClientDhParams = async(auth: any) => {
        if(!scope.active) return Promise.reject(undefined);
        observation.currentConstructor = 'req_pq_multi';
        observation.stage = 'req_pq_sent';
        scope.record();
        const {TLSerialization} = await import('@lib/mtproto/tl_utils');
        const request = new TLSerialization({mtproto: true});
        request.storeMethod('req_pq_multi', {nonce: auth.nonce});
        return authorizer.sendPlainRequest(auth.transport, request.getBytes(true));
      };
    }
    if(dependencies.wrongTemporaryAes) {
      const originalSet = authorizer.sendSetClientDhParams.bind(authorizer);
      let modifiedAuth: any;
      let originalKey: Uint8Array | undefined;
      scope.addCleanup(() => {
        if(modifiedAuth) modifiedAuth.tmpAesKey = originalKey;
      });
      authorizer.sendSetClientDhParams = async(auth: any) => {
        if(!scope.active) return Promise.reject(undefined);
        modifiedAuth = auth;
        originalKey = auth.tmpAesKey;
        auth.tmpAesKey = Uint8Array.from(originalKey, (value: number) => value ^ 0xff);
        try {
          return await originalSet(auth);
        } finally {
          auth.tmpAesKey = originalKey;
          modifiedAuth = undefined;
        }
      };
    }
    if(dependencies.corruptClientHash) {
      const worker = dependencies.CryptoWorker as Record<string, any>;
      const originalInvoke = worker.invokeCrypto.bind(worker);
      let corrupted = false;
      worker.invokeCrypto = async(method: string, ...args: any[]) => {
        const result = await scope.track(originalInvoke(method, ...args));
        if(scope.active && observation.inClientDh && method === 'sha1' && !corrupted) {
          corrupted = true;
          const damaged = Uint8Array.from(result as Uint8Array);
          damaged[0] ^= 0xff;
          return damaged;
        }
        return result;
      };
      scope.addCleanup(() => { worker.invokeCrypto = originalInvoke; });
    }
  } catch{
    if(scope.active) {
      recordPreRequestFailure(name, 'client_initialization', failureMarkers, observation);
      retainFirstAbort(observation, 'unknown');
      scope.record();
      if(boundaryAttempt) dependencies.recordServerDhBoundary?.('not_received', 'unknown');
    }
    return toClientRow(name, observation);
  }

  let complete = false;
  try {
    await authorizer.getTransportType();
  } catch{
    if(scope.active) {
      recordPreRequestFailure(name, 'transport_selection', failureMarkers, observation);
      retainFirstAbort(observation, 'unknown');
      scope.record();
    }
  }
  if(scope.active && !failureMarkers.has(name)) {
    try {
      if(name.startsWith('N')) {
        await authorizer.__auth(2, false);
      } else {
        await authorizer.auth(2, false);
        complete = true;
      }
    } catch{
      if(scope.active) {
        recordPreRequestFailure(name, 'unknown', failureMarkers, observation);
        retainFirstAbort(observation, observation.proto404 ? 'response_mismatch' : 'unknown');
        scope.record();
      }
    }
  }
  if(scope.active && complete) {
    observation.stage = 'dh_gen_verified';
    if(!observation.abortObserved && !observation.proto404) observation.abort = 'none';
  } else if(scope.active && observation.proto404) {
    retainFirstAbort(observation, 'response_mismatch');
  }
  if(boundaryAttempt && scope.active) {
    const result = await attributeFirstServerDhBoundary(scope, boundaryAttempt, dependencies, config);
    if(scope.active) dependencies.recordServerDhBoundary?.(result.actual, result.control);
  }
  if(scope.active) scope.record();
  return toClientRow(name, observation);
}

function newServerDhBoundaryAttempt(): ServerDhBoundaryAttempt {
  return {
    started: false,
    liveDecryptEntered: false,
    material: {received: false, complete: false}
  };
}

function copyBoundaryBytes(value: unknown): Uint8Array | undefined {
  if(!(value instanceof Uint8Array)) return undefined;
  return Uint8Array.from(value);
}

function captureServerDhBoundaryMaterial(attempt: ServerDhBoundaryAttempt, auth: any, response: unknown) {
  const material = attempt.material;
  material.received = true;
  if(!(response instanceof Uint8Array)) return;

  let responseCopy: Uint8Array | undefined;
  let nonce: Uint8Array | undefined;
  let serverNonce: Uint8Array | undefined;
  let newNonce: Uint8Array | undefined;
  try {
    responseCopy = copyBoundaryBytes(response);
    nonce = copyBoundaryBytes(auth?.nonce);
    serverNonce = copyBoundaryBytes(auth?.serverNonce);
    newNonce = copyBoundaryBytes(auth?.newNonce);
    if(!responseCopy || !nonce || !serverNonce || !newNonce) return;

    material.response = responseCopy;
    material.nonce = nonce;
    material.serverNonce = serverNonce;
    material.newNonce = newNonce;
    material.complete = true;
  } catch{
    material.complete = false;
  } finally {
    if(!material.complete) {
      responseCopy?.fill(0);
      nonce?.fill(0);
      serverNonce?.fill(0);
      newNonce?.fill(0);
    }
  }
}

function disposeServerDhBoundaryMaterial(material: ServerDhBoundaryMaterial): boolean {
  material.response?.fill(0);
  material.nonce?.fill(0);
  material.serverNonce?.fill(0);
  material.newNonce?.fill(0);
  material.response = undefined;
  material.nonce = undefined;
  material.serverNonce = undefined;
  material.newNonce = undefined;
  material.received = false;
  material.complete = false;
  return true;
}

type ServerDhBoundaryAssessment = {
  result: ServerDhBoundaryResult,
  helperExpected?: boolean
};

async function attributeFirstServerDhBoundary(
  scope: CaseScope,
  attempt: ServerDhBoundaryAttempt,
  dependencies: HarnessDependencies,
  config: Config
): Promise<ServerDhBoundaryResults> {
  const material = attempt.material;
  if(!material.received) return {actual: 'not_received', control: 'unknown'};
  if(!material.complete || !material.response || !material.nonce || !material.serverNonce || !material.newNonce) {
    return {actual: 'unknown', control: 'unknown'};
  }

  const savedRandomState = experimentState.randomState;
  let controlResponse: Uint8Array | undefined;
  try {
    const [{TLDeserialization, TLSerialization}, bytesCmpModule] = await Promise.all([
      import('@lib/mtproto/tl_utils'),
      import('@helpers/bytes/bytesCmp')
    ]);
    const bytesCmp = bytesCmpModule.default;
    const actualAssessment = assessServerDhBoundaryResponse(
      material.response,
      material.nonce,
      material.serverNonce,
      TLDeserialization,
      bytesCmp
    );
    experimentState.randomState = attempt.randomStateBeforeDh ?? savedRandomState;
    const actualReplay = replayServerDhBoundaryResponse(
      material.response,
      material.nonce,
      material.serverNonce,
      material.newNonce,
      dependencies,
      config
    );
    scope.track(actualReplay);
    const actualHelperEntered = await actualReplay;
    const actual = resolveServerDhBoundaryResult(actualAssessment, actualHelperEntered, attempt.liveDecryptEntered);

    controlResponse = createServerDhBoundaryControlResponse(TLSerialization, material.nonce, material.serverNonce);
    const controlAssessment = assessServerDhBoundaryResponse(
      controlResponse,
      material.nonce,
      material.serverNonce,
      TLDeserialization,
      bytesCmp
    );
    experimentState.randomState = attempt.randomStateBeforeDh ?? savedRandomState;
    const controlReplay = replayServerDhBoundaryResponse(
      controlResponse,
      material.nonce,
      material.serverNonce,
      material.newNonce,
      dependencies,
      config
    );
    scope.track(controlReplay);
    const controlHelperEntered = await controlReplay;
    const control = controlAssessment.result === 'decrypt_entered' && controlHelperEntered ? 'decrypt_entered' : 'unknown';
    return {actual: control === 'decrypt_entered' ? actual : 'unknown', control};
  } catch{
    return {actual: 'unknown', control: 'unknown'};
  } finally {
    experimentState.randomState = savedRandomState;
    controlResponse?.fill(0);
  }
}

function assessServerDhBoundaryResponse(
  responseBytes: Uint8Array,
  nonce: Uint8Array,
  serverNonce: Uint8Array,
  TLDeserialization: any,
  bytesCmp: (left: Uint8Array, right: Uint8Array) => boolean
): ServerDhBoundaryAssessment {
  if(!responseBytes.byteLength) return {result: 'envelope_rejected', helperExpected: false};
  if(responseBytes.byteLength === 4) return {result: 'envelope_rejected', helperExpected: false};

  let deserializer: any;
  try {
    deserializer = new TLDeserialization(responseBytes, {mtproto: true});
    deserializer.fetchLong('auth_key_id');
    deserializer.fetchLong('msg_id');
    deserializer.fetchInt('msg_len');
  } catch{
    return {result: 'envelope_rejected', helperExpected: false};
  }

  let response: any;
  try {
    response = deserializer.fetchObject('Server_DH_Params', 'RESPONSE');
    if(!response || (response._ !== 'server_DH_params_ok' && response._ !== 'server_DH_params_fail')) {
      return {result: 'constructor_rejected', helperExpected: false};
    }
  } catch{
    return {result: 'constructor_rejected', helperExpected: false};
  }

  try {
    if(!bytesCmp(nonce, response.nonce) || !bytesCmp(serverNonce, response.server_nonce)) {
      return {result: 'nonce_mismatch', helperExpected: false};
    }
  } catch{
    return {result: 'unknown'};
  }

  if(response._ !== 'server_DH_params_ok') return {result: 'unknown'};
  return {result: 'decrypt_entered', helperExpected: true};
}

function resolveServerDhBoundaryResult(
  assessment: ServerDhBoundaryAssessment,
  replayHelperEntered: boolean,
  liveHelperEntered: boolean
): ServerDhBoundaryResult {
  if(assessment.helperExpected === undefined || replayHelperEntered !== assessment.helperExpected || replayHelperEntered !== liveHelperEntered) {
    return 'unknown';
  }
  return assessment.result;
}

function createServerDhBoundaryControlResponse(TLSerialization: any, nonce: Uint8Array, serverNonce: Uint8Array): Uint8Array {
  const body = new TLSerialization({mtproto: true});
  body.storeObject({
    _: 'server_DH_params_ok',
    nonce,
    server_nonce: serverNonce,
    encrypted_answer: new Uint8Array([0x53, 0x59, 0x4e, 0x54])
  }, 'Server_DH_Params', 'synthetic_control');
  const bodyBytes = body.getBytes(true) as Uint8Array;

  const header = new TLSerialization({mtproto: true});
  header.storeLongP(0, 0, 'auth_key_id');
  header.storeLong(4, 'msg_id');
  header.storeInt(bodyBytes.byteLength, 'msg_len');
  const headerBytes = header.getBytes(true) as Uint8Array;
  const result = new Uint8Array(headerBytes.byteLength + bodyBytes.byteLength);
  result.set(headerBytes);
  result.set(bodyBytes, headerBytes.byteLength);
  bodyBytes.fill(0);
  headerBytes.fill(0);
  return result;
}

async function replayServerDhBoundaryResponse(
  response: Uint8Array,
  nonceValue: Uint8Array,
  serverNonceValue: Uint8Array,
  newNonceValue: Uint8Array,
  dependencies: HarnessDependencies,
  config: Config
): Promise<boolean> {
  const nonce = Uint8Array.from(nonceValue);
  const serverNonce = Uint8Array.from(serverNonceValue);
  const newNonce = Uint8Array.from(newNonceValue);
  const pq = new Uint8Array([0x0f, 0x35]);
  const p = new Uint8Array([0x03]);
  const q = new Uint8Array([0x05]);
  let deliveredResponse: Uint8Array | undefined;
  const transport = {
    send: () => {
      deliveredResponse = Uint8Array.from(response);
      return Promise.resolve(deliveredResponse);
    }
  };
  const configurator = new dependencies.DcConfigurator();
  const authorizer = new dependencies.Authorizer({
    timeManager: new dependencies.TimeManager(),
    dcConfigurator: configurator
  }) as any;
  const silentLog: any = () => {};
  silentLog.error = () => {};
  authorizer.log = silentLog;
  let helperEntered = false;
  authorizer.decryptServerDhDataAnswer = async() => {
    helperEntered = true;
    throw undefined;
  };
  const auth = {
    dcId: 2,
    nonce,
    serverNonce,
    newNonce,
    temp: false,
    media: false,
    transport,
    pq,
    p,
    q,
    publicKey: config.public_key
  };
  try {
    await authorizer.sendReqDhParams(auth);
  } catch{}
  finally {
    deliveredResponse?.fill(0);
    nonce.fill(0);
    serverNonce.fill(0);
    newNonce.fill(0);
    pq.fill(0);
    p.fill(0);
    q.fill(0);
  }
  return helperEntered;
}

async function runApiManagerCase(
  scope: CaseScope,
  config: Config,
  endpoint: string,
  dependencies: HarnessDependencies,
  failureMarkers: Map<string, PreRequestFailureCategory>
): Promise<ClientRow> {
  const observation = scope.observation;
  let manager: any;
  try {
    setTarget(config, endpoint);
    const configurator = new dependencies.DcConfigurator();
    const timeManager = new dependencies.TimeManager();
    const authorizer = new dependencies.Authorizer({timeManager, dcConfigurator: configurator}) as any;
    instrumentAuthorizer(authorizer, scope, dependencies.prefixes, failureMarkers);

    manager = Object.create(dependencies.ApiManager.prototype) as any;
    Object.assign(manager, {
      cachedNetworkers: {},
      gettingNetworkers: {},
      baseDcId: 0,
      transportType: 'websocket',
      dcConfigurator: configurator,
      authorizer,
      timeManager,
      networkerFactory: {getNetworker: () => ({})},
      rootScope: {premium: false}
    });
    manager.getAccountNumber = () => 0;
    manager.changeNetworkerTransport = () => {};
    manager.setOnDrainIfNeeded = () => {};
  } catch{
    recordPreRequestFailure('C', 'client_initialization', failureMarkers, observation);
    retainFirstAbort(observation, 'unknown');
    scope.record();
    return toClientRow('C', observation);
  }

  let complete = false;
  try {
    await manager.getNetworker(2);
    complete = true;
  } catch{
    recordPreRequestFailure('C', 'unknown', failureMarkers, observation);
    retainFirstAbort(observation, observation.proto404 ? 'response_mismatch' : 'unknown');
    scope.record();
  }
  if(complete) {
    observation.stage = 'dh_gen_verified';
    if(!observation.abortObserved && !observation.proto404) observation.abort = 'none';
  } else if(observation.proto404) {
    retainFirstAbort(observation, 'response_mismatch');
  }
  scope.record();
  return toClientRow('C', observation);
}

function instrumentAuthorizer(
  authorizer: any,
  scope: CaseScope,
  prefixes: Record<string, Uint8Array>,
  failureMarkers: Map<string, PreRequestFailureCategory>,
  boundaryAttempt?: ServerDhBoundaryAttempt
) {
  const observation = scope.observation;
  const caseName = scope.name;
  const attemptsByTransport = new WeakMap<object, {temp: boolean, count: number}>();
  const originalSendPlain = authorizer.sendPlainRequest.bind(authorizer);
  authorizer.sendPlainRequest = (transport: object, request: Uint8Array) => {
    if(!scope.active) return Promise.reject(undefined);
    const socket = observeTransport(transport, scope);
    const constructor = classifyConstructor(request, prefixes);
    socket.requests = Math.min(socket.requests + 1, 12);
    if(socket.requests === 3) socket.third = constructor;
    observation.currentConstructor = constructor;
    if(constructor === 'req_pq_multi' || constructor === 'req_pq') observation.stage = 'req_pq_sent';
    if(constructor === 'req_dh_params') observation.stage = 'req_dh_sent';
    if(constructor === 'set_client_dh_params') observation.stage = 'set_client_dh_sent';
    scope.record();
    const activeAuth = boundaryAttempt?.activeAuth;
    if(boundaryAttempt && activeAuth && constructor === 'req_dh_params' && !boundaryAttempt.material.received) {
      const send = (transport as any).send;
      if(typeof send === 'function') {
        let captured = false;
        const captureSend = (body: Uint8Array) => {
          let response: any;
          try {
            response = send(body);
          } catch{
            return Promise.reject(undefined);
          }
          return Promise.resolve(response).then((result: Uint8Array) => {
            if(!captured && scope.active) {
              captured = true;
              captureServerDhBoundaryMaterial(boundaryAttempt, activeAuth, result);
            }
            return result;
          }, () => Promise.reject(undefined));
        };
        (transport as any).send = captureSend;
        let pending: Promise<any>;
        try {
          pending = Promise.resolve(originalSendPlain(transport, request));
        } catch{
          if((transport as any).send === captureSend) (transport as any).send = send;
          return Promise.reject(undefined);
        }
        return scope.track(pending.finally(() => {
          if((transport as any).send === captureSend) (transport as any).send = send;
        }));
      }
    }
    try {
      return scope.track(originalSendPlain(transport, request));
    } catch{
      return Promise.reject(undefined);
    }
  };

  const originalDecrypt = authorizer.decryptServerDhDataAnswer.bind(authorizer);
  authorizer.decryptServerDhDataAnswer = async(...args: any[]) => {
    if(scope.active) {
      observation.stage = 'server_dh_received';
      if(boundaryAttempt?.activeAuth) boundaryAttempt.liveDecryptEntered = true;
      scope.record();
    }
    const result = await originalDecrypt(...args);
    if(scope.active) {
      observation.stage = 'server_dh_validated';
      scope.record();
    }
    return result;
  };

  // _auth retries this boundary, so retain its first safe failure before the retry.
  const originalSendReqDhParams = authorizer.sendReqDhParams.bind(authorizer);
  authorizer.sendReqDhParams = async(...args: any[]) => {
    const auth = args[0];
    const attempt = auth?.transport && attemptsByTransport.get(auth.transport);
    const captureThisAttempt = Boolean(boundaryAttempt && !boundaryAttempt.started && attempt?.temp === false && attempt?.count === 1);
    if(captureThisAttempt && boundaryAttempt) {
      boundaryAttempt.started = true;
      boundaryAttempt.activeAuth = auth;
      boundaryAttempt.randomStateBeforeDh = experimentState.randomState;
    }
    try {
      return await originalSendReqDhParams(...args);
    } catch{
      if(scope.active) {
        retainFirstAbort(observation, observation.stage === 'server_dh_received' ? 'server_dh_validation' : 'unknown');
        scope.record();
      }
      throw undefined;
    } finally {
      if(captureThisAttempt && boundaryAttempt) boundaryAttempt.activeAuth = undefined;
    }
  };

  const originalSet = authorizer.sendSetClientDhParams.bind(authorizer);
  authorizer.sendSetClientDhParams = async(...args: any[]) => {
    if(!scope.active) return Promise.reject(undefined);
    observation.inClientDh = true;
    observation.stage = 'set_client_dh_sent';
    scope.record();
    try {
      const result = await originalSet(...args);
      if(scope.active) {
        observation.stage = 'dh_gen_verified';
        scope.record();
      }
      return result;
    } finally {
      if(scope.active) {
        observation.inClientDh = false;
        scope.record();
      }
    }
  };

  const originalAuthAttempt = authorizer.__auth.bind(authorizer);
  authorizer.__auth = (dcId: number, temp: boolean) => {
    const prior = (observation as any).attempts as Map<boolean, {count: number, previous?: object}> | undefined;
    const current = prior ?? new Map<boolean, {count: number, previous?: object}>();
    const next = current.get(temp) ?? {count: 0};
    next.count++;
    current.set(temp, next);
    (observation as any).attempts = current;
    const configurator = authorizer.dcConfigurator;
    const savedActive = (configurator as any).__webExchangeActiveAttempt;
    (configurator as any).__webExchangeActiveAttempt = {temp, attempts: next};
    try {
      return originalAuthAttempt(dcId, temp);
    } finally {
      (configurator as any).__webExchangeActiveAttempt = savedActive;
    }
  };

  const configurator = authorizer.dcConfigurator as any;
  const originalChoose = configurator.chooseServer.bind(configurator);
  const originalTransportSocket = configurator.transportSocket.bind(configurator);
  configurator.transportSocket = (...args: any[]) => {
    if(!scope.active) return null;
    try {
      return originalTransportSocket(...args);
    } catch{
      recordPreRequestFailure(caseName, 'transport_construction', failureMarkers, observation);
      scope.record();
      return null;
    }
  };
  configurator.chooseServer = (...args: any[]) => {
    if(!scope.active) return null;
    let transport: any;
    try {
      transport = originalChoose(...args);
    } catch{
      recordPreRequestFailure(caseName, 'transport_selection', failureMarkers, observation);
      scope.record();
      return null;
    }
    const active = configurator.__webExchangeActiveAttempt as {temp: boolean, attempts: {count: number, previous?: object}} | undefined;
    if(transport && active && (typeof transport === 'object' || typeof transport === 'function')) {
      attemptsByTransport.set(transport, {temp: active.temp, count: active.attempts.count});
      const socket = (transport as any).connection || transport;
      if(active.attempts.count > 1 && active.attempts.previous === socket) {
        observation.retryReused = true;
        observation.retrySocket = socket;
        scope.record();
      }
      active.attempts.previous = socket;
    }
    if(transport) observeTransport(transport, scope);
    return transport;
  };
}

function recordPreRequestFailure(
  caseName: string,
  category: PreRequestFailureCategory,
  failureMarkers: Map<string, PreRequestFailureCategory>,
  observation: Observation
) {
  if(failureMarkers.has(caseName)) return;
  const requestCount = [...observation.sockets.values()].reduce((total, socket) => total + socket.requests, 0);
  if(requestCount === 0) failureMarkers.set(caseName, category);
}

function observeTransport(transport: any, scope: CaseScope): SocketObservation {
  const observation = scope.observation;
  const connection = transport.connection || transport;
  let socket = observation.sockets.get(connection);
  if(socket) return socket;
  socket = {transport, connection, requests: 0, third: 'unknown', closed: connection?.readyState === 3};
  observation.sockets.set(connection, socket);
  scope.transports.add(transport);
  if(!socket.closed && typeof connection.addEventListener === 'function') {
    socket.closeListener = () => {
      socket!.closed = true;
      if(scope.active) scope.record();
    };
    connection.addEventListener('close', socket.closeListener, {once: true});
  } else if(!socket.closed && typeof connection.once === 'function') {
    socket.closeListener = () => {
      socket!.closed = true;
      if(scope.active) scope.record();
    };
    connection.once('close', socket.closeListener);
  }
  scope.record();
  const originalSend = transport.send.bind(transport);
  transport.send = (body: Uint8Array) => {
    if(!scope.active) return Promise.reject(undefined);
    let response: any;
    try {
      response = originalSend(body);
    } catch{
      return Promise.reject(undefined);
    }
    if(response && typeof response.then === 'function') {
      const observed = Promise.resolve(response).then((result: Uint8Array) => {
        if(!scope.active) return result;
        if(result?.byteLength === 4 && new DataView(result.buffer, result.byteOffset, 4).getInt32(0, true) === -404) {
          observation.proto404 = true;
          retainFirstAbort(observation, 'response_mismatch');
          scope.record();
        }
        return result;
      }, () => {
        if(scope.active && !observation.proto404) {
          retainFirstAbort(observation, 'transport_closed');
          scope.record();
        }
        return Promise.reject(undefined);
      });
      return scope.track(observed);
    }
    return response;
  };
  if(!transportsToClose.includes(transport)) transportsToClose.push(transport);
  return socket;
}

function newObservation(): Observation {
  return {
    stage: 'unknown', abort: 'unknown', abortObserved: false, retryReused: false, proto404: false, inClientDh: false,
    currentConstructor: 'unknown', sockets: new Map()
  };
}

function toClientRow(name: string, observation: Observation): ClientRow {
  const sockets = [...observation.sockets.values()];
  const maxRequests = sockets.reduce((largest, socket) => Math.max(largest, socket.requests), 0);
  const retrySocket = observation.retrySocket ? observation.sockets.get(observation.retrySocket) : undefined;
  const busiest = retrySocket ? [retrySocket] : sockets.filter((socket) => socket.requests === maxRequests && socket.requests > 0);
  const third = busiest.length === 0 ? 'unknown' : busiest.every((socket) => socket.third === busiest[0].third) ? busiest[0].third : 'other';
  let stage = observation.stage;
  if(!['req_pq_sent', 'res_pq_ok', 'req_dh_sent', 'server_dh_received', 'server_dh_validated', 'set_client_dh_sent', 'dh_gen_verified', 'unknown'].includes(stage)) stage = 'unknown';
  let abort = observation.abort;
  if(observation.proto404 && abort === 'none') abort = 'response_mismatch';
  return {
    case: name,
    client_stage: stage,
    client_abort_reason: abort,
    plain_requests_on_socket: Math.min(retrySocket?.requests ?? maxRequests, 12),
    retry_reused_socket: observation.retryReused,
    third_constructor: third,
    proto_error_404: observation.proto404
  };
}

function classifyConstructor(request: Uint8Array, prefixes: Record<string, Uint8Array>): Constructor {
  for(const name of ['set_client_dh_params', 'req_pq_multi', 'req_dh_params'] as const) {
    const prefix = prefixes[name];
    if(prefix && request.length >= prefix.length && prefix.every((value, index) => request[index] === value)) return name;
  }
  return 'other';
}

async function requestPrefixes(TLSerialization: any): Promise<Record<string, Uint8Array>> {
  const nonce = new Uint8Array(16);
  const entries: Array<[string, string, unknown]> = [
    ['req_pq_multi', 'req_pq_multi', {nonce}],
    ['req_dh_params', 'req_DH_params', {nonce, server_nonce: nonce, p: new Uint8Array([1]), q: new Uint8Array([1]), public_key_fingerprint: '0', encrypted_data: new Uint8Array([1])}],
    ['set_client_dh_params', 'set_client_DH_params', {nonce, server_nonce: nonce, encrypted_data: new Uint8Array([1])}]
  ];
  const output: Record<string, Uint8Array> = {};
  for(const [name, method, payload] of entries) {
    const serializer = new TLSerialization({mtproto: true});
    serializer.storeMethod(method, payload as any);
    output[name] = serializer.getBytes(true).slice(0, 4);
  }
  return output;
}

function setTarget(config: Config, endpoint: string) {
  assertLoopbackEndpoint(endpoint);
  const next = {
    mode: 'private',
    endpoint,
    fingerprint: config.public_key.fingerprint,
    publicKey: 'synthetic test-only public key',
    publicKeyHex: {modulus: config.public_key.modulus, exponent: config.public_key.exponent},
    routeLock: {
      mode: 'private',
      endpoint,
      transport: 'websocket',
      dcIds: [1, 2, 3, 4, 5],
      connectionTypes: ['client', 'upload', 'download']
    }
  };
  if(experimentState.target) Object.assign(experimentState.target, next);
  else experimentState.target = next;
}

function assertLoopbackEndpoint(endpoint: string) {
  const parsed = new URL(endpoint);
  expect(parsed.protocol).toBe('ws:');
  expect(['127.0.0.1', '[::1]', '::1']).toContain(parsed.hostname);
  expect(parsed.port).not.toBe('');
  expect(parsed.pathname).toBe('/apiws');
  expect(parsed.username).toBe('');
  expect(parsed.password).toBe('');
}

function rowFor(rows: ClientRow[], name: string) {
  const row = rows.find((candidate) => candidate.case === name);
  if(!row) throw new Error('synthetic case result missing');
  return row;
}
