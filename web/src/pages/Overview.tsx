import { Boxes, Gauge, KeyRound, Server, Users, type LucideIcon } from "lucide-react";
import { api } from "../api";
import { ErrorBanner, PageHeader, StatusBadge, formatTime, useLoad } from "../components";
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

export default function Overview() {
  const { data, error } = useLoad(async () => ({ overview: await api.overview(), clusters: await api.clusters() }));
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
