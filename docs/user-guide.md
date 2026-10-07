# User guide

## Sign in

Open the Route URL and enter the admin token. The token is kept in the
browser until you sign out. The sidebar has a dark mode switch.

## Clusters

A cluster is one LLM cluster the hub manages.

**Add a cluster**

| Field | Meaning |
|---|---|
| Name | A lowercase name, for example `ocp4-prod-llm-site1-a` |
| Site | Free text, for example `site1` |
| Gateway namespace | The namespace of the gateway. Use the same one on every cluster |
| Gateway name | The name of the `Gateway` object |
| Kubeconfig | Pasted once, stored encrypted, never shown again |
| Gateway URL | Optional. The gateway's address without a path, for example `http://192.168.1.9`. Models are then listed from its `/v1/models` |
| API key for /v1/models | Optional. Only needed when the gateway requires a key. Stored encrypted |
| Enforce API keys | See below |

The kubeconfig must contain the cluster's CA certificate
(`certificate-authority-data`). Without it the connection fails with
"certificate signed by unknown authority".

**Enforce API keys.** When on, the gateway rejects every request that does not
carry a key issued here. This applies to all routes on that gateway, including
ones this tool did not create. Per-tenant quotas need it, because it is what
tells the gateway which tenant a request belongs to. It is off by default so
adding a cluster never breaks existing clients.

**Row buttons**

| Button | What it does |
|---|---|
| Test | Checks the connection, the installed CRDs and the gateway |
| Sync | Applies the current state now |
| Manifests | Shows the YAML a sync applies, with key values masked |
| Edit | Changes settings. Leave the kubeconfig empty to keep the stored one |
| Remove | Forgets the cluster. Objects already applied stay on it |

**Status**

| Status | Meaning |
|---|---|
| Synced | The cluster matches the desired state |
| Pending | Something changed and has not been applied yet |
| Error | The last sync failed. The message is under the badge |

## Models

Models appear on their own. Every minute the hub asks each cluster's gateway
for its model list (when a Gateway URL is set) and reads the routes to find
the backends. **Discover now** runs it immediately.

Each endpoint has a tag:

- **discovered**: the cluster already exposes the model. Nothing is created for it.
- **manual**: you added it here, and the hub creates the route and backend.

A discovered endpoint can also carry a warning:

| Tag | Meaning |
|---|---|
| no quota here | That cluster does not serve the model from an `AIServiceBackend`, so a quota cannot be attached there |
| no quota possible | No cluster can carry a quota for this model. Setting one is refused |
| quota unverified | The route has no `modelNameOverride` for the backend. The gateway only documents quota matching against that field, so test that the quota takes effect |

**Add a model by hand** when a model runs in a cluster but has no route yet.
Enter the name clients send in the `model` field, then tick each cluster and
give the host and port of the model server.

**Pool for tenants without a quota.** Once any tenant has a quota on a model,
tenants without one share this pool. Keep it at 1 token to make a quota
mandatory.

Deleting a model removes its quotas from every cluster. A discovered model
comes back on the next poll, without its quotas.

## Tenants

A tenant is a team.

1. **Add tenant**. The ID cannot be changed later.
2. **Create key**. The key is shown once. Copy it then.
3. **Set quota**. Pick a model, a number of tokens and a window (minute, hour or day).

Clients send the key as `Authorization: Bearer sk-...`.

| Action | Effect |
|---|---|
| Revoke a key | The key stops working after the next sync |
| Disable a tenant | All its keys and quotas are removed from the clusters until it is enabled again |
| Delete a tenant | Removes the tenant, its keys and its quotas |

A quota is one budget for the tenant on that model across all sites.

## Typical first setup

1. Add each LLM cluster and press **Test**.
2. Open **Models** and check that your models were found.
3. Add a tenant, create a key, set a quota.
4. Open **Manifests** on one cluster and read what will be applied.
5. Turn on **Enforce API keys** on one test cluster and send a request with the key.
