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
and a quota of 1 token per hour on one model, sends a few requests with a
one-token answer through the gateway, and removes the tenant again. It takes
about a minute. Every request has the same prompt of about 300 tokens.

| Step | What a pass proves |
|---|---|
| The gateway lists the model | The Gateway URL is right and the model is served |
| A temporary tenant, key and quota are applied | The cluster and the gateway's controller accept the objects |
| A request with the new key is answered | A key issued here works, and the tenant's own quota rule matches its requests |
| A request with an unknown key is refused | "Enforce API keys" is really in effect |
| The tokens are counted where the Usage page reads them | The Usage page looks at the right counters |
| A request over the quota is refused | The quota is enforced |
| A usage reset lets the tenant through again | **Reset usage** works on this gateway |
| A client cannot choose its tenant | A request that names another client ID in `x-aigw-client-id` is still counted for the tenant of its key. A failure means a client can spend another tenant's budget |
| The model reports cached prompt tokens | The answer to a repeated prompt says how much came from the prefix cache. **Unclear** when the model does not report it, or reports none |
| A long prompt is accepted | A request of 64 KB is not refused for its size. It names a model that does not exist, so no model is asked. A failure means the gateway has Envoy Gateway's default limit of 32 KiB, and every longer prompt gets 413: set `connection.bufferLimit` in a `ClientTrafficPolicy` on the Gateway |
| A conversation stays on one site | Three requests with one session ID are served by the same site, as named in the `x-llm-served-by` response header. It also lists where ten other session IDs landed. Skipped unless the model has an entry route on this cluster. It raises the temporary quota and sends 13 small requests |
| The cluster has the fleet's current entry routes | The cluster's fleet revision is the fleet's. Skipped outside the fleet |
| A budget across models is counted and refuses | The tenant's quota on the model is taken away and it gets a budget of 1 credit across models, which alone holds it there; a long request is answered and its cost in credits counted where the Usage page reads it, and the next request is refused with 429. A failure names what is missing: global rate limiting in Envoy Gateway, a route whose own policy overrides the budget's, the cost on the route, or the way to the shared Redis. Skipped on a model without prices, or when the gateways still take API keys |
| A tenant held to its models is refused for any other | The tenant is held to the model under test alone and is still served it; then to no model at all, and its next request is refused with 403. Skipped when the gateways still take API keys |

| The temporary tenant is removed | Nothing is left behind |

The counter step compares the counter with what the first request should
cost: its total tokens, or the model's cost expression over its token
counts. The gateway counts 1 more than that, for the check before the
request. The step fails in two cases:

- The counter holds 1 and the request cost more. The request is counted and
  its cost is not, so a quota would never be used up.
- The counter holds twice the cost or more. With an entry route a request
  passes two gateways, and only the entry may charge it.

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
| no quota possible | No cluster can carry a quota for this model and it is not a fleet model. Setting one is refused |
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

The hub checks the expression by the gateway's own rules before it saves it,
because the gateway does not refuse a bad one: it logs it and then charges
nothing. The result has to be a whole number.

To charge a prompt answered from the model's prefix cache less, subtract the
cached part first. `input_tokens` includes it:

```
(cached_input_tokens <= input_tokens
  ? 10u * (input_tokens - cached_input_tokens) + cached_input_tokens
  : 10u * input_tokens) + 40u * output_tokens
```

