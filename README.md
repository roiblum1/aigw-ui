# AI Gateway Control

A web UI and REST API that manage Envoy AI Gateway (now "Agent Router") on
several OpenShift clusters from one hub.

You run it once, on the hub. You give it a kubeconfig for each LLM cluster. It
then finds the models those clusters serve, lets you create tenants with API
keys and token quotas, and applies the matching gateway resources to every
cluster.

```
            hub cluster                          LLM clusters
   ┌───────────────────────────┐        ┌──────────────────────────┐
   │  UI ─► API server ─► Postgres       │  Envoy AI Gateway        │
   │           │                │  poll  │   AIGatewayRoute (yours) │
   │           ├────────────────┼───────►│                          │
   │           └────────────────┼───────►│   QuotaPolicy, API keys  │
   └───────────────────────────┘  apply  └──────────────────────────┘
```

## What it does

- **Clusters**: register LLM clusters, test the connection, see sync status, preview the YAML before it is applied.
- **Models**: discovered automatically every minute from each gateway's `/v1/models` and its `AIGatewayRoute` objects. Models can also be added by hand.
- **Tenants**: one tenant per team, with API keys that can be issued and revoked.
- **Quotas**: a token budget per tenant per model, per minute, hour or day, rendered as `QuotaPolicy` on every cluster, with an optional dry-run mode and a per-model cost expression.
- **Site weights**: reads how many instances of each model are ready on each cluster and keeps the gateways' zone weights in line, so each site gets traffic in proportion to what it can serve.
- **Usage**: live tokens used per tenant and model in the current window, read from the quota counters in Redis.
- **Activity**: a task log of every change, showing per cluster which objects were created, updated or deleted and whether the gateway accepted them, and an audit log of who asked for what.
- **Self-test**: checks on a real gateway, with a temporary tenant, that a key works, usage is counted, a quota refuses and a reset frees.
- **Architecture**: the design of the whole multi-site platform, with its diagrams, inside the UI.
- **API**: everything the UI does is available over REST for a self-service portal.

## Quick start on OpenShift

```sh
helm upgrade --install aigw-ui deploy/chart/aigw-ui -n aigw-ui --create-namespace

oc get route aigw-ui -n aigw-ui -o jsonpath='https://{.spec.host}{"\n"}'
oc get secret aigw-ui-auth -n aigw-ui -o jsonpath='{.data.admin-token}' | base64 -d; echo
```

Open the URL and sign in with the token. The image is
`docker.io/roi12345/aigw-ui`.

For a disconnected environment, see
[docs/deployment.md](docs/deployment.md#disconnected-install).

## Quick start on a laptop

```sh
make db                                   # Postgres in podman on 127.0.0.1:55432
export ADMIN_TOKEN=$(openssl rand -hex 24)
export ENCRYPTION_KEY=$(openssl rand -base64 32)
make build
DATABASE_URL=postgres://postgres:dev@127.0.0.1:55432/aigw ./bin/aigw-ui
```

Open http://localhost:8080 and sign in with `ADMIN_TOKEN`. For UI work, run
`make dev-ui` next to the server.

## Tests and releases

```sh
go test ./...                # unit tests
hack/test-apiserver.sh       # tests that need a real Kubernetes API server; starts a throwaway one
```

The GitHub workflow runs both on every pull request and push, and pushes the
image to `ghcr.io/roiblum1/aigw-ui`. To release: set the version in
`deploy/chart/aigw-ui/Chart.yaml`, write `docs/release-notes/v<version>.md`,
list it in `docs/release-notes/README.md`, and push the tag `v<version>`. The
workflow refuses a version without a release note.

## Documentation

| Document | Contents |
|---|---|
| [docs/user-guide.md](docs/user-guide.md) | Using the UI: clusters, models, tenants, keys, quotas |
| [docs/platform-architecture.md](docs/platform-architecture.md) | The design of the whole multi-site platform, with diagrams, and what of it is built |
| [docs/architecture.md](docs/architecture.md) | Components, data flows, what is applied to the clusters |
| [docs/deployment.md](docs/deployment.md) | Helm chart, settings, disconnected install, upgrade, uninstall |
| [docs/security.md](docs/security.md) | Where kubeconfigs and keys are kept, and who can read them |
| [docs/api.md](docs/api.md) | REST API reference with examples |
| [docs/operations.md](docs/operations.md) | Backup, restore, troubleshooting |
| [docs/release-notes/](docs/release-notes/README.md) | What changed in each version |

## Repository layout

```
cmd/server/            entry point: reads the config and wires the packages together
internal/config/       settings from environment variables
internal/api/          HTTP API, one file per resource, and static UI serving
internal/store/        Postgres access, one file per resource, and migrations
internal/render/       turns desired state into Kubernetes objects and counter names (pure)
internal/kube/         one cluster: sync (apply, prune), gateway status, discovery
internal/gateway/      reads /v1/models from a gateway
internal/syncer/       background sync and discovery loops, and task results
internal/weights/      decides each site's share of a model's traffic from its ready capacity (pure)
internal/usage/        reads the quota counters in Redis and builds the usage report
internal/selftest/     checks keys, counters and quotas with real requests through a gateway
internal/secretbox/    AES-256-GCM encryption of stored secrets
web/src/pages/         one file per page of the UI
web/src/               shared UI pieces: API client, components, charts
web/src/styles/        styles per feature
deploy/chart/          Helm chart for OpenShift
deploy/offline/        scripts that build and load the offline bundle
docs/                  documentation; the platform design page is embedded in the server
docs/release-notes/    what changed in each version
hack/                  seed-demo.py: demo data for showing the UI without a gateway
Containerfile          image build
```

How the packages depend on each other: `api` calls `store`, `syncer`,
`usage` and `selftest`. `selftest` calls `store`, `syncer`, `usage` and
`gateway`. `syncer` calls `store`, `render`, `kube`, `gateway` and `weights`. `usage` calls
`store` and `render`. `render` depends on nothing, which keeps it easy to
test.

## Status

Built and tested: the UI and API, Postgres storage, model discovery, sync to
clusters, the image, the Helm chart on OpenShift 4.20, and the offline bundle.

Not tested against a real Envoy AI Gateway installation yet. The Self-test
button on a cluster checks most of the open points on your own gateway. See
[docs/architecture.md](docs/architecture.md#not-verified-on-a-real-gateway).

Not built yet: usage history beyond the current window, monthly budgets and grants,
LDAP login. The audit log records a name the caller gives, not a verified
identity.
