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
      browser.includes('waitForDirectMessageCount')) {
    throw new Error('pinned web observer is already patched');
  }

  const existingPollOptionSelector =
    "const POLL_ACTIVE_OPTION_SELECTOR = '[data-poll-option-idx]:not(.fade-2-exit-active)';";
  if(browser.includes(existingPollOptionSelector)) {
    browser = replaceOnce(browser,
      existingPollOptionSelector,
      "const POLL_ACTIVE_OPTION_SELECTOR = '[data-poll-option-idx]:not(.fade-2-exit-active):not(.fade-2-exit-to)';",
      'active poll option selector');
  } else {
    browser = replaceOnce(browser,
      "const POLL_ANSWER_TWO = 'browser-ci-poll-answer-two';",
      "const POLL_ANSWER_TWO = 'browser-ci-poll-answer-two';\nconst ACTIVE_POLL_OPTION_SELECTOR = '[data-poll-option-idx]:not(.fade-2-exit-active):not(.fade-2-exit-to)';",
      'active poll option selector');

    browser = replaceOnce(browser,
      `function getPollOptionClickTarget(poll, answerText) {
  return poll.locator('[data-poll-option-idx]')
  .filter({hasText: answerText})
  .locator(':scope > div')
  .first();
}`,
      `function getPollOptionClickTarget(poll, answerText) {
  return poll.locator(ACTIVE_POLL_OPTION_SELECTOR)
  .filter({hasText: answerText})
  .locator(':scope > div')
  .first();
}`,
      'active poll click target');

    browser = replaceOnce(browser,
      "const options = bubble ? [...bubble.querySelectorAll('[data-poll-option-idx]')] : [];",
      "const options = bubble ? [...bubble.querySelectorAll('[data-poll-option-idx]')]\n    .filter((element) => !element.classList.contains('fade-2-exit-active') && !element.classList.contains('fade-2-exit-to')) : [];",
      'active poll results diagnostics');

    browser = replaceOnce(browser,
      `    if(message.querySelectorAll('[data-poll-option-idx]').length !== 2) return false;
    return expectedAnswers.every((answerText) => [...message.querySelectorAll('*')]
    .filter((node) => node.children.length === 0 && node.textContent?.trim() === answerText).length === 1);`,
      `    const options = [...message.querySelectorAll('[data-poll-option-idx]')]
    .filter((element) => !element.classList.contains('fade-2-exit-active') && !element.classList.contains('fade-2-exit-to'));
    if(options.length !== 2) return false;
    return expectedAnswers.every((answerText) => options.filter((option) => [...option.querySelectorAll('*')]
    .some((node) => node.children.length === 0 && node.textContent?.trim() === answerText)).length === 1);`,
      'active poll message predicate');

    browser = replaceOnce(browser,
      "const answers = poll.locator('[data-poll-option-idx]');",
      'const answers = poll.locator(ACTIVE_POLL_OPTION_SELECTOR);',
      'active poll content assertion');
    browser = replaceOnce(browser,
      "if(await poll.getByText(answerText, {exact: true}).count() !== 1) {",
      'if(await answers.filter({hasText: answerText}).count() !== 1) {',
      'active poll answer assertion');

    browser = replaceOnce(browser,
      "const options = [...poll.querySelectorAll('[data-poll-option-idx]')];",
      "const options = [...poll.querySelectorAll('[data-poll-option-idx]')]\n    .filter((element) => !element.classList.contains('fade-2-exit-active') && !element.classList.contains('fade-2-exit-to'));",
      'active poll content diagnostics');
    browser = replaceOnce(browser,
      'answerTextCounts: expectedAnswers.map((answerText) => countText(poll, answerText, false)),',
      'answerTextCounts: expectedAnswers.map((answerText) => options.reduce((count, option) => count + countText(option, answerText, false), 0)),',
      'active poll answer text diagnostics');
    browser = replaceOnce(browser,
      'visibleAnswerTextCounts: expectedAnswers.map((answerText) => countText(poll, answerText, true)),',
      'visibleAnswerTextCounts: expectedAnswers.map((answerText) => options.reduce((count, option) => count + countText(option, answerText, true), 0)),',
      'active poll visible answer diagnostics');

    browser = replaceOnce(browser,
      "const selectedAnswer = poll.locator('[data-poll-option-idx]').filter({hasText: POLL_ANSWER_ONE});",
      'const selectedAnswer = poll.locator(ACTIVE_POLL_OPTION_SELECTOR).filter({hasText: POLL_ANSWER_ONE});',
      'active selected poll answer');
    browser = replaceOnce(browser,
      'getPollOptionClickTarget, getPollResultIndicator,',
      'getPollOptionClickTarget, getPollResultIndicator, assertPollContent,',
      'poll content assertion export');
  }

  const uniquePeerSearchWaiter = `async function waitForUniquePeerSearchResult(page, resultsSelector, username, stage) {
  const usernameMatches = page.locator(\`${'${resultsSelector}'}:visible\`).filter({hasText: username});
  const deadline = Date.now() + 25_000;
  while(Date.now() < deadline) {
    const remaining = deadline - Date.now();
    try {
      await page.waitForFunction(({resultsSelector, username}) => {
        const matches = [...document.querySelectorAll(resultsSelector)].filter((row) => {
          const rect = row.getBoundingClientRect();
          const style = window.getComputedStyle(row);
          return rect.width > 0 && rect.height > 0 && style.display !== 'none' && style.visibility !== 'hidden' &&
            (row.textContent || '').includes(username);
        });
        return matches.length === 1;
      }, {resultsSelector, username}, {timeout: remaining});
    } catch {
      const matchCount = await usernameMatches.count().catch(() => 0);
      failStage(stage + (matchCount > 0 ? '_search_result_not_unique' : '_search_results'));
    }

    const matchCount = await usernameMatches.count().catch(() => 0);
    if(matchCount !== 1) continue;
    const peerId = await usernameMatches.getAttribute('data-peer-id');
    if(peerId) return {selectedRow: usernameMatches, peerId};
  }

  const matchCount = await usernameMatches.count().catch(() => 0);
  failStage(stage + (matchCount > 0 ? '_search_result_not_unique' : '_search_results'));
}
`;
  const peerSelection = 'async function selectUniquePeer(page, inputSelector, resultsSelector, username, stage) {';
  browser = replaceOnce(browser, peerSelection, `${uniquePeerSearchWaiter}\n${peerSelection}`, 'unique peer search waiter');

  const oldPeerSelection = `  const rows = page.locator(resultsSelector);
  await rows.first().waitFor({state: 'visible', timeout: 25_000}).catch(() => failStage(\`${'${stage}'}_search_results\`));
  const count = await rows.count();
  const usernameMatches = rows.filter({hasText: username});
  const matchCount = await usernameMatches.count();
  let selectedRow;
  if(matchCount === 1) {
    selectedRow = usernameMatches;
  } else {
    const visible = [];
    for(let index = 0; index < count; index++) {
      const row = rows.nth(index);
      if(await row.isVisible()) visible.push(row);
    }
    if(visible.length !== 1) failStage(\`${'${stage}'}_search_result_not_unique\`);
    selectedRow = visible[0];
  }`;
  const settledPeerSelection = `  const {selectedRow, peerId} = await waitForUniquePeerSearchResult(page, resultsSelector, username, stage);`;
  browser = replaceOnce(browser, oldPeerSelection, settledPeerSelection, 'unique peer search selection');
  browser = replaceOnce(browser,
    `  const peerId = await selectedRow.getAttribute('data-peer-id');
  if(!peerId) failStage(\`${'${stage}'}_peer_identity\`);`,
    `  if(!peerId) failStage(\`${'${stage}'}_peer_identity\`);`,
    'unique peer identity extraction');

  const oldCustomEmojiMessageWaiter = `async function waitForCustomEmojiMessage(page, text, docId, stage) {
  setPageScenarioStep(page, stage);
  await page.waitForFunction(({text, docId}) => {
    const messages = [...document.querySelectorAll('#column-center .bubble .message')]
    .filter((message) => (message.textContent || '').includes(text));
    return messages.length === 1 && [...messages[0].querySelectorAll('.custom-emoji')]
    .some((emoji) => emoji.dataset.docId === docId);
  }, {text, docId}, {timeout: 30_000}).catch(() => failStage(stage));
}`;
  const settledCustomEmojiMessageWaiter = `async function waitForCustomEmojiMessage(page, text, docId, stage) {
  setPageScenarioStep(page, stage);
  const messageIdHandle = await page.waitForFunction(({text, docId}) => {
    const messages = [...document.querySelectorAll('#column-center .bubble .message')]
    .filter((message) => (message.textContent || '').includes(text));
    const messageIds = new Set();
    for(const message of messages) {
      const bubble = message.closest('.bubble');
      const messageId = bubble?.getAttribute('data-mid') || '';
      const hasEmoji = [...message.querySelectorAll('.custom-emoji')]
      .some((emoji) => emoji.dataset.docId === docId);
      if(hasEmoji && /^-?[0-9]{1,20}$/.test(messageId)) messageIds.add(messageId);
    }
    return messageIds.size === 1 ? [...messageIds][0] : false;
  }, {text, docId}, {timeout: 30_000}).catch(() => failStage(stage));
  const messageId = await messageIdHandle.jsonValue();
  await messageIdHandle.dispose();
  return messageId;
}`;
  browser = replaceOnce(browser, oldCustomEmojiMessageWaiter, settledCustomEmojiMessageWaiter, 'settled custom emoji message identity');

  const oldCustomReactionWaiter = `async function waitForCustomMessageReaction(page, text, docId, expectedCount, stage) {
  setPageScenarioStep(page, stage);
  await page.waitForFunction(({text, docId, expectedCount}) => {
    const messages = [...document.querySelectorAll('#column-center .bubble .message')]
    .filter((message) => (message.textContent || '').includes(text));
    if(messages.length !== 1) return false;
    const reactions = [...messages[0].closest('.bubble').querySelectorAll('reaction-element')];
    const reaction = reactions.find((element) => {
      return element.reactionCount?.reaction?._ === 'reactionCustomEmoji' &&
        String(element.reactionCount.reaction.document_id) === docId;
    });
    return expectedCount === 0 ? !reaction : reaction?.reactionCount.count === expectedCount;
  }, {text, docId, expectedCount}, {timeout: 30_000}).catch(() => failStage(stage));
}`;
  const settledCustomReactionWaiter = `async function waitForCustomMessageReaction(page, text, docId, messageId, expectedCount, stage) {
  setPageScenarioStep(page, stage);
  if(!/^-?[0-9]{1,20}$/.test(messageId || '')) failStage(stage + '_message_identity');
  await page.waitForFunction(({text, docId, messageId, expectedCount}) => {
    const messages = [...document.querySelectorAll('#column-center .bubble .message')]
    .filter((message) => {
      const bubble = message.closest('.bubble');
      return bubble?.getAttribute('data-mid') === messageId && (message.textContent || '').includes(text);
    });
    if(messages.length === 0) return false;
    const reactions = messages.flatMap((message) => [...message.closest('.bubble').querySelectorAll('reaction-element')]);
    const reaction = reactions.find((element) => {
      return element.reactionCount?.reaction?._ === 'reactionCustomEmoji' &&
        String(element.reactionCount.reaction.document_id) === docId;
    });
    return expectedCount === 0 ? !reaction : reaction?.reactionCount.count === expectedCount;
  }, {text, docId, messageId, expectedCount}, {timeout: 30_000}).catch(() => failStage(stage));
}`;
  browser = replaceOnce(browser, oldCustomReactionWaiter, settledCustomReactionWaiter, 'custom reaction message identity');

  const oldCustomReactionAction = 'async function reactWithCustomEmoji(page, otherPage, text, docId, stage) {';
  const settledCustomReactionAction = 'async function reactWithCustomEmoji(page, otherPage, text, docId, stage, messageId, otherMessageId) {';
  browser = replaceOnce(browser, oldCustomReactionAction, settledCustomReactionAction, 'custom reaction message identity arguments');
  browser = replaceOnce(browser,
    "  const message = page.locator('#column-center .bubble .message').filter({hasText: text});",
    "  const message = page.locator('#column-center .bubble[data-mid=\"' + messageId + '\"] .message').filter({hasText: text}).first();",
    'custom reaction actor message selection');
  browser = replaceOnce(browser,
    '  await waitForCustomMessageReaction(page, text, docId, 1,',
    '  await waitForCustomMessageReaction(page, text, docId, messageId, 1,',
    'custom reaction actor identity');
  browser = replaceOnce(browser,
    '  await waitForCustomMessageReaction(otherPage, text, docId, 1,',
    '  await waitForCustomMessageReaction(otherPage, text, docId, otherMessageId, 1,',
    'custom reaction receiver identity');

  browser = replaceOnce(browser,
    "  await waitForCustomEmojiMessage(alice.page, messageText, docId, 'alice_custom_emoji_send_visible');",
    "  const aliceMessageId = await waitForCustomEmojiMessage(alice.page, messageText, docId, 'alice_custom_emoji_send_visible');",
    'custom message sender identity');
  browser = replaceOnce(browser,
    "  await waitForCustomEmojiMessage(bob.page, messageText, docId, 'bob_custom_emoji_receive_visible');",
    "  const bobMessageId = await waitForCustomEmojiMessage(bob.page, messageText, docId, 'bob_custom_emoji_receive_visible');",
    'custom message receiver identity');
  browser = replaceOnce(browser,
    "  await reactWithCustomEmoji(bob.page, alice.page, messageText, docId, 'bob_custom_emoji_reaction');",
    "  await reactWithCustomEmoji(bob.page, alice.page, messageText, docId, 'bob_custom_emoji_reaction', bobMessageId, aliceMessageId);",
    'custom reaction sender and receiver identities');

  const directMessageWaiter = `async function waitForDirectMessageCount(page, message, stage, timeoutMs = GROUP_CREATED_STEP_TIMEOUT_MS) {
  currentStage = stage;
  await startGroupMessageTrace(page, stage);
  const deadline = Date.now() + Math.min(timeoutMs, GROUP_MESSAGE_SETTLE_TIMEOUT_MS);
  let stableSince;
  let lastResult = 'pending';
  while(Date.now() <= deadline) {
    let sample;
    try {
      sample = await message.evaluateAll((elements) => {
        // Count each bubble element. Its data-mid is diagnostic only while the
        // optimistic message is replaced by its sent copy.
        const bubbleElements = [...new Set(elements.map((element) => element.closest('.bubble')).filter(Boolean))];
        const bubbles = bubbleElements.map((bubble) => {
          const id = bubble.getAttribute('data-mid') || '';
          const sendState = bubble.classList.contains('is-error') ? 'failed' :
            bubble.classList.contains('is-sending') ? 'pending' : 'sent';
          return {
            messageId: /^-?[0-9]{1,20}(\\.[0-9]{1,4})?$/.test(id) ? id : null,
            sendState
          };
        });
        const serverIds = bubbles.filter((bubble) => bubble.sendState === 'sent').map((bubble) => bubble.messageId).filter(Boolean);
        return {
          count: bubbles.length,
          bubbles,
          distinctIds: new Set(bubbles.map((bubble) => bubble.messageId).filter(Boolean)).size > 1,
          distinctServerIds: new Set(serverIds).size > 1
        };
      });
    } catch {
      groupMessageTrace.result = 'read_error';
      return false;
    }
    groupMessageTrace.samples.push(sample);
    lastResult = groupMessageSampleResult(sample);
    if(['disappeared', 'failed_bubble', 'multiple_ids', 'duplicate_bubbles'].includes(lastResult)) {
      groupMessageTrace.result = lastResult;
      return false;
    }
    if(lastResult === 'sent') {
      if(stableSince === undefined) stableSince = Date.now();
      else if(Date.now() - stableSince >= GROUP_MESSAGE_STABLE_WINDOW_MS) {
        groupMessageTrace.result = 'settled';
        return true;
      }
    } else {
      stableSince = undefined;
    }

    const remaining = deadline - Date.now();
    if(remaining <= 0) break;
    try {
      await page.waitForTimeout(Math.min(GROUP_MESSAGE_SAMPLE_INTERVAL_MS, remaining));
    } catch {
      groupMessageTrace.result = 'read_error';
      return false;
    }
  }

  groupMessageTrace.result = lastResult === 'pending' || lastResult === 'transient_pair' ||
    lastResult === 'missing_message_id' ? 'settle_timeout' : 'stability_timeout';
  return false;
}
`;
  const legacyDirectMessageAssertion = '    await page.waitForTimeout(600);\n    currentStage = stage;\n    const count = await message.count();\n    if(count !== 1) failStage(`${stage}_message_count`);';
  const currentDirectMessageAssertion = '    await waitForSingleSentMessage(message, text, timeoutMs).catch(() => failStage(`${stage}_message_count`));';
  const settledDirectMessageAssertion = '    const messageStage = `${stage}_message_count`;\n    const settled = await waitForDirectMessageCount(page, message, messageStage, timeoutMs);\n    if(!settled) failStage(messageStage);';
  if(browser.includes(legacyDirectMessageAssertion)) {
    browser = replaceOnce(browser,
      'async function sendText(page, text, stage, chat, timeoutMs = 30_000, groupTitle) {',
      `${directMessageWaiter}\nasync function sendText(page, text, stage, chat, timeoutMs = 30_000, groupTitle) {`,
      'legacy direct message settle helper');
    browser = replaceOnce(browser, legacyDirectMessageAssertion, settledDirectMessageAssertion, 'direct message settle assertion');
    const legacyDirectMessageExport = 'waitForGroupMessageCount, waitForPollMessage';
    const currentDirectMessageExport = 'waitForGroupMessageCount, waitForSingleSentMessage,';
    if(browser.includes(legacyDirectMessageExport)) {
      browser = replaceOnce(browser, legacyDirectMessageExport,
        'waitForGroupMessageCount, waitForDirectMessageCount, waitForPollMessage',
        'direct message settle helper export');
    } else if(browser.includes(currentDirectMessageExport)) {
      browser = replaceOnce(browser, currentDirectMessageExport,
        'waitForGroupMessageCount, waitForDirectMessageCount, waitForSingleSentMessage,',
        'direct message settle helper export');
    } else {
      throw new Error('pinned web runner has no direct message helper export insertion point');
    }
  } else if(!browser.includes(currentDirectMessageAssertion)) {
    throw new Error('pinned web runner has no supported direct message assertion');
  }

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

  const finalObservation = [
    `    await registry.waitForSetup();
    const apps = {alice, bob, ...(guest ? {guest} : {})};`,
    `    await registry.waitForSetup();
    const apps = {alice, bob};`
  ].find((candidate) => browser.includes(candidate));
  if(!finalObservation) {
    throw new Error('pinned web runner has no supported final worker evidence insertion point');
  }
  browser = replaceOnce(
    browser,
    finalObservation,
    finalObservation.replace(
      '    const apps =',
      '    await registry.collectMtprotoSourceFetches();\n    const apps ='
    ),
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
