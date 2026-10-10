import { useState, type FormEvent } from "react";
import { api, type Model, type Quota, type Tenant, type UsageReport, type Window } from "../api";
import { WindowSelect, formatTokens, windowLabel } from "../components";
import { UsageMeter, confirmReset } from "./Usage";

/** The quota being changed in its row: the model stays, the budget is edited. */
interface Edit {
  id: string;
  limit: string;
  window: Window;
}

/**
 * The quotas of one tenant. A quota that exists is changed in its own row.
 * The form above the table only adds a quota for a model that has none, so
 * adding never replaces one.
 */
export default function TenantQuotas(props: {
  tenant: Tenant;
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

  const usageOf = (modelId: string) => usage?.quotas.find((u) => u.model_id === modelId);
  const withQuota = new Set(quotas.map((q) => q.model_id));
  const free = props.models.filter((m) => !withQuota.has(m.id));

  const add = (e: FormEvent) => {
    e.preventDefault();
    act(async () => {
      await api.setQuota(tenant.id, added.model_id, Number(added.limit), added.window, added.shadow);
      setAdded({ model_id: "", limit: "", window: added.window, shadow: false });
    });
  };

  const save = (e: FormEvent, q: Quota) => {
    e.preventDefault();
    if (!edit) return;
    act(async () => {
      await api.setQuota(tenant.id, q.model_id, Number(edit.limit), edit.window, q.shadow);
      setEdit(null);
    });
  };

  return (
    <section className="card">
      <h2>Token quotas</h2>
      <p className="hint">One budget per model, counted across every site.</p>
      {free.length === 0 ? (
        props.models.length > 0 && <p className="hint">Every model has a quota. Change one with Edit in its row.</p>
      ) : (
        <form className="inline-form" onSubmit={add}>
          <select required value={added.model_id} onChange={(e) => setAdded({ ...added, model_id: e.target.value })}>
            <option value="">Add a quota for…</option>
            {free.map((m) => (
              <option key={m.id} value={m.id} disabled={!m.quota_capable}>
                {m.name}
                {m.quota_capable ? "" : " (no quota possible)"}
              </option>
            ))}
          </select>
          <input
            required
            type="number"
            min={1}
            placeholder="Tokens"
            value={added.limit}
            onChange={(e) => setAdded({ ...added, limit: e.target.value })}
          />
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
                  <td className="strong">{q.model_name}</td>
                  <td>
                    {editing ? (
                      <form id={`quota-${q.id}`} className="inline-form quota-edit" onSubmit={(e) => save(e, q)}>
                        <input
                          required
                          autoFocus
                          type="number"
                          min={1}
                          aria-label={`Tokens for ${q.model_name}`}
                          value={editing.limit}
                          onChange={(e) => setEdit({ ...editing, limit: e.target.value })}
                          onKeyDown={(e) => e.key === "Escape" && setEdit(null)}
                        />
                        <WindowSelect value={editing.window} onChange={(w) => setEdit({ ...editing, window: w })} />
                      </form>
                    ) : (
                      <>
                        {formatTokens(q.token_limit)} per {windowLabel[q.window]}
                        {q.shadow && " "}
                        {q.shadow && (
                          <span className="tag warn" title="Counted, but requests are not rejected by this quota.">
                            dry run
                          </span>
                        )}
                      </>
                    )}
                  </td>
                  {usage?.enabled && <td>{used ? <UsageMeter q={used} /> : "—"}</td>}
                  <td className="row-actions">
                    {editing ? (
                      <>
                        <button type="submit" form={`quota-${q.id}`} className="primary" disabled={busy}>
                          Save
                        </button>
                        <button disabled={busy} onClick={() => setEdit(null)}>
                          Cancel
                        </button>
                      </>
                    ) : (
                      <>
                        <button
                          disabled={busy}
                          onClick={() => setEdit({ id: q.id, limit: String(q.token_limit), window: q.window })}
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
                          onClick={() => act(() => api.setQuota(tenant.id, q.model_id, q.token_limit, q.window, !q.shadow))}
                        >
                          {q.shadow ? "Enforce" : "Dry run"}
                        </button>
                        <button
                          className="danger"
                          disabled={busy}
                          onClick={() =>
                            confirm(`Remove the quota of ${tenant.slug} on ${q.model_name}?`) && act(() => api.deleteQuota(q.id))
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
      {edit && (
        <p className="hint">
          What the tenant has used in this window stays counted against the new limit. Changing the window starts a new
          count.
        </p>
      )}
    </section>
  );
}
