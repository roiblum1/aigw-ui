#!/usr/bin/env python3
"""Fill an aigw-ui instance with demo data so the Usage page and the Overview
charts have something to show without a real gateway.

It creates a cluster entry that points nowhere, two models, a few tenants with
quotas, and then writes made-up usage into Redis the way a gateway's rate limit
service would, updating it for a few minutes so the live chart moves.

Everything it creates is named demo-*. Remove it again with --clean.

  hack/seed-demo.py --url https://aigw-ui.example --token <admin token> \\
      --redis-cli 'oc -n aigw-ui exec deploy/redis -- sh -c' --minutes 10

--redis-cli is a command that runs its last argument as a shell line where
redis-cli can reach Redis. The script appends the redis-cli call to it.
"""
import argparse, json, random, shlex, ssl, subprocess, time, urllib.error, urllib.request

p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
p.add_argument("--url", required=True, help="base URL of aigw-ui")
p.add_argument("--token", required=True, help="admin token")
p.add_argument("--redis-cli", required=True, help="command prefix that runs a shell line next to Redis")
p.add_argument("--redis-args", default='--tls --insecure --no-auth-warning -a "$REDIS_PASSWORD"',
               help="arguments for redis-cli inside that shell")
p.add_argument("--minutes", type=float, default=5, help="how long to keep the usage moving")
p.add_argument("--insecure", action="store_true", help="do not verify the aigw-ui certificate")
p.add_argument("--clean", action="store_true", help="remove the demo data instead of creating it")
args = p.parse_args()

ctx = ssl._create_unverified_context() if args.insecure else ssl.create_default_context()

def call(method, path, body=None):
    req = urllib.request.Request(args.url.rstrip("/") + "/api/v1" + path, method=method,
                                 data=json.dumps(body).encode() if body is not None else None,
                                 headers={"Authorization": "Bearer " + args.token, "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, context=ctx) as res:
            data = res.read()
            return json.loads(data) if data else None
    except urllib.error.HTTPError as e:
        raise SystemExit(f"{method} {path}: {e.code} {e.read().decode()}")

def redis_set(values):
    """Writes all counters in one remote call; each expires after a day like a real one."""
    line = "; ".join("redis-cli " + args.redis_args + " SET " + shlex.quote(k) + " " + str(v) + " EX 86400 >/dev/null"
                     for k, v in values.items())
    res = subprocess.run(shlex.split(args.redis_cli) + [line], capture_output=True, text=True)
    if res.returncode != 0:
        raise SystemExit("writing to Redis failed: " + res.stderr.strip())

CLUSTER = "demo-site1"
MODELS = {"demo-glm": 2_500_000, "demo-qwen-coder": 3_000_000}  # name -> shared pool per day
# model -> tenant -> (limit, window, dry run, share of the limit already used)
PLAN = {
    "demo-glm": {
        "demo-team-a": (1_000_000, "1d", False, 0.41), "demo-team-b": (50_000, "1h", False, 0.88),
        "demo-team-c": (2_000, "1m", True, 1.3), "demo-research": (800_000, "1d", False, 0.72),
        "demo-search": (400_000, "1d", False, 0.0),
    },
    "demo-qwen-coder": {
        "demo-platform": (1_500_000, "1d", False, 0.55), "demo-research": (600_000, "1d", False, 0.74),
        "demo-team-a": (300_000, "1d", True, 0.2),
    },
}
SECONDS = {"1d": 86400, "1h": 3600, "1m": 60}

if args.clean:
    for t in call("GET", "/tenants"):
        if t["slug"].startswith("demo-"):
            call("DELETE", "/tenants/" + t["id"])
    for m in call("GET", "/models"):
        if m["name"].startswith("demo-"):
            call("DELETE", "/models/" + m["id"])
    for c in call("GET", "/clusters"):
        if c["name"].startswith("demo-"):
            call("DELETE", "/clusters/" + c["id"])
    print("demo data removed; its counters in Redis expire with their windows")
    raise SystemExit

# A cluster that cannot be reached: syncs fail, which is fine for a demo of usage.
kubeconfig = ('apiVersion: v1\nkind: Config\nclusters: [{name: c, cluster: {server: "https://127.0.0.1:1"}}]\n'
              'users: [{name: u, user: {token: x}}]\ncontexts: [{name: x, context: {cluster: c, user: u}}]\ncurrent-context: x\n')
clusters = {c["name"]: c for c in call("GET", "/clusters")}
if CLUSTER not in clusters:
    call("POST", "/clusters", {"name": CLUSTER, "site": "demo", "namespace": "ai-gateway", "gateway_name": "llm", "kubeconfig": kubeconfig})
cluster_id = {c["name"]: c for c in call("GET", "/clusters")}[CLUSTER]["id"]

models = {m["name"]: m for m in call("GET", "/models")}
for name, pool in MODELS.items():
    if name not in models:
        call("POST", "/models", {"name": name, "default_limit": pool, "default_window": "1d",
                                 "endpoints": [{"cluster_id": cluster_id, "host": name + ".svc", "port": 8000}]})
models = {m["name"]: m for m in call("GET", "/models")}

tenants = {t["slug"]: t for t in call("GET", "/tenants")}
for slug in sorted({s for quotas in PLAN.values() for s in quotas}):
    if slug not in tenants:
        call("POST", "/tenants", {"slug": slug, "display_name": slug.removeprefix("demo-")})
tenants = {t["slug"]: t for t in call("GET", "/tenants")}
for model, quotas in PLAN.items():
    for slug, (limit, window, shadow, _) in quotas.items():
        call("PUT", f"/tenants/{tenants[slug]['id']}/quotas",
             {"model_id": models[model]["id"], "token_limit": limit, "window": window, "shadow": shadow})

def keys(now):
    """The Redis key of every demo quota and pool for the window that holds now."""
    out = {}
    for model, quotas in PLAN.items():
        m = models[model]
        base = f"ai-gateway-quota_backend_name_ai-gateway/{m['slug']}_model_name_override_{m['name']}_"
        slugs = sorted(quotas)  # bucket rules are ordered by tenant slug
        for i, slug in enumerate(slugs):
            rule = f"rule-{i}-x-aigw-client-id|^{slug}\\.[a-f0-9]+$-match-0"
            start = now // SECONDS[quotas[slug][1]] * SECONDS[quotas[slug][1]]
            out[(model, slug)] = f"{base}{rule}_{rule}_{start}"
        pool = f"rule-{len(slugs)}-match--1"
        out[(model, None)] = f"{base}{pool}_{pool}_{now // 86400 * 86400}"
    return out

used = {(model, slug): int(q[0] * q[3]) for model, quotas in PLAN.items() for slug, q in quotas.items()}
for model in PLAN:
    used[(model, None)] = sum(v for (m, s), v in used.items() if m == model and s)

print(f"demo data is in place; moving usage for {args.minutes:g} minutes (Ctrl-C to stop)")
end = time.time() + args.minutes * 60
while True:
    values = {}
    for (model, slug), key in keys(int(time.time())).items():
        if slug and PLAN[model][slug][3] > 0 and not PLAN[model][slug][2]:
            step = random.randint(200, 2500)
            # A real gateway rejects a tenant at its limit, so the demo stops there too.
            step = min(step, PLAN[model][slug][0] - used[(model, slug)])
            used[(model, slug)] += step
            used[(model, None)] += step
        values[key] = used[(model, slug)]
    redis_set(values)
    if time.time() >= end:
        break
    time.sleep(3)
