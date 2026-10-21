// The installer against a stand-in for the hub, which serves aigw-token, the
// plugin's archive, the Oh My Pi extension, POST /api/v1/token and
// GET /api/v1/my/models, and for the gateway's /v1/models; with stand-ins
// for claude and omp.
//   node --test clients/install/

import { test } from "node:test";
import assert from "node:assert/strict";
import { execFile, execFileSync } from "node:child_process";
import { createServer } from "node:http";
import { existsSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const installer = fileURLToPath(new URL("./install.sh", import.meta.url));
const helper = readFileSync(fileURLToPath(new URL("../aigw-token/aigw-token", import.meta.url)));
const extension = "// the Oh My Pi extension\n";
const key = "aigw-pk-test";
const token = "eyJ.t.sig";
// node, and the system's tools, without a claude or omp of this machine.
const path = (...first) => [...first, dirname(process.execPath), "/usr/bin", "/bin"].join(":");

// A marketplace archive as the hub makes it, small.
function archive(dir) {
	const src = join(dir, "archive");
	mkdirSync(join(src, ".claude-plugin"), { recursive: true });
	mkdirSync(join(src, "aigw-usage"), { recursive: true });
	writeFileSync(join(src, ".claude-plugin", "marketplace.json"), '{"name":"aigw"}\n');
	writeFileSync(join(src, "aigw-usage", "plugin.json"), "{}\n");
	return execFileSync("tar", ["-czf", "-", "-C", src, "."]);
}

// The hub, which is the gateway too. The gateway refuses the token `refuse`
// times first, as one that takes API keys does until a sync lists it. A hub
// without `models` is one from before 0.34.0.
function hub(tarball, { refuse = 0, models = true } = {}) {
	const seen = [];
	let url;
	const server = createServer((req, res) => {
		seen.push(`${req.method} ${req.url}`);
		const auth = req.headers.authorization;
		if (req.url === "/clients/aigw-token") return res.writeHead(200).end(helper);
		if (req.url === "/clients/claude-code.tar.gz") return res.writeHead(200).end(tarball);
		if (req.url === "/clients/aigw-usage.ts") return res.writeHead(200).end(extension);
		if (req.url === "/api/v1/token" && auth === `Bearer ${key}`)
			return res.writeHead(200).end(JSON.stringify({ token, expires_in: 604800 }));
		if (req.url === "/api/v1/my/models" && auth === `Bearer ${key}` && models)
			return res.writeHead(200).end(
				JSON.stringify({
					gateway_url: url,
					anthropic_base_url: `${url}/anthropic`,
					openai_base_url: `${url}/v1`,
					claude_code: { model: "big", small_model: "small" },
					models: [
						{ name: "big", context_window: 65536 },
						{ name: "small", context_window: 0 },
					],
				}),
			);
		if (req.url === "/v1/models" && auth === `Bearer ${token}`) {
			if (refuse-- > 0) return res.writeHead(401).end("{}");
			return res.writeHead(200).end('{"data":[]}');
		}
		res.writeHead(404).end("{}");
	});
	return new Promise((resolve) =>
		server.listen(0, "127.0.0.1", () => {
			url = `http://127.0.0.1:${server.address().port}`;
			resolve({ url, seen, close: () => server.close() });
		}),
	);
}

// A home with stand-ins for claude and omp. claude logs its arguments and
// lists the marketplace once it was added.
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
	// omp keeps its roles in a file, as omp config does, and logs each set.
	writeFileSync(
		join(bin, "omp"),
		`#!/bin/sh
case "$1 $2 $3" in
"config get modelRoles") printf '{"key":"modelRoles","value":%s}\\n' "$(cat "${dir}/roles" 2>/dev/null || echo '{}')" ;;
"config set modelRoles") printf '%s' "$4" > "${dir}/roles"; echo "$4" >> "${dir}/omp.log" ;;
esac
exit 0
`,
		{ mode: 0o755 },
	);
	return { dir, bin };
}

