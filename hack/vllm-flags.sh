#!/bin/sh
# Writes the flag list of the vLLM in an image, for the hub's flag check, and
# vLLM's own verdicts on internal/vllmflags/testdata/cases.json (the test
# TestVerdictsMatchVLLM compares the hub's with them):
#
#   hack/vllm-flags.sh docker.io/vllm/vllm-openai-cpu:v0.32.0
#
# Any image of the version will do: the flags are the Python package's, the
# same in the CUDA and the CPU builds. The image runs without network.
set -eu
image=${1:?usage: hack/vllm-flags.sh IMAGE}
here=$(cd "$(dirname "$0")/.." && pwd)
run() {
  podman run --rm --network none --entrypoint python3 \
    -v "$here/hack/vllm-flags.py:/vllm-flags.py:ro" \
    -v "$here/internal/vllmflags/testdata/cases.json:/cases.json:ro" \
    "$image" /vllm-flags.py "$@"
}
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
run catalog "$image" > "$tmp"
version=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["vllm"])' "$tmp")
mkdir -p "$here/internal/vllmflags/catalogs"
cat "$tmp" > "$here/internal/vllmflags/catalogs/vllm-$version.json"
run verdicts /cases.json > "$here/internal/vllmflags/testdata/verdicts-$version.json"
echo "vLLM $version: internal/vllmflags/catalogs/vllm-$version.json, testdata/verdicts-$version.json"
