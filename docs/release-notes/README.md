# Release notes

One file per release, newest first. The version is the chart version and the
image tag (`ghcr.io/roiblum1/aigw-ui:<version>`).

| Version | Date | Summary |
|---|---|---|
| [0.22.1](v0.22.1.md) | not released yet | Fixed: the budget across models was not charged on entry routes, could not be set while a model had a standard, and moved tenants onto each other's counters; a route serving several priced models charges each its prices |
| [0.22.0](v0.22.0.md) | not released yet | A tenant's budget across every priced model, enforced by the gateways through one rate limit policy and counted in the Redis the gateways share, and the models a tenant may call (403); two self-test steps |
| [0.21.0](v0.21.0.md) | not released yet | Wallets removed: a tenant's budgets are per model only, as before 0.20.0; a wallet bucket stays as the tenant's budget on its model, so no gateway limit changes |
| [0.20.3](v0.20.3.md) | not released yet | Fixed: the self-test's counter step warned on a correct count, because the revoked-token step before it added refused requests to the counter |
| [0.20.2](v0.20.2.md) | not released yet | Fixed: a revoked token could work again for up to a minute after it expired; the self-test's expired token was inside the gateway's 60-second allowance |
| [0.20.1](v0.20.1.md) | not released yet | Fixed: a hub not allowed to list LLMInferenceServiceConfigs now says so; the 0.16 upgrade notes name that permission |
| [0.20.0](v0.20.0.md) | not released yet | Wallets: one budget across every priced model, per hour or per day, enforced by the hub; a spent wallet refuses the tenant on every priced model until the window ends, or serves it as best-effort |
| [0.19.0](v0.19.0.md) | not released yet | A model's standard budget: every tenant up to it through one rule on the tenant header, a tenant's quota an exception that adds to it; starts at 00:00 UTC; needs tokens only. Fixed: first prices turned an unset best-effort limit into 1 credit |
| [0.18.0](v0.18.0.md) | not released yet | The hub issues tokens: a new key is a JWT the hub signs, which every gateway checks against the hub's public keys; API keys and tokens are both taken until the last API key is revoked; revoking a token refuses it everywhere until it expires |
| [0.17.0](v0.17.0.md) | not released yet | The budget planner: what the fleet serves a day at the models' prices, what can be given and what is left, which model is saturated at its busiest hour, an overcommit suggestion, and Split |
| [0.16.0](v0.16.0.md) | not released yet | Every LLMInferenceService measured on its own with one versioned benchmark profile: spec hash, a sweep that doubles then bisects, drained or live, a check for other traffic, prefill/decode through the peer listener, and a campaign window |
| [0.15.0](v0.15.0.md) | not released yet | Market prices: a model's default price is its OpenRouter price, from a snapshot built into the server, matched by Hugging Face id, annotation or by hand; nothing changes a price by itself, and the page shows when the market moved |
| [0.14.2](v0.14.2.md) | not released yet | Prices set by hand are the normal path: the Prices dialog shows the other models' prices beside the ones typed; GPU costs are optional and only check the prices |
| [0.14.1](v0.14.1.md) | not released yet | Site weights of the entry route from the measurements, once every deployment of a model is measured; the annotation stays the fallback |
| [0.14.0](v0.14.0.md) | not released yet | What is left to give: each model's budgets against what its GPUs serve a day and an overcommit limit, shown in the quota form, on the Capacity page and the Overview; hourly budgets at their daily maximum; the measured break-even prices in one click |
| [0.13.2](v0.13.2.md) | not released yet | A cluster where the hub may not list pods or nodes says so, instead of showing GPUs as unlabelled |
| [0.13.1](v0.13.1.md) | not released yet | A capacity measurement asks the pod how long a request it takes and stops at once when its prompts do not fit; the Capacity page fits a laptop screen and explains a shape whose GPU is not known |
| [0.13.0](v0.13.0.md) | not released yet | A Capacity page: measure each shape of a model with AIPerf, GPU prices, what a model can be given a day, the overcommit of its budgets, and one price that pays for its GPUs |
| [0.12.3](v0.12.3.md) | not released yet | Entry route health check every 3s and no refusal when every site fails it; quotas, model card and self-test for a fleet model served from a pool only; budget plugins for Claude Code and Oh My Pi |
| [0.12.2](v0.12.2.md) | not released yet | Docs corrected: what best-effort does under load; why a site is not chosen by its cache |
| [0.12.1](v0.12.1.md) | not released yet | A new quota on a model from a cluster's own chart is counted at once |
| [0.12.0](v0.12.0.md) | not released yet | Envoy AI Gateway 1.2.0: current API versions, keys in `x-api-key`, a self-test step for long prompts, measured performance |
| [0.11.0](v0.11.0.md) | not released yet | Usage history with charts on the Overview, a page where a tenant sees its own usage, a limit on best-effort use |
| [0.10.0](v0.10.0.md) | not released yet | Prices per model: quotas and usage in dollars, cached prompts charged less, dry-run |
| [0.9.2](v0.9.2.md) | not released yet | Cost expressions checked by the gateway's rules, two new self-test steps, `kv_transfer_params` removed from requests |
| [0.9.1](v0.9.1.md) | not released yet | Fixes for best-effort mode from a review, code split by responsibility, more tests |
| [0.9.0](v0.9.0.md) | not released yet | A model can serve a tenant past its budget as best-effort; a new quota reaches the proxy without another change |
| [0.8.0](v0.8.0.md) | not released yet | The hub renders each model's entry route for the fleet; no zone weight below 1 |
| [0.7.1](v0.7.1.md) | 2026-10-09 | The image moved to `ghcr.io/roiblum1/aigw-ui` |
| [0.7.0](v0.7.0.md) | 2026-10-09 | Site weights from ready model instances |
| [0.6.1](v0.6.1.md) | 2026-10-09 | **Security fix:** revoked keys and keys of disabled tenants kept working. Architecture page |
| [0.6.0](v0.6.0.md) | 2026-10-08 | Gateway self-test, audit log, counters that survive other tenants' quota changes |
| [0.5.7](v0.5.7.md) | 2026-10-08 | Chart legends stay inside their panel |
| [0.5.6](v0.5.6.md) | 2026-10-08 | Overview explains itself when empty; demo data script |
| [0.5.5](v0.5.5.md) | 2026-10-08 | Live overview charts, shared pool usage, and a restructured codebase |
| [0.4.0](v0.4.0.md) | 2026-10-08 | Live token usage per tenant with reset; task log of every change |
| [0.3.0](v0.3.0.md) | 2026-10-08 | Partial updates no longer lose data; cost expressions, dry-run quotas, periodic sync |
| [0.2.1](v0.2.1.md) | 2026-10-07 | Stops the gateway controller's "Failed to add finalizer" error |
| [0.2.0](v0.2.0.md) | 2026-10-07 | Model discovery through the gateway's `/v1/models`; quotas per backend namespace |
| [0.1.1](v0.1.1.md) | 2026-10-07 | First release |

[0.4.0](v0.4.0.md) and [0.5.5](v0.5.5.md) list everything that has not been
verified against a real gateway yet. [0.6.0](v0.6.0.md) says which of those
the new self-test checks for you.

To upgrade to any version:

```sh
helm upgrade aigw-ui deploy/chart/aigw-ui -n aigw-ui
```

Database migrations run when the server starts. There is no downgrade path
for the database, so back it up first (see [operations](../operations.md)).
