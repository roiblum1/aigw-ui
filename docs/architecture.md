# Architecture

This document describes how this tool works today. For the design of the
whole platform, including routing between sites, see
[platform-architecture.md](platform-architecture.md).

## Components

| Component | Runs on | Role |
|---|---|---|
| API server (Go) | hub | Serves the UI and the REST API, runs the sync and discovery loops, and reads the usage counters |
| UI (React) | hub, inside the server image | Static files served by the API server |
| Postgres | hub | Source of truth: clusters, models, tenants, keys, quotas. Also the usage history |
| Envoy AI Gateway | each LLM cluster | Serves the traffic. Not installed by this tool |
| Redis | hub | Shared usage counters of the gateways' rate limit service. Not installed by this tool. The server reads it and, when resets are allowed, deletes a counter |

The server is stateless. All state is in Postgres, so the pod can be restarted
or replaced at any time.

## Two loops

**Discovery (clusters to hub).** Every `DISCOVERY_INTERVAL` (default 60s), and
when a cluster is added, the server asks each cluster which models it serves.
There are two modes, chosen per cluster by whether a Gateway URL is set.

*With a Gateway URL (recommended).* Two sources are combined:

1. `GET <gateway url>/v1/models` gives the list of model names. This is the
   gateway's own answer, so it covers routes in every namespace and every
   backend type.
2. A scan of the `AIGatewayRoute` objects in all namespaces that are attached
   to the cluster's gateway gives, for each name, the `AIServiceBackend`s it
   is served from. A quota needs these: a `QuotaPolicy` can only target an
   `AIServiceBackend`.

A model that the gateway lists but that has no `AIServiceBackend` (for example
one served from an `InferencePool`) is shown with "no quota possible", and the
API refuses a quota on it. A fleet model is the exception: its quota attaches to
the entry backend the hub renders, so its sites may serve it from a pool only.

If either source fails, the poll fails and nothing is changed. Saving the
names without their backends would remove quota policies already in place.

*Without a Gateway URL.* Only the route scan runs, and only in the cluster's
gateway namespace.

In both modes:

- A rule exposes a model when it matches the `x-ai-eg-model` header exactly. Regex and prefix matches have no single model name and are ignored.
- The same model name on several clusters is one model with one endpoint per cluster.
- A model that disappears from a cluster loses that endpoint. The model and its quotas are kept.
- Routes created by this tool are ignored.

**Two names for one model.** If a gateway exposes the same model under two
names, such as `glm-5.3` and `publishers/llm-glm53/models/glm-5.3`, they are
two models here, each with its own quota. A tenant with a quota on both has
two separate budgets for what is really one model. Setting
`excludeFromModelsEndpoint` on the alias rule hides it from `/v1/models` and
therefore from this tool.

**Sync (hub to clusters).** After every change, and on the Sync buttons, the
server renders the desired objects for each cluster from Postgres and applies
them with server-side apply (field manager `aigw-ui`). It then deletes objects
it created earlier that are no longer wanted.

The objects go in five stages, each stage's eight at a time:

1. the tenants' key `Secret`;
2. the `SecurityPolicy`, which names it: with these two first, a revoke or a
   disabled tenant is in effect at the start of a sync, not at its end;
3. what routes and policies name: `Backend`, `AIServiceBackend` and the
   request classes;
4. the `AIGatewayRoute`s;
5. the policies that attach to routes and backends: `QuotaPolicy`,
   `BackendTrafficPolicy`, `EnvoyPatchPolicy`.

A stage that fails stops the sync before the next one. The routes of the
cluster's own are marked before any of it (see below), each namespace's
routes listed once. The leftovers of every kind are listed at once and
deleted in the order of `kube.managed`. The task log lists the objects in the
order they were rendered, whatever order they were written in.

Each cluster has one client, kept while its kubeconfig stays the same. The
sync, the polls and the measurements of a cluster share its connections and
its rate: at most 50 requests a second after a burst of 100. Up to 0.30.0 a
new client was made for every sync and poll, at client-go's default of 5
after 10, and a sync of the lab's few models took 10 to 13 s. Polls run on a
loop of their own, so a change never waits for a poll.

Every `SYNC_INTERVAL` (default 5 minutes) every cluster is synced again even
if nothing changed. This retries a cluster whose last sync failed and puts
back an object that someone edited or deleted by hand. Since 0.32.0 the hub
also syncs every cluster as soon as it starts, so what changed while it was
down reaches the clusters at once; up to 0.31.0 the first sync after a
restart came with the first resync, 5 minutes later (0.31.0 run).

## What is applied to a cluster

| In the UI | Objects on the cluster |
|---|---|
| Model found by discovery | none; the existing route and backends are left alone |
| Model added by hand | `Backend`, `AIServiceBackend`, `AIGatewayRoute` |
| Tenant quota | one `QuotaPolicy` per model in each namespace that holds one of the model's `AIServiceBackend`s |
| Tenant API keys, with "Enforce API keys" on | one `Secret` (`aigw-ui-api-keys`) and one `SecurityPolicy` on the gateway |
| Tenant budgets across models | one `BackendTrafficPolicy` (`aigw-ui-tenant-budgets`) on the gateway, with one rule for every tenant; each tenant's amount is in its tokens |
| A tenant held to some models | one rule per such tenant in the `SecurityPolicy` |

The Secret holds one entry per active key, the client ID and the key, under
`data`. It must not be written through `stringData`: server-side apply cannot
remove an entry that came in that way, and the gateway trusts every entry. As
a second guard, each sync compares the Secret on the cluster with the keys it
should hold and removes any other entry.

All objects carry the label `app.kubernetes.io/managed-by: aigw-ui`. They are
created in the cluster's gateway namespace, except a `QuotaPolicy`, which has
to be in the same namespace as the backends it targets.

**Ownership rule.** The server only ever deletes an object that carries that
label. The label is checked on the hub as well as in the API query, so a
cluster's own routes cannot be removed by a sync.

## How a tenant quota is enforced

1. A key has the client ID `<tenant>.<random>`, for example `team-search.5018d00f`. A token's is `<tenant>.<jti>`, 16 hex digits, and is in the token as `cid`.
2. The gateway's API key auth validates the key and forwards the client ID in the `x-aigw-client-id` header. A token is checked by the gateway's JWT provider against the hub's public keys: while any tenant has an API key, every token is listed as one too and the API key auth writes its client ID; once none has, the JWT provider writes it from `cid`. Either way it also writes `x-aigw-tenant` (`sub`) and `x-aigw-key` (`jti`). See [security](security.md#tokens).
3. While the gateways take API keys, the model's `QuotaPolicy` has one bucket rule per tenant that matches `^<tenant>\.[a-f0-9]+$`, so all keys of a tenant share one bucket.
   Once they take **tokens only**, the tenants' own quotas take **one rule per amount** instead: the tenants with the same limit, window and dry-run setting on a model share a rule that matches the model's `x-aigw-quota-<model>` header exactly (`500000-1h`) and `x-aigw-tenant` with `type: Distinct`, which counts each of them in a bucket of its own. The token names the amount of each of the tenant's own quotas (claim `quotas`, by model slug), and the JWT provider copies it to that header; it takes a client's own value of the header out first and writes nothing when the token names none, so a tenant cannot pick an amount. The rule keeps its position while any tenant has the amount (table `quota_amounts`, kept by a trigger on `quotas`), so a tenant moving to an amount that is there changes no object on the gateways: only its next token does. There is no rule for every tenant: until 0.26.0 a model's **standard budget** was one, `x-aigw-tenant` with `type: Distinct` first, which a tenant's own quota only added to. A tenant with neither a quota nor a budget across models draws from the model's default bucket, the pool.
