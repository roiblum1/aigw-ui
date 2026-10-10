import { useState } from "react";
import { Activity } from "lucide-react";
import { api, type UsageQuota } from "../api";
import {
  Empty,
  ErrorBanner,
  PageHeader,
  formatAmount,
  formatNumber,
  formatTime,
  useAction,
  useLoad,
  usePolling,
  windowLabel,
} from "../components";

export const REFRESH_MS = 5000;

function resetsIn(iso: string, now: number): string {
  const s = Math.max(0, Math.round((new Date(iso).getTime() - now) / 1000));
  if (s >= 3600) return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`;
  if (s >= 60) return `${Math.floor(s / 60)}m ${s % 60}s`;
  return `${s}s`;
}

/** The time of day, with the date when it is not today. */
function untilTime(iso: string): string {
  const d = new Date(iso);
  const time = d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
  return d.toDateString() === new Date().toDateString() ? time : `${d.toLocaleDateString()} ${time}`;
}

export function UsageMeter({ q }: { q: UsageQuota }) {
  const ratio = q.limit > 0 ? q.used / q.limit : 0;
  const percent = Math.round(ratio * 100);
  const level = ratio >= 1 ? "full" : ratio >= 0.8 ? "high" : "ok";
  return (
    <div className="meter-cell">
      <div
        className={`meter ${level}`}
        role="progressbar"
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={Math.min(percent, 100)}
        aria-label={`${q.tenant_slug} on ${q.model_name}`}
      >
        <span style={{ width: `${Math.min(percent, 100)}%` }} />
      </div>
      <div className="detail">
        {formatNumber(q.used, q.unit)} of {formatAmount(q.limit, q.unit)} ({percent}%)
      </div>
      {q.dry_run && (
        <span className="tag warn" title="The model's prices are being tried out. The tenant is counted and not refused.">
          dry-run
        </span>
      )}
      {q.best_effort_until && (
        <span
          className="tag warn"
          title="The budget is spent. Requests are answered as best-effort: queued behind all others and dropped first when a site is full."
        >
          best-effort until {untilTime(q.best_effort_until)}
        </span>
      )}
      {q.overage_used > 0 && (
        <div className="detail">
          {formatAmount(q.overage_used, q.unit)} used as best-effort{q.unit === "credits" && ", not charged"}
        </div>
      )}
    </div>
  );
}

/** Asks first: a reset cannot be undone and applies on every site at once. */
export function confirmReset(q: UsageQuota): boolean {
  return confirm(
    `Reset the usage of ${q.tenant_slug} on ${q.model_name}?\n\n` +
      `${formatAmount(q.used, q.unit)} used in this window go back to 0 on every cluster. ` +
      `The limit stays at ${formatAmount(q.limit, q.unit)}.`,
  );
}

function Counters({ q }: { q: UsageQuota }) {
  if (q.counters.filter((c) => !c.overage).length < 2) return null;
  return (
    <div className="detail">
      {q.counters.map((c) => (
        <div key={c.backend} title={`Clusters: ${c.clusters.join(", ")}`}>
          <span className="mono">{c.backend}</span>: {formatNumber(c.used, q.unit)}
          {c.overage && " (best-effort)"}
        </div>
      ))}
    </div>
  );
}

export default function Usage() {
  const { data, error, reload } = useLoad(() => api.usage());
  usePolling(reload, REFRESH_MS);
  const history = useLoad(() => api.overage());
  usePolling(history.reload, 6 * REFRESH_MS);
  const action = useAction();
  const [filter, setFilter] = useState("");
  const reset = (q: UsageQuota) => {
    if (!confirmReset(q)) return;
    action.run(async () => {
      await api.resetUsage(q.tenant_id, q.model_id);
      await reload();
    });
  };
  const now = data ? new Date(data.at).getTime() : Date.now();

  const rows = (data?.quotas ?? []).filter((q) => {
    const f = filter.trim().toLowerCase();
    return !f || q.tenant_slug.includes(f) || q.model_name.toLowerCase().includes(f);
  });
  // The counter of the best-effort route is a second one by design.
  const split = (data?.quotas ?? []).some((q) => q.counters.filter((c) => !c.overage).length > 1);

  return (
    <>
      <PageHeader
        title="Usage"
        subtitle="What each tenant has used in the current window, read from the rate limit counters in Redis. In dollars for a model with prices, in tokens for any other."
      >
        {data?.enabled && (
          <>
            <input
              type="search"
              placeholder="Filter by tenant or model"
              aria-label="Filter by tenant or model"
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
            />
            <span className="detail live" title={`Refreshes every ${REFRESH_MS / 1000} seconds`}>
              <i aria-hidden /> Live
            </span>
          </>
        )}
      </PageHeader>
      <ErrorBanner message={error || action.error} />

      {data && !data.enabled && (
        <Empty icon={Activity}>
          Usage monitoring is off. Set <code>redis.url</code> in the chart (or <code>REDIS_URL</code>) to the Redis
          that the gateways count quotas in.
        </Empty>
      )}
      {data?.enabled && data.hint && <div className="banner warn">{data.hint}</div>}
      {data?.enabled && split && (
        <div className="banner warn">
          Some quotas have more than one counter because the model's backends are named differently between
          clusters. Each counter is held to the limit on its own, so the tenant can use more than one budget. Give
          the backends the same namespace and name on every cluster to get a single budget.
        </div>
      )}
      {data?.enabled && data.quotas.length === 0 && (
        <Empty icon={Activity}>No tenant has a quota yet. Set one on a tenant to see its usage here.</Empty>
      )}
      {data?.enabled && data.quotas.length > 0 && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Tenant</th>
                <th>Model</th>
                <th>Used</th>
                <th>Window</th>
                <th>Resets in</th>
                {data.can_reset && <th />}
              </tr>
            </thead>
            <tbody>
              {rows.map((q) => (
                <tr key={q.tenant_id + q.model_id}>
                  <td className="strong">{q.tenant_slug}</td>
                  <td>
                    {q.model_name}
                    {q.shadow && " "}
                    {q.shadow && (
                      <span className="tag warn" title="Counted, but requests are not rejected by this quota.">
                        dry run
                      </span>
                    )}
                  </td>
                  <td>
                    <UsageMeter q={q} />
                    <Counters q={q} />
                  </td>
                  <td>per {windowLabel[q.window]}</td>
                  <td className="mono">{resetsIn(q.resets_at, now)}</td>
                  {data.can_reset && (
                    <td className="row-actions">
                      <button disabled={action.busy || q.used + q.overage_used === 0} onClick={() => reset(q)}>
                        Reset usage
                      </button>
                    </td>
                  )}
                </tr>
              ))}
              {rows.length === 0 && (
                <tr>
                  <td colSpan={6} className="detail">
                    Nothing matches "{filter}".
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
      )}
      {data?.enabled && (history.data?.length ?? 0) > 0 && (
        <>
          <h2>Best-effort periods</h2>
          <p className="hint">
            When a tenant's budget on a model was nearly spent and its requests were served as best-effort, for
            the models set to do that. A period ends with the quota's window.
          </p>
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Tenant</th>
                  <th>Model</th>
                  <th>From</th>
                  <th>Until</th>
                </tr>
              </thead>
              <tbody>
                {history.data!.map((o) => (
                  <tr key={o.tenant_id + o.model_id + o.since}>
                    <td className="strong">{o.tenant_slug}</td>
                    <td>{o.model_name}</td>
                    <td>{formatTime(o.since)}</td>
                    <td>
                      {formatTime(o.until)}
                      {o.active && " "}
                      {o.active && <span className="tag warn">now</span>}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}
    </>
  );
}
