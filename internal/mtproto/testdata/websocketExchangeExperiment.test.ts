import {readFileSync, writeFileSync} from 'node:fs';
import {afterAll, expect, it, vi} from 'vitest';
import NodeWebSocket from 'ws';

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
const WEB_EXCHANGE_TEST_ONLY_MARKER = 'teagram-web-authorizer-exchange-experiment-test-only';
const WEB_EXCHANGE_PRE_REQUEST_FAILURE_MARKER = 'synthetic_pre_request_failure';

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
  requests: number,
  third: Constructor,
};

type Observation = {
  stage: ClientStage,
  abort: AbortReason,
  retryReused: boolean,
  proto404: boolean,
  inClientDh: boolean,
  currentConstructor: Constructor,
  sockets: Map<object, SocketObservation>,
  retrySocket?: object,
};

const CASES = ['A', 'B', 'C', 'N1', 'N2', 'N3'];
let transportsToClose: any[] = [];

afterAll(() => {
  for(const transport of transportsToClose) {
    try {
      transport.destroy();
    } catch{}
  }
  transportsToClose = [];
  vi.unstubAllGlobals();
});

it('runs isolated shipped-Web exchanges against synthetic server instances', async() => {
  const rows: ClientRow[] = [];
  const failureMarkers = new Map<string, PreRequestFailureCategory>();
  const configPath = process.env.WEB_EXCHANGE_CONFIG_PATH;
  const clientRowsPath = process.env.WEB_EXCHANGE_CLIENT_ROWS_PATH;

  try {
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

    if(config && sharedDependencies) {
      rows.push(await runAuthorizerCase('A', config, config.cases.A.endpoint, sharedDependencies, failureMarkers));
      rows.push(await runAuthorizerCase('B', config, config.cases.B.endpoint, {...sharedDependencies, injectRetryAbort: true}, failureMarkers));
      rows.push(await runApiManagerCase(config, config.cases.C.endpoint, sharedDependencies, failureMarkers));
      rows.push(await runAuthorizerCase('N1', config, config.cases.N1.endpoint, {...sharedDependencies, nextReqPQ: true}, failureMarkers));
      rows.push(await runAuthorizerCase('N2', config, config.cases.N2.endpoint, {...sharedDependencies, wrongTemporaryAes: true}, failureMarkers));
      rows.push(await runAuthorizerCase('N3', config, config.cases.N3.endpoint, {...sharedDependencies, corruptClientHash: true}, failureMarkers));
    }

    expect(rows).toHaveLength(6);
    expect(failureMarkers.size).toBe(0);
    expect(rowFor(rows, 'B').retry_reused_socket).toBe(true);
    expect(rowFor(rows, 'B').third_constructor).toBe('req_pq_multi');
    expect(rowFor(rows, 'N1').third_constructor).toBe('req_pq_multi');
    expect(rowFor(rows, 'N1').proto_error_404).toBe(false);
    expect(rowFor(rows, 'N2').proto_error_404).toBe(true);
    expect(rowFor(rows, 'N3').proto_error_404).toBe(true);
  } finally {
    try {
      if(clientRowsPath) {
        writeFileSync(clientRowsPath, JSON.stringify(rows), {mode: 0o600});
      }
    } finally {
      for(const name of CASES) {
        const category = failureMarkers.get(name);
        if(category) process.stdout.write(`${WEB_EXCHANGE_PRE_REQUEST_FAILURE_MARKER} case=${name} category=${category}\n`);
      }
    }
  }
});

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
};

