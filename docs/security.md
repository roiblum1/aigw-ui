# Security

## Where secrets are kept

| Secret | Stored in | Protection |
|---|---|---|
| Cluster kubeconfigs | Postgres, `clusters.kubeconfig_enc` | AES-256-GCM |
| Tenant API keys | Postgres, `api_keys.key_enc` | AES-256-GCM |
| Encryption key | Kubernetes Secret `<release>-auth` on the hub | Kubernetes RBAC, etcd encryption if enabled |
| Admin token | Kubernetes Secret `<release>-auth` on the hub | same |
| Postgres password | Kubernetes Secret `<release>-postgresql` | same |
| Tenant API keys on the LLM clusters | Kubernetes Secret `aigw-ui-api-keys` in the gateway namespace | Kubernetes RBAC on that cluster |

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
the one Secret named `aigw-ui-api-keys`; and get on the `Gateway`. It never
lists Secrets. A ServiceAccount kubeconfig limited to that would reduce the
damage if the hub were compromised.

## Tenant API keys

Keys are stored encrypted rather than hashed, because every sync has to write
them to the clusters. The UI and API show a key only once, when it is created.
Afterwards only the first characters are shown.

## Signing in

The UI and the API share one admin token. Everyone who has it has full access,
and there is no per-user audit trail. LDAP login is not built yet.

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

## What the tool can change on an LLM cluster

It applies and deletes only objects labelled
`app.kubernetes.io/managed-by: aigw-ui`, in the gateway namespace you set. The
label is checked on the hub before any delete.

Turning on "Enforce API keys" attaches API key auth to the whole gateway. That
is the one setting that affects traffic this tool does not otherwise manage.
