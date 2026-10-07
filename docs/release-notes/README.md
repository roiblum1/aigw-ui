# Release notes

One file per release, newest first. The version is the chart version and the
image tag (`docker.io/roi12345/aigw-ui:<version>`).

| Version | Date | Summary |
|---|---|---|
| [0.3.0](v0.3.0.md) | 2026-10-08 | Partial updates no longer lose data; cost expressions, dry-run quotas, periodic sync |
| [0.2.1](v0.2.1.md) | 2026-10-07 | Stops the gateway controller's "Failed to add finalizer" error |
| [0.2.0](v0.2.0.md) | 2026-10-07 | Model discovery through the gateway's `/v1/models`; quotas per backend namespace |
| [0.1.1](v0.1.1.md) | 2026-10-07 | First release |

To upgrade to any version:

```sh
helm upgrade aigw-ui deploy/chart/aigw-ui -n aigw-ui
```

Database migrations run when the server starts. There is no downgrade path
for the database, so back it up first (see [operations](../operations.md)).
