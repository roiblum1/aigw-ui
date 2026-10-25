// The Windows installer, install.ps1, against the same stand-ins as
// install.sh's tests (standin.mjs). It runs under pwsh here, which is
// PowerShell 7 on Linux: Windows PowerShell 5.1 and Windows itself are not
// tried. Needs pwsh (GitHub's Ubuntu runners have it); skipped without.
//   node --test clients/install/

import { test } from "node:test";
import assert from "node:assert/strict";
import { execFile, execFileSync } from "node:child_process";
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { archive, extension, helperPs1, home, hub, key, path } from "./standin.mjs";

const installer = fileURLToPath(new URL("./install.ps1", import.meta.url));
const quiet = { POWERSHELL_TELEMETRY_OPTOUT: "1", POWERSHELL_UPDATECHECK: "Off" };

function findPwsh() {
	try {
		return execFileSync("sh", ["-c", "command -v pwsh"]).toString().trim() || null;
	} catch {
		return null;
	}
}
const pwsh = findPwsh();
const skip = pwsh ? false : "pwsh is not installed";

function exec(args, env) {
	return new Promise((resolve) => {
		const child = execFile(pwsh, ["-NoLogo", "-NoProfile", "-NonInteractive", ...args], { env: { AIGW_CHECK_SECONDS: "20", ...quiet, ...env } }, (err, stdout, stderr) =>
			resolve({ code: err ? err.code : 0, stdout, stderr }),
		);
		child.stdin.end();
	});
}

// As a file: powershell -File install.ps1 HUB [GATEWAY].
const run = (args, env) => exec(["-ExecutionPolicy", "Bypass", "-File", installer, ...args], env);

// As the tenant's page shows it, from the hub: iex "& { $(irm HUB/clients/install.ps1) } HUB".
const line = (hubUrl, rest, env) => exec(["-Command", `iex "& { $(irm ${hubUrl}/clients/install.ps1) } ${hubUrl} ${rest}"`], env);

const helperOf = (dir) => `powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File "${dir}/.local/share/aigw/aigw-token.ps1"`;

