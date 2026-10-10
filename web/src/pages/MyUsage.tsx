import { useCallback, useEffect, useMemo, useState, type FormEvent } from "react";
import { LogOut } from "lucide-react";
import { api, Unauthorized, type MyBudget, type MyUsage as Report, type Unit } from "../api";
import BrandMark from "../BrandMark";
import { Bar } from "../charts";
import { ErrorBanner, formatAmount, formatNumber, formatPrice, windowLabel } from "../components";
import { HistoryView, Seg } from "./History";

// The key is kept for this tab only: closing the tab signs out.
const KEY = "aigw-ui-tenant-key";

function storedKey(): string {
  try {
    return sessionStorage.getItem(KEY) ?? "";
  } catch {
    return "";
  }
}

function storeKey(key: string) {
  try {
    if (key) sessionStorage.setItem(KEY, key);
    else sessionStorage.removeItem(KEY);
  } catch {
    // Without storage the key lasts for this page load only.
  }
}

const units: Unit[] = ["credits", "tokens"];
const unitLabel: Record<Unit, string> = { credits: "Dollars", tokens: "Tokens" };

function resets(iso: string): string {
  const at = new Date(iso);
  const minutes = Math.max(0, Math.round((at.getTime() - Date.now()) / 60000));
  const left = minutes >= 60 ? `${Math.floor(minutes / 60)} h ${minutes % 60} min` : `${minutes} min`;
  return `${at.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}, in ${left}`;
}

function Budget({ b }: { b: MyBudget }) {
  const left = Math.max(0, b.limit - b.used);
  return (
    <div className="panel">
      <h3>{b.model_name}</h3>
      <Bar label={`Budget per ${windowLabel[b.window]}`} used={b.used} limit={b.limit} format={(n) => formatNumber(n, b.unit)} />
      <p className="detail">
        {formatAmount(left, b.unit)} left. A new {windowLabel[b.window]} starts at {resets(b.resets_at)}.
      </p>
      {!b.enforced && (
        <p className="detail">
          <span className="tag warn" title="Your usage is counted and you are not refused when the budget is spent.">
            counted only
          </span>{" "}
          This budget is not enforced yet.
        </p>
      )}
      {b.enforced && b.used >= b.limit && !b.best_effort_until && (
        <p className="detail">
          <span className="tag warn">spent</span> Your budget is spent. Requests can be refused until the new{" "}
          {windowLabel[b.window]}.
        </p>
      )}
      {b.best_effort_until && !b.best_effort_capped && (
        <p className="detail">
          <span className="tag warn">best-effort</span> Your budget is spent. Until the new {windowLabel[b.window]} your
          requests are answered at the lowest priority: behind all others, and refused first when the model is busy.
        </p>
      )}
      {b.best_effort_capped && (
        <p className="detail">
          <span className="tag warn">refused</span> Your budget and the best-effort allowance are spent. Requests are
          refused until the new {windowLabel[b.window]}.
        </p>
      )}
      {(b.best_effort_used > 0 || b.best_effort_limit) && (
        <p className="detail">
          Used as best-effort: {formatAmount(b.best_effort_used, b.unit)}
          {b.best_effort_limit ? ` of ${formatAmount(b.best_effort_limit, b.unit)}` : ""}
          {b.unit === "credits" ? ". It is not charged to your budget." : "."}
        </p>
      )}
    </div>
  );
}

