#!/usr/bin/env bash
# Fails when the Go code, tests aside, has two blocks that are the same over
# 100 tokens or more: about 15 lines. Such a block belongs in one function.
#
#   hack/check-duplication.sh
set -euo pipefail

cd "$(dirname "$0")/.."
found=$(go run github.com/mibk/dupl@v1.1.0 -t 100 -plumbing cmd internal | grep -v '_test\.go' || true)
if [ -n "$found" ]; then
  echo "repeated code:"
  echo "$found"
  exit 1
fi
echo "no repeated code"
