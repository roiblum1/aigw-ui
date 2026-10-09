#!/usr/bin/env bash
# Runs the tests that need a real Kubernetes API server (the ones named
# TestRealAPIServer*). Server-side apply is done by the API server, so those
# tests cannot use a stand-in.
#
# Starts a throwaway etcd and kube-apiserver from the envtest binaries on
# 127.0.0.1, runs the tests and stops both. Needs Go and internet access the
# first time, to download the binaries.
set -euo pipefail

cd "$(dirname "$0")/.."
bin="$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@latest use -p path)"
work="$(mktemp -d)"
etcd_port=23791 peer_port=23801 api_port=16444

stop() {
  kill "${api_pid:-}" "${etcd_pid:-}" 2>/dev/null || true
  rm -rf "$work"
}
trap stop EXIT

"$bin/etcd" --data-dir "$work/etcd" \
  --listen-client-urls "http://127.0.0.1:$etcd_port" --advertise-client-urls "http://127.0.0.1:$etcd_port" \
  --listen-peer-urls "http://127.0.0.1:$peer_port" >"$work/etcd.log" 2>&1 &
etcd_pid=$!

token="$(openssl rand -hex 16)"
echo "$token,admin,admin,system:masters" >"$work/tokens.csv"
openssl genrsa -out "$work/sa.key" 2048 2>/dev/null

"$bin/kube-apiserver" --etcd-servers "http://127.0.0.1:$etcd_port" \
  --secure-port "$api_port" --bind-address 127.0.0.1 --cert-dir "$work/certs" \
  --service-account-key-file "$work/sa.key" --service-account-signing-key-file "$work/sa.key" \
  --service-account-issuer https://test --token-auth-file "$work/tokens.csv" \
  --authorization-mode AlwaysAllow --service-cluster-ip-range 10.0.0.0/24 >"$work/apiserver.log" 2>&1 &
api_pid=$!

cat >"$work/kubeconfig" <<KUBECONFIG
apiVersion: v1
kind: Config
clusters:
  - name: test
    cluster: {server: "https://127.0.0.1:$api_port", insecure-skip-tls-verify: true}
users:
  - name: admin
    user: {token: "$token"}
contexts:
  - name: test
    context: {cluster: test, user: admin}
current-context: test
KUBECONFIG
kc() { "$bin/kubectl" --kubeconfig "$work/kubeconfig" --request-timeout=10s "$@"; }

for _ in $(seq 60); do
  kc get --raw /readyz >/dev/null 2>&1 && break
  sleep 1
done
kc get --raw /readyz >/dev/null || { echo "the API server did not start:"; tail -20 "$work/apiserver.log"; exit 1; }

# The gateway CRDs are the real ones, at the versions the clusters run, so the
# API server checks what this tool renders against their schema and rules.
eg="https://raw.githubusercontent.com/envoyproxy/gateway/${ENVOY_GATEWAY_VERSION:-v1.8.5}/charts/gateway-helm/charts/crds/crds/generated"
aigw="https://raw.githubusercontent.com/envoyproxy/ai-gateway/${AI_GATEWAY_VERSION:-v1.1.0}/manifests/charts/ai-gateway-crds-helm/templates"
for crd in \
  "$eg/gateway.envoyproxy.io_backends.yaml" \
  "$eg/gateway.envoyproxy.io_backendtrafficpolicies.yaml" \
  "$eg/gateway.envoyproxy.io_securitypolicies.yaml" \
  "$aigw/aigateway.envoyproxy.io_aigatewayroutes.yaml" \
  "$aigw/aigateway.envoyproxy.io_aiservicebackends.yaml" \
  "$aigw/aigateway.envoyproxy.io_quotapolicies.yaml"; do
  curl -fsSL "$crd" | kc apply --server-side -f -
done
kc apply -f internal/kube/testdata/llminferenceservice-crd.yaml
kc wait --for=condition=Established crd --all --timeout=60s

KUBE_TEST_KUBECONFIG="$work/kubeconfig" go test ./internal/kube/ -run RealAPIServer -count=1 -v
