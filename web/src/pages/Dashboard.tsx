import { useEffect, useMemo, useRef, useState } from "react";
import { api, type Unit, type UsageQuota, type UsageReport, type Window } from "../api";
import { Bar, Donut, HISTORY, Legend, RateChart, short, topSlices } from "../charts";
import { formatDollars, formatNumber, useLoad, usePolling, windowLabel } from "../components";
import { REFRESH_MS } from "./Usage";

const windows: Window[] = ["1d", "1h", "1m"];
// Money and tokens cannot be added up, so the charts show one at a time.
const units: Unit[] = ["credits", "tokens"];
const unitLabel: Record<Unit, string> = { credits: "Dollars", tokens: "Tokens" };

/**
 * Remembers the total used at each refresh and turns the differences into a
 * rate. A total that drops means a window ended or a reset; that sample is
 * shown as zero instead of a negative rate.
 */
function useRate(report: UsageReport | null, unit: Unit): number[] {
  const last = useRef<{ at: number; total: number; unit: Unit } | null>(null);
  const [points, setPoints] = useState<number[]>([]);
  useEffect(() => {
    if (!report?.enabled) return;
    const at = new Date(report.at).getTime();
    const total = report.quotas.reduce((sum, q) => sum + (q.unit === unit ? q.used : 0), 0);
    const prev = last.current;
    last.current = { at, total, unit };
    if (prev && prev.unit !== unit) {
      setPoints([]);
      return;
    }
    if (!prev || at <= prev.at) return;
    const perMinute = Math.max(0, total - prev.total) / ((at - prev.at) / 60000);
    setPoints((p) => [...p, perMinute].slice(-HISTORY));
  }, [report, unit]);
  return points;
}

export default function Dashboard() {
  const { data, reload } = useLoad(() => api.usage());
  usePolling(reload, REFRESH_MS);
  const presentUnits = useMemo(() => units.filter((u) => data?.quotas.some((q) => q.unit === u)), [data]);
  const [pickedUnit, setPickedUnit] = useState<Unit | null>(null);
  const unit = pickedUnit && presentUnits.includes(pickedUnit) ? pickedUnit : (presentUnits[0] ?? "tokens");
  const rate = useRate(data, unit);
  const quotas = useMemo(() => (data?.quotas ?? []).filter((q) => q.unit === unit), [data, unit]);

  const present = useMemo(() => windows.filter((w) => quotas.some((q) => q.window === w)), [quotas]);
  const [picked, setPicked] = useState<Window | null>(null);
  const win = picked && present.includes(picked) ? picked : present[0];
  const show = (n: number) => formatNumber(n, unit);
  const total = (n: number) => (unit === "credits" ? formatDollars(Math.round(n / 100_000) * 100_000).replace(".00", "") : short(n));
  const what = unit === "credits" ? "money" : "tokens";

  if (!data) return null;
  // Say why there are no charts instead of leaving the page looking unfinished.
  if (!data.enabled || data.quotas.length === 0 || !win) {
    return (
      <section className="card">
        <h2>Usage right now</h2>
        {data.enabled ? (
          <p className="hint">
            No tenant has a quota yet, so there is nothing to chart. Add a cluster and a model, then set a quota on a
            tenant under <a href="#tenants">Tenants</a>. The charts appear here as soon as one exists.
          </p>
        ) : (
          <p className="hint">
            Usage monitoring is off. Set <code>redis.url</code> in the chart (or <code>REDIS_URL</code>) to the Redis
            that the gateways count quotas in, and live charts appear here.
          </p>
        )}
      </section>
    );
  }

  const inWindow = quotas.filter((q) => q.window === win);
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
  const hottest = [...quotas].sort((a, b) => ratio(b) - ratio(a)).slice(0, 5);
  const atLimit = quotas.filter((q) => ratio(q) >= 1).length;
  const near = quotas.filter((q) => ratio(q) >= 0.8 && ratio(q) < 1).length;
  const idle = quotas.filter((q) => q.used === 0).length;
  const dry = quotas.filter((q) => q.shadow).length;
  const pools = data.pools.filter((p) => p.unit === unit);
  // The last few samples, averaged: one refresh alone jumps around too much.
  const recent = rate.slice(-3);
  const now = recent.length > 0 ? recent.reduce((sum, r) => sum + r, 0) / recent.length : 0;

  return (
    <section className="card">
      <div className="dash-head">
        <div>
          <h2>Usage right now</h2>
          <p className="hint">
            Read from the gateways' counters in Redis. Each window is counted on its own. Models with prices are
            counted in dollars, the others in tokens.
          </p>
        </div>
        {presentUnits.length > 1 && (
          <div className="seg" role="group" aria-label="Unit">
            {presentUnits.map((u) => (
              <button key={u} type="button" className={u === unit ? "on" : ""} aria-pressed={u === unit} onClick={() => setPickedUnit(u)}>
                {unitLabel[u]}
              </button>
            ))}
          </div>
        )}
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
              title={`${show(used)} of ${show(allocated)} allocated ${what} used per ${windowLabel[win]}`}
              center={allocated > 0 ? `${Math.round((used / allocated) * 100)}%` : "0%"}
              sub="used"
              slices={[
                { label: "Used", value: used, color: "var(--chart-1)" },
                { label: "Free", value: Math.max(0, allocated - used), color: "var(--muted)" },
              ]}
            />
            <Legend
              format={show}
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
            <p className="detail">Nothing used in this window yet.</p>
          ) : (
            <div className="panel-chart">
              <Donut title={`Share of ${what} used, by tenant`} center={total(used + over)} sub={unit === "credits" ? "used" : "tokens"} slices={tenantSlices} />
              <Legend format={show} slices={tenantSlices} />
            </div>
          )}
        </div>

        <div className="panel">
          <h3>On which model</h3>
          {modelSlices.length === 0 ? (
            <p className="detail">Nothing used in this window yet.</p>
          ) : (
            <div className="panel-chart">
              <Donut title={`Share of ${what} used, by model`} center={String(modelSlices.length)} sub={modelSlices.length === 1 ? "model" : "models"} slices={modelSlices} />
              <Legend format={show} slices={modelSlices} />
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
              format={show}
              note={` per ${windowLabel[q.window]}${q.shadow ? ", dry run" : ""}`}
            />
          ))}
        </div>

        <div className="panel">
          <h3>Shared pool per model</h3>
          <p className="detail">
            Every request also draws from the model's pool. A tenant past its own quota keeps going while the pool has
            room left.
          </p>
          {pools.map((p) =>
            p.dry_run ? (
              <p className="detail" key={p.model_id}>
                {p.model_name}: {show(p.used)} used per {windowLabel[p.window]}. In dry-run the pool has no limit.
              </p>
            ) : (
              <Bar key={p.model_id} label={p.model_name} used={p.used} limit={p.limit} format={show} note={` per ${windowLabel[p.window]}`} />
            ),
          )}
        </div>

        <div className="panel">
          <h3>{unitLabel[unit]} per minute</h3>
          <div className="rate-now">
            {show(Math.round(now))}
            <span className="detail"> now, across all tenants</span>
          </div>
          <RateChart points={rate} />
          <p className="detail">Measured since you opened this page. Nothing is stored.</p>
        </div>
      </div>
    </section>
  );
}
