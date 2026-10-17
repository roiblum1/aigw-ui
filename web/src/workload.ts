// Turns a tenant's recorded requests and tokens into what its own page shows
// about its workload. Nothing here needs a browser, so it is tested with
// node alone.

import type { Step } from "./history";

/** What a tenant sent to one model in one hour or day, as the gateways counted it. */
export interface TokenPoint {
  at: string;
  model_name: string;
  requests: number;
  /** Every input token, the cached ones too. */
  input_tokens: number;
  cached_tokens: number;
  output_tokens: number;
}

/** A model's prices: credits for a million tokens. */
export interface Price {
  model_name: string;
  price_input: number;
  price_cached: number;
  price_output: number;
}

export interface Totals {
  requests: number;
  input: number;
  cached: number;
  output: number;
  /** The share of the input tokens served from the model server's cache, null without input. */
  cacheShare: number | null;
  /** Input and output tokens per request, null without requests. */
  promptPerRequest: number | null;
  answerPerRequest: number | null;
  /**
   * Credits the cache saved: each cached token at the input price less the
   * cached price. Null when no model with prices was used.
   */
  saved: number | null;
  /** Requests were counted, but no token: the model's answers report none to the gateway. */
  noTokens: boolean;
}

/** Adds points up, with the prices that apply to each model. */
export function totals(points: TokenPoint[], prices: Price[]): Totals {
  const price = new Map(prices.map((p) => [p.model_name, p]));
  let requests = 0;
  let input = 0;
  let cached = 0;
  let output = 0;
  let saved: number | null = null;
  for (const p of points) {
    requests += p.requests;
    input += p.input_tokens;
    cached += p.cached_tokens;
    output += p.output_tokens;
    const m = price.get(p.model_name);
    if (m) saved = (saved ?? 0) + (p.cached_tokens * Math.max(0, m.price_input - m.price_cached)) / 1_000_000;
  }
  return {
    requests,
    input,
    cached,
    output,
    cacheShare: input > 0 ? Math.min(1, cached / input) : null,
    promptPerRequest: requests > 0 ? input / requests : null,
    answerPerRequest: requests > 0 ? output / requests : null,
    saved: saved === null ? null : Math.round(saved),
    noTokens: requests > 0 && input === 0 && output === 0,
  };
}

export interface ModelTotals extends Totals {
  model: string;
}

/** Totals for each model, the one with the most input first. */
export function byModel(points: TokenPoint[], prices: Price[]): ModelTotals[] {
  const groups = new Map<string, TokenPoint[]>();
  for (const p of points) {
    const g = groups.get(p.model_name);
    if (g) g.push(p);
    else groups.set(p.model_name, [p]);
  }
  return [...groups.entries()]
    .map(([model, ps]) => ({ model, ...totals(ps, prices) }))
    .sort((a, b) => b.input + b.output - (a.input + a.output) || a.model.localeCompare(b.model));
}

export interface Over {
  /** The start of every step, oldest first. */
  steps: Date[];
  series: { label: string; values: number[] }[];
  peak: number;
}

const STEP_MS: Record<Step, number> = { hour: 3_600_000, day: 86_400_000 };

function lay(points: TokenPoint[], from: string, step: Step, now: Date, parts: { label: string; of: (p: TokenPoint) => number }[]): Over {
  const size = STEP_MS[step];
  const start = new Date(from).getTime();
  const count = Math.max(1, Math.floor((now.getTime() - start) / size) + 1);
  const steps = Array.from({ length: count }, (_, i) => new Date(start + i * size));
  const series = parts.map((part) => ({ label: part.label, values: new Array<number>(count).fill(0) }));
  for (const p of points) {
    const i = Math.floor((new Date(p.at).getTime() - start) / size);
    if (i < 0 || i >= count) continue;
    parts.forEach((part, n) => (series[n].values[i] += part.of(p)));
  }
  const peak = Math.max(0, ...steps.map((_, i) => series.reduce((sum, s) => sum + s.values[i], 0)));
  return { steps, series, peak };
}

/** Tokens per step, stacked: the input the model computed, the input it had cached, and the output. */
export function tokensOver(points: TokenPoint[], from: string, step: Step, now: Date): Over {
  return lay(points, from, step, now, [
    { label: "new input", of: (p) => Math.max(0, p.input_tokens - p.cached_tokens) },
    { label: "cached input", of: (p) => p.cached_tokens },
    { label: "output", of: (p) => p.output_tokens },
  ]);
}

/** Requests per step. */
export function requestsOver(points: TokenPoint[], from: string, step: Step, now: Date): Over {
  return lay(points, from, step, now, [{ label: "requests", of: (p) => p.requests }]);
}

/** The share of each step's input served from the cache, in whole percent; 0 in a step without input. */
export function cacheOver(points: TokenPoint[], from: string, step: Step, now: Date): Over {
  const input = lay(points, from, step, now, [{ label: "input", of: (p) => p.input_tokens }]);
  const cached = lay(points, from, step, now, [{ label: "cached", of: (p) => p.cached_tokens }]);
  const values = input.series[0].values.map((n, i) => (n > 0 ? Math.round((100 * Math.min(n, cached.series[0].values[i])) / n) : 0));
  return { steps: input.steps, series: [{ label: "from the cache", values }], peak: values.some((v) => v > 0) ? 100 : 0 };
}

/** A token count in words a person reads: 950, 12.4k, 3.1M. */
export function shortTokens(n: number): string {
  const one = (v: number) => (v < 100 ? v.toFixed(1).replace(/\.0$/, "") : String(Math.round(v)));
  if (n < 1000) return String(Math.round(n));
  if (n < 1_000_000) return `${one(n / 1000)}k`;
  return `${one(n / 1_000_000)}M`;
}

/** A share as whole percent, or a dash when there is none. */
export function percent(share: number | null): string {
  return share === null ? "—" : `${Math.round(share * 100)}%`;
}
