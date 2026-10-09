import { constants } from "node:fs";
import { open } from "node:fs/promises";

export const MAX_PLAYWRIGHT_REPORT_BYTES = 16 * 1024 * 1024;

const UNAVAILABLE = "unavailable";
const STAT_FIELDS = ["expected", "skipped", "unexpected", "flaky"];
const MAX_JSON_DEPTH = 256;

function isRecord(value) {
  return value !== null && typeof value === "object" && !Array.isArray(value);
}

function hasDuplicateJsonKeys(text) {
  let index = 0;
  const numberPattern = /-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?/y;

  function skipWhitespace() {
    while (text[index] === " " || text[index] === "\t" || text[index] === "\n" || text[index] === "\r") {
      index += 1;
    }
  }

  function parseString(decode = false) {
    const start = index;
    if (text[index] !== '"') throw new Error("Invalid JSON string");
    index += 1;

    while (index < text.length) {
      const character = text.charCodeAt(index);
      if (character === 34) {
        index += 1;
        return decode ? JSON.parse(text.slice(start, index)) : undefined;
      }
      if (character === 92) {
        index += 2;
        continue;
      }
      if (character < 32) throw new Error("Invalid JSON string");
      index += 1;
    }

    throw new Error("Unterminated JSON string");
  }

  function parseObject(depth) {
    if (depth > MAX_JSON_DEPTH) throw new Error("JSON nesting limit exceeded");
    index += 1;
    skipWhitespace();
    if (text[index] === "}") {
      index += 1;
      return false;
    }

    const keys = new Set();
    while (true) {
      skipWhitespace();
      const key = parseString(true);
      if (keys.has(key)) return true;
      keys.add(key);
      skipWhitespace();
      if (text[index] !== ":") throw new Error("Invalid JSON object");
      index += 1;
      if (parseValue(depth + 1)) return true;
      skipWhitespace();
      if (text[index] === "}") {
        index += 1;
        return false;
      }
      if (text[index] !== ",") throw new Error("Invalid JSON object");
      index += 1;
    }
  }

  function parseArray(depth) {
    if (depth > MAX_JSON_DEPTH) throw new Error("JSON nesting limit exceeded");
    index += 1;
    skipWhitespace();
    if (text[index] === "]") {
      index += 1;
      return false;
    }

    while (true) {
      if (parseValue(depth + 1)) return true;
      skipWhitespace();
      if (text[index] === "]") {
        index += 1;
        return false;
      }
      if (text[index] !== ",") throw new Error("Invalid JSON array");
      index += 1;
    }
  }

  function parseValue(depth) {
    if (depth > MAX_JSON_DEPTH) throw new Error("JSON nesting limit exceeded");
    skipWhitespace();

    if (text[index] === "{") return parseObject(depth);
    if (text[index] === "[") return parseArray(depth);
    if (text[index] === '"') {
      parseString();
      return false;
    }
    if (text.startsWith("true", index)) {
      index += 4;
      return false;
    }
    if (text.startsWith("false", index)) {
      index += 5;
      return false;
    }
    if (text.startsWith("null", index)) {
      index += 4;
      return false;
    }

    numberPattern.lastIndex = index;
    const number = numberPattern.exec(text);
    if (number) {
      index = numberPattern.lastIndex;
      return false;
    }
    throw new Error("Invalid JSON value");
  }

  try {
    skipWhitespace();
    if (parseValue(0)) return true;
    skipWhitespace();
    return index !== text.length;
  } catch {
    return true;
  }
}

export function classifyPlaywrightReport(reportText) {
  if (typeof reportText !== "string" || Buffer.byteLength(reportText, "utf8") > MAX_PLAYWRIGHT_REPORT_BYTES) {
    return UNAVAILABLE;
  }

  if (hasDuplicateJsonKeys(reportText)) return UNAVAILABLE;

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
