import assert from 'node:assert/strict';
import {test} from 'node:test';

let selectRealClientScenarios;
try {
  ({selectRealClientScenarios} = await import('./real_client_scenarios.mjs'));
} catch(error) {
  if(error.code !== 'ERR_MODULE_NOT_FOUND') throw error;
}

test('fails when the pinned web contract omits the custom emoji scenario', () => {
  assert.throws(() => selectRealClientScenarios({
    required: ['sign-in', 'first-message', 'message', 'group'],
    optional: ['channel', 'poll']
  }), /web contract omits required scenarios: custom-emoji/);
});

test('selects the scenarios supported by the pinned web contract', () => {
  assert.deepEqual(selectRealClientScenarios({
    required: ['sign-in', 'first-message', 'message', 'custom-emoji', 'group'],
    optional: ['channel', 'poll']
  }), ['sign-in', 'first-message', 'message', 'custom-emoji', 'group', 'channel', 'poll']);
});

test('fails when the pinned web contract requires an unsupported scenario', () => {
  assert.throws(() => selectRealClientScenarios({required: ['new-scenario'], optional: []}),
    /server runner does not support required scenarios: new-scenario/);
});
