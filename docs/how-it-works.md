# What each action does, and why

Every action in the UI is listed here with three things: what happens when
you use it, how the server does it, and why it was built that way. For the
steps to click through, see the [user guide](user-guide.md). For the design
as a whole, see [architecture](architecture.md).

Two things hold for every action:

- **Postgres is the source of truth.** An action first saves what you want.
  The clusters are changed afterwards, by a *sync*. If a cluster is down, the
  change waits and is applied when it is back.
- **The server only touches objects it created.** They carry the label
  `app.kubernetes.io/managed-by: aigw-ui`. A sync never changes or deletes
  anything else on a cluster.

## Clusters

### Add a cluster

**What it does.** The cluster appears in the list, its models are read at
once, and it gets its first sync.

**How.** The kubeconfig is encrypted with the server's key and stored in
Postgres. It is never returned by the API. The server then asks the cluster
which models it serves (see *Discovery*).

**Why.**

- *One kubeconfig per cluster, stored encrypted.* The hub has to write to
  every cluster, so it needs credentials for each. Encrypting them means a
  copy of the database alone does not give access to the clusters.
- *The hub applies the objects itself.* It does not go through Argo CD,
  because a key that is revoked has to stop working within seconds, not at
  the next Git sync.

### Test connection

**What it does.** Says whether the cluster answers, which Kubernetes version
it runs, whether the gateway's kinds are installed, and whether the Gateway
object exists.

**How.** Read-only calls with the stored kubeconfig. Nothing is written.

**Why.** A wrong namespace or gateway name otherwise shows up only as a
failed sync later, with a less direct message.

### Enforce API keys

**What it does.** The gateway refuses every request without a valid key.

**How.** The sync writes one Secret with all active keys and one
`SecurityPolicy` on the gateway that checks the `Authorization` header
against it. The gateway then puts the key's client ID,
`<tenant>.<random>`, into the header `x-aigw-client-id`.

**Why.**

- *It is a switch per cluster, off by default.* Turning it on for a gateway
  that already serves clients without keys would cut them all off.
- *The client ID header is what quotas match on.* Without the key check a
  client could send that header itself and spend another tenant's budget.
  That is why a fleet cluster must have it on.

### Client listener, peer host, part of the fleet

**What it does.** *Part of the fleet* makes the cluster one of the sites
that share each model's traffic. The client listener is the Gateway listener
clients come in on. The peer host and port are where the other sites reach
this gateway.

**How.** The key check is attached to the client listener alone. The peer
host becomes the cluster's endpoint in every model's entry route.

**Why.**

- *The key check must not cover the peer listener.* The gateway that takes a
  request removes the key before it forwards the request to another site, so
  a key check there would refuse every forwarded request.
- *All fleet clusters must use the same gateway namespace.* The namespace is
  part of the name of every quota counter. A site with another namespace
  would count its tenants' tokens apart from the rest, with no error.
- *A fleet cluster cannot be renamed.* Its name is its zone on every
  gateway. A new name is a new zone, and every model's conversations would
  be dealt out again.

### Sync

**What it does.** Makes the cluster match what is stored. The result is in
the cluster's row and in Activity.

**How.** The server renders every object the cluster should have and writes
each with server-side apply. Then it deletes the objects it created earlier
that are no longer wanted. A sync runs after every change, on the button,
and every 5 minutes.

**Why.**

- *The whole state every time, not one change.* A sync that was missed, or
  an object someone edited by hand, is put right by the next one.
- *Server-side apply.* The server owns exactly the fields it writes, and the
  API server reports a conflict when someone else owns one.
- *The 5-minute sync.* It retries a cluster that was down and undoes manual
  edits to the server's own objects.

### Discovery

**What it does.** Reads which models the cluster serves, which backends they
are served from, and how many instances of each are ready.

**How.** Three reads per cluster, every 60 seconds:

1. `/v1/models` on the gateway, when a Gateway URL is set, for the names.
2. The cluster's `AIGatewayRoute` objects, for the backends of each model
   and the listeners each route is attached to.
3. The `LLMInferenceService` objects, for the ready instances.

**Why.**

- *If one read fails, the poll changes nothing.* Saving model names without
  their backends would remove quota policies that are in place. "Could not
  ask" must never be stored as "nothing is running".
- *A model that disappears keeps its quotas.* Only its endpoint on that
  cluster is removed. Deleting the model would delete the tenants' quotas
  with it, and the model may be back after a rollout.

