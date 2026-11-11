import assert from 'node:assert/strict';
import {EventEmitter} from 'node:events';
import {copyFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync} from 'node:fs';
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

function patchBrowserForTest(t, globals, {
  legacyDirectMessage = false,
  exposePeerSelection = false,
  exposeCustomEmojiWaiters = false,
} = {}) {
  const sourceRoot = process.env.REAL_CLIENT_WEB_ROOT;
  assert.ok(sourceRoot, 'REAL_CLIENT_WEB_ROOT must name the pinned companion checkout');
  const webRoot = mkdtempSync(join(tmpdir(), 'real-client-observer-test-'));
  t.after(() => rmSync(webRoot, {recursive: true, force: true}));
  const scripts = join(webRoot, 'scripts/real-client');
  mkdirSync(scripts, {recursive: true});
  for(const name of ['browser.cjs', 'observation.cjs']) {
    copyFileSync(join(sourceRoot, 'scripts/real-client', name), join(scripts, name));
  }

  const browserPath = join(scripts, 'browser.cjs');
  if(exposePeerSelection) {
    const browserSource = readFileSync(browserPath, 'utf8');
    writeFileSync(browserPath, `${browserSource}\nmodule.exports.testSelectUniquePeer = selectUniquePeer;\n`);
  }
  if(exposeCustomEmojiWaiters) {
    const browserSource = readFileSync(browserPath, 'utf8');
    writeFileSync(browserPath, `${browserSource}\nmodule.exports.testWaitForCustomEmojiMessage = waitForCustomEmojiMessage;\nmodule.exports.testWaitForCustomMessageReaction = waitForCustomMessageReaction;\n`);
  }
  if(legacyDirectMessage) {
    const currentAssertion = '    await waitForSingleSentMessage(message, text, timeoutMs).catch(() => failStage(`${stage}_message_count`));';
    const legacyAssertion = '    await page.waitForTimeout(600);\n    currentStage = stage;\n    const count = await message.count();\n    if(count !== 1) failStage(`${stage}_message_count`);';
    let browserSource = readFileSync(browserPath, 'utf8');
    assert.equal(browserSource.split(currentAssertion).length - 1, 1);
    browserSource = browserSource.replace(currentAssertion, legacyAssertion);
    const currentExport = 'waitForGroupMessageCount, waitForSingleSentMessage,';
    assert.equal(browserSource.split(currentExport).length - 1, 1);
    browserSource = browserSource.replace(currentExport, 'waitForGroupMessageCount,');
    writeFileSync(browserPath, browserSource);
  }

  patchRealClientObserver(webRoot);
  const observation = createRequire(join(scripts, 'test.cjs'))(join(scripts, 'observation.cjs'));
  const browser = loadBrowserModule(browserPath, observation, globals);
  return {browser, browserPath, observation};
}

function peerSelectionHarness(t, states, {loseFirstLocatorMatch = false} = {}) {
  let stateIndex = 0;
  let selectedPeerId;
  let locatorCountCalls = 0;
  const visible = (row) => ({
    textContent: row.text,
    dataset: {peerId: row.peerId},
    display: row.display || 'block',
    getAttribute: (name) => name === 'data-peer-id' ? row.peerId : null,
    getBoundingClientRect: () => ({width: 120, height: 40}),
  });
  const document = {querySelectorAll: () => states[stateIndex].map(visible)};
  const input = {
    first() { return this; },
    async waitFor() {},
    async fill() {},
  };
  const rows = {
    first() { return this; },
    async waitFor() {},
    filter({hasText}) {
      const matches = () => states[stateIndex].filter((row) => row.display !== 'none' && row.text.includes(hasText));
      return {
        async count() {
          if(loseFirstLocatorMatch && locatorCountCalls++ === 0) {
            stateIndex = Math.min(stateIndex + 1, states.length - 1);
            return 0;
          }
          return matches().length;
        },
        async getAttribute(name) { return name === 'data-peer-id' ? matches()[0]?.peerId : null; },
        async click() { selectedPeerId = matches()[0]?.peerId; },
      };
    },
    async count() { return states[stateIndex].length; },
    nth(index) {
      return {async isVisible() { return states[stateIndex][index]?.display !== 'none'; }};
    },
  };
  const {browser} = patchBrowserForTest(t, {
    document,
    window: {getComputedStyle: (element) => ({display: element.display, visibility: 'visible'})},
  }, {exposePeerSelection: true});
  const page = {
    locator(selector) { return selector === '#column-left .input-search-input' ? input : rows; },
    async waitForFunction(predicate, args) {
      while(stateIndex < states.length) {
        if(predicate(args)) return;
        if(stateIndex === states.length - 1) break;
        stateIndex++;
      }
      throw new Error('search did not reach the requested peer state');
    },
    async evaluate() { return undefined; },
  };
  return {browser, page, selectedPeerId: () => selectedPeerId, stateIndex: () => stateIndex};
}

