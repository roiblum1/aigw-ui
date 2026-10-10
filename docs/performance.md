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
  site calls the same Redis on the hub, twice per request with a quota. Its
  distance from a site is added to every such request.

## Many tenants

One model with a quota for 201 tenants, which is 201 rules in its
`QuotaPolicy`:

| | 2 tenants | 201 tenants |
|---|---|---|
| Added per request, 2 KB | 3.9 ms | 4.2 ms |
| Requests a second | 5,400 | 5,200 |

The number of tenants does not change much.

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

- A real model, and so time to first token and tokens a second through the
  gateway. Streaming was run for correctness, not for speed.
- An entry route over two real sites. The lab's sites are stand-ins.
- More than one proxy pod, and the rate limit service at its own limit.
- Redis in another site than the gateway.
