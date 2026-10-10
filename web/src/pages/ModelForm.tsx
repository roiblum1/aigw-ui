import { useState } from "react";
import BackendList from "./BackendList";
import { api, type Cluster, type Model, type Window } from "../api";
import { Field, FormModal, WindowSelect } from "../components";

interface EndpointRow {
  enabled: boolean;
  host: string;
  port: string;
  upstream_model: string;
}

export default function ModelForm(props: {
  model: Model | null;
  clusters: Cluster[];
  onClose: () => void;
  onSaved: () => Promise<void>;
}) {
  const { model, clusters } = props;
  const [name, setName] = useState(model?.name ?? "");
  const [limit, setLimit] = useState(String(model?.default_limit ?? 1));
  const [window, setWindow] = useState<Window>(model?.default_window ?? "1d");
  const [cost, setCost] = useState(model?.cost_expression ?? "");
  const [rows, setRows] = useState<Record<string, EndpointRow>>(() =>
    Object.fromEntries(
      clusters.map((c) => {
        const e = model?.endpoints.find((x) => x.cluster_id === c.id && x.source !== "discovered");
        return [
          c.id,
          { enabled: !!e, host: e?.host ?? "", port: String(e?.port ?? 8000), upstream_model: e?.upstream_model ?? "" },
        ];
      }),
    ),
  );
  const setRow = (id: string, patch: Partial<EndpointRow>) =>
    setRows((r) => ({ ...r, [id]: { ...r[id], ...patch } }));

  const save = async () => {
    const body = {
      name,
      default_limit: Number(limit),
      default_window: window,
      cost_expression: cost.trim(),
      endpoints: clusters
        .filter((c) => rows[c.id].enabled)
        .map((c) => ({
          cluster_id: c.id,
          host: rows[c.id].host.trim(),
          port: Number(rows[c.id].port),
          upstream_model: rows[c.id].upstream_model.trim(),
        })),
    };
    if (model) await api.updateModel(model.id, body);
    else await api.createModel(body);
    await props.onSaved();
  };

  return (
    <FormModal
      title={model ? `Edit ${model.name}` : "Add model"}
      submitLabel={model ? "Save" : "Add model"}
      onClose={props.onClose}
      onSubmit={save}
      wide
    >
      <Field
        label="Model name"
        hint={
          model ? "A model cannot be renamed." : 'The name clients send in the "model" field of the request, e.g. GLM5.3.'
        }
      >
        <input required disabled={!!model} value={name} onChange={(e) => setName(e.target.value)} />
      </Field>

      <h3>Endpoints</h3>
      {clusters.length === 0 && <p className="hint">Add a cluster first to expose this model.</p>}
      {clusters.map((c) => {
        const row = rows[c.id];
        const found = model?.endpoints.find((x) => x.cluster_id === c.id && x.source === "discovered");
        if (found && !row.enabled) {
          return (
            <div className="endpoint" key={c.id}>
              <div className="endpoint-line">
                <span className="strong">{c.name}</span>
                <span className="tag">discovered</span>
                <BackendList endpoint={found} />
              </div>
              <p className="detail">Already exposed by this cluster's own route. Nothing is created for it here.</p>
            </div>
          );
        }
        return (
          <div className="endpoint" key={c.id}>
            <label className="check">
              <input type="checkbox" checked={row.enabled} onChange={(e) => setRow(c.id, { enabled: e.target.checked })} />
              <span>{c.name}</span>
            </label>
            {row.enabled && (
              <div className="endpoint-fields">
                <Field label="Host">
                  <input
                    required
                    value={row.host}
                    placeholder="glm.models.svc.cluster.local"
                    onChange={(e) => setRow(c.id, { host: e.target.value })}
                  />
                </Field>
                <Field label="Port">
                  <input
                    required
                    type="number"
                    min={1}
                    max={65535}
                    value={row.port}
                    onChange={(e) => setRow(c.id, { port: e.target.value })}
                  />
                </Field>
                <Field label="Name on the server">
                  <input
                    value={row.upstream_model}
                    placeholder={name || "same as model name"}
                    onChange={(e) => setRow(c.id, { upstream_model: e.target.value })}
                  />
                </Field>
              </div>
            )}
          </div>
        );
      })}

      <h3>Shared pool</h3>
      <p className="hint">
        Applies once at least one tenant has a quota on this model. Every request draws from this pool as well as
        from the tenant's own quota, and is let through while either has tokens left. Keep it at 1 to hold every
        tenant strictly to its quota. Set it to what the model can serve to let tenants borrow what others leave
        unused.
      </p>
      <div className="grid-2">
        <Field label="Tokens">
          <input required type="number" min={1} value={limit} onChange={(e) => setLimit(e.target.value)} />
        </Field>
        <Field label="Window">
          <WindowSelect value={window} onChange={setWindow} />
        </Field>
      </div>

      <h3>What a request costs</h3>
      <Field
        label="Cost expression"
        hint="Optional. Leave empty to charge every token the same. Counts are whole numbers and literals need a u, e.g. input_tokens + output_tokens * 4u. input_tokens includes the cached part."
      >
        <input
          value={cost}
          spellCheck={false}
          placeholder="total_tokens"
          onChange={(e) => setCost(e.target.value)}
        />
      </Field>
    </FormModal>
  );
}