test("signs in, sets up Claude Code and Oh My Pi from the hub, waits for the gateway, then keeps them", { skip }, async () => {
	const h = home();
	const s = await hub(archive(h.dir), { refuse: 1 });
	const env = { HOME: h.dir, PATH: path(h.bin), AIGW_PERSONAL_KEY: key };
	mkdirSync(join(h.dir, ".claude"));
	writeFileSync(join(h.dir, "roles"), JSON.stringify({ default: "google/gemini:high", smol: "aigw/gone", slow: "aigw/big" }));
	// A model the person may call is theirs to keep; one they may not is
	// replaced. A date-like string stays a string.
	writeFileSync(
		join(h.dir, ".claude", "settings.json"),
		JSON.stringify({
			model: "opus",
			since: "2026-10-25T10:00:00Z",
			permissions: { allow: ["Bash(git status)"] },
			env: { FOO: "1", ANTHROPIC_MODEL: "claude-opus-4", ANTHROPIC_DEFAULT_HAIKU_MODEL: "big" },
		}),
	);
	try {
		let r = await run([s.url + "/"], env);
		assert.equal(r.code, 0, r.stderr);
		const ps1 = join(h.dir, ".local", "share", "aigw", "aigw-token.ps1");
		assert.equal(readFileSync(ps1, "utf8"), helperPs1.toString());
		assert.equal(
			readFileSync(join(h.dir, ".local", "bin", "aigw-token.cmd"), "utf8"),
			`@echo off\r\npowershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File "${ps1}" %*\r\n`,
		);
		assert.equal(readFileSync(join(h.dir, ".config", "aigw", "hub"), "utf8"), s.url + "\n");
		assert.equal(readFileSync(join(h.dir, ".config", "aigw", "personal-key"), "utf8"), key + "\n");
		// The client files' version it installed, for aigw-token.
		assert.match(readFileSync(join(h.dir, ".config", "aigw", "clients"), "utf8"), /^v1 \d{10}\n$/);

		// Everything else in the settings is kept, and the old file beside them.
		const settings = JSON.parse(readFileSync(join(h.dir, ".claude", "settings.json"), "utf8"));
		assert.deepEqual(settings, {
			model: "opus",
			since: "2026-10-25T10:00:00Z",
			permissions: { allow: ["Bash(git status)"] },
			env: {
				FOO: "1",
				ANTHROPIC_MODEL: "big",
				ANTHROPIC_DEFAULT_HAIKU_MODEL: "big",
				ANTHROPIC_BASE_URL: `${s.url}/anthropic`,
				ANTHROPIC_DEFAULT_OPUS_MODEL: "big",
				ANTHROPIC_DEFAULT_SONNET_MODEL: "big",
				ANTHROPIC_DEFAULT_FABLE_MODEL: "big",
				ANTHROPIC_SMALL_FAST_MODEL: "small",
				CLAUDE_CODE_MAX_CONTEXT_TOKENS: "65536",
				CLAUDE_CODE_MAX_OUTPUT_TOKENS: "16384",
			},
			apiKeyHelper: helperOf(h.dir),
			statusLine: { type: "command", command: `node "${h.dir}/.claude/aigw-usage/statusline.mjs"`, refreshInterval: 30 },
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

		// Again, the way the tenant's page says, with no key given and the
		// gateway's address as Claude Code has it: still signed in, the
		// address taken without its path, the marketplace updated.
		r = await line(s.url, `${s.url}/anthropic`, { ...env, AIGW_PERSONAL_KEY: "" });
		assert.equal(r.code, 0, r.stderr);
		assert.match(r.stderr, /already signed in/);
		assert.ok(s.seen.includes("GET /clients/install.ps1"));
		assert.equal(JSON.parse(readFileSync(join(h.dir, ".claude", "settings.json"), "utf8")).env.ANTHROPIC_BASE_URL, `${s.url}/anthropic`);
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

// Claude Code's settings need no node on Windows; the plugin does, for its
// hooks and status line.
test("without node: the settings without a status line, and no plugin; without Oh My Pi: skipped", { skip }, async () => {
	const h = home();
	const s = await hub(archive(h.dir));
	try {
		const r = await run([s.url], { HOME: h.dir, PATH: `${h.bin}:/usr/bin:/bin`, AIGW_PERSONAL_KEY: key });
		assert.equal(r.code, 0, r.stderr);
		const settings = JSON.parse(readFileSync(join(h.dir, ".claude", "settings.json"), "utf8"));
		assert.equal(settings.apiKeyHelper, helperOf(h.dir));
		assert.equal(settings.env.ANTHROPIC_MODEL, "big");
		assert.equal(settings.statusLine, undefined);
		assert.match(r.stderr, /node is not installed: the aigw-usage plugin/);
		assert.ok(!existsSync(join(h.dir, "claude.log")));
		assert.ok(!s.seen.includes("GET /clients/claude-code.tar.gz"));
	} finally {
		s.close();
	}
});

test("a hub without the person's models, and settings that are not JSON", { skip }, async () => {
	const h = home();
	const s = await hub(archive(h.dir), { models: false });
	try {
		let r = await run([s.url, `${s.url}/`], { HOME: h.dir, PATH: path(h.bin), AIGW_PERSONAL_KEY: key });
		assert.equal(r.code, 0, r.stderr);
		const settings = JSON.parse(readFileSync(join(h.dir, ".claude", "settings.json"), "utf8"));
		assert.deepEqual(settings.env, { ANTHROPIC_BASE_URL: `${s.url}/anthropic` });
		assert.match(r.stderr, /the hub gave no model for Claude Code/);

		writeFileSync(join(h.dir, ".claude", "settings.json"), "{ not json");
		r = await run([s.url], { HOME: h.dir, PATH: path(h.bin), AIGW_PERSONAL_KEY: key });
		assert.equal(r.code, 0, r.stderr);
		assert.match(r.stderr, /is not JSON; left as it is/);
		assert.equal(readFileSync(join(h.dir, ".claude", "settings.json"), "utf8"), "{ not json");
	} finally {
		s.close();
	}
});

test("a key the hub does not take stops it before the settings", { skip }, async () => {
	const h = home();
	const s = await hub(archive(h.dir));
	try {
		const r = await run([s.url], { HOME: h.dir, PATH: path(h.bin), AIGW_PERSONAL_KEY: "aigw-pk-wrong" });
		assert.notEqual(r.code, 0);
		assert.match(r.stderr, /not signed in/);
		assert.ok(!existsSync(join(h.dir, ".claude", "settings.json")));
		assert.ok(!existsSync(join(h.dir, "claude.log")));
		assert.ok(!existsSync(join(h.dir, ".omp")));
	} finally {
		s.close();
	}
});

// aigw-token runs the installer again by itself (AIGW_UPDATE=1): it asks for
// nothing, keeps the gateway address given by hand before, and skips the
// gateway's check; never signed in, it stops. Meanwhile aigw-token starts no
// update of its own, though the hub's tokens name newer client files.
test("an update asks for nothing and keeps the gateway given by hand", { skip }, async () => {
	const h = home();
	const s = await hub(archive(h.dir), { tokenVersion: "v2" });
	const env = { HOME: h.dir, PATH: path(h.bin), AIGW_PERSONAL_KEY: key };
	mkdirSync(join(h.dir, ".claude"));
	const base = () => JSON.parse(readFileSync(join(h.dir, ".claude", "settings.json"), "utf8")).env.ANTHROPIC_BASE_URL;
	try {
		let r = await run([s.url, "http://127.0.0.1:9/v1"], env);
		assert.equal(r.code, 0, r.stderr);
		assert.equal(base(), "http://127.0.0.1:9/anthropic");
		writeFileSync(join(h.dir, ".config", "aigw", "clients"), "v0 0\n");

		r = await run([s.url], { ...env, AIGW_PERSONAL_KEY: "", AIGW_UPDATE: "1" });
		assert.equal(r.code, 0, r.stderr);
		assert.equal(base(), "http://127.0.0.1:9/anthropic");
		assert.doesNotMatch(r.stderr, /gateway at/);
		assert.match(readFileSync(join(h.dir, ".config", "aigw", "clients"), "utf8"), /^v1 /);
		await new Promise((resolve) => setTimeout(resolve, 1500));
		assert.ok(!s.seen.some((x) => x.startsWith("GET /clients/install.")), s.seen.join(", "));

		// Run by hand without an address: the hub's again.
		r = await run([s.url], env);
		assert.equal(r.code, 0, r.stderr);
		assert.equal(base(), `${s.url}/anthropic`);
		assert.ok(!existsSync(join(h.dir, ".config", "aigw", "gateway")));

		// Signed out: the update stops without asking.
		rmSync(join(h.dir, ".config", "aigw", "hub"));
		r = await run([s.url], { ...env, AIGW_PERSONAL_KEY: "", AIGW_UPDATE: "1" });
		assert.notEqual(r.code, 0);
		assert.match(r.stderr, /the update stops/);
	} finally {
		s.close();
	}
});

// The whole round: the hub is upgraded to other client files; the next
// token aigw-token fetches names them, and it runs the hub's installer in
// the background, which puts the new files in place and keeps the new
// version. Nothing is asked and nothing is printed but the token.
test("a machine follows a hub upgrade by itself", { skip }, async () => {
	const h = home();
	const s = await hub(archive(h.dir));
	const env = { HOME: h.dir, PATH: path(h.bin), AIGW_PERSONAL_KEY: key };
	const kept = join(h.dir, ".omp", "agent", "extensions", "aigw-usage.ts");
	try {
		let r = await run([s.url], env);
		assert.equal(r.code, 0, r.stderr);
		assert.equal(readFileSync(kept, "utf8"), extension);

		s.state.version = s.state.tokenVersion = "v2";
		s.state.extension = "// the Oh My Pi extension, new\n";
		r = await exec(["-ExecutionPolicy", "Bypass", "-File", join(h.dir, ".local", "share", "aigw", "aigw-token.ps1")], { ...env, AIGW_TOKEN_RENEW_SECONDS: "0" });
		assert.equal(r.code, 0, r.stderr);
		assert.equal(r.stdout, "eyJ.t.sig\n");
		for (const end = Date.now() + 30000; Date.now() < end && !/^v2 /.test(readFileSync(join(h.dir, ".config", "aigw", "clients"), "utf8")); ) {
			await new Promise((resolve) => setTimeout(resolve, 200));
		}
		assert.match(readFileSync(join(h.dir, ".config", "aigw", "clients"), "utf8"), /^v2 /, readFileSync(join(h.dir, ".config", "aigw", "update.log"), "utf8"));
		assert.equal(readFileSync(kept, "utf8"), "// the Oh My Pi extension, new\n");
		assert.match(readFileSync(join(h.dir, ".config", "aigw", "update.log"), "utf8"), /done/);

		// A fix the same day: taken at once, as the last try finished a setup.
		s.state.version = s.state.tokenVersion = "v3";
		s.state.extension = "// the Oh My Pi extension, fixed\n";
		await new Promise((resolve) => setTimeout(resolve, 1100));
		r = await exec(["-ExecutionPolicy", "Bypass", "-File", join(h.dir, ".local", "share", "aigw", "aigw-token.ps1")], { ...env, AIGW_TOKEN_RENEW_SECONDS: "0" });
		assert.equal(r.stdout, "eyJ.t.sig\n");
		for (const end = Date.now() + 30000; Date.now() < end && !/^v3 /.test(readFileSync(join(h.dir, ".config", "aigw", "clients"), "utf8")); ) {
			await new Promise((resolve) => setTimeout(resolve, 200));
		}
		assert.equal(readFileSync(kept, "utf8"), "// the Oh My Pi extension, fixed\n");
	} finally {
		s.close();
	}
});
