import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { chmod, mkdir, mkdtemp, readFile, readdir, rm, symlink, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { Writable } from "node:stream";
import test from "node:test";

import {
  classifyPlaywrightReport,
  classifyPlaywrightReportFile,
  MAX_PLAYWRIGHT_REPORT_BYTES,
} from "./e2e-admin-diagnostics.mjs";
import { runE2EAdmin } from "./run-e2e-admin.mjs";

const wrapperPath = new URL("./run-e2e-admin.mjs", import.meta.url);
const workflowText = await readFile(new URL("../../.github/workflows/ci.yml", import.meta.url), "utf8");

function report({ errors = [], expected = 0, skipped = 0, unexpected = 0, flaky = 0 } = {}) {
  return JSON.stringify({
    errors,
    stats: { expected, skipped, unexpected, flaky },
  });
}

test("classifies only top-level error count and integer Playwright stats", () => {
  assert.equal(classifyPlaywrightReport(report({ errors: [{ message: "private-error-canary" }] })), "setup");
  assert.equal(classifyPlaywrightReport(report({ expected: 2, unexpected: 1 })), "test-failure");
  assert.equal(classifyPlaywrightReport(report({ errors: [{ message: "private-error-canary" }], unexpected: 1 })), "unavailable");
  assert.equal(classifyPlaywrightReport(report({ errors: [{ message: "private-error-canary" }], expected: 1 })), "unavailable");
  assert.equal(classifyPlaywrightReport(report({ unexpected: 1 }).replace('"unexpected":1', '"unexpected":1.5')), "unavailable");
  assert.equal(classifyPlaywrightReport("not-json"), "unavailable");
  assert.equal(classifyPlaywrightReport("null"), "unavailable");
  assert.equal(classifyPlaywrightReport("{}"), "unavailable");
});

test("duplicate report keys are unavailable, including escaped duplicate count names", () => {
  const validStats = '"stats":{"expected":0,"skipped":0,"unexpected":1,"flaky":0}';
  assert.equal(classifyPlaywrightReport(`{"errors":[{}],"errors":[],${validStats}}`), "unavailable");
  assert.equal(classifyPlaywrightReport(`{"errors":[],${validStats},"stats":{"expected":0,"skipped":0,"unexpected":1,"flaky":0}}`), "unavailable");
  const escapedE = `${String.fromCharCode(92)}u0065`;
  assert.equal(classifyPlaywrightReport(`{"errors":[],"stats":{"expected":0,"skipped":0,"unexpected":0,"unexpect${escapedE}d":1,"flaky":0}}`), "unavailable");
  assert.equal(classifyPlaywrightReport(report({ expected: 1, unexpected: 1 }).replace('"unexpected":1', '"title":"\\"unexpected\\":0,\\"unexpected\\":1","unexpected":1')), "test-failure");
});

test("missing, non-regular, invalid, or oversized reports are unavailable", async (t) => {
  const directory = await mkdtemp(path.join(os.tmpdir(), "e2e-admin-report-test-"));
  t.after(() => rm(directory, { force: true, recursive: true }));

  assert.equal(await classifyPlaywrightReportFile(path.join(directory, "missing.json")), "unavailable");

  const invalidPath = path.join(directory, "invalid.json");
  await writeFile(invalidPath, "{broken");
  assert.equal(await classifyPlaywrightReportFile(invalidPath), "unavailable");

  const oversizedPath = path.join(directory, "oversized.json");
  await writeFile(oversizedPath, Buffer.alloc(MAX_PLAYWRIGHT_REPORT_BYTES + 1, 0x20));
  assert.equal(await classifyPlaywrightReportFile(oversizedPath), "unavailable");

  const symlinkPath = path.join(directory, "symlink.json");
  await symlink(invalidPath, symlinkPath);
  assert.equal(await classifyPlaywrightReportFile(symlinkPath), "unavailable");
});

async function runWrapper(t, { reportKind, status, output = "" }) {
  const directory = await mkdtemp(path.join(os.tmpdir(), "e2e-admin-wrapper-test-"));
  t.after(() => rm(directory, { force: true, recursive: true }));

  const binDirectory = path.join(directory, "bin");
  const runnerTemp = path.join(directory, "runner-temp");
  await mkdir(binDirectory);
  await mkdir(runnerTemp);

  const fakePnpmPath = path.join(binDirectory, "pnpm");
const fakePnpm = `#!/usr/bin/env node
const fs = require("node:fs");
fs.writeFileSync(process.env.FAKE_INVOCATION_PATH, JSON.stringify({
  command: process.argv.slice(2),
  reportPath: process.env.PLAYWRIGHT_JSON_OUTPUT_FILE || null,
}));
const output = process.env.FAKE_PLAYWRIGHT_OUTPUT || "";
if (output) process.stdout.write(output + "\\n");
if (process.env.PLAYWRIGHT_JSON_OUTPUT_FILE && process.env.FAKE_REPORT_KIND !== "missing") {
  const report = process.env.FAKE_REPORT_KIND === "invalid"
    ? "{broken"
    : process.env.FAKE_REPORT_KIND === "setup"
      ? JSON.stringify({ errors: [{ message: "report-canary synthetic-password-canary synthetic-admin-token-canary ::error::forged-report" }], stats: { expected: 0, skipped: 0, unexpected: 0, flaky: 0 } })
      : JSON.stringify({ errors: [], stats: { expected: 0, skipped: 0, unexpected: 2, flaky: 0 } });
  fs.writeFileSync(process.env.PLAYWRIGHT_JSON_OUTPUT_FILE, report);
}
process.exit(Number(process.env.FAKE_PLAYWRIGHT_STATUS));
`;
  await writeFile(fakePnpmPath, fakePnpm);
  await chmod(fakePnpmPath, 0o755);

  const child = spawn(process.execPath, [wrapperPath.pathname], {
    cwd: process.cwd(),
    env: {
      ...process.env,
      PATH: `${binDirectory}${path.delimiter}${process.env.PATH || ""}`,
      RUNNER_TEMP: runnerTemp,
      GITHUB_WORKSPACE: process.cwd(),
      FAKE_REPORT_KIND: reportKind,
      FAKE_INVOCATION_PATH: path.join(directory, "invocation.json"),
      FAKE_PLAYWRIGHT_STATUS: String(status),
      FAKE_PLAYWRIGHT_OUTPUT: output,
    },
    stdio: ["ignore", "pipe", "pipe"],
  });

  const stdout = [];
  const stderr = [];
  child.stdout.on("data", (chunk) => stdout.push(chunk));
  child.stderr.on("data", (chunk) => stderr.push(chunk));
  const exitCode = await new Promise((resolve, reject) => {
    child.once("error", reject);
    child.once("close", resolve);
  });

  assert.deepEqual(await readdir(runnerTemp), []);
  return {
    exitCode,
    stdout: Buffer.concat(stdout).toString("utf8"),
    stderr: Buffer.concat(stderr).toString("utf8"),
    invocation: JSON.parse(await readFile(path.join(directory, "invocation.json"), "utf8")),
    runnerTemp,
  };
}

test("wrapper emits one count-only setup annotation and disables workflow commands around raw output", async (t) => {
  const canary = "workflow-output-canary";
  const forgedCommands = `::error::${canary}\n::stop-commands::forged-token`;
  const result = await runWrapper(t, { reportKind: "setup", status: 1, output: forgedCommands });

  assert.equal(result.exitCode, 1);
  assert.deepEqual(result.invocation.command, ["exec", "playwright", "test", "--reporter=list,html,json"]);
  assert.ok(result.invocation.reportPath.startsWith(`${result.runnerTemp}${path.sep}`));
  const stop = result.stdout.match(/::stop-commands::([0-9a-f]{64})\n/u);
  assert.ok(stop, "raw Playwright output must be behind a per-run command stop token");
  const resume = `::${stop[1]}::\n`;
  const resumeIndex = result.stdout.indexOf(resume);
  assert.ok(resumeIndex > stop.index, "the command stop token must be resumed after Playwright exits");
  const rawOutput = result.stdout.slice(stop.index + stop[0].length, resumeIndex);
  assert.ok(rawOutput.includes(`::error::${canary}`));
  assert.ok(rawOutput.includes("::stop-commands::forged-token"));
  assert.ok(rawOutput.includes("report-canary ::error::forged-report") === false);

  const afterResume = result.stdout.slice(resumeIndex + resume.length);
  assert.deepEqual(afterResume.trimEnd().split("\n"), [
    "::error::e2e-admin failed (category: setup; details redacted)",
  ]);
  assert.doesNotMatch(afterResume, /canary|forged/u);
  assert.equal(result.stderr, "");
});

test("reporter parse failure preserves the Playwright exit status and reports unavailable", async (t) => {
  const result = await runWrapper(t, { reportKind: "invalid", status: 23 });
  assert.equal(result.exitCode, 23);
  assert.match(result.stdout, /::error::e2e-admin failed \(category: unavailable; details redacted\)\n/u);
  assert.doesNotMatch(result.stdout, /broken|report-canary|forged/u);
});

async function runWithInjectedReporting(t, { classifyReportFile, failAnnotationWrite = false }) {
  const directory = await mkdtemp(path.join(os.tmpdir(), "e2e-admin-injected-test-"));
  t.after(() => rm(directory, { force: true, recursive: true }));
  const runnerTemp = path.join(directory, "runner-temp");
  const workspace = path.join(directory, "workspace");
  await mkdir(runnerTemp);
  await mkdir(workspace);

  const stdoutChunks = [];
  const stderrChunks = [];
  const stdout = new Writable({ write(chunk, _encoding, callback) { stdoutChunks.push(Buffer.from(chunk)); callback(); } });
  const stderr = new Writable({ write(chunk, _encoding, callback) { stderrChunks.push(Buffer.from(chunk)); callback(); } });
  const binDirectory = path.join(directory, "bin");
  await mkdir(binDirectory);
  const fakePnpmPath = path.join(binDirectory, "pnpm");
  const fakePnpm = `#!/usr/bin/env node
const fs = require("node:fs");
fs.writeFileSync(process.env.PLAYWRIGHT_JSON_OUTPUT_FILE, ${JSON.stringify(report({ expected: 1, unexpected: 1 }))});
process.exit(23);
`;
  await writeFile(fakePnpmPath, fakePnpm);
  await chmod(fakePnpmPath, 0o755);
  const stdoutText = () => Buffer.concat(stdoutChunks).toString("utf8");
  const stderrText = () => Buffer.concat(stderrChunks).toString("utf8");
  const defaultWrite = (stream, line) => new Promise((resolve, reject) => {
    stream.write(`${line}\n`, (error) => (error ? reject(error) : resolve()));
  });
  const write = async (stream, line) => {
    if (failAnnotationWrite && line.startsWith("::error::")) {
      throw new Error("annotation-writer-private-canary");
    }
    await defaultWrite(stream, line);
  };

  const status = await runE2EAdmin({
    ...process.env,
    PATH: `${binDirectory}${path.delimiter}${process.env.PATH || ""}`,
    GITHUB_WORKSPACE: workspace,
    RUNNER_TEMP: runnerTemp,
  }, {
    classifyReportFile,
    stdout,
    stderr,
    write,
  });

  assert.deepEqual(await readdir(runnerTemp), []);
  return { status, stdout: stdoutText(), stderr: stderrText() };
}

test("classifier rejection after child exit preserves status, cleans the report, and redacts the error", async (t) => {
  const result = await runWithInjectedReporting(t, {
    classifyReportFile: async () => { throw new Error("reporter-private-canary"); },
  });

  assert.equal(result.status, 23);
  assert.match(result.stdout, /category: unavailable; details redacted/u);
  assert.doesNotMatch(result.stdout, /reporter-private-canary/u);
  assert.doesNotMatch(result.stderr, /reporter-private-canary/u);
});

test("annotation write failure after child exit preserves status, cleans the report, and redacts the error", async (t) => {
  const result = await runWithInjectedReporting(t, {
    classifyReportFile: async () => "test-failure",
    failAnnotationWrite: true,
  });

  assert.equal(result.status, 23);
  assert.doesNotMatch(result.stdout, /annotation-writer-private-canary|::error::/u);
  assert.doesNotMatch(result.stderr, /annotation-writer-private-canary/u);
});

test("a successful Playwright run is never reclassified as a failure", async (t) => {
  const result = await runWrapper(t, { reportKind: "test-failure", status: 0 });
  assert.equal(result.exitCode, 0);
  assert.doesNotMatch(result.stdout, /::error::/u);
});

test("hosted CI runs the synthetic verifier and the e2e-admin reporting wrapper", () => {
  assert.match(workflowText, /node --test deploy\/browser-acceptance\/\*\.test\.mjs/u);
  assert.match(workflowText, /run: node deploy\/browser-acceptance\/run-e2e-admin\.mjs/u);
});
