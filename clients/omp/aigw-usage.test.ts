// The extension's models, against a stand-in for the hub.
//   node --test clients/omp/

import { test } from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import aigw, { definesAigw, ompModels } from "./aigw-usage.ts";

const clients = {
	openai_base_url: "https://llm.example.com/v1",
	models: [
		{ name: "GLM5.3", context_window: 32768, prices: { input: 0.6, cached: 0.11, output: 2.2 } },
		{ name: "qwen-small", context_window: 0, prices: null },
	],
};

// A long model's answer is held to omp's default; a short one's to a
// quarter of its length; an unknown length and missing prices are left to
// omp.
test("omp's models for the hub's", () => {
	assert.deepEqual(ompModels([...clients.models, { name: "long", context_window: 200000, prices: null }]), [
		{ id: "GLM5.3", name: "GLM5.3", contextWindow: 32768, maxTokens: 8192, cost: { input: 0.6, output: 2.2, cacheRead: 0.11, cacheWrite: 0 } },
		{ id: "qwen-small", name: "qwen-small" },
		{ id: "long", name: "long", contextWindow: 200000, maxTokens: 16384 },
	]);
});

test("a provider aigw of the person's own in models.yml", () => {
	assert.ok(definesAigw("providers:\n  aigw:\n    baseUrl: x\n"));
	assert.ok(definesAigw("providers:\n    aigw:   # mine\n"));
	assert.ok(!definesAigw("providers:\n  aigw-anthropic:\n"));
	assert.ok(!definesAigw(""));
});

// Loaded, it asks the hub with the personal key aigw-token keeps and adds
// the models under "aigw", with aigw-token for tokens; with the hub down it
// adds the list it kept; with the person's own provider aigw it adds none.
test("registers the tenant's models from the hub, then the kept ones", async () => {
	const home = mkdtempSync(join(tmpdir(), "aigw-omp-"));
	const config = join(home, ".config", "aigw");
	mkdirSync(config, { recursive: true });
	mkdirSync(join(home, ".local", "bin"), { recursive: true });
	writeFileSync(join(home, ".local", "bin", "aigw-token"), "#!/bin/sh\n", { mode: 0o755 });
	writeFileSync(join(config, "personal-key"), "aigw-pk-test\n");
	const seen: string[] = [];
	const server = createServer((req, res) => {
		seen.push(`${req.url} ${req.headers.authorization}`);
		res.writeHead(200, { "Content-Type": "application/json" }).end(JSON.stringify(clients));
	});
	await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
	const { port } = server.address() as { port: number };
	writeFileSync(join(config, "hub"), `http://127.0.0.1:${port}\n`);
	Object.assign(process.env, { HOME: home, AIGW_CONFIG_DIR: config, AIGW_HUB_URL: "", AIGW_API_KEY: "" });

	const load = async () => {
		const providers: Record<string, any> = {};
		const notes: string[] = [];
		const pi = {
			registerProvider: (name: string, config: unknown) => (providers[name] = config),
			setLabel: () => {},
			registerCommand: () => {},
			on: (event: string, handler: (e: unknown, ctx: unknown) => Promise<void>) => {
				if (event === "session_start") {
					handler({}, { hasUI: true, ui: { notify: (m: string) => notes.push(m), setStatus: () => {} } });
				}
			},
		};
		await aigw(pi as any);
		return { providers, notes };
	};

	const want = {
		baseUrl: "https://llm.example.com/v1",
		api: "openai-completions",
		apiKey: `!${join(home, ".local", "bin", "aigw-token")}`,
		models: ompModels(clients.models),
	};
	let got = await load();
	assert.deepEqual(got.providers, { aigw: want });
	assert.deepEqual(seen.slice(0, 1), ["/api/v1/my/models Bearer aigw-pk-test"]);

	await new Promise(resolve => server.close(resolve));
	got = await load();
	assert.deepEqual(got.providers, { aigw: want });
	assert.match(got.notes[0], /cannot be reached: the models are the last ones it gave/);

	mkdirSync(join(home, ".omp", "agent"), { recursive: true });
	writeFileSync(join(home, ".omp", "agent", "models.yml"), "providers:\n  aigw:\n    baseUrl: https://mine/v1\n");
	got = await load();
	assert.deepEqual(got.providers, {});
	assert.match(got.notes[0], /models.yml defines the provider aigw/);
});
