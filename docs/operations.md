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

## Troubleshooting

| What you see | Cause | What to do |
|---|---|---|
| `certificate signed by unknown authority` | The kubeconfig has no CA for the cluster's API | Use a kubeconfig with `certificate-authority-data` |
| `the server could not find the requested resource (is the CRD installed on this cluster?)` | Envoy Gateway or the AI gateway CRDs are missing | Press **Test** to see which, and install them |
| `connection refused` or a timeout | The hub cannot reach the cluster's API | Check routing and firewalls from the hub to port 6443 |
| `Unauthorized` or `forbidden` in the sync message | The kubeconfig's token expired or lacks rights | Edit the cluster and paste a new kubeconfig |
| No models appear | No `AIGatewayRoute` in the gateway namespace matches the model header exactly | Check `oc get aigatewayroutes -n <namespace>`; see [architecture.md](architecture.md) |
| A cluster stays Pending | `config.autoSync` is off, or the last sync is still running | Press **Sync** |
| Server logs `waiting for postgres` at start | The database is still starting | Normal on a fresh install; it waits up to two minutes |
| Server exits with `ENCRYPTION_KEY must be 32 bytes` | The Secret value is not 32 random bytes in base64 | Recreate it with `openssl rand -base64 32` |
| Every request with a valid key gets 401 at the gateway | The key Secret has not reached that cluster | Check the cluster's sync status |
| Requests are rejected with 429 for a tenant that has no quota | The model has quotas for other tenants, so this one only has the shared pool | Give the tenant a quota |

## Checking what is on a cluster

```sh
oc get backends,aiservicebackends,aigatewayroutes,quotapolicies,securitypolicies,secrets \
  -n <gateway namespace> -l app.kubernetes.io/managed-by=aigw-ui
```

## Removing everything from a cluster by hand

```sh
oc delete backends,aiservicebackends,aigatewayroutes,quotapolicies,securitypolicies,secrets \
  -n <gateway namespace> -l app.kubernetes.io/managed-by=aigw-ui
```
