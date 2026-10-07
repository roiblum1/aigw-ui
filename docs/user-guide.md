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

**Shared pool.** Once any tenant has a quota on a model, every request also
draws from this pool, and tenants without a quota use only the pool. Keep it
at 1 token to hold everyone strictly to their quota. See
[Letting tenants use what others leave unused](#letting-tenants-use-what-others-leave-unused)
for the other way to use it.

**Cost expression.** By default every token costs the same. An expression
such as `input_tokens + output_tokens * 4u` makes output tokens cost four
times as much against every quota on the model. Number literals need the `u`
suffix. Changing it does not reset what tenants have already used.

Deleting a model removes its quotas from every cluster. A discovered model
comes back on the next poll, without its quotas.

## Tenants

A tenant is a team.

1. **Add tenant**. The ID cannot be changed later.
2. **Create key**. The key is shown once. Copy it then.
3. **Set quota**. Pick a model, a number of tokens and a window (minute, hour or day).
   Tick **Dry run** to count usage against the quota without rejecting
   requests, and press **Enforce** on the quota when you are ready.

Clients send the key as `Authorization: Bearer sk-...`.

| Action | Effect |
|---|---|
| Revoke a key | The key stops working after the next sync |
| Disable a tenant | All its keys and quotas are removed from the clusters until it is enabled again |
| Delete a tenant | Removes the tenant, its keys and its quotas |

A quota is one budget for the tenant on that model across all sites.

## Letting tenants use what others leave unused

The gateway has no setting that moves unused tokens from one tenant to
another. The **shared pool** of a model gives the same result.

Every request is charged to two buckets: the tenant's own quota and the
model's shared pool. It is let through while either of them has tokens left.
So:

| Shared pool | Effect |
|---|---|
| 1 token (the default) | Strict. A tenant stops at its own quota |
| What the model can serve in the window | Tenants can go past their quota while the model as a whole is under that capacity. Once the pool is used up, only tenants with quota left are served |

With the pool set to capacity, a tenant's quota becomes its guaranteed share
and everything nobody is using is open to whoever asks first.

Things to know:

- The pool and the quotas are counted separately and each in its own window.
  Use the same window for both to keep this easy to reason about.
- A tenant without any quota on the model can use the pool too.
- The guarantee holds only while the quotas add up to no more than the pool.
- The **Overview** shows how full each model's pool is.

## Overview charts

With usage monitoring on, the Overview shows the current window live:

| Chart | Shows |
|---|---|
| Allocated and used | The sum of all tenant quotas in the selected window, and how much of it is used |
| Who is using it | Share of the tokens used, by tenant |
| On which model | Share of the tokens used, by model |
| Closest to the limit | The five quotas with the highest percentage used, in any window |
| Shared pool per model | How full each model's shared pool is |
| Tokens per minute | The rate across all tenants, measured while the page is open. Nothing is stored, so it starts empty each time |

Quotas with different windows are never added together; pick the window with
the buttons at the top right.

## Activity

The **Activity** page is the task log. Every change you make, here or through
the API, adds a line: adding a tenant, creating a key, setting a quota,
switching a quota to dry run, resetting usage, and so on.

Open a line to see each cluster and what the sync did there:

| You see | Meaning |
|---|---|
| In progress | The cluster has not synced since the change |
| Succeeded, with a list of objects | The cluster's API server confirmed each object as created, updated or deleted |
| Succeeded, "Nothing had to change" | The cluster already had everything, for example a new tenant without a key or quota |
| gateway: Accepted | The gateway's controller accepted the object |
| Applied, not accepted | The object exists, but the gateway refuses it. The reason is shown |
| Failed | The cluster could not be synced. It is tried again every few minutes, and the line turns to Succeeded when it works |

The log keeps the last 500 tasks.

## Usage

The **Usage** page shows, for every tenant quota, how many tokens were used in
the current window and when the window resets. It refreshes every 5 seconds.
The same bar appears next to each quota on a tenant's page.

| Bar | Meaning |
|---|---|
| Green | Below 80% of the limit |
| Amber | 80% or more |
| Red | The limit is reached. Requests are rejected, unless the quota is a dry run |

The numbers come straight from the counters the gateways keep in Redis, so
they are what the gateways enforce. They cover the current window only: a
day starts at 00:00 UTC and an hour on the hour. There is no history.

**Reset usage** sets a tenant's usage on one model back to zero for the rest
of the current window, on every site. The limit does not change. The button
appears only when `redis.allowReset` is on. A tenant that had already reached
the limit can stay blocked for a while after a reset if the rate limit
service caches over-limit keys locally (`LOCAL_CACHE_SIZE_IN_BYTES`).

The page needs `redis.url` to be set, see [deployment](deployment.md).

## Typical first setup

1. Add each LLM cluster and press **Test**.
2. Open **Models** and check that your models were found.
3. Add a tenant, create a key, set a quota.
4. Open **Manifests** on one cluster and read what will be applied.
5. Turn on **Enforce API keys** on one test cluster and send a request with the key.
