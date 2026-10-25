import { execFileSync } from "node:child_process";
import { mkdtemp, readFile, readdir, rm, mkdir } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, resolve, basename } from "node:path";
import { fileURLToPath } from "node:url";
import sharp from "sharp";

const toolDir = dirname(fileURLToPath(import.meta.url));
const repoDir = resolve(toolDir, "../..");
const artworkDir = join(repoDir, "internal/reactionassets");
const tag = "v14.0.2";
const commit = "7a3dad4a4da30497093dab22eafba135f02308e1";

const manifest = JSON.parse(await readFile(join(artworkDir, "manifest.json"), "utf8"));
if (manifest.length !== 73 || new Set(manifest.map(({ emoji }) => emoji)).size !== 73) {
  throw new Error("reaction manifest must contain 73 unique emoticons");
}
for (const { svg } of manifest) {
  if (typeof svg !== "string" || basename(svg) !== svg || !/^[0-9a-f]+(?:-[0-9a-f]+)*\.svg$/.test(svg)) {
    throw new Error(`invalid Twemoji source filename: ${svg}`);
  }
}

const tempDir = await mkdtemp(join(tmpdir(), "teagram-twemoji-"));
try {
  const upstreamDir = join(tempDir, "twemoji");
  execFileSync("git", ["clone", "--quiet", "--depth", "1", "--branch", tag, "https://github.com/twitter/twemoji.git", upstreamDir], { stdio: "inherit" });
  const actualCommit = execFileSync("git", ["-C", upstreamDir, "rev-parse", "HEAD"], { encoding: "utf8" }).trim();
  if (actualCommit !== commit) {
    throw new Error(`Twemoji ${tag} resolved to ${actualCommit}, expected ${commit}`);
  }

  execFileSync("cargo", ["build", "--locked", "--release", "--manifest-path", join(toolDir, "Cargo.toml")], {
    cwd: repoDir,
    stdio: "inherit",
  });
  const converter = join(toolDir, "target/release/reaction-artwork");
  const sourceDir = join(upstreamDir, "assets/svg");
  const staticDir = join(artworkDir, "static");
  const centerDir = join(artworkDir, "center");
  const tgsDir = join(artworkDir, "tgs");
  await mkdir(staticDir, { recursive: true });
  await mkdir(centerDir, { recursive: true });
  await mkdir(tgsDir, { recursive: true });

  const staticNames = new Set(manifest.map(({ svg }) => `${svg.slice(0, -4)}.webp`));
  const centerNames = new Set(manifest.map(({ svg }) => `${svg.slice(0, -4)}.webp`));
  const tgsNames = new Set(manifest.map(({ svg }) => `${svg.slice(0, -4)}.tgs`));
  for (const [dir, expected] of [[staticDir, staticNames], [centerDir, centerNames], [tgsDir, tgsNames]]) {
    for (const name of await readdir(dir)) {
      if (!expected.has(name)) await rm(join(dir, name), { recursive: true });
    }
  }

  for (const { svg } of manifest) {
    const input = join(sourceDir, svg);
    const stem = svg.slice(0, -4);
    const staticPath = join(staticDir, `${stem}.webp`);
    const centerPath = join(centerDir, `${stem}.webp`);
    await sharp(input, { density: 1024 })
      .resize(512, 512, { fit: "contain", background: { r: 0, g: 0, b: 0, alpha: 0 } })
      .webp({ lossless: true, quality: 100, effort: 6 })
      .toFile(staticPath);
    await sharp(staticPath)
      .trim({ background: { r: 0, g: 0, b: 0, alpha: 0 }, threshold: 0 })
      .resize(256, 256, { fit: "contain", background: { r: 0, g: 0, b: 0, alpha: 0 } })
      .extend({ top: 128, right: 128, bottom: 128, left: 128, background: { r: 0, g: 0, b: 0, alpha: 0 } })
      .webp({ lossless: true, quality: 100, effort: 6 })
      .toFile(centerPath);
    execFileSync(converter, [input, join(tgsDir, `${stem}.tgs`)], { cwd: repoDir, stdio: "inherit" });
  }
} finally {
  await rm(tempDir, { recursive: true, force: true });
}
