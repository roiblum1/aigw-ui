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

## Task log

| Method | Path | Result |
|---|---|---|
| GET | `/tasks` | The newest 50 tasks, newest first. `?limit=` up to 500 |

Every call that changes something adds a task. A portal can poll this to see
whether its request reached the clusters.

```json
{
  "id": 42,
  "action": "quota.set",
  "summary": "Set quota of team-a on GLM5.3 to 2000000 tokens per day",
  "status": "succeeded",
  "message": "",
  "created_at": "2026-10-08T09:30:00Z",
  "results": [
    {
      "cluster_name": "site1-a",
      "status": "succeeded",
      "message": "1 object changed, 5 already in place.",
      "changes": [
        {"kind": "QuotaPolicy", "namespace": "ai-gateway", "name": "glm5-3",
         "action": "updated", "gateway": "Accepted", "gateway_message": "..."}
      ],
      "rejected": [],
      "finished_at": "2026-10-08T09:30:04Z"
    }
  ]
}
```

- `status` is `pending` until every cluster has synced, `failed` when a
  cluster could not be synced, otherwise `succeeded`. A failed cluster is
  tried again by the next sync, and the task turns `succeeded` when it works.
- `changes` lists the objects that sync created, updated or deleted, after
  the cluster's API server confirmed them. `gateway` is the condition the
  gateway's controller set on the object a moment later.
- `rejected` lists objects the gateway reports as `NotAccepted`. The object
  exists, but the gateway is not using it.
- Changes made close together are applied by one sync, so their tasks show
  the same list of objects.

`action` is one of `cluster.add`, `cluster.update`, `model.add`,
`model.update`, `model.delete`, `tenant.add`, `tenant.update`,
`tenant.delete`, `key.add`, `key.revoke`, `quota.set`, `quota.delete`,
`usage.reset`, `sync` and `discovery`.

## Usage

| Method | Path | Result |
|---|---|---|
| GET | `/usage` | Tokens used in the current window, for every tenant quota |
| GET | `/usage?tenant_id=<id>` | The same for one tenant |
| POST | `/tenants/{id}/quotas/{model_id}/reset` | `{"counters": 1, "deleted": 1}`. The tenant's usage on that model is back to zero |

```json
{
  "enabled": true,
  "can_reset": false,
  "at": "2026-10-08T09:31:56Z",
  "quotas": [
    {
      "tenant_id": "<id>", "tenant_slug": "team-b",
      "model_id": "<id>", "model_name": "GLM5.3",
      "limit": 50000, "window": "1h", "shadow": false,
      "used": 46800,
      "resets_at": "2026-10-08T10:00:00Z",
      "counters": [
        {"backend": "ai-gateway/glm5-3", "clusters": ["site1-a", "site2-a"], "used": 46800}
      ]
    }
  ],
  "pools": [
    {"model_id": "<id>", "model_name": "GLM5.3", "limit": 2500000, "window": "1d",
     "used": 1182572, "resets_at": "2026-10-09T00:00:00Z", "counters": [ ... ]}
  ]
}
```

The numbers are read from the rate limit counters in Redis at the moment of
the call. `enabled` is `false` when the server has no Redis configured.
`pools` holds the same numbers for each model's shared pool, the bucket every
request to the model is charged to. It is left out when `tenant_id` is given.

`counters` has more than one entry when the model's backends are named
differently between clusters; `used` is then the highest of them, because
each counter is held to the limit on its own. `hint` appears when no counter
was found and says what Redis holds instead. If Redis cannot be reached the
call returns 502.

A reset deletes the counter of the current window, on every cluster that
shares it. The limit and the window do not change, and the window still ends
at `resets_at`. It returns 403 unless `redis.allowReset` is on (`can_reset` in
`GET /usage`), and 404 when the tenant has no applied quota on that model.

## Clusters

| Method | Path | Body | Result |
|---|---|---|---|
| GET | `/clusters` | | List |
| POST | `/clusters` | cluster | Created cluster |
| PUT | `/clusters/{id}` | cluster; fields left out keep their stored value | Updated cluster |
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
  "kubeconfig": "apiVersion: v1\nkind: Config\n...",
  "gateway_url": "http://192.168.1.9",
  "discovery_token": ""
}
```

`gateway_url` and `discovery_token` are optional. The URL must be the address
only; a path such as `/v1/models` is rejected. The kubeconfig and the token
are never returned. A cluster reports `has_discovery_token` instead.

On update, any field that is left out keeps its stored value, so a body with
only `{"site": "site2"}` changes the site and nothing else. An empty
`kubeconfig` or `discovery_token` also keeps the stored one. To remove the
gateway URL send `"gateway_url": ""`; that removes the token too.

## Models

| Method | Path | Body | Result |
|---|---|---|---|
| GET | `/models` | | List with endpoints |
| POST | `/models` | model | `{"id": "..."}` |
| PUT | `/models/{id}` | model; `name` is ignored, fields left out keep their stored value | `{"id": "..."}` |
| DELETE | `/models/{id}` | | 204 |

```json
{
  "name": "GLM5.3",
  "default_limit": 1,
  "default_window": "1d",
  "cost_expression": "",
  "endpoints": [
    {"cluster_id": "<id>", "host": "glm.models.svc.cluster.local", "port": 8000, "upstream_model": ""}
  ]
}
```

`endpoints` holds only the manual endpoints. Discovered endpoints are returned
by `GET` with `"source": "discovered"` and cannot be set. On update, leaving
`endpoints` out keeps the manual endpoints; `"endpoints": []` removes them.

`cost_expression` is optional. It is a CEL expression over `input_tokens`,
`output_tokens`, `total_tokens`, `cached_input_tokens`,
`cache_creation_input_tokens` and `reasoning_tokens` that says how much a
request charges to quotas, for example `input_tokens + output_tokens * 4u`.
The counts are unsigned integers, so number literals need the `u` suffix.
Empty charges `total_tokens`.

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
| PUT | `/tenants/{id}/quotas` | `{"model_id", "token_limit", "window", "shadow"}` | The tenant's quotas |
| DELETE | `/quotas/{id}` | | 204 |

`window` is `1m`, `1h` or `1d`. `PUT .../quotas` creates the quota or replaces
the existing one for that model. It returns 400 when the model has nothing a
quota can attach to (`"quota_capable": false` in `GET /models`).

`shadow` is optional. With `true` the quota is a dry run: usage is counted
but the quota never rejects a request. Left out, a new quota is enforced and
an existing one keeps its setting.

The `secret` of a key is returned only by the
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