test('peer selection waits for one visible matching row after stale and duplicate results', async (t) => {
  assert.equal(typeof patchRealClientObserver, 'function');
  const states = [
    [
      {text: 'Bob recent chat', peerId: '11'},
      {text: 'Carol recent chat', peerId: '12'},
    ],
    [
      {text: 'Alice @alice_unique', peerId: '41', display: 'none'},
      {text: 'Carol recent chat', peerId: '12'},
    ],
    [
      {text: 'Alice @alice_unique', peerId: '42'},
      {text: 'Alice @alice_unique duplicate', peerId: '-42'},
    ],
    [
      {text: 'Alice @alice_unique', peerId: '42'},
      {text: 'Alice Group', peerId: '-42'},
    ],
  ];
  const {browser, page, selectedPeerId, stateIndex} = peerSelectionHarness(t, states);
  const peerId = await browser.testSelectUniquePeer(
    page,
    '#column-left .input-search-input',
    '#search-container .row[data-peer-id]',
    '@alice_unique',
    'bob_custom_emoji_status_chat'
  );
  assert.equal(peerId, '42');
  assert.equal(selectedPeerId(), '42');
  assert.equal(stateIndex(), 3);
});

test('peer selection retries when a unique search result disappears before its locator is read', async (t) => {
  assert.equal(typeof patchRealClientObserver, 'function');
  const states = [
    [{text: 'Alice @alice_unique', peerId: '42'}],
    [],
    [{text: 'Alice @alice_unique', peerId: '42'}],
  ];
  const {browser, page, selectedPeerId, stateIndex} = peerSelectionHarness(t, states, {loseFirstLocatorMatch: true});

  const peerId = await browser.testSelectUniquePeer(
    page,
    '#column-left .input-search-input',
    '#search-container .row[data-peer-id]',
    '@alice_unique',
    'bob_custom_emoji_status_chat'
  );

  assert.equal(peerId, '42');
  assert.equal(selectedPeerId(), '42');
  assert.equal(stateIndex(), 2);
});

test('peer selection reports missing search results when no visible username row appears', async (t) => {
  assert.equal(typeof patchRealClientObserver, 'function');
  const states = [[{text: 'Bob recent chat', peerId: '11'}]];
  const {browser, page} = peerSelectionHarness(t, states);

  await assert.rejects(
    browser.testSelectUniquePeer(
      page,
      '#column-left .input-search-input',
      '#search-container .row[data-peer-id]',
      '@alice_unique',
      'bob_custom_emoji_status_chat'
    ),
    /bob_custom_emoji_status_chat_search_results/
  );
});

test('peer selection reports ambiguity when multiple visible username rows remain', async (t) => {
  assert.equal(typeof patchRealClientObserver, 'function');
  const states = [[
    {text: 'Alice @alice_unique', peerId: '42'},
    {text: 'Alice @alice_unique duplicate', peerId: '-42'},
  ]];
  const {browser, page} = peerSelectionHarness(t, states);

  await assert.rejects(
    browser.testSelectUniquePeer(
      page,
      '#column-left .input-search-input',
      '#search-container .row[data-peer-id]',
      '@alice_unique',
      'bob_custom_emoji_status_chat'
    ),
    /bob_custom_emoji_status_chat_search_result_not_unique/
  );
});

