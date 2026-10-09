# Architecture

This document describes how this tool works today. For the design of the
whole platform, including routing between sites, see
[platform-architecture.md](platform-architecture.md).

## Components

| Component | Runs on | Role |
|---|---|---|
| API server (Go) | hub | Serves the UI and the REST API, runs the sync and discovery loops |
| UI (React) | hub, inside the server image | Static files served by the API server |
| Postgres | hub | Source of truth: clusters, models, tenants, keys, quotas |
| Envoy AI Gateway | each LLM cluster | Serves the traffic. Not installed by this tool |
| Redis | hub | Shared token counters for the gateways' rate limit service. Not installed by this tool and not used by the server itself |

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
API refuses a quota on it.

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

Every `SYNC_INTERVAL` (default 5 minutes) every cluster is synced again even
if nothing changed. This retries a cluster whose last sync failed and puts
back an object that someone edited or deleted by hand.

## What is applied to a cluster

| In the UI | Objects on the cluster |
|---|---|
| Model found by discovery | none; the existing route and backends are left alone |
| Model added by hand | `Backend`, `AIServiceBackend`, `AIGatewayRoute` |
| Tenant quota | one `QuotaPolicy` per model in each namespace that holds one of the model's `AIServiceBackend`s |
| Tenant API keys, with "Enforce API keys" on | one `Secret` (`aigw-ui-api-keys`) and one `SecurityPolicy` on the gateway |

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

1. A key has the client ID `<tenant>.<random>`, for example `team-search.5018d00f`.
2. The gateway's API key auth validates the key and forwards the client ID in the `x-aigw-client-id` header.
3. The model's `QuotaPolicy` has one bucket rule per tenant that matches `^<tenant>\.[a-f0-9]+$`, so all keys of a tenant share one bucket.
4. The gateway's quota rate limit service counts tokens in Redis.

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

## Task log

Each change is stored as a task with one pending result per cluster. When a
cluster is synced, the sync first notes which tasks are open for it, applies
the desired state, and then writes the outcome on those tasks.

For every object the sync reads it, applies it, and compares: an object that
was not there is "created", one whose generation moved is "updated", one that
is the same is not listed. After a change it waits two seconds and reads the
status of the AI gateway objects, to record whether the gateway's controller
accepted them. That reading is informational: it never fails a sync.

## Audit log

A wrapper around the API records every request that is not a read: method,
path, status, the caller's address and the `X-On-Behalf-Of` name. The handler
adds the same action and summary it writes to the task log. The task log
answers "did it reach the clusters"; the audit log answers "who asked, and
what did the server say". Bodies are not stored.

## Self-test

The self-test runs in the background in the server. It creates a tenant
`selftest-<random>` with a key and a quota of 1 token per hour, syncs the one
cluster, and sends chat requests with `max_tokens: 1` to the cluster's Gateway
URL. It waits up to 90 seconds for the gateway to learn the key, looks up the
tenant's counter through the same code as the Usage page, expects the next
request to be refused, resets the counter, and expects an answer again. Then
it deletes the tenant and syncs once more. Runs are kept in memory; the
outcome is in the task log.

## Live usage

The server reads token usage from the same Redis the gateways' quota rate
limit services count in. It computes the name of each counter and fetches
them with one `MGET`. With `redis.allowReset` on it can also delete a
counter, which is how a reset works; otherwise it only reads.

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
  again from zero for the current window when the number of rules changes,
  which happens when a quota is added at the end or the last one is removed.
  The self-test does both. With the default pool of 1 token this does not
  matter; with a large shared pool, expect the pool's usage to restart.

## Not verified on a real gateway

The rendered objects follow the published API, but these points have not been
run against a live Envoy AI Gateway:

- the `forwardClientIDHeader` field of the API key `SecurityPolicy`;
- that a `RegularExpression` client selector puts all matching keys in one bucket;
- that counters are shared when two clusters use the same Redis;
- that a quota takes effect on a backend whose route sets no `modelNameOverride`. The gateway documents that a quota "only applies when its `modelName` matches the `modelNameOverride`" and says nothing about routes without one. The UI marks these backends "quota unverified";
- how a dry-run quota (`shadowMode`) behaves next to the default bucket. The gateway documents that a shadowed rule never rejects; whether the tenant is then still held to the pool for tenants without a quota is not stated;
- that the gateway accepts every cost expression the API lets through. The API only checks the names and characters used;
- that the gateway's controller sets a condition within two seconds, and that its first condition is `Accepted` or `NotAccepted`. If it is slower, the task log shows the object without a gateway verdict, or with the verdict on the previous version;
- that the counter names computed for the Usage page match what a live rate limit service writes. They follow the gateway's and the rate limit service's source. If they do not match, the page says what it found in Redis instead;
- that a reset frees a tenant that was already rejected. Deleting the counter was tested; the rate limit service's own over-limit cache was not;
- that `/v1/models` still answers once "Enforce API keys" is on. If it needs a key, set the cluster's API key for `/v1/models`.

The **Self-test** button on a cluster checks several of these with real
requests: client ID forwarding and the tenant rule (a request with a new key
is answered and counted), the counter names, the refusal over the limit, and
the reset. Run it on one test cluster before a first sync to production.

Also not verified: that the gateway accepts the placeholder rule that keeps a
removed quota's position, a rule with an `Exact` match on a client ID no key
has. If it does not, the task shows "Applied, not accepted" after a quota is
removed.

## Requirements on each LLM cluster

- Envoy Gateway with the `Backend` API enabled.
- Envoy AI Gateway 1.0 or later (`QuotaPolicy`).
- For quotas: the dedicated quota rate limit service, pointed at the hub Redis. The AI gateway Helm chart does not deploy it.

The Test button on the Clusters page reports which of the CRDs are installed.
