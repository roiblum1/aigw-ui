import { useEffect, useMemo, useRef, useState } from "react";
import { api, type UsageQuota, type UsageReport, type Window } from "./api";
import { useLoad, windowLabel } from "./components";

const REFRESH_MS = 5000;
/** Points kept for the live rate chart: ten minutes at one sample per refresh. */
const HISTORY = 120;
const PALETTE = ["var(--chart-1)", "var(--chart-2)", "var(--chart-3)", "var(--chart-4)", "var(--chart-5)", "var(--chart-6)"];
const windows: Window[] = ["1d", "1h", "1m"];

const fmt = (n: number) => n.toLocaleString("en-US");
/** 1,234,567 -> 1.2M, for the middle of a donut where space is tight. */
function short(n: number): string {
  if (n >= 1e9) return `${(n / 1e9).toFixed(1)}B`;
  if (n >= 1e6) return `${(n / 1e6).toFixed(1)}M`;
  if (n >= 1e4) return `${Math.round(n / 1e3)}k`;
  return fmt(n);
}

interface Slice {
  label: string;
  value: number;
  color: string;
}

/**
 * A donut drawn with stroke dashes on a circle whose circumference is 100, so
 * a slice's length is its percentage.
 */
function Donut(props: { slices: Slice[]; title: string; center: string; sub: string }) {
  const total = props.slices.reduce((sum, s) => sum + s.value, 0);
  let offset = 25; // start at twelve o'clock
  return (
    <svg viewBox="0 0 42 42" className="donut" role="img" aria-label={props.title}>
      <circle cx="21" cy="21" r="15.915" fill="none" stroke="var(--muted)" strokeWidth="5" />
      {total > 0 &&
        props.slices
          .filter((s) => s.value > 0)
          .map((s) => {
            const len = (s.value / total) * 100;
            const el = (
              <circle
                key={s.label}
                cx="21"
                cy="21"
                r="15.915"
                fill="none"
                stroke={s.color}
                strokeWidth="5"
                strokeDasharray={`${len} ${100 - len}`}
                strokeDashoffset={offset}
              >
                <title>{`${s.label}: ${fmt(s.value)} (${Math.round(len)}%)`}</title>
              </circle>
            );
            offset -= len;
            return el;
          })}
      <text x="21" y="20.5" textAnchor="middle" className="donut-value">
        {props.center}
      </text>
      <text x="21" y="25.5" textAnchor="middle" className="donut-sub">
        {props.sub}
      </text>
    </svg>
  );
}

function Legend({ slices, unit }: { slices: Slice[]; unit?: string }) {
  return (
    <ul className="legend">
      {slices.map((s) => (
        <li key={s.label}>
          <i style={{ background: s.color }} aria-hidden />
          <span className="legend-label" title={s.label}>
            {s.label}
          </span>
          <span className="legend-value">
            {fmt(s.value)}
            {unit}
          </span>
        </li>
      ))}
    </ul>
  );
}

/** Tokens per minute over the samples taken while this page has been open. */
function RateChart({ points }: { points: number[] }) {
  if (points.length < 2) return <p className="detail">Collecting… the first reading appears after a few seconds.</p>;
  const max = Math.max(...points, 1);
  const w = 300;
  const h = 70;
  // Spread the first minute of samples over the width instead of a thin strip.
  const step = w / (Math.max(points.length, 13) - 1);
  const start = w - (points.length - 1) * step;
  const xy = points.map((p, i) => `${(start + i * step).toFixed(1)},${(h - 4 - (p / max) * (h - 12)).toFixed(1)}`);
  return (
    <svg viewBox={`0 0 ${w} ${h}`} className="rate" role="img" aria-label="Tokens per minute while this page has been open" preserveAspectRatio="none">
      <polygon points={`${start},${h} ${xy.join(" ")} ${w},${h}`} fill="var(--chart-1)" opacity="0.15" />
      <polyline points={xy.join(" ")} fill="none" stroke="var(--chart-1)" strokeWidth="1.8" vectorEffect="non-scaling-stroke" />
    </svg>
  );
}

