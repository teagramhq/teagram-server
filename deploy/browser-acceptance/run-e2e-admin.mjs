import { randomBytes } from "node:crypto";
import { spawn } from "node:child_process";
import { mkdtemp, rm } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { pathToFileURL } from "node:url";

import { classifyPlaywrightReportFile } from "./e2e-admin-diagnostics.mjs";

function pathIsInside(parent, candidate) {
  const relative = path.relative(parent, candidate);
  return relative === "" || (relative !== ".." && !relative.startsWith(`..${path.sep}`) && !path.isAbsolute(relative));
}

function reportDirectoryParent(env) {
  const runnerTemp = env.RUNNER_TEMP;
  if (typeof runnerTemp !== "string" || !path.isAbsolute(runnerTemp)) return null;

  const candidate = path.resolve(runnerTemp);
  const workspace = path.resolve(env.GITHUB_WORKSPACE || process.cwd());
  if (pathIsInside(workspace, candidate)) return null;
  return candidate;
}

function writeLine(stream, line) {
  return new Promise((resolve, reject) => {
    stream.write(`${line}\n`, (error) => (error ? reject(error) : resolve()));
  });
}

function signalExitCode(signal) {
  const signalNumber = os.constants.signals[signal];
  return typeof signalNumber === "number" ? 128 + signalNumber : 1;
}

function runPlaywright(reportPath, env, suppressOutput) {
  const childEnv = { ...env, PLAYWRIGHT_HTML_OPEN: "never" };
  delete childEnv.PLAYWRIGHT_JSON_OUTPUT_DIR;
  delete childEnv.PLAYWRIGHT_JSON_OUTPUT_NAME;
  delete childEnv.PLAYWRIGHT_JSON_OUTPUT_FILE;

  const args = ["exec", "playwright", "test", "--reporter=list,html"];
  if (reportPath) {
    args[args.length - 1] = "--reporter=list,html,json";
    childEnv.PLAYWRIGHT_JSON_OUTPUT_FILE = reportPath;
  }

  return new Promise((resolve) => {
    let child;
    try {
      child = spawn("pnpm", args, { env: childEnv, stdio: suppressOutput ? "ignore" : ["ignore", "inherit", 1] });
    } catch {
      resolve(127);
      return;
    }

    let settled = false;
    child.once("error", (error) => {
      if (settled) return;
      settled = true;
      resolve(error.code === "ENOENT" ? 127 : 1);
    });
    child.once("close", (code, signal) => {
      if (settled) return;
      settled = true;
      resolve(Number.isInteger(code) ? code : signal ? signalExitCode(signal) : 1);
    });
  });
}

async function createReportDirectory(env) {
  const parent = reportDirectoryParent(env);
  if (!parent) return null;
  try {
    return await mkdtemp(path.join(parent, "e2e-admin-playwright-"));
  } catch {
    return null;
  }
}

export async function runE2EAdmin(env = process.env, {
  classifyReportFile = classifyPlaywrightReportFile,
  executePlaywright = runPlaywright,
  stdout = process.stdout,
  stderr = process.stderr,
  write = writeLine,
} = {}) {
  const reportDirectory = await createReportDirectory(env);
  const reportPath = reportDirectory ? path.join(reportDirectory, "results.json") : null;
  const stopToken = randomBytes(32).toString("hex");
  let outputGuardEnabled = false;
  let annotationsEnabled = false;
  let playwrightStatus = 1;

  try {
    await write(stdout, `::stop-commands::${stopToken}`);
    outputGuardEnabled = true;
  } catch {
    // If the guard cannot be written, the Playwright output is suppressed below.
  }

  try {
    playwrightStatus = await executePlaywright(reportPath, env, !outputGuardEnabled);
  } finally {
    if (outputGuardEnabled) {
      try {
        await write(stdout, `\n::${stopToken}::`);
        annotationsEnabled = true;
      } catch {
        annotationsEnabled = false;
      }
    }
  }

  if (playwrightStatus !== 0 && annotationsEnabled) {
    let category = "unavailable";
    if (reportPath) {
      try {
        const result = await classifyReportFile(reportPath);
        if (result === "setup" || result === "test-failure") category = result;
      } catch {
        // Reporter failures stay count-only and never replace the Playwright status.
      }
    }
    try {
      await write(stdout, `::error::e2e-admin failed (category: ${category}; details redacted)`);
    } catch {
      // The original Playwright exit status remains authoritative if reporting cannot write.
    }
  }

  if (reportDirectory) {
    try {
      await rm(reportDirectory, { force: true, recursive: true });
    } catch {
      try {
        stderr.write("e2e-admin temporary report cleanup failed (details redacted)\n");
      } catch {
        // Cleanup diagnostics must not replace the Playwright status.
      }
    }
  }

  return playwrightStatus;
}

if (process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href) {
  try {
    process.exitCode = await runE2EAdmin();
  } catch {
    process.exitCode = 1;
  }
}
