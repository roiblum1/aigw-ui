# Using capacity that would sit idle

A quota is a budget: a tenant may use so many tokens of a model per hour or
per day. A strict budget wastes capacity. When one tenant has spent its
budget and the others are quiet, the model has room and the tenant still
gets 429.

There are two ways to let that tenant continue: the **shared pool** and
**best-effort**. They answer the same need in different places, and this
page explains each and then puts them side by side.

Both are set per model, on the **Models** page.

The examples count in tokens. For a model with prices the budgets and the
pool are in dollars, and everything else on this page is the same. See
[charging by cost](pricing.md).

## Shared pool

**What it does.** The model gets one extra budget that every tenant can draw
from. A tenant past its own quota keeps being served, exactly like everyone
else, until the pool is spent too.

**How.** Every request is charged to two buckets: the tenant's own quota and
the model's shared pool. The gateway lets it through while *either* bucket
has tokens left.

| Shared pool | Effect |
|---|---|
| 1 token (the default) | Strict. A tenant stops at its own quota |
| What the model can serve in the window | A tenant can go past its quota while the model as a whole is under that number. Once the pool is spent, only tenants with quota left are served |

With the pool set to the model's capacity, a tenant's quota is its
guaranteed share, and whatever nobody is using goes to whoever asks first.

**An example.** A model can serve 1,000,000 tokens per hour. Tenant A has a
quota of 300,000 and tenant B of 300,000. The pool is 1,000,000.

- A uses 500,000 and B uses 100,000. The pool holds 600,000, so A is served
  the whole time, 200,000 past its quota.
- A uses 800,000 and B then uses 200,000. The pool is now spent. A is past
  its quota and gets 429. B is still under its 300,000 and is served.

**Things to know.**

- The pool is a number you set. It does not follow what the model can serve
  right now: with half the instances down, the pool is as large as before.
- A request past a tenant's quota is served like any other. The model
  cannot tell it from a request within budget, so under load it competes
  with them on equal terms.
- The pool is a hard limit as well. When it is spent, a tenant past its
  quota gets 429 until the window ends.
- A tenant without any quota on the model uses the pool alone.
- The guarantee holds only while the quotas add up to no more than the
  pool.
- The pool and the quotas are counted apart, each in its own window. Use
  the same window for both.
- It works on every route: a cluster's own route and the entry route.

## Best-effort

**What it does.** A tenant whose budget is spent is not refused. Its
requests are sent on as a lower class, `best-effort`, and are no longer
counted against its budget. Tenants within their budget run as `standard`.

