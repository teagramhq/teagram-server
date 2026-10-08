#!/usr/bin/env node
// Harness-owned recomputation of the producer's artifact digest.
// The producer commits to sha256 over "path\0length\0bytes\0" for every emitted
// file except the target manifest, in its own runtime's collation order. The
// fixture recomputes that value here instead of trusting the caller's manifest.

import { createHash } from 'node:crypto';
import { lstatSync, readFileSync, readdirSync } from 'node:fs';
import { relative, resolve, sep } from 'node:path';

const MANIFEST = 'mtproto-target.json';
const directory = process.argv[2];

function stagedFiles(root) {
  const files = [];
  const visit = (current) => {
    for (const entry of readdirSync(current, { withFileTypes: true })) {
      const path = `${current}/${entry.name}`;
      const stats = lstatSync(path);
      if (stats.isSymbolicLink()) throw new Error('staged artifact contains a symlink');
      if (stats.isDirectory()) visit(path);
      else if (stats.isFile()) files.push(path);
      else throw new Error('staged artifact contains a non-regular entry');
    }
  };
  visit(resolve(directory));
  return files.sort((left, right) => relative(directory, left).localeCompare(relative(directory, right)));
}

const hash = createHash('sha256');
for (const path of stagedFiles(directory)) {
  const relativePath = relative(directory, path).split(sep).join('/');
  if (relativePath === MANIFEST) continue;
  const contents = readFileSync(path);
  hash.update(`${relativePath}\0${contents.length}\0`);
  hash.update(contents);
  hash.update('\0');
}

process.stdout.write(`${JSON.stringify({
  digest: `sha256:${hash.digest('hex')}`,
  collationLocale: Intl.Collator().resolvedOptions().locale,
})}\n`);
