import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

import { parse } from "yaml";

const compose = parse(readFileSync(new URL("./compose.yaml", import.meta.url), "utf8"));
const dockerfile = readFileSync(new URL("./Dockerfile", import.meta.url), "utf8");
const packageLock = JSON.parse(readFileSync(new URL("./package-lock.json", import.meta.url), "utf8"));
const runner = readFileSync(new URL("./run.sh", import.meta.url), "utf8");

test("fixed project contains only the isolated browser and CONNECT observer", () => {
  assert.equal(compose.name, "telegram-browser-acceptance");
  assert.deepEqual(Object.keys(compose.services).sort(), ["browser", "observer"]);
  assert.equal(compose.networks["browser-internal"].internal, true);
  assert.equal(compose.networks["observer-egress"].driver, "bridge");
  assert.equal(compose.services.browser.depends_on.observer.condition, "service_healthy");
  assert.deepEqual(compose.services.browser.networks, ["browser-internal"]);
  assert.deepEqual(Object.keys(compose.services.observer.networks).sort(), ["browser-internal", "observer-egress"]);
  assert.deepEqual(compose.services.browser.build.args, {
    PLAYWRIGHT_IMAGE: "${PLAYWRIGHT_IMAGE:-mcr.microsoft.com/playwright:v1.61.1-noble@sha256:824f1a789072e648c62541c2cfa4479c4061a290d5c27766d67dc1dcbc19b321}",
  });
  assert.equal(compose.services.browser.platform, "${PLAYWRIGHT_PLATFORM:-linux/arm64}");
});

test("services cannot publish ports, mount extra host data, or write Docker logs", () => {
  for (const service of Object.values(compose.services)) {
    assert.equal(service.logging.driver, "none");
    assert.equal(service.read_only, true);
    assert.deepEqual(service.cap_drop, ["ALL"]);
    assert.equal(service.privileged ?? false, false);
    for (const forbidden of ["cap_add", "dns", "extra_hosts", "ipc", "network_mode", "ports", "volumes"]) {
      assert.equal(Object.hasOwn(service, forbidden), false, `${forbidden} must not be configured`);
    }
    assert.ok(service.security_opt.includes("no-new-privileges:true"));
    assert.ok(service.security_opt.some((item) => item.startsWith("seccomp=")));
    assert.equal(service.mem_limit, service.memswap_limit);
    assert.ok(Number.parseFloat(service.cpus) > 0);
    assert.equal(service.ulimits.core.soft, 0);
    assert.equal(service.ulimits.core.hard, 0);
  }
  for (const variable of ["DEBUG", "PWDEBUG", "DEBUG_FILE", "PLAYWRIGHT_DISABLE_FORCED_CHROMIUM_PROXIED_LOOPBACK"]) {
    assert.equal(Object.hasOwn(compose.services.browser.environment, variable), false);
  }
  assert.deepEqual(compose.services.observer.tmpfs, [
    "/tmp:rw,noexec,nosuid,nodev,size=16m,mode=1777",
    "/home/pwuser:rw,noexec,nosuid,nodev,size=16m,mode=0700,uid=1000,gid=1000",
  ]);
  assert.ok(compose.services.browser.tmpfs.includes("/dev/shm:rw,noexec,nosuid,nodev,size=256m,mode=1777"));
  assert.equal(compose.services.browser.mem_limit, "1024m");
  assert.equal(compose.services.browser.memswap_limit, "1024m");
  assert.equal(compose.services.browser.cpus, "1.0");
  assert.equal(compose.services.observer.mem_limit, "128m");
  assert.equal(compose.services.observer.memswap_limit, "128m");
  assert.equal(compose.services.browser.environment.NODE_USE_ENV_PROXY, "1");
  assert.equal(compose.services.browser.environment.HTTP_PROXY, "http://browser-observer:3128");
  assert.equal(compose.services.browser.environment.HTTPS_PROXY, "http://browser-observer:3128");
  assert.equal(compose.services.browser.environment.NO_PROXY, "browser-observer,localhost,127.0.0.1");
});

test("image and npm dependencies remain locked to the reviewed versions", () => {
  const pins = [
    "cf0daee9b994042e011bc29f20cdff1a9f682a039b43fcd738f7d8a9d3bcd9d6",
    "824f1a789072e648c62541c2cfa4479c4061a290d5c27766d67dc1dcbc19b321",
  ];
  for (const digest of pins) assert.ok(runner.includes(digest));
  for (const name of ["@playwright/test", "playwright"]) {
    const entry = packageLock.packages[`node_modules/${name}`];
    assert.equal(entry.version, "1.61.1");
    assert.match(entry.integrity, /^sha512-[A-Za-z0-9+/]+=*$/u);
  }
  assert.equal(packageLock.packages["node_modules/yaml"].version, "2.8.1");
  assert.match(dockerfile, /npm ci --omit=dev --ignore-scripts --no-audit --no-fund/u);
});

test("wrapper fixes the project, closes stdin for readiness and removes only local images", () => {
  assert.ok(runner.includes("--env-file /dev/null"));
  assert.ok(runner.includes("--project-name telegram-browser-acceptance"));
  assert.ok(runner.includes("run --no-deps --rm -T"));
  assert.ok(runner.includes("exec </dev/null"));
  assert.ok(runner.includes("down --rmi local"));
  assert.ok(runner.includes("available_kib >= 2097152"));
  assert.ok(runner.includes("$APPROVED_QA_SCRIPT_SHA256"));
  assert.ok(runner.includes("--volume \"$MANIFEST_PATH:/run/release-record.json:ro\""));
  assert.ok(runner.includes("--volume \"$QA_SCRIPT_PATH:/run/approved-qa.mjs:ro\""));
});
