// Stand-ins for the installers' tests (install.test.mjs, install-ps1.test.mjs):
// the hub, which serves the client files, POST /api/v1/token and GET
// /api/v1/my/models and is the gateway's /v1/models too; and a home with
// stand-ins for claude and omp.

import { execFileSync } from "node:child_process";
import { createServer } from "node:http";
import { mkdirSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = (file) => fileURLToPath(new URL(file, import.meta.url));
export const helper = readFileSync(here("../aigw-token/aigw-token"));
export const helperPs1 = readFileSync(here("../aigw-token/aigw-token.ps1"));
export const installPs1 = readFileSync(here("./install.ps1"));
export const installSh = readFileSync(here("./install.sh"));
export const extension = "// the Oh My Pi extension\n";
export const key = "aigw-pk-test";
export const token = "eyJ.t.sig";
// node, and the system's tools, without a claude or omp of this machine.
export const path = (...first) => [...first, dirname(process.execPath), "/usr/bin", "/bin"].join(":");

// A marketplace archive as the hub makes it, small.
export function archive(dir) {
	const src = join(dir, "archive");
	mkdirSync(join(src, ".claude-plugin"), { recursive: true });
	mkdirSync(join(src, "aigw-usage"), { recursive: true });
	writeFileSync(join(src, ".claude-plugin", "marketplace.json"), '{"name":"aigw"}\n');
	writeFileSync(join(src, "aigw-usage", "plugin.json"), "{}\n");
	return execFileSync("tar", ["-czf", "-", "-C", src, "."]);
}

// The hub, which is the gateway too. The gateway refuses the token `refuse`
// times first, as one that takes API keys does until a sync lists it. A hub
// without `models` is one from before 0.34.0. Its client files are v1, but
// its tokens can name `tokenVersion`, as a hub upgraded meanwhile would.
// `state` changes what it serves: `version` (both answers) and `extension`
// stand for a hub upgrade.
export function hub(tarball, { refuse = 0, models = true, tokenVersion = "v1" } = {}) {
	const seen = [];
	let url;
	const state = { version: "v1", tokenVersion, extension };
	const files = {
		"/clients/aigw-token": helper,
		"/clients/aigw-token.ps1": helperPs1,
		"/clients/install.sh": installSh,
		"/clients/install.ps1": installPs1,
		"/clients/claude-code.tar.gz": tarball,
	};
	const server = createServer((req, res) => {
		seen.push(`${req.method} ${req.url}`);
		const auth = req.headers.authorization;
		if (files[req.url]) return res.writeHead(200).end(files[req.url]);
		if (req.url === "/clients/aigw-usage.ts") return res.writeHead(200).end(state.extension);
		if (req.url === "/api/v1/token" && auth === `Bearer ${key}`)
			return res.writeHead(200, { "content-type": "application/json" }).end(JSON.stringify({ token, expires_in: 604800, clients_version: state.tokenVersion }));
		if (req.url === "/api/v1/my/models" && auth === `Bearer ${key}` && models)
			return res.writeHead(200, { "content-type": "application/json" }).end(
				JSON.stringify({
					gateway_url: url,
					anthropic_base_url: `${url}/anthropic`,
					openai_base_url: `${url}/v1`,
					claude_code: { model: "big", small_model: "small" },
					models: [
						{ name: "big", context_window: 65536 },
						{ name: "small", context_window: 0 },
					],
					clients_version: state.version,
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
			resolve({ url, seen, state, close: () => server.close() });
		}),
	);
}

// A home with stand-ins for claude and omp. claude logs its arguments and
// lists the marketplace once it was added.
export function home() {
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
