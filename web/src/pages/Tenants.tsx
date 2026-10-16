import { useState, type FormEvent } from "react";
import { ArrowLeft, Copy, Plus, Users } from "lucide-react";
import { api, type ApiKey, type SigningKeys, type Tenant } from "../api";
import {
  Empty,
  ErrorBanner,
  Field,
  FormModal,
  Modal,
  PageHeader,
  formatDollars,
  formatTime,
  useAction,
  useLoad,
  usePolling,
  windowLabel,
} from "../components";
import { TenantBudgetCard, TenantModelsCard } from "./TenantBudget";
import TenantQuotas from "./TenantQuotas";
import { REFRESH_MS } from "./Usage";

/** A live token created before the tenant's budget, models or quotas last changed: it carries the old ones. */
function olderLimits(k: ApiKey, t: Tenant): boolean {
  return k.kind === "token" && !k.revoked_at && !expired(k) && !!t.limits_changed_at && new Date(k.created_at) < new Date(t.limits_changed_at);
}

/** The tenants whose ID or display name has the text in it, in any case. */
function shown(tenants: Tenant[], find: string): Tenant[] {
  const f = find.trim().toLowerCase();
  if (!f) return tenants;
  return tenants.filter((t) => t.slug.includes(f) || t.display_name.toLowerCase().includes(f));
}

