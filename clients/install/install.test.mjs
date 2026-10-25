// The installer against a stand-in for the hub, which serves aigw-token, the
// plugin's archive, the Oh My Pi extension, POST /api/v1/token and
// GET /api/v1/my/models, and for the gateway's /v1/models; with stand-ins
// for claude and omp.
//   node --test clients/install/

import { test } from "node:test";
import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { archive, extension, helper, home, hub, key, path } from "./standin.mjs";

const installer = fileURLToPath(new URL("./install.sh", import.meta.url));

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
		// The client files' version it installed, for aigw-token.
		assert.match(readFileSync(join(h.dir, ".config", "aigw", "clients"), "utf8"), /^v1 \d{10}\n$/);

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

// Git Bash, MSYS2 or Cygwin on Windows: Claude Code there runs its
// apiKeyHelper with cmd.exe, which cannot run the sh aigw-token, so the
// person is given the PowerShell line and nothing is installed.
test("a Unix shell on Windows is sent to the PowerShell setup", async () => {
	const h = home();
	const s = await hub(archive(h.dir));
	writeFileSync(join(h.bin, "uname"), '#!/bin/sh\necho "MINGW64_NT-10.0-22631"\n', { mode: 0o755 });
	try {
		const r = await run([s.url], { HOME: h.dir, PATH: path(h.bin), AIGW_PERSONAL_KEY: key });
		assert.notEqual(r.code, 0);
		assert.match(r.stderr, /a Unix shell on Windows/);
		assert.ok(r.stderr.includes(`iex "& { $(irm ${s.url}/clients/install.ps1) } ${s.url}"`), r.stderr);
		assert.deepEqual(s.seen, []);
		assert.ok(!existsSync(join(h.dir, ".local")));
	} finally {
		s.close();
	}
});

// In WSL, Windows' PATH is added to Linux's: claude and omp installed on
// Windows show up under /mnt/c. They are left out, never run, and the
// person is told how to set up Windows' own tools.
test("in WSL, Windows' own claude and omp are left out", async () => {
	const h = home();
	const s = await hub(archive(h.dir));
	const win = join(h.dir, "mnt", "c", "Users", "me", "AppData", "Roaming", "npm");
	mkdirSync(win, { recursive: true });
	for (const name of ["claude", "omp"]) {
		writeFileSync(join(win, name), `#!/bin/sh\ntouch "${h.dir}/windows-ran"\nexit 1\n`, { mode: 0o755 });
	}
	try {
		const env = { HOME: h.dir, PATH: path(win), AIGW_PERSONAL_KEY: key, WSL_DISTRO_NAME: "Ubuntu", AIGW_WINDOWS_DRIVES: join(h.dir, "mnt") };
		let r = await run([s.url], env);
		assert.equal(r.code, 0, r.stderr);
		assert.ok(!existsSync(join(h.dir, "windows-ran")));
		assert.match(r.stderr, /Claude Code is not installed/);
		assert.match(r.stderr, /Oh My Pi is not installed/);
		assert.match(r.stderr, /left out Windows' own claude omp on the PATH/);
		assert.ok(r.stderr.includes(`iex "& { $(irm ${s.url}/clients/install.ps1) } ${s.url}"`), r.stderr);

		// Claude Code installed in WSL too, ahead on the PATH: that one is set up.
		mkdirSync(join(h.dir, ".claude"));
		r = await run([s.url], { ...env, PATH: path(h.bin, win) });
		assert.equal(r.code, 0, r.stderr);
		assert.ok(!existsSync(join(h.dir, "windows-ran")));
		assert.match(readFileSync(join(h.dir, "claude.log"), "utf8"), /plugin install aigw-usage@aigw/);
		assert.equal(JSON.parse(readFileSync(join(h.dir, ".claude", "settings.json"), "utf8")).env.ANTHROPIC_MODEL, "big");
	} finally {
		s.close();
	}
});

// aigw-token runs the installer again by itself (AIGW_UPDATE=1): it asks for
// nothing, keeps the gateway address given by hand before, and skips the
// gateway's check; never signed in, it stops. Meanwhile aigw-token starts no
// update of its own, though the hub's tokens name newer client files.
test("an update asks for nothing and keeps the gateway given by hand", async () => {
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
test("a machine follows a hub upgrade by itself", async () => {
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
		r = await new Promise((resolve) =>
			execFile(join(h.dir, ".local", "bin", "aigw-token"), [], { env: { ...env, AIGW_TOKEN_RENEW_SECONDS: "0" } }, (err, stdout, stderr) =>
				resolve({ code: err ? err.code : 0, stdout, stderr }),
			),
		);
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
		r = await new Promise((resolve) =>
			execFile(join(h.dir, ".local", "bin", "aigw-token"), [], { env: { ...env, AIGW_TOKEN_RENEW_SECONDS: "0" } }, (err, stdout, stderr) =>
				resolve({ code: err ? err.code : 0, stdout, stderr }),
			),
		);
		assert.equal(r.stdout, "eyJ.t.sig\n");
		for (const end = Date.now() + 30000; Date.now() < end && !/^v3 /.test(readFileSync(join(h.dir, ".config", "aigw", "clients"), "utf8")); ) {
			await new Promise((resolve) => setTimeout(resolve, 200));
		}
		assert.equal(readFileSync(kept, "utf8"), "// the Oh My Pi extension, fixed\n");
	} finally {
		s.close();
	}
});