### Self-test

**What it does.** Proves on one cluster, with real requests, that keys,
quotas, counters and (for a fleet cluster) stickiness work.

**How.** It creates a temporary tenant with a key and a quota of one token,
sends a request, waits for the counter, expects the next request to be
refused, resets the counter, expects an answer again, and deletes the
tenant. For a model with an entry route it also sends three requests with
one session ID and checks that one site served all three.

**Why.** Several parts of the gateway's behaviour are documented but can
only be checked with traffic: that the client ID is forwarded, that all keys
of a tenant share one bucket, and that the counter names the Usage page
computes are the ones the gateway writes.

## Models

### Discovered models and models added by hand

**What it does.** A discovered model is one a cluster already exposes; the
server leaves its route and backends alone. A model added by hand gets a
`Backend`, an `AIServiceBackend` and an `AIGatewayRoute` from the server.

**Why.** Routes that a cluster's chart owns have one writer already. Two
writers for one object undo each other's changes.

### Default quota and cost expression

**What it does.** The default quota is a pool shared by every tenant that
has no quota of its own on the model. The cost expression says how many
tokens a request is charged, for example output tokens counted four times.

**How.** Both are fields of the model's `QuotaPolicy`.

**Why.** The default pool is 1 token per day. A tenant without a quota is
therefore refused as soon as the model has any quota, in place of drawing
from an unlimited pool.

### Entry route

**What it does.** With it on, any site's gateway takes a request for the
model and sends the conversation to one of the sites that serve it.

**How.** The server writes four objects named `fleet-<model>` on every fleet
cluster: a `Backend` with one endpoint per site, an `AIServiceBackend`, an
`AIGatewayRoute` on the client listener, and a `BackendTrafficPolicy` with
the routing rules. A fifth, an `EnvoyPatchPolicy` named `fleet-<model>-retry`,
changes two things in the route the gateway generates.

**Why.**

- *A hash on the session header.* Requests of one conversation go to one
  site, so the site's prompt cache is reused.
- *Only the session ID is hashed, not the tenant.* Hashing the tenant would
  pin a customer to one site.
- *Two session headers.* Claude Code sends `x-claude-code-session-id` and
  Open WebUI sends `x-openwebui-chat-id`. A request is hashed on the ones
  it carries.
- *The patch.* When a site answers 503, the gateway picks another. Envoy
  Gateway allows five picks, and with a hash five are too few: for one
  conversation in seven, all five landed on the failing site, and the
  client got its 503. Envoy Gateway has no setting for the number, so the
  server patches it to 20. On a test gateway that took the errors from 31
  of 200 requests to 0 of 400.
- *The patch also sets the Host.* Envoy Gateway forwards a request with the
  site's peer host as its `Host`. A peer listener answers for the peer
  server name, so it would return 404, while the health check keeps
  passing. The patch makes the forwarded request carry the peer server
  name.
- *One patch per model.* Envoy Gateway applies the patches of one policy
  together. In a shared policy, one model whose route is not there yet
  would undo the patch for every model.
- *The same sites, weights and order on every cluster.* Two gateways with
  different lists would send one conversation to two sites. The *fleet
  revision* on the Clusters page shows which cluster is behind.
- *Off by default, per model.* A cluster's own route for the model on the
  client listener is older and wins, so the switch is turned on model by
  model as the clusters are moved over.
- *A route that cannot be rendered is left as it is.* If the server has no
  `FLEET_DOMAIN`, or a model has no site left, the server removes nothing.
  A missing setting must not look like "switched off". The model's quotas
  are still applied meanwhile.
- *A fleet cluster cannot be deleted.* Its gateway would keep routes and
  keys that nobody updates or revokes. Leaving the fleet removes them.

### Site weights

**What it does.** Each site gets conversations in proportion to what it can
serve of the model.

**How.** A site's capacity is its ready instances times the capacity of one
instance, which you declare on the `LLMInferenceService` with
`aigw-ui.io/capacity-per-instance`. The weight is the capacity times 100.

**Why.**

- *The capacity of one instance is declared, not measured.* A multi-node
  instance is one instance and can be worth several single-node ones. Only
  a benchmark can tell.
- *Up by one instance per poll.* A site that comes back gets its
  conversations back gradually, not all at once onto a cold cache.
- *Down only after two polls agree.* One failed readiness check then moves
  no conversation.
