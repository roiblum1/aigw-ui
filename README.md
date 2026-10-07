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
- **Models**: discovered automatically from each cluster's `AIGatewayRoute` objects every minute. Models can also be added by hand.
- **Tenants**: one tenant per team, with API keys that can be issued and revoked.
- **Quotas**: a token budget per tenant per model, per minute, hour or day, rendered as `QuotaPolicy` on every cluster.
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

## Documentation

| Document | Contents |
|---|---|
| [docs/user-guide.md](docs/user-guide.md) | Using the UI: clusters, models, tenants, keys, quotas |
| [docs/architecture.md](docs/architecture.md) | Components, data flows, what is applied to the clusters |
| [docs/deployment.md](docs/deployment.md) | Helm chart, settings, disconnected install, upgrade, uninstall |
| [docs/security.md](docs/security.md) | Where kubeconfigs and keys are kept, and who can read them |
| [docs/api.md](docs/api.md) | REST API reference with examples |
| [docs/operations.md](docs/operations.md) | Backup, restore, troubleshooting |

## Repository layout

```
cmd/server/          entry point
internal/api/        HTTP API and static UI serving
internal/store/      Postgres access and migrations
internal/render/     turns desired state into Kubernetes objects
internal/kube/       applies, prunes and discovers on one cluster
internal/syncer/     background sync and discovery loops
web/                 React UI (Vite, Tailwind CSS)
deploy/chart/        Helm chart for OpenShift
deploy/offline/      scripts that build and load the offline bundle
Containerfile        image build
```

## Status

Built and tested: the UI and API, Postgres storage, model discovery, sync to
clusters, the image, the Helm chart on OpenShift 4.20, and the offline bundle.

Not tested against a real Envoy AI Gateway installation yet. See
[docs/architecture.md](docs/architecture.md#not-verified-on-a-real-gateway).

Not built yet: usage history, monthly budgets and grants, cross-site failover,
LDAP login, an audit log.
