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

## If the hub is down

The LLM clusters keep serving with the configuration they last received. You
cannot make changes until the hub is back. Nothing on the clusters depends on
the server being up.

## Logs

```sh
oc logs -n aigw-ui deploy/aigw-ui
```

Failed syncs and failed discovery polls are logged with the cluster name. The
same message is shown in the cluster's row in the UI.

Every change request is logged as a line with `msg=audit`, with the same
fields as the audit log in the UI. `request with a wrong token` marks a failed
sign-in or API call.

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
| A model shows "no quota possible" | It is served only from something other than an `AIServiceBackend` | Expected; a `QuotaPolicy` cannot target it |
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
| Requests are rejected with 429 for a tenant that has no quota | The model has quotas for other tenants, so this one only has the shared pool | Give the tenant a quota |

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
