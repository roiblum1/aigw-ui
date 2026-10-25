import { useState } from "react";
import { ErrorBanner, useLoad, usePolling } from "./components";
import { formatDollars } from "./money";
import { peopleRows, type KeyPrice, type KeyUses } from "./people";
import { Seg } from "./pages/History";
import { percent, shortTokens } from "./workload";

const periods = [
  { value: "1", label: "Day" },
  { value: "7", label: "Week" },
  { value: "30", label: "30 days" },
] as const;

type Period = (typeof periods)[number]["value"];

/**
 * What each key of a tenant used, the biggest first: on the tenants' page
 * for the members of its group, on the Tenants page for an admin. The
 * gateways count each key by the client ID its tokens carry; the hub reads
 * those counters every minute.
 */
export function KeyUsageCard(props: {
  title: string;
  hint: string;
  load: (days: number) => Promise<KeyUses>;
  cacheKey: string;
  prices: KeyPrice[];
}) {
  const [period, setPeriod] = useState<Period>("7");
  const days = Number(period);
  const uses = useLoad(() => props.load(days), `${props.cacheKey}/${days}`);
  usePolling(uses.reload, 60_000);
  const rows = uses.data ? peopleRows(uses.data.keys, props.prices) : [];
  const priced = rows.some((r) => r.cost !== null);
  return (
    <section className="card">
      <div className="dash-head">
        <div>
          <h2>{props.title}</h2>
          <p className="hint">{props.hint}</p>
        </div>
        <Seg label="Time" value={period} onChange={setPeriod} options={periods} />
      </div>
      <ErrorBanner message={uses.error} />
      {uses.data && rows.length === 0 && <p className="hint">No key, and nothing used.</p>}
      {rows.length > 0 && (
        <div className="overflow-x-auto">
          <table className="plain">
            <thead>
              <tr>
                <th>Who</th>
                <th className="text-right">Requests</th>
                <th className="text-right">Input tokens</th>
                <th className="text-right">From cache</th>
                <th className="text-right">Output tokens</th>
                {priced && <th className="text-right">Cost</th>}
                <th className="text-right">Share</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((r) => (
                <tr key={r.key.key_id}>
                  <td>
                    <span className="strong">{r.who}</span>
                    {r.key.me && <span className="tag ml-2">you</span>}
                    {!r.key.owner && <span className="tag ml-2">admin's key</span>}
                    {r.key.revoked_at && <span className="tag warn ml-2">revoked</span>}
                  </td>
                  <td className="text-right tabular-nums">{r.key.totals.requests.toLocaleString("en-US")}</td>
                  <td className="text-right tabular-nums">{shortTokens(r.key.totals.input_tokens)}</td>
                  <td className="text-right tabular-nums">{shortTokens(r.key.totals.cached_tokens)}</td>
                  <td className="text-right tabular-nums">{shortTokens(r.key.totals.output_tokens)}</td>
                  {priced && <td className="text-right tabular-nums">{r.cost === null ? "–" : formatDollars(r.cost)}</td>}
                  <td className="text-right tabular-nums">{percent(r.share)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}