4. The gateway's quota rate limit service counts tokens in Redis. Under an amount's rule the key of a tenant's bucket holds the rule's position, the amount and the slug (`…_rule-0-x-aigw-quota-<model>|500000-1h-match-0_…_rule-0-x-aigw-tenant-match-1_<tenant>_<window start>`); under the rule for budgets across models the header and the slug (`…_rule-3-x-aigw-budget|.+-match-0_…_rule-3-x-aigw-tenant-match-1_<tenant>_<window start>`). `render.Counters` names them, so the Usage page, the best-effort mover, usage reset and the history read them as before. A tenant whose amount changes is counted under the new amount from its next token: its count starts again.
5. The cost of an answer is charged when the answer ends, by a second rule that reads the request's headers again (Envoy Gateway's `ApplyOnStreamDone`). A backend that removes a header removes it from those same headers. So the backends the hub renders remove the credentials only (`authorization`, `x-api-key`, while the gateways take tokens), and `x-aigw-tenant`, `x-aigw-key`, `x-aigw-client-id`, `x-aigw-budget` and `x-aigw-quota-<model>` reach the model server. Up to 0.29.0 the backend of a model added by hand removed the first four: each answer counted 1 under a tenant's quota and nothing under its budget, while the pool, which reads no tenant, counted the cost (seen on the lab, AI gateway 1.2.0, Envoy Gateway 1.9.2).

`QuotaPolicy` runs in `Shared` mode: a request passes if any matching bucket
has quota left, and it is also charged to the model's default bucket. The
default bucket is therefore a shared pool for tenants that have no quota of
their own. It defaults to 1 token per day, which in practice makes a quota
mandatory once a model has any quota.

**The `serviceQuota` field.** Every `QuotaPolicy` is written with a
`serviceQuota` of 4,294,967,295 tokens per second. The gateway does not
enforce this field. It is set because the gateway's controller otherwise
writes the object back with an empty `serviceQuota`, which its own CRD
rejects, and logs "Failed to add finalizer" on every reconcile. The value is
high enough to mean "no limit" if the field is enforced later.

## One budget across sites

Counters add up across sites when all of these hold:

- every cluster's quota rate limit service uses the same Redis (on the hub);
- the gateway namespace is the same on every cluster;
- the `AIServiceBackend` for a model has the same name on every cluster.

For models added by hand the tool guarantees the names. For discovered models
the names are yours.

## A budget across models

A tenant's budget across every priced model is enforced by the gateways, not
by the hub (`internal/render/budget.go`), since 0.22.0. The hub only writes
the amounts into manifests; while it is down every gateway keeps enforcing
what it last applied.

1. Every route of a priced model carries `llmRequestCosts` with the key
   `aigw_ui_credits`: the model's price expression, which the AI gateway
   evaluates on each answer and writes to the request's dynamic metadata
   under `io.envoy.ai_gateway`. Cached input tokens are charged at the cached
   price on the OpenAI path and on the Anthropic path (the latter needs the
   gateway's patch `0002`; stock AI Gateway 1.2.0 reports no cached tokens
   there and charges them as input). The hub writes the expression on the
   routes it renders; on a route a cluster's own chart renders it is put
   there by the sync, next to the quota revision, from the annotations
   `aigw-ui.io/credits-expression` and `aigw-ui.io/credits-model` of the
   model's `QuotaPolicy`. A route of the cluster's own that serves several
   priced models gets one expression that charges each model its prices
   (`model == 'a' ? (…) : model == 'b' ? (…) : 0u`). Other costs the route has
   stay. A model whose prices are in a dry run writes no cost: it refuses
   nobody for them.
2. The amount is in the tenant's tokens. When the hub signs a token it
   writes the tenant's budget as two claims, `requests_per_unit` (credits)
   and `unit` (`MINUTE`, `HOUR` or `DAY`, as Envoy names its rate limit
   units), and its list of models, when it has one, as `models`. The
   gateway's JWT filter checks the token and puts its claims into the
   request's metadata (`envoy.filters.http.jwt_authn`, under the provider
   `aigw-hub`), and copies `requests_per_unit` into the header
   `x-aigw-budget`, which a client cannot set.
