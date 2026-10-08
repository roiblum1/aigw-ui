import { useState } from "react";
import SelfTest from "./SelfTest";
import { Plus, RefreshCw, Server } from "lucide-react";
import { api, type Cluster, type ClusterInput, type ProbeResult } from "../api";
import {
  Empty,
  ErrorBanner,
  Field,
  FormModal,
  Modal,
  PageHeader,
  StatusBadge,
  formatTime,
  useAction,
  useLoad,
} from "../components";

const emptyCluster: ClusterInput = {
  name: "",
  site: "",
  namespace: "",
  gateway_name: "",
  auth_enabled: false,
  kubeconfig: "",
  gateway_url: "",
  discovery_token: "",
};

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
                  <td>{c.auth_enabled ? "Enforced" : "Off"}</td>
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

function ClusterForm(props: { cluster: Cluster | null; onClose: () => void; onSaved: () => Promise<void> }) {
  const { cluster } = props;
  const [form, setForm] = useState<ClusterInput>(
    cluster ? { ...cluster, kubeconfig: "", discovery_token: "" } : emptyCluster,
  );
  const set = (patch: Partial<ClusterInput>) => setForm((f) => ({ ...f, ...patch }));

  const save = async () => {
    const body: ClusterInput = {
      name: form.name,
      site: form.site,
      namespace: form.namespace,
      gateway_name: form.gateway_name,
      auth_enabled: form.auth_enabled,
      kubeconfig: form.kubeconfig,
      gateway_url: form.gateway_url,
      discovery_token: form.discovery_token,
    };
    if (cluster) await api.updateCluster(cluster.id, body);
    else await api.createCluster(body);
    await props.onSaved();
  };

  return (
    <FormModal
      title={cluster ? `Edit ${cluster.name}` : "Add cluster"}
      submitLabel={cluster ? "Save" : "Add cluster"}
      onClose={props.onClose}
      onSubmit={save}
    >
      <div className="grid-2">
        <Field label="Name">
          <input
            required
            value={form.name}
            placeholder="ocp4-prod-llm-site1-a"
            onChange={(e) => set({ name: e.target.value })}
          />
        </Field>
        <Field label="Site">
          <input value={form.site} placeholder="site1" onChange={(e) => set({ site: e.target.value })} />
        </Field>
        <Field label="Gateway namespace" hint="Use the same namespace on every cluster so quotas add up across sites.">
          <input required value={form.namespace} onChange={(e) => set({ namespace: e.target.value })} />
        </Field>
        <Field label="Gateway name">
          <input required value={form.gateway_name} onChange={(e) => set({ gateway_name: e.target.value })} />
        </Field>
      </div>
      <Field
        label="Kubeconfig"
        hint={cluster ? "Leave empty to keep the stored kubeconfig." : "Stored encrypted. It is never shown again."}
      >
        <textarea
          rows={6}
          className="mono"
          required={!cluster}
          value={form.kubeconfig}
          spellCheck={false}
          onChange={(e) => set({ kubeconfig: e.target.value })}
        />
      </Field>
      <h3>Model discovery</h3>
      <div className="grid-2">
        <Field
          label="Gateway URL"
          hint="The gateway's address, without a path. Models are then listed from its /v1/models. Leave empty to read routes in the gateway namespace only."
        >
          <input
            value={form.gateway_url}
            placeholder="http://192.168.1.9"
            onChange={(e) => set({ gateway_url: e.target.value })}
          />
        </Field>
        <Field
          label="API key for /v1/models"
          hint={
            cluster?.has_discovery_token
              ? "A key is stored. Leave empty to keep it."
              : "Only needed when the gateway requires a key."
          }
        >
          <input
            type="password"
            autoComplete="off"
            disabled={!form.gateway_url}
            value={form.discovery_token}
            onChange={(e) => set({ discovery_token: e.target.value })}
          />
        </Field>
      </div>
      <label className="check">
        <input type="checkbox" checked={form.auth_enabled} onChange={(e) => set({ auth_enabled: e.target.checked })} />
        <span>
          Enforce API keys on this gateway
          <small>
            Requests without a key issued here are rejected, for every route on the gateway. Per-tenant quotas need
            this.
          </small>
        </span>
      </label>
    </FormModal>
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