export default function Tenants() {
  const { data: tenants, error, reload } = useLoad(api.tenants, "tenants");
  const signing = useLoad(api.signingKeys, "signing-keys");
  const [creating, setCreating] = useState(false);
  const [openId, setOpenId] = useState<string | null>(null);
  const [find, setFind] = useState("");

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
      <PageHeader title="Tenants" subtitle="Teams that hold keys and budgets.">
        <button className="primary" onClick={() => setCreating(true)}>
          <Plus />
          Add tenant
        </button>
      </PageHeader>
      <ErrorBanner message={error} />
      {signing.data && tenants && (
        <IdentityPanel signing={signing.data} tenants={tenants} onChanged={signing.reload} />
      )}

      {tenants && tenants.length === 0 && <Empty icon={Users}>No tenants yet.</Empty>}
      {tenants && tenants.length > 0 && (
        <div className="inline-form">
          <input
            type="search"
            aria-label="Find a tenant"
            placeholder="Find a tenant by ID or name"
            value={find}
            onChange={(e) => setFind(e.target.value)}
          />
          {find && (
            <span className="detail self-center">
              {shown(tenants, find).length} of {tenants.length}
            </span>
          )}
        </div>
      )}
      {tenants && tenants.length > 0 && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Tenant</th>
                <th>Status</th>
                <th>Active keys</th>
                <th>Quotas</th>
                <th>Budget across models</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {shown(tenants, find).map((t) => (
                <tr key={t.id}>
                  <td>
                    <div className="strong">{t.display_name || t.slug}</div>
                    <div className="detail mono">{t.slug}</div>
                    {t.models && (
                      <div className="detail" title="The tenant may call only the models listed on its page.">
                        {t.models.length === 1 ? "1 model" : `${t.models.length} models`}
                      </div>
                    )}
                  </td>
                  <td>
                    <span className={t.enabled ? "badge synced" : "badge pending"}>
                      {t.enabled ? "Enabled" : "Disabled"}
                    </span>
                  </td>
                  <td>
                    {t.key_count}
                    {t.api_key_count > 0 && (
                      <span
                        className="tag warn"
                        title="Still on API keys: while any tenant has one, every gateway lists every key and token. Give it a token and revoke the API keys."
                      >
                        {t.api_key_count} API {t.api_key_count === 1 ? "key" : "keys"}
                      </span>
                    )}
                  </td>
                  <td>{t.quota_count}</td>
                  <td>{t.budget ? `${formatDollars(t.budget.credits)} per ${windowLabel[t.budget.window]}` : "—"}</td>
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

const identityText: Record<SigningKeys["identity"], string> = {
  keys: "The gateways take API keys only. The first token you create moves every gateway to take tokens too.",
  both: "The gateways take API keys and tokens, and list every key and token, until the last API key is revoked.",
  tokens: "The gateways take tokens only, checked against the hub's public keys; they list no key.",
};

/** How the gateways tell tenants apart, and the hub's signing keys. */
function IdentityPanel(props: { signing: SigningKeys; tenants: Tenant[]; onChanged: () => void }) {
  const { signing, tenants } = props;
  const action = useAction();
  const onKeys = tenants.filter((t) => t.api_key_count > 0);
  const active = signing.keys.find((k) => !k.retired_at);
  const retired = signing.keys.filter((k) => k.retired_at && k.tokens > 0);
  const rotate = () => {
    if (
      !confirm(
        "Make a new signing key? It signs every token from now on. Tokens the current key signed keep working until they expire or are revoked; " +
          "the gateways keep its public key until then.",
      )
    )
      return;
    action.run(async () => void (await api.rotateSigningKey(), props.onChanged()));
  };
  return (
    <section className="card">
      <h2>Keys and tokens</h2>
      <p>{identityText[signing.identity]}</p>
      {onKeys.length > 0 && signing.identity !== "keys" && (
        <p className="warn-text">
          Still on API keys: {onKeys.map((t) => t.slug).join(", ")}. Give each a token, then revoke its API keys.
        </p>
      )}
      <p className="detail">
        {active ? (
          <>
            Tokens are signed by key <span className="mono">{active.id}</span> since {formatTime(active.created_at)}, last{" "}
            {signing.lifetime_days} days, and name <span className="mono">{signing.issuer}</span> for{" "}
            <span className="mono">{signing.audience}</span>.
          </>
        ) : (
          <>No token was issued yet; the first one makes the signing key.</>
        )}
        {retired.length > 0 &&
          ` Retired keys still published for their tokens: ${retired.map((k) => `${k.id} (${k.tokens})`).join(", ")}.`}
        {signing.denied > 0 &&
          (signing.denied === 1
            ? " 1 revoked token is refused until it expires."
            : ` ${signing.denied} revoked tokens are refused until they expire.`)}
      </p>
      <ErrorBanner message={action.error} />
      {active && (
        <button disabled={action.busy} onClick={rotate}>
          Rotate signing key
        </button>
      )}
    </section>
  );
}

/** Which key a row is: an API key's start, a token's or a personal key's end. */
function keyText(k: ApiKey): string {
  return k.kind === "key" ? `${k.key_prefix}…` : `…${k.key_prefix}`;
}

/** A token past its expiry. A personal key does not expire. */
function expired(k: ApiKey): boolean {
  return k.kind === "token" && k.expires_at != null && new Date(k.expires_at).getTime() <= Date.now();
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
  const { data, error, reload } = useLoad(async () => {
    const [detail, models, signing] = await Promise.all([api.tenant(props.id), api.models(), api.signingKeys()]);
    return { detail, models, signing };
  }, "tenant/" + props.id);
  // Usage is loaded on its own so that Redis being down never hides the tenant.
  const usage = useLoad(() => api.usage(props.id), "usage/" + props.id);
  usePolling(usage.reload, REFRESH_MS);
  const action = useAction();
  const [newKey, setNewKey] = useState<{ secret: string; key: ApiKey } | null>(null);
  const [keyName, setKeyName] = useState("");
  // Every hook comes before the return below: the first render, without
  // data, must call the same hooks as the next one.
  const [keyKind, setKeyKind] = useState<"personal" | "token">("personal");

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
      setNewKey(await api.createKey(tenant.id, keyName, keyKind));
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
        <div className="banner warn">
          This tenant is disabled. The gateways refuse its keys and tokens until it is enabled.
        </div>
      )}

      <section className="card">
        <h2>Keys</h2>
        <p className="hint">
          A <strong>personal key</strong> is for a person in Claude Code: its helper trades the key at the hub for a
          token that lives a few days, and gets a new one every few minutes, so a change of budget or models reaches the
          person with nothing to do. A <strong>token</strong> is for a tool that takes a fixed API key; it carries the
          budget and models of the day it is created. Revoking either refuses it on every gateway after the next sync.
        </p>
        <form className="inline-form" onSubmit={createKey}>
          <input value={keyName} placeholder="Key name, e.g. dana-laptop" onChange={(e) => setKeyName(e.target.value)} />
          <select aria-label="Kind" value={keyKind} onChange={(e) => setKeyKind(e.target.value as "personal" | "token")}>
            <option value="personal">Personal key</option>
            <option value="token">Token</option>
          </select>
          <button type="submit" className="primary" disabled={action.busy}>
            Create
          </button>
        </form>
        {keys.length === 0 ? (
          <p className="hint">No keys yet.</p>
        ) : (
          <table className="plain">
            <thead>
              <tr>
                <th>Name</th>
                <th>Kind</th>
                <th>Key</th>
                <th>Client ID</th>
                <th>Created</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {keys.map((k) => (
                <tr key={k.id} className={k.revoked_at || expired(k) ? "muted" : ""}>
                  <td>{k.name || "—"}</td>
                  <td>
                    {k.kind === "personal" ? (
                      <>
                        Personal key
                        <div className="detail">{k.renewed_at ? `last token ${formatTime(k.renewed_at)}` : "no token yet"}</div>
                      </>
                    ) : k.kind === "token" ? (
                      <>
                        Token
                        <div className="detail">expires {formatTime(k.expires_at)}</div>
                      </>
                    ) : (
                      <span className="tag warn" title="An API key of the kind before tokens">
                        API key
                      </span>
                    )}
                  </td>
                  <td className="mono">{keyText(k)}</td>
                  <td className="mono">{k.client_id}</td>
                  <td>
                    {formatTime(k.created_at)}
                    {olderLimits(k, tenant) && (
                      <div>
                        <span
                          className="tag warn"
                          title="The token carries the budget, the list of models and the amount of each quota the tenant had when it was created. Create a new token and revoke this one."
                        >
                          older limits
                        </span>
                      </div>
                    )}
                  </td>
                  <td className="row-actions">
                    {k.revoked_at ? (
                      <span className="badge pending">Revoked</span>
                    ) : expired(k) ? (
                      <span className="badge pending">Expired</span>
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
        tokensOnly={data.signing.identity === "tokens"}
        quotas={quotas}
        models={data.models}
        usage={usage.data ?? null}
        busy={action.busy}
        act={act}
        resetUsage={(modelId) =>
          action.run(async () => void (await api.resetUsage(tenant.id, modelId), await usage.reload()))
        }
      />
      <TenantBudgetCard
        tenant={tenant}
        models={data.models}
        usage={usage.data ?? null}
        identity={data.signing.identity}
        busy={action.busy}
        act={act}
        resetBudget={() => action.run(async () => void (await api.resetBudget(tenant.id), await usage.reload()))}
      />
      <TenantModelsCard tenant={tenant} models={data.models} identity={data.signing.identity} busy={action.busy} act={act} />

      {newKey && (
        <Modal
          title={newKey.key.kind === "personal" ? "Personal key created" : newKey.key.kind === "token" ? "Token created" : "Key created"}
          onClose={() => setNewKey(null)}
        >
          <p>
            Copy it now. It is not shown again.{" "}
            {newKey.key.kind === "token" && (
              <>
                The client pastes it as its API key: <span className="mono">Authorization: Bearer …</span> or{" "}
                <span className="mono">x-api-key</span>. It expires {formatTime(newKey.key.expires_at)}.
              </>
            )}
            {newKey.key.kind === "personal" && (
              <>
                The person signs in once with <span className="mono">aigw-token</span> (in the repository's{" "}
                <span className="mono">clients/aigw-token</span>) and names it as Claude Code's{" "}
                <span className="mono">apiKeyHelper</span>; see the user guide.
              </>
            )}
          </p>
          <pre className="code">{newKey.secret}</pre>
          {newKey.key.kind === "personal" && (
            <pre className="code">{`aigw-token login ${window.location.origin}
(paste the key when it asks: not on the command line, where the shell keeps it)

~/.claude/settings.json:
{
  "apiKeyHelper": "aigw-token",
  "env": { "ANTHROPIC_BASE_URL": "<the gateway's address>" }
}`}</pre>
          )}
          <footer>
            <button className="primary" onClick={() => navigator.clipboard?.writeText(newKey.secret)}>
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
