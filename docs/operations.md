# Operations

## Back up the encryption key

```sh
oc get secret aigw-ui-auth -n aigw-ui -o yaml > aigw-ui-auth.backup.yaml
```

Store it somewhere safe, away from the database backups. Without it the
kubeconfigs and API keys in the database cannot be read, and every cluster has
to be added again and every key reissued.

## Back up and restore the database

```sh
# backup
oc exec -n aigw-ui aigw-ui-postgresql-0 -- pg_dump -U postgres -Fc aigw > aigw.dump

# restore into an empty database
oc scale deploy/aigw-ui -n aigw-ui --replicas=0
oc exec -i -n aigw-ui aigw-ui-postgresql-0 -- pg_restore -U postgres -d aigw --clean --if-exists < aigw.dump
oc scale deploy/aigw-ui -n aigw-ui --replicas=1
```

A restored database only works with the encryption key it was written with.

## Upgrading a hub from CI

The workflow can upgrade one hub to every commit on `main`, right after
the image of that commit is pushed. It is off until you set it up.

**What it does.** The job `deploy` runs `hack/deploy-hub.sh`: a `helm
upgrade` of the release with the values it already has and the new image
tag. It waits until the new pod is ready. If it is not ready within five
minutes, Helm rolls the release back to what was running, and the job
fails.

**Tested once.** A pull request's run notes the tree it tested as the
status `ci/tested-tree` on the pull request's head. After the merge, the
job `seen` compares it with the merge commit's tree: when they are the same
(`main` did not move in between), the tests and AIPerf do not run again
and the image is built at once, about nine minutes sooner; the deploy
follows the image. A merge after
`main` moved, a squash or rebase merge, a tag, and a pull request from a
fork, which may not write the status, are tested again.

**Set it up once.**

1. On the hub's cluster, create an account that may change the release's
   namespace and nothing else, with a token that does not expire:

   ```sh
   oc -n aigw-ui create serviceaccount aigw-ui-deployer
   oc -n aigw-ui create rolebinding aigw-ui-deployer --clusterrole=admin \
     --serviceaccount=aigw-ui:aigw-ui-deployer
   oc -n aigw-ui apply -f - <<'YAML'
   apiVersion: v1
   kind: Secret
   metadata:
     name: aigw-ui-deployer-token
     annotations: {kubernetes.io/service-account.name: aigw-ui-deployer}
   type: kubernetes.io/service-account-token
   YAML
   ```

2. In the GitHub repository, create an environment named `hub` with three
   secrets: `HUB_SERVER` (the API address), `HUB_TOKEN` and `HUB_CA` (the
   `token` and `ca.crt` of that Secret, decoded).
3. Set the repository variable `HUB_DEPLOY` to `true`. `HUB_NAMESPACE` and
   `HUB_RELEASE` are optional; both default to `aigw-ui`.

**Things to know.**

- **The database is not backed up first.** A new version can change the
  database at start, and a rollback of the release does not undo that.
  Changes so far only add tables and columns, so the older version keeps
  working. Take a backup before a version whose release note says
  otherwise.
- **The account can read the Secrets of its namespace**, which hold the
  admin token and the encryption key. Anyone who can change the workflow
  on `main`, or the environment's secrets, can use it.
- **Turn it off** by setting `HUB_DEPLOY` to anything else. To remove it
  for good, delete the service account on the cluster too.
- **Try it by hand** with `DRY_RUN=1 hack/deploy-hub.sh` and the same
  variables: it asks the cluster whether it accepts the upgrade and
  changes nothing.

## The market snapshot