3. One `BackendTrafficPolicy`, `aigw-ui-tenant-budgets`, on the client
   listener, with one global rate limit rule for every tenant: it matches a
   request with `x-aigw-budget` (a token with a budget), counts per tenant
   (`Distinct` on `x-aigw-tenant`), takes its limit from the token's claims
   (`limit.fromMetadata`), and is charged nothing for the request and the
   credits metadata for the answer. It is `shared: true`, one bucket across
   every route of the listener; without it Envoy Gateway keys the bucket by
   route, which is by model (seen on the lab, 2026-10-16). Envoy Gateway
   puts the same limit on the charge after the answer, so both count in the
   same window. When the limit cannot be read from the token the rule's own
   applies, the most the gateway counts, so the budget does not hold, as
   when the shared Redis cannot be reached: with an Envoy Gateway before
   1.9.0, whose CRD drops `fromMetadata`, or a unit Envoy does not know,
   which the hub never signs. The self-test's budget step then fails. This
   is the upstream
   way to give each tenant its own number without a rule per tenant (Agent
   Router, formerly Envoy AI Gateway, "Per-Tenant Limits Without Per-Tenant
   Policies"; their example has `ext_authz` return the number, which puts a
   service in every request: here the token carries it).

   The counter's name is the policy, the rule, the tenant and the window
   start (`render.BudgetCounters`): nothing of the cluster and nothing of
   the amount. So every cluster counts a tenant in the same key of the Redis
   the gateways share; a token with a new amount counts on in the same key,
   and so does a budget removed and set again in the same period; a new
   window starts a new count. A reset deletes the tenant's key of the
   running window in that Redis (`POST /tenants/{id}/budget/reset`, behind
   `redis.allowReset` like a quota's).

   A change of budget reaches the gateway with the tenant's next token:
   tokens signed before it carry the amount from before. The tenant's page
   marks them (`limits_changed_at`).
4. The policy is merged with the policy of each entry and best-effort route
   (`mergeType: StrategicMerge` on those). A policy on a route otherwise
   replaces the listener's on that route: Envoy Gateway reports the budget
   policy `Overridden`, and every request through an entry route went
   uncharged (lab, 2026-10-16). The sync reports `Overridden` as a rejection
   with Envoy Gateway's message, which names the routes; a chart's own route
   with its own `BackendTrafficPolicy` needs the same `mergeType`.
5. Envoy Gateway's global rate limit service asks Redis before each request
   whether the bucket has room, and charges it when the stream ends. Once it
   is spent the next request is refused (HTTP 429) on every route of the
   listener, priced or not. The request that spends it is answered in full,
   so a tenant can end one request over its amount, more with requests in
   parallel.
6. On a model with prices the budget is the tenant's only limit (0.25.0): a
   tenant has a budget across models or quotas of its own, never both (on
   a model without prices either, since 0.27.0), and the hub refuses either
   while the other is there (HTTP 409). The model's `QuotaPolicy` ends with one more rule, which matches
   `x-aigw-budget` (`RegularExpression` `.+`) and `x-aigw-tenant`
   (`Distinct`) and holds 4,294,967,295 a day: in `Shared` mode a request
   passes while any bucket it matches has room, so a token with a budget is
   not held by the model's pool or a quota it had from before. Its bucket
   per tenant is what the tenant used of the model, which the Usage page and
   the history read (a day since 0.26.0; a minute in 0.25.0).
   The rule has a position of its own (`models.budget_pass_slot`, 0.32.0)
   that never moves, as an amount's does not: each tenant's counter under it
   is named after it. Up to 0.31.0 it was the last rule and moved whenever an
   amount was added after it or the last one was freed. What a tenant used
   under the old position since the history's last look, up to a minute's,
   never reached the history, and the Usage page showed the model's use start
   again from 0 (0.31.0 run: 5 of a tenant's 10 requests that day). The
   upgrade keeps each model's rule where it was, after its amounts; a new
   model's is position 0, and its first amount takes 1. A new amount takes
   the lowest free position other than this one, so it may go after the
   rule. The pool's counter is named after the number of rules, so its
   count starts again when that number changes. It is rendered while an
   enabled tenant has a budget, on a model whose prices charge the budgets
   (not in a dry run), and such a model then gets its `QuotaPolicy` even
   without a quota, so that its pool holds the tenants with neither. The tenant's counters there only count (`Counter.Budget`): the
   Usage page shows what it used inside its budget, and the best-effort
   mover never moves it. It is one more key in the same call to the quota's
   rate limit service, not another round trip.

It needs tokens only, as quotas by amount do: the rule reads the tenant header.
And it needs global rate limiting enabled in the Envoy Gateway configuration
of every cluster, with the shared Redis and a timeout that reaches it (the
default of 20 ms does not across a network: every call is cancelled and the
request passes). The self-test checks the whole path.

What it costs per request is one round trip to that Redis, in series with
the per-model quota's: measured on the lab with a mock model, 4 ms through
the gateway alone, 37 ms with either the budget or the quota, 69 ms with
both, for a Redis about 33 ms away; the model rule adds nothing measurable.
Up to 1,700 requests a second the rate limit services kept that pace. So the
Redis belongs near the gateways (see [deployment](deployment.md)).

How far it scales. The policy and the models rule are the same size for a
thousand tenants as for one, and the hub changes neither when a budget
changes: only the tokens do. With a rule per tenant, as 0.22 rendered it,
Envoy Gateway wrote every rule into every route of the listener, twice,
about 1.2 KB per tenant per route; it took at most 256 rules, and the
configuration a change pushed grew with them (3.2 MB of routes and 11 to 19
seconds per change at 256 tenants on a listener of 10 routes, measured on
the lab; see [performance](performance.md#many-tenants)). The time a request
takes did not depend on the number of tenants either way.

A tenant held to some models has the list in its tokens, and one `Deny`
rule in the `SecurityPolicy` holds every such tenant to its own: a CEL
expression that refuses a request whose `x-ai-eg-model`, which the AI
gateway sets from the request body before the authorization filter runs, is
not in the token's `models`. A token without the claim may call every model;
an empty list refuses every model. The gateway counts an error in a CEL
expression as no match, which for a `Deny` rule would let the request
through, so every lookup in it is guarded. A model not in the list is
refused with 403 before any quota is asked. `GET /v1/models` is answered by
the AI gateway itself and is not refused; it lists every model.

## Task log

Each change is stored as a task with one pending result per cluster. When a
cluster is synced, the sync first notes which tasks are open for it, applies
the desired state, and then writes the outcome on those tasks.

For every object the sync applies it and compares it with the object before:
an object that was not there is "created", one whose generation moved is
"updated", one that is the same is not listed. The object before is the one
the cluster's client remembers from its last sync (its UID and generation);
only the first sync after the server starts reads each object first. An
object someone else changed since is listed as updated once, and one
someone deleted as created. After a change the sync waits two seconds and
reads the status of the changed AI gateway objects, eight at a time, to
record whether the gateway's controller accepted them; an object it did not
change has its status in the answer to the apply. That reading is
informational: it never fails a sync.

On a local API server with 20 models in best-effort mode (222 objects), a
sync that changes nothing took 4.6 s and one that creates everything 11.1 s;
0.30.0 took 2 min 11 s and 2 min 9 s.

## Site weights

For each model, the gateways split conversations between the sites by zone
weights in a `BackendTrafficPolicy`. This tool keeps those weights in line
with what each site can serve.

**The number.** A site's capacity for a model is

```
sum over the model's LLMInferenceServices on that cluster of
    ready instances x capacity of one instance
```

- *Ready instances* come from the workload that `status.workloads` of each
  `LLMInferenceService` names: the Deployment's available replicas, or the
  LeaderWorkerSet's ready groups for a multi-node deployment, read from that
  object. A multi-node instance therefore counts once, however many nodes it
  spans. The desired count is the object's `spec.replicas` too, which an
  autoscaler sets when the service has `spec.scaling` and no replicas of its
  own. KServe 0.21 copies the same counts into `status.workloads`, but only
  when the rest of its reconcile succeeds, and it bumps `observedGeneration`
  whatever happens (knative `PostProcessReconcile`), so after a router error
  the copy is old and nothing says so. Without get on Deployments or
  LeaderWorkerSets the copy is used. Only `v1alpha2` has `status.workloads`;
  a cluster that serves `v1alpha1` alone reports no ready count.
- `serving.kserve.io/stop` stops a service in any case of `true`, as KServe
  reads it (`strings.EqualFold`).
- With separate prefill and decode, an instance is a decode replica together
  with its share of the prefill replicas, in the ratio the spec asks for. Four
  decode and two prefill replicas are four instances; with one prefill replica
  down there is prefill for two.
- *Capacity of one instance* cannot be counted: a multi-node prefill/decode
  instance is one instance and can be worth several single-node ones. It is
  declared on the `LLMInferenceService` in the annotation
  `aigw-ui.io/capacity-per-instance`, as any number in a unit that is the same
  for that model everywhere, for example tokens per second from a benchmark.
  Without the annotation an instance counts as 1.
- *Measured instead of declared.* Once every LLMInferenceService of a model
  that has a ready instance, on every site, has a result that counts (see
  [capacity](capacity.md)), the capacity of one instance is what that LLMI
  was measured to serve, in thousands of input-equivalent tokens a second,
  and the annotation is not used for that model. A model is all measured or
  all declared, and counted with one version of the benchmark profile, so
  its sites' numbers are in one unit. An LLMI that is not measured yet puts
  the model back on the annotation until it is (`internal/llmi`); one whose
  spec changed keeps its last result, marked stale, so tuning a model does
  not switch its weights back and forth. A change from one to the other is applied at once
  rather than one instance per poll, because it changes the unit. The
  Models page marks a site's weight "measured". The older name
  `aigw-ui.io/capacity-per-replica` is read when the newer one is not set,
  and the Models page says so. The number must be from 0.01 to 10000;
  anything else counts as 1, with a note on the Models page.
- Every `LLMInferenceService` of the model on the cluster is counted, in any
  namespace. Set `aigw-ui.io/ignore: "true"` on one that the gateway does
  not route to, such as a canary, to leave it out.
- A deployment that has not reported a ready count next to one that has adds
  nothing. The model is "unknown" on a site only while none of its
  deployments has reported.

**Which sites.** The sites are the clusters marked *Part of the fleet*. A
cluster serves a model when it has an `LLMInferenceService` for it;
`/v1/models` is not used for this, because once the entry route exists every
gateway lists every fleet model. A model's sites are the fleet clusters that
serve it and are not drained out.

**From capacity to weight.** The capacity is read on every discovery poll.
The applied capacity follows it by these rules (`internal/weights`):

| Observed | What is applied |
|---|---|
| The first reading | The reading itself |
| More than applied | At most one instance more per poll, so a site that comes back gets its conversations back gradually |
| Less than applied | Nothing on the first poll; the lower value once a second poll in a row agrees |
| The cluster no longer has a deployment of the model | The same as "less than applied", down to 0. The site then leaves the model's sites |
| Unknown: cluster unreachable, or no status yet | The last value is kept. Unknown is never zero |
| An operator drains the site | At most one instance less per poll, down to 0 |

The zone weight is the applied capacity × 100, rounded, **and never below
1**: 8 instances are 800, an instance declared 2.6 is 260.

- Envoy rejects a locality weight of 0. Envoy Gateway checks everything it
  generates and, on any error, publishes nothing, so one zero would freeze
  that gateway's whole configuration, key revocations included.
- A serving site with nothing ready therefore stays listed at weight 1. Its
  health check keeps traffic off it, and it ramps up from 1 when it recovers.
- A drained site steps down to 1 and leaves the model's sites on the poll
  after that. Undrain lists it again at 1 at once.
- A model that would be left with no site keeps the sites and weights it
  has, and the Models page shows why.

The weight stops at 10,000,000, so the sum over a model's sites fits the
32-bit number the gateway takes.

The weights are worked out once per discovery round for the whole fleet,
after every cluster has been polled, and stored on the model (`models.fleet_zones`). Every cluster is rendered from
that one list: gateways with different weights would send one conversation
to different sites.

## Entry route

With a model's **entry route** switched on, the hub renders four objects for
it on every fleet cluster, in the gateway namespace, all named
`fleet-<model slug>`:

| Object | What it says |
|---|---|
| `Backend` | One endpoint per site of the model: the site's peer host and port, with the cluster name as `zone`, over mTLS with the fleet CA and the gateway's client certificate |
| `AIServiceBackend` | Points at that `Backend`. The model's `QuotaPolicy` attaches here |
| `AIGatewayRoute` | On the client listener: requests for the model go to that backend. Request timeout 3600s in place of the gateway's 60s |
| `BackendTrafficPolicy` | On the `HTTPRoute` the gateway generates from the route, which has the same name |

A fifth object, `EnvoyPatchPolicy/fleet-<model slug>-retry`, changes two
things in the route the gateway generates. See "Retry" and "Host" below.

The policy holds the routing:

- **Site choice:** a consistent hash on the session headers, weighted by the
  zone weights. By default these are `x-claude-code-session-id` for Claude
  Code and `x-openwebui-chat-id` for Open WebUI, which sends it when
  `ENABLE_FORWARD_USER_INFO_HEADERS` is on. A request is hashed on the ones
  it carries. The
  tenant and the key are not hashed: that would pin a customer to one site.
  The site is not chosen by what it has cached; see
  [cache-aware routing between sites](how-it-works.md#cache-aware-routing-between-sites).
- **Zones:** exactly the endpoints of the `Backend`, in the same order,
  sorted by cluster name. A zone with an endpoint and no weight would get a
  weight of 1 from the gateway. The order keeps every cluster's objects and
  fleet revision identical; the hash table does not depend on it. Envoy's
  Maglev sorts the hosts by their resolved address and port, so a
  conversation lands on the same site from every gateway only if **every
  gateway resolves each peer host to the same address**. Split-horizon DNS
  that gives a peer host a different address on each site breaks that.
- **Retry:** on connect failure, reset, 500 and 503, once per other site. A
  site over its limit answers 503 at once and the request goes to the next
  one. A site whose scheduler (endpoint picker) cannot be reached answers
  500 to every request: Envoy AI Gateway up to 1.2.0 sets
  `failure_mode_allow: false` on the pool's ext_proc filter
  (`internal/extensionserver/inferencepool.go:347` in v1.2.0) and ignores
  the pool's `FailOpen`. A 500 from the model itself is tried on another site
  too. A reset is retried as well: a site that drops the connection has lost
  the request, since vLLM aborts a request whose connection closes.
- **Health check:** `GET /healthz/<model>` on each site's peer listener
  every 3s, with `panicThreshold: 1`: a site that fails is not used while
  another site passes, and when every site fails the request is sent anyway.
  The interval is not 5s because vLLM closes an idle connection after 5
  seconds, and a check every 5 seconds kept meeting the closing connection. Slashes in a model name stay in the path.
- **Retry picks from 20 sites, not 5.** Envoy Gateway lets the gateway pick
  a site at most five times for one try of a request, and then takes a site
  that already failed. With a hash, every pick lands by the same weights, so
  with one site holding 800 of 1100 all five land on it for one conversation
  in seven. Those got the failing site's 503 however many retries were
  allowed: 31 of 200 requests on the test gateway. Envoy Gateway has no
  setting for the number
  ([envoyproxy/gateway#5690](https://github.com/envoyproxy/gateway/issues/5690)),
  so the patch sets it to 20. With it, 0 of 400 requests got the 503.
- **Host:** a forwarded request carries the peer server name
  (`peers.llm.<domain>`) as its `Host`. Envoy Gateway would set it to the
  site's peer host. A peer listener that answers for the peer server name
  then returns 404 to every forwarded request, while the health check,
  which sets the name itself, keeps passing. The patch sets the `Host`.
- **About the patch:**
  - Each model has a policy of its own. Envoy Gateway applies the patches
    of one policy together, so in a shared policy one model whose route is
    not there yet would take the settings away from every model.
  - It needs `extensionApis.enableEnvoyPatchPolicy: true` in the Envoy
    Gateway configuration. A cluster without the `EnvoyPatchPolicy` kind
    cannot join the fleet.
  - A patch that is not in effect does not stop the gateway. The sync
    reports it in the task log, and the self-test fails on it.
  - It names the route as Envoy Gateway and the AI Gateway generate it
    today. Run the self-test on one cluster after every upgrade of either.
  - **Two names of the route configuration.** Envoy Gateway names the
    client listener's route configuration `<namespace>/<gateway>/<listener>`,
    and under its runtime flag `XDSNameSchemeV2` `http-<port>` for a
    plain-HTTP listener. The flag is off up to 1.9.2 and is to be on by
    default from 1.10 (Envoy Gateway's EnvoyPatchPolicy task). Before each
    sync the hub reads the listener's port from the `Gateway` and writes the
    three patches for both names. Envoy Gateway applies the ones whose
    configuration exists and reports the other name as not found
    (`Programmed: False`, `ResourceNotFound`). The policy's annotation
    `aigw-ui.io/route-configurations` lists both names, and the sync takes
    that report as in effect while one of them is found. With both missing it
    reports the patch as before. Envoy Gateway writes the not-found report
    over any other error of the same policy, so an error in the patches of
    the name that exists shows only in the self-test's stickiness step,
    which then gets no answer.
  - The retry part can go when Envoy Gateway makes the number a setting.
- **Circuit breaker:** as good as off (100000). Each site's own limit decides.

Nothing is owned twice: the hub is the only writer of these objects, and the
charts no longer ship an entry policy.

**Switching it on** (Models page, per model, off by default) is refused
while no fleet cluster serves the model, while the model has endpoints
entered by hand, or while the server has no `FLEET_DOMAIN`. From then on
requests through the entry route are counted on the entry backend, in a
counter of its own, so a tenant's usage starts at 0 there.

**A cluster's own route and the quota.** A route a cluster already has for
the model on the client listener is older than the entry route and wins the
match, so clients of that cluster keep using it until it is moved. Discovery
reads which listeners each route is attached to:

| The cluster's own route is attached to | Quota on its backends | What it means |
|---|---|---|
| The client listener, or the whole Gateway | Kept | Clients still reach the backend. The Models page names the cluster |
| Only other listeners, such as the peer listener | Removed | Only other sites' gateways reach it, and they have charged the request already |

A route on the whole Gateway is reached from both sides, so a request that
another site forwards is counted there a second time, in the backend's own
counter. Attach the route to the peer listener alone to end that.

**An entry route that cannot be rendered is held.** When the server has no
`FLEET_DOMAIN`, or none of a model's sites is a fleet cluster with a peer
host any more, the hub leaves the model's five entry objects on the clusters
as they are. Everything else is still synced, the model's quotas included:
they are rendered on the entry backend that is there, so a lowered limit or
a tenant that left does not wait for the hold to end. The cluster's sync
message and the Models page say which model is held and why. A missing
setting is never read as "switched off".

**A fleet cluster cannot be deleted.** Its gateway would keep its entry
routes with today's sites and weights, and its keys, with nobody to update
or revoke them. Take it out of the fleet first: that sync removes the entry
routes. Then delete it.

**A fleet cluster cannot be renamed.** Its name is its zone on every
gateway, and a new zone deals every model's conversations out again. Take
the cluster out of the fleet first.

**Same fleet everywhere.** Everything that must be equal on every fleet
cluster (each model's sites and weights, and the shared settings) is hashed
into a *fleet revision*. A sync that applied everything stores it on the
cluster. The Clusters page shows it, and marks a fleet cluster "outdated"
while its revision is not the fleet's current one: until it is synced, it
can send a conversation to another site than the other clusters do.

**What the entry route does not do.** These are limits of this version:

- **Quotas are not enforced on an entry route with more than one site.**
  See "Known problem" at the top of the 0.8.0 release note. A model with
  one site gets no zone weights and keeps its quotas.
- A request with both session headers is hashed on both, so it lands
  elsewhere than one with either alone. A client should send one.
- A request without a session header has nothing to hash and goes to a
  site picked at random each time, so only clients that send the header get
  cache reuse. A client that sends it also chooses its site by choosing the
  value.
- The site is picked by capacity, not by load. A site that is up and slow
  keeps its share.
- Sites that declare different `aigw-ui.io/model-revision` values still
  share the model's traffic. The Models page warns; nothing is held back.
- A request that was reset after it was sent in full is tried on another
  site, so the work can be done twice. It is charged once.
- Every gateway checks every site of every model: gateways × sites × models
  probes every 5 seconds. Six clusters, six sites and 20 models are 720.
- A cluster's own traffic also goes out to its peer host and comes back in.
- The peer listener checks no key. The client certificate in
  `llm-peer-client` is all that stands between a caller and every model,
  without a quota. Whoever can read Secrets in a gateway namespace has that.

**What each cluster must already have** (from the gateway chart): the client
and peer listeners, the ConfigMap `llm-peer-ca` and the Secret
`llm-peer-client` in the gateway namespace, DNS for the peer hosts, and on
each serving site a route on the peer listener that answers
`/healthz/<model>` and serves the model.

A model is matched by its name here, or by the name the cluster's route sends
to the backend (`modelNameOverride`), against `spec.model.name` of the
`LLMInferenceService`, or the service's own name when that is empty.

**Drain.** *Drain* on the Models page takes one site out for one model before
maintenance: its weight steps down to 1 and it then leaves the model's sites.
*Undrain* lists it again at 1 and it comes back one instance per poll.
Draining the last site that has capacity is refused. Both are in the task
log and the audit log.

**Recipe check.** Each `LLMInferenceService` can declare
`aigw-ui.io/model-revision` and `aigw-ui.io/max-model-len`. Since 0.34.0 a
deployment without the length annotation has the length its servers report:
discovery asks one ready pod's model server for `/v1/models`
(`max_model_len`) through the API server's pod proxy, at the scheme and port
of its readiness check, once per pod (`kube.servedLength`, remembered by pod
UID; a pod that does not answer is asked again after 10 minutes). The Models
page warns when the sites that serve a model differ: a different revision
means one model name gives different answers, and a smaller length means a
long request fails at that site. The tool only warns;
the operator decides, and can drain a site.

## Best-effort route

A model in best-effort mode has, on every fleet cluster, a second
`AIServiceBackend`, `fleetbe-<slug>`, that sends to the same `Backend` and
sets `x-llm-d-inference-objective: best-effort`, and a `QuotaPolicy` of the
same name that counts in shadow mode. While at least one tenant is past its
budget it also has an `AIGatewayRoute`, a `BackendTrafficPolicy` and an
`EnvoyPatchPolicy` called `fleetbe-<slug>` (the patch `fleetbe-<slug>-retry`). The
route matches the model header and `x-aigw-client-id` against the listed
tenants. The traffic policy is the entry route's, with 429 added to the
statuses that send a request to the next site.

**The tenant as the scheduler's flow.** The hub's token check writes the
token's `sub`, the tenant, into `x-llm-d-inference-fairness-id` as well as
`x-aigw-tenant`. A site's scheduler with flow control shares each class out
by that header, so within a class, best-effort or standard, one tenant
cannot crowd out the others; on the entry site too, since every request
passes the client listener first. A request with a plain API key carries
none and joins the scheduler's default flow. Without flow control the
scheduler only labels its request metrics with it (at most 1,000 values,
then `other`).

**When best-effort would not rank below regular tenants**, the sync says so
in the task message. A scheduler holds best-effort back only if all of this
holds where the model is served:

- the class `best-effort` names the pool: a namespace takes one, for the
  first pool by name, and a scheduler takes only the classes that name its
  own pool (llm-d router v0.10.0). Another pool in that namespace serves
  best-effort requests at priority 0, as regular tenants' are;
- the site's own class `standard` for the pool, if it has one, ranks above
  `best-effort` and not below 0: regular tenants' requests name it;
- the pool's scheduler runs flow control. Without it, it refuses
  best-effort requests only while it counts the pool full, which its
  default detector did not do in the lab test of the
  [optimization guide](optimization.md#what-a-site-does-with-the-class).
  The hub reads that from the `LLMInferenceService` (its scheduler config, or
  that of the last config it is built on that has one) and says nothing
  when the config is in a ConfigMap.

The overage loop (`internal/overage`) decides who is listed. It reads the
usage report, the same counters as the Usage page, every `OVERAGE_INTERVAL`
and stores each move in the table `overage` with the end of the quota's
window. A change of the set queues one sync of all clusters.

A model can limit best-effort use (`models.best_effort_limit`). The same
loop compares the limit with the tenant's counter on the best-effort
backend. At the limit it sets `overage.capped_at`, and the next sync
renders the best-effort route without the tenant. The period stays in the
table until the window ends, so the tenant is not moved again in it. Its
own budget is spent, so the gateway's quota filter refuses it. This has
been tested in the loop and against Postgres, not on a gateway.
[What each action does](how-it-works.md#best-effort-when-a-budget-is-spent)
has the reasons.

Seen on a test gateway (Envoy Gateway 1.9.1, AI Gateway 1.1.0): a tenant at
38 of 30 tokens was moved 4 seconds after the mode was set and answered 18
seconds after; the serving site received `best-effort` for it and `standard`
for another tenant, also when the client sent a class of its own; a usage
reset put it back. Not seen there: a request going to a second site after a
429, because the test fleet has one site.

What the serving site does with the class is the scheduler's part. A real
model (vLLM 0.29, KServe 0.21 with its default scheduler settings) that got
more requests than it could serve treated `best-effort` and `standard`
alike: nothing was refused and both waited in one queue. See
[optimization.md](optimization.md#what-a-site-does-with-the-class).

## API-key policy and the peer listener

The policy is the API key check, the JWT check, or both during the switch to
tokens (`internal/render/identity.go`). Its authorization rules refuse, in
order: revoked tokens and those of deleted tenants, by `jti` and, since
0.32.0, by spans of their expiry (`revoked-span`, see
[security](security.md#tokens)); revoked personal keys' tokens, by `cid`;
disabled tenants, by `sub`; and a model not in the token's list. During
the switch the Secret `aigw-ui-api-keys` lists the API keys, the admins'
tokens and, since 0.34.0, the tokens personal keys got (`personal_tokens`,
under `<slug>.<jti>`), since Envoy's API key check runs first and refuses
what it does not list; a personal key's helper gets the same stored token
until a new one has had two minutes to reach the gateways
(`store.listedToken`, [security](security.md#tokens)). A fleet cluster must have a *Client listener* set. The `SecurityPolicy`
attaches to the whole Gateway unless the cluster has one; then it attaches to that listener alone
(`sectionName`). A gateway that also has a listener for requests forwarded by
other sites needs this: the entry gateway removes the key before forwarding,
so a key check on that listener would refuse every cross-site request.

**A route with its own policy.** A `SecurityPolicy` on a route of the client
listener replaces the hub's on that route: Envoy Gateway reports the hub's
policy `Overridden` and names the routes. Requests there are not checked for
a tenant's key or token, carry no tenant header, and are charged to no
tenant's budget or quota. Since 0.29.0 the sync reads the policy's status and
adds a warning to the task message; it is not a rejection, since the route
is the site's (a site export showed KServe's route of glm53 on the client
listener with an internal key of its own, 2026-10-16). A route that
forwards nothing is left out: none of its rules has a backend or mirrors to
one, so it reaches no model. The gateway chart's health check
(`envoy-ai-gateway-health`, which answers `/healthz/gateway` itself) is such
a route, has a policy of its own, and is on the client listener wherever a
site turns it on (`health.enabled`, for a global load balancer). The hub gets each route the report names to see that; a route it may
not get stays in the warning. Discovery adds the
same kind of warning to the cluster's discovery message when an
`LLMInferenceService`'s `status.router.gateways[].listeners` holds the client
listener: KServe's route there reaches the model without the entry route and
without the AI gateway's processing.

## Audit log

A wrapper around the API records every request that is not a read: method,
path, status, the caller's address and the `X-On-Behalf-Of` name. The handler
adds the same action and summary it writes to the task log. The task log
answers "did it reach the clusters"; the audit log answers "who asked, and
what did the server say". Bodies are not stored.

## Prices

`model_prices` holds every set of prices of a model, in credits for a
million tokens, with the day it starts and when the server began using it.
The newest applied row is the one in use.

- `syncer.RunPrices` wakes at every 00:00 UTC and once at start. It applies
  the rows whose day has come in one transaction and queues one sync. For a
  model's first prices the same transaction converts `default_limit` and
  every `token_limit` and sets `price_dry_run`.
- `render.Prices.Expression` returns the cost expression. `store` puts it
  in `render.Model.CostExpression` in place of a typed one, so the renderer
  itself knows nothing about prices.
- In dry-run every tenant rule is rendered with `shadowMode` and the
  default bucket with the largest limit. The usage report marks those
  quotas, and the overage loop skips them as it skips any shadow quota.
- `internal/costcel` evaluates the rendered expression in the tests, with
  the gateway's CEL environment, so a change to it that the gateway would
  refuse fails a test.

## Self-test

The self-test runs in the background in the server. It creates a tenant
`selftest-<random>` with a key and a quota of 1 token per hour, syncs the one
cluster, and sends chat requests with `max_tokens: 1` to the cluster's Gateway
URL. It waits up to 90 seconds for the gateway to learn the key, looks up the
tenant's counter through the same code as the Usage page, expects the next
request to be refused, resets the counter, and expects an answer again. Then
it deletes the tenant and syncs once more. Runs are kept in memory; the
outcome is in the task log.

The counter is compared with what the request should cost. The server
evaluates the model's cost expression itself, with the CEL environment the
gateway uses (`internal/costcel`), over the token counts in the answer. The
gateway's count is that cost plus 1: the check before a request is a hit of 1
on the same counter.

Two steps check what a price for cached prompts would rest on. One resets
the counter and sends a request with the tenant's key and a foreign
`x-aigw-client-id`; the request must still be counted for the tenant, which
proves the gateway replaces the header after it has checked the key. The
other reads `usage.prompt_tokens_details.cached_tokens` from the answer to a
repeated prompt.

A step reads the temporary tenant's token counters on the model, as the
history does, and passes when its requests and input tokens were counted.

These are the server's own test requests. A client's request never passes
through the server.

## Live usage

The server reads token usage from the same Redis the gateways' quota rate
limit services count in. It computes the name of each counter and fetches
them with one `MGET`. With `redis.allowReset` on it can also delete a
counter, which is how a reset works; otherwise it only reads.

The names come from every cluster's desired state, which the usage page, the
best-effort loop, the history and the tenants' page build from one read of
what the clusters have in common (`store.RenderStatesWithoutKeys`): one query
per cluster for its endpoints, and about thirteen for the rest, where each
cluster took about fifteen of its own up to 0.30.0.

A counter's name is built by the gateway from the `QuotaPolicy`:

```
ai-gateway-quota_backend_name_<namespace>/<AIServiceBackend>_model_name_override_<model>_<rule>_<rule>_<window start>
<rule> = rule-<position>-x-aigw-client-id|<tenant pattern>-match-0
```

Two things follow from that name:

- **One budget across sites needs identical names.** Clusters share a counter
  only when the backend has the same namespace and name on each. Otherwise
  every cluster counts on its own and the tenant gets the limit once per
  cluster. The Usage page shows a warning when it sees this.
- **A rule's position is part of the name.** So a quota keeps the position it
  got when it was created, for as long as it exists. When a quota is removed,
  its position is filled with a placeholder rule that matches no request, and
  the next new quota on that model takes it over. Adding or removing one
  tenant's quota therefore never renames another tenant's counter.
- **The pool is named after the number of rules.** The pool's counter starts
  again from zero for the current window when the number of rules changes:
  when a quota or an amount is added at the end or the last one is removed,
  and when the first budget across models is set or the last one removed (its
  rule comes last). The self-test does both. With the default pool of 1 token
  this does not matter; a large shared pool can let up to twice its limit
  through in that window. On the lab, 2026-10-17, a pool of 20 credits an hour
  counted under rule 5, 10 and 11 within one hour and let 78 through. The AI
  gateway names the pool's counter after its position; the hub cannot keep it
  still without padding every policy with empty rules.

## Token counts

While the hub reads Redis (`redis.tokenCounts`, on by default) and the
gateways take tokens only, each gateway counts every tenant's requests and
the input, cached and output tokens of their answers, per model and hour.
Nothing passes through the hub for it.

- The budget policy `aigw-ui-tenant-budgets` on the client listener gets four
  rules after its budget rule: requests, input, cached, output. Each matches
  `x-aigw-tenant` and `x-ai-eg-model` with `type: Distinct`, is `shared`,
  counts by the hour, and is in `shadowMode` with the largest limit, so it
  never refuses. The requests rule adds 1 per request; the others add the
  answer's count when it ends. The policy is rendered for these rules alone
  when no tenant has a budget, and the budget rule stays first, so no rule
  moves.
- The routes the hub renders write the counts into the request's metadata
  under `aigw_ui_input` (`InputToken`, cached tokens included),
  `aigw_ui_cached` (`CachedInputToken`) and `aigw_ui_output` (`OutputToken`).
  The sync adds the same keys to the routes of a cluster's own that it marks
  (those of a model with quotas or budgets). A route it does not touch writes
  them only when the site's `GatewayConfig` declares them
  (`globalLLMRequestCosts`, see [deployment](deployment.md#token-counts-on-every-route));
  its requests are counted without tokens otherwise.
- The counter of rule *i* (1 to 4) is
  `<ns>/aigw-ui-tenant-budgets_<ns>/aigw-ui-tenant-budgets/rule/<i>_<ns>/aigw-ui-tenant-budgets/rule/<i>_rule-<i>-match-0_<tenant>_rule-<i>-match-1_<model>_<hour start>`
  (`render.TokenCounterKey`, from Envoy Gateway 1.9.2's descriptors and the
  rate limit service's key format; the self-test step "The tenant's requests
  and tokens are counted by model" checks it on a gateway).

The history reads the requests counter of every tenant and model it may
call, and the other three only where a request was counted or it already
follows the pair: about one key per tenant and model a minute, in batches of
1,000 per `MGET`. The tenants' own page shows what it recorded.

## Usage history

`internal/history` keeps what was used in the past. Every
`USAGE_HISTORY_INTERVAL` (default 1 minute) it reads every counter it
follows and adds what each grew by since its last look to the running hour:
a quota's counters to `usage_hours`, the token counters to `token_hours`.
Each counter is followed by its own Redis key in `usage_counter_cursors`
(0.30.0; before, one cursor per tenant and model in `usage_cursors`).

| Case | What is added |
|---|---|
| Same key as at the last look | The counter minus the cursor |
| The counter is lower than the cursor | The counter: it was reset |
| A new window began | The new counter, and what the old window's counter grew by after the last look. That part goes to the last hour of the old window |
| A key the hub no longer computes, in a window still running | What it grew by since the last look. It is read to the end of its window. That covers a tenant's old amount after a change of quota |
| Redis cannot be read | Nothing. The cursors stay, and the next look adds it |

The counters a tenant has are those of its quota or budget, and while a
token signed before its last change of limits may still be in use (the
longer of `tokens.lifetimeDays` and `tokens.renewedLifetimeHours`), those of
every other amount of the model: such a token is counted under the amount it
names. Each amount's rule is read once per such tenant, on each route the
model counts on, however many tenants hold the amount and however many
clusters render it. With the default token lifetime of 180 days that is
nearly every tenant: at 1,000 tenants and five amounts on a best-effort
model, 10,000 keys a minute. A reset of a tenant's quota records the use since the last look
first, so the reset does not take it out of the history. A quota's counter
keeps a cursor at 0 too; a token counter while another of the pair's holds
something.

A key the history never looked at is not followed: a counter that gets its
first use and stops being computed between two looks is lost. No rule
moves since 0.32.0, so a counter stops being computed only when its tenant
changes: a new amount (covered by the amounts above), or a quota, budget,
model list or the tenant itself removed within a minute of its first use.
Up to 0.31.0 a budget
tenant's rule moved with every amount added after it, and a tenant that
used a model in the minute before such a move lost that use from the
history (0.31.0 run).

The counter of a window that ended is still in Redis: the rate limit
service lets a key expire one window length and some jitter after its last
hit. That is how the end of a window is read after it.

A look runs in one transaction that first takes an advisory lock, so two
replicas never add the same usage twice. Rows older than 400 days are
deleted once a day, from both tables.

The history is read by `GET /usage/history` and by the tenants' own page.

## The tenants' own page

`GET /api/v1/my/usage` is outside the admin API. Its bearer token is a
tenant's API key. The server finds the stored keys that start like it,
opens them and compares in constant time; the tenant of the match is the
only one the answer is about. The answer is built from the usage report of
that tenant, its rows of the history and the prices of its models.

The answer also carries the tenant's token history (`token_days`,
`token_hours`): requests and input, cached and output tokens per model,
for the models it may call. The page shows them as its workload: totals,
the share of the prompt from the cache, tokens and requests over time, and
per model what the cache saved at the model's prices.

The UI serves it at `/my-usage`, a path of its own next to the admin
pages, with its own sign-in. See [security](security.md#the-tenants-own-page).

The page's **Set up Claude Code and Oh My Pi** runs the installer from
`/clients/`, which the hub serves without a sign-in from files compiled into
its binary (package `clients`, `clients/embed.go`): `install.sh`,
`aigw-token`, the Omp extension, and the Claude Code plugin packed on first
request as a marketplace archive (`claude-code.tar.gz`, the same bytes every
time).

`GET /api/v1/my/models` (`internal/api/myclients.go`, 0.34.0) takes the same
keys and gives what the clients are set up with: the gateway's address
(`config.gatewayURL`, a path of `/anthropic` or `/v1` dropped at startup)
with `/anthropic` and `/v1` added, the models the tenant may call that have
an endpoint, with their length and prices in dollars, and Claude Code's
model and small model (`config.claudeCode`, else the tenant's priciest and
cheapest). The installer writes them into Claude Code's settings
(`ANTHROPIC_BASE_URL`, `ANTHROPIC_MODEL` and the alias models,
`CLAUDE_CODE_MAX_CONTEXT_TOKENS` and `CLAUDE_CODE_MAX_OUTPUT_TOKENS` when
the length is known). The Omp extension asks for them when omp starts and
registers them as the provider `aigw` (`pi.registerProvider`), with
`!aigw-token` as the key command, so nothing is written to `models.yml`.

## Deployments

The hub deploys models through the Argo CD platform; it never applies a
model to a cluster itself. Git is the source of truth: the hub keeps none of
it in Postgres, reads the files back on every request (`internal/gitops`,
the clone fetched at most every 10 s) and writes a file only when someone
saves on the page.

**Layout** (argocd-day2-platform's, `gitops.dir`):

| File | Holds |
|---|---|
| `operators/<model>/<model>.yaml` | the model app's config: chart repository and revision, `projectNamespace`, sync options |
| `operators/<model>/values.yaml` | the recipe: weights, images, the model's vLLM and LMCache flags |
| the site's layers | `sites/<site>/values.yaml`, `…/<env>/values.yaml`, `defaults/hosted-clusters/<model>/values{,-<env>,-<cluster>}.yaml`, `…/mces/<mce>/values.yaml`, `…/<cluster>/values.yaml` |
| `sites/<site>/<env>/mces/<mce>/<cluster>/<model>/values.yaml` | the cluster's own layer; the directory registers the model there |

A model is an operator whose merged values have `model.name`; the others
are the platform's and stay out of the page. The hub merges the layers as
Helm does (maps merge, lists replace, null removes a key) in the order the
platform's deploy chart uses (`gitops.Layers`; `ocp-versions/` is left out:
the hub does not know a cluster's OpenShift version).

**Writing.** A save of a deployment writes the cluster's own file as what
differs from the layers below (`gitops.Override`: changed values, null for
a value taken away), so a recipe change still reaches every cluster. The
file is patched, not rewritten (`gitops.Patch`): a change of plain values
is made in the text itself (blank lines, flow maps and comment columns
stay); anything else re-encodes the tree, which keeps comments and the
order of the keys that stay. Every save names the git blob of the file it
was made on; git refuses an old one (409) and the push is retried three
times when someone pushed in between. A new model copies an existing
model's app config (name and namespace changed) and renames the recipe's
object names that start with the source's name (SCC, StorageClass, PVCs,
LMCache engine, peer routes), since a cluster that runs both would have two
apps fight over them.

**Checks before a commit** (`gitops.CheckValues`): `model.name`,
`namespace` equal to the app's `projectNamespace`, `model.subPath` or a
`model.uri`, an LLMInferenceService name of at most 42 characters, every
flag in `llmisvc.args` and `llmisvc.extraArgs` one plain token or
`--option='value'` (KServe's launcher runs them through `bash eval`), no
`--kv-transfer-config`, `--max-model-len` in one place, no
`VLLM_ADDITIONAL_ARGS`. The chart checks the same at render.

**Status.** Two sources, each optional: Argo CD's Application of the
deployment (`ARGOCD_APP_NAME`, read with get/list in `ARGOCD_NAMESPACE`,
cached 10 s), and what discovery last saw of its LLMInferenceService
(`model_deployments` by cluster, namespace and name).

**The chart** (`model-deployer`, doomsday-stack): `generic-model`'s stack
(node preparation, local PVs, per-node S3 pull jobs, LMCache, SCC, the
LLMInferenceService) with the fleet side (peer route with an Exact match,
`/healthz/<model>`, the classes `interactive`/`standard`/`batch`, flow
control merged into the model's scheduler config, keep-alive 75) and
nothing on the client listener. A cluster's shape: data parallel over ports
(`llmisvc.dataParallelExternalLB`: the flags, a pool of one port per rank,
supervisor probes), KServe's `parallelism` (tensor, data, dataLocal,
expert), a replica over several nodes (`llmisvc.multiNode`: KServe's
LeaderWorkerSet) with RoCE (`llmisvc.roce`), and its own flags
(`llmisvc.extraArgs`, rendered before the recipe's `args`).

## Not verified on a real gateway

- Deployments (0.35.0): the GPU shapes of `model-deployer` (data parallel
  over ports, tensor parallel, multi-node over RoCE, LMCache) were rendered
  and compared with the old charts, not run on GPUs.

**Run on a gateway.** On OpenShift 4.22 with Envoy Gateway 1.9.1 and Envoy
AI Gateway 1.1.0, one site, a model added by hand:

- the `forwardClientIDHeader` field of the API key `SecurityPolicy`, and
  that the gateway replaces an `x-aigw-client-id` a client sends;
- that a `RegularExpression` client selector counts a tenant's keys in one
  bucket, and that two tenants are counted apart;
- that the counter names computed for the Usage page are the ones the rate
  limit service writes;
- that a tenant over its limit gets 429, and that a reset frees it;
- that the counter grows by a request's cost plus 1;
- that the gateway uses the cost expression of a priced model, for plain
  and for streamed answers;
- that a model in dry-run (`shadowMode` on every rule, the largest default
  bucket) refuses nobody;
- that the gateway removes `kv_transfer_params` from a request;
- that `/v1/models` answers 401 without a key once "Enforce API keys" is
  on. Set the cluster's API key for `/v1/models`.

**Not run:**

- that counters are shared when two clusters use the same Redis;
- that a quota takes effect on a backend whose route sets no `modelNameOverride`. The gateway documents that a quota "only applies when its `modelName` matches the `modelNameOverride`" and says nothing about routes without one. The UI marks these backends "quota unverified";
- how one dry-run quota (`shadowMode`) behaves next to a small default bucket. The gateway documents that a shadowed rule never rejects; whether the tenant is then still held to the pool for tenants without a quota is not stated;
- a real vLLM reporting cached tokens. The run used a stand-in that reports them the way vLLM documents it;
- that the gateway's controller sets a condition within two seconds in every case. If it is slower, the task log shows the object without a gateway verdict, or with the verdict on the previous version;
- that a reset frees a tenant when the rate limit service keeps its own over-limit cache (`LOCAL_CACHE_SIZE_IN_BYTES`).

The **Self-test** button on a cluster checks most of the first list with
real requests on your own gateway. Run it on one test cluster before a first
sync to production, and after every upgrade of the gateway.

Not verified for site weights, which were tested against a real Kubernetes API
server with cut-down CRDs but not against KServe or Envoy Gateway:

- that a live KServe fills `status.workloads.primary.readyReplicas` and
  `status.workloads.prefill.readyReplicas` as its 0.21 source says;
- that `serving.kserve.io/stop: "true"` is how a stopped service is marked. It
  is what makes a stopped service count as zero and not as unknown;
- that the available replicas of the Deployment `status.workloads.primary`
  names match what KServe copies into the status on a live cluster (0.29.0
  reads the Deployment; tested against a stand-in API only);
- that Envoy Gateway accepts a zone weight of 0, and what it does with a zone
  that has no endpoint;
- that Argo CD leaves the weights alone with the setting above.

Not verified for token counts (0.30.0): the counter names on a live
gateway (worked out from Envoy Gateway 1.9.2 and the rate limit service's
source; the self-test checks them), and the extra latency of four more
descriptors on every request.

Not verified for the denied-token spans on a real gateway: 0.32.0's rule
used CEL Envoy does not build, and the lab's proxy then refused every change
([security](security.md#tokens)). 0.32.1's rule ran in Envoy 1.39.1 locally,
behind the same JWT provider and in the form Envoy Gateway 1.9.2 gives it,
and refused and answered as it should; not yet on the lab's gateway. After
an upgrade, check that a token of a deleted test tenant is refused and that
the proxy took the configuration (operations, "nothing changes").

Also not verified: that the gateway accepts the placeholder rule that keeps a
removed quota's position, a rule with an `Exact` match on a client ID no key
has. If it does not, the task shows "Applied, not accepted" after a quota is
removed.

## Requirements on each LLM cluster

- Envoy Gateway with the `Backend` API enabled.
- Envoy AI Gateway 1.2.0 for quotas by amount and budgets across models.
  1.1.0 charges a `QuotaPolicy` rule that counts per tenant (`Distinct`) to
  one shared bucket ("Per-tenant token limits charge the right tenant" in
  the 1.2.0 release notes), so every tenant with the same amount shares one
  counter there; the self-test's tenant-header step fails on it and says
  so. Shared pools and the entry route work on 1.1.0. This tool writes `AIGatewayRoute` and
  `AIServiceBackend` as `aigateway.envoyproxy.io/v1beta1`, and `QuotaPolicy`
  as `v1alpha1`, the only version it has. Up to 0.11.0 it wrote all three as
  `v1alpha1`, which both gateway versions still serve but call deprecated.
- For quotas: the dedicated quota rate limit service, pointed at the Redis the gateways share (not on the hub in production). The AI gateway Helm chart does not deploy it.
- For budgets across models: global rate limiting in the Envoy Gateway
  configuration (`rateLimit.backend` with the shared Redis and `timeout` of a
  few seconds); Envoy Gateway then runs its own rate limit service next to
  the AI gateway's. See [deployment](deployment.md#before-the-first-budget-across-models).
  Token counts need it too: with `redis.tokenCounts` on (the default) and
  tokens only, the hub writes the budget policy for them even without a
  budget. Turn the setting off for a site without it.
- For tenants of one class to be served in turn when a pool is full: flow
  control in the model's scheduler with `round-robin-fairness-policy` on the
  band of priority 0. Its default, `global-strict-fairness-policy`, serves
  the oldest request first whatever the tenant; the sync warns about it on
  the pools of best-effort models. See
  [optimization](optimization.md#tenants-in-turn).
- A `ClientTrafficPolicy` on the Gateway with `connection.bufferLimit`, for
  example `50Mi`. Without it Envoy Gateway buffers 32 KiB of a request and
  the gateway answers 413 to every longer prompt. This tool does not write
  it: it belongs to whoever installs the Gateway. The self-test checks it.
- For quotas on a model with two or more sites: a gateway with
  [agent-router#2833](https://github.com/theagentrouter/agent-router/pull/2833),
  which no release has yet.

The Test button on the Clusters page reports which of the CRDs are installed.
