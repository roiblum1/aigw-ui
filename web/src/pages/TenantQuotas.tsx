import { useState, type FormEvent } from "react";
import { api, type Model, type ModelBudget, type Quota, type Tenant, type Unit, type UsageReport, type Window } from "../api";
import { LimitInput, WindowSelect, formatAmount, limitText, limitValue, useLoad, windowLabel } from "../components";
import { hasPrices } from "./TenantBudget";
import { UsageMeter, confirmReset } from "./Usage";

/** The quota being changed in its row: the model stays, the budget is edited. */
interface Edit {
  id: string;
  /** Tokens, or dollars for a model with prices. */
  limit: string;
  window: Window;
}

/** How many windows fit in a day: an hourly budget can be used 24 times. */
const perDay: Record<Window, number> = { "1m": 1440, "1h": 24, "1d": 1 };

/** The most a budget lets a tenant use in a day. */
const dailyMax = (limit: number, window: Window) => limit * perDay[window];

/** What the model's budgets add up to a day if this tenant's budget changes from before to after. */
function afterChange(b: ModelBudget, before: number, after: number) {
  const given = b.given_day - before + after;
  return { given, left: b.ceiling_day - given, overcommit: b.capacity_day > 0 ? given / b.capacity_day : null };
}

/**
 * Where a model's budgets stand against what its GPUs serve a day, and where
 * they would stand with the budget being typed: before is this tenant's
 * budget now, at its daily maximum, after the one in the form.
 */
function BudgetLine(props: { b: ModelBudget | undefined; before: number; after: number | null }) {
  const { b } = props;
  if (!b) return null;
  const show = (n: number) => formatAmount(n, b.unit);
  if (!b.measured) {
    return (
      <p className="hint">
        {b.model_name} is not measured yet, so what it can be given is not known. Measure it on the Capacity page.
      </p>
    );
  }
  const next = props.after == null ? null : afterChange(b, props.before, props.after);
  return (
    <p className="hint">
      {b.model_name} can be given {show(b.capacity_day)} a day, up to {show(b.ceiling_day)} with its{" "}
      {b.overcommit_limit}× overcommit limit. The budgets take {show(b.given_day)}
      {b.left_day >= 0 ? `, so ${show(b.left_day)} is left.` : `, ${show(-b.left_day)} past the ceiling.`}
      {next && (
        <>
          {" "}
          With this budget:{" "}
          <span className={next.left < 0 ? "strong warn-text" : "strong"}>
            {show(next.given)} given{next.overcommit != null && ` (${next.overcommit.toFixed(2)}×)`},{" "}
            {next.left >= 0 ? `${show(next.left)} left` : `${show(-next.left)} past the ceiling`}
          </span>
          .
        </>
      )}
    </p>
  );
}

/** Asks before a budget takes a model past its ceiling. Nothing is refused. */
function confirmBudget(b: ModelBudget | undefined, before: number, after: number): boolean {
  if (!b?.measured) return true;
  const next = afterChange(b, before, after);
  if (next.left >= 0) return true;
  const show = (n: number) => formatAmount(n, b.unit);
  return confirm(
    `${b.model_name}'s budgets would add up to ${show(next.given)} a day, ${show(-next.left)} past its ceiling of ${show(b.ceiling_day)} ` +
      `(${b.overcommit_limit}× what its GPUs serve a day). If tenants then use more than the GPUs serve, every answer gets slower. Save it anyway?`,
  );
}

/**
 * The quotas of one tenant. A quota that exists is changed in its own row.
 * The form above the table only adds a quota for a model that has none, so
 * adding never replaces one.
 */