async function runAuthorizerCase(
  name: string,
  config: Config,
  endpoint: string,
  dependencies: HarnessDependencies,
  failureMarkers: Map<string, PreRequestFailureCategory>
): Promise<ClientRow> {
  const observation = newObservation();
  let authorizer: any;
  let restoreInjection = () => {};
  try {
    setTarget(config, endpoint);
    const configurator = new dependencies.DcConfigurator();
    const timeManager = new dependencies.TimeManager();
    authorizer = new dependencies.Authorizer({timeManager, dcConfigurator: configurator}) as any;
    instrumentAuthorizer(authorizer, observation, dependencies.prefixes, name, failureMarkers);

    if(dependencies.injectRetryAbort) {
      const originalSendClientDhParams = authorizer.sendSetClientDhParams.bind(authorizer);
      let injected = false;
      authorizer.sendSetClientDhParams = async(auth: any) => {
        if(!injected) {
          injected = true;
          if(observation.stage !== 'server_dh_validated') {
            throw new Error('synthetic retry injection preceded server-DH validation');
          }
          observation.abort = 'unknown';
          throw new Error('synthetic post-validation abort');
        }
        return originalSendClientDhParams(auth);
      };
      restoreInjection = () => { authorizer.sendSetClientDhParams = originalSendClientDhParams; };
    }
    if(dependencies.nextReqPQ) {
      authorizer.sendSetClientDhParams = async(auth: any) => {
        observation.currentConstructor = 'req_pq_multi';
        observation.stage = 'req_pq_sent';
        const {TLSerialization} = await import('@lib/mtproto/tl_utils');
        const request = new TLSerialization({mtproto: true});
        request.storeMethod('req_pq_multi', {nonce: auth.nonce});
        return authorizer.sendPlainRequest(auth.transport, request.getBytes(true));
      };
    }
    if(dependencies.wrongTemporaryAes) {
      const originalSet = authorizer.sendSetClientDhParams.bind(authorizer);
      authorizer.sendSetClientDhParams = async(auth: any) => {
        const originalKey = auth.tmpAesKey;
        auth.tmpAesKey = Uint8Array.from(originalKey, (value: number) => value ^ 0xff);
        try {
          return await originalSet(auth);
        } finally {
          auth.tmpAesKey = originalKey;
        }
      };
    }
    if(dependencies.corruptClientHash) {
      const worker = dependencies.CryptoWorker as Record<string, any>;
      const originalInvoke = worker.invokeCrypto.bind(worker);
      let corrupted = false;
      worker.invokeCrypto = async(method: string, ...args: any[]) => {
        const result = await originalInvoke(method, ...args);
        if(observation.inClientDh && method === 'sha1' && !corrupted) {
          corrupted = true;
          const damaged = Uint8Array.from(result as Uint8Array);
          damaged[0] ^= 0xff;
          return damaged;
        }
        return result;
      };
      restoreInjection = () => { worker.invokeCrypto = originalInvoke; };
    }
  } catch{
    recordPreRequestFailure(name, 'client_initialization', failureMarkers, observation);
    observation.abort = 'unknown';
    closeObservedTransports(observation);
    return toClientRow(name, observation);
  }

  let complete = false;
  try {
    try {
      await authorizer.getTransportType();
    } catch{
      recordPreRequestFailure(name, 'transport_selection', failureMarkers, observation);
      observation.abort = 'unknown';
    }
    if(!failureMarkers.has(name)) {
      try {
        if(name.startsWith('N')) {
          await authorizer.__auth(2, false);
        } else {
          await authorizer.auth(2, false);
          complete = true;
        }
      } catch{
        recordPreRequestFailure(name, 'unknown', failureMarkers, observation);
        if(observation.proto404) observation.abort = 'response_mismatch';
        else if(observation.abort === 'none') observation.abort = 'unknown';
      }
    }
  } finally {
    restoreInjection();
    closeObservedTransports(observation);
  }
  if(complete) {
    observation.stage = 'dh_gen_verified';
    if(!observation.proto404 && observation.abort === 'none') observation.abort = 'none';
  } else if(observation.proto404) {
    observation.abort = 'response_mismatch';
  }
  return toClientRow(name, observation);
}

async function runApiManagerCase(
  config: Config,
  endpoint: string,
  dependencies: HarnessDependencies,
  failureMarkers: Map<string, PreRequestFailureCategory>
): Promise<ClientRow> {
  const observation = newObservation();
  let manager: any;
  try {
    setTarget(config, endpoint);
    const configurator = new dependencies.DcConfigurator();
    const timeManager = new dependencies.TimeManager();
    const authorizer = new dependencies.Authorizer({timeManager, dcConfigurator: configurator}) as any;
    instrumentAuthorizer(authorizer, observation, dependencies.prefixes, 'C', failureMarkers);

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
    observation.abort = 'unknown';
    closeObservedTransports(observation);
    return toClientRow('C', observation);
  }

  let complete = false;
  try {
    await manager.getNetworker(2);
    complete = true;
  } catch{
    recordPreRequestFailure('C', 'unknown', failureMarkers, observation);
    if(observation.proto404) observation.abort = 'response_mismatch';
    else if(observation.abort === 'none') observation.abort = 'unknown';
  } finally {
    closeObservedTransports(observation);
  }
  if(complete) {
    observation.stage = 'dh_gen_verified';
    if(!observation.proto404 && observation.abort === 'none') observation.abort = 'none';
  } else if(observation.proto404) {
    observation.abort = 'response_mismatch';
  }
  return toClientRow('C', observation);
}

