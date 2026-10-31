import {execFileSync} from 'node:child_process';
import {readFileSync, writeFileSync} from 'node:fs';
import {join, resolve} from 'node:path';
import {pathToFileURL} from 'node:url';
import workerSourceObservation from './worker_source_observation.cjs';

const {classifyWorkerSourceFetchFromResourceTiming} = workerSourceObservation;

function replaceOnce(source, before, after, description) {
  const first = source.indexOf(before);
  if(first < 0 || source.indexOf(before, first + before.length) >= 0) {
    throw new Error(`pinned web observer no longer has one ${description} insertion point`);
  }
  return `${source.slice(0, first)}${after}${source.slice(first + before.length)}`;
}

export function patchRealClientObserver(webRoot) {
  const observationPath = join(webRoot, 'scripts/real-client/observation.cjs');
  const browserPath = join(webRoot, 'scripts/real-client/browser.cjs');
  let observation = readFileSync(observationPath, 'utf8');
  let browser = readFileSync(browserPath, 'utf8');
  if(observation.includes('noteMtprotoSourceFetch') || browser.includes('collectMtprotoSourceFetches') ||
      browser.includes('Count stable server message identities')) {
    throw new Error('pinned web observer is already patched');
  }

  const originalMessageTextAssertion = `async function getSingleExactMessageTextFailure(messages, expected) {
  const texts = await messages.evaluateAll((elements) => elements.map((element) => {
    const text = element.cloneNode(true);
    text.querySelectorAll('.time, .reactions').forEach((chrome) => chrome.remove());
    text.querySelectorAll('[data-sticker-emoji]').forEach((emoji) => emoji.replaceWith(emoji.dataset.stickerEmoji));
    text.querySelectorAll('img[alt]').forEach((emoji) => emoji.replaceWith(emoji.alt));
    return text.textContent;
  }));
  return getSingleExactMessageFailure(texts, expected);
}`;
  const messageTextAssertion = `async function getSingleExactMessageTextFailure(messages, expected) {
  // Count stable server message identities, not duplicate DOM elements.
  const matches = await messages.evaluateAll((elements) => elements.map((element) => {
    const text = element.cloneNode(true);
    text.querySelectorAll('.time, .reactions').forEach((chrome) => chrome.remove());
    text.querySelectorAll('[data-sticker-emoji]').forEach((emoji) => emoji.replaceWith(emoji.dataset.stickerEmoji));
    text.querySelectorAll('img[alt]').forEach((emoji) => emoji.replaceWith(emoji.alt));
    const messageId = element.closest('.bubble')?.getAttribute('data-mid') || '';
    return {messageId, text: text.textContent};
  }));
  if(matches.length === 0 || matches.some(({messageId}) => !/^-?[0-9]{1,20}(?:\\.[0-9]{1,4})?$/.test(messageId))) {
    return 'missing_message_id';
  }
  if(new Set(matches.map(({messageId}) => messageId)).size !== 1) return 'count';
  if(matches.some(({text}) => text !== expected)) return 'content';
  return null;
}`;
  browser = replaceOnce(browser, originalMessageTextAssertion, messageTextAssertion, 'stable message identity assertion');
  browser = replaceOnce(browser,
    '    const count = await message.count();\n    if(count !== 1) failStage(`${stage}_message_count`);',
    '    const failure = await getSingleExactMessageTextFailure(message, text);\n    if(failure) failStage(`${stage}_message_count`);',
    'direct message identity assertion');

  const originalWorkerMethods = `const WORKER_CDP_METHODS = Object.freeze([
  'Network.enable',
  'Audits.enable',
  'Log.enable',
  'Runtime.enable',
  'Runtime.callFunctionOn',
  'Runtime.runIfWaitingForDebugger'
]);`;
  const workerMethods = `const WORKER_CDP_METHODS = Object.freeze([
  'Network.enable',
  'Audits.enable',
  'Log.enable',
  'Runtime.enable',
  'Runtime.evaluate',
  'Runtime.callFunctionOn',
  'Runtime.runIfWaitingForDebugger'
]);`;
  observation = replaceOnce(observation, originalWorkerMethods, workerMethods, 'worker CDP method list');

  const sourceFetchStates = "const SOURCE_FETCH_STATES = Object.freeze(['not_requested', 'ok', 'http_4xx', 'http_5xx', 'failed']);";
  observation = replaceOnce(
    observation,
    sourceFetchStates,
    `${classifyWorkerSourceFetchFromResourceTiming.toString()}\n\n${sourceFetchStates}`,
    'source fetch classifier'
  );

  const observerMethod = `    noteMtprotoSourceFetch(status, targetId) {
      if(!SOURCE_FETCH_STATES.includes(status) || typeof targetId !== 'string' || !targetId) {
        recordObserverError(targetId);
        return;
      }
      if(status === 'not_requested' || state.mtprotoSourceFetch !== 'not_requested') return;
      state.mtprotoSourceChunk = 'mtproto_worker';
      state.mtprotoSourceFetch = status;
      workerSourceLabels.add('mtproto_worker');
      if(status === 'ok') {
        if(state.mtprotoSourceFetchedSequence === null) state.mtprotoSourceFetchedSequence = state.sequence;
        mtprotoSourceTargetIds.add(targetId);
      }
    },
    noteWorkerEvent,`;
  observation = replaceOnce(observation, '    noteWorkerEvent,', observerMethod, 'worker source evidence method');
  observation = replaceOnce(
    observation,
    '  classifyWorkerSource,',
    '  classifyWorkerSource,\n  classifyWorkerSourceFetchFromResourceTiming,',
    'worker source classifier export'
  );

  const collectWorkerSources = `  async function collectMtprotoSourceFetches() {
    await waitForSetup();
    // Shared workers can finish this import before CDP enables Network; use only
    // the matching source status retained in that worker's resource timeline.
    const workerTargets = [...targetSessions.entries()].flatMap(([targetId, sessionId]) => {
      const observer = ownerOf(targetId);
      const target = targetDiagnostics.get(targetId);
      if(!observer || observer.control || target?.type !== 'shared_worker' ||
          observer.state.mtprotoSourceFetch !== 'not_requested') return [];
      return [{targetId, sessionId, observer}];
    });
    const expression = '(' + observation.classifyWorkerSourceFetchFromResourceTiming.toString() +
      ')(performance.getEntriesByType("resource"))';
    await Promise.all(workerTargets.map(async({targetId, sessionId, observer}) => {
      const deadline = Date.now() + 2000;
      do {
        if(observer.state.mtprotoSourceFetch !== 'not_requested') return;
        let status;
        try {
          const response = await sendNested(sessionId, 'Runtime.evaluate', {expression, returnByValue: true});
          status = response?.result?.value;
        } catch {
          observer.recordObserverError(targetId);
          return;
        }
        if(!observation.SOURCE_FETCH_STATES.includes(status)) {
          observer.recordObserverError(targetId);
          return;
        }
        if(status !== 'not_requested') {
          observer.noteMtprotoSourceFetch(status, targetId);
          return;
        }
        if(Date.now() >= deadline) return;
        await new Promise((resolvePromise) => setTimeout(resolvePromise, 100));
      } while(Date.now() < deadline);
    }));
  }

`;
  const waitForSetup = `  async function waitForSetup() {
    while(pendingAttachments.size > 0 || setupPromises.size > 0) {
      await Promise.all([...pendingAttachments.values(), ...setupPromises]);
    }
  }`;
  browser = replaceOnce(browser, waitForSetup, `${collectWorkerSources}${waitForSetup}`, 'worker source recovery');

  const publicWaitForSetup = `    async waitForSetup() {
      await waitForSetup();
    },
    async readPollSendObservation(observer) {`;
  const publicWorkerSourceCollection = `    async collectMtprotoSourceFetches() {
      await collectMtprotoSourceFetches();
    },
    async waitForSetup() {
      await waitForSetup();
    },
    async readPollSendObservation(observer) {`;
  browser = replaceOnce(browser, publicWaitForSetup, publicWorkerSourceCollection, 'worker source recovery export');

  const finalObservation = `    await registry.waitForSetup();
    const apps = {alice, bob};`;
  browser = replaceOnce(
    browser,
    finalObservation,
    `    await registry.waitForSetup();
    await registry.collectMtprotoSourceFetches();
    const apps = {alice, bob};`,
    'final worker evidence collection'
  );

  writeFileSync(observationPath, observation);
  writeFileSync(browserPath, browser);
}

