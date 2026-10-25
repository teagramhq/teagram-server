import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import test from 'node:test';
import sharp from 'sharp';

const renderer = fileURLToPath(new URL('./render.mjs', import.meta.url));

test('renders transparent WebP with the longest side at 512 pixels', async () => {
  const svg = '<svg xmlns="http://www.w3.org/2000/svg" width="200" height="100" viewBox="0 0 200 100"><circle cx="100" cy="50" r="30" fill="#ff0000"/></svg>';
  const result = spawnSync(process.execPath, [renderer], {
    cwd: path.dirname(renderer),
    input: `${JSON.stringify({ svg: Buffer.from(svg).toString('base64') })}\n`,
    encoding: 'utf8',
  });

  assert.equal(result.status, 0, result.stderr);
  const response = JSON.parse(result.stdout.trim());
  assert.equal(response.error, undefined, response.error);
  const webp = Buffer.from(response.webp, 'base64');
  assert.equal(webp.toString('ascii', 0, 4), 'RIFF');
  assert.equal(webp.toString('ascii', 8, 12), 'WEBP');

  const { data, info } = await sharp(webp).ensureAlpha().raw().toBuffer({ resolveWithObject: true });
  assert.equal(info.width, 512);
  assert.equal(info.height, 256);
  assert.equal(data[3], 0);
  const center = (128 * info.width + 256) * 4;
  assert.ok(data[center] > 200);
  assert.equal(data[center + 3], 255);
});
