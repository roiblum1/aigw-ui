import { useState } from "react";
import BackendList from "./BackendList";
import ModelForm from "./ModelForm";
import ModelPrices from "./ModelPrices";
import { Boxes, Plus, Radar } from "lucide-react";
import { api, type Endpoint, type Model, type Window } from "../api";
import {
  Empty,
  ErrorBanner,
  Field,
  FormModal,
  LimitInput,
  PageHeader,
  formatAmount,
  formatNumber,
  formatPrice,
  formatTime,
  limitText,
  limitValue,
  useAction,
  useLoad,
  windowLabel,
} from "../components";

export default function Models() {
  const { data, error, reload } = useLoad(async () => {
    const [models, clusters] = await Promise.all([api.models(), api.clusters()]);
    return { models, clusters };
  }, "models-page");
  const action = useAction();
  const [editing, setEditing] = useState<Model | "new" | null>(null);
  // The ID, so the dialog shows the model as it is after a change.
  const [pricing, setPricing] = useState<string | null>(null);
  const [limiting, setLimiting] = useState<Model | null>(null);
  const [standardOf, setStandardOf] = useState<Model | null>(null);
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
        `Serve tenants past their budget on ${m.name} as best-effort? They are no longer refused: their requests are marked best-effort and counted apart from the budget. Whether they wait behind the others depends on each site's scheduler; with KServe's default settings they do not.`,
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
                      <span
                        className="tag"
                        title="What a million input tokens cost, the same on every site. The model's quotas and usage are counted in dollars."
                      >
                        {m.prices ? `${formatPrice(m.prices.price_input)} / 1M in` : "priced"}
                      </span>
                    ) : (
                      m.cost_expression && (
                        <span className="tag" title={m.cost_expression}>
                          weighted
                        </span>
                      )
                    )}
                    {m.market?.state === "moved" && (
                      <span
                        className="tag warn"
                        title="The market price changed since the model's prices were taken from it. Prices shows the new one; nothing changes by itself."
                      >
                        market moved
                      </span>
                    )}
                    {m.price_dry_run && (
                      <span className="tag warn" title="Every tenant is counted in dollars and nobody is refused. Switch it off under Prices.">
                        dry-run
                      </span>
                    )}
                    {m.pending_prices && (
                      <div className="detail">New prices from {formatTime(m.pending_prices.effective_at)}</div>
                    )}
                    <StandardLine model={m} />
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
                      title="What tenants are charged for a million tokens of the model, one price on every site and GPU. With prices, quotas and usage are counted in dollars."
                      onClick={() => setPricing(m.id)}
                    >
                      Prices
                    </button>
                    <button
                      title="The budget every tenant has on the model, through one rule in place of a rule per tenant. A tenant's own quota adds to it."
                      onClick={() => setStandardOf(m)}
                    >
                      Standard budget
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
      {standardOf && <StandardBudget model={standardOf} onClose={() => setStandardOf(null)} onChanged={reload} />}
      {priced && <ModelPrices key={priced.id} model={priced} onClose={() => setPricing(null)} onChanged={reload} />}
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
  const measured = c.basis === "measured";
  const title =
    `${c.detail}\nCapacity applied ${c.weight ?? "unknown"}, last read ${c.observed ?? "unknown"} at ${formatTime(c.observed_at)}; ` +
    `changed ${formatTime(c.changed_at)}.\nThe weight is the capacity × 100, and at least 1.` +
    (measured
      ? "\nThe capacity comes from the measured shapes of the model's deployments, because every site's are measured."
      : "\nThe capacity comes from aigw-ui.io/capacity-per-instance, until every site's deployments of the model are measured.") +
    (moving ? "\nIt moves one instance per poll, and drops only after two polls agree." : "");
  return (
    <span className={moving || c.drained ? "tag warn" : "tag manual"} title={title}>
      {c.drained ? "draining · " : ""}
      weight {mine.weight}
      {moving ? goal : ""}
      {share}
      {measured && " · measured"}
    </span>
  );
}

/** The backends a discovered endpoint's quota attaches to, with what limits it. */

