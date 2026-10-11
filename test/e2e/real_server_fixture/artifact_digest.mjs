#!/usr/bin/env node

import {createHash} from 'node:crypto';
import {lstatSync, readdirSync, readFileSync} from 'node:fs';
import {relative, resolve, sep} from 'node:path';

function canonicalFiles(root, producerReport) {
  const files = [];
  const visit = (directory) => {
    for(const entry of readdirSync(directory, {withFileTypes: true})) {
      const file = resolve(directory, entry.name);
      const info = lstatSync(file);
      if(info.isSymbolicLink()) {
        throw new Error('artifact contains a symbolic link');
      }
      if(info.mode & 0o222) {
        throw new Error('artifact is writable');
      }
      if(entry.isDirectory() && info.isDirectory()) {
        visit(file);
      } else if(entry.isFile() && info.isFile() && info.nlink === 1) {
        files.push(file);
      } else {
        throw new Error('artifact contains an unsafe entry');
      }
    }
  };

  const rootInfo = lstatSync(root);
  if(rootInfo.isSymbolicLink() || !rootInfo.isDirectory() || rootInfo.mode & 0o222) {
    throw new Error('artifact directory is writable or unsafe');
  }
  visit(root);
  const pathFor = (file) => relative(root, file).split(sep).join('/');
  if(producerReport) {
    const collator = new Intl.Collator('en-US');
    return files.sort((left, right) => collator.compare(pathFor(left), pathFor(right)));
  }
  return files.sort((left, right) => Buffer.compare(Buffer.from(pathFor(left)), Buffer.from(pathFor(right))));
}

function digest(directory, excludeTargetManifest) {
  const root = resolve(directory);
  const hash = createHash('sha256');
  for(const file of canonicalFiles(root, excludeTargetManifest)) {
    const relativePath = relative(root, file).split(sep).join('/');
    if(excludeTargetManifest && relativePath === 'mtproto-target.json') continue;
    const contents = readFileSync(file);
    hash.update(relativePath + '\0' + contents.length + '\0');
    hash.update(contents);
    hash.update('\0');
  }
  return 'sha256:' + hash.digest('hex');
}

const mode = process.argv[2];
const directory = process.argv[3];
if(!['artifact', 'producer-report'].includes(mode) || !directory) {
  process.stderr.write('digest mode and artifact directory are required\n');
  process.exitCode = 2;
} else {
  try {
    process.stdout.write(digest(directory, mode === 'producer-report') + '\n');
  } catch {
    process.stderr.write('canonical artifact digest failed\n');
    process.exitCode = 1;
  }
}
