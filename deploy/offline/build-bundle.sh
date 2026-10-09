#!/usr/bin/env bash
# Run on a machine WITH internet access. Builds the application image, pulls
# the Postgres image, and packs both with the Helm chart into one tarball to
# carry into the disconnected environment.
#
#   deploy/offline/build-bundle.sh [version]
#
# Needs podman and helm. Set PLATFORM if your clusters are not x86_64, and
# APP_REPO to tag the application image under another name.
set -euo pipefail

cd "$(dirname "$0")/../.."
VERSION="${1:-$(sed -n 's/^appVersion: *"\(.*\)"/\1/p' deploy/chart/aigw-ui/Chart.yaml)}"
PLATFORM="${PLATFORM:-linux/amd64}"
PG_IMAGE="${PG_IMAGE:-quay.io/sclorg/postgresql-16-c9s:latest}"
APP_IMAGE="${APP_REPO:-ghcr.io/roiblum1/aigw-ui}:${VERSION}"
OUT="dist/aigw-ui-offline-${VERSION}"

rm -rf "$OUT" && mkdir -p "$OUT"

echo ">> building ${APP_IMAGE} for ${PLATFORM}"
podman build --platform "$PLATFORM" -t "$APP_IMAGE" -f Containerfile .

echo ">> pulling ${PG_IMAGE} for ${PLATFORM}"
podman pull --platform "$PLATFORM" "$PG_IMAGE"

echo ">> saving images"
podman save --multi-image-archive -o "$OUT/images.tar" "$APP_IMAGE" "$PG_IMAGE"

echo ">> packaging chart"
helm package deploy/chart/aigw-ui --app-version "$VERSION" --destination "$OUT" >/dev/null

cp deploy/offline/load-images.sh deploy/offline/INSTALL.md "$OUT/"
cat > "$OUT/bundle.env" <<ENV
VERSION=${VERSION}
APP_IMAGE=${APP_IMAGE}
PG_IMAGE=${PG_IMAGE}
ENV

tar -C dist -czf "${OUT}.tar.gz" "$(basename "$OUT")"
echo ">> done: ${OUT}.tar.gz ($(du -h "${OUT}.tar.gz" | cut -f1))"
