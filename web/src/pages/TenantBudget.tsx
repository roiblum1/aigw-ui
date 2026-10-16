import { useState, type FormEvent } from "react";
import { api, type Model, type SigningKeys, type Tenant, type UsageReport, type Window } from "../api";
import { LimitInput, WindowSelect, formatDollars, limitText, limitValue, windowLabel } from "../components";
import { UsageMeter, confirmBudgetReset } from "./Usage";

/** Why a budget or a list of models cannot be enforced yet, or null. */
function held(identity: SigningKeys["identity"] | undefined): string | null {
  if (identity === "tokens") return null;
  return identity === "keys"
    ? "The gateways take API keys only. A budget across models and a list of models need every tenant on tokens: create a token for each tenant, then revoke its API keys."
    : "The gateways still take API keys. Until the last API key is revoked, the gateways do not hold tenants to a budget across models or to their models.";
}

/**
 * A tenant's budget across every priced model. One rule per tenant on the
 * gateway, charged with what each answer cost at the model's prices; the
 * tenant's quotas per model hold alongside it.
 */
export function TenantBudgetCard(props: {
  tenant: Tenant;
  models: Model[];
  usage: UsageReport | null;
  identity: SigningKeys["identity"] | undefined;
  busy: boolean;
  act: (fn: () => Promise<unknown>) => void;
  resetBudget: () => void;
}) {
  const { tenant, usage, busy, act } = props;
  const [edit, setEdit] = useState<{ limit: string; window: Window } | null>(null);
  const used = usage?.budgets.find((b) => b.tenant_id === tenant.id);
  const priced = props.models.filter((m) => m.prices);
  const unpriced = props.models.filter((m) => !m.prices || m.price_dry_run);
  const why = held(props.identity);

  const save = (e: FormEvent) => {
    e.preventDefault();
    if (!edit) return;
    act(async () => {
      await api.setTenantBudget(tenant.id, limitValue(edit.limit, "credits"), edit.window);
      setEdit(null);
    });
  };
  const remove = () =>
    confirm(
      `Remove the budget across models of ${tenant.slug}? Its quotas per model stay. ` +
        "Set again in the same period, it comes back with what was used.",
    ) &&
    act(() => api.setTenantBudget(tenant.id, null, "1d"));

  return (
    <section className="card">
      <h2>Budget across models</h2>
      <p className="hint">
        One amount in dollars for every model with prices, counted together: a cheaper model spends it more slowly.
        Once it is spent the gateway refuses the tenant (HTTP 429) on every model, until the next{" "}
        {tenant.budget ? windowLabel[tenant.budget.window] : "period"}. A quota per model holds alongside it.
        {unpriced.length > 0 &&
          ` Models without prices, or with prices in a dry run, cost nothing from it, and are refused all the same once it is spent: ${unpriced.map((m) => m.name).join(", ")}.`}
      </p>
      {tenant.budget && why && <div className="banner warn">{why}</div>}
      {edit ? (
        <form className="inline-form" onSubmit={save}>
          <LimitInput
            autoFocus
            unit="credits"
            label="Budget across models"
            value={edit.limit}
            onChange={(limit) => setEdit({ ...edit, limit })}
            onKeyDown={(e) => e.key === "Escape" && setEdit(null)}
          />
          <WindowSelect value={edit.window} onChange={(w) => setEdit({ ...edit, window: w })} />
          <button type="submit" className="primary" disabled={busy}>
            Save
          </button>
          <button type="button" disabled={busy} onClick={() => setEdit(null)}>
            Cancel
          </button>
        </form>
      ) : tenant.budget ? (
        <table className="plain">
          <tbody>
            <tr>
              <td className="strong">
                {formatDollars(tenant.budget.credits)} per {windowLabel[tenant.budget.window]}
              </td>
              {usage?.enabled && (
                <td>
                  {used ? (
                    <UsageMeter q={{ ...used, overage_used: 0, unit: "credits", shadow: false, model_id: "", model_name: "every model" }} />
                  ) : (
                    "—"
                  )}
                </td>
              )}
              <td className="row-actions">
                {usage?.can_reset && used && (
                  <button
                    disabled={busy || used.used === 0}
                    onClick={() => confirmBudgetReset(used) && props.resetBudget()}
                  >
                    Reset usage
                  </button>
                )}
                <button
                  disabled={busy}
                  onClick={() => setEdit({ limit: limitText(tenant.budget!.credits, "credits"), window: tenant.budget!.window })}
                >
                  Edit
                </button>
                <button className="danger" disabled={busy} onClick={remove}>
                  Remove
                </button>
              </td>
            </tr>
          </tbody>
        </table>
      ) : (
        <p className="hint">
          No budget across models.{" "}
          {priced.length === 0 ? (
            "No model has prices yet, so there is nothing to count it in."
          ) : why ? (
            why
          ) : (
            <button disabled={busy} onClick={() => setEdit({ limit: "", window: "1d" })}>
              Set a budget
            </button>
          )}
        </p>
      )}
      {edit && (
        <p className="hint">
          A new amount holds what the tenant already used in the running {windowLabel[edit.window]}. A new period
          length starts a new count. Reset usage starts it over.
        </p>
      )}
    </section>
  );
}

/** The models a tenant may call: every model, or a list of its own. */
export function TenantModelsCard(props: {
  tenant: Tenant;
  models: Model[];
  identity: SigningKeys["identity"] | undefined;
  busy: boolean;
  act: (fn: () => Promise<unknown>) => void;
}) {
  const { tenant, models, busy, act } = props;
  const [picked, setPicked] = useState<Set<string> | null>(null);
  const why = held(props.identity);
  const allowed = tenant.models;

  const save = (e: FormEvent) => {
    e.preventDefault();
    if (!picked) return;
    act(async () => {
      await api.setTenantModels(tenant.id, [...picked]);
      setPicked(null);
    });
  };

  return (
    <section className="card">
      <h2>Models</h2>
      <p className="hint">
        Which models the tenant may call. Any other is refused by the gateway (HTTP 403) before a model or a quota is
        asked. A model added later is closed to a tenant with a list until it is added here.
      </p>
      {allowed && why && <div className="banner warn">{why}</div>}
      {picked ? (
        <form onSubmit={save}>
          <div className="check-list">
            {models.map((m) => (
              <label key={m.id} className="check">
                <input
                  type="checkbox"
                  checked={picked.has(m.id)}
                  onChange={(e) => {
                    const next = new Set(picked);
                    if (e.target.checked) next.add(m.id);
                    else next.delete(m.id);
                    setPicked(next);
                  }}
                />
                <span>{m.name}</span>
              </label>
            ))}
          </div>
          <div className="row-actions">
            <button type="submit" className="primary" disabled={busy}>
              Save
            </button>
            <button type="button" disabled={busy} onClick={() => setPicked(null)}>
              Cancel
            </button>
          </div>
        </form>
      ) : (
        <div className="row-actions">
          <span>
            {allowed == null
              ? "Every model."
              : allowed.length === 0
                ? "No model: every request is refused."
                : models
                    .filter((m) => allowed.includes(m.id))
                    .map((m) => m.name)
                    .join(", ")}
          </span>
          {allowed == null ? (
            !why && (
              <button disabled={busy} onClick={() => setPicked(new Set())}>
                Limit to some models
              </button>
            )
          ) : (
            <>
              <button disabled={busy} onClick={() => setPicked(new Set(allowed))}>
                Edit
              </button>
              <button disabled={busy} onClick={() => act(() => api.setTenantModels(tenant.id, null))}>
                Allow every model
              </button>
            </>
          )}
        </div>
      )}
    </section>
  );
}