function SignIn({ onKey }: { onKey: (key: string, report: Report) => void }) {
  const [key, setKey] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    try {
      onKey(key.trim(), await api.myUsage(key.trim()));
    } catch (err) {
      setError(err instanceof Unauthorized ? "That is not an API key of an active tenant." : err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="grid min-h-screen place-items-center bg-sidebar p-4">
      <form onSubmit={submit} className="w-full max-w-[400px] rounded-2xl border border-border bg-card p-7 shadow-2xl shadow-black/30">
        <div className="mb-6 flex items-center gap-3">
          <BrandMark className="size-10" />
          <div>
            <h1 className="text-lg">Your usage</h1>
            <p className="text-[13px] text-muted-foreground">Budgets, usage and prices of your team</p>
          </div>
        </div>
        <ErrorBanner message={error} />
        <label className="field">
          <span>API key</span>
          <input type="password" required autoFocus placeholder="sk-…" autoComplete="off" value={key} onChange={(e) => setKey(e.target.value)} />
          <small>
            Any key of your team. It is only used to find your team, and stays in this tab until you close it. You see
            your own team and no other.
          </small>
        </label>
        <button type="submit" className="primary mt-1 w-full" disabled={busy}>
          {busy ? "Signing in…" : "Show my usage"}
        </button>
      </form>
    </div>
  );
}

/** A tenant's own page: it signs in with one of its API keys. */
export default function MyUsage() {
  const [key, setKey] = useState(storedKey);
  const [report, setReport] = useState<Report | null>(null);
  const [error, setError] = useState("");
  const [range, setRange] = useState<"hours" | "days">("days");
  const [pickedUnit, setPickedUnit] = useState<Unit | null>(null);

  const signOut = useCallback(() => {
    storeKey("");
    setKey("");
    setReport(null);
  }, []);

  useEffect(() => {
    if (!key) return;
    let stop = false;
    const load = async () => {
      if (document.hidden) return;
      try {
        const r = await api.myUsage(key);
        if (!stop) {
          setReport(r);
          setError("");
        }
      } catch (err) {
        if (err instanceof Unauthorized) signOut();
        else if (!stop) setError(err instanceof Error ? err.message : String(err));
      }
    };
    load();
    const timer = setInterval(load, 15_000);
    return () => {
      stop = true;
      clearInterval(timer);
    };
  }, [key, signOut]);

  const points = report ? (range === "days" ? report.days : report.hours) : [];
  const present = useMemo(
    () => units.filter((u) => report?.budgets.some((b) => b.unit === u) || points.some((p) => p.unit === u)),
    [report, points],
  );
  const unit = pickedUnit && present.includes(pickedUnit) ? pickedUnit : (present[0] ?? "credits");
  const now = useMemo(() => new Date(), [report]);

  if (!key)
    return (
      <SignIn
        onKey={(k, r) => {
          storeKey(k);
          setKey(k);
          setReport(r);
        }}
      />
    );

  return (
    <div className="min-h-screen">
      <header className="flex items-center gap-3 bg-sidebar px-4 py-3 md:px-9">
        <BrandMark />
        <div className="min-w-0 flex-1 leading-tight">
          <div className="truncate text-sm font-semibold text-white">Your usage</div>
          <div className="text-xs text-sidebar-foreground">
            {report ? (report.display_name && report.display_name !== report.tenant ? `${report.display_name} · ${report.tenant}` : `Team ${report.tenant}`) : "Loading…"}
          </div>
        </div>
        <button
          className="flex h-9 items-center gap-2 rounded-lg border-0 bg-transparent px-3 text-[13.5px] font-medium text-sidebar-foreground shadow-none hover:bg-white/10 hover:text-white"
          onClick={signOut}
        >
          <LogOut className="size-4" />
          <span className="hidden sm:inline">Sign out</span>
        </button>
      </header>
      <main className="mx-auto w-full max-w-[1100px] px-4 pb-12 pt-6 md:px-9 md:pt-8">
        <ErrorBanner message={error} />
        {report && (
          <>
            <section className="card">
              <h2>Your budgets</h2>
              {!report.usage_enabled ? (
                <p className="hint">Usage is not being counted on this installation.</p>
              ) : report.budgets.length === 0 ? (
                <p className="hint">
                  Your team has no budget of its own on any model. Where a model has a shared pool, your requests draw
                  from that.
                </p>
              ) : (
                <>
                  <p className="hint">What you used in the running period of each budget. It is read from the gateways every few seconds.</p>
                  <div className="dash-grid">
                    {report.budgets.map((b) => (
                      <Budget key={b.model_name} b={b} />
                    ))}
                  </div>
                </>
              )}
            </section>

            <section className="card">
              <div className="dash-head">
                <div>
                  <h2>Usage over time</h2>
                  <p className="hint">Per model. Days and hours are in UTC.</p>
                </div>
                {present.length > 1 && (
                  <Seg label="Unit" value={unit} onChange={setPickedUnit} options={present.map((u) => ({ value: u, label: unitLabel[u] }))} />
                )}
                <Seg
                  label="Time"
                  value={range}
                  onChange={setRange}
                  options={[
                    { value: "hours", label: "2 days" },
                    { value: "days", label: "30 days" },
                  ]}
                />
              </div>
              {points.length === 0 ? (
                <p className="hint">Nothing is recorded for this time.</p>
              ) : (
                <HistoryView
                  points={points}
                  from={range === "days" ? report.days_from : report.hours_from}
                  step={range === "days" ? "day" : "hour"}
                  unit={unit}
                  by="model"
                  now={now}
                />
              )}
            </section>

            {report.prices.length > 0 && (
              <section className="card">
                <h2>Prices</h2>
                <p className="hint">
                  What a request costs is worked out from its tokens. The part of a prompt the model still has in its
                  cache from an earlier request costs the lower cached price. Prices cover the cost of running the
                  models and nothing more.
                </p>
                <table className="plain">
                  <thead>
                    <tr>
                      <th>Model</th>
                      <th className="text-right">Input, per million tokens</th>
                      <th className="text-right">Cached input</th>
                      <th className="text-right">Output</th>
                    </tr>
                  </thead>
                  <tbody>
                    {report.prices.map((p) => (
                      <tr key={p.model_name}>
                        <td className="strong">{p.model_name}</td>
                        <td className="text-right tabular-nums">{formatPrice(p.price_input)}</td>
                        <td className="text-right tabular-nums">{formatPrice(p.price_cached)}</td>
                        <td className="text-right tabular-nums">{formatPrice(p.price_output)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </section>
            )}
          </>
        )}
      </main>
    </div>
  );
}
