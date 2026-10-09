# Release notes

One file per release, newest first. The version is the chart version and the
image tag (`ghcr.io/roiblum1/aigw-ui:<version>`).

| Version | Date | Summary |
|---|---|---|
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
