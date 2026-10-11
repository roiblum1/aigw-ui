// What a tenant has used of its budgets, as text. Nothing here reads a file
// or the network, so it is tested with node alone.

export const CREDITS_PER_DOLLAR = 100_000;

/** Credits as money: two decimals, and more only for an amount below a cent. */
export function dollars(credits) {
  const d = credits / CREDITS_PER_DOLLAR;
  const small = d !== 0 && Math.abs(d) < 0.01;
  return d.toLocaleString("en-US", { style: "currency", currency: "USD", minimumFractionDigits: 2, maximumFractionDigits: small ? 5 : 2 });
}

/** A token count in at most four characters and a letter: 950, 12.5k, 2.5M. */
export function tokens(n) {
  for (const [size, letter] of [[1e9, "B"], [1e6, "M"], [1e3, "k"]]) {
    if (n >= size) return `${+(n / size).toFixed(n >= size * 100 ? 0 : 1)}${letter}`;
  }
  return String(n);
}

export function amount(n, unit) {
  return unit === "credits" ? dollars(n) : tokens(n);
}

/** The share of a budget that is used, in whole percent, rounded down. */
export function percent(b) {
  return b.limit > 0 ? Math.floor((b.used * 100) / b.limit) : 100;
}

/** A time of day for a moment today, and the date too for a later day. */
export function clock(iso, now = new Date()) {
  const t = new Date(iso);
  const time = t.toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit" });
  return t.toDateString() === now.toDateString() ? time : `${t.toLocaleDateString("en-GB", { day: "numeric", month: "short" })} ${time}`;
}

// How far a budget is gone. A higher level is worth telling the user about
// once more.
export const OK = 0, NEAR = 1, SPENT = 2, BEST_EFFORT = 3, REFUSED = 4;

export function level(b, warnAt = 90) {
  if (b.best_effort_capped) return REFUSED;
  if (b.best_effort_until) return BEST_EFFORT;
  if (!b.enforced) return OK;
  const p = percent(b);
  return p >= 100 ? SPENT : p >= warnAt ? NEAR : OK;
}

/**
 * The budget to show for a model: the one of that model that is furthest
 * gone, since a model can have an hourly and a daily budget. Without a
 * budget on the model, the furthest gone of all.
 */
export function pick(budgets, model) {
  const name = (model ?? "").toLowerCase();
  const mine = budgets.filter((b) => b.model_name.toLowerCase() === name);
  const from = mine.length ? mine : budgets;
  return [...from].sort((a, b) => level(b) - level(a) || percent(b) - percent(a))[0];
}

/** One line for the status line. */
export function line(b, now = new Date()) {
  const of = `${amount(b.used, b.unit)} of ${amount(b.limit, b.unit)}${b.unit === "tokens" ? " tokens" : ""}`;
  const head = `${b.model_name} ${of} (${percent(b)}%)`;
  if (b.best_effort_capped) return `${head} · refused until ${clock(b.best_effort_until, now)}`;
  if (b.best_effort_until) return `${head} · best-effort until ${clock(b.best_effort_until, now)}`;
  return `${head} · resets ${clock(b.resets_at, now)}${b.enforced ? "" : " · not enforced"}`;
}

/** What to tell the user when a budget reached this level. */
export function warning(b, now = new Date()) {
  const what = `${amount(b.used, b.unit)} of ${amount(b.limit, b.unit)}${b.unit === "tokens" ? " tokens" : ""}`;
  if (b.best_effort_capped) {
    return `${b.model_name}: your budget and the best-effort allowance are spent. Requests are refused until ${clock(b.best_effort_until, now)}.`;
  }
  if (b.best_effort_until) {
    return `${b.model_name}: your budget is spent (${what}). Requests are served as best-effort until ${clock(b.best_effort_until, now)}.`;
  }
  return percent(b) >= 100
    ? `${b.model_name}: your budget is spent (${what}). Requests are refused until ${clock(b.resets_at, now)}.`
    : `${b.model_name}: ${percent(b)}% of your budget is used (${what}). It resets at ${clock(b.resets_at, now)}.`;
}

/**
 * The budgets that reached a higher level than the user was told about, and
 * the levels to remember. A budget is known by its model, window and the
 * end of the window, so the next window starts clean.
 */
export function due(budgets, told, warnAt = 90) {
  const next = {};
  const out = [];
  for (const b of budgets) {
    const key = `${b.model_name}|${b.window}|${b.resets_at}`;
    const now = level(b, warnAt);
    next[key] = Math.max(now, told[key] ?? OK);
    if (now > (told[key] ?? OK)) out.push(b);
  }
  return { budgets: out, told: next };
}
