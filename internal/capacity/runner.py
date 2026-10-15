# Runs in the capacity Job, in an image with AIPerf. It makes up coding-agent
# sessions, replays them against one instance at a growing load, and prints
# one line per step that starts with "AIGW-". The server reads those lines
# from the pod's log and nothing else.
#
# The load doubles from the first step until a step misses the targets, then
# the gap between the last step that met them and the first that did not is
# halved a few times. A drained measurement first waits until the instance
# has no request in flight, and a step in which it counted prompt tokens the
# measurement did not send fails: something else still reaches it.
import glob
import http.client
import http.server
import json
import os
import re
import socket
import ssl
import subprocess
import sys
import threading
import time
import urllib.request

cfg = json.loads(os.environ["AIGW_CAPACITY"])
url = cfg["url"]
settings = cfg["settings"]
mode = settings.get("mode", "")
# The pods whose counters the measurement reads: the instance it measures,
# or every decode pod of a prefill/decode deployment.
metrics_urls = cfg.get("metrics_urls") or [url]
work = os.environ.get("AIGW_WORK", "/tmp")

MAX_FOREIGN_SHARE = 0.05


def emit(kind, obj):
    print("AIGW-" + kind + " " + json.dumps(obj, separators=(",", ":")), flush=True)


def tail(path, limit=1500):
    try:
        with open(path, errors="replace") as f:
            text = f.read()
    except OSError:
        return ""
    return text[-limit:].strip()