function Bar(props: { label: string; used: number; limit: number; note?: string }) {
  const ratio = props.limit > 0 ? props.used / props.limit : 0;
  const level = ratio >= 1 ? "full" : ratio >= 0.8 ? "high" : "ok";
  return (
    <div className="hbar">
      <div className="hbar-head">
        <span className="legend-label" title={props.label}>
          {props.label}
        </span>
        <span className="legend-value">{Math.round(ratio * 100)}%</span>
      </div>
      <div className={`meter ${level}`}>
        <span style={{ width: `${Math.min(ratio * 100, 100)}%` }} />
      </div>
      <div className="detail">
        {fmt(props.used)} of {fmt(props.limit)}
        {props.note}
      </div>
    </div>
  );
}

/**
 * Remembers the total used at each refresh and turns the differences into a
 * rate. A total that drops means a window ended or a reset; that sample is
 * shown as zero instead of a negative rate.
 */
function useRate(report: UsageReport | null): number[] {
  const last = useRef<{ at: number; total: number } | null>(null);
  const [points, setPoints] = useState<number[]>([]);
  useEffect(() => {
    if (!report?.enabled) return;
    const at = new Date(report.at).getTime();
    const total = report.quotas.reduce((sum, q) => sum + q.used, 0);
    const prev = last.current;
    last.current = { at, total };
    if (!prev || at <= prev.at) return;
    const perMinute = Math.max(0, total - prev.total) / ((at - prev.at) / 60000);
    setPoints((p) => [...p, perMinute].slice(-HISTORY));
  }, [report]);
  return points;
}

/** Folds everything after the first five slices into one "others" slice. */
function topSlices(items: { label: string; value: number }[]): Slice[] {
  const sorted = items.filter((i) => i.value > 0).sort((a, b) => b.value - a.value);
  const top = sorted.slice(0, 5).map((s, i) => ({ ...s, color: PALETTE[i] }));
  const rest = sorted.slice(5).reduce((sum, s) => sum + s.value, 0);
  if (rest > 0) top.push({ label: `${sorted.length - 5} others`, value: rest, color: PALETTE[5] });
  return top;
}

