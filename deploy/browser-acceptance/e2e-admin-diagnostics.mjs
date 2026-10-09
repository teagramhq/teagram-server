import { constants } from "node:fs";
import { open } from "node:fs/promises";

export const MAX_PLAYWRIGHT_REPORT_BYTES = 16 * 1024 * 1024;

const UNAVAILABLE = "unavailable";
const STAT_FIELDS = ["expected", "skipped", "unexpected", "flaky"];

function isRecord(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

export function classifyPlaywrightReport(reportText) {
  if (typeof reportText !== "string" || Buffer.byteLength(reportText, "utf8") > MAX_PLAYWRIGHT_REPORT_BYTES) {
    return UNAVAILABLE;
  }

  let report;
  try {
    report = JSON.parse(reportText);
  } catch {
    return UNAVAILABLE;
  }

  if (!isRecord(report) || !Array.isArray(report.errors) || !isRecord(report.stats)) {
    return UNAVAILABLE;
  }

  for (const field of STAT_FIELDS) {
    if (!Number.isSafeInteger(report.stats[field]) || report.stats[field] < 0) {
      return UNAVAILABLE;
    }
  }

  const noTestsRan = report.stats.expected === 0
    && report.stats.unexpected === 0
    && report.stats.flaky === 0;

  if (report.errors.length > 0 && noTestsRan) return "setup";
  if (report.errors.length === 0 && report.stats.unexpected > 0) return "test-failure";
  return UNAVAILABLE;
}

async function readBoundedRegularFile(filePath) {
  let file;
  let reportText = null;
  try {
    file = await open(filePath, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
    const before = await file.stat();
    if (!before.isFile() || before.size > MAX_PLAYWRIGHT_REPORT_BYTES) {
      reportText = null;
    } else {
      const chunks = [];
      let bytesReadTotal = 0;
      while (bytesReadTotal <= MAX_PLAYWRIGHT_REPORT_BYTES) {
        const remaining = MAX_PLAYWRIGHT_REPORT_BYTES + 1 - bytesReadTotal;
        const chunk = Buffer.allocUnsafe(Math.min(64 * 1024, remaining));
        const result = await file.read(chunk, 0, chunk.length, bytesReadTotal);
        if (result.bytesRead === 0) break;
        bytesReadTotal += result.bytesRead;
        if (bytesReadTotal > MAX_PLAYWRIGHT_REPORT_BYTES) break;
        chunks.push(chunk.subarray(0, result.bytesRead));
      }

      const after = await file.stat();
      if (bytesReadTotal <= MAX_PLAYWRIGHT_REPORT_BYTES
          && after.isFile()
          && after.size === before.size
          && after.size === bytesReadTotal) {
        try {
          reportText = new TextDecoder("utf-8", { fatal: true }).decode(Buffer.concat(chunks, bytesReadTotal));
        } catch {
          reportText = null;
        }
      }
    }
  } catch {
    reportText = null;
  }

  if (file) {
    try {
      await file.close();
    } catch {
      return null;
    }
  }
  return reportText;
}

export async function classifyPlaywrightReportFile(filePath) {
  const reportText = await readBoundedRegularFile(filePath);
  return reportText === null ? UNAVAILABLE : classifyPlaywrightReport(reportText);
}
