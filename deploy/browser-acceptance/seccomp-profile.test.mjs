import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import test from "node:test";

const dockerDefaultBytes = readFileSync(new URL("./seccomp-docker-default-v28.5.2.json", import.meta.url));
const dockerDefault = JSON.parse(dockerDefaultBytes.toString("utf8"));
const browserProfile = JSON.parse(readFileSync(new URL("./seccomp-browser.json", import.meta.url), "utf8"));

test("profile matches the pinned Docker default except Chromium's three reviewed syscall changes", () => {
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
  expected.syscalls.splice(s390CloneIndex, 0, {
    names: ["clone"],
    action: "SCMP_ACT_ALLOW",
    args: [{ index: 0, value: 1879048192, op: "SCMP_CMP_MASKED_EQ" }],
    excludes: { caps: ["CAP_SYS_ADMIN"], arches: ["s390", "s390x"] },
  });
  expected.syscalls.push({ names: ["unshare"], action: "SCMP_ACT_ALLOW" });

  assert.deepEqual(browserProfile, expected);
});

test("the added clone allowance requires all three Chromium namespace flags", () => {
  const rules = browserProfile.syscalls.filter((rule) => (
    rule.names?.length === 1 && rule.names[0] === "clone" &&
    rule.args?.[0]?.value === 1879048192
  ));
  assert.equal(rules.length, 1);
  assert.deepEqual(rules[0].args, [{ index: 0, value: 0x70000000, op: "SCMP_CMP_MASKED_EQ" }]);
  assert.ok(browserProfile.syscalls.some((rule) => (
    rule.names?.length === 1 && rule.names[0] === "unshare" && rule.action === "SCMP_ACT_ALLOW" && !rule.includes && !rule.excludes
  )));
  assert.ok(browserProfile.syscalls.some((rule) => (
    rule.names?.length === 1 && rule.names[0] === "chroot" && rule.action === "SCMP_ACT_ALLOW" && !rule.includes
  )));
  for (const syscall of ["setns", "mount", "keyctl", "bpf", "ptrace", "personality"]) {
    const originalRules = dockerDefault.syscalls.filter((rule) => rule.names?.includes(syscall));
    const changedRules = browserProfile.syscalls.filter((rule) => rule.names?.includes(syscall));
    assert.deepEqual(changedRules, originalRules, `${syscall} must have only Docker's pinned default rules`);
  }
});