function run(args, env) {
	return new Promise((resolve) =>
		execFile("sh", [installer, ...args], { env: { AIGW_CHECK_SECONDS: "20", ...env } }, (err, stdout, stderr) =>
			resolve({ code: err ? err.code : 0, stdout, stderr }),
		),
	);
}

test("signs in, sets up Claude Code and Oh My Pi from the hub, waits for the gateway, then keeps them", async () => {
	const h = home();
	const s = await hub(archive(h.dir), { refuse: 1 });
	const env = { HOME: h.dir, PATH: path(h.bin), AIGW_PERSONAL_KEY: key };
	mkdirSync(join(h.dir, ".claude"));
	writeFileSync(join(h.dir, "roles"), JSON.stringify({ default: "google/gemini:high", smol: "aigw/gone", slow: "aigw/big" }));
	// A model the person may call is theirs to keep; one they may not is replaced.
	writeFileSync(
		join(h.dir, ".claude", "settings.json"),
		JSON.stringify({ model: "opus", env: { FOO: "1", ANTHROPIC_MODEL: "claude-opus-4", ANTHROPIC_DEFAULT_HAIKU_MODEL: "big" } }),
	);
	try {
		let r = await run([s.url + "/"], env);
		assert.equal(r.code, 0, r.stderr);
		const helperPath = join(h.dir, ".local", "bin", "aigw-token");
		assert.equal(readFileSync(helperPath, "utf8"), helper.toString());
		assert.equal(readFileSync(join(h.dir, ".config", "aigw", "hub"), "utf8"), s.url + "\n");
		assert.equal(readFileSync(join(h.dir, ".config", "aigw", "personal-key"), "utf8"), key + "\n");

		// Everything else in the settings is kept, and the old file beside them.
		const settings = JSON.parse(readFileSync(join(h.dir, ".claude", "settings.json"), "utf8"));
		assert.deepEqual(settings, {
			model: "opus",
			env: {
				FOO: "1",
				ANTHROPIC_BASE_URL: `${s.url}/anthropic`,
				ANTHROPIC_MODEL: "big",
				ANTHROPIC_DEFAULT_OPUS_MODEL: "big",
				ANTHROPIC_DEFAULT_SONNET_MODEL: "big",
				ANTHROPIC_DEFAULT_FABLE_MODEL: "big",
				ANTHROPIC_DEFAULT_HAIKU_MODEL: "big",
				ANTHROPIC_SMALL_FAST_MODEL: "small",
				CLAUDE_CODE_MAX_CONTEXT_TOKENS: "65536",
				CLAUDE_CODE_MAX_OUTPUT_TOKENS: "16384",
			},
			apiKeyHelper: helperPath,
			statusLine: { type: "command", command: "node ~/.claude/aigw-usage/statusline.mjs", refreshInterval: 30 },
		});
		assert.ok(existsSync(join(h.dir, ".claude", "settings.json.aigw-backup")));
		assert.match(r.stderr, /model big/);

		const market = join(h.dir, ".claude", "aigw-marketplace");
		assert.ok(existsSync(join(market, ".claude-plugin", "marketplace.json")));
		assert.ok(existsSync(join(market, "aigw-usage", "plugin.json")));
		assert.deepEqual(readFileSync(join(h.dir, "claude.log"), "utf8").trim().split("\n"), [
			"plugin marketplace list",
			`plugin marketplace add ${market}`,
			`plugin install aigw-usage@aigw --config hub_url=${s.url} --config warn_at=90`,
		]);
		assert.equal(readFileSync(join(h.dir, ".omp", "agent", "extensions", "aigw-usage.ts"), "utf8"), extension);
		// omp's roles: the person's own provider kept, a gateway model they
		// may no longer call replaced, an empty role set.
		assert.deepEqual(JSON.parse(readFileSync(join(h.dir, "roles"), "utf8")), {
			default: "google/gemini:high",
			smol: "aigw/small",
			slow: "aigw/big",
		});
		assert.match(r.stderr, /waiting for the gateway to take your token/);
		assert.match(r.stderr, /the gateway at .* takes your token/);

		// Again, with no key given and the gateway's address as Claude Code
		// has it: still signed in to the same hub, so no key is asked for,
		// the address is taken without its path, and the marketplace is
		// updated, not added twice.
		r = await run([s.url, `${s.url}/anthropic`], { ...env, AIGW_PERSONAL_KEY: "" });
		assert.equal(r.code, 0, r.stderr);
		assert.match(r.stderr, /already signed in/);
		assert.equal(JSON.parse(readFileSync(join(h.dir, ".claude", "settings.json"), "utf8")).env.ANTHROPIC_BASE_URL, `${s.url}/anthropic`);
		// Nothing to change in omp's roles the second time.
		assert.equal(readFileSync(join(h.dir, "omp.log"), "utf8").trim().split("\n").length, 1);
		assert.deepEqual(readFileSync(join(h.dir, "claude.log"), "utf8").trim().split("\n").slice(3), [
			"plugin marketplace list",
			"plugin marketplace update aigw",
			`plugin install aigw-usage@aigw --config hub_url=${s.url} --config warn_at=90`,
		]);
	} finally {
		s.close();
	}
});

