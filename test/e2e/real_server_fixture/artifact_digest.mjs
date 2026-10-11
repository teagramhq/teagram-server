#!/usr/bin/env node

import {createHash} from 'node:crypto';
import {lstatSync, readdirSync, readFileSync} from 'node:fs';
import {relative, resolve, sep} from 'node:path';

function canonicalFiles(root) {
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
  const collator = new Intl.Collator('en-US');
  return files.sort((left, right) => collator.compare(pathFor(left), pathFor(right)));
}

function digest(directory) {
  const root = resolve(directory);
  const hash = createHash('sha256');
  for(const file of canonicalFiles(root)) {
    const relativePath = relative(root, file).split(sep).join('/');
    if(relativePath === 'mtproto-target.json') continue;
    const contents = readFileSync(file);
    hash.update(relativePath + '\0' + contents.length + '\0');
    hash.update(contents);
    hash.update('\0');
  }
  return 'sha256:' + hash.digest('hex');
}

const directory = process.argv[2];
if(!directory) {
  process.stderr.write('artifact directory is required\n');
  process.exitCode = 2;
} else {
  try {
    process.stdout.write(digest(directory) + '\n');
  } catch {
    process.stderr.write('canonical artifact digest failed\n');
    process.exitCode = 1;
  }
}
