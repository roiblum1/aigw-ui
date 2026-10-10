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
`usage.reset`, `sync`, `discovery`, `cluster.delete` and `selftest`.

## Documents

| Method | Path | Result |
|---|---|---|
| GET | `/docs/platform-architecture` | The platform design as one HTML page, `text/html` |
| GET | `/docs` | The guides and release notes built into the server: `[{"name", "title", "group"}]`. `group` is `Guides` or `Release notes` |
| GET | `/docs/text/{name}` | One document: `{"name", "title", "markdown"}`. `name` is one from the list, for example `user-guide` or `release-notes/v0.8.0`. 404 for any other |

## Audit log

| Method | Path | Result |
|---|---|---|
| GET | `/audit` | The newest 100 entries, newest first. `?limit=` up to 500, `?before=<id>` for older ones |

Every request that is not a `GET` adds an entry, whether it succeeded or not.
Request bodies are never stored.

```json
{
  "id": 912,
  "at": "2026-10-08T09:30:00Z",
  "actor": "admin token",
  "on_behalf_of": "alice",
  "method": "PUT",
  "path": "/api/v1/tenants/<id>/quotas",
  "status": 200,
  "action": "quota.set",
  "summary": "Set quota of team-a on GLM5.3 to 2000000 tokens per day",
  "remote_addr": "10.128.0.7",
  "forwarded_for": "192.168.4.20",
  "user_agent": "portal/1.4",
  "duration_ms": 12
}
```

- `on_behalf_of` is the `X-On-Behalf-Of` request header. A portal should set
  it to the user it acts for. It may be percent-encoded for names outside
  ASCII. The server does not verify it.
- `action` and `summary` are empty when the request was refused before it
  changed anything; `status` then says why (400, 404, 409).
- `remote_addr` is the peer of the connection, which is the router when the
  server runs behind a Route. `forwarded_for` is the `X-Forwarded-For` header
  as received.

## Usage

| Method | Path | Result |
|---|---|---|
| GET | `/usage` | Tokens used in the current window, for every tenant quota |
| GET | `/usage?tenant_id=<id>` | The same for one tenant |
| GET | `/usage/history?step=day&days=30` | What every tenant used of every model per day or hour |
| GET | `/my/usage` | A tenant's own budgets, usage and prices. Takes one of the tenant's API keys as the bearer token, not the admin token |
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

`overage_used` is what the tenant used as best-effort, after its budget was
spent. It is counted apart and is not part of `used`. `best_effort_until` is
there while the tenant's requests for the model are served as best-effort.

`best_effort_limit` is the model's limit on best-effort use, when it has
one, and `best_effort_capped` is `true` once the tenant reached it: it is
refused until `best_effort_until`.

### History

`GET /usage/history` takes `step` (`day`, the default, or `hour`), `days`
(1 to 400 for days, 1 to 31 for hours; the default is 30 days or 2 days of
hours) and `tenant_id`.

```json
{
  "from": "2026-10-09T00:00:00Z",
  "step": "hour",
  "points": [
    {"at": "2026-10-10T17:00:00Z", "tenant_id": "<id>", "tenant_slug": "team-b",
     "model_id": "<id>", "model_name": "GLM5.3", "unit": "credits",
     "used": 5268, "best_effort": 0}
  ]
}
```

`from` is the start of the first step. A step in which nothing was used has
no point. `used` is what was used within the tenant's budget and
`best_effort` what was used after it, in the model's `unit`. Times are UTC.
The server reads the counters once a minute, so the last minute is missing.

### A tenant's own usage

`GET /my/usage` is for the tenant itself. The bearer token is one of its
API keys. It answers 401 for a key that is revoked, unknown or of a tenant
that is turned off, 429 after ten wrong keys from one address in a minute,
and 404 when `config.tenantPage` is off.

