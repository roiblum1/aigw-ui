# Performance

What the gateway adds to a request, what one proxy pod carries, and what the
hub itself costs. The hub is not on the request path: a request passes the
gateway on its cluster and nothing of this tool.

All numbers are from one lab cluster (OpenShift 4.22, virtual machines, one
proxy pod) in October 2026. They show sizes and proportions. Measure on your
own hardware before sizing production.

## How it was measured

- **A model server that answers at once.** nginx returning a fixed chat
  completion, so that the time measured is the gateway's and not a model's.
- **Load from inside the cluster**, from a pod next to the gateway, with
  fortio. A 1 MB prompt was sent with a small Python loop.
- **Added time** is the time through the gateway minus the time straight to
  the model server, one request after the other on one connection.
- **Requests a second** is 32 connections sending as fast as they are
  answered, for 20 seconds.
- Averages are given. Runs differ by about 10%.

## What the gateway adds to one request

| Prompt | No quota on the model | Quota in tokens | Quota in dollars |
|---|---|---|---|
| 2 KB | 2.3 ms | 3.9 ms | 4.2 ms |
| 100 KB | 4.6 ms | 6.3 ms | 6.6 ms |
| 1 MB | 15 ms | 17 ms | 17 ms |

Envoy AI Gateway 1.2.0. Straight to the model server the same requests took
0.25 ms, 0.5 ms and 12 ms.

- **A quota costs about 1.5 ms.** The gateway asks the rate limit service
  before the request and tells it the cost after the answer. Each is a call
  to Redis.
- **Dollars cost the same as tokens.** Working out a price from three
  token counts is not measurable.
- **The size of the prompt costs more than the quota.** The gateway reads
  the whole body to find the model's name. On a model with an entry route
  the body is buffered twice: at the entry gateway and at the serving one.
- **A model takes seconds.** Against that, 4 ms is nothing a client sees.

## What one proxy pod carries

| | Requests a second | CPU used at that rate |
|---|---|---|
| No quota on the model | 9,400 | Envoy 7.8, extproc 8.7 |
| Quota in tokens | 5,400 | Envoy 5.7, extproc 3.9, rate limit service 3.9 |
| Quota in dollars | 5,400 | Envoy 6.0, extproc 2.5, rate limit service 2.1 |

These are small requests answered at once, so they are an upper bound: a
real model holds a connection for seconds, and the number of open requests
matters more than requests a second.

- **A quota takes about 40% of a proxy pod's throughput.**
- **The proxy needs CPU.** A pod with a request of 100m CPU, which is a
  common default, is throttled long before these numbers. Give a production
  proxy whole cores, and add replicas for more.
- **The rate limit service and Redis are shared.** Every gateway of every
  site calls the same Redis, twice per request with a quota: the check
  before the request, which waits for the answer, and the charge after it,
  which does not. The distance to Redis is added to every such request once
  per check (see below).

## A budget across models, and Redis in another site

Measured on the lab on 2026-10-16 with the policies of 0.22.1: one proxy pod,
a mock model that answers in 0.3 ms, and the rate limit services counting in
a Redis on another cluster, about 33 ms away. Requests one at a time, 300 per
path, three rounds; time until the whole answer, in ms:

| Path | p50 | p99 |
|---|---|---|
| The gateway alone: token, AI gateway, the models rule | 4.3 | 6.6 to 10 |
| With the tenant's budget across models | 37.5 | 45 to 47 |
| With the model's quota | 37.7 | 47 to 65 |
| With both | 69.4 | 78 to 91 |
| With both and a list of models | 69.3 | 75 to 84 |

AIPerf 0.13 against the same mock, streaming, gave the same as time to the
first token: 6.7, 38.7, 39.6 and 71.3 ms at one request at a time; 13, 39, 39
and 70 ms at sixteen.

The same once the sandbox hub ran 0.22.1 and rendered the policies itself,
on the lab's client listener, between a tenant with a quota on the model and
one with the same quota and a budget across models: 38 and 69 ms at p50 one
at a time, 37.6 and 68.4 ms at sixteen (1,500 requests each, two rounds);
AIPerf time to the first token 40 and 71 ms, and 39.5 and 69.4 ms at
sixteen, with no error in 6,400 requests. A tenant without a budget pays
nothing for the policy.

- **Each check is one round trip to Redis, in series.** The quota's rate
  limit filter and the budget's run one after the other, and each asks Redis
  before the request goes on. The charge after the answer is sent once the
  stream has ended and adds nothing to the answer.
