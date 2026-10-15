#!/usr/bin/env bash
# Run on a machine WITH internet access. Reads today's market prices into
# internal/market/openrouter.json, builds the application image and the AIPerf
# image for capacity measurements, pulls the Postgres image, and packs them
# with the Helm chart into one tarball to carry into the disconnected
# environment.
#
#   deploy/offline/build-bundle.sh [version]
#
# Needs podman and helm. Set PLATFORM if your clusters are not x86_64, and
# APP_REPO or AIPERF_REPO to tag the images under other names.
set -euo pipefail

cd "$(dirname "$0")/../.."
VERSION="${1:-$(sed -n 's/^appVersion: *"\(.*\)"/\1/p' deploy/chart/aigw-ui/Chart.yaml)}"
PLATFORM="${PLATFORM:-linux/amd64}"
PG_IMAGE="${PG_IMAGE:-quay.io/sclorg/postgresql-16-c9s:latest}"
APP_IMAGE="${APP_REPO:-ghcr.io/roiblum1/aigw-ui}:${VERSION}"
AIPERF_VERSION="$(sed -n 's/^ARG AIPERF_VERSION=//p' deploy/aiperf/Containerfile)"
AIPERF_IMAGE="${AIPERF_REPO:-ghcr.io/roiblum1/aigw-aiperf}:${AIPERF_VERSION}"
OUT="dist/aigw-ui-offline-${VERSION}"

rm -rf "$OUT" && mkdir -p "$OUT"

# This machine has internet access: the image gets today's market prices.
echo ">> reading the market snapshot"
hack/fetch-market.sh
MARKET_DATE="$(python3 -c 'import json; print(json.load(open("internal/market/openrouter.json"))["snapshot_date"])')"

echo ">> building ${APP_IMAGE} for ${PLATFORM}"
podman build --platform "$PLATFORM" -t "$APP_IMAGE" -f Containerfile .

echo ">> building ${AIPERF_IMAGE} for ${PLATFORM}"
podman build --platform "$PLATFORM" -t "$AIPERF_IMAGE" -f deploy/aiperf/Containerfile deploy/aiperf

echo ">> pulling ${PG_IMAGE} for ${PLATFORM}"
podman pull --platform "$PLATFORM" "$PG_IMAGE"

echo ">> saving images"
podman save --multi-image-archive -o "$OUT/images.tar" "$APP_IMAGE" "$AIPERF_IMAGE" "$PG_IMAGE"

echo ">> packaging chart"
helm package deploy/chart/aigw-ui --app-version "$VERSION" --destination "$OUT" >/dev/null

cp deploy/offline/load-images.sh deploy/offline/INSTALL.md "$OUT/"
cat > "$OUT/bundle.env" <<ENV
VERSION=${VERSION}
APP_IMAGE=${APP_IMAGE}
AIPERF_IMAGE=${AIPERF_IMAGE}
PG_IMAGE=${PG_IMAGE}
MARKET_SNAPSHOT=${MARKET_DATE}
ENV

tar -C dist -czf "${OUT}.tar.gz" "$(basename "$OUT")"
echo ">> done: ${OUT}.tar.gz ($(du -h "${OUT}.tar.gz" | cut -f1))"
