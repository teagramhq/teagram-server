import {readFileSync, writeFileSync} from 'node:fs';
import EventListenerBase from '@helpers/eventListenerBase';
import bytesCmp from '@helpers/bytes/bytesCmp';
import bytesFromHex from '@helpers/bytes/bytesFromHex';
import bytesToHex from '@helpers/bytes/bytesToHex';
import {Authorizer} from '@lib/mtproto/authorizer';
import rsaKeysManager from '@lib/mtproto/rsaKeysManager';
import TcpObfuscated from '@lib/mtproto/transports/tcpObfuscated';
import {test, vi} from 'vitest';

const repairedWebRevision = '569529c1f36923c098760ef727808254dcb6a662';
const serverSourceRevision = '193b8c24357e22f3fc16099936e3bf187f935445';
const baselineWebRevision = 'c88211e3985942343bf40dcbbcb8e8f5b4b7d364';
const baselineTestHead = '258a6b10ccfa3f9e8359c4b7ed962f6b1cf3363f';
const baselineCiRun = 'https://github.com/teagramhq/teagram-server/actions/runs/38062345654';
const baselineArtifact = `${baselineCiRun}/artifacts/11673384202`;
const randomSeed = 'lcg32:1597';
const nonceHex = '0102030405060708090a0b0c0d0e0f10';
const categoryNames = ['upgrade', 'framing', 'codec', 'exchange', 'client_decode', 'pass'];

const seededRandom = vi.hoisted(() => {
  let state = 0;
  const initialState = 1597;
  return {
    reset() {
      state = initialState;
    },
    fill(bytes: Uint8Array) {
      for(let i = 0; i < bytes.length; i++) {
        state = (Math.imul(state, 1664525) + 1013904223) >>> 0;
        bytes[i] = state >>> 24;
      }
      return bytes;
    }
  };
});

vi.mock('@helpers/array/randomize', () => ({default: seededRandom.fill}));

type WebClientD1Vector = {
  format: string,
  web_revision: string,
  seed: string,
  nonce: string,
  messages: {
    init: string,
    req_pq_multi: string
  }
};

type WebClientD1ServerReport = {
  stage: string,
  category: string,
  response_message_count: number,
  first_message_complete_packet: boolean
};

type WebClientD1BaselineReport = {
  server_stage: string,
  response_message_count: number,
  first_message_complete_packet: boolean,
  original_boundaries: string,
  coalesced: string
};

type Attempt = {
  category: string,
  res_pq_consumer_count: number,
  first_message_left_pending: boolean
};

type WebClientD1SanitizedReport = {
  baseline: {
    web_source_revision: string,
    server_source_revision: string,
    original_test_head: string,
    original_ci_run: string,
    original_sanitized_artifact: string,
    follow_up_test_head: string,
    follow_up_base: string,
    response_message_count: number,
    first_message_complete_packet: boolean,
    reproduced_original_boundaries: string,
    reproduced_coalesced: string
  },
  repaired_client: {
    web_source_revision: string,
    server_source_revision: string,
    test_head: string,
    follow_up_base: string,
    ci_run: string,
    sanitized_artifact_name: string,
    response_message_count: number,
    first_message_complete_packet: boolean,
    original_boundaries: string,
    coalesced: string,
    original_res_pq_consumer_count: number,
    coalesced_res_pq_consumer_count: number,
    first_message_left_pending: boolean
  }
};

type ConnectionEvents = {
  open: () => void,
  message: (buffer: ArrayBuffer) => void,
  close: () => void
};

class FakeConnection extends EventListenerBase<ConnectionEvents> {
  public static instances: FakeConnection[] = [];
  public sent: Uint8Array[] = [];
  public closeCount = 0;

  constructor(
    public dcId: number,
    public url: string,
    public logSuffix: string
  ) {
    super();
    FakeConnection.instances.push(this);
  }

  public send(data: Uint8Array) {
    this.sent.push(data.slice());
  }

  public async open() {
    await Promise.all(this.dispatchResultableEvent('open'));
  }

  public message(data: Uint8Array) {
    const copy = data.slice();
    this.dispatchEvent('message', copy.buffer);
  }

  public close() {
    this.closeCount++;
    this.dispatchEvent('close');
  }
}

function readJSON<T>(path: string): T {
  return JSON.parse(readFileSync(path, 'utf8')) as T;
}

function readResponseMessages(path: string) {
  const encoded = readJSON<unknown>(path);
  if(!Array.isArray(encoded) || encoded.length === 0 || !encoded.every((message): message is string =>
    typeof message === 'string' && message.length > 0 && /^(?:[0-9a-f]{2})+$/i.test(message))) {
    throw new Error('D1 server response is unavailable');
  }
  return encoded.map(bytesFromHex);
}

async function waitFor(predicate: () => boolean, timeoutMs = 5000) {
  const deadline = Date.now() + timeoutMs;
  while(Date.now() < deadline) {
    if(predicate()) return true;
    await new Promise(resolve => setTimeout(resolve, 10));
  }
  return predicate();
}