function customEmojiMessage(text, docId, messageId, {sent = true, hasReaction = false, hasEmoji = true} = {}) {
  const reaction = {
    reactionCount: {
      reaction: {_: 'reactionCustomEmoji', document_id: docId},
      count: 1,
    },
  };
  const bubble = {
    getAttribute: (name) => name === 'data-mid' ? messageId : null,
    classList: {contains: (name) => (name === 'is-sent' || name === 'is-read') && sent},
    querySelectorAll: (selector) => selector === 'reaction-element' && hasReaction ? [reaction] : [],
  };
  return {
    textContent: `${text}👍`,
    dataset: {docId},
    closest: () => bubble,
    querySelectorAll: (selector) => selector === '.custom-emoji' && hasEmoji ? [{dataset: {docId}}] : [],
  };
}

function customEmojiWaitPage(messages) {
  return {
    async waitForFunction(predicate, args) {
      const result = predicate(args);
      if(!result) throw new Error('receiver state did not appear');
      return {jsonValue: async() => result, dispose: async() => {}};
    },
  };
}

test('custom emoji reaction wait follows the settled message ID through optimistic duplicates', async (t) => {
  assert.equal(typeof patchRealClientObserver, 'function');
  const messageText = 'browser-ci-custom-emoji-receiver';
  const docId = '-3086666970307253143';
  const optimistic = customEmojiMessage(messageText, docId, '2.0001', {sent: false});
  const sent = customEmojiMessage(messageText, docId, '3', {hasReaction: true});
  const document = {querySelectorAll: () => [optimistic, sent]};
  const {browser} = patchBrowserForTest(t, {document}, {exposeCustomEmojiWaiters: true});
  const page = customEmojiWaitPage([optimistic, sent]);

  const messageId = await browser.testWaitForCustomEmojiMessage(page, messageText, docId, 'alice_custom_emoji_send_visible');
  assert.equal(messageId, '3');
  await browser.testWaitForCustomMessageReaction(
    page, messageText, docId, messageId, 1, 'bob_custom_emoji_reaction_receiver'
  );
});

test('custom emoji reaction wait ignores a reaction on the optimistic duplicate', async (t) => {
  assert.equal(typeof patchRealClientObserver, 'function');
  const messageText = 'browser-ci-custom-emoji-receiver';
  const docId = '-3086666970307253143';
  const optimistic = customEmojiMessage(messageText, docId, '2.0001', {sent: false, hasReaction: true});
  const sent = customEmojiMessage(messageText, docId, '3');
  const document = {querySelectorAll: () => [optimistic, sent]};
  const {browser} = patchBrowserForTest(t, {document}, {exposeCustomEmojiWaiters: true});
  const page = customEmojiWaitPage([optimistic, sent]);

  await assert.rejects(
    browser.testWaitForCustomMessageReaction(
      page, messageText, docId, '3', 1, 'bob_custom_emoji_reaction_receiver'
    ),
    /bob_custom_emoji_reaction_receiver/
  );
});

test('custom emoji receiver wait accepts an incoming message without sender state', async (t) => {
  assert.equal(typeof patchRealClientObserver, 'function');
  const messageText = 'browser-ci-custom-emoji-receiver';
  const docId = '-3086666970307253143';
  const received = customEmojiMessage(messageText, docId, '9', {sent: false});
  const document = {querySelectorAll: () => [received]};
  const {browser} = patchBrowserForTest(t, {document}, {exposeCustomEmojiWaiters: true});
  const page = customEmojiWaitPage([received]);

  assert.equal(
    await browser.testWaitForCustomEmojiMessage(page, messageText, docId, 'bob_custom_emoji_receive_visible'),
    '9'
  );
});

test('custom emoji receiver wait fails when the requested emoji is missing', async (t) => {
  assert.equal(typeof patchRealClientObserver, 'function');
  const messageText = 'browser-ci-custom-emoji-receiver';
  const docId = '-3086666970307253143';
  const received = customEmojiMessage(messageText, docId, '9', {sent: false, hasEmoji: false});
  const document = {querySelectorAll: () => [received]};
  const {browser} = patchBrowserForTest(t, {document}, {exposeCustomEmojiWaiters: true});
  const page = customEmojiWaitPage([received]);

  await assert.rejects(
    browser.testWaitForCustomEmojiMessage(page, messageText, docId, 'bob_custom_emoji_receive_visible'),
    /bob_custom_emoji_receive_visible/
  );
});

