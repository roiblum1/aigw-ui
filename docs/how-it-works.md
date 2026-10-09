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
the routing rules.

**Why.**

- *A hash on the session header.* Requests of one conversation go to one
  site, so the site's prompt cache is reused.
- *Only the session ID is hashed, not the tenant.* Hashing the tenant would
  pin a customer to one site.
- *The same sites, weights and order on every cluster.* Two gateways with
  different lists would send one conversation to two sites. The *fleet
  revision* on the Clusters page shows which cluster is behind.
- *Off by default, per model.* A cluster's own route for the model on the
  client listener is older and wins, so the switch is turned on model by
  model as the clusters are moved over.
- *A route that cannot be rendered is left as it is.* If the server has no
  `FLEET_DOMAIN`, or a model has no site left, the server removes nothing.
  A missing setting must not look like "switched off".

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
- *Once per round, for the whole fleet.* Working the weights out after each
  cluster would send the gateways several lists in a row, and every list
  moves conversations.

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
