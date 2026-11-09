import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, mkdir, readFile, writeFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { readDesktopProfileConfig, writeDesktopProfileConfig } from "./daemon-profile-config.ts";

// The explicit binary is built from this checkout; never resolve a real CLI.
const binary = process.env.MULTICA_PROFILE_TEST_BINARY ?? "/missing-fixture-cli";
async function fixture(t, raw) {
  const home = await mkdtemp(join(tmpdir(), "multica-desktop-config-"));
  t.after(() => rm(home, { recursive: true, force: true }));
  const path = join(home, ".multica/profiles/desktop-fixture/config.json");
  await mkdir(join(path, ".."), { recursive: true });
  if (raw !== null) await writeFile(path, raw);
  return { path, options: { cwd: home, env: { HOME: home, USERPROFILE: home } } };
}

test("Desktop preserves exact unknown JSON and never sends it back through JS", async (t) => {
  const { path, options } = await fixture(t, '{"future":900719925474099312345,"nested":{"n":null}}');
  const cfg = await readDesktopProfileConfig(path);
  cfg.token = "synthetic-token";
  await writeDesktopProfileConfig(binary, "desktop-fixture", path, cfg, options);
  assert.match(await readFile(path, "utf8"), /900719925474099312345/);
});

test("Desktop refuses a stale edit and retains newer bytes", async (t) => {
  const { path, options } = await fixture(t, '{}');
  const cfg = await readDesktopProfileConfig(path);
  const newer = '{"future":"newer"}';
  await writeFile(path, newer);
  cfg.server_url = "https://fixture.invalid";
  await assert.rejects(writeDesktopProfileConfig(binary, "desktop-fixture", path, cfg, options));
  assert.equal(await readFile(path, "utf8"), newer);
});

test("Desktop rejects malformed config instead of silently replacing it", async (t) => {
  for (const raw of ["{broken", "[]", "null", '{"token":42}']) {
    const { path } = await fixture(t, raw);
    await assert.rejects(readDesktopProfileConfig(path));
    assert.equal(await readFile(path, "utf8"), raw);
  }
});

test("CLI failure has no direct-write fallback and no token in the error", async (t) => {
  const { path, options } = await fixture(t, '{}');
  const cfg = await readDesktopProfileConfig(path);
  cfg.token = "synthetic-private-token";
  await assert.rejects(writeDesktopProfileConfig("/missing-cli", "desktop-fixture", path, cfg, options),
    (err) => !err.message.includes(cfg.token));
  assert.equal(await readFile(path, "utf8"), '{}');
});

test("two Desktop subprocess writers cannot silently overwrite each other", async (t) => {
  const { path, options } = await fixture(t, '{"future":null}');
  const a = await readDesktopProfileConfig(path);
  const b = await readDesktopProfileConfig(path);
  a.server_url = "https://a.invalid";
  b.server_url = "https://b.invalid";
  const results = await Promise.allSettled([a, b].map(config =>
    writeDesktopProfileConfig(binary, "desktop-fixture", path, config, options)));
  assert.equal(results.filter(r => r.status === "fulfilled").length, 1);
  const fresh = await readDesktopProfileConfig(path);
  fresh.token = "synthetic-new";
  await writeDesktopProfileConfig(binary, "desktop-fixture", path, fresh, options);
  const saved = JSON.parse(await readFile(path, "utf8"));
  assert.equal(saved.token, "synthetic-new");
  assert.equal(saved.future, null);
});

test("Desktop create, unset and replay protection", async (t) => {
  const { path, options } = await fixture(t, null);
  const cfg = await readDesktopProfileConfig(path);
  cfg.token = "synthetic-token";
  await writeDesktopProfileConfig(binary, "desktop-fixture", path, cfg, options);
  await assert.rejects(writeDesktopProfileConfig(binary, "desktop-fixture", path, cfg, options));
  const fresh = await readDesktopProfileConfig(path);
  delete fresh.token;
  await writeDesktopProfileConfig(binary, "desktop-fixture", path, fresh, options);
  assert.equal(JSON.parse(await readFile(path,"utf8")).token, undefined);
});
