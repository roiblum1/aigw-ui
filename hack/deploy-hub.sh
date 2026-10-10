#!/usr/bin/env bash
# Upgrades the hub's Helm release to the image of one commit. CI runs it after
# the image of a commit on main is pushed. It keeps the values the release
# has and changes the image tag alone.
#
#   HUB_SERVER      API server of the hub's cluster, https://...:6443
#   HUB_TOKEN       token of an account that may change the release's namespace
#   HUB_CA          the cluster's CA bundle, PEM
#   HUB_NAMESPACE   namespace of the release (default aigw-ui)
#   HUB_RELEASE     name of the release (default aigw-ui)
#   IMAGE_TAG       image tag to run, such as sha-0123456789ab
#   DRY_RUN         set to 1 to ask the cluster what would change, and stop
#
# A failed upgrade is rolled back to the release that was running.
set -euo pipefail

cd "$(dirname "$0")/.."
: "${HUB_SERVER:?}" "${HUB_TOKEN:?}" "${HUB_CA:?}" "${IMAGE_TAG:?}"
namespace="${HUB_NAMESPACE:-aigw-ui}"
release="${HUB_RELEASE:-aigw-ui}"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
umask 077
printf '%s\n' "$HUB_CA" >"$work/ca.crt"
printf '%s' "$HUB_TOKEN" >"$work/token"
cat >"$work/kubeconfig" <<KUBECONFIG
apiVersion: v1
kind: Config
clusters:
  - name: hub
    cluster: {server: "$HUB_SERVER", certificate-authority: "$work/ca.crt"}
users:
  - name: deployer
    user: {tokenFile: "$work/token"}
contexts:
  - name: hub
    context: {cluster: hub, user: deployer, namespace: "$namespace"}
current-context: hub
KUBECONFIG
export KUBECONFIG="$work/kubeconfig"

# Helm 4 renamed --atomic.
rollback=--atomic
if helm upgrade --help | grep -q -- --rollback-on-failure; then
  rollback=--rollback-on-failure
fi
args=("$release" deploy/chart/aigw-ui --namespace "$namespace" --reset-then-reuse-values --set "image.tag=$IMAGE_TAG")
if [ "${DRY_RUN:-}" = 1 ]; then
  helm upgrade "${args[@]}" --dry-run=server >/dev/null
  echo "dry run: the cluster accepts the upgrade of $release to $IMAGE_TAG"
  exit 0
fi

echo "before: $(helm list --namespace "$namespace" --filter "^$release\$" --no-headers)"
helm upgrade "${args[@]}" --wait --timeout 5m "$rollback"
tag="$(helm get values "$release" --namespace "$namespace" --output json | sed -n 's/.*"tag":"\([^"]*\)".*/\1/p')"
[ "$tag" = "$IMAGE_TAG" ] || { echo "the release runs image tag '$tag', not $IMAGE_TAG" >&2; exit 1; }
echo "after:  $(helm list --namespace "$namespace" --filter "^$release\$" --no-headers)"
