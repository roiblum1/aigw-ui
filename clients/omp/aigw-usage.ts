// Oh My Pi extension for the AI gateway. It adds the models you may call to
// omp, as the provider "aigw" (`omp --model aigw/<model>`), with their
// lengths and prices from the hub and tokens from aigw-token; it shows your
// budget in the status area and tells you once when it is nearly spent,
// spent, or served as best-effort. /aigw-usage lists every budget.
//
// The hub's installer puts it in ~/.omp/agent/extensions/ (see
// clients/README.md). With aigw-token signed in there is nothing to set:
// the hub and the personal key it keeps are used. Otherwise set
//   AIGW_HUB_URL   the address of the AI Gateway Control hub
//   AIGW_API_KEY   your key for the gateway
// AIGW_WARN_AT is the percent to warn at (90). A provider "aigw" of your
// own in ~/.omp/agent/models.yml is left as it is, and no model is added.
//
// It asks the hub only: for the models when omp starts (the last list is
// kept for when the hub cannot be reached), for the budgets at most every
// 30 seconds and after each turn. No prompt and no answer is sent anywhere.

import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";
import type { ExtensionAPI, ExtensionContext } from "@oh-my-pi/pi-coding-agent";

interface Budget {
	model_name: string;
	/** The tenant's budget across models, which holds on every model. */
	across?: boolean;
	unit: "tokens" | "credits";
	limit: number;
	window: string;
	used: number;
	resets_at: string;
	enforced: boolean;
	best_effort_until?: string;
	best_effort_capped?: boolean;
}

interface TenantBudget {
	limit: number;
	window: string;
	used: number;
	resets_at: string;
	enforced: boolean;
}

const ALL_MODELS = "All models";

/** The budgets of an answer of /my/usage: one per model, and the one across models. */
function budgetsOf(body: { budgets?: Budget[]; budget?: TenantBudget | null }): Budget[] {
	const out = [...(body.budgets ?? [])];
	const all = body.budget;
	if (all) out.push({ model_name: ALL_MODELS, across: true, unit: "credits", limit: all.limit, window: all.window, used: all.used, resets_at: all.resets_at, enforced: all.enforced });
	return out;
}

/** Where aigw-token keeps its sign-in, and this extension the last list of models. */
const configDir = () => process.env.AIGW_CONFIG_DIR || join(process.env.XDG_CONFIG_HOME || join(homedir(), ".config"), "aigw");

/** The hub and personal key that `aigw-token login` kept, if any. */
function helperLogin(): { hub: string; key: string } {
	const dir = configDir();
	try {
		return { hub: readFileSync(join(dir, "hub"), "utf8").trim(), key: readFileSync(join(dir, "personal-key"), "utf8").trim() };
	} catch {
		return { hub: "", key: "" };
	}
}

/** A model of GET /api/v1/my/models. */
interface ClientModel {
	name: string;
	context_window: number;
	prices: { input: number; cached: number; output: number } | null;
}

/** The answer of GET /api/v1/my/models. */
interface Clients {
	openai_base_url: string;
	models: ClientModel[];
}

/** omp's model, as models.yml and registerProvider take it. */
export interface OmpModel {
	id: string;
	name: string;
	contextWindow?: number;
	maxTokens?: number;
	cost?: { input: number; output: number; cacheRead: number; cacheWrite: number };
}

/**
 * omp's models for the hub's: a known length bounds the answer to a quarter
 * of it, at most omp's own default of 16,384 tokens, so that a long prompt
 * and its answer fit; an unknown one is left to omp (128,000 tokens).
 */
export function ompModels(models: ClientModel[]): OmpModel[] {
	return models.map(m => ({
		id: m.name,
		name: m.name,
		...(m.context_window > 0 ? { contextWindow: m.context_window, maxTokens: Math.min(16384, Math.floor(m.context_window / 4)) } : {}),
		...(m.prices ? { cost: { input: m.prices.input, output: m.prices.output, cacheRead: m.prices.cached, cacheWrite: 0 } } : {}),
	}));
}