- **So the distance to Redis is paid once or twice per request.** With
  Redis in the same site it is about 1.5 ms each (above); 33 ms away it is
  33 ms each. Put the Redis the gateways share in the region of the
  gateways that carry most of the traffic.
- **The rate limit services kept up.** With both checks the gateway served
  1,776 requests a second at 128 at a time, still at 65 ms; the services
  pipeline their calls to Redis, so their pool of 10 connections did not cap
  them.
- **A model's own time hides it.** AIPerf against the lab's model on CPU
  (512-token prompts, 1 to 2 seconds to the first token) could not tell the
  budget's 33 ms from the model's own variation.

## Many tenants

One model with a quota for 201 tenants, which is 201 rules in its
`QuotaPolicy`:

| | 2 tenants | 201 tenants |
|---|---|---|
| Added per request, 2 KB | 3.9 ms | 4.2 ms |
| Requests a second | 5,400 | 5,200 |

The number of tenants does not change much.

Budgets across models, 2026-10-16, on the lab with a listener of 10 routes,
a mock model on a route without a quota, and the shared Redis about 33 ms
away. The tenant measured had the last rule; p50 in ms:

| Tenants with a budget | With a budget | Without | Without, 64 at a time | Routes on the listener | A change in the proxy after |
|---|---|---|---|---|---|
| 4 | 37.4 | 4.4 | 883 a second | 181 KB | 3 s |
| 64 | 37.2 | 4.5 | 899 a second | 891 KB | 6 s |
| 128 | 37.4 | 4.3 | 1,040 a second | 1.7 MB | 8.5 s |
| 256 | 37.5 | 4.7 | 882 a second | 3.2 MB | 11 to 19 s |
| One `Distinct` rule for all | 37.0 | (every tenant has it) | | 142 KB | 2.5 s |

- **A request takes as long with 256 tenants as with 4.** Envoy checks the
  tenant header against every rule of the route, which costs nothing
  measurable here; only the rule that matches asks Redis. A tenant without a
  budget asks nothing.
- **The configuration grows with the tenants:** about 1.2 KB per tenant per
  route, as Envoy's configuration dump counts it (7.3 MB of it in all at 256
  tenants, from 1.4 MB). The proxy's memory grew by 5 MB. Envoy Gateway and
  the AI gateway's controller logged no error up to 256.
- **A change takes longer to reach the proxy**, any change of the gateway
  and not only a budget's: 3 s with 4 tenants, 11 to 19 s with 256.
- **One `Distinct` rule for every tenant** stays at 142 KB and 2.5 s, at the
  same 37 ms per request, but gives every tenant the same amount and makes
  every tenant pay the round trip.
- Not measured: the size of the extension message itself (the dump is JSON;
  the message is protobuf and smaller), and per-model quotas with as many
  tenants, which put their rules on that model's routes only.

## Gateway versions

| | 1.1.0 | 1.2.0 |
|---|---|---|
| Added per request, 2 KB, quota in dollars | 4.1 ms | 4.2 ms |
| Added per request, 1 MB, quota in dollars | 16 ms | 17 ms |
| Requests a second, quota in tokens | 5,600 | 5,400 |
| Requests a second, no quota | 9,350 | 9,370 |

No difference outside the noise. A 1.2.0 built with the fixes for two-site
quotas and for cached tokens on the Anthropic path measured the same.

## A limit that is not about speed

Without a `ClientTrafficPolicy` that sets `connection.bufferLimit`, Envoy
Gateway buffers 32 KiB of a request. A prompt of 29 KB was answered and one
of 35 KB got 413. With the limit at 50Mi, prompts of 1 MB and 3 MB were
answered. The self-test's step "A long prompt is accepted" checks it.

## What the hub costs

| | Measured |
|---|---|
| Reading the usage of 205 quotas (`GET /usage`) | 0.2 s, most of it the way to Redis from outside the cluster |
| The usage history | one such read a minute, and one write to Postgres for the tenants that used something |
| The best-effort loop | one such read every 15 seconds, only when a model serves a spent budget as best-effort |
| Applying a change | one server-side apply per object and cluster; a sync of 22 objects takes a few seconds |

The hub reads counters and writes Kubernetes objects. Its load grows with
the number of tenants, models and clusters, not with the number of
requests. Redis is read with one `MGET` for all counters.

## Not measured

- A real model on GPUs, and so time to first token and tokens a second
  through the gateway. The lab's model runs on CPU.
- An entry route over two real sites. The lab's sites are stand-ins.
- More than one proxy pod, and the rate limit service at its own limit.