async function replayThroughPendingConsumer(messages: Uint8Array[], vector: WebClientD1Vector): Promise<Attempt> {
  FakeConnection.instances = [];
  const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});
    const prepare = vi.spyOn(rsaKeysManager, 'prepare').mockResolvedValue(undefined);
    const select = vi.spyOn(rsaKeysManager, 'select').mockResolvedValue(undefined);
  let transport: TcpObfuscated | undefined;
  let connection: FakeConnection | undefined;
  let pendingConsumerCalls = 0;
  let requestSettled = false;
  let firstMessageLeftPending = false;

  try {
    seededRandom.reset();
    transport = new TcpObfuscated(
      FakeConnection as any,
      1,
      'wss://synthetic.example.test/apiws',
      '',
      3000
    );
    transport.setAutoReconnect(false);
    const activeConnection = FakeConnection.instances[0];
    connection = activeConnection;
    await activeConnection.open();

    const authorizer = new Authorizer({
      timeManager: {generateId: () => '72623859790382856'} as any,
      dcConfigurator: {} as any
    });
    const nonce = bytesFromHex(vector.nonce);
    const auth: any = {
      dcId: 1,
      nonce,
      temp: false,
      media: false,
      transport
    };
    const sendReqPQ = (authorizer as any).sendReqPQ.bind(authorizer) as (value: typeof auth) => Promise<unknown>;
    // D1 stops at resPQ; its ephemeral server key is not a client pin.
    select.mockImplementation(async() => {
      pendingConsumerCalls++;
      return undefined;
    });
    const request = sendReqPQ(auth).then(
      () => {requestSettled = true;},
      () => {requestSettled = true;}
    );

    const requestSent = await waitFor(() => activeConnection.sent.length === 2);
    const requestMatches = requestSent &&
      bytesToHex(activeConnection.sent[0]) === vector.messages.init &&
      bytesToHex(activeConnection.sent[1]) === vector.messages.req_pq_multi;
    if(!requestMatches) {
      await waitFor(() => requestSettled, 100);
      return {category: 'exchange', res_pq_consumer_count: pendingConsumerCalls, first_message_left_pending: false};
    }

    for(let i = 0; i < messages.length; i++) {
      activeConnection.message(messages[i]);
      const receiveIdle = await waitFor(() => {
        const state = (transport as any).receiveState;
        return !state || !state.processing && state.queue.length === 0;
      });
      if(!receiveIdle) {
        return {category: 'client_decode', res_pq_consumer_count: pendingConsumerCalls, first_message_left_pending: false};
      }
      if(i === 0 && messages.length > 1) {
        firstMessageLeftPending = !requestSettled && pendingConsumerCalls === 0;
      }
    }

    await waitFor(() => requestSettled);
    const decodedResPQ = pendingConsumerCalls === 1 && select.mock.calls.length === 1 &&
      bytesCmp(auth.nonce, nonce) && auth.serverNonce instanceof Uint8Array && auth.serverNonce.byteLength === 16 &&
      auth.pq instanceof Uint8Array && auth.pq.byteLength > 0 && auth.pq.byteLength <= 8 &&
      Array.isArray(auth.fingerprints) && auth.fingerprints.length > 0 &&
      JSON.stringify(select.mock.calls[0]?.[0]) === JSON.stringify(auth.fingerprints) &&
      requestSettled &&
      (messages.length === 1 || firstMessageLeftPending);

    return {
      category: decodedResPQ ? 'pass' : activeConnection.closeCount > 0 ? 'framing' : 'client_decode',
      res_pq_consumer_count: pendingConsumerCalls,
      first_message_left_pending: firstMessageLeftPending
    };
  } catch {
    return {
      category: connection?.closeCount ? 'framing' : 'client_decode',
      res_pq_consumer_count: pendingConsumerCalls,
      first_message_left_pending: firstMessageLeftPending
    };
  } finally {
    transport?.destroy();
    select.mockRestore();
    prepare.mockRestore();
    consoleError.mockRestore();
  }
}