def peer_proxy(peer):
    """Serves plain HTTP on 127.0.0.1 and sends each request on to the
    gateway's peer listener with the site's client certificate: AIPerf has no
    option for one. Answers are passed on as they arrive, so streaming and
    the time to the first token are kept."""
    host, port = peer["address"].rsplit(":", 1)
    context = ssl.create_default_context(cafile=peer["ca"])
    context.load_cert_chain(peer["cert"], peer["key"])

    class Upstream(http.client.HTTPSConnection):
        def connect(self):
            sock = socket.create_connection((host, int(port)), self.timeout)
            self.sock = context.wrap_socket(sock, server_hostname=peer["sni"])

    class Handler(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def forward(self):
            n = int(self.headers.get("Content-Length") or 0)
            body = self.rfile.read(n) if n else None
            headers = {k: v for k, v in self.headers.items()
                       if k.lower() not in ("host", "connection", "content-length", "transfer-encoding")}
            headers["Host"] = peer["sni"]
            up = Upstream(host, int(port), timeout=cfg["grace_seconds"] + 600)
            try:
                up.request(self.command, self.path, body=body, headers=headers)
                resp = up.getresponse()
                self.send_response(resp.status)
                for k, v in resp.getheaders():
                    if k.lower() not in ("transfer-encoding", "connection", "content-length"):
                        self.send_header(k, v)
                self.send_header("Transfer-Encoding", "chunked")
                self.end_headers()
                while True:
                    chunk = resp.read1(65536)
                    if not chunk:
                        break
                    self.wfile.write(b"%x\r\n%s\r\n" % (len(chunk), chunk))
                    self.wfile.flush()
                self.wfile.write(b"0\r\n\r\n")
            except (OSError, http.client.HTTPException) as e:
                self.send_error(502, str(e)[:200])
            finally:
                up.close()

        do_GET = do_POST = forward

        def log_message(self, *args):
            pass

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    server.daemon_threads = True
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return "http://127.0.0.1:%d" % server.server_address[1]


def metrics():
    """The vLLM metrics of the pods measured, each summed over its labels and
    over the pods. None when one of them cannot be read."""
    found = {}
    for base in metrics_urls:
        try:
            text = urllib.request.urlopen(base + "/metrics", timeout=15).read().decode()
        except Exception:
            return None
        for line in text.splitlines():
            m = re.match(r"^(vllm:[a-z_]+)(?:\{[^}]*\})?\s+([0-9.eE+-]+)$", line)
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


def growth(before, after, name):
    if before is None or after is None or name not in before or name not in after:
        return None
    return max(after[name] - before[name], 0.0)


def cache_growth(before, after):
    """What the instance's prompt and cached-prompt counters grew by."""
    for prompt, cached in CACHE_COUNTERS:
        p, c = growth(before, after, prompt), growth(before, after, cached)
        if p is not None and c is not None:
            return p, c
    return growth(before, after, "vllm:prompt_tokens_total"), None


def stat(summary, name, key="avg"):
    value = (summary.get(name) or {}).get(key)
    return float(value) if value is not None else 0.0


def run(cmd, log):
    with open(log, "w") as out:
        return subprocess.run(cmd, stdout=out, stderr=subprocess.STDOUT).returncode


if cfg.get("peer"):
    url = peer_proxy(cfg["peer"])


def model_length():
    """The longest request the instance takes, prompt and answer together, as
    vLLM lists it under /v1/models. None when it does not say."""
    try:
        listed = json.loads(urllib.request.urlopen(url + "/v1/models", timeout=15).read())
        for m in listed.get("data", []):
            if m.get("id") == cfg["model"] and isinstance(m.get("max_model_len"), int):
                return m["max_model_len"]
    except Exception:
        pass
    return None


# A request longer than the model takes is refused, so every step would only
# count errors. The deployment may not declare its length to the hub.
longest = model_length()
need = settings["max_prompt_tokens"] + settings["max_output_tokens"]
if longest is not None and need > longest:
    emit("ERROR", {"error": "the model takes at most {:,} tokens, prompt and answer together, and the settings ask for up to {:,}: "
                            "set the longest prompt to at most {:,}".format(longest, need, max(longest - settings["max_output_tokens"], 0))})
    sys.exit(1)

# A drained instance still finishes what it had; anything after that is
# other traffic, and would be counted as the instance's own.
if mode == "drained":
    waited, wait = 0, cfg.get("wait_idle_seconds", 600)
    while True:
        m = metrics()
        running = None if m is None else m.get("vllm:num_requests_running")
        if running is not None and running <= 0:
            emit("IDLE", {"waited_s": waited})
            break
        if waited >= wait:
            emit("ERROR", {"error": "after {} minutes the instance still has {} requests in flight: something still sends to it, "
                                    "such as a client of the site's own route".format(wait // 60, "an unknown number of" if running is None else int(running))})
            sys.exit(1)
        time.sleep(5)
        waited += 5

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


def measure(concurrency):
    """Runs one step, prints it, and says whether it met the targets."""
    out = work + "/out/c%d" % concurrency
    before, started = metrics(), time.monotonic()
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
    after, seconds = metrics(), time.monotonic() - started
    found = glob.glob(out + "/**/profile_export_aiperf.json", recursive=True)
    if code != 0 or not found:
        emit("STEP", {"concurrency": concurrency, "error": "AIPerf failed: " + tail(out + ".log")})
        return False
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
        "server_output_tokens": growth(before, after, "vllm:generation_tokens_total"),
        "server_seconds": seconds,
    }
    emit("STEP", step)
    total = step["requests"] + step["errors"]
    foreign = 0.0
    if prompt_tokens:
        foreign = max(1 - step["input_tokens"] / prompt_tokens, 0.0)
    return not (step["requests"] == 0 or step["errors"] > settings.get("max_error_share", 0.01) * total
                or step["ttft_p95_ms"] > settings["ttft_p95_ms"] or step["itl_p95_ms"] > settings["itl_p95_ms"]
                or (mode == "drained" and foreign > MAX_FOREIGN_SHARE))


# Up until it breaks, then halve the gap.
passed, missed = None, None
concurrency = settings["first_concurrency"]
while True:
    if measure(concurrency):
        passed = concurrency
        if concurrency >= settings["max_concurrency"]:
            break
        concurrency = min(concurrency * 2, settings["max_concurrency"])
    else:
        missed = concurrency
        break
for _ in range(settings.get("bisect_steps", 0)):
    if passed is None or missed is None or missed - passed < 2:
        break
    middle = (passed + missed) // 2
    if measure(middle):
        passed = middle
    else:
        missed = middle

emit("DONE", {})
