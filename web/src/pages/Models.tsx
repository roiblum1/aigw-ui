import { useState } from "react";
import { Boxes, Plus, Radar } from "lucide-react";
import { api, type Cluster, type Endpoint, type Model, type Window } from "../api";
import {
  Empty,
  ErrorBanner,
  Field,
  FormModal,
  PageHeader,
  WindowSelect,
  formatTime,
  formatTokens,
  useAction,
  useLoad,
  windowLabel,
} from "../components";

export default function Models() {
  const { data, error, reload } = useLoad(async () => ({
    models: await api.models(),
    clusters: await api.clusters(),
  }));
  const action = useAction();
  const [editing, setEditing] = useState<Model | "new" | null>(null);

  const remove = (m: Model) => {
    const discovered = m.endpoints.some((e) => e.source === "discovered");
    const note = discovered
      ? " It is still exposed by a cluster, so it will be found again on the next poll, without its quotas."
      : "";
    if (!confirm(`Delete ${m.name}? Its quotas are removed from every cluster.${note}`)) return;
    action.run(async () => {
      await api.deleteModel(m.id);
      await reload();
    });
  };

  const drain = (m: Model, e: Endpoint) => {
    const on = !e.capacity?.drained;
    if (on && !confirm(`Drain ${m.name} on ${e.cluster_name}? Its conversations move to the other sites, step by step.`))
      return;
    action.run(async () => {
      await api.drainSite(m.id, e.cluster_id, on);
      await reload();
    });
  };

  return (
    <>
      <PageHeader
        title="Models"
        subtitle="Found automatically from each cluster's AI gateway routes. You can also add one by hand."
      >
        <button
          disabled={action.busy || !data?.clusters.length}
          onClick={() => action.run(async () => void (await api.discoverAll(), await reload()))}
        >
          <Radar />
          {action.busy ? "Checking…" : "Discover now"}
        </button>
        <button className="primary" disabled={!data} onClick={() => setEditing("new")}>
          <Plus />
          Add model
        </button>
      </PageHeader>
      <ErrorBanner message={error || action.error} />

      {data && data.models.length === 0 && (
        <Empty icon={Boxes}>
          No models yet. They appear here once a cluster with AI gateway routes is added.
          {data.clusters.map(
            (c) =>
              c.discovery_message.startsWith("failed") && (
                <div key={c.id} className="detail">
                  {c.name}: {c.discovery_message}
                </div>
              ),
          )}
        </Empty>
      )}
      {data && data.models.length > 0 && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Model</th>
                <th>Served from</th>
                <th>Shared pool</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {data.models.map((m) => (
                <tr key={m.id}>
                  <td>
                    <div className="strong">{m.name}</div>
                    <div className="detail mono">{m.slug}</div>
                    {!m.quota_capable && m.endpoints.length > 0 && (
                      <span className="tag warn" title="No cluster serves this model from an AIServiceBackend, and a QuotaPolicy cannot target anything else.">
                        no quota possible
                      </span>
                    )}
                  </td>
                  <td>
                    {m.endpoints.length === 0 && <span className="detail">Not exposed on any cluster</span>}
                    {m.endpoints.map((e) => (
                      <div key={e.cluster_id} className="endpoint-line">
                        <span>{e.cluster_name}</span>
                        {e.source === "discovered" ? (
                          <>
                            <span className="tag">discovered</span>
                            <BackendList endpoint={e} />
                          </>
                        ) : (
                          <>
                            <span className="tag manual">manual</span>
                            <span className="detail mono">
                              {e.host}:{e.port}
                            </span>
                          </>
                        )}
                        <SiteWeight model={m} endpoint={e} />
                        {e.capacity && e.capacity.weight !== null && (
                          <button
                            className="link"
                            disabled={action.busy}
                            title={
                              e.capacity.drained
                                ? "Let the site take traffic again. Its weight comes back one instance per poll."
                                : "Step the site's weight down to 0, one instance per poll, before maintenance."
                            }
                            onClick={() => drain(m, e)}
                          >
                            {e.capacity.drained ? "Undrain" : "Drain"}
                          </button>
                        )}
                      </div>
                    ))}
                    {m.warnings.map((w) => (
                      <div key={w} className="detail warn-text">
                        {w}
                      </div>
                    ))}
                    {m.site_weights_note && (
                      <div className="detail" title="The gateways keep the weights they have.">
                        Site weights not written: {m.site_weights_note}.
                      </div>
                    )}
                  </td>
                  <td>
                    {formatTokens(m.default_limit)} per {windowLabel[m.default_window]}
                    {m.cost_expression && (
                      <span className="tag" title={m.cost_expression}>
                        weighted
                      </span>
                    )}
                  </td>
                  <td className="row-actions">
                    <button onClick={() => setEditing(m)}>Edit</button>
                    <button className="danger" disabled={action.busy} onClick={() => remove(m)}>
                      Delete
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {editing && data && (
        <ModelForm
          model={editing === "new" ? null : editing}
          clusters={data.clusters}
          onClose={() => setEditing(null)}
          onSaved={reload}
        />
      )}
    </>
  );
}

/** A site's share of the model's traffic, and the ready capacity it comes from. */
function SiteWeight({ model, endpoint }: { model: Model; endpoint: Endpoint }) {
  const c = endpoint.capacity;
  if (!c || c.weight === null) return null;
  const total = model.site_weights.reduce((sum, z) => sum + z.weight, 0);
  const mine = model.site_weights.find((z) => z.zone === endpoint.cluster_name);
  const share = mine && total > 0 ? ` · ${Math.round((mine.weight / total) * 100)}% of traffic` : "";
  const target = c.drained ? 0 : c.observed;
  const moving = target !== null && target !== c.weight;
  const title =
    `${c.detail}\nLast read ${formatTime(c.observed_at)}, weight last changed ${formatTime(c.changed_at)}.` +
    (moving ? "\nThe weight follows the ready capacity one step per poll, and drops only after two polls agree." : "");
  return (
    <span className={moving || c.drained ? "tag warn" : "tag manual"} title={title}>
      {c.drained ? (moving ? "draining · " : "drained · ") : ""}
      weight {c.weight}
      {moving ? ` → ${target}` : ""}
      {share}
    </span>
  );
}

/** The backends a discovered endpoint's quota attaches to, with what limits it. */
function BackendList({ endpoint }: { endpoint: Endpoint }) {
  const backends = endpoint.backends ?? [];
  if (backends.length === 0) {
    return (
      <span className="tag warn" title="This cluster does not serve the model from an AIServiceBackend, so a quota cannot be attached here.">
        no quota here
      </span>
    );
  }
  return (
    <>
      <span className="detail mono">
        {backends.map((b) => (b.namespace ? `${b.namespace}/${b.name}` : b.name)).join(", ")}
      </span>
      {backends.some((b) => !b.override) && (
        <span
          className="tag warn"
          title="The route sets no modelNameOverride for this backend. The gateway documents quota matching only against modelNameOverride, so check that the quota takes effect."
        >
          quota unverified
        </span>
      )}
    </>
  );
}

interface EndpointRow {
  enabled: boolean;
  host: string;
  port: string;
  upstream_model: string;
}

function ModelForm(props: {
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
        hint="Optional. Leave empty to charge every token the same. Counts are whole numbers and literals need a u, e.g. input_tokens + output_tokens * 4u."
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
