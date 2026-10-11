import {readFileSync, writeFileSync} from 'node:fs';
import {expect, it, vi} from 'vitest';

// teagram-web-single-permanent-exchange-web-test-only
const exchangeState = vi.hoisted(() => ({
  target: undefined as any,
  randomState: 0x1a2b3c4d
}));

vi.mock('@config/mtprotoTarget', () => ({
  MTProtoTarget: exchangeState.target,
  getMtprotoTarget: () => exchangeState.target,
  isPrivateMtprotoTarget: () => exchangeState.target?.mode === 'private',
  validateMtprotoTarget: (target: unknown) => target
}));

vi.mock('@helpers/array/randomize', () => ({
  default: (array: ArrayBufferView) => {
    const bytes = new Uint8Array(array.buffer, array.byteOffset, array.byteLength);
    for(let i = 0; i < bytes.length; ++i) {
      let value = exchangeState.randomState >>> 0;
      value ^= value << 13;
      value ^= value >>> 17;
      value ^= value << 5;
      exchangeState.randomState = value >>> 0;
      bytes[i] = value & 0xff;
    }
    return array;
  }
}));

type ClientStage =
  | 'req_pq_sent'
  | 'res_pq_ok'
  | 'req_dh_sent'
  | 'server_dh_received'
  | 'server_dh_validated'
  | 'set_client_dh_sent'
  | 'dh_gen_verified'
  | 'unknown';

type Config = {
  origin: string,
  endpoint: string,
  public_key: {modulus: string, exponent: string, fingerprint: string}
};

type NodeWebSocketConstructor = new (
  address: string | URL,
  protocols?: string | string[],
  options?: {headers?: Record<string, string>}
) => WebSocket;

const ORIGIN = 'https://permanent-exchange.synthetic.invalid';
const TEST_NAME = 'runs one shipped-Web permanent exchange against the real server';

it(TEST_NAME, async() => {
  const configPath = process.env.WEB_PERMANENT_CONFIG_PATH;
  const resultPath = process.env.WEB_PERMANENT_CLIENT_RESULT_PATH;
  let clientStage: ClientStage = 'unknown';
  let attempts = 0;
  let cleanupSucceeded = true;
  const transports = new Set<any>();

  try {
    if(process.env.WEB_PERMANENT_CI_ONLY !== '1' || !configPath || !resultPath) {
      throw new Error('synthetic Web exchange inputs are unavailable');
    }
    const config = JSON.parse(readFileSync(configPath, 'utf8')) as Config;
    assertSyntheticConfig(config);
    setSyntheticTarget(config);

    const {installNodeEnv} = await import('./api/nodeEnv');
    installNodeEnv();
    const NodeWebSocket = globalThis.WebSocket as unknown as NodeWebSocketConstructor;
    class OriginWebSocket extends NodeWebSocket {
      constructor(address: string | URL, protocols?: string | string[]) {
        super(address, protocols, {headers: {Origin: ORIGIN}});
      }
    }
    vi.stubGlobal('WebSocket', OriginWebSocket);

    await import('../lib/crypto/crypto.worker');
    const [authorizerModule, configuratorModule, timeManagerModule, tlModule] = await Promise.all([
      import('@lib/mtproto/authorizer'),
      import('@lib/mtproto/dcConfigurator'),
      import('@lib/mtproto/timeManager'),
      import('@lib/mtproto/tl_utils')
    ]);
    const configurator = new configuratorModule.DcConfigurator();
    const authorizer = new authorizerModule.Authorizer({
      timeManager: new timeManagerModule.TimeManager(),
      dcConfigurator: configurator
    }) as any;

    const originalChooseServer = configurator.chooseServer.bind(configurator) as (...args: any[]) => any;
    configurator.chooseServer = (...args: any[]) => {
      const transport = originalChooseServer(...args);
      if(transport) transports.add(transport);
      return transport;
    };

    const prefixes = requestPrefixes(tlModule.TLSerialization);
    const originalSendPlainRequest = authorizer.sendPlainRequest.bind(authorizer);
    authorizer.sendPlainRequest = (transport: any, request: Uint8Array) => {
      const method = classifyRequest(request, prefixes);
      if(method === 'req_pq_multi') clientStage = 'req_pq_sent';
      if(method === 'req_DH_params') clientStage = 'req_dh_sent';
      if(method === 'set_client_DH_params') clientStage = 'set_client_dh_sent';
      const pending = originalSendPlainRequest(transport, request);
      return pending.then((response: Uint8Array) => {
        if(method === 'req_pq_multi') clientStage = 'res_pq_ok';
        if(method === 'req_DH_params') clientStage = 'server_dh_received';
        return response;
      });
    };

    const originalDecrypt = authorizer.decryptServerDhDataAnswer.bind(authorizer);
    authorizer.decryptServerDhDataAnswer = async(...args: any[]) => {
      clientStage = 'server_dh_received';
      await originalDecrypt(...args);
      clientStage = 'server_dh_validated';
    };

    const originalSendClientDh = authorizer.sendSetClientDhParams.bind(authorizer);
    authorizer.sendSetClientDhParams = async(...args: any[]) => {
      clientStage = 'set_client_dh_sent';
      await originalSendClientDh(...args);
      clientStage = 'dh_gen_verified';
    };

    const originalOneShot = authorizer.__auth.bind(authorizer);
    authorizer.__auth = (dcId: number, temp: boolean) => {
      attempts++;
      if(attempts !== 1 || temp !== false) return Promise.reject(undefined);
      return originalOneShot(dcId, temp);
    };

    await authorizer.getTransportType();
    await authorizer.__auth(2, false);
  } catch{
    // Keep caught values out of test output and assertions.
  } finally {
    for(const transport of transports) {
      if(!await closeTransport(transport)) cleanupSucceeded = false;
    }
    vi.unstubAllGlobals();
    if(resultPath) {
      try {
        const result = {client_stage: allowedClientStage(clientStage) ? clientStage : 'unknown'};
        writeFileSync(resultPath, `${JSON.stringify(result)}\n`, {mode: 0o600, flag: 'wx'});
      } catch{
        cleanupSucceeded = false;
      }
    }
  }

  expect(attempts).toBe(1);
  expect(cleanupSucceeded).toBe(true);
  expect(clientStage).toBe('dh_gen_verified');
}, 35_000);

