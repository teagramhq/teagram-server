import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {test} from 'node:test';

const source = readFileSync(new URL('./run_real_client_browser_flow.mjs', import.meta.url), 'utf8');

test('runner imports pathToFileURL before loading the pinned web contract', () => {
  assert.match(source, /pathToFileURL\(/);
  assert.match(source, /^import \{[^}]*\bpathToFileURL\b[^}]*\} from 'node:url';$/m);
});