/** Sets the most a tenant may use of a model as best-effort in one period. */
/** A model's standard budget, its waiting change and the rules it saves. */
function StandardLine({ model: m }: { model: Model }) {
  const next = m.standard_next;
  return (
    <>
      {m.standard && (
        <div className="detail">
          Standard: {formatAmount(m.standard.limit, m.unit)} per {windowLabel[m.standard.window]} for every tenant
        </div>
      )}
      {next && (
        <div className="detail">
          {next.standard
            ? `From ${formatTime(next.at)}: ${formatAmount(next.standard.limit, m.unit)} per ${windowLabel[next.standard.window]} for every tenant`
            : `From ${formatTime(next.at)}: no standard`}
        </div>
      )}
      {m.standard_held && <div className="detail warn-text">Standard not in effect: {m.standard_held}.</div>}
      {m.unit === "credits" && !m.standard && !next?.standard && (
        <div
          className="detail warn-text"
          title="A tenant without a quota of its own on this model has no budget of its own here: it draws from the model's pool, which every tenant shares. A standard gives each tenant its own budget, in one rule for all."
        >
          No standard: tenants without a quota of their own share the pool.
        </div>
      )}
      {(m.rules > 0 || m.rules_per_tenant > 0) && (
        <div
          className="detail"
          title="Bucket rules in the model's QuotaPolicy on each gateway. Every rule is checked for every request. With tokens only, tenants with the same quota of their own share one rule."
        >
          {m.rules} {m.rules === 1 ? "rule" : "rules"}
          {m.rules_per_tenant > m.rules && `, ${m.rules_per_tenant} with a rule per tenant`}
        </div>
      )}
    </>
  );
}

/** Sets the budget every tenant has on a model, from the next 00:00 UTC. */
function StandardBudget(props: { model: Model; onClose: () => void; onChanged: () => Promise<void> }) {
  const m = props.model;
  const current = m.standard_next ? m.standard_next.standard : m.standard;
  const [on, setOn] = useState(current !== null);
  const [text, setText] = useState(current ? limitText(current.limit, m.unit) : "");
  const [window, setWindow] = useState<Window>(current?.window ?? "1d");
  return (
    <FormModal
      title={`Standard budget of ${m.name}`}
      submitLabel="Save"
      onClose={props.onClose}
      onSubmit={async () => {
        await api.setStandard(m.id, on ? { limit: limitValue(text, m.unit), window } : null);
        await props.onChanged();
      }}
    >
      <p className="hint">
        Every tenant may use the model up to the standard, each with a count of its own, through one rule on the
        gateway in place of a rule per tenant. A tenant's own quota on the model is an exception that adds to the
        standard: a request passes while either has room, so no tenant can be held below it. To give a tenant less,
        lower the standard and give the others exceptions. A standard of the smallest amount closes the model to
        everyone without an exception.
      </p>
      <p className="hint">
        It starts at the next 00:00 UTC, when every count starts anew, so nobody's count is lost. It needs every tenant
        on tokens: the gateway reads the tenant from the token.
      </p>
      <label className="check">
        <input type="checkbox" checked={on} onChange={(e) => setOn(e.target.checked)} />
        <span>Give every tenant a standard budget</span>
      </label>
      {on && (
        <>
          <Field label={m.unit === "credits" ? "Each tenant, in dollars" : "Each tenant, in tokens"}>
            <LimitInput unit={m.unit} value={text} onChange={setText} label="Standard budget" autoFocus />
          </Field>
          <Field label="Per">
            <select value={window} onChange={(e) => setWindow(e.target.value as Window)}>
              <option value="1m">minute</option>
              <option value="1h">hour</option>
              <option value="1d">day</option>
            </select>
          </Field>
        </>
      )}
      {m.standard_next && (
        <p className="hint">
          A change waits for {formatTime(m.standard_next.at)}; saving replaces it.{" "}
          <button
            type="button"
            className="ghost"
            onClick={async () => {
              await api.cancelStandard(m.id);
              await props.onChanged();
              props.onClose();
            }}
          >
            Cancel the change
          </button>
        </p>
      )}
    </FormModal>
  );
}

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
