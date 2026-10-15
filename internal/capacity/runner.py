# Runs in the capacity Job, in an image with AIPerf. It makes up coding-agent
# sessions, replays them against one instance at a growing load, and prints
# one line per step that starts with "AIGW-". The server reads those lines
# from the pod's log and nothing else.
#
# The load doubles from the first step until a step misses the targets, then
# the gap between the last step that met them and the first that did not is
# halved a few times. A drained measurement first waits until the instance
# has no request in flight, and before each step watches its prompt counter
# for a quiet spell while it sends nothing: whatever the instance counts then
# is someone else's.
#
# The instance's counters are read every few seconds during a step and taken
# over the time AIPerf had requests out, not over AIPerf's whole run with its
# start and its exit.
import datetime
import glob
import http.client
import http.server
import json
import multiprocessing
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
# How long a drained step first waits for the instance to have nothing in
# flight, and then watches its counters while nothing is sent.
SETTLE_SECONDS = cfg.get("settle_seconds", 60)
QUIET_SECONDS = cfg.get("quiet_seconds", 20)
# How often the counters are read during a step.
SAMPLE_SECONDS = 2
# How many processes pass a prefill/decode measurement on to the peer
# listener.
PROXY_PROCESSES = cfg.get("proxy_processes", 8)


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
    the time to the first token are kept. Connections to the gateway are
    kept and used again, from one request to the next and from one step to
    the next, and opened before a step starts: opening hundreds at once,
    with a TLS handshake each, would add to the time to the first token of
    the step's first requests.

    Returns the proxy's URL, and a function that opens enough connections
    for a step at a load and returns once they are open."""
    host, port = peer["address"].rsplit(":", 1)
    context = ssl.create_default_context(cafile=peer["ca"])
    context.load_cert_chain(peer["cert"], peer["key"])

    class Upstream(http.client.HTTPSConnection):
        def connect(self):
            sock = socket.create_connection((host, int(port)), self.timeout)
            sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
            self.sock = context.wrap_socket(sock, server_hostname=peer["sni"])

    # The connections to the gateway not in use, in this process.
    idle, idle_lock = [], threading.Lock()

    class Handler(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"
        # The headers and each token go out as small writes: without this,
        # each would wait for AIPerf's delayed acknowledgement of the last.
        disable_nagle_algorithm = True
        up = None

        def send(self, body, headers):
            """Sends the request on, on a connection kept from before if the
            gateway has kept it open, else on a new one. Only a connection
            the gateway closed is tried again: the request never reached
            the model."""
            if self.up is None:
                with idle_lock:
                    self.up = idle.pop() if idle else None
            for fresh in (self.up is None, True):
                if fresh:
                    if self.up is not None:
                        self.up.close()
                    self.up = Upstream(host, int(port), timeout=cfg["grace_seconds"] + 600)
                try:
                    self.up.request(self.command, self.path, body=body, headers=headers)
                    return self.up.getresponse()
                except ConnectionError:
                    if fresh:
                        raise

        def forward(self):
            n = int(self.headers.get("Content-Length") or 0)
            body = self.rfile.read(n) if n else None
            headers = {k: v for k, v in self.headers.items()
                       if k.lower() not in ("host", "connection", "content-length", "transfer-encoding")}
            headers["Host"] = peer["sni"]
            try:
                resp = self.send(body, headers)
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
                self.wfile.write(b"0\r\n\r\n")
                if resp.will_close:
                    self.up.close()
                    self.up = None
            except (OSError, http.client.HTTPException) as e:
                # A connection left half read is never used again.
                if self.up is not None:
                    self.up.close()
                    self.up = None
                try:
                    self.send_error(502, str(e)[:200])
                except OSError:
                    self.close_connection = True  # AIPerf left

        do_GET = do_POST = forward

        def finish(self):
            if self.up is not None:
                with idle_lock:
                    idle.append(self.up)
                self.up = None
            super().finish()

        def log_message(self, *args):
            pass

    class Server(http.server.ThreadingHTTPServer):
        daemon_threads = True
        # AIPerf opens a step's connections all at once. With the default
        # backlog of 5 most of them would be dropped and retried a second
        # or more later, which the time to the first token would count.
        request_queue_size = 4096

    # One process holds Python's lock for all its threads; at a few hundred
    # streams that, not the model, would set the time between tokens. The
    # processes share the socket, and the kernel hands each connection to
    # one of them, about evenly: each keeps a quarter more than its share.
    server = Server(("127.0.0.1", 0), Handler)
    fork = multiprocessing.get_context("fork")
    want = fork.Value("i", 0, lock=False)
    have = fork.Array("i", PROXY_PROCESSES, lock=False)

    def keep_warm(index):
        while True:
            share = -(-want.value * 5 // (4 * PROXY_PROCESSES))
            with idle_lock:
                have[index] = len(idle)
            if have[index] >= share:
                time.sleep(0.2)
                continue
            up = Upstream(host, int(port), timeout=cfg["grace_seconds"] + 600)
            try:
                up.connect()
            except OSError:
                time.sleep(1)
                continue
            with idle_lock:
                idle.append(up)

    def serve(index):
        threading.Thread(target=keep_warm, args=(index,), daemon=True).start()
        server.serve_forever()

    for i in range(PROXY_PROCESSES):
        fork.Process(target=serve, args=(i,), daemon=True).start()

    def warm(concurrency, wait=60):
        want.value = concurrency
        deadline = time.monotonic() + wait
        while sum(have) < concurrency and time.monotonic() < deadline:
            time.sleep(0.2)

    return "http://127.0.0.1:%d" % server.server_address[1], warm


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


class Sampler:
    """Reads the counters at the start of a step, every few seconds during
    it, and at its end, and gives them at any time in between."""

    def __init__(self):
        self.samples, self.done = [], threading.Event()

    def take(self):
        m = metrics()
        if m is not None:
            self.samples.append((time.time(), m))

    def loop(self):
        while not self.done.wait(SAMPLE_SECONDS):
            self.take()

    def start(self):
        self.take()
        self.thread = threading.Thread(target=self.loop, daemon=True)
        self.thread.start()

    def stop(self):
        self.done.set()
        self.thread.join()
        self.take()

    def at(self, t):
        """The counters at wall time t, straight between two readings."""
        for (ta, a), (tb, b) in zip(self.samples, self.samples[1:]):
            if ta <= t <= tb:
                f = (t - ta) / (tb - ta) if tb > ta else 0.0
                return {k: v + (b.get(k, v) - v) * f for k, v in a.items()}
        return self.samples[-1][1]

    def window(self, t0, t1):
        """The counters at the start and the end of [t0, t1], and its length
        in seconds; the whole step when AIPerf does not say."""
        if len(self.samples) < 2:
            return None, None, 0.0
        first, last = self.samples[0][0], self.samples[-1][0]
        if not t0 or not t1 or t1 <= t0:
            t0, t1 = first, last
        t0, t1 = max(t0, first), min(t1, last)
        return self.at(t0), self.at(t1), max(t1 - t0, 0.0)


def quiet_spell():
    """Drained, before a step: waits until the instance has nothing in
    flight, then watches its prompt counter while nothing is sent. Returns
    the seconds watched and the prompt tokens counted meanwhile, or why the
    instance never ran dry."""
    waited = 0
    while True:
        m = metrics()
        running = None if m is None else m.get("vllm:num_requests_running")
        if running is not None and running <= 0:
            break
        if waited >= SETTLE_SECONDS:
            return "after {} seconds with nothing sent the instance still had {} requests in flight: something else sends to it, " \
                   "such as a client of the site's own route".format(waited, "an unknown number of" if running is None else int(running))
        time.sleep(1)
        waited += 1
    before, started = metrics(), time.monotonic()
    time.sleep(QUIET_SECONDS)
    after = metrics()
    return time.monotonic() - started, growth(before, after, "vllm:prompt_tokens_total")


def sending(summary):
    """When AIPerf had requests out, in seconds since the epoch: its first
    request to its last answer when it says so, else the start and end of
    its profiling, a few seconds wider; (None, None) without either."""
    t0, t1 = stat(summary, "min_request_timestamp") / 1e9, stat(summary, "max_response_timestamp") / 1e9
    if t0 and t1:
        return t0, t1
    try:
        # Written in this container's local time.
        return (datetime.datetime.fromisoformat(summary["start_time"]).timestamp(),
                datetime.datetime.fromisoformat(summary["end_time"]).timestamp())
    except (KeyError, TypeError, ValueError):
        return None, None


def stat(summary, name, key="avg"):
    value = (summary.get(name) or {}).get(key)
    return float(value) if value is not None else 0.0


def run(cmd, log):
    with open(log, "w") as out:
        return subprocess.run(cmd, stdout=out, stderr=subprocess.STDOUT).returncode


warm_proxy = None
if cfg.get("peer"):
    url, warm_proxy = peer_proxy(cfg["peer"])


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
    quiet_s, quiet_tokens = None, None
    if mode == "drained":
        quiet = quiet_spell()
        if isinstance(quiet, str):
            emit("STEP", {"concurrency": concurrency, "error": quiet})
            return False
        quiet_s, quiet_tokens = quiet
    if warm_proxy:
        warm_proxy(concurrency)
    sampler = Sampler()
    sampler.start()
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
    sampler.stop()
    found = glob.glob(out + "/**/profile_export_aiperf.json", recursive=True)
    if code != 0 or not found:
        emit("STEP", {"concurrency": concurrency, "error": "AIPerf failed: " + tail(out + ".log")})
        return False
    with open(found[0]) as f:
        s = json.load(f)
    before, after, seconds = sampler.window(*sending(s))
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
    if quiet_s is not None:
        step["quiet_s"], step["quiet_prompt_tokens"] = quiet_s, quiet_tokens
    emit("STEP", step)
    total = step["requests"] + step["errors"]
    return not (step["requests"] == 0 or step["errors"] > settings.get("max_error_share", 0.01) * total
                or step["ttft_p95_ms"] > settings["ttft_p95_ms"] or step["itl_p95_ms"] > settings["itl_p95_ms"]
                or (mode == "drained" and foreign_share(step) > MAX_FOREIGN_SHARE))


def foreign_share(step):
    """The part of the prompt tokens the instance counted that something else
    sent: Step.ForeignShare in the server, which decides; this only steers
    the sweep."""
    counted = step["server_prompt_tokens"]
    if not counted:
        return 0.0
    own = step["input_tokens"] + step["errors"] * settings["max_prompt_tokens"]
    share = (counted - own) / counted
    if step.get("quiet_prompt_tokens") is not None and step.get("quiet_s") and step["server_seconds"]:
        share = max(share, step["quiet_prompt_tokens"] / step["quiet_s"] * step["server_seconds"] / counted)
    return min(max(share, 0.0), 1.0)


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
