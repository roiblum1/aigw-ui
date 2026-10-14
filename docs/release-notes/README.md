# Release notes

One file per release, newest first. The version is the chart version and the
image tag (`ghcr.io/roiblum1/aigw-ui:<version>`).

| Version | Date | Summary |
|---|---|---|
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