The default prices of models come from `internal/market/openrouter.json`, a
copy of OpenRouter's public catalog built into the server
([pricing](pricing.md#market-prices)).

- **Weekly.** The workflow `market` reads the catalog every Monday and
  opens or updates the pull request "Market snapshot YYYY-MM-DD" on the
  branch `market-snapshot`. A pull request opened by the workflow's own
  token starts no other workflow; push to it, or close and reopen it, to
  run CI. Merging it changes no price.
- **By hand.** `hack/fetch-market.sh` writes the file on any machine with
  internet access; `hack/fetch-market.sh FILE` reads a catalog saved
  earlier.
- **Offline.** `deploy/offline/build-bundle.sh` runs the script before it
  builds the image, and writes the snapshot's date to `bundle.env` as
  `MARKET_SNAPSHOT`.
- **Releases.** A version tag fails CI when the snapshot is more than 60
  days old (`hack/check-market.sh`).

## The upstream documentation

The image carries the documentation of Envoy AI Gateway, Envoy Gateway,
KServe, vLLM and llm-d, which the Docs page shows under **Upstream** where
there is no internet. `docs/upstream/projects.json` pins each project's
version; `cmd/upstream-docs` fetches it while the image is built.

- **Weekly.** The workflow `upstream-docs` moves every project to its newest
  release every Monday, fetches the docs to check they are still where the
  converter looks for them, and opens or updates the pull request
  "Upstream docs YYYY-MM-DD" on the branch `upstream-docs-bump`, with the
  new versions and what the image will carry. Like the market snapshot's,
  it starts no CI by itself.
- **What goes in.** Each project's Markdown pages under its roots, the
  images they show, and its `LICENSE` (all five are Apache 2.0). The pages
  are turned into plain Markdown: Hugo shortcodes, Docusaurus admonitions
  and components, and MkDocs admonitions and tabs; links between pages stay
  in the hub, other links go to the project's site or GitHub. About 57 MB,
  most of it vLLM's and llm-d's images.
- **By hand.** `go run ./cmd/upstream-docs fetch -out upstream-docs` on any
  machine with access to GitHub; a hub started from the checkout reads
  `upstream-docs` (`UPSTREAM_DOCS_DIR`).
- **A project that moved its docs** fails the fetch ("no pages under …"),
  in the weekly workflow before a pull request and in the image build: fix
  its `roots` in the file.

## If the hub is down

The LLM clusters keep serving with the configuration they last received. You
cannot make changes until the hub is back. Nothing on the clusters depends on
the server being up.

## If the shared Redis is down

The gateways keep answering, without limits: both rate limit services let a
request through when they cannot reach Redis (the AI gateway's quota service,
`quotaRateLimitFailureModeDeny: false`, and Envoy Gateway's, which the budget
across models uses, without `failClosed`). This is on purpose: a prompt does
not wait on anything outside the gateway. Budgets and quotas hold
again as soon as Redis is back; what was used meanwhile was not counted.

Watch for it rather than refuse:

- Envoy's rate limit filter counts each request it let through this way in
  `ratelimit.failure_mode_allowed` (per upstream cluster). Alert when it is
  above zero.
- The Usage page and the self-test say "Redis" in their error when the hub
  cannot read it either.
- Alert on the Redis itself (up, memory, connections) where it runs.

## Logs

```sh
oc logs -n aigw-ui deploy/aigw-ui
```

Failed syncs and failed discovery polls are logged with the cluster name. The
same message is shown in the cluster's row in the UI.

Every change request is logged as a line with `msg=audit`, with the same
fields as the audit log in the UI. `request with a wrong token` marks a failed
sign-in or API call. Since 0.32.0 an admin API request that takes more than
2 seconds in all is logged as `msg="slow request"` with its method, path,
status and `took_ms`.

**How long the hub took.** Every admin API answer carries
`Server-Timing: app;dur=<milliseconds>`: the hub's own time until it started
the answer (0.32.0). What a client measures beyond it is the network and the
transfer. On the sandbox hub the test machine took 0.53 s for `/healthz`
alone, and 1.4 s for `GET /tenants` and 2.2 to 2.9 s for `GET /usage` at
1,000 tenants, without gzip (300 and 883 KB); the hub took 5 and 12 ms for
the same lists on a laptop, and 2.0 s to create the 1,000 tenants that took
the test machine 133 s ([performance](performance.md#the-admin-api)). A
script that times the API should send `Accept-Encoding: gzip`, as the UI
does, and read the header.

## Troubleshooting

| What you see | Cause | What to do |
|---|---|---|
| `certificate signed by unknown authority` | The kubeconfig has no CA for the cluster's API | Use a kubeconfig with `certificate-authority-data` |
| `the server could not find the requested resource (is the CRD installed on this cluster?)` | Envoy Gateway or the AI gateway CRDs are missing | Press **Test** to see which, and install them |
| `connection refused` or a timeout | The hub cannot reach the cluster's API | Check routing and firewalls from the hub to port 6443 |
| `Unauthorized` or `forbidden` in the sync message | The kubeconfig's token expired or lacks rights | Edit the cluster and paste a new kubeconfig |
| No models appear | No `AIGatewayRoute` in the gateway namespace matches the model header exactly | Check `oc get aigatewayroutes -n <namespace>`; see [architecture.md](architecture.md) |
| Discovery says `GET /v1/models returned 401 Unauthorized` | The gateway requires a key for its model list | Edit the cluster and set the API key for `/v1/models` |
| Discovery says `read routes for the backends: ... forbidden` | The kubeconfig cannot list routes in all namespaces | Use a kubeconfig that can, or clear the Gateway URL |
| A model shows "no quota possible" | It is served only from something other than an `AIServiceBackend` | Expected; a `QuotaPolicy` cannot target it. Turn the model into a fleet model to put quotas on it at the entry |
| A cluster stays Pending | `config.autoSync` is off, or the last sync is still running | Press **Sync** |
| A cluster shows Error after an outage | The last sync failed | Nothing: it is retried every `config.syncInterval` (5 minutes) |
| Server logs `waiting for postgres` at start | The database is still starting | Normal on a fresh install; it waits up to two minutes |
| Server exits with `ENCRYPTION_KEY must be 32 bytes` | The Secret value is not 32 random bytes in base64 | Recreate it with `openssl rand -base64 32` |
| Every request with a valid key gets 401 at the gateway | The key Secret has not reached that cluster | Check the cluster's sync status |
| Self-test: "No answer within 90 seconds", last answer 401 | The gateway does not know the new key | Check that the `SecurityPolicy` `aigw-ui-api-key-auth` is accepted and attached to the gateway |
| Self-test: "No answer within 90 seconds", last answer 429 | The key works but the tenant's rule does not match, so the request falls to the pool | Check that the gateway forwards the client ID in `x-aigw-client-id` |
| Self-test: "No counter appeared under the expected name" | The rate limit service writes to another Redis, or names its keys differently | The message shows a real key from Redis when there is one. Check the Redis address on both sides, then `redis.keyPrefix` |
| Self-test: reset step fails, "still refused" | The rate limit service caches over-limit tenants (`LOCAL_CACHE_SIZE_IN_BYTES`) | Expected with that setting: a reset only helps once the window ends |
| A tenant named `selftest-…` stays on the Tenants page | A self-test was cut short, for example by a restart | Delete it, or run a self-test: it removes leftovers first |
| A new quota on a model is not counted or enforced | AI Gateway 1.1.0 does not pass a new quota rule to the proxy until the route changes. The hub forces that on the routes it renders, from 0.9.2 on with an annotation Envoy Gateway reads. On a route the cluster's chart owns it cannot | Create or revoke any key, which makes the gateway rebuild its routes, or wait for the next change. To check: the rule's name appears in the proxy's `config_dump` once it is in |
| Saving a model fails with "cost_expression is not one the gateway accepts" | The expression would be ignored by the gateway, which then charges nothing | Fix it as the message says. Numbers next to a count need a `u`, and the result must be a whole number |
| A priced model refuses nobody | It is in dry-run | Switch dry-run off under **Prices** on the Models page, after checking the tenants' limits |
| New prices are not in effect | They start at the next 00:00 UTC | The model's row says from when. The Activity page gets a line "model.prices" when they start |
| A cached prompt costs the same as a new one | The model server does not report cached tokens, or the cached price equals the input price | Run the self-test: "The model reports cached prompt tokens". See [deployment](deployment.md#before-cached-prompts-are-charged-less) |
| Self-test: counter step fails, "The request is counted and its cost is not" | The gateway does not hand the computed cost to the rate limit service | Check the controller's log for the model's cost expression, and that the rate limit service is the one the controller is configured for |
| Self-test: "A client cannot choose its tenant" fails | The gateway keeps an `x-aigw-client-id` header a client sends | Check that the `SecurityPolicy` `aigw-ui-api-key-auth` is attached to the listener clients use, and that no route in front of it sets the header |
| A tenant past its budget still gets 429 on a best-effort model | The move takes up to one `config.overageInterval` plus a sync. Or: the quota's window is a second or a minute, the quota is a dry run, Redis is not configured, or more than 200 tenants are listed | See the warnings in the model's row and the Activity page for lines that start with "overage" |
| Self-test: "did not show up on the best-effort route" | The route `fleetbe-<model>` is not accepted, or the model's entry route still takes the tenant's requests | `oc get aigatewayroute,envoypatchpolicy -n <namespace>`; both `fleetbe-<model>` objects must be accepted |
| The task message warns that `SecurityPolicy aigw-ui-api-key-auth does not hold on some routes` | A `SecurityPolicy` on a route of the client listener replaces the hub's there. The message names the routes. A route that forwards nothing, such as the gateway chart's `envoy-ai-gateway-health`, is left out, but only where the hub may get HTTPRoutes | Move the route to the peer listener, or remove its own policy. Requests on it are charged to no tenant's budget or quota. If the only route named is `envoy-ai-gateway-health`, grant the hub get on `httproutes` in the gateway namespace ([security](security.md#kubeconfigs)) |
| The task message warns that `Best-effort requests are not held back on InferencePool …` | The `LLMInferenceService` serving that pool runs its scheduler without flow control, so best-effort requests are refused only once the scheduler counts the pool full, which its default detector may never do | Turn on `featureGates: [flowControl]` with the `concurrency-detector` in the service's scheduler config ([optimization](optimization.md#what-a-site-does-with-the-class)); it needs a gateway with agent-router#2595 |
| The task message warns that `The request class standard of InferencePool … has priority …` | The site's own class `standard` for that pool ranks with or below `best-effort`, or below 0: regular tenants' requests name it | Give `standard` a priority of 0 or more, above `best-effort` (`config.bestEffortPriority`, -1 by default) |
| The task message warns that `Best-effort requests rank with regular tenants' on InferencePool …` | Two pools of best-effort models share a namespace; the class `best-effort` there is for the other one | Give each such pool a namespace of its own |
| The discovery message says an `LLMInferenceService` `has its route on the client listener` | KServe put the model's own route on the listener clients reach (`spec.router.gateway`) | Put its router on the peer listener: requests on KServe's route skip the entry route, the budgets and the quotas |
| Self-test: "answered but not counted for the tenant of its token" | Envoy AI Gateway before 1.2.0 charges a rule that counts per tenant to one shared bucket | Upgrade the AI gateway to 1.2.0. Quotas by amount and budgets across models need it |
| A capacity measurement of a data-parallel pod fails or measures one rank | The pod serves one API server per rank, and its probes reach a supervisor port | The hub sends the load to every port of the model's `InferencePool` (`targetPorts`) and adds up each rank's `/metrics`. It needs get on InferencePools in the model's namespace; without it, it uses the readiness probe's port |
| On a model added by hand, a tenant's quota counts 1 a request and its budget nothing, while the model's pool counts the cost | A hub before 0.29.0: the model server's backend removed the tenant's headers before the gateway charged the cost | Upgrade the hub; the next sync gives the backends the new header list. What was not counted before is not added afterwards |
| A tenant has a budget across models and own quotas too | They are from before 0.25.0, when a tenant could have both. On a model with prices the budget holds the tenant and the quota is not enforced; the Usage page shows it so, and the planner does not count it as given | Remove the quotas on the tenant's page |
| A tenant's page shows requests on a model but 0 tokens, and names the model | The model's route writes no token counts: the hub neither renders nor marks it (a model without quotas or budgets on a route of the cluster's own) | Declare the token keys on the gateway's `GatewayConfig`; see [deployment](deployment.md#token-counts-on-every-route) |
| A tenant's page says requests and tokens are not counted | The hub has no Redis, `redis.tokenCounts` is off, or the gateways still take API keys | Expected in those cases. With tokens only and Redis, the next sync writes the counting rules |
| Self-test: "The requests were counted, their tokens not" | As the row above, for the model the self-test picked | As the row above |
| Self-test: "No token counter appeared under the expected name" | The budget policy's token rules are not on the cluster, Envoy Gateway's global rate limiting counts in another Redis, or its rate limit service names keys differently | Check that `aigw-ui-tenant-budgets` is accepted and has five rules, and that Envoy Gateway's rate limit service uses the Redis the hub reads |
| The task message warns that `Regular tenants are served oldest first on InferencePool …` | The model's scheduler runs flow control with the default fairness policy on priority 0: one tenant's backlog holds up every other tenant's requests | Give the band `fairnessPolicyRef: round-robin-fairness-policy`; see [optimization](optimization.md#tenants-in-turn) |
| A best-effort tenant used more than the model's best-effort limit | The hub was down: the limit is the hub's, which takes the tenant off the best-effort route (on the lab a limit of 3 let 5 through in 46 s) | Expected while the hub is down; it caps the tenant on its first look after it is back. Quotas, budgets and lists hold without the hub |
| A model's pool let more than its limit through in one period | Its number of rules changed (a new amount, or the first budget across models) and its counter started over | Expected; see [architecture](architecture.md#live-usage) |
| A revoked token still answers for some seconds | A revoke reaches the gateways with the next sync, which waits for one already running. Up to 0.30.0 a sync of the lab took 10 to 13 s and wrote the token policy last; from 0.31.0 it writes it first, at up to 50 requests a second | Expected: the gateways learn of a revoke from the sync, and then Envoy Gateway takes a few seconds more to reach the proxy |
| A model deleted by hand comes back with no endpoint | Up to 0.29.0, discovery added a name the gateway still listed after the hub removed its route | Delete it again. From 0.30.0 a name only the gateway's list has adds no model |
| The hub's pod restarts with `OOMKilled` | 0.30.0 before its fix: with many tenants whose limits changed within a token's lifetime, the usage history built every key once per tenant that held the amount (870 MiB at 1,000 tenants) | Upgrade. The hub then keeps its heap to 90% of its memory limit (`MEMORY_LIMIT`, set by the chart, 1Gi by default) and used 45 to 60 MiB at 1,000 tenants |
| The `SecurityPolicy` `aigw-ui-api-key-auth` has hundreds of lines of `jti`s | A hub before 0.32.0 lists every denied token for as long as it lives, 180 days by default: a revoked token, and every token of a deleted tenant | Upgrade to 0.32.1 or later. The next sync puts runs of ten or more denied tokens into `revoked-span` rules by expiry ([security](security.md#tokens)); what stays a `jti` is a shorter run, a token revoked on its own, or one in the last 10 minutes |
| Nothing changes on a cluster although its syncs succeed and every object is `Accepted`: a revoked token is still answered, a new quota is not counted, the self-test fails its revoke and counter steps | Envoy refused the route configuration Envoy Gateway made, and keeps the one it had. A CEL rule Envoy cannot build does that: hub 0.32.0's `revoked-span` rule did (comprehension and joined strings, which Envoy turns off) | Upgrade the hub to 0.32.1 or later; the next sync replaces the rule and the proxy takes everything that waited. To see it: the proxy's `update_rejected` counters grow (`oc port-forward <envoy pod> 19000`, then `curl -s localhost:19000/stats \| grep update_rejected`; the proxy image has no shell tools), and its log says `failed to create an expression` |
| A budget tenant's history shows less of a model for a day than its counters in Redis | A hub before 0.32.0 moved the rule that counts budget tenants on a model whenever an amount was added after it; use in the minute before the move was not added | Upgrade; the rule no longer moves. What was lost is not added afterwards |
| Saving quotas for many tenants at once fails now and then with 409 `already exists` | A hub from 0.24.0 to 0.31.0: two quotas, or two new amounts, on one model saved at the same time picked the same free position | Upgrade, or save the failed one again. From 0.32.0 quotas on one model are saved in turn |
| A script's API calls are slow and the UI's are not | The script's time is the network's and the uncompressed transfer's | Compare with `Server-Timing` (above); send `Accept-Encoding: gzip` |
| A capacity measurement fails with `the model takes at most N tokens, prompt and answer together, and the settings ask for up to …` | A hub before 0.33.0: the model does not declare `aigw-ui.io/max-model-len` (vLLM chose its length) and the profile asks for more | Upgrade; the Job then scales the settings to the length the pod lists, and the run shows the scaled ones. Or annotate the Service with its length |
| A capacity measurement fails with `too few for a measurement` | The model takes under about 1,334 tokens, so even a scaled session has no prompt of 1,000 | Expected; such a model is not measured |
| `/my-usage` says the hub was not told the gateway's address; Claude Code is left without `ANTHROPIC_BASE_URL`; Oh My Pi adds no model | `config.gatewayURL` is empty | Set it to the address clients send their requests to, such as `https://llm.example.com`. An address that ends with `/anthropic` or `/v1` is taken without it (0.34.0); with 0.33.0 the setup used it as it was, so Claude Code needed `/anthropic` at its end |
| A personal key's token is refused with 401 `Client authentication failed` at the gateway, though the hub gave it and its signature is good | A hub up to 0.33.0 while the gateways take API keys too: Envoy's API key check runs before the JWT check and refuses what its Secret does not list, and those tokens were not listed | Upgrade to 0.34.0, which lists them; or revoke the last API keys, after giving those tenants tokens, so that the gateways take tokens only |
| Right after a first sign-in with a personal key, the gateway refuses it for some seconds; the installer says `waiting for the gateway to take your token` | The gateways take API keys too, and the key had no stored token yet: its first one reaches the gateways with the sync its renewal queued (`key.renew` in the task log) | Expected. A key made from 0.34.0 on, while API keys are taken, has its token from when it was made |
| Claude Code asks for `claude-…` models and the gateway refuses them, or its first request is refused as too long | Claude Code without models set: a 0.33.0 setup set none. It asks for answers of up to 32,000 tokens and takes a model it does not know for 200,000 long | Run the installer again (0.34.0 sets `ANTHROPIC_MODEL` and the alias models, and the length when the hub knows it). Choose the models with `config.claudeCode`; annotate deployments with `aigw-ui.io/max-model-len` for the length |
| Oh My Pi lists no `aigw/` model | The extension cannot reach the hub and kept no list, `aigw-token` is not signed in, or `~/.omp/agent/models.yml` defines a provider `aigw` itself | omp says which when a session starts. Remove the person's own `aigw` provider to get the hub's models |
| The installer stops with `could not download aigw-token` | The machine cannot reach the hub at the address the page showed, or curl does not trust the hub's certificate | Check the address from that machine; give curl the CA (`CURL_CA_BUNDLE`) |
| Requests are rejected with 429 for a tenant that has no quota | The model has quotas for other tenants, so this one only has the shared pool | Give the tenant a quota |

## Upgrading Envoy Gateway to 1.10

Envoy Gateway 1.10 is to turn on version 2 of its xDS names, which renames
the route configuration of a plain-HTTP listener to `http-<port>`. The entry
routes' retry patches are written for both names since 0.29.0, with the port
read from the cluster's `Gateway` before each sync. After the upgrade:

1. Sync the cluster and check that no `fleet-…-retry` patch is reported.
2. Run the self-test on the cluster. Its first step fails when a patch is
   not in effect, and its stickiness step gets no answer when requests
   through the entry route lose their Host (404 from the peer listener).

## Site maintenance

Before taking nodes down or rolling out a model on one site:

1. On the Models page, press **Drain** next to that cluster for each model
   it serves. The weight goes down one instance per discovery poll (60
   seconds by default).
2. Wait until the tag reads "drained · not listed", then do the work.
3. Press **Undrain**. The site is listed again at weight 1 and comes back one
   instance per poll.

Without a drain nothing breaks: lost instances lower the weight after two
polls, and the health checks take a dead site out sooner.

| What you see | Cause | What to do |
|---|---|---|
| A site shows "not listed" | The cluster is not **Part of the fleet**, or has no `LLMInferenceService` whose `spec.model.name` is the model name here | Tick it, or make the names equal on every site |
| "Site weights not updated: no site is left that serves the model" | Every site stopped serving the model or left the fleet | The gateways keep the last sites. Bring a site back, or switch the entry route off |
| A sync message says "The entry route of … was left as it is" | The server has no `FLEET_DOMAIN`, or none of the model's sites is a fleet cluster with a peer host | Set the chart's `fleet.domain`, or bring a site back into the fleet. Until then the model's objects stay on the clusters unchanged, and everything else is synced |
| The Models page says a cluster's own route "is still attached to the client listener or the whole Gateway" | The model has an entry route, and the cluster's chart still exposes the model to clients itself. The older route wins | Attach the chart's route to the peer listener alone. Until then the cluster's backends keep the quota |
| A site's weight is higher than what the gateway can route to | A canary or test `LLMInferenceService` with the same model name is counted | Set `aigw-ui.io/ignore: "true"` on it |
| A sync reports `EnvoyPatchPolicy fleet-<model>-retry` as not accepted or not programmed | `enableEnvoyPatchPolicy` is off in the Envoy Gateway configuration, or the client listener's name is wrong | Turn it on (`extensionApis.enableEnvoyPatchPolicy: true`) and check the cluster's **Client listener**. Until then forwarded requests can get 404 from a peer listener, and about one request in seven to a site that answers 503 gets the 503. The retry part exists because of [envoyproxy/gateway#5690](https://github.com/envoyproxy/gateway/issues/5690) |
| Deleting a cluster fails with "is part of the fleet" | Its gateway would keep entry routes and keys that nobody updates | Untick **Part of the fleet**, wait for its sync, then delete |
| Ticking **Part of the fleet** fails with "no EnvoyPatchPolicy kind" or "could not be asked" | The cluster has no Envoy Gateway CRDs for patches, or does not answer | Install them and set `extensionApis.enableEnvoyPatchPolicy: true`; check the kubeconfig |
| Discovery fails with "read what the cluster serves" | The kubeconfig cannot list `llminferenceservices` | Use a kubeconfig that can. The poll changed nothing |
| "Entry route" cannot be switched on: "no FLEET_DOMAIN" | The chart's `fleet.domain` is empty | Set it and upgrade |
| A cluster shows "fleet · outdated" for more than a few minutes | Its last sync failed, or auto sync is off | Read the cluster's sync message and press **Sync**. Until then it can choose another site for a conversation than the rest |
| A site in the fleet gets no traffic for a model | Its health check `GET /healthz/<model>` on the peer listener fails | Check the model's serving route on that site and the peer certificates |
| Cross-site requests get 401 at the serving site | The API-key policy covers the listener other sites forward to | Set the cluster's **Client listener** |
| Saving a cluster fails with "a fleet cluster …" | **Part of the fleet** needs **Enforce API keys**, a client listener, a peer host, and the gateway namespace the other fleet clusters use. A fleet cluster also cannot be renamed | Set them, or leave the fleet |

## Checking what is on a cluster

```sh
oc get backends,aiservicebackends,aigatewayroutes,securitypolicies,secrets \
  -n <gateway namespace> -l app.kubernetes.io/managed-by=aigw-ui
oc get quotapolicies -A -l app.kubernetes.io/managed-by=aigw-ui
```

## Removing everything from a cluster by hand

```sh
oc delete backends,aiservicebackends,aigatewayroutes,securitypolicies,secrets \
  -n <gateway namespace> -l app.kubernetes.io/managed-by=aigw-ui
oc delete quotapolicies -A -l app.kubernetes.io/managed-by=aigw-ui
```