/** Whether a models.yml defines the provider aigw itself: a key "aigw:" indented under providers. */
export function definesAigw(yaml: string): boolean {
	return /^\s+aigw:\s*(#.*)?$/m.test(yaml);
}

/**
 * The command omp runs for a token: aigw-token where the installer puts it,
 * or on the PATH. On Windows omp runs it with its own shell, which cannot run
 * the sh aigw-token: the PowerShell one that install.ps1 puts in place,
 * through Windows PowerShell, with a path in forward slashes.
 */
export function tokenCommand(platform: string = process.platform, home: string = homedir()): string {
	if (platform === "win32") {
		const ps1 = join(home, ".local", "share", "aigw", "aigw-token.ps1");
		if (!existsSync(ps1)) return "!aigw-token.cmd";
		return `!powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File "${ps1.replaceAll("\\", "/")}"`;
	}
	const installed = join(home, ".local", "bin", "aigw-token");
	return `!${existsSync(installed) ? installed : "aigw-token"}`;
}

/**
 * The tenant's models from the hub, or the last list kept when it cannot be
 * reached; the answer is kept for that. Undefined when there is neither.
 */
async function fetchClients(hub: string, key: string): Promise<{ clients?: Clients; error?: string }> {
	const kept = join(configDir(), "models.json");
	try {
		const res = await fetch(`${hub}/api/v1/my/models`, { headers: { Authorization: `Bearer ${key}` }, signal: AbortSignal.timeout(3000) });
		if (res.ok) {
			const clients = (await res.json()) as Clients;
			try {
				mkdirSync(configDir(), { recursive: true, mode: 0o700 });
				writeFileSync(kept, JSON.stringify(clients));
			} catch {}
			return { clients };
		}
		if (res.status === 401) return { error: "the hub does not accept this key" };
	} catch {}
	try {
		return { clients: JSON.parse(readFileSync(kept, "utf8")) as Clients, error: "the hub cannot be reached: the models are the last ones it gave" };
	} catch {
		return { error: "the hub cannot be reached and no list of models was kept" };
	}
}

interface State {
	fetched: number;
	budgets?: Budget[];
	usageEnabled?: boolean;
	error?: string;
	status?: number;
}

const OK = 0, NEAR = 1, SPENT = 2, BEST_EFFORT = 3, REFUSED = 4;

function dollars(credits: number): string {
	const d = credits / 100_000;
	const small = d !== 0 && Math.abs(d) < 0.01;
	return d.toLocaleString("en-US", { style: "currency", currency: "USD", minimumFractionDigits: 2, maximumFractionDigits: small ? 5 : 2 });
}

function tokens(n: number): string {
	for (const [size, letter] of [[1e9, "B"], [1e6, "M"], [1e3, "k"]] as const) {
		if (n >= size) return `${+(n / size).toFixed(n >= size * 100 ? 0 : 1)}${letter}`;
	}
	return String(n);
}

const amount = (n: number, unit: string) => (unit === "credits" ? dollars(n) : tokens(n));
const percent = (b: Budget) => (b.limit > 0 ? Math.floor((b.used * 100) / b.limit) : 100);
const usedOf = (b: Budget) => `${amount(b.used, b.unit)} of ${amount(b.limit, b.unit)}${b.unit === "tokens" ? " tokens" : ""}`;

function clock(iso: string): string {
	const t = new Date(iso);
	const time = t.toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit" });
	return t.toDateString() === new Date().toDateString() ? time : `${t.toLocaleDateString("en-GB", { day: "numeric", month: "short" })} ${time}`;
}

function level(b: Budget, warnAt: number): number {
	if (b.best_effort_capped) return REFUSED;
	if (b.best_effort_until) return BEST_EFFORT;
	if (!b.enforced) return OK;
	const p = percent(b);
	return p >= 100 ? SPENT : p >= warnAt ? NEAR : OK;
}

// The budget of the model in use that is furthest gone; without a budget
// on that model, the furthest gone of all.
function pick(budgets: Budget[], model: string | undefined, warnAt: number): Budget | undefined {
	const name = (model ?? "").toLowerCase();
	const mine = budgets.filter(b => b.across || b.model_name.toLowerCase() === name);
	return [...(mine.length ? mine : budgets)].sort((a, b) => level(b, warnAt) - level(a, warnAt) || percent(b) - percent(a))[0];
}

function line(b: Budget): string {
	const head = `${b.model_name} ${usedOf(b)} (${percent(b)}%)`;
	if (b.best_effort_capped && b.best_effort_until) return `${head} · refused until ${clock(b.best_effort_until)}`;
	if (b.best_effort_until) return `${head} · best-effort until ${clock(b.best_effort_until)}`;
	return `${head} · resets ${clock(b.resets_at)}${b.enforced ? "" : " · not enforced"}`;
}

function warning(b: Budget): string {
	if (b.best_effort_capped && b.best_effort_until) {
		return `${b.model_name}: your budget and the best-effort allowance are spent. Requests are refused until ${clock(b.best_effort_until)}.`;
	}
	if (b.best_effort_until) {
		return `${b.model_name}: your budget is spent (${usedOf(b)}). Requests are served as best-effort until ${clock(b.best_effort_until)}.`;
	}
	return percent(b) >= 100
		? `${b.model_name}: your budget is spent (${usedOf(b)}). Requests are refused until ${clock(b.resets_at)}.`
		: `${b.model_name}: ${percent(b)}% of your budget is used (${usedOf(b)}). It resets at ${clock(b.resets_at)}.`;
}

export default async function aigwUsage(pi: ExtensionAPI) {
	const login = helperLogin();
	const hub = (process.env.AIGW_HUB_URL || login.hub).replace(/\/+$/, "");
	const key = process.env.AIGW_API_KEY || login.key;
	const warnAt = Number(process.env.AIGW_WARN_AT) || 90;

	// The models, registered before omp picks one. Told once a session
	// starts, when something kept them out.
	let modelsNote = "";
	let yaml = "";
	try {
		yaml = readFileSync(join(homedir(), ".omp", "agent", "models.yml"), "utf8");
	} catch {}
	if (definesAigw(yaml)) {
		modelsNote = "~/.omp/agent/models.yml defines the provider aigw, so the gateway's models from the hub are not added. Remove it there to get them.";
	} else if (hub && key) {
		const { clients, error } = await fetchClients(hub, key);
		if (clients && !clients.openai_base_url) {
			modelsNote = "the hub is not told the gateway's address (config.gatewayURL), so no model is added.";
		} else if (clients) {
			pi.registerProvider("aigw", {
				baseUrl: clients.openai_base_url,
				api: "openai-completions",
				// With aigw-token signed in, omp runs it for a token, and again after a 401.
				apiKey: process.env.AIGW_API_KEY || tokenCommand(),
				models: ompModels(clients.models),
			});
		}
		if (error) modelsNote = `models: ${error}.`;
	}

	let state: State | undefined;
	// The level each budget was last warned at, by model, window and the end
	// of the window, so the next window starts clean.
	const told = new Map<string, number>();

	// Asks the hub unless the last answer is younger than maxAge seconds. A
	// failure is kept too: the hub blocks an address after ten wrong keys in
	// a minute, so a wrong key is not sent again for five minutes.
	async function refresh(maxAge: number): Promise<State> {
		const now = Date.now();
		const wait = state?.error ? (state.status === 401 ? 300 : 60) : maxAge;
		if (state && now - state.fetched < wait * 1000) return state;
		const last = state?.budgets;
		if (!hub || !key) {
			state = { fetched: now, error: !hub ? "no hub: sign in with aigw-token, or set AIGW_HUB_URL" : "no key: sign in with aigw-token, or set AIGW_API_KEY" };
			return state;
		}
		try {
			const res = await fetch(`${hub}/api/v1/my/usage`, { headers: { Authorization: `Bearer ${key}` }, signal: AbortSignal.timeout(4000) });
			if (res.ok) {
				const body = (await res.json()) as { usage_enabled: boolean; budgets?: Budget[]; budget?: TenantBudget | null };
				state = { fetched: now, budgets: budgetsOf(body), usageEnabled: body.usage_enabled };
			} else {
				const reasons: Record<number, string> = {
					401: "the hub does not accept this API key",
					404: "the hub has the tenants' page turned off",
					429: "too many wrong keys; the hub will answer again in a minute",
				};
				state = { fetched: now, status: res.status, error: reasons[res.status] ?? `the hub answered ${res.status}`, budgets: last };
			}
		} catch {
			state = { fetched: now, error: "the hub cannot be reached", budgets: last };
		}
		return state;
	}

	async function show(ctx: ExtensionContext, maxAge: number) {
		if (!ctx.hasUI) return;
		const s = await refresh(maxAge);
		if (!s.budgets) {
			ctx.ui.setStatus("aigw-usage", `usage: ${s.error}`);
			return;
		}
		const b = pick(s.budgets, ctx.model?.id, warnAt);
		ctx.ui.setStatus("aigw-usage", b ? line(b) + (s.error ? " · old" : "") : s.usageEnabled ? "usage: no budget" : "usage: the hub keeps no usage");
		if (s.error) return;
		const seen = new Set<string>();
		for (const budget of s.budgets) {
			const id = `${budget.model_name}|${budget.window}|${budget.resets_at}`;
			seen.add(id);
			const now = level(budget, warnAt);
			if (now > (told.get(id) ?? OK)) {
				told.set(id, now);
				ctx.ui.notify(warning(budget), now >= SPENT ? "error" : "warning");
			}
		}
		for (const id of told.keys()) if (!seen.has(id)) told.delete(id);
	}

	const quietly = (ctx: ExtensionContext, maxAge: number) => show(ctx, maxAge).catch(() => {});

	pi.setLabel("AI gateway");
	pi.on("session_start", async (_event, ctx) => {
		if (modelsNote && ctx.hasUI) ctx.ui.notify(`AI gateway: ${modelsNote}`, "warning");
		await quietly(ctx, 30);
	});
	// After a turn the counters have just moved, so ask again soon.
	pi.on("turn_end", async (_event, ctx) => quietly(ctx, 5));

	pi.registerCommand("aigw-usage", {
		description: "Show your budgets on the AI gateway",
		handler: async (_args, ctx) => {
			const s = await refresh(0);
			if (!s.budgets) return ctx.ui.notify(`AI gateway usage: ${s.error}`, "error");
			if (s.budgets.length === 0) return ctx.ui.notify("You have no budget on any model.", "info");
			ctx.ui.notify(s.budgets.map(line).join("\n") + (s.error ? `\n(old: ${s.error})` : ""), "info");
			await quietly(ctx, 30);
		},
	});
}