test("without node: signs in and says what to put in Claude Code's settings; without Oh My Pi: skips it", async () => {
	const h = home();
	const s = await hub(archive(h.dir));
	mkdirSync(join(h.dir, ".claude"));
	try {
		const r = await run([s.url], { HOME: h.dir, PATH: "/usr/bin:/bin", AIGW_PERSONAL_KEY: key });
		assert.equal(r.code, 0, r.stderr);
		assert.match(r.stderr, /node is not installed/);
		assert.match(r.stderr, /"apiKeyHelper": ".*\/\.local\/bin\/aigw-token"/);
		assert.match(r.stderr, new RegExp(`"ANTHROPIC_BASE_URL": "${s.url}/anthropic"`));
		assert.match(r.stderr, /"ANTHROPIC_MODEL": "big"/);
		assert.match(r.stderr, /"ANTHROPIC_DEFAULT_HAIKU_MODEL": "small"/);
		assert.match(r.stderr, /claude is not on the PATH/);
		assert.match(r.stderr, /Oh My Pi is not installed/);
		assert.ok(!existsSync(join(h.dir, ".claude", "settings.json")));
		assert.ok(!s.seen.includes("GET /clients/claude-code.tar.gz"));
	} finally {
		s.close();
	}
});

// A hub from before 0.34.0 gives no models: the address given is used, and
// Claude Code is told to be given a model.
test("a hub without the person's models", async () => {
	const h = home();
	const s = await hub(archive(h.dir), { models: false });
	try {
		const r = await run([s.url, `${s.url}/`], { HOME: h.dir, PATH: path(h.bin), AIGW_PERSONAL_KEY: key });
		assert.equal(r.code, 0, r.stderr);
		const settings = JSON.parse(readFileSync(join(h.dir, ".claude", "settings.json"), "utf8"));
		assert.deepEqual(settings.env, { ANTHROPIC_BASE_URL: `${s.url}/anthropic` });
		assert.match(r.stderr, /the hub gave no model for Claude Code/);
	} finally {
		s.close();
	}
});

test("a key the hub does not take stops it before the settings", async () => {
	const h = home();
	const s = await hub(archive(h.dir));
	try {
		const r = await run([s.url], { HOME: h.dir, PATH: path(h.bin), AIGW_PERSONAL_KEY: "aigw-pk-wrong" });
		assert.notEqual(r.code, 0);
		assert.ok(!existsSync(join(h.dir, ".claude", "settings.json")));
		assert.ok(!existsSync(join(h.dir, "claude.log")));
		assert.ok(!existsSync(join(h.dir, ".omp")));
	} finally {
		s.close();
	}
});
