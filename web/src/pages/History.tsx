import { useEffect, useMemo, useState } from "react";
import { api, type Step, type Unit, type UsagePoint } from "../api";
import { Legend, TimeBars, paletteColor } from "../charts";
import { ErrorBanner, formatNumber, useLoad, usePolling } from "../components";
import { stack, stepLabel, totals, type GroupBy } from "../history";

const ranges = [
  { label: "2 days", step: "hour" as Step, days: 2 },
  { label: "7 days", step: "day" as Step, days: 7 },
  { label: "30 days", step: "day" as Step, days: 30 },
  { label: "90 days", step: "day" as Step, days: 90 },
];
const units: Unit[] = ["credits", "tokens"];
const unitLabel: Record<Unit, string> = { credits: "Dollars", tokens: "Tokens" };

/** The chart, the figures and the table of one set of points. Also the tenants' own page draws it. */
export function HistoryView(props: { points: UsagePoint[]; from: string; step: Step; unit: Unit; by: GroupBy; now: Date }) {
  const { points, unit, step, by } = props;
  const show = (n: number) => formatNumber(n, unit);
  const stacked = useMemo(() => stack(points, unit, props.from, step, props.now, by), [points, unit, props.from, step, props.now, by]);
  const rows = useMemo(() => totals(points, unit, by), [points, unit, by]);
  const perStep = stacked.total / stacked.steps.length;
  return (
    <>
      <div className="stat-row">
        <div>
          <b>{show(stacked.total)}</b>
          <span>used in this time</span>
        </div>
        <div>
          <b>{show(Math.round(perStep))}</b>
          <span>on average per {step}</span>
        </div>
        <div>
          <b>{show(stacked.peak)}</b>
          <span>{stacked.peakAt ? `most in one ${step}, ${stepLabel(stacked.peakAt, step)}` : `most in one ${step}`}</span>
        </div>
        <div>
          <b>{show(stacked.bestEffort)}</b>
          <span>of it as best-effort{unit === "credits" ? ", not charged to a budget" : ""}</span>
        </div>
      </div>
      <div className="history-split">
        <div>
          <TimeBars
            steps={stacked.steps}
            series={stacked.series}
            peak={stacked.peak}
            label={(at) => stepLabel(at, step)}
            format={show}
            title={`Usage per ${step}, by ${by}`}
          />
          <Legend format={show} slices={stacked.series.map((s, i) => ({ label: s.label, value: s.total, color: paletteColor(i) }))} />
        </div>
        {rows.length > 0 && (
          <table className="plain">
            <thead>
              <tr>
                <th>{by === "tenant" ? "Tenant" : "Model"}</th>
                <th className="text-right">Within budget</th>
                <th className="text-right">Best-effort</th>
              </tr>
            </thead>
            <tbody>
              {rows.slice(0, 10).map((r) => (
                <tr key={r.label}>
                  <td className="strong">{r.label}</td>
                  <td className="text-right tabular-nums">{show(r.used)}</td>
                  <td className="text-right tabular-nums">{r.bestEffort > 0 ? show(r.bestEffort) : "—"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </>
  );
}

export function Seg<T extends string>(props: { label: string; options: readonly { value: T; label: string }[]; value: T; onChange: (v: T) => void }) {
  return (
    <div className="seg" role="group" aria-label={props.label}>
      {props.options.map((o) => (
        <button key={o.value} type="button" className={o.value === props.value ? "on" : ""} aria-pressed={o.value === props.value} onClick={() => props.onChange(o.value)}>
          {o.label}
        </button>
      ))}
    </div>
  );
}

/** Usage over time for every tenant, on the Overview. */
export default function History() {
  const [range, setRange] = useState("30 days");
  const picked = ranges.find((r) => r.label === range) ?? ranges[2];
  const { data, error, reload } = useLoad(() => api.usageHistory(picked.step, picked.days));
  usePolling(reload, 60_000);
  useEffect(() => {
    reload();
  }, [range, reload]);
  const [by, setBy] = useState<GroupBy>("tenant");
  const present = useMemo(() => units.filter((u) => data?.points.some((p) => p.unit === u)), [data]);
  const [pickedUnit, setPickedUnit] = useState<Unit | null>(null);
  const unit = pickedUnit && present.includes(pickedUnit) ? pickedUnit : (present[0] ?? "credits");
  const now = useMemo(() => new Date(), [data]);

  return (
    <section className="card">
      <div className="dash-head">
        <div>
          <h2>Usage over time</h2>
          <p className="hint">
            What the gateways counted, kept per hour. Days and hours are in UTC, like the gateways' quota periods.
          </p>
        </div>
        {present.length > 1 && (
          <Seg label="Unit" value={unit} onChange={setPickedUnit} options={present.map((u) => ({ value: u, label: unitLabel[u] }))} />
        )}
        <Seg label="Group by" value={by} onChange={setBy} options={[{ value: "tenant", label: "By tenant" }, { value: "model", label: "By model" }]} />
        <Seg label="Time" value={picked.label} onChange={setRange} options={ranges.map((r) => ({ value: r.label, label: r.label }))} />
      </div>
      <ErrorBanner message={error} />
      {data && data.points.length === 0 && (
        <p className="hint">
          Nothing is recorded for this time yet. The server reads the counters once a minute, so the first usage shows
          up here about a minute after a tenant with a quota sends a request.
        </p>
      )}
      {data && data.points.length > 0 && <HistoryView points={data.points} from={data.from} step={data.step} unit={unit} by={by} now={now} />}
    </section>
  );
}
