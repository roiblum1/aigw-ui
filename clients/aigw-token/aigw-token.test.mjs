// The helper against a stand-in for the hub's POST /api/v1/token.
//   node --test clients/aigw-token/

import { test } from "node:test";
import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { createServer } from "node:http";
import { mkdtempSync, readFileSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const helper = fileURLToPath(new URL("./aigw-token", import.meta.url));
const key = "aigw-pk-test";

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

function run(args, env) {
  return new Promise((resolve) =>
    execFile(helper, args, { env: { ...process.env, ...env } }, (err, stdout, stderr) => resolve({ code: err ? err.code : 0, stdout, stderr })),
  );
}

test("signs in, renews, keeps, falls back, and stops on a revoked key", async () => {
  const h = await hub();
  const env = { AIGW_CONFIG_DIR: mkdtempSync(join(tmpdir(), "aigw-")) };
  try {
    let r = await run(["login", h.url + "/", key], env);
    assert.equal(r.code, 0, r.stderr);
    assert.equal(r.stdout, "", "login prints no token");
    assert.equal(statSync(join(env.AIGW_CONFIG_DIR, "personal-key")).mode & 0o777, 0o600);
    assert.deepEqual(h.seen[0], { method: "POST", url: "/api/v1/token", auth: `Bearer ${key}` });

    // The token kept, without asking the hub again.
    r = await run([], env);
    assert.equal(r.stdout, "eyJ.token-1.sig\n");
    assert.equal(r.stderr, "");
    assert.equal(h.seen.length, 1);

    // Old enough: a new one.
    r = await run([], { ...env, AIGW_TOKEN_RENEW_SECONDS: "0" });
    assert.equal(r.stdout, "eyJ.token-2.sig\n");
    assert.equal(h.seen.length, 2);

    // The hub fails: the token kept, and nothing but the token on stdout.
    h.setStatus(503);
    r = await run([], { ...env, AIGW_TOKEN_RENEW_SECONDS: "0" });
    assert.equal(r.code, 0);
    assert.equal(r.stdout, "eyJ.token-2.sig\n");

    // The key revoked: no token, and the one kept is forgotten.
    h.setStatus(401);
    r = await run([], { ...env, AIGW_TOKEN_RENEW_SECONDS: "0" });
    assert.notEqual(r.code, 0);
    assert.equal(r.stdout, "");
    assert.match(r.stderr, /does not take this personal key/);
    assert.throws(() => readFileSync(join(env.AIGW_CONFIG_DIR, "token")));
  } finally {
    h.close();
  }
});

test("the hub cannot be reached and no token is kept", async () => {
  const env = { AIGW_CONFIG_DIR: mkdtempSync(join(tmpdir(), "aigw-")) };
  const r = await run(["login", "http://127.0.0.1:9", key], env);
  assert.notEqual(r.code, 0);
  assert.match(r.stderr, /HTTP 000/);
});

test("reads the key from stdin, with or without a newline at the end", async () => {
  const h = await hub();
  try {
    for (const input of [key, key + "\n"]) {
      const env = { AIGW_CONFIG_DIR: mkdtempSync(join(tmpdir(), "aigw-")) };
      const r = await new Promise((resolve) => {
        const child = execFile(helper, ["login", h.url], { env: { ...process.env, ...env } }, (err, stdout, stderr) =>
          resolve({ code: err ? err.code : 0, stdout, stderr }),
        );
        child.stdin.end(input);
      });
      assert.equal(r.code, 0, r.stderr);
      assert.equal(readFileSync(join(env.AIGW_CONFIG_DIR, "personal-key"), "utf8"), key + "\n");
    }
  } finally {
    h.close();
  }
});

test("refuses what is not a personal key", async () => {
  const r = await run(["login", "http://127.0.0.1:9", "sk-abc"], { AIGW_CONFIG_DIR: mkdtempSync(join(tmpdir(), "aigw-")) });
  assert.notEqual(r.code, 0);
  assert.match(r.stderr, /starts with aigw-pk-/);
});
