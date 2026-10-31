import assert from 'node:assert/strict';
import {EventEmitter} from 'node:events';
import {copyFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync} from 'node:fs';
import {createRequire} from 'node:module';
import {join} from 'node:path';
import {tmpdir} from 'node:os';
import vm from 'node:vm';
import {test} from 'node:test';

let patchRealClientObserver;
try {
  ({patchRealClientObserver} = await import('./patch_real_client_worker_observer.mjs'));
} catch(error) {
  if(error.code !== 'ERR_MODULE_NOT_FOUND') throw error;
}

function loadBrowserModule(browserPath, observation) {
  const module = {exports: {}};
  const requireFromBrowser = createRequire(browserPath);
  const browserRequire = (name) => {
    if(name === './observation.cjs') return observation;
    if(name === 'playwright') return {chromium: {}};
    return requireFromBrowser(name);
  };
  vm.runInNewContext(readFileSync(browserPath, 'utf8'), {
    Buffer,
    clearTimeout,
    module,
    process,
    require: browserRequire,
    setTimeout,
  }, {filename: browserPath});
  return module.exports;
}

function patchBrowserForTest(t) {
  const sourceRoot = process.env.REAL_CLIENT_WEB_ROOT;
  assert.ok(sourceRoot, 'REAL_CLIENT_WEB_ROOT must name the pinned companion checkout');
  const webRoot = mkdtempSync(join(tmpdir(), 'real-client-observer-test-'));
  t.after(() => rmSync(webRoot, {recursive: true, force: true}));
  const scripts = join(webRoot, 'scripts/real-client');
  mkdirSync(scripts, {recursive: true});
  for(const name of ['browser.cjs', 'observation.cjs']) {
    copyFileSync(join(sourceRoot, 'scripts/real-client', name), join(scripts, name));
  }

  patchRealClientObserver(webRoot);
  const observation = createRequire(join(scripts, 'test.cjs'))(join(scripts, 'observation.cjs'));
  const browserPath = join(scripts, 'browser.cjs');
  const browser = loadBrowserModule(browserPath, observation);
  return {browser, browserPath, observation};
}

class FakeBrowserSession extends EventEmitter {
  constructor(resourceEntries) {
    super();
    this.resourceEntries = resourceEntries;
  }

  async send(method, params = {}) {
    if(method === 'Target.attachToTarget') return {sessionId: 'worker-session'};
    if(method !== 'Target.sendMessageToTarget') return {};

    const command = JSON.parse(params.message);
    let result = {};
    if(command.method === 'Runtime.evaluate') {
      const evaluate = new Function('performance', `return ${command.params.expression}`);
      const value = evaluate({getEntriesByType: () => this.resourceEntries});
      result = {result: {type: 'string', value}};
    }
    queueMicrotask(() => this.emit('Target.receivedMessageFromTarget', {
      sessionId: params.sessionId,
      message: JSON.stringify({id: command.id, result}),
    }));
    return {};
  }
}

test('the patched direct-message assertion counts stable message ids once', async (t) => {
  assert.equal(typeof patchRealClientObserver, 'function');
  const {browser, browserPath} = patchBrowserForTest(t);
  const browserSource = readFileSync(browserPath, 'utf8');
  const sendTextSource = browserSource.slice(browserSource.indexOf('async function sendText'),
    browserSource.indexOf('async function waitForMessageReaction'));
  assert.match(sendTextSource, /getSingleExactMessageTextFailure\(message, text\)/);
  assert.doesNotMatch(sendTextSource, /const count = await message\.count\(\)/);
  const messageLocator = (bubbles) => ({
    evaluateAll: async (inspect) => inspect(bubbles.map(({id, text}) => ({
      closest: () => ({getAttribute: () => id}),
      cloneNode: () => ({
        querySelectorAll: () => [],
        textContent: text,
      }),
    }))),
  });
  const matchingText = 'browser-ci-hello';
  assert.equal(await browser.getSingleExactMessageTextFailure(messageLocator([
    {id: '-7', text: matchingText},
    {id: '-7', text: matchingText},
  ]), matchingText), null);
  assert.equal(await browser.getSingleExactMessageTextFailure(messageLocator([
    {id: '-7', text: matchingText},
    {id: '8', text: matchingText},
  ]), matchingText), 'count');
  assert.equal(await browser.getSingleExactMessageTextFailure(messageLocator([
    {id: '', text: matchingText},
  ]), matchingText), 'missing_message_id');
});

test('the patched observer recovers missed CDP evidence from the worker timing entry', async (t) => {
  assert.equal(typeof patchRealClientObserver, 'function');
  const {browser, observation} = patchBrowserForTest(t);
  assert.equal(observation.allowCdpMethod('worker', 'Runtime.evaluate'), true);
  assert.equal(observation.classifyWorkerSourceFetchFromResourceTiming([
    {name: 'https://teagramd.test/index.worker-Abc123_-.js', responseStatus: 200},
  ]), 'ok');

  const observer = observation.createContextObserver('alice');
  observer.registerTarget({targetId: 'page-1', type: 'page'});
  observer.state.pageCoverageComplete = true;
  observer.state.windowElapsed = true;
  const registry = browser.createWorkerDiscovery();
  const session = new FakeBrowserSession([
    {name: 'https://teagramd.test/index.worker-Abc123_-.js', responseStatus: 200},
  ]);
  await registry.launch({newBrowserCDPSession: async() => session});
  registry.registerContext('context-1', observer);
  session.emit('Target.targetCreated', {targetInfo: {
    targetId: 'worker-1',
    type: 'shared_worker',
    url: 'blob:https://teagramd.test/worker-1',
    browserContextId: 'context-1',
  }});
  await registry.waitForSetup();
  await registry.collectMtprotoSourceFetches();

  assert.equal(observer.state.mtprotoSourceFetch, 'ok');
  assert.equal(observer.classifyMtproto(), 'mtproto_candidate');
  const missingSourceObserver = observation.createContextObserver('alice');
  missingSourceObserver.noteMtprotoSourceFetch('not_requested', 'worker-no-source');
  assert.equal(missingSourceObserver.state.mtprotoSourceFetch, 'not_requested');

  const absentObserver = observation.createContextObserver('alice');
  absentObserver.registerTarget({targetId: 'page-2', type: 'page'});
  absentObserver.state.pageCoverageComplete = true;
  absentObserver.state.windowElapsed = true;
  const absentRegistry = browser.createWorkerDiscovery();
  const absentSession = new FakeBrowserSession([]);
  await absentRegistry.launch({newBrowserCDPSession: async() => absentSession});
  absentRegistry.registerContext('context-2', absentObserver);
  absentSession.emit('Target.targetCreated', {targetInfo: {
    targetId: 'worker-2',
    type: 'shared_worker',
    url: 'blob:https://teagramd.test/worker-2',
    browserContextId: 'context-2',
  }});
  await absentRegistry.waitForSetup();
  await absentRegistry.collectMtprotoSourceFetches();

  assert.equal(absentObserver.state.mtprotoSourceFetch, 'not_requested');
  assert.equal(absentObserver.classifyMtproto(), 'unknown');
});
