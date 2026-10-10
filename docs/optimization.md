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
requests are sent on as a lower class, `best-effort`. A site serves them at
once when it has room. When it has not, it queues them behind every other
request and drops them first. Tenants within their budget run as `standard`
and are not held up by them.

**How.**

1. Every 15 seconds the hub reads the usage counters. A tenant that has used
   90% of its quota on the model is recorded as past its budget until the
   end of the quota's window.
2. The next sync lists the tenant in a second entry route for the model.
   That route marks every request with
   `x-llm-d-inference-objective: best-effort`, counts it, and refuses
   nobody.
3. A site with no room for best-effort work answers 429, and the request is
   tried at the next site.
4. When the window ends, the tenant is served as `standard` again. Nobody
   has to do anything.

**An example.** Same model, A and B with 300,000 each, best-effort on.

- A reaches 270,000 (90%). Within about 15 seconds its requests go out as
  `best-effort`. The model is quiet, so they are answered as fast as before.
- B and others get busy and the sites fill up. A's requests are now queued
  behind theirs, and some are dropped. B, still within budget, sees no
  difference.
- The hour ends. A is `standard` again with a full budget.

**Things to know.**

- The decision is made by the sites, per request, from what they can serve
  at that moment. There is no number to set and none to keep up to date.
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

## Side by side

| | Shared pool | Best-effort |
|---|---|---|
| A tenant past its quota is served | while the pool has tokens | while a site has room |
| What "room" is | a number you set per model | what the sites can serve at that moment |
| Its requests under load | compete equally with requests within budget | wait behind them and are dropped first |
| Protection for tenants within budget | only through the size of the pool | by class, at the model |
| When the extra runs out | 429 from the gateway | the site queues, then answers 429; the request is tried at the next site |
| An upper limit on the extra use | yes, the pool | no |
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
- **Best-effort** when tenants within budget must not be slowed down by
  the ones past it, and when the model's capacity changes during the day.
  It costs more to set up and gives a better result under load.

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
- **What has been run on a gateway.** For best-effort with one site: the
  move at 90%, the class the site receives, the separate counter, and the
  return to `standard` after a reset. Not run: a model under real load
  dropping best-effort work first, which is the serving stack's part, and a
  window ending by itself.

See also [what each action does, and why](how-it-works.md#best-effort-when-a-budget-is-spent)
for the reasons behind the best-effort design.
