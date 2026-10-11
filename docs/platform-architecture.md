# Platform architecture

The design of the whole multi-site LLM platform is one page with its
diagrams:

**[platform-architecture.html](platform-architecture.html)**

Open the file in a browser, or open **Architecture** in the UI, which shows
the same page. It needs nothing from the network.

It covers the hub and the LLM clusters, one request from end to end, how a
conversation finds its site (a weighted hash in the entry gateway, with no
separate service that picks a site), the settings inside each site, how
the routing is configured, and what happens under load and failure.

[architecture.md](architecture.md) is a different document: it describes how
this tool works today.

## The page is a target design

It describes where the platform is going. This is what this tool, the hub's
UI and API server, does of it today:

| Part of the design | Today |
|---|---|
| Tenants, API keys, model catalog, quotas | Built |
| The path of a request | As designed: client, gateway, model. No request passes through this tool. It writes objects to the clusters and reads the counters |
| Prices and budgets in money | Built: prices per model for input, cached input and output. The gateway computes each request's cost, and quotas and usage are in dollars, per minute, hour or day. See [pricing.md](pricing.md). Monthly budgets are not built: the gateway's longest window is a day |
| Live usage | Built, read from the quota counters in Redis |
| Config delivered to every cluster | Built, but differently: this tool applies it itself with each cluster's kubeconfig, not through ACM and Argo CD |
| API keys in Postgres | Stored encrypted, not hashed, because every sync has to write them to the clusters |
| Site weights | Built: the weight is ready instances of the model times the declared capacity of one instance, times 100 and never below 1. Rendered by the hub into each model's entry route. See [architecture.md](architecture.md#site-weights) |
| Priority class | Built per model, not per key: a tenant whose budget is spent is marked as best-effort, or tenants share a pool. The class changes what a site does only when its scheduler acts on priorities. See [optimization.md](optimization.md#what-a-site-does-with-the-class) |
| Usage collector and past usage | Built differently: no collector. The hub reads the quota counters once a minute and keeps what was used per tenant, model and hour. Token counts by kind, such as cached tokens, are not kept |
| Choosing a site | Built: a weighted hash of the session header in each entry gateway, from the entry route this tool renders. There is no separate service that picks a site, and a site is not chosen by what it has cached. See [how-it-works.md](how-it-works.md#cache-aware-routing-between-sites) |
| Peer listener, EPP settings | Not part of this tool. They belong to the cluster charts |

## Keeping it up to date

The page is a single self-contained HTML file with no scripts. To replace it,
overwrite `docs/platform-architecture.html` and rebuild the image: the server
embeds the file (`docs/embed.go`). Remove any tag that loads something from
the internet, such as web fonts; the clusters are disconnected.
