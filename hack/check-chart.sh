#!/usr/bin/env bash
# Lints the chart and renders it the ways it is installed, so a template that
# only breaks with some setting is found here and not at an upgrade.
#
#   hack/check-chart.sh
set -euo pipefail

cd "$(dirname "$0")/.."
chart=deploy/chart/aigw-ui

helm lint "$chart"

render() {
  local name=$1
  shift
  helm template aigw-ui "$chart" --namespace aigw-ui "$@" >/dev/null || { echo "the chart does not render: $name"; exit 1; }
  echo "renders: $name"
}

render "defaults"
render "usage monitoring" --set redis.url=rediss://redis.example:6379 --set redis.allowReset=true --set redis.caConfigMap=redis-ca
render "entry routes" --set fleet.domain=example.com
render "external database and secret" --set postgresql.enabled=false --set externalDatabase.existingSecret=aigw-db --set auth.existingSecret=aigw-ui-auth
render "disconnected install" --set global.imageRegistry=registry.example:5000 --set 'global.imagePullSecrets[0]=pull-secret'
