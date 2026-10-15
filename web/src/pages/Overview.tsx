import { Boxes, Gauge, KeyRound, Server, Users, type LucideIcon } from "lucide-react";
import { api, type Budgets, type Unit } from "../api";
import { short } from "../charts";
import { ErrorBanner, PageHeader, StatusBadge, formatDollars, formatTime, useLoad } from "../components";
import Dashboard from "./Dashboard";
import History from "./History";

function Stat(props: { label: string; value: number; icon: LucideIcon; note?: string; alert?: boolean }) {
  const Icon = props.icon;
  return (
    <div className="flex items-center gap-3.5 rounded-xl border border-border bg-card p-4 shadow-(--shadow)">
      <span className="grid size-10 flex-none place-items-center rounded-lg bg-primary-soft text-primary">
        <Icon className="size-5" />
      </span>
      <div className="min-w-0">
        <div className="text-2xl font-semibold leading-tight tabular-nums">{props.value}</div>
        <div className="text-[13px] text-muted-foreground">{props.label}</div>
        {props.note && (
          <div className={`text-xs font-medium ${props.alert ? "text-danger" : "text-success"}`}>{props.note}</div>
        )}
      </div>
    </div>
  );
}

/**
 * What each model can be given a day from what its GPUs serve, and how much of
 * that its budgets take: the answer to "how much can I still give".
 */
function BudgetsCard({ b }: { b: Budgets }) {
  if (b.models.length === 0) return null;
  // Dollars to the cent, tokens written short: 3.6B tokens.
  const formatAmount = (n: number, unit: Unit) => (unit === "credits" ? formatDollars(n) : `${short(n)} tokens`);
  const left = (n: number, unit: Unit) =>
    n >= 0 ? formatAmount(n, unit) : <span className="warn-text">{formatAmount(-n, unit)} past</span>;
  return (
    <section className="card">
      <h2>Budgets</h2>
      <p className="hint">
        What each model's ready GPUs serve a day at its expected busy share, the ceiling its overcommit limit allows, and
        what the tenants' budgets take. Hourly and per-minute budgets count at the most they allow in a day. See{" "}
        <a href="#capacity">Capacity</a>.
      </p>
      {b.fleet.value_day > 0 && (
        <p>
          The fleet serves <span className="strong">{formatDollars(b.fleet.value_day)}</span> a day at the models' prices;{" "}
          {formatDollars(b.fleet.can_be_given_day)} can be given, up to {formatDollars(b.fleet.ceiling_day)} with
          overcommit, {formatDollars(b.fleet.given_day)} is given, and{" "}
          {b.fleet.left_day >= 0 ? (
            <span className="strong">{formatDollars(b.fleet.left_day)} is left</span>
          ) : (
            <span className="warn-text">the budgets are {formatDollars(-b.fleet.left_day)} past the ceiling</span>
          )}
          .
        </p>
      )}
      <p className="detail">
        Yesterday's spend: <span className="strong">{formatDollars(b.market.yesterday_spend)}</span>, what the same
        traffic cost at the models' prices (OpenRouter's, where they are the market's).
        {b.market.cost_day != null &&
          ` The measured GPUs cost ${formatDollars(b.market.cost_day)} a day${b.market.cost_partial ? ", some left out for want of a GPU cost" : ""}.`}
      </p>
      <div className="table-wrap">
        <table className="plain">
          <thead>
            <tr>
              <th>Model</th>
              <th className="text-right">Can be given a day</th>
              <th className="text-right">Ceiling</th>
              <th className="text-right">Given</th>
              <th className="text-right">Left</th>
              <th className="text-right wrap" title="The busiest hour of the last 30 days, against what the model serves in an hour">
                Busiest hour
              </th>
            </tr>
          </thead>
          <tbody>
            {b.models.map((m) => (
              <tr key={m.model_id}>
                <td className="strong">{m.model_name}</td>
                {m.measured ? (
                  <>
                    <td className="text-right tabular-nums">{formatAmount(m.capacity_day, m.unit)}</td>
                    <td className="text-right tabular-nums">
                      {formatAmount(m.ceiling_day, m.unit)} <span className="detail">({m.overcommit_limit}×)</span>
                    </td>
                    <td className="text-right tabular-nums">{formatAmount(m.given_day, m.unit)}</td>
                    <td className="text-right tabular-nums">{left(m.left_day, m.unit)}</td>
                    <td className="text-right tabular-nums">
                      {m.peak_hour_share == null ? "–" : `${Math.round(m.peak_hour_share * 100)}%`}
                      {m.saturated && <span className="tag warn">saturated</span>}
                    </td>
                  </>
                ) : (
                  <td colSpan={5} className="detail">
                    Not measured: given {formatAmount(m.given_day, m.unit)} a day against an unknown capacity
                  </td>
                )}
              </tr>
            ))}
            {b.credits.capacity_day > 0 && (
              <tr>
                <td className="strong">All in dollars</td>
                <td className="text-right tabular-nums strong">{formatAmount(b.credits.capacity_day, "credits")}</td>
                <td className="text-right tabular-nums">{formatAmount(b.credits.ceiling_day, "credits")}</td>
                <td className="text-right tabular-nums">{formatAmount(b.credits.given_day, "credits")}</td>
                <td className="text-right tabular-nums strong">{left(b.credits.left_day, "credits")}</td>
                <td />
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </section>
  );
}

export default function Overview() {
  const { data, error } = useLoad(async () => ({ overview: await api.overview(), clusters: await api.clusters() }));
  const budgets = useLoad(api.budgets);
  const o = data?.overview;
  return (
    <>
      <PageHeader title="Overview" subtitle="State of the gateway configuration across all sites." />
      <ErrorBanner message={error} />
      {o && (
        <div className="mb-4 grid grid-cols-2 gap-3 lg:grid-cols-5">
          <Stat
            label="Clusters"
            value={o.clusters}
            icon={Server}
            alert={o.clusters_error > 0}
            note={o.clusters_error > 0 ? `${o.clusters_error} failing` : undefined}
          />
          <Stat label="Models" value={o.models} icon={Boxes} />
          <Stat label="Tenants" value={o.tenants} icon={Users} />
          <Stat label="Active keys" value={o.active_keys} icon={KeyRound} />
          <Stat label="Quotas" value={o.quotas} icon={Gauge} />
        </div>
      )}
      {budgets.data && <BudgetsCard b={budgets.data} />}
      <Dashboard />
      <History />
      {data && data.clusters.length > 0 && (
        <section className="card">
          <h2>Cluster sync</h2>
          <table className="plain">
            <thead>
              <tr>
                <th>Cluster</th>
                <th>Site</th>
                <th>Status</th>
                <th>Detail</th>
                <th>Last sync</th>
              </tr>
            </thead>
            <tbody>
              {data.clusters.map((c) => (
                <tr key={c.id}>
                  <td className="strong">{c.name}</td>
                  <td>{c.site || "—"}</td>
                  <td>
                    <StatusBadge status={c.sync_status} />
                  </td>
                  <td className="detail">{c.sync_message}</td>
                  <td className="whitespace-nowrap">{formatTime(c.synced_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </section>
      )}
      {data && data.clusters.length === 0 && (
        <section className="card">
          <h2>Get started</h2>
          <ol className="steps">
            <li>
              Add your LLM clusters under <a href="#clusters">Clusters</a>.
            </li>
            <li>
              Register the models they serve under <a href="#models">Models</a>.
            </li>
            <li>
              Create a tenant, issue a key and set its quotas under <a href="#tenants">Tenants</a>.
            </li>
          </ol>
        </section>
      )}
    </>
  );
}
