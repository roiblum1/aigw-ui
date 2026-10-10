/** Small SVG charts. Drawn by hand so the UI needs no chart library. */

/** Points kept for the live rate chart: ten minutes at one sample every five seconds. */
export const HISTORY = 120;
const PALETTE = ["var(--chart-1)", "var(--chart-2)", "var(--chart-3)", "var(--chart-4)", "var(--chart-5)", "var(--chart-6)"];

export const fmt = (n: number) => n.toLocaleString("en-US");
/** 1,234,567 -> 1.2M, for the middle of a donut where space is tight. */
export function short(n: number): string {
  if (n >= 1e9) return `${(n / 1e9).toFixed(1)}B`;
  if (n >= 1e6) return `${(n / 1e6).toFixed(1)}M`;
  if (n >= 1e4) return `${Math.round(n / 1e3)}k`;
  return fmt(n);
}

export interface Slice {
  label: string;
  value: number;
  color: string;
}

/**
 * A donut drawn with stroke dashes on a circle whose circumference is 100, so
 * a slice's length is its percentage.
 */
export function Donut(props: { slices: Slice[]; title: string; center: string; sub: string }) {
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

export function Legend({ slices, unit, format = fmt }: { slices: Slice[]; unit?: string; format?: (n: number) => string }) {
  return (
    <ul className="legend">
      {slices.map((s) => (
        <li key={s.label}>
          <i style={{ background: s.color }} aria-hidden />
          <span className="legend-label" title={s.label}>
            {s.label}
          </span>
          <span className="legend-value">
            {format(s.value)}
            {unit}
          </span>
        </li>
      ))}
    </ul>
  );
}

/** Usage per minute over the samples taken while this page has been open. */
export function RateChart({ points }: { points: number[] }) {
  if (points.length < 2) return <p className="detail">Collecting… the first reading appears after a few seconds.</p>;
  const max = Math.max(...points, 1);
  const w = 300;
  const h = 70;
  // Spread the first minute of samples over the width instead of a thin strip.
  const step = w / (Math.max(points.length, 13) - 1);
  const start = w - (points.length - 1) * step;
  const xy = points.map((p, i) => `${(start + i * step).toFixed(1)},${(h - 4 - (p / max) * (h - 12)).toFixed(1)}`);
  return (
    <svg viewBox={`0 0 ${w} ${h}`} className="rate" role="img" aria-label="Usage per minute while this page has been open" preserveAspectRatio="none">
      <polygon points={`${start},${h} ${xy.join(" ")} ${w},${h}`} fill="var(--chart-1)" opacity="0.15" />
      <polyline points={xy.join(" ")} fill="none" stroke="var(--chart-1)" strokeWidth="1.8" vectorEffect="non-scaling-stroke" />
    </svg>
  );
}

export function Bar(props: { label: string; used: number; limit: number; note?: string; format?: (n: number) => string }) {
  const format = props.format ?? fmt;
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
        {format(props.used)} of {format(props.limit)}
        {props.note}
      </div>
    </div>
  );
}

/** Folds everything after the first five slices into one "others" slice. */
export function topSlices(items: { label: string; value: number }[]): Slice[] {
  const sorted = items.filter((i) => i.value > 0).sort((a, b) => b.value - a.value);
  const top = sorted.slice(0, 5).map((s, i) => ({ ...s, color: PALETTE[i] }));
  const rest = sorted.slice(5).reduce((sum, s) => sum + s.value, 0);
  if (rest > 0) top.push({ label: `${sorted.length - 5} others`, value: rest, color: PALETTE[5] });
  return top;
}

export const paletteColor = (i: number) => PALETTE[Math.min(i, PALETTE.length - 1)];

/**
 * Usage over time: one bar per hour or day, stacked by series. The data is
 * laid out by stack() in history.ts.
 */
export function TimeBars(props: {
  steps: Date[];
  series: { label: string; values: number[] }[];
  peak: number;
  label: (at: Date) => string;
  format: (n: number) => string;
  title: string;
}) {
  const { steps, series, peak } = props;
  if (peak <= 0) return <p className="detail">Nothing was used in this time.</p>;
  // Three labels fit under the bars on a phone: the first, the middle, the last.
  const marks = [0, Math.floor((steps.length - 1) / 2), steps.length - 1].filter((v, i, a) => a.indexOf(v) === i);
  return (
    <div className="timebars" role="img" aria-label={props.title}>
      <div className="timebars-scale" aria-hidden>
        <span>{props.format(peak)}</span>
        <span>{props.format(Math.round(peak / 2))}</span>
        <span>0</span>
      </div>
      <div className="timebars-plot">
        <div className="timebars-bars">
          {steps.map((at, i) => {
            const sum = series.reduce((n, s) => n + s.values[i], 0);
            const parts = series.filter((s) => s.values[i] > 0).map((s) => `${s.label}: ${props.format(s.values[i])}`);
            return (
              <div key={i} className="timebars-col" title={`${props.label(at)}: ${props.format(sum)}${parts.length > 1 ? "\n" + parts.join("\n") : parts.length === 1 ? ` (${series.find((s) => s.values[i] > 0)?.label})` : ""}`}>
                {series.map((s, n) =>
                  s.values[i] > 0 ? <span key={s.label} style={{ height: `${(s.values[i] / peak) * 100}%`, background: paletteColor(n) }} /> : null,
                )}
              </div>
            );
          })}
        </div>
        <div className="timebars-axis" aria-hidden>
          {marks.map((i) => (
            <span key={i}>{props.label(steps[i])}</span>
          ))}
        </div>
      </div>
    </div>
  );
}
