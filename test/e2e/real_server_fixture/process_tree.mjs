import {spawn} from 'node:child_process';

const children = new WeakMap();
const POLL_INTERVAL_MS = 25;
const DEFAULT_GRACE_MS = 5_000;
export const FIXTURE_CLEANUP_TIMEOUT_MS = 90_000;

export function spawnTracked(command, args, options) {
  const detached = process.platform !== 'win32';
  const child = spawn(command, args, {...options, detached});
  children.set(child, {detached, pid: child.pid, termination: undefined});
  return child;
}

function signalGroup(pid, signal) {
  try {
    process.kill(-pid, signal);
    return true;
  } catch(error) {
    if(error.code === 'ESRCH') return false;
    return false;
  }
}

function groupExists(pid) {
  try {
    process.kill(-pid, 0);
    return true;
  } catch(error) {
    return error.code !== 'ESRCH';
  }
}

async function waitForGroupExit(pid, graceMs) {
  const deadline = Date.now() + graceMs;
  while(groupExists(pid)) {
    if(Date.now() >= deadline) return false;
    await new Promise((resolve) => setTimeout(resolve, POLL_INTERVAL_MS));
  }
  return true;
}

function childRunning(child) {
  return child.exitCode === null && child.signalCode === null;
}

async function waitForChildExit(child, graceMs) {
  const deadline = Date.now() + graceMs;
  while(childRunning(child)) {
    if(Date.now() >= deadline) return false;
    await new Promise((resolve) => setTimeout(resolve, POLL_INTERVAL_MS));
  }
  return true;
}

export function terminate(child, signal, {graceMs = DEFAULT_GRACE_MS} = {}) {
  if(!child) return Promise.resolve(true);
  const target = children.get(child);
  if(target?.detached && process.platform !== 'win32' && Number.isInteger(target.pid)) {
    if(signal === 'SIGKILL') {
      if(groupExists(target.pid)) signalGroup(target.pid, signal);
      return waitForGroupExit(target.pid, graceMs);
    }
    if(!target.termination) {
      if(!groupExists(target.pid)) return Promise.resolve(true);
      signalGroup(target.pid, signal);
      target.termination = (async () => {
        if(await waitForGroupExit(target.pid, graceMs)) return true;
        signalGroup(target.pid, 'SIGKILL');
        return waitForGroupExit(target.pid, graceMs);
      })();
    }
    return target.termination;
  }

  if(signal !== 'SIGKILL' && target?.termination) return target.termination;
  if(!Number.isInteger(child.pid) || !childRunning(child)) return Promise.resolve(true);
  try {
    child.kill(signal);
  } catch {
    return Promise.resolve(false);
  }
  if(signal === 'SIGKILL') return waitForChildExit(child, graceMs);
  if(!target?.termination) {
    if(!target) return waitForChildExit(child, graceMs);
    target.termination = (async () => {
      if(await waitForChildExit(child, graceMs)) return true;
      try {
        child.kill('SIGKILL');
      } catch {
        return false;
      }
      return waitForChildExit(child, graceMs);
    })();
  }
  return target.termination;
}

export function terminateFixture(child, signal) {
  return terminate(child, signal, {graceMs: FIXTURE_CLEANUP_TIMEOUT_MS});
}
