import { useState } from "react";
import { api, type Cluster, type ClusterInput } from "../api";
import { Field, FormModal } from "../components";

const emptyCluster: ClusterInput = {
  name: "",
  site: "",
  namespace: "",
  gateway_name: "",
  auth_enabled: false,
  kubeconfig: "",
  gateway_url: "",
  discovery_token: "",
  fleet_enabled: false,
  client_listener: "",
  peer_host: "",
  peer_port: 8443,
};

export default function ClusterForm(props: { cluster: Cluster | null; onClose: () => void; onSaved: () => Promise<void> }) {
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
      fleet_enabled: form.fleet_enabled,
      client_listener: form.client_listener,
      peer_host: form.peer_host,
      peer_port: form.peer_port,
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
      <Field
        label="Client listener"
        hint="Optional. The Gateway listener clients come in on, for example https. The key check then applies to it alone, and not to a listener other sites forward to."
      >
        <input
          placeholder="whole Gateway"
          value={form.client_listener}
          onChange={(e) => set({ client_listener: e.target.value })}
        />
      </Field>
      <div className="grid-2">
        <Field label="Peer host" hint="Optional. The name the other sites reach this gateway under, for example llm.site1-a.example.com.">
          <input value={form.peer_host} onChange={(e) => set({ peer_host: e.target.value })} />
        </Field>
        <Field label="Peer port">
          <input
            type="number"
            min={1}
            max={65535}
            value={form.peer_port}
            onChange={(e) => set({ peer_port: Number(e.target.value) })}
          />
        </Field>
      </div>
      <label className="check">
        <input
          type="checkbox"
          checked={form.fleet_enabled}
          disabled={(!form.auth_enabled || !form.client_listener || !form.peer_host) && !form.fleet_enabled}
          onChange={(e) => set({ fleet_enabled: e.target.checked })}
        />
        <span>
          Part of the fleet
          <small>
            This cluster shares each model's traffic with the other fleet clusters. It gets the site weights, under its
            name as the zone, for the models it serves. Needs API keys enforced, a client listener and a peer host, and
            the same gateway namespace as the other fleet clusters.
          </small>
        </span>
      </label>
    </FormModal>
  );
}
