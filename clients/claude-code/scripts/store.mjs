// The last answer of the hub, kept in a file. The hooks write it and the
// status line reads it, so the status line needs neither the key nor the
// network. The key is never written.

import { mkdirSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";

export const dir = process.env.AIGW_USAGE_DIR || join(homedir(), ".claude", "aigw-usage");

function read(name, fallback) {
  try {
    return JSON.parse(readFileSync(join(dir, name), "utf8"));
  } catch {
    return fallback;
  }
}

function write(name, value) {
  mkdirSync(dir, { recursive: true, mode: 0o700 });
  const tmp = join(dir, `${name}.${process.pid}.tmp`);
  writeFileSync(tmp, JSON.stringify(value), { mode: 0o600 });
  renameSync(tmp, join(dir, name));
}

export const cache = { read: () => read("usage.json", null), write: (v) => write("usage.json", v) };
export const told = { read: () => read("told.json", {}), write: (v) => write("told.json", v) };

/** Where the hub is and the key to ask with. Empty when not set up. */
export function config(env = process.env) {
  return {
    hub: (env.CLAUDE_PLUGIN_OPTION_HUB_URL || env.AIGW_HUB_URL || "").replace(/\/+$/, ""),
    key: env.CLAUDE_PLUGIN_OPTION_API_KEY || env.AIGW_API_KEY || env.ANTHROPIC_AUTH_TOKEN || env.ANTHROPIC_API_KEY || "",
    warnAt: Number(env.CLAUDE_PLUGIN_OPTION_WARN_AT || env.AIGW_WARN_AT) || 90,
  };
}

/**
 * Asks the hub unless the last answer is younger than maxAge seconds, and
 * returns what is known now. A failure is kept as well, so a hub that is
 * down or a wrong key is not asked again on every prompt: the hub blocks an
 * address after ten wrong keys in a minute.
 */
export async function refresh({ hub, key }, maxAge, now = Date.now()) {
  const last = cache.read();
  const wait = last?.error ? (last.status === 401 ? 300 : 60) : maxAge;
  if (last && last.hub === hub && now - last.fetched < wait * 1000) return last;
  if (!hub || !key) {
    const next = { hub, fetched: now, error: !hub ? "no hub address is set" : "no API key is set" };
    cache.write(next);
    return next;
  }
  let next;
  try {
    const res = await fetch(`${hub}/api/v1/my/usage`, { headers: { Authorization: `Bearer ${key}` }, signal: AbortSignal.timeout(4000) });
    if (res.ok) {
      const body = await res.json();
      next = { hub, fetched: now, tenant: body.tenant, usage_enabled: body.usage_enabled, budgets: body.budgets ?? [] };
    } else {
      const reasons = { 401: "the hub does not accept this API key", 404: "the hub has the tenants' page turned off", 429: "too many wrong keys; the hub will answer again in a minute" };
      next = { hub, fetched: now, status: res.status, error: reasons[res.status] ?? `the hub answered ${res.status}` };
    }
  } catch (err) {
    next = { hub, fetched: now, error: `the hub cannot be reached (${err.cause?.code ?? err.name})` };
  }
  // Keep the last budgets beside an error, so the status line can go on
  // showing them, marked as old.
  if (next.error && last?.budgets && last.hub === hub) Object.assign(next, { budgets: last.budgets, budgets_at: last.budgets_at ?? last.fetched });
  cache.write(next);
  return next;
}
