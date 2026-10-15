#!/usr/bin/env bash
# Writes internal/market/openrouter.json, the market snapshot the server
# embeds: the public OpenRouter catalog, in dollars per million tokens. It
# needs no key. Run it on a machine with internet access; the build itself
# never fetches anything.
#
#   hack/fetch-market.sh            # from https://openrouter.ai/api/v1/models
#   hack/fetch-market.sh FILE       # from a catalog saved earlier
#
# A price of -1 means variable and is left out. Long-context tiers under
# "overrides" are ignored: the base price is kept.
set -euo pipefail

cd "$(dirname "$0")/.."
out=internal/market/openrouter.json
src="${1:-}"
if [ -z "$src" ]; then
  src="$(mktemp)"
  trap 'rm -f "$src"' EXIT
  curl -fsS --max-time 60 https://openrouter.ai/api/v1/models -o "$src"
fi

python3 - "$src" "$out" <<'PY'
import datetime, decimal, json, sys

src, out = sys.argv[1], sys.argv[2]
data = json.load(open(src))["data"]

def per_million(value):
    """A price per token as OpenRouter writes it ("0.0000028") in dollars for
    a million tokens (2.8); None when absent, variable (-1) or not a number."""
    if value is None or value == "":
        return None
    try:
        d = decimal.Decimal(str(value))
    except decimal.InvalidOperation:
        return None
    if d < 0:
        return None
    return float((d * 1000000).normalize())

entries = []
for m in data:
    p = m.get("pricing") or {}
    prompt, completion = per_million(p.get("prompt")), per_million(p.get("completion"))
    if prompt is None or completion is None:
        continue
    entries.append({
        "id": m["id"],
        "hf_id": m.get("hugging_face_id") or "",
        "name": m.get("name") or m["id"],
        "context": m.get("context_length") or 0,
        "prompt": prompt,
        "completion": completion,
        "cache_read": per_million(p.get("input_cache_read")),
    })
entries.sort(key=lambda e: e["id"])
snapshot = {
    "source": "OpenRouter",
    "url": "https://openrouter.ai/api/v1/models",
    "snapshot_date": datetime.datetime.now(datetime.timezone.utc).date().isoformat(),
    "models": entries,
}
# One model per line, so that a weekly snapshot reads as a short diff.
with open(out, "w") as f:
    head = {k: v for k, v in snapshot.items() if k != "models"}
    f.write(json.dumps(head, ensure_ascii=False)[:-1] + ', "models": [\n')
    f.write(",\n".join(json.dumps(e, ensure_ascii=False) for e in entries))
    f.write("\n]}\n")
print(f"{out}: {len(entries)} models, snapshot {snapshot['snapshot_date']}")
PY
