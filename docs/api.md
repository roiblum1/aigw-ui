# API reference

Base path: `/api/v1`. Every request needs `Authorization: Bearer <admin token>`.
Bodies and responses are JSON. Unknown fields in a body are rejected.

```sh
export URL=https://$(oc get route aigw-ui -n aigw-ui -o jsonpath='{.spec.host}')
export TOKEN=$(oc get secret aigw-ui-auth -n aigw-ui -o jsonpath='{.data.admin-token}' | base64 -d)
curl -s -H "Authorization: Bearer $TOKEN" $URL/api/v1/overview
```

## Errors

| Status | Meaning |
|---|---|
| 400 | Invalid input. The body is `{"error": "..."}` |
| 401 | Missing or wrong token |
| 404 | The item, or an item it refers to, does not exist |
| 409 | An item with that name already exists |

Sync, probe and discover return 200 with `"ok": false` or `"reachable": false`
and an `error` text when the cluster call failed, because the request itself
succeeded.

## Overview

| Method | Path | Result |
|---|---|---|
| GET | `/overview` | Counts of clusters, models, tenants, active keys, quotas |
| GET | `/healthz` (no `/api/v1`, no token) | `ok` |

## Clusters

| Method | Path | Body | Result |
|---|---|---|---|
| GET | `/clusters` | | List |
| POST | `/clusters` | cluster | Created cluster |
| PUT | `/clusters/{id}` | cluster; empty `kubeconfig` keeps the stored one | Updated cluster |
| DELETE | `/clusters/{id}` | | 204. Applied objects stay on the cluster |
| POST | `/clusters/{id}/probe` | | Reachability, CRDs, gateway |
| POST | `/clusters/{id}/sync` | | Applied and removed counts |
| POST | `/clusters/{id}/discover` | | Number of models found |
| GET | `/clusters/{id}/manifests` | | `{"yaml": "..."}`, key values masked |
| POST | `/sync` | | Syncs all, returns the cluster list |
| POST | `/discover` | | Polls all, returns the cluster list |

```json
{
  "name": "ocp4-prod-llm-site1-a",
  "site": "site1",
  "namespace": "ai-gateway",
  "gateway_name": "llm",
  "auth_enabled": false,
  "kubeconfig": "apiVersion: v1\nkind: Config\n..."
}
```

The kubeconfig is never returned.

## Models

| Method | Path | Body | Result |
|---|---|---|---|
| GET | `/models` | | List with endpoints |
| POST | `/models` | model | `{"id": "..."}` |
| PUT | `/models/{id}` | model; `name` is ignored | `{"id": "..."}` |
| DELETE | `/models/{id}` | | 204 |

```json
{
  "name": "GLM5.3",
  "default_limit": 1,
  "default_window": "1d",
  "endpoints": [
    {"cluster_id": "<id>", "host": "glm.models.svc.cluster.local", "port": 8000, "upstream_model": ""}
  ]
}
```

`endpoints` holds only the manual endpoints. Discovered endpoints are returned
by `GET` with `"source": "discovered"` and cannot be set.

## Tenants, keys and quotas

| Method | Path | Body | Result |
|---|---|---|---|
| GET | `/tenants` | | List with key and quota counts |
| POST | `/tenants` | `{"slug", "display_name"}` | Created tenant |
| GET | `/tenants/{id}` | | `{"tenant", "keys", "quotas"}` |
| PUT | `/tenants/{id}` | `{"display_name", "enabled"}` | Updated tenant |
| DELETE | `/tenants/{id}` | | 204 |
| POST | `/tenants/{id}/keys` | `{"name"}` | `{"key": {...}, "secret": "sk-..."}` |
| DELETE | `/keys/{id}` | | 204, key revoked |
| PUT | `/tenants/{id}/quotas` | `{"model_id", "token_limit", "window"}` | The tenant's quotas |
| DELETE | `/quotas/{id}` | | 204 |

`window` is `1m`, `1h` or `1d`. `PUT .../quotas` creates the quota or replaces
the existing one for that model. The `secret` of a key is returned only by the
call that creates it.

## Example: what a portal does when a request is approved

```sh
api() { curl -s -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' "$@"; }

TENANT=$(api -X POST $URL/api/v1/tenants -d '{"slug":"team-search","display_name":"Search team"}' | jq -r .id)
MODEL=$(api $URL/api/v1/models | jq -r '.[] | select(.name=="GLM5.3") | .id')

api -X PUT $URL/api/v1/tenants/$TENANT/quotas \
  -d "{\"model_id\":\"$MODEL\",\"token_limit\":2000000,\"window\":\"1d\"}"

api -X POST $URL/api/v1/tenants/$TENANT/keys -d '{"name":"portal"}' | jq -r .secret
```

The clusters are synced about a second after each call when `AUTO_SYNC` is on.