test('replays the validated D1 response through the repaired production pending request', async() => {
  const workerLog = vi.spyOn(console, 'log').mockImplementation(() => {});
  try {
    await import('@lib/crypto/crypto.worker');
  } finally {
    workerLog.mockRestore();
  }

  const vectorPath = process.env.D1_VECTOR_PATH;
  const resultPath = process.env.D1_RESULT_PATH;
  const baselinePath = process.env.D1_BASELINE_REPORT_PATH;
  const responsePath = process.env.D1_RESPONSE_PATH;
  const reportPath = process.env.D1_SANITIZED_REPORT_PATH;
  const testHead = process.env.D1_TEST_HEAD || '';
  const followUpBase = process.env.D1_FOLLOWUP_BASE || '';
  const ciRun = process.env.D1_CI_RUN_URL || '';
  if(!vectorPath || !resultPath || !baselinePath || !responsePath || !reportPath ||
    !/^[0-9a-f]{40}$/i.test(testHead) || !/^[0-9a-f]{40}$/i.test(followUpBase) ||
    !/^https:\/\/github\.com\/teagramhq\/teagram-server\/actions\/runs\/\d+$/.test(ciRun)) {
    throw new Error('D1 report inputs are unavailable');
  }

  const vector = readJSON<WebClientD1Vector>(vectorPath);
  if(vector.format !== 'teagram-web-obfuscated2-abridged-v1' || vector.web_revision !== baselineWebRevision ||
    vector.seed !== randomSeed || vector.nonce !== nonceHex) {
    throw new Error('D1 vector metadata is invalid');
  }
  const serverResult = readJSON<WebClientD1ServerReport>(resultPath);
  if(!categoryNames.includes(serverResult.stage) || serverResult.category !== serverResult.stage ||
    !Number.isSafeInteger(serverResult.response_message_count) || serverResult.response_message_count < 0 ||
    serverResult.response_message_count > 16 || typeof serverResult.first_message_complete_packet !== 'boolean') {
    throw new Error('D1 server stage is invalid');
  }

  const baselineReport = readJSON<WebClientD1BaselineReport>(baselinePath);
  if(!categoryNames.includes(baselineReport.server_stage) ||
    !categoryNames.includes(baselineReport.original_boundaries) || !categoryNames.includes(baselineReport.coalesced)) {
    throw new Error('D1 baseline stage is invalid');
  }
  const canReplay = serverResult.stage === 'client_decode' && baselineReport.server_stage === 'client_decode' &&
    serverResult.response_message_count > 0 && serverResult.response_message_count === baselineReport.response_message_count;
  const replayUnavailableCategory = serverResult.stage !== 'client_decode' ? serverResult.stage : 'exchange';
  let originalBoundaries: Attempt = {
    category: replayUnavailableCategory,
    res_pq_consumer_count: 0,
    first_message_left_pending: false
  };
  let coalesced: Attempt = {
    category: replayUnavailableCategory,
    res_pq_consumer_count: 0,
    first_message_left_pending: false
  };
  if(canReplay) {
    const responseMessages = readResponseMessages(responsePath);
    if(responseMessages.length !== serverResult.response_message_count) {
      throw new Error('D1 response count does not match the validated capture');
    }
    originalBoundaries = await replayThroughPendingConsumer(responseMessages, vector);
    const byteLength = responseMessages.reduce((total, message) => total + message.byteLength, 0);
    const combined = new Uint8Array(byteLength);
    let offset = 0;
    for(const message of responseMessages) {
      combined.set(message, offset);
      offset += message.byteLength;
    }
    coalesced = await replayThroughPendingConsumer([combined], vector);
  }

  const report: WebClientD1SanitizedReport = {
    baseline: {
      web_source_revision: baselineWebRevision,
      server_source_revision: serverSourceRevision,
      original_test_head: baselineTestHead,
      original_ci_run: baselineCiRun,
      original_sanitized_artifact: baselineArtifact,
      follow_up_test_head: testHead,
      follow_up_base: followUpBase,
      response_message_count: serverResult.response_message_count,
      first_message_complete_packet: serverResult.first_message_complete_packet,
      reproduced_original_boundaries: baselineReport.original_boundaries,
      reproduced_coalesced: baselineReport.coalesced
    },
    repaired_client: {
      web_source_revision: repairedWebRevision,
      server_source_revision: serverSourceRevision,
      test_head: testHead,
      follow_up_base: followUpBase,
      ci_run: ciRun,
      sanitized_artifact_name: 'websocket-d1-sanitized-report',
      response_message_count: serverResult.response_message_count,
      first_message_complete_packet: serverResult.first_message_complete_packet,
      original_boundaries: originalBoundaries.category,
      coalesced: coalesced.category,
      original_res_pq_consumer_count: originalBoundaries.res_pq_consumer_count,
      coalesced_res_pq_consumer_count: coalesced.res_pq_consumer_count,
      first_message_left_pending: originalBoundaries.first_message_left_pending
    }
  };
  writeFileSync(reportPath, `${JSON.stringify(report)}\n`, {mode: 0o600, flag: 'wx'});
  console.log(JSON.stringify(report));

  if(serverResult.stage !== 'client_decode' || baselineReport.original_boundaries !== 'client_decode' ||
    baselineReport.coalesced !== 'pass' || originalBoundaries.category !== 'pass' || coalesced.category !== 'pass' ||
    originalBoundaries.res_pq_consumer_count !== 1 || coalesced.res_pq_consumer_count !== 1 ||
    !serverResult.first_message_complete_packet && !originalBoundaries.first_message_left_pending) {
    throw new Error('D1 repaired receive-path acceptance failed');
  }
});
