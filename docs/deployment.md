# Deployment

## What the chart installs

The Helm chart is in `deploy/chart/aigw-ui`. In one namespace on the hub it
creates:

| Object | Purpose |
|---|---|
| Deployment `<release>` | The API server and UI, one replica |
| StatefulSet `<release>-postgresql` | One Postgres with a persistent volume |
| Route `<release>` | HTTPS access. Plain HTTP is redirected |
| Secret `<release>-auth` | Admin token and encryption key, generated once |
| Secret `<release>-postgresql` | Database password, generated once |
| NetworkPolicy | Only the server pod may reach Postgres |

Both pods run under the `restricted-v2` SCC: a random non-root UID, no added
capabilities, and a read-only root filesystem for the server.

## Connected install

```sh
helm upgrade --install aigw-ui deploy/chart/aigw-ui -n aigw-ui --create-namespace
```

Images used: `ghcr.io/roiblum1/aigw-ui:<version>` and
`quay.io/sclorg/postgresql-16-c9s:latest`.

## Disconnected install

**1. On a machine with internet access**, build the bundle. It needs `podman`
and `helm`.

```sh
deploy/offline/build-bundle.sh          # or: make bundle
```

This writes `dist/aigw-ui-offline-<version>.tar.gz` (about 180 MB) with:

| File | What it is |
|---|---|
| `images.tar` | The application image and the Postgres image, for `linux/amd64` |
| `aigw-ui-<version>.tgz` | The Helm chart |
| `load-images.sh` | Pushes the images to your registry |
| `INSTALL.md` | These steps |

| Variable | Default | Use |
|---|---|---|
| `PLATFORM` | `linux/amd64` | Target architecture |
| `PG_IMAGE` | `quay.io/sclorg/postgresql-16-c9s:latest` | For example `registry.redhat.io/rhel9/postgresql-16` |
| `APP_REPO` | `ghcr.io/roiblum1/aigw-ui` | Name the application image is tagged with |

**2. Carry the tarball inside**, then:

```sh
tar xzf aigw-ui-offline-<version>.tar.gz && cd aigw-ui-offline-<version>
podman login registry.example.internal:5000
./load-images.sh registry.example.internal:5000

helm upgrade --install aigw-ui ./aigw-ui-*.tgz -n aigw-ui --create-namespace \
  --set global.imageRegistry=registry.example.internal:5000
```

`global.imageRegistry` replaces the registry host of both images and keeps the
rest of the path, so they are pulled from
`<registry>/roiblum1/aigw-ui` and `<registry>/sclorg/postgresql-16-c9s`.
`load-images.sh` pushes to the same paths.

If the registry needs credentials, create a pull secret in the namespace and
add `--set 'global.imagePullSecrets={my-pull-secret}'`.

Nothing is downloaded at run time. The UI uses system fonts and bundled icons.

## After installing

```sh
oc get route aigw-ui -n aigw-ui -o jsonpath='https://{.spec.host}{"\n"}'
oc get secret aigw-ui-auth -n aigw-ui -o jsonpath='{.data.admin-token}' | base64 -d; echo
```

