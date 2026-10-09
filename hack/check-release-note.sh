#!/usr/bin/env bash
# Fails unless the version in the chart has a release note. With a tag as the
# first argument, the tag must also be that version.
#
#   hack/check-release-note.sh            # on every change
#   hack/check-release-note.sh v0.7.0     # when releasing a tag
#
# Prints the path of the release note.
set -euo pipefail

cd "$(dirname "$0")/.."
chart=deploy/chart/aigw-ui/Chart.yaml
version="$(sed -n 's/^appVersion: *"\{0,1\}\([^"]*\)"\{0,1\} *$/\1/p' "$chart")"
chart_version="$(sed -n 's/^version: *"\{0,1\}\([^"]*\)"\{0,1\} *$/\1/p' "$chart")"
note="docs/release-notes/v$version.md"

fail() { echo "release note check: $*" >&2; exit 1; }

[ -n "$version" ] || fail "no appVersion in $chart"
[ "$version" = "$chart_version" ] || fail "$chart has version $chart_version and appVersion $version; they must be equal"
[ -s "$note" ] || fail "version $version has no release note; write $note"
grep -q "v$version.md" docs/release-notes/README.md || fail "$note is not listed in docs/release-notes/README.md"
if [ -n "${1:-}" ] && [ "$1" != "v$version" ]; then
  fail "the tag is $1 but $chart says v$version"
fi
echo "$note"
