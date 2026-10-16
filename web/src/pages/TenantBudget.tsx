import { useState, type FormEvent } from "react";
import { api, type Model, type SigningKeys, type Tenant } from "../api";

/** Why a budget or a list of models cannot be enforced yet, or null. */
export function held(identity: SigningKeys["identity"] | undefined): string | null {
  if (identity === "tokens") return null;
  return identity === "keys"
    ? "The gateways take API keys only. A budget across models and a list of models need every tenant on tokens: create a token for each tenant, then revoke its API keys."
    : "The gateways still take API keys. Until the last API key is revoked, the gateways do not hold tenants to a budget across models or to their models.";
}

/** The models a tenant may call: every model, or a list of its own. */
export function TenantModelsCard(props: {
  tenant: Tenant;
  models: Model[];
  /** The models the tenant has a quota on: they stay in its list until the quota is removed. */
  withQuota: string[];
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
        asked. A model added later is closed to a tenant with a list until it is added here. The list travels in the
        tenant's tokens: tokens created after a change carry the new one. A model the tenant has a quota on stays in
        the list until that quota is removed.
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
                  disabled={picked.has(m.id) && props.withQuota.includes(m.id)}
                  title={props.withQuota.includes(m.id) ? "The tenant has a quota on this model. Remove the quota first." : undefined}
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
              <button disabled={busy} onClick={() => setPicked(new Set(props.withQuota))}>
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
