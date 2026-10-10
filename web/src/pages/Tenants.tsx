import { useState, type FormEvent } from "react";
import { ArrowLeft, Copy, Plus, Users } from "lucide-react";
import { api, type Tenant } from "../api";
import {
  Empty,
  ErrorBanner,
  Field,
  FormModal,
  Modal,
  PageHeader,
  formatTime,
  useAction,
  useLoad,
  usePolling,
} from "../components";
import TenantQuotas from "./TenantQuotas";
import { REFRESH_MS } from "./Usage";

export default function Tenants() {
  const { data: tenants, error, reload } = useLoad(api.tenants);
  const [creating, setCreating] = useState(false);
  const [openId, setOpenId] = useState<string | null>(null);

  if (openId) {
    return (
      <TenantDetail
        id={openId}
        onBack={() => {
          setOpenId(null);
          reload();
        }}
      />
    );
  }

  return (
    <>
      <PageHeader title="Tenants" subtitle="Teams that hold API keys and token quotas.">
        <button className="primary" onClick={() => setCreating(true)}>
          <Plus />
          Add tenant
        </button>
      </PageHeader>
      <ErrorBanner message={error} />

      {tenants && tenants.length === 0 && <Empty icon={Users}>No tenants yet.</Empty>}
      {tenants && tenants.length > 0 && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Tenant</th>
                <th>Status</th>
                <th>Active keys</th>
                <th>Quotas</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {tenants.map((t) => (
                <tr key={t.id}>
                  <td>
                    <div className="strong">{t.display_name || t.slug}</div>
                    <div className="detail mono">{t.slug}</div>
                  </td>
                  <td>
                    <span className={t.enabled ? "badge synced" : "badge pending"}>
                      {t.enabled ? "Enabled" : "Disabled"}
                    </span>
                  </td>
                  <td>{t.key_count}</td>
                  <td>{t.quota_count}</td>
                  <td className="row-actions">
                    <button onClick={() => setOpenId(t.id)}>Manage</button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {creating && <TenantForm onClose={() => setCreating(false)} onCreated={(t) => setOpenId(t.id)} />}
    </>
  );
}

function TenantForm(props: { onClose: () => void; onCreated: (t: Tenant) => void }) {
  const [slug, setSlug] = useState("");
  const [displayName, setDisplayName] = useState("");
  return (
    <FormModal
      title="Add tenant"
      submitLabel="Add tenant"
      onClose={props.onClose}
      onSubmit={async () => props.onCreated(await api.createTenant(slug, displayName))}
    >
      <Field label="ID" hint="Lowercase letters, digits and dashes. It cannot be changed later.">
        <input
          required
          pattern="[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?"
          value={slug}
          placeholder="team-search"
          onChange={(e) => setSlug(e.target.value)}
        />
      </Field>
      <Field label="Display name">
        <input value={displayName} placeholder="Search team" onChange={(e) => setDisplayName(e.target.value)} />
      </Field>
    </FormModal>
  );
}

function TenantDetail(props: { id: string; onBack: () => void }) {
  const { data, error, reload } = useLoad(async () => ({
    detail: await api.tenant(props.id),
    models: await api.models(),
  }));
  // Usage is loaded on its own so that Redis being down never hides the tenant.
  const usage = useLoad(() => api.usage(props.id));
  usePolling(usage.reload, REFRESH_MS);
  const action = useAction();
  const [newKey, setNewKey] = useState<string | null>(null);
  const [keyName, setKeyName] = useState("");

  if (!data) {
    return (
      <>
        <button className="ghost back" onClick={props.onBack}>
          <ArrowLeft />
          Tenants
        </button>
        <ErrorBanner message={error} />
      </>
    );
  }
  const { tenant, keys, quotas } = data.detail;
  const act = (fn: () => Promise<unknown>) => action.run(async () => void (await fn(), await reload()));

  const createKey = (e: FormEvent) => {
    e.preventDefault();
    act(async () => {
      setNewKey((await api.createKey(tenant.id, keyName)).secret);
      setKeyName("");
    });
  };

  const remove = () => {
    if (!confirm(`Delete ${tenant.slug}? Its keys and quotas are removed from every cluster.`)) return;
    action.run(async () => {
      await api.deleteTenant(tenant.id);
      props.onBack();
    });
  };

  return (
    <>
      <button className="ghost back" onClick={props.onBack}>
        <ArrowLeft />
        Tenants
      </button>
      <PageHeader title={tenant.display_name || tenant.slug} subtitle={tenant.slug}>
        <button
          disabled={action.busy}
          onClick={() => act(() => api.updateTenant(tenant.id, tenant.display_name, !tenant.enabled))}
        >
          {tenant.enabled ? "Disable" : "Enable"}
        </button>
        <button className="danger" disabled={action.busy} onClick={remove}>
          Delete
        </button>
      </PageHeader>
      <ErrorBanner message={error || action.error} />
      {!tenant.enabled && (
        <div className="banner warn">This tenant is disabled. Its keys are removed from the clusters until it is enabled.</div>
      )}

      <section className="card">
        <h2>API keys</h2>
        <form className="inline-form" onSubmit={createKey}>
          <input value={keyName} placeholder="Key name, e.g. ci-pipeline" onChange={(e) => setKeyName(e.target.value)} />
          <button type="submit" className="primary" disabled={action.busy}>
            Create key
          </button>
        </form>
        {keys.length === 0 ? (
          <p className="hint">No keys yet.</p>
        ) : (
          <table className="plain">
            <thead>
              <tr>
                <th>Name</th>
                <th>Key</th>
                <th>Client ID</th>
                <th>Created</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {keys.map((k) => (
                <tr key={k.id} className={k.revoked_at ? "muted" : ""}>
                  <td>{k.name || "—"}</td>
                  <td className="mono">{k.key_prefix}…</td>
                  <td className="mono">{k.client_id}</td>
                  <td>{formatTime(k.created_at)}</td>
                  <td className="row-actions">
                    {k.revoked_at ? (
                      <span className="badge pending">Revoked</span>
                    ) : (
                      <button
                        className="danger"
                        disabled={action.busy}
                        onClick={() => confirm("Revoke this key?") && act(() => api.revokeKey(k.id))}
                      >
                        Revoke
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      <TenantQuotas
        tenant={tenant}
        quotas={quotas}
        models={data.models}
        usage={usage.data ?? null}
        busy={action.busy}
        act={act}
        resetUsage={(modelId) =>
          action.run(async () => void (await api.resetUsage(tenant.id, modelId), await usage.reload()))
        }
      />

      {newKey && (
        <Modal title="Key created" onClose={() => setNewKey(null)}>
          <p>Copy this key now. It is not shown again.</p>
          <pre className="code">{newKey}</pre>
          <footer>
            <button className="primary" onClick={() => navigator.clipboard?.writeText(newKey)}>
              <Copy />
              Copy
            </button>
            <button onClick={() => setNewKey(null)}>Done</button>
          </footer>
        </Modal>
      )}
    </>
  );
}
