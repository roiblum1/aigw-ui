import { useState } from "react";
import ClusterForm from "./ClusterForm";
import SelfTest from "./SelfTest";
import { Plus, RefreshCw, Server } from "lucide-react";
import { api, type Cluster, type ProbeResult } from "../api";
import {
  Empty,
  ErrorBanner,
  Modal,
  PageHeader,
  StatusBadge,
  formatTime,
  useAction,
  useLoad,
} from "../components";

export default function Clusters() {
  const { data: clusters, error, reload } = useLoad(api.clusters);
  const action = useAction();
  const [editing, setEditing] = useState<Cluster | "new" | null>(null);
  const [manifests, setManifests] = useState<{ name: string; yaml: string } | null>(null);
  const [probe, setProbe] = useState<{ name: string; result: ProbeResult } | null>(null);
  const [selfTest, setSelfTest] = useState<Cluster | null>(null);

  const sync = (c: Cluster) =>
    action.run(async () => {
      const res = await api.syncCluster(c.id);
      await reload();
      if (!res.ok) throw new Error(`${c.name}: ${res.error}`);
    });

  const remove = (c: Cluster) => {
    if (!confirm(`Remove ${c.name}? Objects already applied to the cluster stay there.`)) return;
    action.run(async () => {
      await api.deleteCluster(c.id);
      await reload();
    });
  };

  return (
    <>
      <PageHeader title="Clusters" subtitle="The LLM clusters this hub applies gateway configuration to.">
        <button
          disabled={action.busy || !clusters?.length}
          onClick={() => action.run(async () => void (await api.syncAll(), await reload()))}
        >
          <RefreshCw />
          Sync all
        </button>
        <button className="primary" onClick={() => setEditing("new")}>
          <Plus />
          Add cluster
        </button>
      </PageHeader>
      <ErrorBanner message={error || action.error} />

      {clusters && clusters.length === 0 && (
        <Empty icon={Server}>No clusters yet. Add one with a kubeconfig that can manage the gateway namespace.</Empty>
      )}
      {clusters && clusters.length > 0 && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Name</th>
                <th>Site</th>
                <th>Gateway</th>
                <th>API keys</th>
                <th>Status</th>
                <th>Last sync</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {clusters.map((c) => (
                <tr key={c.id}>
                  <td className="strong">{c.name}</td>
                  <td>{c.site || "—"}</td>
                  <td className="mono">
                    {c.namespace}/{c.gateway_name}
                  </td>
                  <td>
                    {c.auth_enabled ? "Enforced" : "Off"}
                    {c.auth_enabled && c.client_listener && <div className="detail">on listener {c.client_listener}</div>}
                    {c.fleet_enabled && (
                      <span
                        className={c.fleet_outdated ? "tag warn" : "tag manual"}
                        title={
                          c.fleet_outdated
                            ? "This cluster's entry routes are older than the fleet's. Until it is synced it can send a conversation to another site than the other clusters do."
                            : "This cluster shares traffic with the other fleet clusters, and its entry routes are the fleet's current ones."
                        }
                      >
                        {c.fleet_outdated ? "fleet · outdated" : "fleet"}
                        {c.fleet_revision && ` · ${c.fleet_revision}`}
                      </span>
                    )}
                  </td>
                  <td>
                    <StatusBadge status={c.sync_status} />
                    {c.sync_message && (
                      <div className="detail clamp" title={c.sync_message}>
                        {c.sync_message}
                      </div>
                    )}
                  </td>
                  <td>
                    {formatTime(c.synced_at)}
                    {c.discovered_at && (
                      <div className="detail clamp" title={`${c.discovery_message} (${formatTime(c.discovered_at)})`}>
                        Discovery: {c.discovery_message}
                      </div>
                    )}
                  </td>
                  <td className="row-actions">
                    <button
                      disabled={action.busy}
                      onClick={() =>
                        action.run(async () => setProbe({ name: c.name, result: await api.probeCluster(c.id) }))
                      }
                    >
                      Test
                    </button>
                    <button
                      disabled={!c.gateway_url}
                      title={c.gateway_url ? "Check keys, counters and quotas with real requests" : "Needs a gateway URL"}
                      onClick={() => setSelfTest(c)}
                    >
                      Self-test
                    </button>
                    <button disabled={action.busy} onClick={() => sync(c)}>
                      Sync
                    </button>
                    <button
                      disabled={action.busy}
                      onClick={() =>
                        action.run(async () => setManifests({ name: c.name, yaml: (await api.manifests(c.id)).yaml }))
                      }
                    >
                      Manifests
                    </button>
                    <button onClick={() => setEditing(c)}>Edit</button>
                    <button className="danger" disabled={action.busy} onClick={() => remove(c)}>
                      Remove
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {editing && (
        <ClusterForm cluster={editing === "new" ? null : editing} onClose={() => setEditing(null)} onSaved={reload} />
      )}
      {manifests && (
        <Modal title={`Manifests for ${manifests.name}`} onClose={() => setManifests(null)} wide>
          <p className="hint">What the next sync applies. API key values are masked here.</p>
          <pre className="code">{manifests.yaml || "# Nothing to apply: no models are exposed on this cluster."}</pre>
        </Modal>
      )}
      {selfTest && <SelfTest cluster={selfTest} onClose={() => setSelfTest(null)} />}
      {probe && <ProbeModal name={probe.name} result={probe.result} onClose={() => setProbe(null)} />}
    </>
  );
}

function ProbeModal(props: { name: string; result: ProbeResult; onClose: () => void }) {
  const { result } = props;
  const check = (ok: boolean) => <span className={ok ? "badge synced" : "badge error"}>{ok ? "Found" : "Missing"}</span>;
  return (
    <Modal title={`Connection test: ${props.name}`} onClose={props.onClose}>
      {!result.reachable && <ErrorBanner message={result.error ?? "Cluster is not reachable"} />}
      {result.probe && (
        <table className="plain">
          <tbody>
            <tr>
              <td>Kubernetes version</td>
              <td className="mono">{result.probe.kubernetes_version}</td>
            </tr>
            <tr>
              <td>Gateway</td>
              <td>{check(result.probe.gateway_found)}</td>
            </tr>
            {Object.entries(result.probe.kinds).map(([kind, ok]) => (
              <tr key={kind}>
                <td>{kind} CRD</td>
                <td>{check(ok)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </Modal>
  );
}