export default function TenantQuotas(props: {
  tenant: Tenant;
  /** The gateways take tokens only: the amounts travel in the tenant's tokens. */
  tokensOnly: boolean;
  quotas: Quota[];
  models: Model[];
  usage: UsageReport | null;
  busy: boolean;
  /** Runs a change and loads the tenant again. */
  act: (fn: () => Promise<unknown>) => void;
  /** Runs a usage reset and loads the usage again. */
  resetUsage: (modelId: string) => void;
}) {
  const { tenant, quotas, usage, busy, act } = props;
  const [added, setAdded] = useState<{ model_id: string; limit: string; window: Window; shadow: boolean }>({
    model_id: "",
    limit: "",
    window: "1d",
    shadow: false,
  });
  const [edit, setEdit] = useState<Edit | null>(null);
  const budgets = useLoad(api.budgets, "budgets");
  const budgetOf = (modelId: string) => budgets.data?.models.find((b) => b.model_id === modelId);
  /** Runs a change, then reads the budgets again. */
  const change = (fn: () => Promise<unknown>) =>
    act(async () => {
      await fn();
      await budgets.reload();
    });
  /** A budget across models holds the tenant on the model: it has no quota of its own there. */
  const covered = (modelId: string) => tenant.budget != null && hasPrices(props.models.find((m) => m.id === modelId));
  const coveredNames = props.models.filter((m) => covered(m.id)).map((m) => m.name);
  // What this tenant can use a day over the models counted in dollars.
  const dollarsDay = quotas
    .filter((q) => q.unit === "credits" && !q.shadow && !covered(q.model_id))
    .reduce((n, q) => n + dailyMax(q.token_limit, q.window), 0);

  const usageOf = (modelId: string) => usage?.quotas.find((u) => u.model_id === modelId);
  const withQuota = new Set(quotas.map((q) => q.model_id));
  const free = props.models.filter((m) => !withQuota.has(m.id) && !covered(m.id));
  const addedUnit: Unit = props.models.find((m) => m.id === added.model_id)?.unit ?? "tokens";

  const addedAfter = added.limit === "" || added.shadow ? null : dailyMax(limitValue(added.limit, addedUnit), added.window);
  const add = (e: FormEvent) => {
    e.preventDefault();
    if (addedAfter != null && !confirmBudget(budgetOf(added.model_id), 0, addedAfter)) return;
    change(async () => {
      await api.setQuota(tenant.id, added.model_id, limitValue(added.limit, addedUnit), added.window, added.shadow);
      setAdded({ model_id: "", limit: "", window: added.window, shadow: false });
    });
  };

  /** A quota's budget a day as counted against the model, 0 for a dry run. */
  const counted = (q: Quota) => (q.shadow ? 0 : dailyMax(q.token_limit, q.window));
  const editedAfter = (q: Quota) =>
    !edit || edit.limit === "" || q.shadow ? null : dailyMax(limitValue(edit.limit, q.unit), edit.window);
  const save = (e: FormEvent, q: Quota) => {
    e.preventDefault();
    if (!edit) return;
    const after = editedAfter(q);
    if (after != null && !confirmBudget(budgetOf(q.model_id), counted(q), after)) return;
    change(async () => {
      await api.setQuota(tenant.id, q.model_id, limitValue(edit.limit, q.unit), edit.window, q.shadow);
      setEdit(null);
    });
  };

  return (
    <section className="card">
      <h2>Quotas</h2>
      <p className="hint">
        One budget per model, counted across every site. A model with prices is counted in dollars, any other in
        tokens.
        {dollarsDay > 0 && (
          <>
            {" "}
            Over the models in dollars, this tenant can use up to <span className="strong">{formatAmount(dollarsDay, "credits")}</span>{" "}
            a day.
          </>
        )}
        {props.tokensOnly && (
          <>
            {" "}
            The amount travels in the tenant&apos;s tokens: a personal key&apos;s next token, within minutes, carries a change;
            a token created before it keeps the old amount. A changed amount starts the tenant&apos;s count again.
          </>
        )}
      </p>
      {coveredNames.length > 0 && (
        <p className="hint">
          Its budget across models holds it on every model with prices ({coveredNames.join(", ")}), so a quota of its
          own is only for a model without prices. A tenant has one budget across models or a budget per model, not
          both.
        </p>
      )}
      {free.length === 0 ? (
        props.models.length > 0 &&
        coveredNames.length === 0 && <p className="hint">Every model has a quota. Change one with Edit in its row.</p>
      ) : (
        <form className="inline-form" onSubmit={add}>
          <select
            required
            value={added.model_id}
            onChange={(e) => setAdded({ ...added, model_id: e.target.value, limit: "" })}
          >
            <option value="">Add a quota for…</option>
            {free.map((m) => (
              <option key={m.id} value={m.id} disabled={!m.quota_capable}>
                {m.name}
                {m.quota_capable ? "" : " (no quota possible)"}
              </option>
            ))}
          </select>
          <LimitInput unit={addedUnit} value={added.limit} onChange={(limit) => setAdded({ ...added, limit })} />
          <WindowSelect value={added.window} onChange={(w) => setAdded({ ...added, window: w })} />
          <label className="check" title="Usage is counted against this quota, but it never rejects a request.">
            <input
              type="checkbox"
              checked={added.shadow}
              onChange={(e) => setAdded({ ...added, shadow: e.target.checked })}
            />
            <span>Dry run</span>
          </label>
          <button type="submit" className="primary" disabled={busy}>
            Add quota
          </button>
        </form>
      )}
      {added.model_id && <BudgetLine b={budgetOf(added.model_id)} before={0} after={addedAfter} />}
      {quotas.length === 0 ? (
        <p className="hint">No quotas yet.</p>
      ) : (
        <table className="plain">
          <thead>
            <tr>
              <th>Model</th>
              <th>Limit</th>
              {usage?.enabled && <th>Used now</th>}
              <th />
            </tr>
          </thead>
          <tbody>
            {quotas.map((q) => {
              const used = usageOf(q.model_id);
              const editing = edit?.id === q.id ? edit : null;
              return (
                <tr key={q.id}>
                  <td className="strong">
                    {q.model_name}
                  </td>
                  <td>
                    {editing ? (
                      <form id={`quota-${q.id}`} className="inline-form quota-edit" onSubmit={(e) => save(e, q)}>
                        <LimitInput
                          autoFocus
                          unit={q.unit}
                          label={`Limit for ${q.model_name}`}
                          value={editing.limit}
                          onChange={(limit) => setEdit({ ...editing, limit })}
                          onKeyDown={(e) => e.key === "Escape" && setEdit(null)}
                        />
                        <WindowSelect value={editing.window} onChange={(w) => setEdit({ ...editing, window: w })} />
                      </form>
                    ) : (
                      <>
                        {formatAmount(q.token_limit, q.unit)} per {windowLabel[q.window]}
                        {q.shadow && " "}
                        {q.shadow && (
                          <span className="tag warn" title="Counted, but requests are not rejected by this quota.">
                            dry run
                          </span>
                        )}
                        {covered(q.model_id) && " "}
                        {covered(q.model_id) && (
                          <span
                            className="tag warn"
                            title="From before a tenant had one or the other. Its budget across models holds it on this model, so this quota holds nothing: remove it."
                          >
                            holds nothing: budget across models
                          </span>
                        )}
                      </>
                    )}
                  </td>
                  {usage?.enabled && <td>{used ? <UsageMeter q={used} /> : "—"}</td>}
                  <td className="row-actions">
                    {editing ? (
                      <>
                        {/* The keys keep React from turning the Edit button that was
                            just clicked into this submit button: the browser would
                            then finish that click by submitting the form. */}
                        <button key="save" type="submit" form={`quota-${q.id}`} className="primary" disabled={busy}>
                          Save
                        </button>
                        <button key="cancel" type="button" disabled={busy} onClick={() => setEdit(null)}>
                          Cancel
                        </button>
                      </>
                    ) : covered(q.model_id) ? (
                      <button
                        key="remove"
                        className="danger"
                        disabled={busy}
                        onClick={() =>
                          confirm(`Remove the quota of ${tenant.slug} on ${q.model_name}?`) && change(() => api.deleteQuota(q.id))
                        }
                      >
                        Remove
                      </button>
                    ) : (
                      <>
                        <button
                          key="edit"
                          type="button"
                          disabled={busy}
                          onClick={() => setEdit({ id: q.id, limit: limitText(q.token_limit, q.unit), window: q.window })}
                        >
                          Edit
                        </button>
                        {usage?.can_reset && used && (
                          <button
                            disabled={busy || used.used + used.overage_used === 0}
                            onClick={() => confirmReset(used) && props.resetUsage(q.model_id)}
                          >
                            Reset usage
                          </button>
                        )}
                        <button
                          disabled={busy}
                          onClick={() => change(() => api.setQuota(tenant.id, q.model_id, q.token_limit, q.window, !q.shadow))}
                        >
                          {q.shadow ? "Enforce" : "Dry run"}
                        </button>
                        <button
                          className="danger"
                          disabled={busy}
                          onClick={() =>
                            confirm(`Remove the quota of ${tenant.slug} on ${q.model_name}?`) && change(() => api.deleteQuota(q.id))
                          }
                        >
                          Remove
                        </button>
                      </>
                    )}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
      {edit &&
        quotas
          .filter((q) => q.id === edit.id)
          .map((q) => <BudgetLine key={q.id} b={budgetOf(q.model_id)} before={counted(q)} after={editedAfter(q)} />)}
      {edit && (
        <p className="hint">
          What the tenant has used in this window stays counted against the new limit. Changing the window starts a new
          count.
        </p>
      )}
    </section>
  );
}
