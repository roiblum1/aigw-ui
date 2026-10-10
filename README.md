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
- **Quotas**: a budget per tenant per model, per minute, hour or day, rendered as `QuotaPolicy` on every cluster, with an optional dry-run mode. In tokens, or in dollars for a model with prices.
- **Prices**: per model, what a million input, cached input and output tokens cost. The gateway computes each request's cost, so a prompt answered from the prefix cache costs the tenant less.
- **Spent budgets**: per model, a tenant past its budget is refused, draws from a shared pool, or is served as best-effort behind everyone else.
- **Entry route and site weights**: per model, renders on every fleet cluster the route that sends each conversation to one of the sites that serve the model, weighted by how many instances each site has ready.
- **Usage**: what each tenant has used per model in the current window, in tokens or dollars, read from the quota counters in Redis.
- **Activity**: a task log of every change, showing per cluster which objects were created, updated or deleted and whether the gateway accepted them, and an audit log of who asked for what.
- **Self-test**: checks on a real gateway, with a temporary tenant, that a key works, usage is counted at the right amount, a quota refuses, a reset frees, a client cannot name another tenant, and the model reports cached tokens.
- **Architecture**: the design of the whole multi-site platform, with its diagrams, inside the UI.
- **API**: everything the UI does is available over REST for a self-service portal.

## Quick start on OpenShift

```sh
helm upgrade --install aigw-ui deploy/chart/aigw-ui -n aigw-ui --create-namespace

oc get route aigw-ui -n aigw-ui -o jsonpath='https://{.spec.host}{"\n"}'
oc get secret aigw-ui-auth -n aigw-ui -o jsonpath='{.data.admin-token}' | base64 -d; echo
```

Open the URL and sign in with the token. The image is
`ghcr.io/roiblum1/aigw-ui`.

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
hack/check-duplication.sh    # fails when the Go code repeats a block of about 15 lines
hack/check-chart.sh          # lints the chart and renders it the ways it is installed

# The store's tests need a Postgres they may create databases in. Each test
# creates one and drops it again. Without the variable they are skipped.
STORE_TEST_DATABASE_URL=postgres://postgres:dev@127.0.0.1:55432/postgres go test ./internal/store/
```

The GitHub workflow runs all of these on every pull request and push, the Go
tests with the race detector and a Postgres of its own, and pushes the
image to `ghcr.io/roiblum1/aigw-ui`. To release: set the version in
`deploy/chart/aigw-ui/Chart.yaml`, write `docs/release-notes/v<version>.md`,
list it in `docs/release-notes/README.md`, and push the tag `v<version>`. The
workflow refuses a version without a release note.

## Documentation

The same documents are in the website, on the **Docs** page.

| Document | Contents |
|---|---|
| [docs/how-it-works.md](docs/how-it-works.md) | What each action does, how, and why it was built that way |
| [docs/user-guide.md](docs/user-guide.md) | Using the UI: clusters, models, tenants, keys, quotas |
| [docs/pricing.md](docs/pricing.md) | Prices per model: quotas and usage in dollars, cached prompts charged less, dry-run |
| [docs/optimization.md](docs/optimization.md) | Shared pool and best-effort: two ways to serve a tenant past its quota, side by side |
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
internal/overage/      moves a tenant whose budget is spent to best-effort, from the counters
internal/costcel/      checks and evaluates a cost expression the way the gateway does
internal/selftest/     checks keys, counters and quotas with real requests through a gateway
internal/secretbox/    AES-256-GCM encryption of stored secrets
web/src/pages/         one file per page of the UI
web/src/               shared UI pieces: API client, components, charts
web/src/styles/        styles per feature
deploy/chart/          Helm chart for OpenShift
deploy/offline/        scripts that build and load the offline bundle
docs/                  documentation; the platform design page is embedded in the server
docs/release-notes/    what changed in each version
hack/                  checks CI runs, deploy-hub.sh, and seed-demo.py: demo data for the UI
Containerfile          image build
```

How the packages depend on each other: `api` calls `store`, `syncer`,
`usage` and `selftest`. `selftest` calls `store`, `syncer`, `usage` and
`gateway`. `syncer` calls `store`, `render`, `kube`, `gateway` and `weights`. `usage` calls
`store` and `render`. `render` depends on nothing, which keeps it easy to
test.

## Status

Built and tested: the UI and API, Postgres storage, model discovery, sync to
clusters, the image, the Helm chart on OpenShift, and the offline bundle.

Run on a gateway (OpenShift 4.22, Envoy Gateway 1.9.1, Envoy AI Gateway
1.1.0): keys, quotas, the self-test, best-effort with one site, and prices
with a stand-in for the model server. Each release note says what was run
and what was not. The Self-test button on a cluster checks the same points
on your own gateway. Open points are in
[docs/architecture.md](docs/architecture.md#not-verified-on-a-real-gateway).

Known limit: on an entry route with two or more sites, Envoy AI Gateway up
to 1.2.0 counts nothing, so quotas and prices have no effect there. A fix is
proposed upstream.

Not built yet: usage history beyond the current window, budgets longer than
a day, LDAP login. The audit log records a name the caller gives, not a
verified identity.

## Tests and delivery

Every change runs, in GitHub Actions: the Go tests with the race detector,
the store tests against PostgreSQL, the tests against a real Kubernetes API
server with the gateways' own definitions, the UI tests and build, and the
chart checks. A commit on `main` then builds the image, and can upgrade one
hub to it: see [docs/operations.md](docs/operations.md#upgrading-a-hub-from-ci).