Back up the encryption key straight away. See
[operations.md](operations.md#back-up-the-encryption-key).

## Settings

| Value | Default | Meaning |
|---|---|---|
| `global.imageRegistry` | empty | Mirror registry for both images |
| `global.imagePullSecrets` | `[]` | Pull secret names |
| `image.repository` | `ghcr.io/roiblum1/aigw-ui` | Application image |
| `image.tag` | chart `appVersion` | Application image tag |
| `replicaCount` | `1` | Keep at 1; each replica polls and syncs every cluster |
| `config.discoveryInterval` | `60s` | How often clusters are polled for models. `0` turns it off |
| `config.autoSync` | `true` | Apply changes about a second after each edit |
| `redis.url` | empty | Redis to read live usage from: `redis://[:password@]host:6379` or `rediss://[:password@]<route host>:443`. Empty turns usage monitoring off |
| `redis.existingSecret` | empty | Secret with the key `url`, instead of `redis.url` |
| `redis.caSecret`, `redis.caConfigMap` | empty | Secret or ConfigMap with the key `ca.crt`: the CA of the Redis certificate |
| `redis.tlsInsecure` | `false` | Skip verification of the Redis certificate. For tests only |
| `redis.allowReset` | `false` | Allow resetting a tenant's usage. The server then deletes counters in Redis, so its Redis user needs `DEL` |
| `redis.keyPrefix` | empty | The rate limit service's `CACHE_KEY_PREFIX`, if set |
| `config.syncInterval` | `5m` | How often every cluster is synced again without a change. `0` turns it off. Needs `autoSync` |
| `fleet.domain` | empty | The sites' listener for other sites answers as `peers.llm.<domain>`. Empty, with `fleet.peerSNI` empty too: no entry route can be turned on |
| `fleet.peerSNI` | empty | That server name, when it is not `peers.llm.<domain>` |
| `fleet.peerCAConfigMap` | `llm-peer-ca` | ConfigMap in each gateway namespace with the CA of the sites' peer certificates |
| `fleet.peerClientSecret` | `llm-peer-client` | Secret in each gateway namespace with the certificate a gateway presents to another site |
| `fleet.sessionHeader` | `x-claude-code-session-id,x-openwebui-chat-id` | Requests with the same value in one of these headers go to the same site. One name per kind of client, separated by commas |
| `auth.existingSecret` | empty | Your own Secret with `admin-token` and `encryption-key` |
| `postgresql.enabled` | `true` | `false` to use your own Postgres |
| `postgresql.image.repository` / `.tag` | sclorg Postgres 16 | Postgres image |
| `postgresql.storage.size` | `5Gi` | Database volume size |
| `postgresql.storage.storageClassName` | cluster default | Storage class |
| `externalDatabase.existingSecret` | empty | Secret with the key `url` (`postgres://user:pass@host:5432/db`) |
| `route.enabled` | `true` | Create the Route |
| `route.host` | chosen by OpenShift | Hostname |
| `route.termination` | `edge` | TLS termination |
| `networkPolicy.enabled` | `true` | Restrict access to Postgres |
| `resources`, `nodeSelector`, `tolerations` | | Standard pod settings |

**Using your own secrets**

```sh
oc create secret generic my-aigw-auth -n aigw-ui \
  --from-literal=admin-token="$(openssl rand -hex 24)" \
  --from-literal=encryption-key="$(openssl rand -base64 32)"

helm upgrade --install aigw-ui deploy/chart/aigw-ui -n aigw-ui --set auth.existingSecret=my-aigw-auth
```

## Network access

| From | To | Port |
|---|---|---|
| Server pod on the hub | Kubernetes API of every LLM cluster | usually 6443 |
| Browsers and the self-service portal | The Route | 443 |
| Rate limit service on every LLM cluster | Redis on the hub (deployed separately) | your Redis port |

## Upgrade

```sh
helm upgrade aigw-ui deploy/chart/aigw-ui -n aigw-ui
```

The generated token, encryption key and database password are kept. Database
migrations run when the server starts.

## Uninstall

```sh
helm uninstall aigw-ui -n aigw-ui
```

This keeps the two Secrets and the database volume, so a reinstall finds its
data. To remove everything:

```sh
oc delete secret aigw-ui-auth aigw-ui-postgresql -n aigw-ui
oc delete pvc data-aigw-ui-postgresql-0 -n aigw-ui
```

Uninstalling does not remove objects that were applied to the LLM clusters.
Disable tenants and delete models first if you want them gone, or delete the
objects labelled `app.kubernetes.io/managed-by=aigw-ui` on each cluster.

## Tested on

OpenShift 4.20 (Kubernetes 1.33) on AWS, chart version 0.1.1 (0.2.0 has not been deployed there):

- fresh install, both pods ready under `restricted-v2` with no restarts;
- UI and API through the Route, HTTP redirected to HTTPS;
- `helm upgrade` keeps the generated secrets;
- data survives deleting both pods;
- with the cluster registered in itself: connection test, a real apply of the API key Secret, and its removal when enforcement was turned off.

That cluster has no Envoy Gateway, so applying gateway resources was not
tested there.
