// What each key of a tenant used, as the tenant's members see it after
// signing in with OpenShift and an admin sees it on the Tenants page: a row
// per key, the biggest first, with its cost at the models' prices. Nothing
// here needs a browser, so it is tested with node alone.

/** A model's prices, in credits for a million tokens. */
export interface KeyPrice {
  model_name: string;
  price_input: number;
  price_cached: number;
  price_output: number;
}

/** What one key sent to one model over the period. */
export interface KeyModelUse {
  model_name: string;
  requests: number;
  /** Every input token, the cached ones too. */
  input_tokens: number;
  cached_tokens: number;
  output_tokens: number;
}

/** What one key of the tenant used over the period, as the hub answers. */
export interface KeyUse {
  key_id: string;
  name: string;
  /** The OpenShift user who made the key by signing in; "" for an admin's key. */
  owner: string;
  kind: string;
  revoked_at: string | null;
  /** The key of the person who is looking. */
  me: boolean;
  models: KeyModelUse[];
  totals: { requests: number; input_tokens: number; cached_tokens: number; output_tokens: number };
}

export interface KeyUses {
  from: string;
  days: number;
  keys: KeyUse[];
}

/**
 * What a key's use cost, in credits: input not from the cache at the input
 * price, cached input at the cached price, output at the output price. Null
 * when it used no model with prices.
 */
export function keyCost(models: KeyModelUse[], prices: KeyPrice[]): number | null {
  const price = new Map(prices.map((p) => [p.model_name, p]));
  let cost: number | null = null;
  for (const m of models) {
    const p = price.get(m.model_name);
    if (!p) continue;
    const fresh = Math.max(0, m.input_tokens - m.cached_tokens);
    cost = (cost ?? 0) + (fresh * p.price_input + m.cached_tokens * p.price_cached + m.output_tokens * p.price_output) / 1_000_000;
  }
  return cost === null ? null : Math.round(cost);
}

export interface PersonRow {
  key: KeyUse;
  /** Who the row is: the key's owner, or its name for an admin's key. */
  who: string;
  cost: number | null;
  /** The row's share of the tenant's cost, or of its tokens without prices; null when nothing was used. */
  share: number | null;
}

/**
 * The rows of the table: the most expensive first, then the most tokens,
 * then by name. A revoked key is shown only when it used something.
 */
export function peopleRows(keys: KeyUse[], prices: KeyPrice[]): PersonRow[] {
  const tokens = (k: KeyUse) => k.totals.input_tokens + k.totals.output_tokens;
  const rows = keys
    .filter((k) => !k.revoked_at || k.totals.requests > 0)
    .map((k) => ({ key: k, who: k.owner || k.name, cost: keyCost(k.models, prices), share: null as number | null }));
  const priced = rows.some((r) => r.cost !== null);
  const total = rows.reduce((sum, r) => sum + (priced ? (r.cost ?? 0) : tokens(r.key)), 0);
  for (const r of rows) r.share = total > 0 ? (priced ? (r.cost ?? 0) : tokens(r.key)) / total : null;
  return rows.sort(
    (a, b) => (b.cost ?? -1) - (a.cost ?? -1) || tokens(b.key) - tokens(a.key) || a.who.localeCompare(b.who),
  );
}
