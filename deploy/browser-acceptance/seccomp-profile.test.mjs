import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import test from "node:test";

const dockerDefaultBytes = readFileSync(new URL("./seccomp-docker-default-v28.5.2.json", import.meta.url));
const dockerDefault = JSON.parse(dockerDefaultBytes.toString("utf8"));
const browserProfile = JSON.parse(readFileSync(new URL("./seccomp-browser.json", import.meta.url), "utf8"));

const CLONE_NAMESPACE_MASK = 0x7e020000;
const UNSHARE_NAMESPACE_MASK = 0x7e020080;
const CLONE_SHAPES = [0x10000000, 0x70000000, 0x20000000];
const EXTRA_NAMESPACE_FLAGS = [0x00020000, 0x02000000, 0x04000000, 0x08000000];
const DEFAULT_CONTEXT = { arch: "amd64", caps: [] };

function ruleApplies(rule, { arch, caps }) {
  const includes = rule.includes ?? {};
  const excludes = rule.excludes ?? {};
  if (includes.arches && !includes.arches.includes(arch)) return false;
  if (includes.caps && !includes.caps.some((cap) => caps.includes(cap))) return false;
  if (excludes.arches?.includes(arch)) return false;
  if (excludes.caps?.some((cap) => caps.includes(cap))) return false;
  return true;
}

function matchingRules(profile, syscall, args = [], context = DEFAULT_CONTEXT) {
  return profile.syscalls.filter((rule) => (
    rule.names?.includes(syscall) && ruleApplies(rule, context) &&
    (rule.args ?? []).every((condition) => {
      if (condition.op !== "SCMP_CMP_MASKED_EQ") return false;
      const argument = args[condition.index] ?? 0;
      return ((argument & condition.value) >>> 0) === (condition.valueTwo ?? 0);
    })
  ));
}

function syscallAllowed(profile, syscall, args = [], context = DEFAULT_CONTEXT) {
  return matchingRules(profile, syscall, args, context).some((rule) => rule.action === "SCMP_ACT_ALLOW") ||
    profile.defaultAction === "SCMP_ACT_ALLOW";
}

function cloneRule(valueTwo) {
  return {
    names: ["clone"],
    action: "SCMP_ACT_ALLOW",
    args: [{ index: 0, value: CLONE_NAMESPACE_MASK, valueTwo, op: "SCMP_CMP_MASKED_EQ" }],
    excludes: { caps: ["CAP_SYS_ADMIN"], arches: ["s390", "s390x"] },
  };
}

test("profile matches pinned Docker defaults plus exactly the reviewed Chromium syscall changes", () => {
  const digest = createHash("sha256").update(dockerDefaultBytes).digest("hex");
  assert.equal(digest, "01536f1d1df938ae611eba20d6349e0de7a99b6ecdee1549427a0b01b8301e28");

  const expected = structuredClone(dockerDefault);
  const chroot = expected.syscalls.find((rule) => rule.names?.length === 1 && rule.names[0] === "chroot");
  assert.deepEqual(chroot.includes, { caps: ["CAP_SYS_CHROOT"] });
  delete chroot.includes;

  const s390CloneIndex = expected.syscalls.findIndex((rule) => (
    rule.names?.includes("clone") && rule.includes?.arches?.includes("s390")
  ));
  assert.notEqual(s390CloneIndex, -1);
  expected.syscalls.splice(s390CloneIndex, 0, ...CLONE_SHAPES.map(cloneRule));
  expected.syscalls.push({
    names: ["unshare"],
    action: "SCMP_ACT_ALLOW",
    args: [{ index: 0, value: UNSHARE_NAMESPACE_MASK, valueTwo: 0x10000000, op: "SCMP_CMP_MASKED_EQ" }],
  });

  assert.deepEqual(browserProfile, expected);
});

test("semantic seccomp evaluation admits only the reviewed Chromium namespace shapes", () => {
  for (const flags of [0x10000011, 0x70000011, 0x20000011, 0x00000011]) {
    assert.equal(syscallAllowed(browserProfile, "clone", [flags]), true, `clone ${flags.toString(16)} should be allowed`);
  }
  for (const flags of [0x30000011, 0x40000011, 0x50000011, 0x60000011]) {
    assert.equal(syscallAllowed(browserProfile, "clone", [flags]), false, `clone ${flags.toString(16)} should be denied`);
  }
  for (const shape of CLONE_SHAPES) {
    for (const extraFlag of EXTRA_NAMESPACE_FLAGS) {
      const flags = shape | extraFlag | 0x11;
      assert.equal(syscallAllowed(browserProfile, "clone", [flags]), false,
        `clone ${flags.toString(16)} should be denied`);
    }
  }

  assert.equal(syscallAllowed(browserProfile, "unshare", [0x10000000]), true);
  for (const flags of [0x10020000, 0x10000080, 0x40000000, 0x20000000, 0x00020000, 0]) {
    assert.equal(syscallAllowed(browserProfile, "unshare", [flags]), false,
      `unshare ${flags.toString(16)} should be denied`);
  }

  assert.equal(syscallAllowed(browserProfile, "clone3", []), false);
  const clone3Errno = matchingRules(browserProfile, "clone3").find((rule) => rule.action === "SCMP_ACT_ERRNO");
  assert.deepEqual(clone3Errno, dockerDefault.syscalls.find((rule) => (
    rule.names?.includes("clone3") && rule.action === "SCMP_ACT_ERRNO"
  )));
  assert.equal(clone3Errno.errnoRet, 38, "clone3 must remain ENOSYS");
});
