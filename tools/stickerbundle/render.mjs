import readline from 'node:readline';
import { Resvg } from '@resvg/resvg-js';
import sharp from 'sharp';

const FONT_OPTIONS = { font: { loadSystemFonts: false } };

async function render(svgBase64) {
  const svg = Buffer.from(svgBase64, 'base64');
  const original = new Resvg(svg, FONT_OPTIONS);
  if (!Number.isFinite(original.width) || !Number.isFinite(original.height) || original.width <= 0 || original.height <= 0) {
    throw new Error('SVG has invalid dimensions');
  }

  const fitTo = original.width >= original.height
    ? { mode: 'width', value: 512 }
    : { mode: 'height', value: 512 };
  const png = new Resvg(svg, { ...FONT_OPTIONS, fitTo }).render().asPng();
  return sharp(png)
    .webp({ quality: 90, alphaQuality: 100, effort: 3, exact: true })
    .toBuffer();
}

const input = readline.createInterface({ input: process.stdin });
for await (const line of input) {
  try {
    const request = JSON.parse(line);
    const webp = await render(request.svg);
    process.stdout.write(`${JSON.stringify({ webp: webp.toString('base64') })}\n`);
  } catch (error) {
    process.stdout.write(`${JSON.stringify({ error: error.message })}\n`);
  }
}
