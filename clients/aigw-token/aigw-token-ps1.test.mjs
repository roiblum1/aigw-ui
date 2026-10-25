// The PowerShell aigw-token, for Windows, against a stand-in for the hub's
// POST /api/v1/token: the same cases as the sh one, and the two keep the same
// files. Needs pwsh (GitHub's Ubuntu runners have it); skipped without.
//   node --test clients/aigw-token/

import { test } from "node:test";
import assert from "node:assert/strict";
import { execFile, execFileSync } from "node:child_process";
import { createServer } from "node:http";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const script = fileURLToPath(new URL("./aigw-token.ps1", import.meta.url));
const sh = fileURLToPath(new URL("./aigw-token", import.meta.url));
const key = "aigw-pk-test";
const quiet = { POWERSHELL_TELEMETRY_OPTOUT: "1", POWERSHELL_UPDATECHECK: "Off" };

function hasPwsh() {
  try {
    execFileSync("pwsh", ["-NoLogo", "-NoProfile", "-Command", "exit 0"], { env: { ...process.env, ...quiet }, stdio: "ignore" });
    return true;
  } catch {
    return false;
  }
}
const skip = hasPwsh() ? false : "pwsh is not installed";

function hub() {
  const seen = [];
  let status = 200;
  let n = 0;
  const server = createServer((req, res) => {
    seen.push({ method: req.method, url: req.url, auth: req.headers.authorization });
    if (status !== 200) {
      res.writeHead(status).end('{"error":"no"}');
      return;
    }
    n++;
    res.writeHead(200, { "content-type": "application/json" }).end(JSON.stringify({ token: `eyJ.token-${n}.sig`, expires_at: "x", expires_in: 604800 }));
  });
  return new Promise((resolve) =>
    server.listen(0, "127.0.0.1", () =>
      resolve({ url: `http://127.0.0.1:${server.address().port}`, seen, setStatus: (s) => (status = s), close: () => server.close() }),
    ),
  );
}

// As Claude Code and omp run it on Windows: powershell -File, here pwsh.
function run(args, env, input) {
  return new Promise((resolve) => {
    const child = execFile(
      "pwsh",
      ["-NoLogo", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script, ...args],
      { env: { ...process.env, ...quiet, ...env } },
      (err, stdout, stderr) => resolve({ code: err ? err.code : 0, stdout, stderr }),
    );
    child.stdin.end(input ?? "");
  });
}

test("signs in, renews, keeps, falls back, and stops on a revoked key", { skip }, async () => {
  const h = await hub();
  const env = { AIGW_CONFIG_DIR: mkdtempSync(join(tmpdir(), "aigw-")) };
  try {
    let r = await run(["login", h.url + "/", key], env);
    assert.equal(r.code, 0, r.stderr);
    assert.equal(r.stdout, "", "login prints no token");
    assert.match(r.stderr, /signed in to http:\/\/127\.0\.0\.1:\d+$/m);
    assert.deepEqual(h.seen[0], { method: "POST", url: "/api/v1/token", auth: `Bearer ${key}` });
    // The same files as the sh one, so the plugin and the extension find the key.
    assert.equal(readFileSync(join(env.AIGW_CONFIG_DIR, "hub"), "utf8"), h.url + "\n");
    assert.equal(readFileSync(join(env.AIGW_CONFIG_DIR, "personal-key"), "utf8"), key + "\n");

    // The token kept, without asking the hub again.
    r = await run([], env);
    assert.equal(r.stdout, "eyJ.token-1.sig\n");
    assert.equal(r.stderr, "");
    assert.equal(h.seen.length, 1);

    // Old enough: a new one.
    r = await run([], { ...env, AIGW_TOKEN_RENEW_SECONDS: "0" });
    assert.equal(r.stdout, "eyJ.token-2.sig\n");
    assert.equal(h.seen.length, 2);

    // The sh one takes the token the PowerShell one kept.
    const shOut = execFileSync(sh, [], { env: { ...process.env, ...env } }).toString();
    assert.equal(shOut, "eyJ.token-2.sig\n");
    assert.equal(h.seen.length, 2);

    // The hub fails: the token kept, and nothing but the token on stdout.
    h.setStatus(503);
    r = await run([], { ...env, AIGW_TOKEN_RENEW_SECONDS: "0" });
    assert.equal(r.code, 0, r.stderr);
    assert.equal(r.stdout, "eyJ.token-2.sig\n");

    // The key revoked: no token, and the one kept is forgotten.
    h.setStatus(401);
    r = await run([], { ...env, AIGW_TOKEN_RENEW_SECONDS: "0" });
    assert.notEqual(r.code, 0);
    assert.equal(r.stdout, "");
    assert.match(r.stderr, /does not take this personal key/);
    assert.throws(() => readFileSync(join(env.AIGW_CONFIG_DIR, "token")));

    // Logged out: nothing kept.
    r = await run(["logout"], env);
    assert.equal(r.code, 0, r.stderr);
    assert.throws(() => readFileSync(join(env.AIGW_CONFIG_DIR, "personal-key")));
    r = await run([], env);
    assert.notEqual(r.code, 0);
    assert.match(r.stderr, /not signed in/);
  } finally {
    h.close();
  }
});