- *Unknown keeps the last weight.* A cluster that cannot be asked may be
  serving normally.
- *Never 0.* Envoy rejects a weight of 0, and Envoy Gateway then stops
  publishing every change to that gateway, key revocations included. A site
  with nothing ready stays at 1 and its health check keeps traffic off it.
- *No weights for a single site.* There is nothing to weigh, and with
  weights in place Envoy AI Gateway up to 1.2.0 attaches no quota to the
  route. A model with one site therefore keeps its quotas on the entry
  route. With a second site the weights are needed, the quota is not
  enforced, and the model's row shows a warning. The fix is proposed
  upstream in
  [agent-router#2833](https://github.com/theagentrouter/agent-router/pull/2833).
- *Once per round, for the whole fleet.* Working the weights out after each
  cluster would send the gateways several lists in a row, and every list
  moves conversations.

### Best-effort when a budget is spent

**What it does.** A model can be set to keep answering a tenant whose budget
is spent. The tenant's requests are then sent as the class `best-effort`: a
site serves them at once when it has room, queues them behind every other
request when it has not, and drops them first. A site that has no room
answers 429 and the request is tried at the next site. Tenants within their
budget run as `standard`. When the quota's window ends, the tenant is back
to `standard` by itself.

**How.**

1. Every 15 seconds the server reads the counters the Usage page reads. A
   tenant that has used 90% of its quota on the model is recorded as "in
   overage" until the end of its window.
2. A sync then lists the tenant in a second entry route, `fleetbe-<model>`.
   That route matches the model *and* the tenant's client ID, which makes it
   more specific than the model's entry route, so it wins for this tenant.
3. Its backend sets the header `x-llm-d-inference-objective: best-effort`.
   The model's own entry route sets `standard`. Both replace whatever a
   client sent.
4. The route has a `QuotaPolicy` of its own. It counts every tenant under
   the same rule as the model's policy and refuses nobody.

**Why.**

- *The hub chooses the route ahead of the request.* The gateway checks the
  budget in a filter that answers 429 itself, before a route is chosen. A
  fallback backend or a retry never sees that request, and a header set on a
  backend is a fixed value that cannot depend on the budget.
- *90%, not 100%.* The tenant should be moved before the gateway starts
  refusing. A tenant that uses its last 10% within one interval still gets
  429 until the move has reached the gateways, about 15 seconds.
- *Until the window ends.* The counter's name contains the start of the
  window, so the next window starts at 0 and the tenant is within budget
  again without anybody doing anything.
- *Nobody is moved when Redis cannot be read.* "Could not ask" is never
  taken as "has used everything", or as "has used nothing".
- *Only hourly and daily quotas.* A window of a second or a minute is over
  before the loop and a sync can act. Dry-run quotas never refuse, so there
  is nothing to move.
- *Counted apart.* What a tenant uses as best-effort is shown on the Usage
  page next to its budget, not added to it.
- *The route is there only while a tenant is listed.* Its retry patch names
  the route, and a patch for a route that does not exist is reported as not
  programmed.
- *At most 200 tenants per model.* The list is one regular expression in the
  gateway's route table. Above 200, the tenants whose budget is spent are
  listed first, then tenants without a quota, and the model's row shows a
  warning.
- *One sync per look.* However many tenants moved in the same 15 seconds,
  the Activity page gets one line for them and the clusters one sync. The
  periods themselves are on the Usage page.
- *A sync after every start.* A period can end while the server is not
  running. It cannot know, so its first look has the clusters synced once.
- *A changed quota or a reset ends it.* The tenant has a new budget, so it
  is judged against that from the start.
- *Every cluster gets the same list.* The fleet revision covers it, so the
  Clusters page shows a cluster that is behind.
- *The hub creates the class on the serving sites.* The header names an
  `InferenceObjective`, the serving stack's word for a request class. For a
  model in best-effort mode the hub writes one called `best-effort`, with
  priority -1, next to the model's `InferencePool` on every cluster that
  serves it, and removes it when the model goes back to refusing. A request
  without a class has priority 0, and the scheduler drops what is below 0
  first when a pool is full.
- *The pool is read, not configured.* KServe creates the pool for an
  `LLMInferenceService` that has a scheduler and reports it in the
  service's status. The hub reads it at every poll, where it already reads
  the ready instances. A model served without a scheduler has no pool: the
  model's row then shows a warning, and requests sent as best-effort are
  served there like any other.
- *No class called `standard`.* The entry route marks the other requests
  `standard`, and no such class is created: a name the site does not know
  gives priority 0, which is what `standard` should have. A release that
  defines its own `standard` keeps it.
- *One class per namespace.* A class is found by name within a namespace.
  Where two best-effort models have their pools in one namespace, only the
  first pool by name gets the class. Give each model its own namespace.

### Drain and undrain

**What it does.** Drain takes one site out for one model before
maintenance. Undrain brings it back.

**How.** The weight steps down one instance per poll to 1, and the site then
leaves the model's list. Undrain lists it again at 1 and it ramps up.

**Why.**

- *Step by step.* Each step moves only the conversations of one instance.
- *The last site cannot be drained.* The model would be unreachable.

### Quota and the cluster's own route

**What it does.** With the entry route on, requests are counted on the entry
route. A cluster's own route for the model keeps its quota for as long as
clients can still reach it.

**How.** Discovery reads which listeners each of the cluster's routes is
attached to. A backend reached from the client listener or the whole
Gateway keeps its `QuotaPolicy`. A backend reached only from the peer
listener gets none.

**Why.**

- *Kept on the client side.* The older route wins, so without a quota there
  every tenant's limit would be gone the moment the switch is turned on.
- *Removed on the peer side.* The gateway that took the request has charged
  it already. Charging again at the serving site would count it twice.

## Tenants

### Create a tenant

**What it does.** A tenant is a team or an application. It owns keys and
quotas. Disabling it stops all its keys at once.

**Why.** Quotas belong to the tenant, not to a key, so a team can rotate
keys without losing or doubling its budget.

### Create and revoke an API key

**What it does.** A new key is shown once. Revoking a key removes it from
every cluster on the next sync, a few seconds later.

**How.** The key is stored encrypted and written into one Secret per
cluster, under `data`. Each sync also compares the Secret on the cluster
with the keys it should hold and removes any other entry.

**Why.**

- *Encrypted, not hashed.* Every sync has to write the keys to the clusters
  again, so the server needs the keys themselves.
- *`data`, not `stringData`.* Server-side apply cannot remove an entry that
  came in through `stringData`, and the gateway trusts every entry: a
  revoked key would keep working.

### Why a new quota needs the route to change

**What it does.** Every route the server renders carries the annotation
`aigw-ui.io/quota-revision`, which changes when the model's quota rules do.

**Why.** In Envoy AI Gateway 1.1.0 a tenant's rule is enforced through an
entry in the proxy's route, and that entry is only written when Envoy
Gateway builds the route again. A changed `QuotaPolicy` alone does not make
it do so. A quota added to a tenant whose key already existed was therefore
neither counted nor enforced until something else changed. The controller
copies a route's annotations to the `HTTPRoute` it generates, so the
annotation makes that object change, and the route is built with the new
rule. On a test gateway such a quota was enforced 12 seconds later; without
the annotation, not at all. A route that a cluster's own chart renders has
no such annotation: there a new quota takes effect with the next key change.

### Set a quota

**What it does.** Limits the tokens a tenant may use on a model per second,
minute, hour or day. A *dry run* quota counts and never refuses.

**How.** One rule per tenant in the model's `QuotaPolicy`. The rule matches
every client ID of the tenant, so all its keys draw from one bucket. The
gateway's rate limit service counts in Redis.

**Why.**

- *One Redis for all clusters.* The counters then add up across sites and a
  tenant has one budget, not one per site.
- *A rule keeps its position for life.* The position is part of the
  counter's name. If rules moved up when one is removed, other tenants'
  counters would restart. A removed rule leaves a placeholder.

## Usage

**What it does.** Shows how much of each quota is used in the current
window, and can reset a counter.

**How.** The server computes the name of each counter the gateway writes
and reads them all from Redis in one call. A reset deletes the counter.

**Why.**

- *Read from the gateway's own counters.* There is no second count that
  could disagree with what the gateway enforces.
- *Reset is off unless `redis.allowReset` is set.* Reading needs no write
  access to Redis.

## Activity and audit

**What it does.** Activity lists every change with its result on each
cluster. The audit log lists every request that changed something, with who
asked.

**Why.** They answer two questions. Activity: did the change reach the
clusters, and what did the gateway make of it? Audit: who asked for it, and
what did the server answer? Everyone signs in with the one admin token, so
the name you enter at sign-in is what tells people apart.
