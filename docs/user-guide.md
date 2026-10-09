# User guide

## Sign in

Open the Route URL and enter the admin token. The token is kept in the
browser until you sign out. The sidebar has a dark mode switch.

**Your name** is optional. Everyone shares the one admin token, so the name is
what tells people apart in the audit log. Nothing checks it.

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
| Client listener | Optional. The Gateway listener clients come in on, for example `https`. The key check then applies to it alone. Set it on a gateway that also has a listener for other sites |
| Peer host, peer port | Optional. Where the other sites reach this gateway, for example `llm.site1-a.example.com` and 8443 |
| Part of the fleet | The cluster shares each model's traffic with the other fleet clusters. Needs Enforce API keys, a client listener and a peer host, and the same gateway namespace as the other fleet clusters |

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
| Self-test | Checks keys, counters and quotas with real requests. See below. Needs a Gateway URL |
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

**Self-test.** Applying an object only proves the cluster stored it. The
self-test proves the gateway acts on it. It adds a temporary tenant with a key
and a quota of 1 token per hour on one model, sends a few one-token requests
through the gateway, and removes the tenant again. It takes about a minute.

| Step | What a pass proves |
|---|---|
| The gateway lists the model | The Gateway URL is right and the model is served |
| A temporary tenant, key and quota are applied | The cluster and the gateway's controller accept the objects |
| A request with the new key is answered | A key issued here works, and the tenant's own quota rule matches its requests |
| A request with an unknown key is refused | "Enforce API keys" is really in effect |
| The tokens are counted where the Usage page reads them | The Usage page looks at the right counters |
| A request over the quota is refused | The quota is enforced |
| A usage reset lets the tenant through again | **Reset usage** works on this gateway |
| A conversation stays on one site | Three requests with one session ID are served by the same site, as named in the `x-llm-served-by` response header. It also lists where ten other session IDs landed. Skipped unless the model has an entry route on this cluster. It raises the temporary quota and sends 13 small requests |
| The cluster has the fleet's current entry routes | The cluster's fleet revision is the fleet's. Skipped outside the fleet |

The counter step also fails when the counter holds twice what the first
request used or more: with an entry route a request passes two gateways, and
only the entry may charge it. It cannot tell for a model with a cost
expression.
| The temporary tenant is removed | Nothing is left behind |

A step is **Unclear** when it cannot give an answer. The usual case: the
model's shared pool still has tokens, so a tenant over its quota is not
refused. That is how the pool is meant to work; pick a model with a small pool
to test a refusal. Steps are **Skipped** when they need something that is off:
API key enforcement, usage monitoring, or `redis.allowReset`.

While it runs, the temporary tenant `selftest-…` is visible on the Tenants
page. Only the tested cluster is synced. If a periodic sync runs in that
minute, other clusters get the tenant too and lose it again at their next
sync. One self-test runs at a time. The result is also written to **Activity**.

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

**Site weights.** Next to each cluster a model is served from, a grey tag
shows the site's zone weight and its share of the model's traffic, for
example "weight 800 · 73% of traffic". The weight is the number of ready
instances of the model on that cluster, times the capacity declared for one
instance, times 100. It is never below 1. Hover over the tag to see the
deployments it was counted from and when.

| You see | Meaning |
|---|---|
| weight 800 | The site has 8 units ready and the gateways are told so |
| weight 500 → 800 (yellow) | The site has 8 ready; the weight is rising one instance per poll |
| weight 800 → 600 (yellow) | The site reported less once. The weight drops if the next poll agrees |
| weight 1 | The site serves the model and has nothing ready. Its health check keeps traffic off it |
| draining · weight 500 → 1 (yellow) | An operator drained the site; the weight goes down one instance per poll |
| drained · not listed (yellow) | The site is out of the model's sites until **Undrain** |
| not listed | The cluster is not part of the fleet, or does not serve the model |
| No tag | The cluster reports no instance count for this model, for example because it does not run KServe |
| "Site weights not updated: …" | The gateways keep the sites and weights they have, and the line says why |

**Fleet revision.** On the Clusters page a fleet cluster shows a tag such as
"fleet · 9b7e129b7e9b". The code is the same on every cluster whose entry
routes are the fleet's current ones. "fleet · outdated" means the cluster
has not been synced since the sites or weights changed; press **Sync** or
check its sync error.

**Entry route.** The button **Entry route: off / on** in a model's row makes
the hub render, on every fleet cluster, a route that takes requests for the
model and sends each conversation to one of the sites that serve it, by the
weights above. It is off by default and appears once a fleet cluster serves
the model. Switching it changes where the model's quotas attach, so the
model's quota counters restart once. Do not switch it on before the clusters
have their peer listeners and certificates; see
[architecture.md](architecture.md#entry-route).

**When a budget is spent.** A model with an entry route has a second
selector in its row. *Refuse* answers 429 to a tenant whose budget is spent,
as before. *Best-effort* keeps answering: the tenant's requests are queued
behind everyone else's and are the first to be dropped when a site is full.
The tenant goes back to normal when its quota's window ends. *Best-effort,
also without a quota* does the same for tenants that have no quota on the
model. The Usage page shows who is served as best-effort right now, how many
tokens that was, and the earlier periods. Read
[what it does and why](how-it-works.md#best-effort-when-a-budget-is-spent)
before turning it on: a tenant can still get 429 for a few seconds.

**Drain a site.** Before maintenance on one site, press **Drain** next to the
cluster in the model's row. The site's weight steps down to 1, then the site
leaves the model's sites, and its conversations move to the other sites. Press **Undrain** afterwards. The last
site with capacity cannot be drained.

**Warnings.** A yellow line under the clusters says when the sites serve
different revisions of the model, or take different request lengths.

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

**Audit log.** The second tab lists every request that changed something or
tried to, from the UI or the API: when, who, what, the result and the address
it came from. "Refused" is a request the server turned down, for example
because a field was invalid. Reads are not listed.

Who is the name given at sign-in, or by an API caller in the
`X-On-Behalf-Of` header. It is not verified: anyone with the admin token can
send any name. Without a name the entry shows "admin token". The log keeps the
last 20,000 entries, and each entry is also written to the server log.

## Architecture

The **Architecture** page shows the design of the whole platform: the hub, the
LLM clusters, how a request travels and how a conversation finds its site. It
is a target design. The bar at the top opens a list of what this tool does
today and what is not built. The same page is in the repository as
`docs/platform-architecture.html`.

## Docs

The **Docs** page shows the guides and the release notes of the version that
is running. Start with *What each action does, and why*: it goes through
every action in the UI and says what happens, how the server does it and why
it was built that way. Links between documents stay inside the page.

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
