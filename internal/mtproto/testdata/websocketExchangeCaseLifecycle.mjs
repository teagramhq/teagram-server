const DEADLINE = Symbol('deadline');
const setNativeTimeout = globalThis.setTimeout.bind(globalThis);
const clearNativeTimeout = globalThis.clearTimeout.bind(globalThis);

function settleWithin(promise, milliseconds) {
  return new Promise((resolve) => {
    const timer = setNativeTimeout(() => resolve(DEADLINE), milliseconds);
    promise.then((value) => {
      clearNativeTimeout(timer);
      resolve(value);
    }, () => {
      clearNativeTimeout(timer);
      resolve(DEADLINE);
    });
  });
}

export async function runBoundedCase({
  execute,
  onTimeout,
  teardown,
  isQuiescent,
  executionBudgetMs,
  teardownBudgetMs
}) {
  const execution = Promise.resolve().then(execute).then(
    (value) => ({status: 'completed', value}),
    () => ({status: 'execution_failed'})
  );
  const executionResult = await settleWithin(execution, executionBudgetMs);
  const timedOut = executionResult === DEADLINE;
  let timeoutRecorded = true;
  if(timedOut) {
    try {
      onTimeout();
    } catch{
      timeoutRecorded = false;
    }
  }

  const cleanup = Promise.resolve().then(teardown).then(
    () => true,
    () => false
  );
  const teardownResult = await settleWithin(Promise.all([execution, cleanup]), teardownBudgetMs);
  let quiescent = false;
  if(teardownResult !== DEADLINE && teardownResult[1]) {
    try {
      quiescent = isQuiescent() === true;
    } catch{
      quiescent = false;
    }
  }

  let status = 'teardown_failed';
  if(teardownResult !== DEADLINE && quiescent && timeoutRecorded) {
    if(timedOut) status = 'timed_out';
    else if(executionResult.status === 'completed') status = 'completed';
    else status = 'execution_failed';
  }

  return {
    status,
    quiescent,
    verificationPassed: status === 'completed',
    value: status === 'completed' ? executionResult.value : undefined
  };
}