function instrumentAuthorizer(
  authorizer: any,
  observation: Observation,
  prefixes: Record<string, Uint8Array>,
  caseName: string,
  failureMarkers: Map<string, PreRequestFailureCategory>
) {
  const originalSendPlain = authorizer.sendPlainRequest.bind(authorizer);
  authorizer.sendPlainRequest = (transport: object, request: Uint8Array) => {
    const socket = observeTransport(transport, observation);
    const constructor = classifyConstructor(request, prefixes);
    socket.requests = Math.min(socket.requests + 1, 12);
    if(socket.requests === 3) socket.third = constructor;
    observation.currentConstructor = constructor;
    if(constructor === 'req_pq_multi' || constructor === 'req_pq') observation.stage = 'req_pq_sent';
    if(constructor === 'req_dh_params') observation.stage = 'req_dh_sent';
    if(constructor === 'set_client_dh_params') observation.stage = 'set_client_dh_sent';
    return originalSendPlain(transport, request);
  };

  const originalDecrypt = authorizer.decryptServerDhDataAnswer.bind(authorizer);
  authorizer.decryptServerDhDataAnswer = async(...args: any[]) => {
    observation.stage = 'server_dh_received';
    const result = await originalDecrypt(...args);
    observation.stage = 'server_dh_validated';
    return result;
  };

  const originalSet = authorizer.sendSetClientDhParams.bind(authorizer);
  authorizer.sendSetClientDhParams = async(...args: any[]) => {
    observation.inClientDh = true;
    observation.stage = 'set_client_dh_sent';
    try {
      const result = await originalSet(...args);
      observation.stage = 'dh_gen_verified';
      return result;
    } finally {
      observation.inClientDh = false;
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
    try {
      return originalTransportSocket(...args);
    } catch{
      recordPreRequestFailure(caseName, 'transport_construction', failureMarkers, observation);
      return null;
    }
  };
  configurator.chooseServer = (...args: any[]) => {
    let transport: any;
    try {
      transport = originalChoose(...args);
    } catch{
      recordPreRequestFailure(caseName, 'transport_selection', failureMarkers, observation);
      return null;
    }
    const active = configurator.__webExchangeActiveAttempt as {temp: boolean, attempts: {count: number, previous?: object}} | undefined;
    if(transport && active) {
      const socket = (transport as any).connection || transport;
      if(active.attempts.count > 1 && active.attempts.previous === socket) {
        observation.retryReused = true;
        observation.retrySocket = socket;
      }
      active.attempts.previous = socket;
    }
    if(transport) observeTransport(transport, observation);
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

function observeTransport(transport: any, observation: Observation): SocketObservation {
  const connection = transport.connection || transport;
  let socket = observation.sockets.get(connection);
  if(socket) return socket;
  socket = {transport, requests: 0, third: 'unknown'};
  observation.sockets.set(connection, socket);
  const originalSend = transport.send.bind(transport);
  transport.send = (body: Uint8Array) => {
    const response = originalSend(body);
    if(response && typeof response.then === 'function') {
      return response.then((result: Uint8Array) => {
        if(result?.byteLength === 4 && new DataView(result.buffer, result.byteOffset, 4).getInt32(0, true) === -404) {
          observation.proto404 = true;
        }
        return result;
      }, () => {
        if(!observation.proto404) observation.abort = 'transport_closed';
        return Promise.reject(undefined);
      });
    }
    return response;
  };
  if(!transportsToClose.includes(transport)) transportsToClose.push(transport);
  return socket;
}

function newObservation(): Observation {
  return {
    stage: 'unknown', abort: 'none', retryReused: false, proto404: false, inClientDh: false,
    currentConstructor: 'unknown', sockets: new Map()
  };
}

function closeObservedTransports(observation: Observation) {
  const transports = new Set([...observation.sockets.values()].map((socket) => socket.transport));
  for(const transport of transports) {
    try {
      transport.destroy();
    } catch{}
  }
  transportsToClose = transportsToClose.filter((transport) => !transports.has(transport));
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
