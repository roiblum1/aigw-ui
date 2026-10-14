# Runs in the capacity Job, in an image with AIPerf. It makes up coding-agent
# sessions, replays them against one instance at each load step, and prints
# one line per step that starts with "AIGW-". The server reads those lines
# from the pod's log and nothing else.
import glob
import json
import os
import re
import subprocess
import sys

cfg = json.loads(os.environ["AIGW_CAPACITY"])
url = cfg["url"]
settings = cfg["settings"]
work = os.environ.get("AIGW_WORK", "/tmp")


def emit(kind, obj):
    print("AIGW-" + kind + " " + json.dumps(obj, separators=(",", ":")), flush=True)


def tail(path, limit=1500):
    try:
        with open(path, errors="replace") as f:
            text = f.read()
    except OSError:
        return ""
    return text[-limit:].strip()


def counters():
    """The instance's own token counters, summed over their labels."""
    import urllib.request

    try:
        text = urllib.request.urlopen(url + "/metrics", timeout=15).read().decode()
    except Exception:
        return None
    found = {}
    for line in text.splitlines():
        m = re.match(r"^(vllm:[a-z_]+_total)(?:\{[^}]*\})?\s+([0-9.eE+-]+)$", line)
        if m:
            found[m.group(1)] = found.get(m.group(1), 0.0) + float(m.group(2))
    return found


# Pairs of counters of prompt tokens and the part of them from the cache, in
# the order they are tried. A pair is used whole, so both figures count the
# same thing.
CACHE_COUNTERS = [
    ("vllm:prompt_tokens_total", "vllm:prompt_tokens_cached_total"),
    ("vllm:prefix_cache_queries_total", "vllm:prefix_cache_hits_total"),
]


def cache_growth(before, after):
    """What the instance's prompt and cached-prompt counters grew by."""
    if before is None or after is None:
        return None, None
    for prompt, cached in CACHE_COUNTERS:
        if all(name in before and name in after for name in (prompt, cached)):
            return max(after[prompt] - before[prompt], 0.0), max(after[cached] - before[cached], 0.0)
    return None, None


def stat(summary, name, key="avg"):
    value = (summary.get(name) or {}).get(key)
    return float(value) if value is not None else 0.0


def run(cmd, log):
    with open(log, "w") as out:
        return subprocess.run(cmd, stdout=out, stderr=subprocess.STDOUT).returncode


# The sessions. The same ones are replayed at every step, so the cache holds
# their shared parts after the first step, as it would on a busy day.
os.makedirs(work + "/ds", exist_ok=True)
with open(work + "/agentic.json", "w") as f:
    json.dump(cfg["agentic"], f)
code = run(["aiperf", "synthesize", "agentic-code", "--config", work + "/agentic.json",
            "--num-sessions", str(settings["sessions"]), "--seed", "42", "--output", work + "/ds"],
           work + "/synthesize.log")
datasets = glob.glob(work + "/ds/*/dataset.jsonl")
if code != 0 or not datasets:
    emit("ERROR", {"error": "making up the sessions failed: " + tail(work + "/synthesize.log")})
    sys.exit(1)
dataset = datasets[0]

# What a prompt grows to over a session: earlier input and answers included.
prompts, sessions = [], {}
with open(dataset) as f:
    for line in f:
        row = json.loads(line)
        total = sessions.get(row["session_id"], 0) + row.get("input_length", 0)
        prompts.append(total)
        sessions[row["session_id"]] = total + row.get("output_length", 0)
emit("DATASET", {"sessions": len(sessions), "requests": len(prompts),
                 "prompt_mean": sum(prompts) / max(len(prompts), 1), "prompt_max": max(prompts or [0])})

os.makedirs(work + "/out", exist_ok=True)
for concurrency in settings["concurrency"]:
    out = work + "/out/c%d" % concurrency
    before = counters()
    code = run(["aiperf", "profile",
                "--model", cfg["model"], "--url", url,
                "--endpoint-type", "chat", "--streaming",
                "--input-file", dataset, "--custom-dataset-type", "mooncake_trace",
                # The sessions' blocks: AIPerf otherwise reads them as 512 tokens.
                "--isl-block-size", str(cfg["agentic"]["block_size"]),
                "--no-fixed-schedule", "--concurrency", str(concurrency),
                # Whichever comes first: enough requests, or the longest a step may take.
                "--request-count", str(max(settings["min_requests"], 4 * concurrency)),
                "--benchmark-duration", str(settings["step_seconds"]),
                "--benchmark-grace-period", str(cfg["grace_seconds"]),
                "--request-timeout-seconds", str(cfg["grace_seconds"] + 600),
                "--tokenizer", "builtin", "--use-server-token-count",
                "--random-seed", "42", "--no-server-metrics",
                "--ui", "none", "--export-level", "summary", "--artifact-dir", out],
               out + ".log")
    after = counters()
    found = glob.glob(out + "/**/profile_export_aiperf.json", recursive=True)
    if code != 0 or not found:
        emit("STEP", {"concurrency": concurrency, "error": "AIPerf failed: " + tail(out + ".log")})
        break
    with open(found[0]) as f:
        s = json.load(f)
    prompt_tokens, cached_tokens = cache_growth(before, after)
    step = {
        "concurrency": concurrency,
        "requests": int(stat(s, "request_count")),
        "errors": int(stat(s, "error_request_count")),
        "duration_s": stat(s, "benchmark_duration"),
        "input_tokens": int(stat(s, "total_isl")),
        "output_tokens": int(stat(s, "total_osl")),
        "isl_mean": stat(s, "input_sequence_length"),
        "osl_mean": stat(s, "output_sequence_length"),
        "ttft_p50_ms": stat(s, "time_to_first_token", "p50"),
        "ttft_p95_ms": stat(s, "time_to_first_token", "p95"),
        "itl_p50_ms": stat(s, "inter_token_latency", "p50"),
        "itl_p95_ms": stat(s, "inter_token_latency", "p95"),
        "server_prompt_tokens": prompt_tokens,
        "server_cached_tokens": cached_tokens,
    }
    emit("STEP", step)
    # A higher load only gets slower: stop at the first step that misses.
    total = step["requests"] + step["errors"]
    if (step["requests"] == 0 or step["errors"] > 0.01 * total
            or step["ttft_p95_ms"] > settings["ttft_p95_ms"] or step["itl_p95_ms"] > settings["itl_p95_ms"]):
        break

emit("DONE", {})
