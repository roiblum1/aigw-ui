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

So on such a site, best-effort means: the tenant keeps being served, the use
is counted apart from its budget, and under load it competes with everyone
on equal terms. That is the shared pool's behaviour without the pool's upper
limit. Set a [best-effort limit](pricing.md#a-limit-on-best-effort-use) on
the model if that matters.

**Why, from the scheduler's source** (llm-d router v0.10.0; read, not run):

- With the default settings the scheduler holds no request back. It only
  refuses a request with a priority below 0, with 429, when the model
  counts as saturated at the moment the request arrives.
- "Saturated" is read from vLLM's metrics: more than 5 requests waiting or
  the KV cache above 80%. Requests that arrive together are all let in
  before the metric has moved, which is what our test did.
- A request let in is never reordered. vLLM answers its queue in the order
  of arrival, whatever the class.
- A class the scheduler does not know runs with priority 0, and nothing
  reports it. The `InferenceObjective` must be in the pool's namespace and
  name the pool. The hub creates it that way.

So the default gives best-effort one thing only: under steady overload, new
best-effort requests are refused and tried at the next site. It never makes
them wait behind the others.

**Flow control: tried, and not working yet.** The scheduler has a feature
for this, flow control: requests wait in the scheduler and are released in
order of priority while the model has room. It is switched on with
`featureGates: [flowControl]` in the scheduler's settings
(`spec.router.scheduler.config.inline` of the `LLMInferenceService`), which
replace KServe's default settings as a whole. KServe passes them on, and the
scheduler started with flow control and a priority band for -1. Run on the
same lab, with vLLM limited to two requests at a time:

| Settings | Result |
|---|---|
| Flow control with the default saturation detector | Requests were answered, but in the order they arrived: 6 best-effort requests sent at 3 s were all answered before 6 standard requests sent at 5 s. vLLM's own queue held 12 requests, so the scheduler was not holding them back |
| Flow control with `concurrency-detector`, `maxConcurrency: 2` | Broken: one request to an idle model waited 60 s and got 429 (`rejected-ttl-expired`). The scheduler released it, the proxy had no address for it, and the count of running requests never went down |

So on KServe 0.21 with the endpoint picker v0.10.0 we have no settings that
make best-effort wait behind the others. Do not copy either of the above to
a model. What is left to try: the detector's thresholds, a newer endpoint
picker, and the open upstream issue about `concurrency-detector`
([llm-d-router#2881](https://github.com/llm-d/llm-d-router/issues/2881)).

## Side by side

| | Shared pool | Best-effort |
|---|---|---|
| A tenant past its quota is served | while the pool has tokens | always, up to the best-effort limit if one is set |
| Its requests under load | compete equally with requests within budget | as each site's scheduler treats the class. With KServe's default settings: compete equally |
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
  act on the class, so that tenants within budget are not slowed down. On
  sites with KServe's default scheduler settings it protects nobody, and
  the shared pool does the same with less to set up.

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