export default function Dashboard() {
  const { data, reload } = useLoad(() => api.usage());
  useEffect(() => {
    const timer = setInterval(() => {
      if (!document.hidden) reload();
    }, REFRESH_MS);
    return () => clearInterval(timer);
  }, [reload]);
  const rate = useRate(data);

  const present = useMemo(() => windows.filter((w) => data?.quotas.some((q) => q.window === w)), [data]);
  const [picked, setPicked] = useState<Window | null>(null);
  const win = picked && present.includes(picked) ? picked : present[0];

  // Without Redis there is nothing to draw; the Usage page explains how to turn it on.
  if (!data?.enabled || data.quotas.length === 0 || !win) return null;

  const inWindow = data.quotas.filter((q) => q.window === win);
  // A dry-run quota can go past its limit, which would make "free" negative.
  const allocated = inWindow.reduce((sum, q) => sum + q.limit, 0);
  const used = inWindow.reduce((sum, q) => sum + Math.min(q.used, q.limit), 0);
  const over = inWindow.reduce((sum, q) => sum + Math.max(0, q.used - q.limit), 0);

  const byTenant = new Map<string, number>();
  for (const q of inWindow) byTenant.set(q.tenant_slug, (byTenant.get(q.tenant_slug) ?? 0) + q.used);
  const tenantSlices = topSlices([...byTenant].map(([label, value]) => ({ label, value })));
  const byModel = new Map<string, number>();
  for (const q of inWindow) byModel.set(q.model_name, (byModel.get(q.model_name) ?? 0) + q.used);
  const modelSlices = topSlices([...byModel].map(([label, value]) => ({ label, value })));

  const ratio = (q: UsageQuota) => (q.limit > 0 ? q.used / q.limit : 0);
  const hottest = [...data.quotas].sort((a, b) => ratio(b) - ratio(a)).slice(0, 5);
  const atLimit = data.quotas.filter((q) => ratio(q) >= 1).length;
  const near = data.quotas.filter((q) => ratio(q) >= 0.8 && ratio(q) < 1).length;
  const idle = data.quotas.filter((q) => q.used === 0).length;
  const dry = data.quotas.filter((q) => q.shadow).length;
  // The last few samples, averaged: one refresh alone jumps around too much.
  const recent = rate.slice(-3);
  const now = recent.length > 0 ? recent.reduce((sum, r) => sum + r, 0) / recent.length : 0;

  return (
    <section className="card">
      <div className="dash-head">
        <div>
          <h2>Token usage right now</h2>
          <p className="hint">Read from the gateways' counters in Redis. Each window is counted on its own.</p>
        </div>
        <div className="seg" role="group" aria-label="Quota window">
          {present.map((w) => (
            <button key={w} type="button" className={w === win ? "on" : ""} aria-pressed={w === win} onClick={() => setPicked(w)}>
              Per {windowLabel[w]}
            </button>
          ))}
        </div>
      </div>

      <div className="chips">
        <span className={atLimit ? "chip danger" : "chip"}>
          <b>{atLimit}</b> at the limit
        </span>
        <span className={near ? "chip warn" : "chip"}>
          <b>{near}</b> above 80%
        </span>
        <span className="chip">
          <b>{idle}</b> not used in this window
        </span>
        <span className="chip">
          <b>{dry}</b> on dry run
        </span>
      </div>

      <div className="dash-grid">
        <div className="panel">
          <h3>Allocated and used</h3>
          <div className="panel-chart">
            <Donut
              title={`${fmt(used)} of ${fmt(allocated)} allocated tokens used per ${windowLabel[win]}`}
              center={allocated > 0 ? `${Math.round((used / allocated) * 100)}%` : "0%"}
              sub="used"
              slices={[
                { label: "Used", value: used, color: "var(--chart-1)" },
                { label: "Free", value: Math.max(0, allocated - used), color: "var(--muted)" },
              ]}
            />
            <Legend
              slices={[
                { label: "Allocated", value: allocated, color: "transparent" },
                { label: "Used", value: used, color: "var(--chart-1)" },
                { label: "Free", value: Math.max(0, allocated - used), color: "var(--input)" },
                ...(over > 0 ? [{ label: "Over the limit (dry run)", value: over, color: "var(--danger)" }] : []),
              ]}
            />
          </div>
        </div>

        <div className="panel">
          <h3>Who is using it</h3>
          {tenantSlices.length === 0 ? (
            <p className="detail">No tokens used in this window yet.</p>
          ) : (
            <div className="panel-chart">
              <Donut title="Share of tokens used, by tenant" center={short(used + over)} sub="tokens" slices={tenantSlices} />
              <Legend slices={tenantSlices} />
            </div>
          )}
        </div>

        <div className="panel">
          <h3>On which model</h3>
          {modelSlices.length === 0 ? (
            <p className="detail">No tokens used in this window yet.</p>
          ) : (
            <div className="panel-chart">
              <Donut title="Share of tokens used, by model" center={String(modelSlices.length)} sub={modelSlices.length === 1 ? "model" : "models"} slices={modelSlices} />
              <Legend slices={modelSlices} />
            </div>
          )}
        </div>

        <div className="panel">
          <h3>Closest to the limit</h3>
          {hottest.map((q) => (
            <Bar
              key={q.tenant_id + q.model_id}
              label={`${q.tenant_slug} · ${q.model_name}`}
              used={q.used}
              limit={q.limit}
              note={` per ${windowLabel[q.window]}${q.shadow ? ", dry run" : ""}`}
            />
          ))}
        </div>

        <div className="panel">
          <h3>Shared pool per model</h3>
          <p className="detail">
            Every request also draws from the model's pool. A tenant past its own quota keeps going while the pool has
            tokens left.
          </p>
          {data.pools.map((p) => (
            <Bar key={p.model_id} label={p.model_name} used={p.used} limit={p.limit} note={` per ${windowLabel[p.window]}`} />
          ))}
        </div>

        <div className="panel">
          <h3>Tokens per minute</h3>
          <div className="rate-now">
            {fmt(Math.round(now))}
            <span className="detail"> now, across all tenants</span>
          </div>
          <RateChart points={rate} />
          <p className="detail">Measured since you opened this page. Nothing is stored.</p>
        </div>
      </div>
    </section>
  );
}
