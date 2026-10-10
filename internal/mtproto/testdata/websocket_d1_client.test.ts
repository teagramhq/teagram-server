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

    const result = JSON.parse(readFileSync(resultPath, 'utf8')) as {stage: string, category: string};
    const categories = ['upgrade', 'framing', 'codec', 'exchange', 'client_decode', 'pass'];
    if(!categories.includes(result.stage) || result.category !== result.stage) {
      throw new Error('D1 server stage is invalid');
    }

    let stage = result.stage;
    let category = result.category;
    if(stage === 'client_decode') {
      const responsePath = process.env.D1_RESPONSE_PATH;
      if(!responsePath) {
        throw new Error('D1 client response path is unavailable');
      }
      const responseBytes = readFileSync(responsePath);
      const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      try {
        const responseFrame = await obfuscation.decode(new Uint8Array(responseBytes));
        const responseBody = abridgedPacketCodec.readPacket(responseFrame);
        const deserializer = new TLDeserialization<MTLong>(responseBody, {mtproto: true});
        const authKeyId = deserializer.fetchLong('auth_key_id');
        const msgId = deserializer.fetchLong('msg_id');
        deserializer.fetchInt('msg_len');
        const resPQ = deserializer.fetchObject('ResPQ') as {_: string, nonce: Uint8Array};
        if(authKeyId === '0' && msgId !== '0' && resPQ._ === 'resPQ' && bytesCmp(resPQ.nonce, bytesFromHex(nonceHex))) {
          stage = 'pass';
          category = 'pass';
        } else {
          stage = 'client_decode';
          category = 'client_decode';
        }
      } catch {
        stage = 'client_decode';
        category = 'client_decode';
      } finally {
        errorSpy.mockRestore();
      }
    }

    console.log(JSON.stringify({stage, category}));
  } finally {
    try {
      await obfuscation.release();
    } catch {
      throw new Error('D1 client crypto cleanup failed');
    }
  }
});
