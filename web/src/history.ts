// Turns the recorded usage into what a chart of usage over time draws.
// Nothing here needs a browser, so it is tested with node alone.

import type { Unit } from "./money";

export interface Point {
  at: string;
  tenant_slug: string;
  model_name: string;
  unit: Unit;
  used: number;
  best_effort: number;
}

export type Step = "hour" | "day";
export type GroupBy = "tenant" | "model";

export interface Series {
  label: string;
  /** One value per step, within budget and best-effort added up. */
  values: number[];
  total: number;
}

export interface Stacked {
  /** The start of every step, oldest first. */
  steps: Date[];
  /** The largest first. At most `most` of them; the rest is one "others". */
  series: Series[];
  /** The sum of every step, and how much of it was best-effort. */
  total: number;
  bestEffort: number;
  /** The highest step. */
  peak: number;
  peakAt: Date | null;
}

const STEP_MS: Record<Step, number> = { hour: 3_600_000, day: 86_400_000 };

/**
 * Lays the points of one unit out over the steps from `from` up to `now`,
 * one series per tenant or per model.
 */
export function stack(points: Point[], unit: Unit, from: string, step: Step, now: Date, by: GroupBy, most = 5): Stacked {
  const size = STEP_MS[step];
  const start = new Date(from).getTime();
  const count = Math.max(1, Math.floor((now.getTime() - start) / size) + 1);
  const steps = Array.from({ length: count }, (_, i) => new Date(start + i * size));
  const byLabel = new Map<string, Series>();
  let bestEffort = 0;
  for (const p of points) {
    if (p.unit !== unit) continue;
    const i = Math.floor((new Date(p.at).getTime() - start) / size);
    if (i < 0 || i >= count) continue;
    const label = by === "tenant" ? p.tenant_slug : p.model_name;
    let s = byLabel.get(label);
    if (!s) byLabel.set(label, (s = { label, values: new Array<number>(count).fill(0), total: 0 }));
    s.values[i] += p.used + p.best_effort;
    s.total += p.used + p.best_effort;
    bestEffort += p.best_effort;
  }
  const sorted = [...byLabel.values()].sort((a, b) => b.total - a.total || a.label.localeCompare(b.label));
  const series = sorted.slice(0, most);
  const rest = sorted.slice(most);
  if (rest.length > 0) {
    const others: Series = { label: `${rest.length} others`, values: new Array<number>(count).fill(0), total: 0 };
    for (const s of rest) {
      s.values.forEach((v, i) => (others.values[i] += v));
      others.total += s.total;
    }
    series.push(others);
  }
  let peak = 0;
  let peakAt: Date | null = null;
  steps.forEach((at, i) => {
    const sum = series.reduce((n, s) => n + s.values[i], 0);
    if (sum > peak) [peak, peakAt] = [sum, at];
  });
  return { steps, series, total: series.reduce((n, s) => n + s.total, 0), bestEffort, peak, peakAt };
}

export interface Row {
  label: string;
  used: number;
  bestEffort: number;
}

/** What each tenant or model used in all the points of one unit, the largest first. */
export function totals(points: Point[], unit: Unit, by: GroupBy): Row[] {
  const rows = new Map<string, Row>();
  for (const p of points) {
    if (p.unit !== unit) continue;
    const label = by === "tenant" ? p.tenant_slug : p.model_name;
    let r = rows.get(label);
    if (!r) rows.set(label, (r = { label, used: 0, bestEffort: 0 }));
    r.used += p.used;
    r.bestEffort += p.best_effort;
  }
  return [...rows.values()].sort((a, b) => b.used + b.bestEffort - (a.used + a.bestEffort) || a.label.localeCompare(b.label));
}

/** A step's start as an axis label, in UTC like the gateway's days. */
export function stepLabel(at: Date, step: Step): string {
  const day = at.toLocaleDateString("en-GB", { day: "numeric", month: "short", timeZone: "UTC" });
  if (step === "day") return day;
  return `${day} ${String(at.getUTCHours()).padStart(2, "0")}:00`;
}