test("takes the sign-in the sh one made", { skip }, async () => {
  const h = await hub();
  const env = { AIGW_CONFIG_DIR: mkdtempSync(join(tmpdir(), "aigw-")) };
  try {
    // Not execFileSync: the stand-in hub answers from this process.
    await new Promise((resolve, reject) =>
      execFile(sh, ["login", h.url, key], { env: { ...process.env, ...env } }, (err) => (err ? reject(err) : resolve())),
    );
    const r = await run([], env);
    assert.equal(r.code, 0, r.stderr);
    assert.equal(r.stdout, "eyJ.token-1.sig\n");
    assert.equal(h.seen.length, 1);
  } finally {
    h.close();
  }
});

test("the hub cannot be reached and no token is kept", { skip }, async () => {
  const env = { AIGW_CONFIG_DIR: mkdtempSync(join(tmpdir(), "aigw-")) };
  const r = await run(["login", "http://127.0.0.1:9", key], env);
  assert.notEqual(r.code, 0);
  assert.match(r.stderr, /HTTP 000/);
});

test("reads the key from stdin, with or without a newline at the end", { skip }, async () => {
  const h = await hub();
  try {
    for (const input of [key, key + "\n", key + "\r\n"]) {
      const env = { AIGW_CONFIG_DIR: mkdtempSync(join(tmpdir(), "aigw-")) };
      const r = await run(["login", h.url], env, input);
      assert.equal(r.code, 0, r.stderr);
      assert.equal(readFileSync(join(env.AIGW_CONFIG_DIR, "personal-key"), "utf8"), key + "\n");
    }
  } finally {
    h.close();
  }
});

test("refuses what is not a personal key", { skip }, async () => {
  const r = await run(["login", "http://127.0.0.1:9", "sk-abc"], { AIGW_CONFIG_DIR: mkdtempSync(join(tmpdir(), "aigw-")) });
  assert.notEqual(r.code, 0);
  assert.match(r.stderr, /starts with aigw-pk-/);
});

// A hub that names its client files' version with each token, and serves a
// stand-in install.ps1 that notes how it was run.
function followedHub(version) {
  const seen = [];
  const installer = 'param([string]$Hub)\n[IO.File]::WriteAllText((Join-Path $env:AIGW_CONFIG_DIR "ran"), "update=$env:AIGW_UPDATE hub=$Hub`n")\n';
  const server = createServer((req, res) => {
    seen.push(`${req.method} ${req.url}`);
    if (req.url === "/clients/install.ps1") return res.writeHead(200, { "content-type": "text/plain; charset=utf-8" }).end(installer);
    res.writeHead(200, { "content-type": "application/json" }).end(
      JSON.stringify({ token: "eyJ.t.sig", expires_in: 604800, ...(version ? { clients_version: version } : {}) }),
    );
  });
  return new Promise((resolve) =>
    server.listen(0, "127.0.0.1", () => resolve({ url: `http://127.0.0.1:${server.address().port}`, seen, close: () => server.close() })),
  );
}

const waitFor = async (file, ms = 15000) => {
  for (const end = Date.now() + ms; Date.now() < end; await new Promise((r) => setTimeout(r, 100))) {
    try {
      return readFileSync(file, "utf8");
    } catch {}
  }
  return null;
};

// As the sh one: the installer again, from the hub, when the hub serves
// other client files or a week after the last setup; once a day at most;
// not where the installer never ran, nor with AIGW_AUTO_UPDATE=0. On Windows
// it starts hidden and holds none of Claude Code's handles; pwsh on Linux
// shows only that it runs.
test("follows the hub: the installer again when the hub's client files change, or weekly", { skip }, async () => {
  const h = await followedHub("v2");
  const dir = mkdtempSync(join(tmpdir(), "aigw-"));
  const env = { AIGW_CONFIG_DIR: dir, AIGW_TOKEN_RENEW_SECONDS: "0" };
  const now = Math.floor(Date.now() / 1000);
  const installs = () => h.seen.filter((s) => s === "GET /clients/install.ps1").length;
  try {
    let r = await run(["login", h.url, key], env);
    assert.equal(r.code, 0, r.stderr);
    r = await run([], env);
    assert.equal(r.stdout, "eyJ.t.sig\n");
    assert.equal(installs(), 0);

    // Set up a minute ago: a try now is later than that setup.
    writeFileSync(join(dir, "clients"), `v1 ${now - 60}\n`);
    r = await run([], env);
    assert.equal(r.code, 0, r.stderr);
    assert.equal(r.stdout, "eyJ.t.sig\n");
    assert.equal(await waitFor(join(dir, "ran")), `update=1 hub=${h.url}\n`);
    assert.equal(installs(), 1);

    r = await run([], env);
    assert.equal(r.stdout, "eyJ.t.sig\n");
    assert.equal(installs(), 1);

    rmSync(join(dir, "update-tried"));
    writeFileSync(join(dir, "clients"), `v2 ${now - 60}\n`);
    await run([], env);
    assert.equal(installs(), 1);
    writeFileSync(join(dir, "clients"), `v2 ${now - 8 * 86400}\n`);
    rmSync(join(dir, "ran"));
    await run([], env);
    assert.ok(await waitFor(join(dir, "ran")));
    assert.equal(installs(), 2);

    rmSync(join(dir, "update-tried"));
    await run([], { ...env, AIGW_AUTO_UPDATE: "0" });
    assert.equal(installs(), 2);
  } finally {
    h.close();
  }
});
