import { useState, type FormEvent } from "react";
import {
  api,
  type Model,
  type ModelBudget,
  type Quota,
  type SigningKeys,
  type Tenant,
  type Unit,
  type UsageReport,
  type Window,
} from "../api";
import { LimitInput, WindowSelect, formatAmount, limitText, limitValue, useLoad, windowLabel } from "../components";
import { held } from "./TenantBudget";
import { UsageMeter, confirmBudgetReset, confirmReset } from "./Usage";

/** The choice in the add form that gives the tenant one budget for all models. */
const ALL = "all";

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
 * The quotas of one tenant: one budget for all models together, or one
 * quota per model, never both. A quota that exists is changed in its own
 * row. The form above the table only adds what the tenant has not got, so
 * adding never replaces anything.
 */
export default function TenantQuotas(props: {
  tenant: Tenant;
  /** What the gateways take: a budget for all models and the amounts travel in tokens. */
  identity: SigningKeys["identity"] | undefined;
  quotas: Quota[];
  models: Model[];
  usage: UsageReport | null;
  busy: boolean;
  /** Runs a change and loads the tenant again. */
  act: (fn: () => Promise<unknown>) => void;
  /** Runs a usage reset and loads the usage again. */
  resetUsage: (modelId: string) => void;
  /** Resets the use of the budget for all models and loads the usage again. */
  resetBudget: () => void;
}) {
  const { tenant, quotas, usage, busy, act } = props;
  const all = tenant.budget;
  const tokensOnly = props.identity === "tokens";
  /** Why a budget for all models cannot be enforced yet, or null. */
  const why = held(props.identity);
  const [added, setAdded] = useState<{ model_id: string; limit: string; window: Window; shadow: boolean }>({
    model_id: "",
    limit: "",
    window: "1d",
    shadow: false,
  });
  const [edit, setEdit] = useState<Edit | null>(null);
  const [editAll, setEditAll] = useState<{ limit: string; window: Window } | null>(null);
  const budgets = useLoad(api.budgets, "budgets");
  const budgetOf = (modelId: string) => budgets.data?.models.find((b) => b.model_id === modelId);
  /** Runs a change, then reads the budgets again. */
  const change = (fn: () => Promise<unknown>) =>
    act(async () => {
      await fn();
      await budgets.reload();
    });
  // What this tenant can use a day over the models counted in dollars.
  const dollarsDay = all
    ? 0
    : quotas.filter((q) => q.unit === "credits" && !q.shadow).reduce((n, q) => n + dailyMax(q.token_limit, q.window), 0);
  const usedAll = usage?.budgets.find((b) => b.tenant_id === tenant.id);
  /** Whether the tenant may call the model: a tenant with a list of models may call those only. */
  const callable = (modelId: string) => tenant.models == null || tenant.models.includes(modelId);
  const itsModels = props.models.filter((m) => callable(m.id));
  const priced = itsModels.filter((m) => m.prices);
  const uncounted = itsModels.filter((m) => !m.prices || m.price_dry_run);

  const usageOf = (modelId: string) => usage?.quotas.find((u) => u.model_id === modelId);
  const withQuota = new Set(quotas.map((q) => q.model_id));
  const free = props.models.filter((m) => !withQuota.has(m.id));
  const addingAll = added.model_id === ALL;
  const addedUnit: Unit = addingAll ? "credits" : (props.models.find((m) => m.id === added.model_id)?.unit ?? "tokens");
  /** Why "All models" cannot be chosen now, or "". */
  const allClosed =
    quotas.length > 0
      ? "remove the quotas below first"
      : why
        ? "needs every tenant on tokens"
        : priced.length === 0
          ? "no model has prices yet"
          : "";

  const addedAfter =
    addingAll || added.limit === "" || added.shadow ? null : dailyMax(limitValue(added.limit, addedUnit), added.window);
  const add = (e: FormEvent) => {
    e.preventDefault();
    if (addingAll) {
      change(async () => {
        await api.setTenantBudget(tenant.id, limitValue(added.limit, "credits"), added.window);
        setAdded({ model_id: "", limit: "", window: added.window, shadow: false });
      });
      return;
    }
    if (addedAfter != null && !confirmBudget(budgetOf(added.model_id), 0, addedAfter)) return;
    change(async () => {
      await api.setQuota(tenant.id, added.model_id, limitValue(added.limit, addedUnit), added.window, added.shadow);
      setAdded({ model_id: "", limit: "", window: added.window, shadow: false });
    });
  };
  const saveAll = (e: FormEvent) => {
    e.preventDefault();
    if (!editAll) return;
    change(async () => {
      await api.setTenantBudget(tenant.id, limitValue(editAll.limit, "credits"), editAll.window);
      setEditAll(null);
    });
  };
  const removeAll = () =>
    confirm(
      `Remove the budget for all models of ${tenant.slug}? On each model it then draws from the model's pool, ` +
        "until it gets quotas of its own. Set again in the same period, it comes back with what was used.",
    ) && change(() => api.setTenantBudget(tenant.id, null, "1d"));

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
  const removeQuota = (q: Quota) =>
    confirm(`Remove the quota of ${tenant.slug} on ${q.model_name}?`) && change(() => api.deleteQuota(q.id));

  return (
    <section className="card">
      <h2>Quotas</h2>
      <p className="hint">
        One budget for <span className="strong">all models</span> together, in dollars, or one quota per model, each
        with its own amount: not both. Counted across every site. A model with prices is counted in dollars, any other
        in tokens.
        {dollarsDay > 0 && (
          <>
            {" "}
            Over the models in dollars, this tenant can use up to <span className="strong">{formatAmount(dollarsDay, "credits")}</span>{" "}
            a day.
          </>
        )}
        {tokensOnly && (
          <>
            {" "}
            The amount travels in the tenant&apos;s tokens: a personal key&apos;s next token, within minutes, carries a change;
            a token created before it keeps the old amount.
          </>
        )}
      </p>
      {all && why && <div className="banner warn">{why}</div>}
      {all && quotas.length > 0 && (
        <div className="banner warn">
          It also has quotas per model, from before a tenant had one or the other: remove them. On a model with prices
          they hold nothing while it has a budget for all models.
        </div>
      )}
      {all ? (
        <p className="hint">
          It has one budget for all models. To give it quotas per model, remove that budget first.
        </p>
      ) : (
        <form className="inline-form" onSubmit={add}>
          <select
            required
            value={added.model_id}
            onChange={(e) => setAdded({ ...added, model_id: e.target.value, limit: "", shadow: false })}
          >
            <option value="">Add a quota for…</option>
            <option value={ALL} disabled={allClosed !== ""}>
              All models{allClosed ? ` (${allClosed})` : ""}
            </option>
            {free.map((m) => (
              <option key={m.id} value={m.id} disabled={!m.quota_capable || !callable(m.id)}>
                {m.name}
                {!callable(m.id) ? " (not in its models)" : m.quota_capable ? "" : " (no quota possible)"}
              </option>
            ))}
          </select>
          <LimitInput unit={addedUnit} value={added.limit} onChange={(limit) => setAdded({ ...added, limit })} />
          <WindowSelect value={added.window} onChange={(w) => setAdded({ ...added, window: w })} />
          {!addingAll && (
            <label className="check" title="Usage is counted against this quota, but it never rejects a request.">
              <input
                type="checkbox"
                checked={added.shadow}
                onChange={(e) => setAdded({ ...added, shadow: e.target.checked })}
              />
              <span>Dry run</span>
            </label>
          )}
          <button type="submit" className="primary" disabled={busy}>
            Add quota
          </button>
        </form>
      )}
      {(addingAll || all) && (
        <p className="hint">
          All models: one amount in dollars for every model with prices
          {tenant.models != null && " that the tenant may call (its list of models)"}, counted together. The tenant
          chooses what to spend it on, and a cheaper model spends it more slowly. Once it is spent the gateway refuses the tenant (HTTP
          429) on every model until the next {windowLabel[(all ?? added).window]}.
          {uncounted.length > 0 &&
            ` Models without prices, or with prices in a dry run, cost nothing from it, and are refused all the same once it is spent: ${uncounted.map((m) => m.name).join(", ")}.`}
        </p>
      )}
      {added.model_id && !addingAll && <BudgetLine b={budgetOf(added.model_id)} before={0} after={addedAfter} />}
      {quotas.length === 0 && !all ? (
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
            {all && (
              <tr>
                <td className="strong">All models</td>
                <td>
                  {editAll ? (
                    <form id="quota-all" className="inline-form quota-edit" onSubmit={saveAll}>
                      <LimitInput
                        autoFocus
                        unit="credits"
                        label="Limit for all models"
                        value={editAll.limit}
                        onChange={(limit) => setEditAll({ ...editAll, limit })}
                        onKeyDown={(e) => e.key === "Escape" && setEditAll(null)}
                      />
                      <WindowSelect value={editAll.window} onChange={(w) => setEditAll({ ...editAll, window: w })} />
                    </form>
                  ) : (
                    <>
                      {formatAmount(all.credits, "credits")} per {windowLabel[all.window]}
                    </>
                  )}
                </td>
                {usage?.enabled && (
                  <td>
                    {usedAll ? (
                      <UsageMeter
                        q={{ ...usedAll, overage_used: 0, unit: "credits", shadow: false, model_id: "", model_name: "all models" }}
                      />
                    ) : (
                      "—"
                    )}
                  </td>
                )}
                <td className="row-actions">
                  {editAll ? (
                    <>
                      <button key="save" type="submit" form="quota-all" className="primary" disabled={busy}>
                        Save
                      </button>
                      <button key="cancel" type="button" disabled={busy} onClick={() => setEditAll(null)}>
                        Cancel
                      </button>
                    </>
                  ) : (
                    <>
                      <button
                        key="edit"
                        type="button"
                        disabled={busy}
                        onClick={() => setEditAll({ limit: limitText(all.credits, "credits"), window: all.window })}
                      >
                        Edit
                      </button>
                      {usage?.can_reset && usedAll && (
                        <button
                          disabled={busy || usedAll.used === 0}
                          onClick={() => confirmBudgetReset(usedAll) && props.resetBudget()}
                        >
                          Reset usage
                        </button>
                      )}
                      <button className="danger" disabled={busy} onClick={removeAll}>
                        Remove
                      </button>
                    </>
                  )}
                </td>
              </tr>
            )}
            {quotas.map((q) => {
              const used = usageOf(q.model_id);
              const editing = edit?.id === q.id ? edit : null;
              return (
                <tr key={q.id}>
                  <td className="strong">{q.model_name}</td>
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
                        {all && " "}
                        {all && (
                          <span
                            className="tag warn"
                            title="From before a tenant had one or the other. A tenant with a budget for all models has no quota per model: remove it."
                          >
                            from before: remove
                          </span>
                        )}
                        {!all && !callable(q.model_id) && " "}
                        {!all && !callable(q.model_id) && (
                          <span
                            className="tag warn"
                            title="The tenant may not call this model, so the gateway refuses it before the quota is asked. Remove the quota, or add the model to the tenant's models."
                          >
                            not in its models
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
                    ) : all ? (
                      <button key="remove" className="danger" disabled={busy} onClick={() => removeQuota(q)}>
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
                        <button className="danger" disabled={busy} onClick={() => removeQuota(q)}>
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
      {(edit || editAll) && (
        <p className="hint">
          What the tenant has used in this window stays counted against the new limit. Changing the window starts a new
          count.
        </p>
      )}
    </section>
  );
}
