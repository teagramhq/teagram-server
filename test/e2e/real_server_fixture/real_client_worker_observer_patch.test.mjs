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

function loadBrowserModule(browserPath, observation, globals = {}) {
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
    ...globals,
  }, {filename: browserPath});
  return module.exports;
}

function patchBrowserForTest(t, globals) {
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
  const browser = loadBrowserModule(browserPath, observation, globals);
  return {browser, browserPath, observation};
}

function messageBubble(messageId, sendState) {
  return {
    getAttribute: (name) => name === 'data-mid' ? messageId : null,
    classList: {
      contains: (name) => name === 'is-sending' ? sendState === 'pending' :
        name === 'is-error' ? sendState === 'failed' : false,
    },
  };
}

function messageLocator(samples) {
  let sampleIndex = 0;
  return {
    async evaluateAll(inspect) {
      const sample = samples[Math.min(sampleIndex, samples.length - 1)];
      sampleIndex++;
      return inspect(sample.map((bubble) => ({closest: () => bubble})));
    },
    get sampleCount() {
      return sampleIndex;
    },
  };
}

function messagePage() {
  return {
    async evaluate() {
      return undefined;
    },
    async waitForTimeout(milliseconds) {
      await new Promise((resolvePromise) => setTimeout(resolvePromise, milliseconds));
    },
  };
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

test('the patched direct-message assertion settles idless and pending/sent bubble transients by element identity', async (t) => {
  assert.equal(typeof patchRealClientObserver, 'function');
  const {browser, browserPath} = patchBrowserForTest(t);
  const browserSource = readFileSync(browserPath, 'utf8');
  const sendTextSource = browserSource.slice(browserSource.indexOf('async function sendText'),
    browserSource.indexOf('async function waitForMessageReaction'));
  assert.match(sendTextSource,
    /const messageStage = `\$\{stage\}_message_count`;\s+const settled = await waitForDirectMessageCount\(page, message, messageStage, timeoutMs\);/);

  const page = messagePage();
  const sentBubble = messageBubble('8', 'sent');
  const message = messageLocator([
    [messageBubble(null, 'sent'), messageBubble(null, 'sent')],
    [messageBubble('-7', 'pending'), messageBubble('8', 'sent')],
    [sentBubble, sentBubble],
  ]);
  assert.equal(await browser.waitForDirectMessageCount(page, message, 'alice_direct_message_count', 2_000), true);

  const trace = await browser.captureGroupMessageState();
  assert.equal(trace.result, 'settled');
  assert.equal(trace.samples[0].count, 2);
  assert.deepEqual(JSON.parse(JSON.stringify(trace.samples[0].bubbles)), [
    {messageId: null, sendState: 'sent'},
    {messageId: null, sendState: 'sent'},
  ]);
  assert.deepEqual(JSON.parse(JSON.stringify(trace.samples[1].bubbles)), [
    {messageId: '-7', sendState: 'pending'},
    {messageId: '8', sendState: 'sent'},
  ]);
  assert.equal(trace.samples.length >= 3, true);
  assert.equal(trace.samples.at(-1).count, 1);
  assert.deepEqual(JSON.parse(JSON.stringify(trace.samples.at(-1).bubbles)), [
    {messageId: '8', sendState: 'sent'},
  ]);
});

test('the patched direct-message assertion fails if idless duplicate bubbles persist through settling', async (t) => {
  assert.equal(typeof patchRealClientObserver, 'function');
  const {browser} = patchBrowserForTest(t);
  const page = messagePage();
  const message = messageLocator([
    [messageBubble(null, 'sent'), messageBubble(null, 'sent')],
  ]);
  assert.equal(await browser.waitForDirectMessageCount(page, message, 'alice_direct_message_count', 250), false);

  const trace = await browser.captureGroupMessageState();
  assert.equal(trace.result, 'settle_timeout');
  assert.ok(trace.samples.length > 1);
  assert.ok(trace.samples.every((sample) => sample.count === 2));
});

test('the patched direct-message assertion rejects two bubble elements with one id', async (t) => {
  assert.equal(typeof patchRealClientObserver, 'function');
  const {browser} = patchBrowserForTest(t);
  const page = messagePage();
  const message = messageLocator([
    [messageBubble('8', 'sent'), messageBubble('8', 'sent')],
  ]);
  assert.equal(await browser.waitForDirectMessageCount(page, message, 'alice_direct_message_count', 2_000), false);

  const trace = await browser.captureGroupMessageState();
  assert.equal(trace.result, 'duplicate_bubbles');
  assert.deepEqual(JSON.parse(JSON.stringify(trace.samples[0].bubbles)), [
    {messageId: '8', sendState: 'sent'},
    {messageId: '8', sendState: 'sent'},
  ]);
});

test('the patched poll content check ignores an exiting duplicate option row', async (t) => {
  assert.equal(typeof patchRealClientObserver, 'function');
  const question = 'browser-ci-poll-exiting-option-question';
  const option = (text, classes = []) => {
    const textNode = {children: [], textContent: text};
    return {
      textContent: text,
      classList: {contains: (name) => classes.includes(name)},
      getBoundingClientRect: () => ({width: 240, height: 32}),
      querySelectorAll: (selector) => selector === '*' ? [textNode] : [],
    };
  };
  const options = [
    option('browser-ci-poll-answer-one'),
    option('browser-ci-poll-answer-two'),
    option('browser-ci-poll-answer-one', ['fade-2-exit-active', 'fade-2-exit-to']),
  ];
  const questionNode = {children: [], textContent: question};
  const message = {
    dataset: {mid: '56'},
    textContent: `${question} ${options.map(({textContent}) => textContent).join(' ')}`,
    getBoundingClientRect: () => ({width: 420, height: 180}),
    querySelectorAll(selector) {
      if(selector === '[data-poll-option-idx]') return options;
      if(selector === '*') return [questionNode, ...options.flatMap((item) => item.querySelectorAll('*'))];
      return [];
    },
  };
  const globals = {
    document: {querySelectorAll: (selector) => selector === '#column-center .bubble' ? [message] : []},
    window: {getComputedStyle: () => ({display: 'block', visibility: 'visible', opacity: '1'})},
  };
  const {browser} = patchBrowserForTest(t, globals);
  const questionLocator = {waitFor: async() => {}, count: async() => 1};
  const poll = {
    first() { return this; },
    waitFor: async() => {},
    count: async() => 1,
    getByText(text) {
      return text === question ? questionLocator : {count: async() => 0};
    },
    async evaluate(callback, args) {
      return callback(message, args);
    },
    locator(selector) {
      const active = selector.includes(':not(.fade-2-exit-active)')
        ? options.filter((item) => !item.classList.contains('fade-2-exit-active') && !item.classList.contains('fade-2-exit-to'))
        : options;
      return {
        count: async() => active.length,
        filter({hasText}) {
          const matches = active.filter((item) => item.textContent.includes(hasText));
          return {count: async() => matches.length};
        },
      };
    },
  };
  const page = {
    locator: () => poll,
    async waitForFunction(predicate, args) {
      if(!predicate(args)) throw new Error('predicate timed out');
    },
  };

  const resolvedPoll = await browser.waitForPollMessage(page, question, 'poll_exiting_option', 250, 56);
  await browser.assertPollContent(resolvedPoll, {question, description: '', stage: 'poll_exiting_option'});
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
