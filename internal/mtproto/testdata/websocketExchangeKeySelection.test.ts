import {it, vi} from 'vitest';

const configuration = vi.hoisted(async() => {
  // @ts-ignore the pinned Web target resolver is a JavaScript module.
  const {resolveMtprotoTarget} = await import('../../scripts/mtproto-target.mjs');
  return {target: resolveMtprotoTarget({
    MTPROTO_TARGET_MODE: 'private',
    MTPROTO_PRIVATE_ENDPOINT: 'wss://private.example.test:2443/apiws',
    MTPROTO_PRIVATE_RSA_PUBLIC_KEY_FILE: process.cwd() + '/scripts/fixtures/private-mtproto-public.pem'
  })};
});

vi.mock('@config/mtprotoTarget', async() => {
  const {target} = await configuration;
  return {
    MTProtoTarget: target,
    getMtprotoTarget: () => target,
    isPrivateMtprotoTarget: () => true,
    validateMtprotoTarget: (value: unknown) => value
  };
});

import '@lib/crypto/crypto.worker';
import {RSAKeysManager} from '@lib/mtproto/rsaKeysManager';
import bigInt from 'big-integer';

it('selects the synthetic private identity without a network exchange', async() => {
  const {target} = await configuration;
  const manager = new RSAKeysManager();
  const selected = await manager.select([bigInt(target.fingerprint, 16).toString()]);
  if(!selected || selected.modulus !== target.publicKeyHex.modulus || selected.exponent !== target.publicKeyHex.exponent) {
    throw new Error('teagram-web-authorizer-exchange-experiment-test-only key selection failed');
  }
});
