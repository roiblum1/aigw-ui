package capacity

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeAIPerf stands in for the aiperf command. It writes a two-session
// dataset, and for a profile tells the stand-in instance what it served and
// writes a summary whose time to first token grows with the load: 1,000 ms
// per request in flight.
const fakeAIPerf = `#!/usr/bin/env python3
import json, os, sys, urllib.request
args = sys.argv[1:]
def arg(name):
    return args[args.index(name) + 1]
if args[0] == "synthesize":
    d = os.path.join(arg("--output"), "run")
    os.makedirs(d)
    with open(os.path.join(d, "dataset.jsonl"), "w") as f:
        for row in [{"session_id": "a", "input_length": 1000, "output_length": 100},
                    {"session_id": "a", "input_length": 200, "output_length": 100},
                    {"session_id": "b", "input_length": 1000, "output_length": 50}]:
            f.write(json.dumps(row) + "\n")
    sys.exit(0)
c = int(arg("--concurrency"))
if "--no-fixed-schedule" not in args or arg("--tokenizer") != "builtin" or not arg("--isl-block-size").isdigit() or int(arg("--request-count")) < 10:
    print("missing flags"); sys.exit(2)
d = os.path.join(arg("--artifact-dir"), "inner")
os.makedirs(d)
summary = {
    "request_count": {"avg": 10 * c}, "error_request_count": {"avg": 0},
    "benchmark_duration": {"avg": 100.0},
    "total_isl": {"avg": 100000 * c}, "total_osl": {"avg": 1000 * c},
    "input_sequence_length": {"avg": 10000}, "output_sequence_length": {"avg": 100},
    "time_to_first_token": {"p50": 500 * c, "p95": 1000 * c},
    "inter_token_latency": {"p50": 10, "p95": 20},
}
with open(os.path.join(d, "profile_export_aiperf.json"), "w") as f:
    json.dump(summary, f)
# Each --url is a rank of the instance: they share the load equally.
urls = [args[i + 1] for i, a in enumerate(args) if a == "--url"]
for u in urls:
    served = {"prompt": 100000 * c // len(urls), "cached": 80000 * c // len(urls), "output": 1000 * c // len(urls)}
    urllib.request.urlopen(u + "/served", data=json.dumps(served).encode())
`

// instance is what the stand-in for vLLM reports. Its counters grow by what
// the fake aiperf says it served, 80% of the prompt tokens cached.
type instance struct {
	maxModelLen int
	// foreignPerRead is what other clients add to its prompt counter between
	// two readings of /metrics: 0 for an instance that only serves the
	// measurement.
	foreignPerRead int64
	// running is its gauge of requests in flight, and busyAfterFirst what
	// it turns to once the first step has been served: other clients that
	// start to reach the instance during the measurement.
	running, busyAfterFirst int
	// ranks is how many API servers it runs, each on its own port with its
	// own counters, as a data-parallel pod does; 0 is one.
	ranks int
	// beforeVLLM16 publishes the counters of vLLM before 0.16: no
	// prompt_tokens_cached, the local prefix cache's hits and a KV
	// connector's (LMCache) as two counters over the same queried prompts.
	// Of the 80% cached, 50% comes from the local cache and 30% from the
	// connector.
	beforeVLLM16 bool
	// served is what each rank was sent, filled in by runRunner.
	served []*atomic.Int64
}

