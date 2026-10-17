# Security

## Where secrets are kept

| Secret | Stored in | Protection |
|---|---|---|
| Cluster kubeconfigs | Postgres, `clusters.kubeconfig_enc` | AES-256-GCM |
| Tenant API keys and tokens | Postgres, `api_keys.key_enc` | AES-256-GCM |
| Token signing keys (EC P-256) | Postgres, `signing_keys.private_enc` | AES-256-GCM; only the public halves leave the hub |
| Gateway key for `/v1/models` | Postgres, `clusters.discovery_token_enc` | AES-256-GCM |
| Encryption key | Kubernetes Secret `<release>-auth` on the hub | Kubernetes RBAC, etcd encryption if enabled |
| Admin token | Kubernetes Secret `<release>-auth` on the hub | same |
| Postgres password | Kubernetes Secret `<release>-postgresql` | same |
| Redis URL with its password | Kubernetes Secret `<release>-redis`, or the one named in `redis.existingSecret` | same. The server only reads from Redis, unless `redis.allowReset` is on: then it can also delete quota counters |
| Tenant API keys on the LLM clusters | Kubernetes Secret `aigw-ui-api-keys` in the gateway namespace, while any tenant has an API key; tokens are listed there too until then | Kubernetes RBAC on that cluster |

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
the one Secret named `aigw-ui-api-keys`; and get on the `Gateway`. In the
namespaces of the models it serves: the same on `QuotaPolicy` and
`InferenceObjective`, and list and patch on `AIGatewayRoute`. It never
lists Secrets. Across the cluster it reads LLMInferenceServices, the
LLMInferenceServiceConfigs they are built on, the pods of their deployments
(`kserve.io/component=workload`) and the nodes' labels, to count ready
instances, find each deployment's GPU and tell what it runs, and gets the
`Deployment` or `LeaderWorkerSet` each service's status names, for its ready
count. It gets the HTTPRoutes Envoy Gateway names when a policy of their
own replaces the hub's `SecurityPolicy` on them, to leave a route that
forwards nothing out of the sync's warning. Without list on pods and nodes
the instances are still counted, the GPUs are not known, and the cluster's
discovery message says what was not allowed; without get on the workloads
the count KServe copies into the service's status is used; without get on
HTTPRoutes the gateway chart's health check, where it is on, is warned
about on every sync;
without list on pods or on the configs, a change of image or of a shared
config does not mark a measurement stale
([capacity](capacity.md#what-is-measured-each-llminferenceservice)). A ServiceAccount
kubeconfig limited to that would reduce the damage if the hub were
compromised.

**Capacity measurements** need more, in the namespace of each model that is
measured: create, get and delete on Jobs, list on Pods, get on `pods/log`,
and get on the model's `InferencePool` for the ports of a pod that serves
one API server per data-parallel rank.
That is a step up from writing gateway objects: the hub then runs a workload
next to the model, and whoever may create Jobs in a namespace can mount any
Secret of that namespace in one. The Job is described [below](#what-the-tool-can-change-on-an-llm-cluster).
Leave `capacity.image` empty to turn measurements off.

**Prefill/decode measurements are off by default**
(`capacity.peerMeasurements: false`). Such an LLMI is measured through the
gateway's peer listener, so its Job runs in the **gateway's namespace** with
the site's peer client certificate (`fleet.peerClientSecret`) mounted.
Turning them on grants two things you should weigh first:

- **The certificate is unmetered access.** The peer listener asks for no API
  key and counts nothing against a quota, so whoever holds the certificate
  can call every model on that listener for free. The Job's pod holds it
  for the length of the measurement, and anyone who can exec into pods or
  read Secrets in the gateway namespace can take it.
- **Jobs in the gateway namespace.** The hub needs create on Jobs there, which
  lets it mount any Secret of that namespace, among them the tenants' API
  keys (`aigw-ui-api-keys`) and the gateway's own certificates. The hub can
  already write that Secret, but it never reads the others.

Do not grant it while you have no prefill/decode model. With it off, the
Capacity page says so for each prefill/decode LLMI, the API refuses to
measure one (409), and measuring by itself skips them; their site weights
follow `aigw-ui.io/capacity-per-instance`.

## Tenant API keys

Keys are stored encrypted rather than hashed, because every sync has to write
them to the clusters. The UI and API show a key only once, when it is created.
Afterwards only the first characters are shown.

**Revoking.** A revoked API key, and every API key of a disabled or deleted
tenant, is removed from the Secret by the next sync of each cluster. For
tokens, see [below](#tokens). Until a cluster
has synced, the key still works there: check the cluster's status on the
Clusters page, or the task in Activity. Versions before 0.6.1 did not remove
the key at all; see the [0.6.1 release notes](release-notes/v0.6.1.md).

**Where a key is read from.** The `Authorization` header or `x-api-key`.
When a request carries both, `Authorization` is the one that counts: a
wrong key there is refused although `x-api-key` holds a right one. Both
headers are removed before the request reaches a model server.

**Without "Enforce API keys" there are no tenants.** The gateway then takes
the client ID from whoever sends an `x-aigw-client-id` header, so a caller can
use, or use up, any tenant's quota. With enforcement on, the gateway sets the
header itself from the key and overwrites what the client sent. Turn it on for
every cluster where quotas are meant to hold.

## Tokens

Since 0.18, a new key is a **token**: a JSON Web Token the hub signs with
ES256, which the client pastes where it takes an API key. It says who the
tenant is (`sub`, its slug) and which key it is (`jti`), for whom
(`iss` `tokens.issuer`, `aud` `tokens.audience`), and until when (`exp`,
`tokens.lifetimeDays` after it was made, 180 by default). Since 0.23 it
also carries the tenant's budget across models (`requests_per_unit`,
`unit`) and its list of models (`models`) as they were when it was signed,
which the gateway holds the tenant to. A holder can read them but not change
them: any change breaks the signature. Quotas per model are not in it.

**The gateways check tokens themselves.** Every sync writes the hub's public
keys into the SecurityPolicy `aigw-ui-api-key-auth` inline, so no cluster
calls the hub at runtime, and tokens keep working while the hub is down. The
gateway then sets `x-aigw-tenant` from `sub`, `x-aigw-key` from `jti` and
`x-aigw-budget` from `requests_per_unit`, replacing what a client sent (lab check C, Envoy Gateway 1.9.2: a client's
own `x-aigw-tenant` never reaches the model server, even with a key that is
not a token).

**Personal keys.** A personal key (`aigw-pk-` and 43 characters, 256 random
bits) is traded at the hub for a token (`POST /api/v1/token`). The hub keeps
only its SHA-256 hash; the gateways never see it. The tokens it gets live
`tokens.renewedLifetimeHours` (168 by default) and are not stored: each has
its own `jti` and the key's client ID as `cid`. Revoking the key, or
deleting its tenant, refuses those tokens by their `cid` at every gateway
until the last one expires, and a disabled tenant's key gets none. Wrong
keys count against the address as on the tenants' page: ten a minute, then
429. The answer carries `Cache-Control: no-store`, as does the answer that
shows a new key or token once; the hub gzips its other answers but not
these, so that the length of one cannot tell anything about the secret in it
(BREACH). The helper `aigw-token`
keeps the key and the last token in `~/.config/aigw`, readable by the user
only, and passes the key to `curl` on stdin, so it is not on any command
line.

**The signing key** is an EC P-256 key the hub makes when it signs its first
token. Its private half is stored in Postgres, encrypted with the encryption
key, like the kubeconfigs: **whoever can read both the `<release>-auth`
Secret and the database can sign a token for any tenant.** Nothing else
reads it; the API never returns it. Its public half is published to every
gateway.

**Rotating it.** **Rotate signing key** on the Tenants page (or `POST
/api/v1/signing-keys/rotate`) makes a new key, which signs every token from
then on. The old key stays published, and the tokens it signed keep working,
until the last of them expires or is revoked; the Tenants page lists such
keys with their token count. To retire it sooner, give each tenant a new
token and revoke the old ones. If a signing key leaked, rotate, then revoke
every token the old key signed: the old key is unpublished at the next sync
after its last token is revoked.

**Revoking a token** adds a rule to the SecurityPolicy that refuses its
`jti` until five minutes after it expires, after which the gateway refuses it
anyway and the rule is dropped. The five minutes are there because a gateway
takes a token up to 60 seconds past its expiry: Envoy allows that much for
the clocks of the hub and the gateway to differ, and Envoy Gateway has no
setting for it. Deleting a tenant does the same for each of its tokens, and
the rules outlive the tenant. A disabled tenant's tokens are refused by its
slug until it is enabled. Until a cluster has synced, a revoked token still
works there, as an API key does. A sync writes the token policy and the key
Secret before anything else (0.31.0).

**Denied tokens in spans (0.32.0, fixed in 0.32.1).** A `jti` stays in the
policy for as long as its token lives, 180 days by default, and a test that
deletes its tenants leaves one per token: the sandbox's policy grew to
hundreds of lines. Where ten or more denied tokens follow one another by
expiry (`exp`) and no live token expires among them, the policy refuses that
span of expiries instead of each `jti`: rules named `revoked-span` compare a
stored token's `exp` with each span's first and last second, in CEL. 1,000
denials from 15 deletions of test tenants make 33.6 KB and 1,113 lines of
policy as `jti`s, and 5.3 KB and 99 lines as spans. A span's entry is about
as long as eight `jti`s, so shorter runs stay `jti`s. A span refuses no
token that should pass:

- Every stored token that has not expired is either live or denied: a
  token's row goes only with its tenant, and the database denies it then.
  The hub reads both in one snapshot and ends a span at every live token's
  expiry. A denied token that expires in the same second as a live one
  stays a `jti`.
- A token signed after the spans were worked out expires after every span:
  the tokens signed in the last 10 minutes are left out of spans and stay
  `jti`s. If `tokens.lifetimeDays` is lowered, a token signed between the
  restart and the hub's first sync could expire inside an old span and be
  refused until that sync, which comes at once since 0.32.0.
- A stored token's `cid` is `<sub>.<jti>`; a token a personal key got
  carries the key's ID as `cid`, which does not end with its own `jti`, so
  the rule never matches it; those are refused by `cid`.
- The gateway counts an error in a CEL expression as no match, which for a
  `Deny` rule would let the token through, so the rule checks for each
  claim before it reads it.

**Envoy builds CEL with less than CEL has.** Envoy turns comprehensions
(`exists`, `all`, `map`, `filter`) off, and joining strings with `+` unless
told otherwise (Envoy 1.39, `filters/common/expr/evaluator.cc`; Envoy
Gateway does not tell it), and refuses the whole route configuration when an
expression needs them. Envoy Gateway still reports the policy `Accepted`,
and the proxy keeps the configuration it had: no revoke, no new quota and no
other change reaches it. 0.32.0's span rule used both; on the lab every
change after the upgrade's first sync was lost until 0.32.1, and the
self-test failed its revoke and counter steps (2026-10-17). Since 0.32.1 the
rule is a chain of comparisons with `endsWith`, which Envoy 1.39.1 took and
enforced in a local test: tokens inside a span and at both its ends
refused, a second outside it, between spans and a personal key's token in
it answered, across three rules of up to 3,998 characters. The tests check
every CEL rule the hub renders for what Envoy builds.

A rule holds about 19 spans within the 4,096 characters of CEL Envoy Gateway
takes; more spans make more rules. `GET /signing-keys` gives `denied` (every
denied token) and `denied_spans`.

**What the policy's length depends on.** Not on the number of tenants or
keys: a token that works is never listed (with tokens only the gateway
checks its signature), so 1,000 tenants with a token each make the same
policy as one, 1.1 to 1.6 KB. It grows only with what is refused: a line per
disabled tenant, per revoked personal key for up to
`tokens.renewedLifetimeHours` (a week), and per denied token for up to
`tokens.lifetimeDays` (180 days). Spans shrink the last only where denied
tokens were signed together, as a test's or an onboarding batch's are;
tokens revoked one by one among live ones, such as each tenant replacing
its own at a different time, stay a `jti` each. To keep that short, give
people personal keys, whose tokens are refused by the key for a week at
most, or lower `tokens.lifetimeDays`.

**What reaches the model server.** A token is not removed by the JWT check.
The backends the hub renders (the entry routes, the best-effort routes, and
the models added by address) remove `Authorization` and `x-api-key` before
the model server; a route of the cluster's own passes them on, as an API key
was passed before 0.18 only when it was not removed there either. vLLM does
not log request headers.

**The switch from API keys.** While any enabled tenant still has an API key,
the gateways take both: the API key check lists every API key and every
token, does not remove the credential, and lets the JWT check run after it,
which passes a request that holds no token (`failOpen`). Rules then refuse a
revoked or disabled tenant's token, let a token that checked out through,
and refuse any other credential shaped like a token (`eyJ…`), such as an
expired token still listed. An API key starts with `sk-` and is never taken
for one. Once the last API key is revoked, the next sync drops the API key
check and the Secret: the gateways list nothing, and a token writes the
client ID too. The Tenants page says which tenants still have API keys.

**Headers a client cannot set.** The gateway's JWT filter writes
`x-aigw-tenant`, `x-aigw-key`, `x-aigw-client-id`, `x-aigw-budget` and each
`x-aigw-quota-<model>` from the token, and on every request it checks it
first removes a client's own value of each (Envoy's documented
`claim_to_headers` behaviour): a header holds the token's value or nothing.
Up to Envoy 1.39, which Envoy Gateway 1.9.2 ships (1.39.1), only the
provider that checks the token removes them; Envoy 1.40.0 removes them on
every request, also on those that skip the check (runtime flag
`jwt_authn_sanitize_payload_headers_filter_wide`). With tokens only, the
gateway checks a token on every request of the client listener, so that
difference does not open anything there. On the lab a request
with a forged `x-aigw-tenant` was still refused by its own spent budget
(2026-10-16). The budget and quota rules read these headers, so this is what
keeps a tenant to its own counters. They reach the model server too: the
gateway charges an answer's cost by reading them again when the answer
ends, after the backend, so a backend that removed them would leave the cost
uncharged. The backends the hub renders remove the credentials only.

**Defense in depth.** On a listener without the hub's policy nothing removes
them. The gateway chart's ClientTrafficPolicy already removes a client's
`x-aigw-client-id` before any check. One entry removes all of them, the
`x-aigw-quota-<model>` headers of models added later too: a prefix match in
`earlyRequestHeaders.removeOnMatch` (Envoy Gateway 1.8.5 and 1.9.2):

```yaml
spec:
  headers:
    earlyRequestHeaders:
      removeOnMatch:
        - type: Prefix
          value: x-aigw-
```

The backends the hub renders for a model server remove the tenant, key,
client ID and budget headers before the request leaves the gateway. They do
not remove the `x-aigw-quota-<model>` amounts: an `AIServiceBackend` takes 16
header names at most and no prefix (AI gateway 1.2.0), and one name per
model with quotas by amount made every backend invalid from the eleventh
such model on. An amount says nothing about who sent the request. An entry
route's backend keeps them all, since the site it forwards to may count the
request by its tenant.

## The tenants' own page

At `/my-usage` a tenant signs in with one of its API keys or tokens and sees
its own budgets, usage and prices. `config.tenantPage: false` turns it off.
A token is checked on the hub against its own public keys: signature,
issuer, audience and expiry, then that it is one of the tenant's keys and
not revoked.

- **What a key opens.** `GET /api/v1/my/usage` and nothing else. The admin
  API answers 401 to a tenant's key, and the page's endpoint answers 401 to
  the admin token.
- **What it shows.** The tenant the key belongs to, taken from the key and
  from nothing the caller sends. There is no parameter to ask for another
  tenant. Cluster names, backends and other tenants are not in the answer.
- **Token counts are numbers.** The gateways count each tenant's requests
  and tokens per model and hour in the shared Redis, under keys that hold
  the tenant's slug and the model's name, as the budget counters do. The
  hub copies the numbers into `token_hours`. No prompt or answer reaches the
  hub or Redis.
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

- **Name another tenant.** The gateway checks the API key or token and then
  writes the tenant into the header `x-aigw-client-id`, and from a token into
  `x-aigw-tenant`, replacing whatever the client sent. Quotas and usage follow that header. The self-test step "A
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
`app.kubernetes.io/managed-by: aigw-ui`. The label is checked on the hub
before any delete. Most are in the gateway namespace you set; some are next
to what they belong to:

- `QuotaPolicy` objects in the namespace of the model's `AIServiceBackend`,
  often the model's namespace;
- the `InferenceObjective` `best-effort` next to each `InferencePool` that
  serves a model with best-effort on. Each sync also gets the site's own
  class `standard` there, to compare;
- on `AIGatewayRoute`s a chart owns, it patches the annotation
  `gateway.envoyproxy.io/aigw-ui-quota-revision` and, for a priced model,
  adds its credits entry to `spec.llmRequestCosts`. It never deletes them.

Through the gateway it sends each tenant's slug to the schedulers of the
sites that serve the tenant's requests, in `x-llm-d-inference-fairness-id`,
written from the token. A scheduler labels its request metrics with it (at
most 1,000 values), so the slugs appear in each site's Prometheus.

It reads, across the cluster, `LLMInferenceService`s and their configs, the
workload pods, the nodes' labels, the `Deployment` or `LeaderWorkerSet`
each service's status names, and its `InferencePool`, in the gateway
namespace the `Gateway`, and the HTTPRoutes whose own `SecurityPolicy`
replaces the hub's. Each of these that it may not read only takes away what
it is for (GPU shapes, ready counts from the workload itself, the rank ports
of a measurement, a warning without the routes that reach no model).

Turning on "Enforce API keys" attaches API key auth to the whole gateway. That
is the one setting that affects traffic this tool does not otherwise manage.

A capacity measurement creates one Job, `aigw-capacity-<id>`, in the
namespace of the model it measures, when an operator starts it. Its pod runs
the image set in `capacity.image`, gets no service account token, runs as a
non-root user without privileges, and sends requests only to the pod named
in the form, over the cluster network. A prefill/decode measurement, only
with `capacity.peerMeasurements` on, runs in the gateway's namespace instead,
mounts the peer client certificate read-only and sends to the site's own
peer listener ([above](#kubeconfigs)). It loads that pod until its latency
targets break, which slows the answers of that pod's own users. The hub
deletes the Job when the measurement is stopped; Kubernetes deletes it 7 days
after it ends.