function parseArguments(argv) {
  const values = new Map();
  for(let index = 0; index < argv.length; index += 2) {
    const name = argv[index];
    const value = argv[index + 1];
    if(!['--web-root', '--web-revision'].includes(name) || !value || values.has(name)) {
      throw new Error('expected --web-root and --web-revision');
    }
    values.set(name, value);
  }
  const webRoot = values.get('--web-root');
  const webRevision = values.get('--web-revision');
  if(values.size !== 2 || !/^[0-9a-f]{40}$/.test(webRevision || '')) {
    throw new Error('expected --web-root and a full lowercase --web-revision SHA');
  }
  return {webRoot: resolve(webRoot), webRevision};
}

function main() {
  const {webRoot, webRevision} = parseArguments(process.argv.slice(2));
  let actualRevision;
  try {
    actualRevision = execFileSync('git', ['-C', webRoot, 'rev-parse', 'HEAD'], {encoding: 'utf8'}).trim();
  } catch {
    throw new Error('pinned web checkout is unavailable');
  }
  if(actualRevision !== webRevision) throw new Error('pinned web checkout changed before the observer patch');
  patchRealClientObserver(webRoot);
}

if(process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  try {
    main();
  } catch(error) {
    process.stderr.write(`${error?.message || 'worker observer patch failed'}\n`);
    process.exitCode = 1;
  }
}
