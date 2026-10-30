#!/bin/sh
# Writes `vllm serve --help=all` of the vLLM in an image, the flag list the
# hub's vLLM check reads (internal/vllmflags/catalogs/vllm-<version>.txt):
#
#   hack/vllm-flags.sh docker.io/vllm/vllm-openai-cpu:v0.32.0
#
# Any image of the version will do: the flags are the Python package's, the
# same in the CUDA and the CPU builds. The image runs without network. The
# first line of the file names the image; the rest is vLLM's help as printed.
set -eu
image=${1:?usage: hack/vllm-flags.sh IMAGE}
here=$(cd "$(dirname "$0")/.." && pwd)
vllm() { podman run --rm --network none --entrypoint vllm "$image" "$@" 2>/dev/null; }
version=$(vllm --version | tail -n 1)
version=${version%%+*} # a CPU build says 0.32.0+cpu: the flags are the release's
case $version in
[0-9]*.[0-9]*.[0-9]*) ;;
*) echo "no vLLM version from $image: '$version'" >&2; exit 1 ;;
esac
out=$here/internal/vllmflags/catalogs/vllm-$version.txt
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
{ echo "# image: $image"; vllm serve --help=all; } > "$tmp"
grep -q '^  --port ' "$tmp" || { echo "no flag list in the help of $image" >&2; exit 1; }
mkdir -p "$(dirname "$out")"
cat "$tmp" > "$out"
echo "vLLM $version: internal/vllmflags/catalogs/vllm-$version.txt"
