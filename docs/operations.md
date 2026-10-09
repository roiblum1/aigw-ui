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
2. Wait until the tag reads "drained · weight 0", then do the work.
3. Press **Undrain**. The weight comes back one instance per poll.

Without a drain nothing breaks: lost instances lower the weight after two
polls, and the health checks take a dead site out sooner.

| What you see | Cause | What to do |
|---|---|---|
| "Site weights not written: no cluster that serves the model is in the fleet" | No cluster has **Part of the fleet** ticked | Edit each site's cluster and tick it |
| "Site weights not written: the capacity of X is not known yet" | X has a deployment of the model that never reported a ready count | `oc get llminferenceservice -A -o jsonpath='{.items[*].status.workloads}'` on X |
| A serving site shows weight 0 and "no deployment of the model on this cluster" | The `LLMInferenceService`'s `spec.model.name` differs from the model name here | Make the names equal on every site |
| The weight on the Models page is right, the policy on the cluster is not | The policy lacks the label or annotation, or Argo CD reverts the field | See [architecture.md](architecture.md#site-weights) |
| Cross-site requests get 401 at the serving site | The API-key policy covers the listener other sites forward to | Set the cluster's **Client listener** |
| Saving a cluster fails with "a fleet cluster must enforce API keys" | **Part of the fleet** needs **Enforce API keys** | Tick both, or neither |

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