// runRunner runs the Job's script with the fake aiperf against a stand-in
// instance, and returns what it printed.
func runRunner(t *testing.T, set Settings, in *instance) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "aiperf"), []byte(fakeAIPerf), 0o755); err != nil {
		t.Fatal(err)
	}
	var urls []string
	for range max(in.ranks, 1) {
		var reads, prompt, cached, output atomic.Int64
		var busy atomic.Bool
		in.served = append(in.served, &prompt)
		mux := http.NewServeMux()
		mux.HandleFunc("/served", func(w http.ResponseWriter, r *http.Request) {
			var n struct{ Prompt, Cached, Output int64 }
			json.NewDecoder(r.Body).Decode(&n)
			prompt.Add(n.Prompt)
			cached.Add(n.Cached)
			output.Add(n.Output)
			busy.Store(true)
		})
		mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"object":"list","data":[{"id":"m","object":"model","max_model_len":%d}]}`, in.maxModelLen)
		})
		mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
			n := reads.Add(1)
			running := in.running
			if busy.Load() && in.busyAfterFirst > 0 {
				running = in.busyAfterFirst
			}
			p := prompt.Load() + n*in.foreignPerRead
			if in.beforeVLLM16 {
				local := cached.Load() * 5 / 8
				fmt.Fprintf(w, "vllm:prompt_tokens_total{engine=\"0\",model_name=\"m\"} %d\nvllm:prefix_cache_queries_total{engine=\"0\",model_name=\"m\"} %d\n"+
					"vllm:prefix_cache_hits_total{engine=\"0\",model_name=\"m\"} %d\nvllm:external_prefix_cache_queries_total{engine=\"0\",model_name=\"m\"} %d\n"+
					"vllm:external_prefix_cache_hits_total{engine=\"0\",model_name=\"m\"} %d\nvllm:generation_tokens_total{engine=\"0\"} %d\n"+
					"vllm:num_requests_running{engine=\"0\",model_name=\"m\"} %d\n", p, p, local, p, cached.Load()-local, output.Load(), running)
				return
			}
			fmt.Fprintf(w, "# HELP vllm:prompt_tokens_total x\nvllm:prompt_tokens_total{engine=\"0\",model_name=\"m\"} %d\n"+
				"vllm:prompt_tokens_cached_total{engine=\"0\",model_name=\"m\"} %d\nvllm:generation_tokens_total{engine=\"0\"} %d\n"+
				"vllm:num_requests_running{engine=\"0\",model_name=\"m\"} %d\n", p, cached.Load(), output.Load(), running)
		})
		server := httptest.NewServer(mux)
		defer server.Close()
		urls = append(urls, server.URL)
	}

	config, _ := json.Marshal(map[string]any{"urls": urls, "model": "m", "settings": set, "grace_seconds": 1,
		"agentic": Agentic(set), "wait_idle_seconds": 0, "settle_seconds": 0, "quiet_seconds": 0.2})
	cmd := exec.Command(python, "-c", runner)
	cmd.Env = append(os.Environ(), "AIGW_CAPACITY="+string(config), "AIGW_WORK="+filepath.Join(dir, "work"), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.Mkdir(filepath.Join(dir, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// The load doubles until a step misses, then the gap is halved: 1, 2 and 4,
// which misses, then 3.
func TestRunner(t *testing.T) {
	set := Defaults(0)
	set.TTFTMillis = 3000 // 1,000 ms per request in flight: 4 misses it, 3 just meets it
	out := runRunner(t, set, &instance{maxModelLen: 200000})

	l := ParseLog(out)
	if !l.Done || l.Error != "" {
		t.Fatalf("not done:\n%s", out)
	}
	// Session a sends 1,000 then 1,000 + 100 + 200; b sends 1,000.
	if l.Dataset == nil || l.Dataset.Sessions != 2 || l.Dataset.PromptMax != 1300 || l.Dataset.PromptMean != 1100 {
		t.Fatalf("dataset = %+v", l.Dataset)
	}
	var ran []int
	for _, s := range l.Steps {
		ran = append(ran, s.Concurrency)
	}
	if fmt.Sprint(ran) != "[1 2 4 3]" {
		t.Fatalf("steps = %v\n%s", ran, out)
	}
	s := l.Steps[1]
	if s.Requests != 20 || s.InputTokens != 200000 || s.TTFTP95 != 2000 || s.ITLP95 != 20 || s.ServerOutputTokens == nil || *s.ServerOutputTokens != 2000 || s.ServerSeconds <= 0 {
		t.Fatalf("step = %+v", s)
	}
	if share := s.CachedShare(); share == nil || *share != 0.8 {
		t.Fatalf("cached share = %v", share)
	}
	// Drained, each step first watched the instance while nothing was sent,
	// and nothing came.
	if s.QuietSeconds < 0.2 || s.QuietPromptTokens == nil || *s.QuietPromptTokens != 0 || *s.ForeignShare(set) != 0 {
		t.Fatalf("quiet spell = %v %v, foreign %v", s.QuietSeconds, s.QuietPromptTokens, *s.ForeignShare(set))
	}
	r := Analyze(set, l.Steps)
	if r.Best == nil || r.Best.Concurrency != 3 || !strings.Contains(r.Limit, "at 4 in flight") {
		t.Fatalf("result = %+v %q", r.Best, r.Limit)
	}
}

// A pod with an API server per data-parallel rank is measured whole: the
// load goes to every rank in turn, and the counters of all of them are added
// up, so the step reads as it would from one server.
func TestRunnerRanks(t *testing.T) {
	set := Defaults(0)
	set.TTFTMillis = 3000
	in := &instance{maxModelLen: 200000, ranks: 8}
	l := ParseLog(runRunner(t, set, in))
	if !l.Done || len(l.Steps) != 4 {
		t.Fatalf("not done: %+v", l)
	}
	if s := l.Steps[1]; s.ServerOutputTokens == nil || *s.ServerOutputTokens != 2000 {
		t.Fatalf("step = %+v", s)
	}
	for i, served := range in.served {
		if served.Load() == 0 {
			t.Fatalf("rank %d was sent nothing", i)
		}
	}
}

// Before vLLM 0.16 the cache share comes from the prefix cache's counters,
// and what a KV connector such as LMCache loaded counts as cached too: 50%
// from the local cache and 30% from the connector make 80%, as
// prompt_tokens_cached says from 0.16 on.
func TestRunnerCacheBeforeVLLM16(t *testing.T) {
	set := Defaults(0)
	set.TTFTMillis = 3000
	l := ParseLog(runRunner(t, set, &instance{maxModelLen: 200000, beforeVLLM16: true}))
	if !l.Done || len(l.Steps) != 4 {
		t.Fatalf("not done: %+v", l)
	}
	share := l.Steps[1].CachedShare()
	if share == nil {
		t.Fatal("no cached share")
	}
	if *share != 0.8 {
		t.Fatalf("cached share = %v, want 0.8", *share)
	}
}

// A model shorter than the settings would refuse every request: the Job stops
// before it makes up any session, and says what to set.
func TestRunnerModelTooShort(t *testing.T) {
	out := runRunner(t, Defaults(0), &instance{maxModelLen: 8192})
	l := ParseLog(out)
	if l.Dataset != nil || len(l.Steps) != 0 || l.Done {
		t.Fatalf("it went on:\n%s", out)
	}
	want := "the model takes at most 8,192 tokens, prompt and answer together, and the settings ask for up to 168,500: set the longest prompt to at most 6,692"
	if l.Error != want {
		t.Fatalf("error = %q", l.Error)
	}
}

// A stand-in that other clients still reach: its prompt counter grows by far
// more than the measurement sends. Drained, the quiet spell before the first
// step sees it, the step fails as contaminated and the sweep stops; live, the
// same traffic is expected and shown.
func TestRunnerForeignTraffic(t *testing.T) {
	side := &instance{maxModelLen: 200000, foreignPerRead: 10_000_000}
	set := Defaults(0)
	set.MaxConcurrency = 2
	l := ParseLog(runRunner(t, set, side))
	if len(l.Steps) != 1 || !l.Done {
		t.Fatalf("drained: %+v", l.Steps)
	}
	if s := l.Steps[0]; s.QuietPromptTokens == nil || *s.QuietPromptTokens != 10_000_000 {
		t.Fatalf("quiet spell: %+v", s)
	}
	if ok, why := l.Steps[0].Check(set); ok || !strings.Contains(why, "contaminated: ") {
		t.Fatalf("drained step: %v %q", ok, why)
	}

	set.Mode = ModeLive
	l = ParseLog(runRunner(t, set, side))
	if len(l.Steps) != 2 || l.Steps[0].QuietPromptTokens != nil {
		t.Fatalf("live: %+v", l.Steps)
	}
	r := Analyze(set, l.Steps)
	if r.Best == nil || r.Best.Concurrency != 2 || r.Steps[0].ForeignShare == nil || *r.Steps[0].ForeignShare < 0.98 {
		t.Fatalf("live result: %+v", r)
	}
	// Live, the throughput is the instance's own counters: other clients'
	// 10,000,000 prompt tokens a reading and the measurement's own, over
	// the time between the readings.
	if tp := set.Throughput(*r.Best); tp.InputPerSecond < 10_000_000/r.Best.ServerSeconds*0.99 {
		t.Fatalf("live throughput = %+v", tp)
	}
}

// Drained, an instance that other clients start to reach during the
// measurement fails the next step at its quiet spell: it never runs dry.
func TestRunnerTrafficArrives(t *testing.T) {
	l := ParseLog(runRunner(t, Defaults(0), &instance{maxModelLen: 200000, busyAfterFirst: 3}))
	if !l.Done || len(l.Steps) != 2 || l.Steps[0].Error != "" || !strings.Contains(l.Steps[1].Error, "still had 3 requests in flight") {
		t.Fatalf("steps: %+v", l.Steps)
	}
	if r := Analyze(Defaults(0), l.Steps); r.Best == nil || r.Best.Concurrency != 1 || !strings.Contains(r.Limit, "something else sends to it") {
		t.Fatalf("result: %+v %q", r.Best, r.Limit)
	}
}

// A drained instance that never runs dry is not measured.
func TestRunnerNotIdle(t *testing.T) {
	l := ParseLog(runRunner(t, Defaults(0), &instance{maxModelLen: 200000, running: 3}))
	if l.Dataset != nil || !strings.Contains(l.Error, "still has 3 requests in flight") {
		t.Fatalf("not idle: %+v", l)
	}
}

// When AIPerf had requests out: its first request to its last answer when it
// exports them, else its profiling's start and end (AIPerf 0.13 keeps the
// first two to itself), else nothing, and the whole step counts.
func TestRunnerSending(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	fn := func(name string) string {
		start := strings.Index(runner, "def "+name+"(")
		end := strings.Index(runner[start+1:], "\ndef ")
		return runner[start : start+1+end]
	}
	script := "import datetime, json, sys\n" + fn("sending") + "\n" + fn("stat") + "\n" +
		"for line in sys.stdin:\n    print(json.dumps(sending(json.loads(line))))\n"
	cmd := exec.Command(python, "-c", script)
	cmd.Env = append(os.Environ(), "TZ=UTC")
	cmd.Stdin = strings.NewReader(`{"min_request_timestamp":{"avg":1.0e18},"max_response_timestamp":{"avg":1.0001e18},"start_time":"2001-09-09T01:46:39"}
{"start_time":"2001-09-09T01:46:39","end_time":"2001-09-09T01:48:20.5"}
{"benchmark_duration":{"avg":100}}
`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := "[1000000000.0, 1000100000.0]\n[999999999.0, 1000000100.5]\n[null, null]\n"
	if string(out) != want {
		t.Fatalf("got\n%s", out)
	}
}
