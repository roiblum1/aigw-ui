import { useState } from "react";
import BackendList from "./BackendList";
import ModelForm from "./ModelForm";
import ModelPrices from "./ModelPrices";
import { Boxes, Plus, Radar } from "lucide-react";
import { api, type Endpoint, type Model } from "../api";
import {
  Empty,
  ErrorBanner,
  Field,
  FormModal,
  LimitInput,
  PageHeader,
  formatAmount,
  formatNumber,
  formatTime,
  limitText,
  limitValue,
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
  // The ID, so the dialog shows the model as it is after a change.
  const [pricing, setPricing] = useState<string | null>(null);
  const [limiting, setLimiting] = useState<Model | null>(null);
  const priced = data?.models.find((m) => m.id === pricing);

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

  const setFleet = (m: Model) => {
    const question = m.fleet
      ? `Remove the entry route of ${m.name} from every fleet cluster? The model is then only reachable through routes the clusters have themselves. Its quota counters restart once.`
      : `Render an entry route for ${m.name} on every fleet cluster? Quotas then attach to that route alone, so the model's quota counters restart once.`;
    if (!confirm(question)) return;
    action.run(async () => {
      await api.setModelFleet(m.id, !m.fleet);
      await reload();
    });
  };

  const setSpent = (m: Model, choice: string) => {
    const bestEffort = choice !== "refuse";
    if (
      bestEffort &&
      m.spent_mode === "refuse" &&
      !confirm(
        `Serve tenants past their budget on ${m.name} as best-effort? They are no longer refused: their requests are queued behind all others and dropped first when a site is full. Every serving cluster needs an InferenceObjective named best-effort for the model.`,
      )
    )
      return;
    action.run(async () => {
      await api.setModelSpent(m.id, bestEffort ? "best-effort" : "refuse", choice === "best-effort-all", m.best_effort_limit);
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
                        {e.capacity && (e.capacity.weight !== null || e.capacity.drained) && (
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
                      <div className="detail warn-text" title="The gateways keep the sites and weights they have.">
                        Site weights not updated: {m.site_weights_note}.
                      </div>
                    )}
                  </td>
                  <td>
                    {formatAmount(m.default_limit, m.unit)} per {windowLabel[m.default_window]}
                    {m.unit === "credits" ? (
                      <span className="tag" title="The model has prices. Its quotas and usage are counted in dollars.">
                        priced
                      </span>
                    ) : (
                      m.cost_expression && (
                        <span className="tag" title={m.cost_expression}>
                          weighted
                        </span>
                      )
                    )}
                    {m.price_dry_run && (
                      <span className="tag warn" title="Every tenant is counted in dollars and nobody is refused. Switch it off under Prices.">
                        dry-run
                      </span>
                    )}
                    {m.pending_prices && (
                      <div className="detail">New prices from {formatTime(m.pending_prices.effective_at)}</div>
                    )}
                  </td>
                  <td className="row-actions">
                    {(m.fleet || m.site_weights.length > 0) && (
                      <button
                        disabled={action.busy}
                        title={
                          m.fleet
                            ? "Remove the entry route this tool renders for the model on every fleet cluster."
                            : "Render an entry route for the model on every fleet cluster. It sends each conversation to one of the sites that serve the model."
                        }
                        onClick={() => setFleet(m)}
                      >
                        {m.fleet ? "Entry route: on" : "Entry route: off"}
                      </button>
                    )}
                    {m.fleet && (
                      <select
                        aria-label={`When a budget is spent on ${m.name}`}
                        title="What happens to a tenant whose budget for the model is spent. Best-effort needs the entry route."
                        disabled={action.busy}
                        value={m.spent_mode === "refuse" ? "refuse" : m.best_effort_unlimited ? "best-effort-all" : "best-effort"}
                        onChange={(e) => setSpent(m, e.target.value)}
                      >
                        <option value="refuse">Budget spent: refuse</option>
                        <option value="best-effort">Budget spent: best-effort</option>
                        <option value="best-effort-all">Best-effort, also without a quota</option>
                      </select>
                    )}
                    {m.fleet && m.spent_mode === "best-effort" && (
                      <button
                        disabled={action.busy}
                        title="The most a tenant may use of the model as best-effort in one period of its quota. After that it is refused until the period ends."
                        onClick={() => setLimiting(m)}
                      >
                        Best-effort limit: {m.best_effort_limit ? formatNumber(m.best_effort_limit, m.unit) : "none"}
                      </button>
                    )}
                    <button
                      title="What a million tokens of the model cost. With prices, quotas and usage are counted in dollars."
                      onClick={() => setPricing(m.id)}
                    >
                      Prices
                    </button>
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

      {limiting && <BestEffortLimit model={limiting} onClose={() => setLimiting(null)} onChanged={reload} />}
      {priced && <ModelPrices model={priced} onClose={() => setPricing(null)} onChanged={reload} />}
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

/**
 * A site's zone weight for the model and its share of the traffic. The weight
 * is the capacity times 100, and never below 1: the gateway cannot take 0.
 */
function SiteWeight({ model, endpoint }: { model: Model; endpoint: Endpoint }) {
  const c = endpoint.capacity;
  if (!c || (c.weight === null && !c.serving)) return null;
  const total = model.site_weights.reduce((sum, z) => sum + z.weight, 0);
  const mine = model.site_weights.find((z) => z.zone === endpoint.cluster_name);
  if (!mine) {
    const why = c.drained
      ? "Drained: the site is out of this model's sites until Undrain."
      : "Not one of the model's sites. Is the cluster part of the fleet?";
    return (
      <span className={c.drained ? "tag warn" : "tag manual"} title={why}>
        {c.drained ? "drained · not listed" : "not listed"}
      </span>
    );
  }
  const share = total > 0 ? ` · ${Math.round((mine.weight / total) * 100)}% of traffic` : "";
  const target = c.drained ? 0 : c.observed;
  const moving = target !== null && target !== c.weight;
  const goal = target === null ? "" : ` → ${Math.max(1, Math.round(target * 100))}`;
  const title =
    `${c.detail}\nCapacity applied ${c.weight ?? "unknown"}, last read ${c.observed ?? "unknown"} at ${formatTime(c.observed_at)}; ` +
    `changed ${formatTime(c.changed_at)}.\nThe weight is the capacity × 100, and at least 1.` +
    (moving ? "\nIt moves one instance per poll, and drops only after two polls agree." : "");
  return (
    <span className={moving || c.drained ? "tag warn" : "tag manual"} title={title}>
      {c.drained ? "draining · " : ""}
      weight {mine.weight}
      {moving ? goal : ""}
      {share}
    </span>
  );
}

/** The backends a discovered endpoint's quota attaches to, with what limits it. */

/** Sets the most a tenant may use of a model as best-effort in one period. */
function BestEffortLimit(props: { model: Model; onClose: () => void; onChanged: () => Promise<void> }) {
  const m = props.model;
  const [limited, setLimited] = useState(m.best_effort_limit !== null);
  const [text, setText] = useState(m.best_effort_limit ? limitText(m.best_effort_limit, m.unit) : "");
  return (
    <FormModal
      title={`Best-effort limit of ${m.name}`}
      submitLabel="Save"
      onClose={props.onClose}
      onSubmit={async () => {
        await api.setModelSpent(m.id, "best-effort", m.best_effort_unlimited, limited ? limitValue(text, m.unit) : null);
        await props.onChanged();
      }}
    >
      <p className="hint">
        A tenant whose budget is spent goes on at the lowest priority, and that use is not charged to its budget. A
        limit keeps one tenant from taking all the spare capacity: at the limit it is refused until its quota's period
        ends. The limit counts per period of the tenant's quota, an hour or a day.
      </p>
      <label className="check">
        <input type="checkbox" checked={limited} onChange={(e) => setLimited(e.target.checked)} />
        <span>Limit best-effort use</span>
      </label>
      {limited && (
        <Field
          label={m.unit === "credits" ? "At most, in dollars per period" : "At most, in tokens per period"}
          hint="It applies to tenants with a quota on the model. A tenant without one has no counter to measure."
        >
          <LimitInput unit={m.unit} value={text} onChange={setText} label="Best-effort limit" autoFocus />
        </Field>
      )}
    </FormModal>
  );
}