function assertSyntheticConfig(config: Config) {
  if(config?.origin !== ORIGIN ||
    typeof config.endpoint !== 'string' ||
    !/^[0-9a-f]{512}$/.test(config.public_key?.modulus) ||
    !/^(?:[0-9a-f]{2})+$/.test(config.public_key?.exponent) ||
    !/^[0-9a-f]{16}$/.test(config.public_key?.fingerprint)) {
    throw new Error('synthetic Web exchange configuration is invalid');
  }
  const endpoint = new URL(config.endpoint);
  if(endpoint.protocol !== 'ws:' ||
    !['127.0.0.1', '[::1]', '::1'].includes(endpoint.hostname) ||
    !endpoint.port || endpoint.pathname !== '/apiws' ||
    endpoint.username || endpoint.password || endpoint.search || endpoint.hash) {
    throw new Error('synthetic Web exchange endpoint is invalid');
  }
}

function setSyntheticTarget(config: Config) {
  const endpoint = config.endpoint;
  exchangeState.target = {
    mode: 'private',
    endpoint,
    fingerprint: config.public_key.fingerprint,
    publicKey: 'synthetic test-only public key',
    publicKeyHex: {
      modulus: config.public_key.modulus,
      exponent: config.public_key.exponent
    },
    routeLock: {
      mode: 'private',
      endpoint,
      transport: 'websocket',
      dcIds: [1, 2, 3, 4, 5],
      connectionTypes: ['client', 'upload', 'download']
    }
  };
}

function requestPrefixes(TLSerialization: any) {
  const methods: Array<[string, string, Record<string, unknown>]> = [
    ['req_pq_multi', 'req_pq_multi', {nonce: new Uint8Array(16)}],
    ['req_DH_params', 'req_DH_params', {
      nonce: new Uint8Array(16),
      server_nonce: new Uint8Array(16),
      p: new Uint8Array([3]),
      q: new Uint8Array([5]),
      public_key_fingerprint: '1',
      encrypted_data: new Uint8Array([1])
    }],
    ['set_client_DH_params', 'set_client_DH_params', {
      nonce: new Uint8Array(16),
      server_nonce: new Uint8Array(16),
      encrypted_data: new Uint8Array([1])
    }]
  ];
  return methods.map(([name, method, payload]) => {
    const serializer = new TLSerialization({mtproto: true});
    serializer.storeMethod(method, payload);
    return [name, new Uint8Array(serializer.getBytes(true)).slice(0, 4)] as const;
  });
}

function classifyRequest(request: Uint8Array, prefixes: Array<readonly [string, Uint8Array]>) {
  for(const [name, prefix] of prefixes) {
    if(request.byteLength >= prefix.byteLength && prefix.every((value, index) => request[index] === value)) {
      return name;
    }
  }
  return 'other';
}

function allowedClientStage(value: string): value is ClientStage {
  return [
    'req_pq_sent',
    'res_pq_ok',
    'req_dh_sent',
    'server_dh_received',
    'server_dh_validated',
    'set_client_dh_sent',
    'dh_gen_verified',
    'unknown'
  ].includes(value);
}

async function closeTransport(transport: any): Promise<boolean> {
  const connection = transport.connection;
  if(!connection) return true;

  let closed = connection.readyState === 3;
  let resolveClosed: (() => void) | undefined;
  const closedPromise = new Promise<void>((resolve) => { resolveClosed = resolve; });
  const onClose = () => {
    closed = true;
    resolveClosed?.();
  };
  if(!closed && typeof connection.once === 'function') connection.once('close', onClose);

  let destroyed = true;
  try {
    transport.destroy();
  } catch{
    destroyed = false;
  }
  if(!closed && typeof connection.once === 'function') {
    let timer: ReturnType<typeof setTimeout> | undefined;
    await Promise.race([
      closedPromise,
      new Promise<void>((resolve) => { timer = setTimeout(resolve, 1_500); })
    ]);
    if(timer) clearTimeout(timer);
    if(typeof connection.off === 'function') connection.off('close', onClose);
  }
  return destroyed && (closed || connection.readyState === 3);
}
