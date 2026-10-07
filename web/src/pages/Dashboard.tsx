import { useEffect, useMemo, useRef, useState } from "react";
import { api, type UsageQuota, type UsageReport, type Window } from "../api";
import { Bar, Donut, HISTORY, Legend, RateChart, fmt, short, topSlices } from "../charts";
import { useLoad, usePolling, windowLabel } from "../components";
import { REFRESH_MS } from "./Usage";

const windows: Window[] = ["1d", "1h", "1m"];

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

export default function Dashboard() {
  const { data, reload } = useLoad(() => api.usage());
  usePolling(reload, REFRESH_MS);
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
