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

## What is not charged

- **A stream the client leaves midway.** The model server sends the token
  counts at the end, so the gateway never learns them.
- **Best-effort use.** It is counted apart and shown on the Usage page, and
  it is not taken from the tenant's budget.
- **Requests on a model with an entry route and two or more sites**, until
  the gateway attaches quotas there. See
  [limits today](optimization.md#limits-today).

## Not built yet

- Budgets for a part of the day, and a limit on best-effort use.
- A history of usage. The hub knows the running windows only.
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

Not tested:

- a real vLLM, and so real cached counts;
- the server making the change at 00:00 UTC by itself. The prices in the
  test started at once, on a model without tenant quotas;
- the conversion of limits on a gateway. It ran against the database only.