This counts in tenths of a token: a cached prompt token costs a tenth of an
uncached one, and an output token four times as much. Quotas on the model
are then in tenths of a token too. The comparison is there for a cached count
larger than the prompt, which would otherwise be an error and leave the
request uncharged. The model has to report cached tokens for this to have
any effect; the self-test shows whether it does.

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
as before. *Best-effort* keeps answering: the tenant's requests are marked
as the class `best-effort` and counted apart from its budget. Whether they
then wait behind everyone else's depends on the model's scheduler on each
site; with KServe's default settings they do not. See
[optimization.md](optimization.md#what-a-site-does-with-the-class).
The tenant goes back to normal when its quota's window ends. *Best-effort,
also without a quota* does the same for tenants that have no quota on the
model. The Usage page shows who is served as best-effort right now, how many
tokens that was, and the earlier periods. Read
[what it does and why](how-it-works.md#best-effort-when-a-budget-is-spent)
before turning it on: a tenant can still get 429 for a few seconds. How it
compares with the shared pool is in
[using capacity that would sit idle](optimization.md#side-by-side).

**Drain a site.** Before maintenance on one site, press **Drain** next to the
cluster in the model's row. The site's weight steps down to 1, then the site
leaves the model's sites, and its conversations move to the other sites. Press **Undrain** afterwards. The last
site with capacity cannot be drained.

**Warnings.** A yellow line under the clusters says when the sites serve
different revisions of the model, or take different request lengths.

## Deployments

The models on the clusters, deployed by Argo CD from git. Two tables:

- **On the clusters**: each model on each cluster, what it runs on
  (replicas × GPUs, data/tensor/expert parallel, RoCE, LMCache), what this
  cluster sets itself, Argo CD's sync and health, and its ready replicas.
- **Models**: each model's recipe, its chart, and the clusters it runs on.

**Run a model on another cluster in another shape.** Deploy a model,
choose the model and the cluster. The form starts from the recipe and the
site's layers; change only the shape: GPU product, GPUs per pod, and one of

- data-parallel ranks, one port each (B300 with DP8: 8, plus
  `--enable-expert-parallel` in this shape's flags);
- tensor parallel (H200 with TP8: 8);
- over several nodes: data-parallel ranks in all and per pod, expert
  parallel, RoCE and its RDMA network (2 × H200, DP8 TP2 EP16: 8, 4,
  tensor 2, expert on).

Commit and deploy writes only what you changed, into that cluster's file.

**Change a model everywhere.** Edit recipe: images (vLLM and LMCache, the
same build), the model's vLLM flags, LMCache, the S3 path of the weights. A
cluster whose own file sets a value keeps it.

**A new model.** New model, start from the closest model; give it an
operator name (its directory and Application) and a namespace; its object
names (SCC, storage, LMCache engine) are renamed after it. Plan its disks
with the site: two models on the same nodes need their own volume names.

Flags are one per line, one token each (`--speculative-config.method=mtp`),
or `--option='value'` for JSON. Each save is a commit; if someone changed
the same file meanwhile, open it again. Remove asks you to type the model's
operator name.

## Tenants

A tenant is a team.

1. **Add tenant**. The ID cannot be changed later.
2. **Create** a key for each person or tool, shown once:
   - a **personal key** for a person in Claude Code (see below): they never
     handle a token, and a change of the tenant's budget or models reaches
     them with nothing to do;
   - a **token** for a tool that takes a fixed API key. It lasts 180 days
     (`tokens.lifetimeDays`) and carries the tenant's budget and models of
     the day it is created; create a new one after a change, or before it
     expires.
3. **Add quota**. Pick **All models** or a model, an amount (dollars for
   all models and for a model with prices, tokens for any other) and a
   window (minute, hour or day). For a model, tick **Dry run** to count
   usage against the quota without rejecting requests, and press
   **Enforce** on the quota when you are ready. A tenant has one budget for
   all models or quotas per model: **All models** cannot be picked while
   it has a quota, and with all models the form is closed. The list only
   offers models the tenant has no quota on yet.

Clients send the token where they send an API key: `Authorization: Bearer
eyJ...`, or the header `x-api-key`, which is what Anthropic's clients do by
default. The address
stays the same for every model: `/v1/chat/completions` for OpenAI-style
clients and `/anthropic/v1/messages` for Anthropic-style ones.

| Action | Effect |
|---|---|
| Edit a quota | Change the tokens or the window in the quota's own row, then **Save**. What the tenant has used in the current window stays counted against the new limit; a new window starts a new count |
| Show a key | A token or API key in full again, to hand on (since 0.33.0). Each time is in the audit log as `key.reveal`. A personal key cannot be shown: the hub keeps only its hash; create a new one |
| Revoke a key | The key or token stops working after the next sync. A revoked personal key gets no new token, and the tokens it got are refused |
| Disable a tenant | Its keys and quotas are removed from the clusters, and its tokens refused, until it is enabled again |
| Delete a tenant | Removes the tenant, its keys and its quotas |

A quota is one budget for the tenant on that model across all sites.

**All models, or a quota per model.** A tenant is held one way or the
other, never both:

- **All models** (a budget across models): one amount in dollars, any
  amount ($10, $20, $26), for every model with prices together. The tenant
  chooses what to spend it on, and the models' pools do not hold it there.
- **A quota per model**: each with its own amount: $10 on two models and
  $11 on a third is fine. $5 on Opus and $5 on Sonnet are two budgets of
  $5, not one of $10.

While a tenant has one, the hub refuses the other (HTTP 409) and says what
to remove first; with all models that includes a quota on a model without
prices. A tenant that had both before 0.27.0 is marked on its page: remove
its quotas per model. On a model with prices they hold nothing while it has
a budget for all models.

**A tenant with neither.** There is no amount every tenant gets by default
(the standard budget of 0.19 to 0.25 is gone): a tenant has what it is
given. On a model where it has neither a budget across models nor a quota
of its own, it draws from the model's **pool**, which all such tenants
share. A pool of the smallest amount (1), what a new model has, closes the
model to them. While any tenant has a budget across models, a model with
prices gets its `QuotaPolicy` even without a quota, so that its pool holds
the tenants with neither.

**Quotas of their own, with tokens only.** Once every tenant is on tokens,
the tenants with the same quota of their own on a model share one rule on
the gateway, which still counts each of them on its own: a thousand tenants
with a few different amounts take a few rules, not a thousand. The amount
travels in the tenant's tokens, as a budget across models does:

- A change reaches a person with a personal key with its next token, within
  minutes. A token created before the change keeps the old amount until it
  is replaced; the Keys card marks it **older limits**.
- A changed amount starts the tenant's count on that model again.

**A budget across models.** **All models** in a tenant's quotas gives it
one amount in dollars per minute, hour or day for every model with
prices, counted together: a cheaper model spends it more slowly. On those
models it is the tenant's only limit: the tenant has no quota of its own
there, and the model's pool does not hold it. Each such model's
`QuotaPolicy` has one more rule, which lets a token with a budget through
and counts what the tenant used of the model. The gateway refuses the
tenant (HTTP 429) once it is spent, on every model, until the next period. It is enforced by
the gateways themselves: the amount travels in the tenant's tokens, one
rule that every cluster gets holds each tenant to the amount in its token,
and it is counted in the one Redis the gateways share, so a tenant has one
budget wherever its requests land. Models without prices, and models whose
prices are in a dry run, cost nothing from it; once it is spent they are
refused all the same.

- It needs every tenant on tokens, like quotas by amount, and global rate
  limiting enabled in each cluster's Envoy Gateway with the Redis the gateways share
  (see [deployment](deployment.md#before-the-first-budget-across-models)).
  Until then the tenant's page says it is not enforced.
- **A change reaches the gateway with the tenant's next token.** A token
  carries the budget and the list of models the tenant had when it was
  created; the Keys card marks the tokens that carry older ones. Create new
  tokens for the tenant's users and revoke the old ones.
- A new amount holds what the tenant already used in the running period; a
  new period length starts a new count. Another tenant's budget coming or
  going changes nothing for this one. A budget removed and set again in the
  same period comes back with what was used. **Reset usage**, on the Usage
  page and the tenant's page, sets what was used back to 0 for the running
  period on every cluster; the amount stays. Like a quota's reset it needs
  `redis.allowReset`.
- A request is charged once its answer is complete, so a tenant can go
  past its budget by the requests in flight when it is reached, and an
  answer that has started is never cut off.
- A request passes while the budget's counter cannot be read: if a cluster
  cannot reach the shared Redis, the budget does not hold there. The hub is
  not in the path: it signs the tokens and renders the policy, and while it
  is down every budget holds as it is.

**Which models.** **Models** on a tenant's page limits it to some of the
models. Any other is refused by the gateway (HTTP 403) before a model or a
quota is asked, including a model added later, until it is added to the
tenant's list. By default a tenant may call every model. It needs every
tenant on tokens too, and like the budget it travels in the tenant's
tokens: a change reaches the gateway with the tenant's next token.

A list and a budget or quotas go together. A budget for **All models** is
spent on the models in the list that have prices; the others are refused
with 403 before any budget is asked (checked on the lab gateway,
2026-10-17: a tenant with Haiku and Sonnet in its list and a budget for all
models was charged for both, and refused Opus and a model without prices).
A quota can only be on a model in the list: **Add a quota for…** shows the
others as "not in its models", and a model the tenant has a quota on stays
ticked in **Models** until the quota is removed. A quota from before on a
model outside the list is marked "not in its models": remove it.

**Tokens and API keys.** Before 0.18 a key was an API key (`sk-...`) that
every gateway had to list. Now a new key is a token the hub signs, which the
gateways check against the hub's public keys without listing it. **Keys and
tokens** at the top of the Tenants page says what the gateways take now:

1. Until the first token, API keys only, as before.
2. Then both, while any tenant still has an API key. Tenants marked **API
   keys** in the list still have one: give each a token, let it switch its
   clients, then revoke its API keys.
3. Once the last API key is revoked, tokens only: the gateways list no key.

**Personal keys and `aigw-token`.** A personal key is what a person keeps
on their machine; the hub stores only its hash, and the gateways never see
it. The helper `aigw-token` (`clients/aigw-token`, served by the hub at
`/clients/aigw-token`; it needs `sh` and `curl`) trades it at the hub (`POST /api/v1/token`) for a
token that lives 7 days (`tokens.renewedLifetimeHours`), carrying the
tenant's budget and models as they are then, and asks for a new one every
10 minutes. Claude Code and Oh My Pi run it for their tokens. When the
personal key is made, the Tenants page shows the line that sets up both
from the hub, with no internet needed; the person's page `/my-usage` shows
it too (see [the tenants' own page](#the-tenants-own-page)):

```sh
curl -fsSL https://<the hub>/clients/install.sh | sh -s -- https://<the hub>
```

By hand, for Claude Code:

```sh
aigw-token login https://<the hub>     # paste the personal key when asked
```

```json
{
  "apiKeyHelper": "aigw-token",
  "env": {
    "ANTHROPIC_BASE_URL": "https://<the gateway>/anthropic",
    "ANTHROPIC_MODEL": "<a model>",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "<a small model>"
  }
}
```

in `~/.claude/settings.json`. While the gateways take API keys too, a
personal key works from 0.34.0 on; up to 0.33.0 its tokens were refused
until the last API key was revoked. Prompts go to the gateway, never to the hub;
the hub only signs tokens. If it cannot be reached, `aigw-token` goes on
printing the token it has until that expires, so a hub that is down for
less than a week stops nobody. The status-line plugin (`clients/claude-code`)
finds the hub and the key `aigw-token` signed in with.

**Rotate signing key** makes a new key that signs every new token; the old
one keeps working for the tokens it signed until they expire or are revoked.
The same paragraph says how many revoked tokens the gateways refuse until
they expire and, since 0.32.0, in how many spans of their expiry the token
policy holds runs of them. See [security](security.md#tokens).

**How much is left to give.** For a measured model, choosing it in **Add
quota** or editing its budget shows what the model's GPUs serve a day, the
ceiling its overcommit limit allows, what the budgets take and what is left,
and the same with the amount being typed. An hourly budget counts at 24
times its amount. Saving past the ceiling asks you to confirm; nothing is
refused. The **Budgets** card on the Overview shows every model. See
[capacity](capacity.md#budgets-in-dollars).

**Prices.** **Prices** in a model's row sets what a million tokens of the
model cost: input, cached input and output. One price for the model, the
same on every site and GPU. The dialog shows the model's price on
OpenRouter, from a snapshot built into the server, and **Use market prices**
fills it in; any price can be set by hand. It lists the other models'
prices next to the ones being typed, and **market moved** on the Models
page means the market's price changed since the model's was taken from it. The model's quotas and usage are then shown
and entered in dollars, and a prompt answered from the prefix cache costs
less. Prices start at the next 00:00 UTC, and the model starts in dry-run:
counted, nobody refused. A model with prices shows its input price, and
**dry-run** while that is on. See [charging by cost](pricing.md); the
Capacity page can check the prices against what the GPUs cost, which is
optional.

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

The other way to serve a tenant past its quota is best-effort.
[Using capacity that would sit idle](optimization.md) explains both and
compares them.

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

Below it, **Usage over time** shows what was recorded, for the last 2, 7,
30 or 90 days:

| Part | Shows |
|---|---|
| The four figures | The total in that time, the average per day or hour, the highest day or hour, and how much of the total was best-effort |
| The bars | One bar per day, or per hour for 2 days, stacked by tenant or by model. Point at a bar for its numbers |
| The table | Each tenant or model: what it used within its budgets and what as best-effort |

Dollars and tokens are shown one at a time. Days and hours are in UTC, like
the gateways' quota periods. The server reads the counters once a minute,
so new usage appears here about a minute later.
[Charging by cost](pricing.md#usage-over-time) says what the numbers
include.

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

**Upstream** holds the documentation of the projects the platform is built
on, as the image carries it, so it can be read where there is no internet:
Envoy AI Gateway, Envoy Gateway, KServe, vLLM and llm-d, each at the version
it names, in its own folders. A line above each page says where it comes
from, with its license. Links to the project's other pages stay inside the
page; any other link opens the project's site or GitHub in a new tab.

**Filter titles** above the list narrows it to the titles that have the
text, ours and upstream; **Enter** searches the text of every document
instead, and lists the ones that have it, the ones with it in their title
first.

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
day starts at 00:00 UTC and an hour on the hour. Past usage is on the
Overview, under **Usage over time**.

**Reset usage** sets a tenant's usage on one model back to zero for the rest
of the current window, on every site. The limit does not change. The button
appears only when `redis.allowReset` is on. A tenant that had already reached
the limit can stay blocked for a while after a reset if the rate limit
service caches over-limit keys locally (`LOCAL_CACHE_SIZE_IN_BYTES`).

The page needs `redis.url` to be set, see [deployment](deployment.md).

## The tenants' own page

A tenant can see its own budgets and usage without the admin token. It
opens `/my-usage` on the hub's address and signs in with a personal key or
any of its API keys or tokens. The sign-in page of the hub links to it.

| Section | Shows |
|---|---|
| Set up Claude Code and Oh My Pi | One line to paste in a terminal: `curl -fsSL <hub>/clients/install.sh \| sh -s -- <hub>`. It installs `aigw-token` in `~/.local/bin` and signs it in (it asks for the personal key on the terminal), then asks the hub for the gateway's address and the person's models (`GET /api/v1/my/models`) and sets up what is installed. Claude Code: `apiKeyHelper`, `ANTHROPIC_BASE_URL` (the gateway with `/anthropic`), the models (`ANTHROPIC_MODEL` and the alias models, from `config.claudeCode` or by price; a model the person set that they may call is kept), the model's length and longest answer when the hub knows it, and the status line in `~/.claude/settings.json` (everything else is kept, the old file is kept as `settings.json.aigw-backup`), and the `aigw-usage` plugin with `claude plugin`. Oh My Pi: the extension in `~/.omp/agent/extensions`, which adds the person's models as `aigw/<model>` when omp starts and shows the budget, and omp's model roles `default` and `smol` (quick tasks) set to the same two models (a role set to another provider is kept). Last, it checks that the gateway takes the token, and waits up to a minute while the gateways list it. Every file comes from the hub (`/clients/`), so it works with no internet. The panel names the models each tool gets; below it, the same step by step. Open by default after a sign-in with a personal key; with a token or API key it says a personal key is needed. Without `config.gatewayURL` the page says the hub was not told the gateway's address |
| Your budget across models | The tenant's budget across every priced model, when it has one: what is used and left, when the next period starts, and whether a gateway holds the tenant to it |
| Your models | The models the tenant may call, when it is held to some; the budgets and prices below are of those only |
| Your budgets | For each model the tenant has a budget on: what is used and left in the running period, when the next period starts, and whether its requests are served as best-effort or refused |
| Usage over time | Its usage per day for 30 days, or per hour for 2 days, by model |
| Your workload | Its requests and the input, cached and output tokens of their answers, as the gateways counted them: totals, the share of the prompt read from the model's cache, prompt and output tokens per request, what the cache saved at the cached price, tokens, cache share and requests per day or hour, and the same per model. A model whose answers report no token counts, or that served nothing from its cache, is named with what to do |
| Prices | What a million input, cached input and output tokens cost on its priced models |

Prompts and answers are never on it: the gateways count numbers, and the
hub keeps nothing else. The workload needs `redis.tokenCounts` (on by
default) and tenants on tokens; see [deployment](deployment.md#token-counts-on-every-route).

It shows that tenant and no other. Nothing can be changed there. The key
stays in the browser tab until it is closed. Turn the page off with
`config.tenantPage: false`; see [security](security.md#the-tenants-own-page).

The same figures can be shown inside the coding agent: a Claude Code plugin
and an Oh My Pi extension put the budget in the status line and warn before
it is spent. They are in the repository under
[`clients/`](https://github.com/roiblum1/aigw-ui/tree/main/clients).

## Typical first setup

1. Add each LLM cluster and press **Test**.
2. Open **Models** and check that your models were found.
3. Add a tenant, create a key, set a quota.
4. Open **Manifests** on one cluster and read what will be applied.
5. Turn on **Enforce API keys** on one test cluster and send a request with the key.
