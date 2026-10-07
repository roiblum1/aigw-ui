# Installing in a disconnected environment

This bundle holds everything the application needs. Nothing is downloaded at
install time or at run time.

| File | What it is |
|---|---|
| `images.tar` | The application image and the Postgres image |
| `aigw-ui-<version>.tgz` | The Helm chart |
| `load-images.sh` | Pushes the images to your internal registry |

You need `podman`, `helm` and `oc` on a machine that can reach the internal
registry and the hub cluster.

## 1. Push the images

```sh
podman login registry.example.internal:5000
./load-images.sh registry.example.internal:5000
```

## 2. Install on the hub

```sh
helm upgrade --install aigw-ui ./aigw-ui-*.tgz \
  --namespace aigw-ui --create-namespace \
  --set global.imageRegistry=registry.example.internal:5000
```

If the registry needs credentials, create a pull secret in the namespace and
add `--set 'global.imagePullSecrets={my-pull-secret}'`.

## 3. Sign in

```sh
oc get route aigw-ui -n aigw-ui -o jsonpath='https://{.spec.host}{"\n"}'
oc get secret aigw-ui-auth -n aigw-ui -o jsonpath='{.data.admin-token}' | base64 -d; echo
```

## 4. Back up the encryption key

```sh
oc get secret aigw-ui-auth -n aigw-ui -o jsonpath='{.data.encryption-key}'; echo
```

The kubeconfigs and tenant API keys in Postgres are encrypted with this key.
If the Secret is lost, they cannot be read again and every cluster has to be
added again. The chart keeps the Secret on `helm uninstall` for that reason.

## Useful settings

| Setting | Default | Meaning |
|---|---|---|
| `route.host` | chosen by OpenShift | Hostname of the UI |
| `postgresql.storage.size` | `5Gi` | Size of the database volume |
| `postgresql.storage.storageClassName` | cluster default | Storage class for it |
| `postgresql.enabled` | `true` | `false` to use your own Postgres, with `externalDatabase.existingSecret` |
| `auth.existingSecret` | generated | Your own Secret with `admin-token` and `encryption-key` |
| `config.discoveryInterval` | `60s` | How often clusters are polled for models |

## Network access the hub needs

- From the application pod to the Kubernetes API of every LLM cluster (usually port 6443).
- From your browser and the self-service portal to the Route.
