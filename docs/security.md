# Security

## Where secrets are kept

| Secret | Stored in | Protection |
|---|---|---|
| Cluster kubeconfigs | Postgres, `clusters.kubeconfig_enc` | AES-256-GCM |
| Tenant API keys | Postgres, `api_keys.key_enc` | AES-256-GCM |
| Gateway key for `/v1/models` | Postgres, `clusters.discovery_token_enc` | AES-256-GCM |
| Encryption key | Kubernetes Secret `<release>-auth` on the hub | Kubernetes RBAC, etcd encryption if enabled |
| Admin token | Kubernetes Secret `<release>-auth` on the hub | same |
| Postgres password | Kubernetes Secret `<release>-postgresql` | same |
| Redis URL with its password | Kubernetes Secret `<release>-redis`, or the one named in `redis.existingSecret` | same. The server only reads from Redis, unless `redis.allowReset` is on: then it can also delete quota counters |
| Tenant API keys on the LLM clusters | Kubernetes Secret `aigw-ui-api-keys` in the gateway namespace | Kubernetes RBAC on that cluster |

Kubeconfigs are not stored as Kubernetes Secrets. They are encrypted rows in
Postgres, and the key that decrypts them is the Kubernetes Secret.

## Kubeconfigs

- A kubeconfig is write-only. No API call returns it, and the edit form does not show the stored one.
- It is encrypted before it is written, so a database dump or backup does not reveal it.
- It is decrypted in the server's memory each time a cluster is synced or polled.
- The Route forces HTTPS, so it is not sent in clear text when pasted.

**Who can get at them.** Anyone who can read both the `<release>-auth` Secret
and the database can decrypt every kubeconfig. In practice that is anyone with
admin rights on the hub namespace. With cluster-admin kubeconfigs, that person
controls every LLM cluster. Keep access to the hub namespace small.

**Narrower credentials.** The tool only needs, in the gateway namespace of each
LLM cluster: create, get, list, patch and delete on the Envoy Gateway and AI
gateway resources it manages; create on Secrets plus get, patch and delete on
the one Secret named `aigw-ui-api-keys`; and get on the `Gateway`. It never
lists Secrets. A ServiceAccount kubeconfig limited to that would reduce the
damage if the hub were compromised.

## Tenant API keys

Keys are stored encrypted rather than hashed, because every sync has to write
them to the clusters. The UI and API show a key only once, when it is created.
Afterwards only the first characters are shown.

**Revoking.** A revoked key, and every key of a disabled or deleted tenant,
is removed from the Secret by the next sync of each cluster. Until a cluster
has synced, the key still works there: check the cluster's status on the
Clusters page, or the task in Activity. Versions before 0.6.1 did not remove
the key at all; see the [0.6.1 release notes](release-notes/v0.6.1.md).

**Without "Enforce API keys" there are no tenants.** The gateway then takes
the client ID from whoever sends an `x-aigw-client-id` header, so a caller can
use, or use up, any tenant's quota. With enforcement on, the gateway sets the
header itself from the key and overwrites what the client sent. Turn it on for
every cluster where quotas are meant to hold.

## The tenants' own page

At `/my-usage` a tenant signs in with one of its API keys and sees its own
budgets, usage and prices. `config.tenantPage: false` turns it off.

- **What a key opens.** `GET /api/v1/my/usage` and nothing else. The admin
  API answers 401 to a tenant's key, and the page's endpoint answers 401 to
  the admin token.
- **What it shows.** The tenant the key belongs to, taken from the key and
  from nothing the caller sends. There is no parameter to ask for another
  tenant. Cluster names, backends and other tenants are not in the answer.
- **The key comes to the hub.** The hub already holds every key, so it
  learns nothing new. Still, it is the one place where a tenant types its
  key into something other than its client. The page is served over the
  hub's Route, with TLS. No request to a model passes through the hub.
- **In the browser** the key is kept in session storage: it is gone when
  the tab is closed, and other tabs do not have it.
- **Wrong keys.** Ten from one address in a minute and that address gets
  429 until the minute is over. Each is a line in the server log, without
  the key. Keys are 192 random bits, so guessing one is not practical.
- **A revoked key** stops working there at once, before the clusters are
  synced, and so does every key of a tenant that is turned off.

## Signing in

The UI and the API share one admin token. Everyone who has it has full
access. LDAP login is not built yet.

**Audit log.** Every request that changes something, or tries to, is recorded
with the time, the result and the address it came from, and shown under
Activity. Because there is one token, the log cannot prove who a person is:
the name in it is whatever the caller sent. Treat it as a record of what
happened and when, and as a hint about who. A request with a wrong token is
not in the audit log; it is written to the server log.

Entries never contain a request body, so no kubeconfig, key or token ends up
in the log. The table keeps the last 20,000 entries. For a longer or
tamper-proof record, collect the server log: every entry is also written
there as a line that starts with `audit`.

- The browser keeps the token in local storage until sign-out.
- Rotate it by changing `admin-token` in the Secret and restarting the pod:

```sh
oc set data secret/aigw-ui-auth -n aigw-ui admin-token="$(openssl rand -hex 24)"
oc rollout restart deploy/aigw-ui -n aigw-ui
```

## The encryption key cannot be rotated yet

Changing `encryption-key` makes every stored kubeconfig and API key
unreadable. There is no re-encryption command. Treat the key as permanent and
back it up.

## Pod hardening

- Runs under the `restricted-v2` SCC with a random non-root UID.
- No capabilities, no privilege escalation, `RuntimeDefault` seccomp.
- Read-only root filesystem for the server.
- No Kubernetes service account token is mounted. The server has no rights on the hub cluster itself.
- A NetworkPolicy allows only the server pod to reach Postgres.

## What a client cannot do

- **Name another tenant.** The gateway checks the API key and then writes
  the tenant into the header `x-aigw-client-id`, replacing whatever the
  client sent. Quotas and usage follow that header. The self-test step "A
  client cannot choose its tenant" checks it on a real gateway. A cluster
  in the fleet must enforce API keys for this reason.
- **Set the cached token count of its own answer.** A priced model charges
  the cached part of a prompt less, by the count the model server reports.
  vLLM 0.31 lets a request set that count through `kv_transfer_params`.
  The hub's backends remove the field, and the price expression charges a
  cached count above the prompt as not cached. Backends the cluster's own
  chart brings need the same line: see
  [deployment](deployment.md#before-cached-prompts-are-charged-less).

What a client can do: leave a streamed answer before its end. The model
server reports the token counts at the end, so that request is not charged.

## The account CI upgrades the hub with

When [upgrades from CI](operations.md#upgrading-a-hub-from-ci) are set up,
GitHub holds the token of a service account that is admin of the hub's
namespace. It can read that namespace's Secrets, among them the admin token
and the encryption key, and through the release it decides which image
runs. It has no rights elsewhere on the cluster and none on the LLM
clusters. Whoever can change the workflow on `main` or the environment's
secrets has what the account has.

## What the tool can change on an LLM cluster

It applies and deletes only objects labelled
`app.kubernetes.io/managed-by: aigw-ui`, in the gateway namespace you set. The
label is checked on the hub before any delete.

Turning on "Enforce API keys" attaches API key auth to the whole gateway. That
is the one setting that affects traffic this tool does not otherwise manage.
