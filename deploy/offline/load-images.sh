#!/usr/bin/env bash
# Run INSIDE the disconnected environment, from the unpacked bundle directory.
# Loads the images and pushes them to your internal registry.
#
#   ./load-images.sh registry.example.internal:5000
#
# Log in first (podman login <registry>). Add TLS_VERIFY=false for a registry
# with a certificate this machine does not trust.
set -euo pipefail

REGISTRY="${1:?usage: $0 <registry host[:port][/prefix]>}"
REGISTRY="${REGISTRY%/}"
cd "$(dirname "$0")"
. ./bundle.env

podman load -i images.tar

# The chart drops an image's own registry host and keeps the rest of its path.
strip_host() { case "${1%%/*}" in *.*|*:*|localhost) echo "${1#*/}" ;; *) echo "$1" ;; esac; }

for image in "$APP_IMAGE" "$PG_IMAGE"; do
  target="${REGISTRY}/$(strip_host "$image")"
  echo ">> ${image} -> ${target}"
  podman tag "$image" "$target"
  podman push --tls-verify="${TLS_VERIFY:-true}" "$target"
done

cat <<MSG

Images are in ${REGISTRY}. Install with:

  helm upgrade --install aigw-ui ./aigw-ui-*.tgz \\
    --namespace aigw-ui --create-namespace \\
    --set global.imageRegistry=${REGISTRY}
MSG