test('custom emoji receiver wait rejects multiple numeric IDs carrying the emoji', async (t) => {
  assert.equal(typeof patchRealClientObserver, 'function');
  const messageText = 'browser-ci-custom-emoji-receiver';
  const docId = '-3086666970307253143';
  const first = customEmojiMessage(messageText, docId, '2');
  const second = customEmojiMessage(messageText, docId, '3');
  const document = {querySelectorAll: () => [first, second]};
  const {browser} = patchBrowserForTest(t, {document}, {exposeCustomEmojiWaiters: true});
  const page = customEmojiWaitPage([first, second]);

  await assert.rejects(
    browser.testWaitForCustomEmojiMessage(page, messageText, docId, 'bob_custom_emoji_receive_visible'),
    /bob_custom_emoji_receive_visible/
  );
});

test('custom emoji reaction wait fails when the receiver has no reaction', async (t) => {
  assert.equal(typeof patchRealClientObserver, 'function');
  const messageText = 'browser-ci-custom-emoji-receiver';
  const docId = '-3086666970307253143';
  const sent = customEmojiMessage(messageText, docId, '3');
  const document = {querySelectorAll: () => [sent]};
  const {browser} = patchBrowserForTest(t, {document}, {exposeCustomEmojiWaiters: true});
  const page = customEmojiWaitPage([sent]);

  await assert.rejects(
    browser.testWaitForCustomMessageReaction(
      page, messageText, docId, '3', 1, 'bob_custom_emoji_reaction_receiver'
    ),
    /bob_custom_emoji_reaction_receiver/
  );
});

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

test('the pinned web direct-message assertion retains exact text and sent-state checks', async (t) => {
  assert.equal(typeof patchRealClientObserver, 'function');
  const {browser, browserPath} = patchBrowserForTest(t);
  const browserSource = readFileSync(browserPath, 'utf8');
  const sendTextSource = browserSource.slice(browserSource.indexOf('async function sendText'),
    browserSource.indexOf('async function waitForMessageReaction'));
  assert.match(sendTextSource,
    /await waitForSingleSentMessage\(message, text, timeoutMs\)\.catch\(\(\) => failStage\(`\$\{stage\}_message_count`\)\);/);
  assert.match(sendTextSource,
    /if\(chat\)\s*\{\s*await waitForGroupMessageCount\(page, chat, groupTitle, message, `\$\{stage\}_message_count`, timeoutMs\);/);
  assert.doesNotMatch(sendTextSource, /waitForDirectMessageCount/);
  assert.equal(typeof browser.waitForSingleSentMessage, 'function');
  assert.equal(browser.waitForDirectMessageCount, undefined);
});

test('the patched legacy direct-message assertion settles idless and pending/sent bubble transients by element identity', async (t) => {
  assert.equal(typeof patchRealClientObserver, 'function');
  const {browser, browserPath} = patchBrowserForTest(t, undefined, {legacyDirectMessage: true});
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

test('the patched legacy direct-message assertion fails if idless duplicate bubbles persist through settling', async (t) => {
  assert.equal(typeof patchRealClientObserver, 'function');
  const {browser} = patchBrowserForTest(t, undefined, {legacyDirectMessage: true});
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

test('the patched legacy direct-message assertion rejects two bubble elements with one id', async (t) => {
  assert.equal(typeof patchRealClientObserver, 'function');
  const {browser} = patchBrowserForTest(t, undefined, {legacyDirectMessage: true});
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
    option('browser-ci-poll-answer-two', ['fade-2-exit-to']),
  ];
  const activeOptions = (selector) => options.filter((item) =>
    (!selector.includes(':not(.fade-2-exit-active)') || !item.classList.contains('fade-2-exit-active')) &&
    (!selector.includes(':not(.fade-2-exit-to)') || !item.classList.contains('fade-2-exit-to')));
  const questionNode = {children: [], textContent: question};
  const message = {
    dataset: {mid: '56'},
    textContent: `${question} ${options.map(({textContent}) => textContent).join(' ')}`,
    getBoundingClientRect: () => ({width: 420, height: 180}),
    querySelectorAll(selector) {
      if(selector.startsWith('[data-poll-option-idx]')) return activeOptions(selector);
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
      const active = activeOptions(selector);
      return {
        count: async() => active.length,
        getByText(text) {
          return {count: async() => active.filter((item) => item.textContent === text).length};
        },
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
