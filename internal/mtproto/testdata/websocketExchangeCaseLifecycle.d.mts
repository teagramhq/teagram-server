export type BoundedCaseResult<T> = {
  status: 'completed' | 'execution_failed' | 'timed_out' | 'teardown_failed',
  quiescent: boolean,
  verificationPassed: boolean,
  value?: T
};

export function runBoundedCase<T>(options: {
  execute: () => T | Promise<T>,
  onTimeout: () => void,
  teardown: () => void | Promise<void>,
  isQuiescent: () => boolean,
  executionBudgetMs: number,
  teardownBudgetMs: number
}): Promise<BoundedCaseResult<T>>;
