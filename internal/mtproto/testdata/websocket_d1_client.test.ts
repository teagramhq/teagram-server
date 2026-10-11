import {readFileSync, writeFileSync} from 'node:fs';
import bytesCmp from '@helpers/bytes/bytesCmp';
import bytesFromHex from '@helpers/bytes/bytesFromHex';
import bytesToHex from '@helpers/bytes/bytesToHex';
import abridgedPacketCodec from '@lib/mtproto/transports/abridged';
import Obfuscation from '@lib/mtproto/transports/obfuscation';
import {TLDeserialization, TLSerialization} from '@lib/mtproto/tl_utils';
import {test, vi} from 'vitest';

const webClientRevision = 'c88211e3985942343bf40dcbbcb8e8f5b4b7d364';
const randomSeed = 'lcg32:1597';
const nonceHex = '0102030405060708090a0b0c0d0e0f10';

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

function reqPqMultiMessage() {
  const request = new TLSerialization({mtproto: true});
  request.storeMethod('req_pq_multi', {nonce: bytesFromHex(nonceHex)});
  const requestBytes = request.getBytes(true);

  const message = new TLSerialization({mtproto: true});
  message.storeLongP(0, 0, 'auth_key_id');
  message.storeLong('72623859790382856', 'msg_id');
  message.storeInt(requestBytes.byteLength, 'msg_len');
  message.storeRawBytes(requestBytes);
  return message.getBytes(true);
}

function readResponseMessages(path: string) {
  const encoded = JSON.parse(readFileSync(path, 'utf8')) as unknown;
  if(!Array.isArray(encoded) || encoded.length === 0 || !encoded.every((message): message is string =>
    typeof message === 'string' && message.length > 0 && /^(?:[0-9a-f]{2})+$/i.test(message))) {
    throw new Error('D1 server response is unavailable');
  }
  return encoded.map(bytesFromHex);
}

function coalesceResponseMessages(messages: Uint8Array[]) {
  const byteLength = messages.reduce((total, message) => total + message.byteLength, 0);
  const coalesced = new Uint8Array(byteLength);
  let offset = 0;
  for(const message of messages) {
    coalesced.set(message, offset);
    offset += message.byteLength;
  }
  return [coalesced];
}

async function decodeResponseMessages(messages: Uint8Array[], expectedInit: Uint8Array) {
  const obfuscation = new Obfuscation();
  const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
  try {
    seededRandom.reset();
    const init = await obfuscation.init(abridgedPacketCodec);
    if(!bytesCmp(init, expectedInit)) {
      throw new Error('D1 obfuscation state mismatch');
    }

    for(const responseMessage of messages) {
      try {
        const responseFrame = await obfuscation.decode(responseMessage);
        const responseBody = abridgedPacketCodec.readPacket(responseFrame);
        const deserializer = new TLDeserialization<MTLong>(responseBody, {mtproto: true});
        const authKeyId = deserializer.fetchLong('auth_key_id');
        const msgId = deserializer.fetchLong('msg_id');
        const msgLength = deserializer.fetchInt('msg_len');
        const resPQ = deserializer.fetchObject('ResPQ') as {_: string, nonce: Uint8Array};
        if(authKeyId === '0' && msgId !== '0' &&
          msgLength === responseBody.byteLength - 20 &&
          resPQ._ === 'resPQ' && bytesCmp(resPQ.nonce, bytesFromHex(nonceHex))) {
          return 'pass';
        }
      } catch {
        return 'client_decode';
      }
    }
    return 'client_decode';
  } finally {
    errorSpy.mockRestore();
    try {
      await obfuscation.release();
    } catch {
      throw new Error('D1 client crypto cleanup failed');
    }
  }
}

test('reproduces the WebSocket request vector and decodes resPQ', async() => {
  const workerLog = vi.spyOn(console, 'log').mockImplementation(() => {});
  try {
    await import('@lib/crypto/crypto.worker');
  } finally {
    workerLog.mockRestore();
  }
  const obfuscation = new Obfuscation();
  try {
    seededRandom.reset();
    const init = await obfuscation.init(abridgedPacketCodec);
    const request = await obfuscation.encode(abridgedPacketCodec.encodePacket(reqPqMultiMessage()));
    const generated: WebClientD1Vector = {
      format: 'teagram-web-obfuscated2-abridged-v1',
      web_revision: webClientRevision,
      seed: randomSeed,
      nonce: nonceHex,
      messages: {
        init: bytesToHex(init),
        req_pq_multi: bytesToHex(request)
      }
    };

    const generatedVectorPath = process.env.D1_GENERATED_VECTOR_PATH;
    if(generatedVectorPath) {
      writeFileSync(generatedVectorPath, `${JSON.stringify(generated, null, 2)}\n`, {mode: 0o600});
      return;
    }

    const vectorPath = process.env.D1_VECTOR_PATH;
    const resultPath = process.env.D1_RESULT_PATH;
    if(!vectorPath || !resultPath) {
      throw new Error('D1 client test inputs are unavailable');
    }
    const vector = JSON.parse(readFileSync(vectorPath, 'utf8')) as WebClientD1Vector;
    if(vector.format !== generated.format ||
      vector.web_revision !== generated.web_revision ||
      vector.seed !== generated.seed ||
      vector.nonce !== generated.nonce ||
      vector.messages?.init !== generated.messages.init ||
      vector.messages?.req_pq_multi !== generated.messages.req_pq_multi) {
      throw new Error('D1 client vector mismatch');
    }

    const result = JSON.parse(readFileSync(resultPath, 'utf8')) as WebClientD1ServerReport;
    const categories = ['upgrade', 'framing', 'codec', 'exchange', 'client_decode', 'pass'];
    if(!categories.includes(result.stage) || result.category !== result.stage) {
      throw new Error('D1 server stage is invalid');
    }

    if(!Number.isSafeInteger(result.response_message_count) || result.response_message_count < 0 ||
      result.response_message_count > 16 ||
      typeof result.first_message_complete_packet !== 'boolean') {
      throw new Error('D1 response metadata is unavailable');
    }

    let originalBoundaries = result.stage;
    let coalesced = result.stage;
    if(result.stage === 'client_decode') {
      if(result.response_message_count < 1) {
        throw new Error('D1 validated response metadata is unavailable');
      }
      const responsePath = process.env.D1_RESPONSE_PATH;
      if(!responsePath) {
        throw new Error('D1 response capture path is unavailable');
      }
      const responseMessages = readResponseMessages(responsePath);
      if(responseMessages.length !== result.response_message_count) {
        throw new Error('D1 response count does not match the validated capture');
      }

      const expectedInit = bytesFromHex(generated.messages.init);
      originalBoundaries = await decodeResponseMessages(responseMessages, expectedInit);
      coalesced = await decodeResponseMessages(coalesceResponseMessages(responseMessages), expectedInit);
    }

    const reportPath = process.env.D1_BASELINE_REPORT_PATH;
    if(!reportPath) {
      throw new Error('D1 baseline report path is unavailable');
    }
    const report: WebClientD1BaselineReport = {
      server_stage: result.stage,
      response_message_count: result.response_message_count,
      first_message_complete_packet: result.first_message_complete_packet,
      original_boundaries: originalBoundaries,
      coalesced
    };
    writeFileSync(reportPath, `${JSON.stringify(report)}\n`, {mode: 0o600, flag: 'wx'});

    console.log(JSON.stringify(report));
  } finally {
    try {
      await obfuscation.release();
    } catch {
      throw new Error('D1 client crypto cleanup failed');
    }
  }
});
