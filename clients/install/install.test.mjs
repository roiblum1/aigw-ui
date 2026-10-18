// The installer against a stand-in for the hub, which serves aigw-token, the
// plugin's archive and POST /api/v1/token, with a stand-in for claude.
//   node --test clients/install/

import { test } from "node:test";
import assert from "node:assert/strict";
import { execFile, execFileSync } from "node:child_process";
import { createServer } from "node:http";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const installer = fileURLToPath(new URL("./install.sh", import.meta.url));
const helper = readFileSync(fileURLToPath(new URL("../aigw-token/aigw-token", import.meta.url)));
const key = "aigw-pk-test";

// A marketplace archive as the hub makes it, small.
function archive(dir) {
  const src = join(dir, "archive");
  mkdirSync(join(src, ".claude-plugin"), { recursive: true });
  mkdirSync(join(src, "aigw-usage"), { recursive: true });
  writeFileSync(join(src, ".claude-plugin", "marketplace.json"), '{"name":"aigw"}\n');
  writeFileSync(join(src, "aigw-usage", "plugin.json"), "{}\n");
  return execFileSync("tar", ["-czf", "-", "-C", src, "."]);
}

function hub(tarball) {
  const seen = [];
  const server = createServer((req, res) => {
    seen.push(`${req.method} ${req.url}`);
    if (req.url === "/clients/aigw-token") return res.writeHead(200).end(helper);
    if (req.url === "/clients/claude-code.tar.gz") return res.writeHead(200).end(tarball);
    if (req.url === "/api/v1/token" && req.headers.authorization === `Bearer ${key}`)
      return res.writeHead(200).end(JSON.stringify({ token: "eyJ.t.sig", expires_in: 604800 }));
    res.writeHead(404).end("{}");
  });
  return new Promise((resolve) =>
    server.listen(0, "127.0.0.1", () => resolve({ url: `http://127.0.0.1:${server.address().port}`, seen, close: () => server.close() })),
  );
}

// A home with a stand-in claude, which logs its arguments and lists the
// marketplace once it was added.
function home() {
  const dir = mkdtempSync(join(tmpdir(), "aigw-install-"));
  const bin = join(dir, "fake-bin");
  mkdirSync(bin);
  writeFileSync(
    join(bin, "claude"),
    `#!/bin/sh
echo "$*" >> "${dir}/claude.log"
case "$*" in
"plugin marketplace list") [ -f "${dir}/added" ] && echo "aigw (directory)" ;;
"plugin marketplace add "*) touch "${dir}/added" ;;
esac
exit 0
`,
    { mode: 0o755 },
  );
  return { dir, bin };
}

function run(args, env) {
  return new Promise((resolve) =>
    execFile("sh", [installer, ...args], { env }, (err, stdout, stderr) => resolve({ code: err ? err.code : 0, stdout, stderr })),
  );
}

test("installs aigw-token, signs in, writes the settings and installs the plugin, then keeps them", async () => {
  const h = home();
  const s = await hub(archive(h.dir));
  const env = { HOME: h.dir, PATH: `${h.bin}:${process.env.PATH}`, AIGW_PERSONAL_KEY: key };
  mkdirSync(join(h.dir, ".claude"));
  writeFileSync(join(h.dir, ".claude", "settings.json"), JSON.stringify({ model: "opus", env: { FOO: "1" } }));
  try {
    let r = await run([s.url + "/", "https://llm.example.com/"], env);
    assert.equal(r.code, 0, r.stderr);
    const helperPath = join(h.dir, ".local", "bin", "aigw-token");
    assert.equal(readFileSync(helperPath, "utf8"), helper.toString());
    assert.equal(readFileSync(join(h.dir, ".config", "aigw", "hub"), "utf8"), s.url + "\n");
    assert.equal(readFileSync(join(h.dir, ".config", "aigw", "personal-key"), "utf8"), key + "\n");

    // Everything else in the settings is kept, and the old file beside them.
    const settings = JSON.parse(readFileSync(join(h.dir, ".claude", "settings.json"), "utf8"));
    assert.deepEqual(settings, {
      model: "opus",
      env: { FOO: "1", ANTHROPIC_BASE_URL: "https://llm.example.com" },
      apiKeyHelper: helperPath,
      statusLine: { type: "command", command: "node ~/.claude/aigw-usage/statusline.mjs", refreshInterval: 30 },
    });
    assert.ok(existsSync(join(h.dir, ".claude", "settings.json.aigw-backup")));

    const market = join(h.dir, ".claude", "aigw-marketplace");
    assert.ok(existsSync(join(market, ".claude-plugin", "marketplace.json")));
    assert.ok(existsSync(join(market, "aigw-usage", "plugin.json")));
    assert.deepEqual(readFileSync(join(h.dir, "claude.log"), "utf8").trim().split("\n"), [
      "plugin marketplace list",
      `plugin marketplace add ${market}`,
      `plugin install aigw-usage@aigw --config hub_url=${s.url} --config warn_at=90`,
    ]);

    // Again, with no key given: still signed in to the same hub, so no key
    // is asked for, and the marketplace is updated, not added twice.
    r = await run([s.url], { ...env, AIGW_PERSONAL_KEY: "" });
    assert.equal(r.code, 0, r.stderr);
    assert.match(r.stderr, /already signed in/);
    assert.deepEqual(readFileSync(join(h.dir, "claude.log"), "utf8").trim().split("\n").slice(3), [
      "plugin marketplace list",
      "plugin marketplace update aigw",
      `plugin install aigw-usage@aigw --config hub_url=${s.url} --config warn_at=90`,
    ]);
  } finally {
    s.close();
  }
});

test("without node and claude: signs in and says what to do by hand", async () => {
  const h = home();
  const s = await hub(archive(h.dir));
  try {
    const r = await run([s.url], { HOME: h.dir, PATH: "/usr/bin:/bin", AIGW_PERSONAL_KEY: key });
    assert.equal(r.code, 0, r.stderr);
    assert.match(r.stderr, /node is not installed/);
    assert.match(r.stderr, /"apiKeyHelper": ".*\/\.local\/bin\/aigw-token"/);
    assert.match(r.stderr, /<the gateway's address>/);
    assert.match(r.stderr, /claude is not installed/);
    assert.ok(!existsSync(join(h.dir, ".claude", "settings.json")));
    assert.ok(!s.seen.includes("GET /clients/claude-code.tar.gz"));
  } finally {
    s.close();
  }
});

test("a key the hub does not take stops it before the settings", async () => {
  const h = home();
  const s = await hub(archive(h.dir));
  try {
    const r = await run([s.url], { HOME: h.dir, PATH: `${h.bin}:${process.env.PATH}`, AIGW_PERSONAL_KEY: "aigw-pk-wrong" });
    assert.notEqual(r.code, 0);
    assert.ok(!existsSync(join(h.dir, ".claude", "settings.json")));
    assert.ok(!existsSync(join(h.dir, "claude.log")));
  } finally {
    s.close();
  }
});
