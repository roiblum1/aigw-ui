# Charging by cost

By default a quota is a number of tokens, and every token counts the same.
That is unfair between tenants: a token of an answer costs several times
what a token of a prompt costs to serve, and a prompt the model has seen
before costs far less than a new one.

A model can be given **prices**. Its quotas and its usage are then counted
in dollars, each request by what it cost to serve. The aim is to share the
cost of the hardware fairly, not to earn on it.

The gateway works out what a request costs. A request never passes through
the hub: the hub writes the prices into the model's `QuotaPolicy` and reads
the counters afterwards.

## What a request costs

A model has three prices, each for a million tokens:

| Price | For |
|---|---|
| Input | A prompt token the model had to compute |
| Cached input | A prompt token the model took from its prefix cache |
| Output | A token of the answer. Reasoning tokens are part of the answer |

```
cost = (input - cached) x input price
     + cached           x cached price
     + output           x output price
```

- The result is rounded down, in the tenant's favour.
- A request that fails or is refused costs nothing.
- Writing to the cache is not charged. The model computes those tokens for
  the answer anyway.
- A cached count larger than the prompt cannot be true. The whole prompt is
  then charged as not cached.

**The unit.** The gateway counts in whole numbers, so amounts are kept in
credits of $0.00001. The interface shows dollars. A counter holds at most
$42,949.67 per tenant, model and window, and so does a model's shared pool.

**One more per request.** The gateway adds 1 credit to every request, for
the check it makes before the request. That is $0.00001.

**Until measured**, a cached prompt token is priced at a tenth of an input
token and an output token at four times. The Prices dialog fills these in
from the input price. Both can be changed.

## Setting a price without profit

A price is the cost of an hour of GPU divided by what the GPU serves in
that hour.

1. **Cost per GPU-hour.** The yearly cost of the hardware (its price over
   three or four years, power, cooling, rack, network, licences, the
   people who run it) divided by the number of GPUs times 8,760.
2. **What a GPU serves.** Measure the model under load at the latency you
   promise: input tokens per second plus four times the output tokens per
   second.
3. **Target utilization.** The share of the day you expect the GPUs to be
   busy, for example 0.6.

```
input price per 1M = cost per GPU-hour / (3,600 x tokens per second x utilization) x 1,000,000
```

With $2.50 per GPU-hour, 10,000 weighted tokens per second and 0.6, the
input price is about $0.116, cached $0.0116 and output $0.463. These
figures only show the arithmetic.

**Divide by the target, never by actual use.** If a quiet month raised the
price, tenants would use less, and the price would rise again. Capacity
nobody used is a cost of the platform, not of the tenants who did show up.

## Giving a model prices

On the **Models** page, **Prices** in the model's row.

- **Prices start at 00:00 UTC.** Every quota window (minute, hour, day)
  begins anew at that moment, so no counter ever holds amounts at two
  prices. The server makes the change by itself. A model without any
  tenant quota can also start at once.
- **The first prices convert the model.** Its shared pool and its tenants'
  limits were in tokens. They are converted once, at the input price: a
  limit of 2,000,000 tokens at $0.116 becomes $0.23. A limit never becomes
  less than one credit.
- **The model starts in dry-run.** Every tenant is counted in dollars and
  nobody is refused, not by its own limit and not by the pool. Nobody is
  moved to best-effort either. Use the days in dry-run to look at the
  **Usage** page and to set the limits you want: the converted figures
  are a starting point, valued as if every token were an input token.
- **Switching dry-run off** starts enforcing. It is a switch in the Prices
  dialog and asks first.
- **Later prices** change no limit and leave dry-run as it is. A budget of
  $50 stays $50 and buys fewer or more tokens.

A model without prices stays in tokens. Its cost expression, if it has one,
is used as before. The hub never makes up a price.

## What the price of cached tokens depends on

