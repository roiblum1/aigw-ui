#!/usr/bin/env python3
"""Fill an aigw-ui instance with demo data for the budget planner: two priced
models on two sites, a measurement of each LLMInferenceService, tenants with
budgets, and 20 days of spend. The Capacity page then shows the fleet line,
and demo-glm is marked saturated at its busiest hour.

The figures are those of internal/budget/budget_test.go: demo-glm serves
2,180 credits a second ($0.0218), demo-qwen 616.

Measurements, deployments and spend have no API, so it writes them with psql.
Everything it creates is named demo-*; a fresh database is the way to start
again.

  hack/seed-planner-demo.py --url http://127.0.0.1:8080 --token <admin token> \\
      --psql 'podman exec -i aigw-ui-pg psql -U postgres -d aigw'
"""
import argparse, datetime, json, shlex, subprocess, urllib.error, urllib.request, uuid

p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
p.add_argument("--url", required=True, help="base URL of aigw-ui")
p.add_argument("--token", required=True, help="admin token")
p.add_argument("--psql", required=True, help="command that runs psql against the server's database; SQL comes on stdin")
args = p.parse_args()


def call(method, path, body=None):
    req = urllib.request.Request(args.url.rstrip("/") + "/api/v1" + path, method=method,
                                 data=json.dumps(body).encode() if body is not None else None,
                                 headers={"Authorization": "Bearer " + args.token, "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req) as res:
            data = res.read()
            return json.loads(data) if data else None
    except urllib.error.HTTPError as e:
        raise SystemExit(f"{method} {path}: {e.code} {e.read().decode()}")


def sql(text):
    res = subprocess.run(shlex.split(args.psql) + ["-qAt", "-v", "ON_ERROR_STOP=1"], input=text, capture_output=True, text=True)
    if res.returncode != 0:
        raise SystemExit("psql: " + res.stderr.strip())
    return res.stdout.strip()


def q(v):
    return "'" + str(v).replace("'", "''") + "'"


# Clusters that point nowhere: discovery and syncs fail, which is fine here.
KUBECONFIG = ("apiVersion: v1\nkind: Config\nclusters:\n- name: x\n  cluster: {server: 'https://127.0.0.1:1'}\n"
              "contexts:\n- name: x\n  context: {cluster: x, user: x}\ncurrent-context: x\nusers:\n- name: x\n  user: {token: x}\n")
sites = {name: call("POST", "/clusters", {"name": name, "namespace": "envoy-gateway-system", "gateway_name": "envoy-ai-gateway",
                                          "kubeconfig": KUBECONFIG})["id"] for name in ("demo-site-a", "demo-site-b")}

# name -> prices in dollars for a million tokens (input, cached, output)
MODELS = {"demo-glm": (2.8, 0.56, 8.8), "demo-qwen": (0.3, 0.03, 1.0)}
models = {}
for name, (i, c, o) in MODELS.items():
    models[name] = call("POST", "/models", {"name": name, "default_limit": 1, "default_window": "1d"})["id"]
    call("PUT", f"/models/{models[name]}/prices", {"input_usd": i, "cached_usd": c, "output_usd": o, "now": True, "note": "demo"})

# model -> (site, deployment, ready instances, input tokens a second, cached share, output tokens a second)
LLMIS = [
    ("demo-glm", "demo-site-a", "glm-b300", 2, 10000, 0.9, 100),
    ("demo-glm", "demo-site-b", "glm-h200", 1, 5000, 0.9, 50),
    ("demo-qwen", "demo-site-a", "qwen-b200", 4, 20000, 0.9, 400),
]
profile_id, version = sql("SELECT id, version FROM benchmark_profiles WHERE active").split("|")
now = datetime.datetime.now(datetime.timezone.utc)
statements = []
for model, site, name, ready, inp, share, out in LLMIS:
    spec = uuid.uuid4().hex * 2
    # A backend on the cluster is what a tenant's quota attaches to.
    backends = json.dumps([{"name": name, "namespace": "llm", "model": model}])
    statements.append(f"INSERT INTO model_endpoints (model_id, cluster_id, upstream_model, source, backends, serving) "
                      f"VALUES ({q(models[model])}, {q(sites[site])}, '', 'discovered', {q(backends)}, true) ON CONFLICT DO NOTHING;")
    statements.append(f"INSERT INTO model_deployments (cluster_id, namespace, name, model_id, topology, gpu, gpus_per_instance, instances, spec_hash) "
                      f"VALUES ({q(sites[site])}, 'llm', {q(name)}, {q(models[model])}, 'single', 'NVIDIA-B300', 8, {ready}, {q(spec)});")
    seconds = 300
    step = {"concurrency": 16, "requests": 120, "errors": 0, "duration_s": seconds, "input_tokens": inp * seconds,
            "output_tokens": out * seconds, "isl_mean": 100000, "osl_mean": 1000, "ttft_p50_ms": 1500, "ttft_p95_ms": 3000,
            "itl_p50_ms": 20, "itl_p95_ms": 35, "server_prompt_tokens": inp * seconds, "server_cached_tokens": inp * seconds * share}
    settings = {"first_concurrency": 1, "bisect_steps": 2, "max_concurrency": 1024, "min_requests": 60, "step_seconds": 300, "sessions": 200,
                "max_prompt_tokens": 167000, "max_output_tokens": 1500, "ttft_p95_ms": 5000, "itl_p95_ms": 50, "mode": "drained"}
    statements.append(
        "INSERT INTO capacity_runs (model_id, cluster_id, namespace, deployment, instance, topology, gpu, gpus_per_instance, revision, job_name, "
        "settings, status, detail, steps, created_at, finished_at, spec_hash, profile_id, profile_version, phase, started_by) VALUES "
        f"({q(models[model])}, {q(sites[site])}, 'llm', {q(name)}, {q(name + '-0')}, 'single', 'NVIDIA-B300', 8, '', 'aigw-capacity-demo', "
        f"{q(json.dumps(settings))}, 'succeeded', 'demo', {q(json.dumps([step]))}, {q(now.isoformat())}, {q(now.isoformat())}, "
        f"{q(spec)}, {q(profile_id)}, {version}, '', 'operator');")
sql("\n".join(statements))

# Tenants and their daily budgets, in dollars.
BUDGETS = {"demo-team-a": {"demo-glm": 300, "demo-qwen": 40}, "demo-team-b": {"demo-glm": 150}, "demo-research": {"demo-qwen": 60}}
tenants = {}
for slug, budgets in BUDGETS.items():
    tenants[slug] = call("POST", "/tenants", {"slug": slug, "display_name": slug})["id"]
    for model, usd in budgets.items():
        call("PUT", f"/tenants/{tenants[slug]}/quotas", {"model_id": models[model], "token_limit": usd * 100000, "window": "1d"})

# 20 days of spend, in credits. demo-glm's busiest hour, $70 at 15:00
# yesterday, is 89% of what it serves in an hour ($78.48): saturated.
rows = []
today = now.replace(hour=0, minute=0, second=0, microsecond=0)
for d in range(1, 21):
    day = today - datetime.timedelta(days=d)
    for hour, glm, qwen in ((10, 2_000_000, 300_000), (15, 7_000_000 if d == 1 else 3_000_000, 1_000_000)):
        at = (day + datetime.timedelta(hours=hour)).isoformat()
        rows.append(f"({q(tenants['demo-team-a'])}, {q(models['demo-glm'])}, {q(at)}, 'credits', {glm}, 0)")
        rows.append(f"({q(tenants['demo-research'])}, {q(models['demo-qwen'])}, {q(at)}, 'credits', {qwen}, 0)")
sql("INSERT INTO usage_hours (tenant_id, model_id, hour, unit, used, best_effort) VALUES " + ",\n".join(rows) + ";")
print("seeded: open the Capacity page and the Overview")