What a site does with the class is decided by the model's scheduler on that
site, not by the hub. With the scheduler settings KServe 0.21 ships, the
class changed nothing in our test: best-effort requests waited in the same
queue as the others and none was dropped. Read
[what a site does with the class](#what-a-site-does-with-the-class) before
you rely on best-effort to protect the tenants within their budget.

**How.**

1. Every 15 seconds the hub reads the usage counters. A tenant that has used
   90% of its quota on the model is recorded as past its budget until the
   end of the quota's window.
2. The next sync lists the tenant in a second entry route for the model.
   That route marks every request with
   `x-llm-d-inference-objective: best-effort`, counts it, and refuses
   nobody.
3. If a site refuses the request with 429, it is tried at the next site.
4. When the window ends, the tenant is served as `standard` again. Nobody
   has to do anything.

**An example.** Same model, A and B with 300,000 each, best-effort on.

- A reaches 270,000 (90%). Within about 15 seconds its requests go out as
  `best-effort`. The model is quiet, so they are answered as fast as before.
- B and others get busy and the sites fill up. On a site whose scheduler
  acts on the class, A's requests wait behind theirs. On a site with
  KServe's default settings, A's and B's requests wait in one queue and B
  is slowed down by A.
- The hour ends. A is `standard` again with a full budget.

**Things to know.**

- The hub only marks the request. Whether a marked request waits longer or
  is dropped is up to each site's scheduler.
- What a tenant uses as best-effort is shown on the **Usage** page next to
  the budget and is not added to it. It has no upper limit unless you set
  one: **Best-effort limit** in the model's row. A tenant at the limit is
  taken off the best-effort route and refused until its window ends. See
  [a limit on best-effort use](pricing.md#a-limit-on-best-effort-use).
- The move takes up to 15 seconds plus a sync. A tenant that spends its last
  10% faster than that gets 429 for a few seconds.
- Only hourly and daily quotas are moved. A window of a second or a minute
  is over before the hub can act.
- At most 200 tenants per model can be past their budget at once.
- With *Best-effort, also without a quota*, tenants that have no quota on
  the model are always served as best-effort.

**What it needs.**

- The model's **entry route** is on.
- The hub can read the counters (`redis.url` is set).
- The model's `LLMInferenceService` has a scheduler on every serving
  cluster. The hub then creates the class `best-effort` there by itself,
  next to the model's `InferencePool`, and removes it when the mode is
  switched off. Nothing has to be added to the model's release.

## What a site does with the class

The hub's part ends when the request reaches the site with
`x-llm-d-inference-objective: best-effort` and the site has an
`InferenceObjective` of that name with priority -1. The rest is done by the
model's scheduler, the endpoint picker that KServe deploys for an
`LLMInferenceService`.

**What we saw.** On OpenShift 4.22 with KServe 0.21 (endpoint picker
`llm-d-router-endpoint-picker` v0.10.0) and the scheduler settings KServe
writes by default:

- vLLM was limited to two requests at a time. 30 `standard` and 10
  `best-effort` requests were sent together, so up to 28 were waiting.
- No request was refused. 26 `standard` and 6 `best-effort` requests were
  answered, and the others reached the client's timeout of 5 minutes.
- 6 of 10 against 26 of 30 is too few requests to read as a priority.
  The order in which the two classes were answered was not recorded.

**Why nothing was refused.** With KServe's default settings the endpoint
picker does hold best-effort back, in one way: without flow control it uses
its legacy admission, which refuses a request with a priority below 0 (HTTP
429, "system saturated, sheddable request dropped") while it counts the pool
as saturated, and lets every other request through (llm-d router v0.10.0,
`pkg/epp/requestcontrol/admission.go`). Its default saturation detector
(`utilization-detector`) counts a pod as saturated at 5 requests waiting in
vLLM or 80% of its KV cache in use, from the metrics it scrapes. In the test
none was refused, so the picker never counted the pool as saturated, although
vLLM had up to 28 waiting. Why is not known: stale or unread metrics are one
guess [INFERENCE]. The test is not a proof that best-effort is never dropped
on such a site; it may be, under a load that the picker sees.

So on such a site, best-effort means: the tenant keeps being served while the
picker does not see the pool saturated, the use is counted apart from its
budget, and its requests are refused there (429) once it does. The entry
route's best-effort route then tries the next site; when every site is
saturated the tenant gets the 429. Set a
[best-effort limit](pricing.md#a-limit-on-best-effort-use) on the model if an
upper limit matters.

**Not a safety net: the pool's `FailOpen`.** The `InferencePool`s KServe
creates say `failureMode: FailOpen`, but Envoy AI Gateway up to 1.2.0 builds
the pool's ext_proc filter with `failure_mode_allow: false` whatever the pool
says (`internal/extensionserver/inferencepool.go:347` in v1.2.0). With the
picker down, the site answers 500 to every request of the model. The entry
route tries the next site on a 500 since 0.29.0; with one site the client gets
it. Run two replicas of the picker where that matters.

**What the hub checks.** Each sync warns in the task message when, on a
pool that serves a best-effort model, the scheduler runs without flow
control, or with it but takes the regular tenants oldest first (see
[tenants in turn](#tenants-in-turn)), the site's own class `standard` does
not rank above `best-effort` or is below 0, or the class `best-effort` is for
another pool of the same namespace. See
[architecture](architecture.md#best-effort-route).

**What should make the class count.** Flow control: with it, requests wait in
the picker when the pool is saturated, ordered by priority, and the lowest
band is refused first. It is switched on in the scheduler's settings, which
belong to the model's chart. Use the `concurrency-detector`, which counts
the requests the picker itself has in flight, rather than the default
detector, which goes by scraped metrics and did not see the test's queue:

```yaml
# LLMInferenceService
spec:
  router:
    scheduler:
      config:
        inline:
          apiVersion: llm-d.ai/v1alpha1
          kind: EndpointPickerConfig
          featureGates: [flowControl]
          plugins:
            - type: concurrency-detector
              parameters:
                concurrencyMode: requests
                maxConcurrency: 2    # per endpoint, not per pod: see below
            # plus the plugins the model uses today
          flowControl:
            saturationDetector:
              pluginRef: concurrency-detector
            defaultRequestTTL: 300s  # left out, 60s: see below
            defaultNegativePriorityBand:
              maxRequests: 4         # best-effort requests that may wait; more are refused at once
          # plus the scheduling profile the model uses today
```

**How many at once: `maxConcurrency`.** It is counted per endpoint, and an
endpoint is each port the model's `InferencePool` lists: a pod with eight
data-parallel ranks is eight endpoints. The picker holds requests back as
the pool fills toward endpoints × `maxConcurrency` in flight, and only
requests it holds are ordered by class and taken in the tenants' turns.
Left out, it is 100 per endpoint (llm-d router v0.10.0): a pool of 16 rank
endpoints would let about 1,600 requests through first, and in practice
nothing would be held back.

- Not more than each rank's `--max-num-seqs`: above it vLLM queues the
  requests itself, first come first served, where neither the classes nor
  the tenants' turns reach.
- Lower, to keep the answers fast: the hub's capacity measurement of the
  model finds the most requests in flight one instance takes within the
  latency targets ([capacity](capacity.md#how-a-measurement-works)). Divide
  it by the instance's endpoints. If 32 in flight is where the measurement
  stops meeting its targets on a pod of 16 rank endpoints, that is 2.

**How long a request may wait: `defaultRequestTTL`.** Left out, it is 60 s,
and a request that waited longer is refused with an error the client may
retry. On the lab one request already waited 45 to 48 s (below), and a
coding agent's prompt of 100,000 tokens takes a while to be served once its
turn comes. Set it above the longest wait you accept, or to `0s` for none:
the client's own timeout then ends the wait, and the bands' `maxRequests`
bound the queue.

**How many best-effort requests may wait: `defaultNegativePriorityBand`.**
A template for every priority below 0, which is where the hub's class
`best-effort` is (`-1` by default). A small `maxRequests` refuses
best-effort requests at once when the pool is full instead of queuing
them; `0` counts as unset, which is 5,000. A band of `priorityBands` with
`priority: -1` does the same for that one priority.

**Before flow control: one ext_proc filter per pool.** Envoy AI Gateway up
to 1.2.0 adds the pool's ext_proc filter once per route match that sends to
the pool, and every copy runs: a KServe route with 16 matches made 16
scheduling passes for one request (agent-router#2595, merged 2026-10-08, in
no release yet). Each pass would enter the request into flow control again.
Wait for a release with that fix, or keep a pool behind a single match (the
model charts' peer route has one).

**Flow control on the lab (2026-10-17, one CPU replica of `qwen-tiny`).**
Six best-effort requests, then one from each of two regular tenants, three
times: the picker sent two best-effort requests on and queued four. The
regular requests got their first token after 15 to 23 s, the queued
best-effort ones after 26 to 57 s; the picker's metrics showed 24.2 s of
waiting on average at priority −1 against 14.5 s at 0. Repeat the test on
each model before relying on it: limit vLLM with `--max-num-seqs`, send both
classes together, and compare how long each waits.

## Tenants in turn

Within one class the picker queues each tenant on its own: the hub's token
check writes the tenant into `x-llm-d-inference-fairness-id`, the flow's ID.
Which flow is served next is the band's **fairness policy**, and the
default of llm-d router v0.10.0 is `global-strict-fairness-policy`, which
ignores the flows and serves the oldest request first
(`pkg/epp/flowcontrol/registry/config.go`, `DefaultFairnessPolicyRef`). On the
lab a tenant's single request waited 45 to 48 s behind another tenant's six
(2026-10-17). `round-robin-fairness-policy` takes the flows in turn, so one
tenant's backlog does not hold up the others.

Declare the plugin and name it on the band of priority 0, the class
`standard` of every regular tenant, in the scheduler config of the model's
chart (beside the flow control settings above):

```yaml
          plugins:
            - type: round-robin-fairness-policy
            - type: fcfs-ordering-policy
            # plus the concurrency-detector and the plugins the model uses
          flowControl:
            saturationDetector:
              pluginRef: concurrency-detector
            defaultPriorityBand:
              fairnessPolicyRef: round-robin-fairness-policy
              orderingPolicyRef: fcfs-ordering-policy
            # or a band of its own for priority 0:
            # priorityBands:
            #   - priority: 0
            #     fairnessPolicyRef: round-robin-fairness-policy
            #     orderingPolicyRef: fcfs-ordering-policy
```

A band of priority 0 of its own that names no fairness policy runs the
default again. A request with a plain API key carries no fairness ID; all
such requests are one flow. The sync warns about a pool of a best-effort
model whose band of priority 0 does not take tenants in turn, through
either `defaultPriorityBand` or a band of priority 0; a pool without
best-effort models is not checked. Turns only matter once requests wait in
the picker, so they need a `maxConcurrency` the pool reaches (above).

A client cannot choose its turn or its class on the routes the hub renders:
the token check writes the fairness ID from the token and drops one a
client sent, and the hub's backends set `x-llm-d-inference-objective`,
replacing a client's. On a model's own route, which the hub does not
render, a client's `x-llm-d-inference-objective` reaches the scheduler as
it is: a site that defines a class above `standard` should not offer such a
route on the listener clients use.

## Side by side

| | Shared pool | Best-effort |
|---|---|---|
| A tenant past its quota is served | while the pool has tokens | always, up to the best-effort limit if one is set |
| Its requests under load | compete equally with requests within budget | as each site's scheduler treats the class. With KServe's default settings: served until the picker counts the pool saturated, then refused there (429) and tried at the next site |
| Protection for tenants within budget | only through the size of the pool | only on sites whose scheduler acts on the class |
| When the extra runs out | 429 from the gateway | 429 from the gateway at the best-effort limit; without a limit it does not run out |
| An upper limit on the extra use | yes, the pool, for all tenants together | optional, per tenant: the best-effort limit |
| How fast it applies | at once | up to 15 seconds plus a sync |
| Quota windows | any | hourly and daily |
| Tenants without a quota | use the pool | refused, or best-effort with the second setting |
| Where it is decided | in the gateway, before the request is sent on | at the serving site |
| Works on | every route | the entry route |
| Needs on the clusters | nothing more | a model served with a scheduler; the hub creates the class |
| Needs on the hub | nothing more | Redis, to read the counters |
| Where the extra use is shown | the pool on the **Overview** | per tenant on the **Usage** page |

## Which one to use

- **Shared pool** when the model has no entry route, when the serving
  clusters have no request classes, or when you want a hard limit on the
  total. It is the simple one: one number, and the gateway does the rest.
- **Best-effort** when a tenant past its budget should keep working and
  its extra use should be seen per tenant, and when the sites' schedulers
  act on the class, so that tenants within budget are not slowed down. With
  KServe's default scheduler settings a site refuses best-effort only once
  its picker counts the pool saturated, which our test never reached; until
  that is seen to work, do not count on it to protect anyone.

**Both on one model.** A request is refused only when the tenant's quota
and the pool are both spent, and a tenant is moved to best-effort at 90% of
its *own* quota whatever the pool holds. With a large pool and best-effort
on, a tenant past its quota is therefore still served, as best-effort. The
pool then no longer limits anything, so keep it at 1 token on a best-effort
model. This follows from how the two are built and has not been run on a
gateway.

## Limits today

- **More than one site.** With an entry route and two or more sites, Envoy
  AI Gateway up to 1.2.0 attaches no quota to the route, so nothing is
  counted and neither way has anything to act on. The model's row shows a
  warning. A fix is proposed upstream
  ([agent-router#2833](https://github.com/theagentrouter/agent-router/pull/2833)).
  A model with one site is not affected.
- **What has been run on a gateway.** For best-effort with one site, and
  with two sites on a gateway built with that fix: the move at 90%, the
  class the site receives, the separate counter, the limit on best-effort
  use, and the return to `standard` after a reset. One request got a 500 in
  the seconds in which the best-effort route was created. Not run: a
  window ending by itself.
- **Under load.** A real model under more load than it could serve did not
  treat best-effort requests differently. See
  [what a site does with the class](#what-a-site-does-with-the-class).

See also [what each action does, and why](how-it-works.md#best-effort-when-a-budget-is-spent)
for the reasons behind the best-effort design.