```json
{
  "tenant": "team-b", "display_name": "Team B", "at": "2026-10-10T17:20:11Z",
  "usage_enabled": true,
  "budgets": [
    {"model_name": "GLM5.3", "unit": "credits", "limit": 5000, "window": "1h",
     "used": 3184, "resets_at": "2026-10-10T18:00:00Z", "enforced": true,
     "best_effort_used": 0}
  ],
  "prices": [{"model_name": "GLM5.3", "price_input": 11574, "price_cached": 1157, "price_output": 46296}],
  "days_from": "2026-09-11T00:00:00Z", "days": [ ... ],
  "hours_from": "2026-10-09T00:00:00Z", "hours": [ ... ]
}
```

`enforced` is `false` for a dry-run budget. `days` and `hours` are points
as in the history above, for this tenant only. Prices are in credits for a
million tokens.

A reset deletes the counter of the current window, on every cluster that
shares it. The limit and the window do not change, and the window still ends
at `resets_at`. A tenant that was being served as best-effort is served as
standard again (`ended_overage` in the answer). It returns 403 unless `redis.allowReset` is on (`can_reset` in
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
| POST | `/clusters/{id}/selftest` | optional `{"model_id": "<id>"}` | 202 and the run. It continues in the background |
| GET | `/clusters/{id}/selftest` | | The last run of this cluster. 404 if none since the server started |
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
  "discovery_token": "",
  "fleet_enabled": false,
  "client_listener": "",
  "peer_host": "",
  "peer_port": 8443
}
```

`fleet_enabled` makes the cluster one of the sites that share traffic. It
needs `auth_enabled`, `client_listener`, `peer_host` and the same `namespace`
as the other fleet clusters; the request is refused with 400 otherwise.
`peer_host` and `peer_port` are where the other sites reach this gateway.

A cluster in a response also carries `fleet_revision`, which identifies the
entry routes last applied to it, and `fleet_outdated`, true for a fleet
cluster whose revision is not the fleet's current one. Both are read-only.
`client_listener` is the Gateway listener the API-key policy attaches to;
empty attaches it to the whole Gateway.

`gateway_url` and `discovery_token` are optional. The URL must be the address
only; a path such as `/v1/models` is rejected. The kubeconfig and the token
are never returned. A cluster reports `has_discovery_token` instead.

A self-test run looks like this. `status` is `running`, `passed` or `failed`;
a step is `pending`, `running`, `passed`, `failed`, `warning` (no clear
answer) or `skipped`. Poll the `GET` until `status` is no longer `running`.
A second `POST` while one runs, on any cluster, answers 409.

```json
{
  "cluster_id": "<id>", "cluster_name": "site1-a",
  "model_id": "<id>", "model_name": "GLM5.3",
  "status": "passed",
  "started_at": "2026-10-08T09:30:00Z", "finished_at": "2026-10-08T09:30:41Z",
  "steps": [
    {"id": "gateway", "title": "The gateway lists the model", "status": "passed", "detail": "..."},
    {"id": "apply", "title": "A temporary tenant, key and quota are applied", "status": "passed", "detail": "..."}
  ]
}
```

The step IDs are `gateway`, `apply`, `key`, `bad-key`, `counter`, `limit`,
`reset` and `cleanup`.

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
Empty charges `total_tokens`. `input_tokens` includes the cached part.

The expression is compiled and run once with every count at 0, as the
gateway does. One the gateway would not use is refused with 400: an unknown
name, a number without `u` next to a count, a result that is not a whole
number, a division by a count. It can be at most 1000 characters.

Each endpoint of a model carries `capacity`, and the model `site_weights`:

```json
{
  "name": "glm-5.3",
  "endpoints": [
    {"cluster_name": "ocp4-prod-llm-site1-a", "source": "discovered",
     "capacity": {"observed": 8, "observed_at": "2026-10-09T09:30:00Z",
                  "weight": 8, "changed_at": "2026-10-09T08:12:00Z",
                  "detail": "894-llms/glm53: 8 of 8 ready",
                  "serving": true, "drained": false,
                  "revision": "glm-5.3-fp8-2026-09-14", "max_model_len": "262144",
                  "pools": [{"namespace": "894-llms", "name": "glm53-inference-pool",
                             "group": "inference.networking.k8s.io"}]}}
  ],
  "warnings": [],
  "fleet": false,
  "site_weights": [{"zone": "ocp4-prod-llm-site1-a", "weight": 800},
                   {"zone": "ocp4-prod-llm-site2-a", "weight": 300}],
  "site_weights_note": ""
}
```

`observed` and `weight` are `null` while the cluster has not reported a
capacity. `site_weights` is what a sync writes to the gateways; when it is
empty, `site_weights_note` says why. `warnings` lists differences in
revision or request length between the sites. These fields are read-only.

| Method | Path | Body | Result |
|---|---|---|---|
| PUT | `/models/{id}/sites/{cluster_id}/drain` | `{"drained": true}` or `false` | 204. 409 when no other site has capacity for the model |
| PUT | `/models/{id}/fleet` | `{"enabled": true}` or `false` | 204. 409 when no fleet cluster serves the model, the model has manual endpoints, the server has no `FLEET_DOMAIN`, or the model is in best-effort mode and is being turned off |
| PUT | `/models/{id}/spent` | `{"mode": "best-effort", "best_effort_unlimited": false, "best_effort_limit": null}` | 204. `mode` is `refuse` or `best-effort`. `best_effort_limit` is the most a tenant may use as best-effort in one period of its quota, in the model's unit, or `null`. It is not kept from the last call: send it every time. 409 when the model has no entry route |
| GET | `/overage?limit=100` | | The periods in which a tenant was served as best-effort, newest first: `model_id`, `model_name`, `tenant_id`, `tenant_slug`, `since`, `until`, `active`, `capped_at`. `limit` is at most 500 |

A drained site's weight steps down to 1, one instance per poll, and the site
then leaves `site_weights`. `fleet` on a model says whether the hub renders
its entry route on every fleet cluster.

`spent_mode` on a model says what happens to a tenant whose budget for it is
spent. With `best-effort` the tenant is not refused: once it has used 90% of
an hourly or daily quota, its requests for the model are sent as the lowest
class until the window ends. `best_effort_unlimited` does the same, all the
time, for tenants that have no quota on the model. With a
`best_effort_limit`, a tenant that has used that much as best-effort in the
period is taken off the best-effort route and refused until the period
ends; `capped_at` in `/overage` says when. See
[how it works](how-it-works.md#best-effort-when-a-budget-is-spent).

### Prices

A model with prices is counted in credits of $0.00001 in place of tokens.
See [charging by cost](pricing.md).

| Method | Path | Body | Result |
|---|---|---|---|
| GET | `/models/{id}/prices` | | Every set of prices, newest first |
| PUT | `/models/{id}/prices` | `{"input_usd", "cached_usd", "output_usd", "now", "note"}` | The list. Replaces prices that were waiting |
| DELETE | `/models/{id}/prices/pending` | | 204. 404 when none were waiting |
| PUT | `/models/{id}/prices/dry-run` | `{"enabled"}` | 204. 409 for a model without prices in use |

- The three prices are dollars for a million tokens. They are stored as
  whole credits, so $0.11574 is 11574. `cached_usd` cannot be above
  `input_usd`.
- Prices start at the next 00:00 UTC. `"now": true` starts them at once
  and is answered with 409 for a model that has tenant quotas.
- A set of prices carries `effective_at` and `applied_at`, which is null
  while it waits.

A model carries `unit` (`tokens` or `credits`), `prices`, `pending_prices`
and `price_dry_run`. For a model in credits, `default_limit`, a quota's
`token_limit` and every amount in the usage report are credits. The
fields keep their names. Quotas and the usage report carry `unit` too, and
the usage report `dry_run` while nobody is refused.

`default_limit` and `token_limit` can be at most 4294967295, in either
unit. The gateway cannot count further in one window.

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

A tenant slug must not start with `selftest-`. That prefix belongs to the
self-test, which deletes leftover tenants by it.

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
