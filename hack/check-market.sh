#!/usr/bin/env bash
# Fails when the embedded market snapshot is older than MAX_DAYS (60): a
# release must not ship prices the market left behind long ago. Run
# hack/fetch-market.sh to refresh it.
#
#   hack/check-market.sh            # prints the snapshot's date and age
set -euo pipefail

cd "$(dirname "$0")/.."
python3 - "${MAX_DAYS:-60}" <<'PY'
import datetime, json, sys
max_days = int(sys.argv[1])
date = json.load(open("internal/market/openrouter.json"))["snapshot_date"]
age = (datetime.datetime.now(datetime.timezone.utc).date() - datetime.date.fromisoformat(date)).days
if age > max_days:
    sys.exit(f"market check: the snapshot of {date} is {age} days old, more than {max_days}; run hack/fetch-market.sh")
print(f"market snapshot {date}, {age} days old")
PY