The cached price only has an effect when the model server reports how much
of a prompt it took from its cache, and reports it truthfully. See
[deployment](deployment.md#before-cached-prompts-are-charged-less) for the
vLLM flags, and run the self-test on a cluster with the model:

- "The model reports cached prompt tokens" shows whether the count arrives.
- "A client cannot choose its tenant" shows that a request is charged to
  the tenant of its key.
- "The tokens are counted where the Usage page reads them" compares the
  counter with what the request should cost at the model's prices.

A cache hit depends on which instance served the request and on what other
tenants sent before. Two equal requests can therefore cost different
amounts. All tenants share one prefix cache, so a tenant can get the lower
price for a prompt another tenant sent first.

## A budget per hour or per day

A tenant's budget on a model has a period: a minute, an hour or a day. The
gateway keeps the period by itself. A new hour starts on the hour and a new
day at 00:00 UTC, and the counter of the new period starts at 0. The hub
has no part in it, so a budget is kept and reset while the hub is down.

There is no period of three hours, and none is planned. The gateway knows a
second, a minute, an hour and a day. A block of three hours would have to
be kept by the hub: it would read the day's counter every 15 seconds and
change a tenant's rule when a block is spent. That adds a delay, a second
place where a budget is enforced, and a budget that stops being reset when
the hub is down. An hourly budget gives a tenant that spent its budget a
shorter wait than a 3-hour block would, with none of that.

| You want | Set |
|---|---|
| A tenant that overspends waits at most an hour | A budget per hour |
| A team that works in bursts, with one cap for the day | A budget per day |
| Both a cap for the day and a shorter wait | Not possible: a tenant has one budget per model, and the gateway lets a request through when any of its buckets has room |

## Usage over time

The counters in Redis hold the running period only. Once a minute
(`config.usageHistoryInterval`) the hub reads them and adds what was used
since its last look to the table `usage_hours` in Postgres: one row per
tenant, model and hour, with the amount used within the budget and the
amount used as best-effort.

- The **Overview** charts it per day or per hour, by tenant or by model.
- A tenant sees its own on [its page](user-guide.md#the-tenants-own-page).
- `GET /usage/history` returns it, see the [API reference](api.md#usage).

What to know about the numbers:

- **They are what the gateway counted.** A row is the growth of a counter,
  so the history and the budgets always agree. It includes the 1 the
  gateway adds for every request.
- **The end of a period is not lost.** When a period ended since the last
  look, the hub reads its counter once more; the rate limit service keeps
  it for some minutes.
- **The hub being down loses little.** Usage from while it was down is
  added at its next look, to the hour of that look. What is lost is the end
  of a period that ended more than a few minutes before the hub came back.
- **A reset is not subtracted.** Resetting a tenant's usage sets its budget
  back; what it used before stays in the history.
- **Money and tokens are kept apart.** A model that gets prices is in
  credits from then on, and its earlier rows stay in tokens.
- **No request is stored**, and nothing of one: only amounts. Rows are kept
  for 400 days and go with their tenant or model when it is deleted.

## A limit on best-effort use

A model that [serves a spent budget as best-effort](optimization.md) can
limit how much a tenant uses that way: **Best-effort limit** in the model's
row. The limit is per period of the tenant's quota. At the limit the hub
takes the tenant off the best-effort route. Its own budget is spent, so the
gateway refuses it until the period ends.

- The hub looks every `config.overageInterval` (15 seconds), so a tenant
  can go past the limit by what it sends in that time and one sync.
- It applies to tenants with a quota on the model. A tenant without one has
  no counter on the best-effort route.
- Keep the model's shared pool at its smallest. With a large pool a tenant
  off the best-effort route is still served from the pool.
- Best-effort use stays free. The limit keeps one tenant from taking all
  the spare capacity, and keeps the demand for more capacity visible.

## What is not charged

- **A stream the client leaves midway.** The model server sends the token
  counts at the end, so the gateway never learns them.
- **Best-effort use.** It is counted apart and shown on the Usage page, and
  it is not taken from the tenant's budget.
- **Requests on a model with an entry route and two or more sites**, until
  the gateway attaches quotas there. See
  [limits today](optimization.md#limits-today).

## Not built yet

- What a tenant saved through cached prompts. The counter holds one total.
- Months. The gateway's longest window is a day.

## Tested

On OpenShift 4.22 with Envoy Gateway 1.9.1 and AI Gateway 1.1.0, one site,
with a stand-in for the model server that reports a repeated prompt as
cached. Prices were 10 credits for an input token, 1 for a cached one and
40 for an output token.

| Request | Cost | Counter grew by |
|---|---|---|
| 100 prompt tokens, none cached, 1 output | 1040 | 1041 |
| The same prompt, 96 cached | 176 | 177 |
| Streamed, the client asks for usage | 540 | 541 |
| Streamed, the client does not ask, 48 of 50 cached | 108 | 109 |

- Two tenants at once: each counter moved for its own requests only.
- In dry-run a tenant at 5742 of a limit of 3000 was still answered.
- With dry-run off that tenant got 429, and a tenant within its limit was
  answered.
- The self-test passes on a priced model.

The same setup, for 0.11.0, with a budget of $0.05 per hour and one of
$0.025 per minute:

- The tenant with the hourly budget was answered twice and then got 429,
  with its counter at $0.05268.
- The tenant with the budget per minute got 200, 200, 200, 429, 429 within
  one minute and 200 in the next. The hub did nothing for that.
- The history held exactly what the counters held, also over the end of two
  periods, after the hub was stopped while requests went on, and after a
  usage reset. Requests were answered while the hub was down.
- A tenant read its own budget and usage with its key. A wrong key, and a
  tenant's key on the admin API, got 401.

Not tested:

- the best-effort limit on a gateway. It is covered by tests of the loop
  and of the database only, because best-effort needs an entry route, and this test had none;

- a real vLLM, and so real cached counts;
- the server making the change at 00:00 UTC by itself. The prices in the
  test started at once, on a model without tenant quotas;
- the conversion of limits on a gateway. It ran against the database only.
