// Oh My Pi extension: shows your budget on the AI gateway in the status
// area and tells you once when it is nearly spent, spent, or served as
// best-effort. /aigw-usage lists every budget.
//
// Install: copy this file to ~/.omp/agent/extensions/ and set
//   AIGW_HUB_URL   the address of the AI Gateway Control hub
//   AIGW_API_KEY   your key for the gateway
// AIGW_WARN_AT is the percent to warn at (90).
//
// It asks the hub only, at most every 30 seconds and after each turn. No
// prompt and no answer is sent anywhere.

import type { ExtensionAPI, ExtensionContext } from "@oh-my-pi/pi-coding-agent";

interface Budget {
	model_name: string;
	unit: "tokens" | "credits";
	limit: number;
	window: string;
	used: number;
	resets_at: string;
	enforced: boolean;
	best_effort_until?: string;
	best_effort_capped?: boolean;
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
	const mine = budgets.filter(b => b.model_name.toLowerCase() === name);
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

export default function aigwUsage(pi: ExtensionAPI) {
	const hub = (process.env.AIGW_HUB_URL ?? "").replace(/\/+$/, "");
	const key = process.env.AIGW_API_KEY ?? "";
	const warnAt = Number(process.env.AIGW_WARN_AT) || 90;

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
			state = { fetched: now, error: !hub ? "AIGW_HUB_URL is not set" : "AIGW_API_KEY is not set" };
			return state;
		}
		try {
			const res = await fetch(`${hub}/api/v1/my/usage`, { headers: { Authorization: `Bearer ${key}` }, signal: AbortSignal.timeout(4000) });
			if (res.ok) {
				const body = (await res.json()) as { usage_enabled: boolean; budgets?: Budget[] };
				state = { fetched: now, budgets: body.budgets ?? [], usageEnabled: body.usage_enabled };
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

	pi.setLabel("AI gateway usage");
	pi.on("session_start", async (_event, ctx) => quietly(ctx, 30));
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
