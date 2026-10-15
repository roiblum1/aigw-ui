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
// dataset, and for a profile a summary whose time to first token grows with
// the load: 1,000 ms per request in flight.
const fakeAIPerf = `#!/usr/bin/env python3
import json, os, sys
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
`

// instance is what the stand-in for vLLM reports.
type instance struct {
	maxModelLen int
	// promptPerRead is what its prompt counter grows by between two
	// readings of /metrics: 10,000 for an instance that only serves the
	// measurement in this test, far more for one that other clients reach.
	promptPerRead int64
	// running is its gauge of requests in flight.
	running int
}

// runRunner runs the Job's script with the fake aiperf against a stand-in
// instance, and returns what it printed.
func runRunner(t *testing.T, set Settings, in instance) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed")
	}
	if in.promptPerRead == 0 {
		in.promptPerRead = 10000
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "aiperf"), []byte(fakeAIPerf), 0o755); err != nil {
		t.Fatal(err)
	}
	// The counters grow at every reading: prompt tokens, 80% of them
	// cached, and 500 generated tokens.
	var reads atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"object":"list","data":[{"id":"m","object":"model","max_model_len":%d}]}`, in.maxModelLen)
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		n := reads.Add(1)
		fmt.Fprintf(w, "# HELP vllm:prompt_tokens_total x\nvllm:prompt_tokens_total{engine=\"0\",model_name=\"m\"} %d\n"+
			"vllm:prompt_tokens_cached_total{engine=\"0\",model_name=\"m\"} %d\nvllm:generation_tokens_total{engine=\"0\"} %d\n"+
			"vllm:num_requests_running{engine=\"0\",model_name=\"m\"} %d\n", n*in.promptPerRead, n*in.promptPerRead*8/10, n*500, in.running)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	config, _ := json.Marshal(map[string]any{"url": server.URL, "model": "m", "settings": set, "grace_seconds": 1,
		"agentic": Agentic(set), "wait_idle_seconds": 0})
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
	out := runRunner(t, set, instance{maxModelLen: 200000})

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
	if s.Requests != 20 || s.InputTokens != 200000 || s.TTFTP95 != 2000 || s.ITLP95 != 20 || s.ServerOutputTokens == nil || *s.ServerOutputTokens != 500 || s.ServerSeconds <= 0 {
		t.Fatalf("step = %+v", s)
	}
	if share := s.CachedShare(); share == nil || *share != 0.8 {
		t.Fatalf("cached share = %v", share)
	}
	r := Analyze(set, l.Steps)
	if r.Best == nil || r.Best.Concurrency != 3 || !strings.Contains(r.Limit, "at 4 in flight") {
		t.Fatalf("result = %+v %q", r.Best, r.Limit)
	}
}

// A model shorter than the settings would refuse every request: the Job stops
// before it makes up any session, and says what to set.
func TestRunnerModelTooShort(t *testing.T) {
	out := runRunner(t, Defaults(0), instance{maxModelLen: 8192})
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
// more than the measurement sends. Drained, the step fails as contaminated
// and the sweep stops; live, the same traffic is expected and shown.
func TestRunnerForeignTraffic(t *testing.T) {
	side := instance{maxModelLen: 200000, promptPerRead: 10_000_000}
	set := Defaults(0)
	set.MaxConcurrency = 2
	l := ParseLog(runRunner(t, set, side))
	if len(l.Steps) != 1 || !l.Done {
		t.Fatalf("drained: %+v", l.Steps)
	}
	if ok, why := l.Steps[0].Check(set); ok || !strings.Contains(why, "contaminated: 99%") {
		t.Fatalf("drained step: %v %q", ok, why)
	}

	set.Mode = ModeLive
	l = ParseLog(runRunner(t, set, side))
	if len(l.Steps) != 2 {
		t.Fatalf("live: %+v", l.Steps)
	}
	r := Analyze(set, l.Steps)
	if r.Best == nil || r.Best.Concurrency != 2 || r.Steps[0].ForeignShare == nil || *r.Steps[0].ForeignShare < 0.98 {
		t.Fatalf("live result: %+v", r)
	}
	// Live, the throughput is the instance's own counters: 10,000,000
	// prompt tokens over the time between two readings.
	if tp := set.Throughput(*r.Best); tp.InputPerSecond < 10_000_000/r.Best.ServerSeconds*0.99 {
		t.Fatalf("live throughput = %+v", tp)
	}
}

// A drained instance that never runs dry is not measured.
func TestRunnerNotIdle(t *testing.T) {
	l := ParseLog(runRunner(t, Defaults(0), instance{maxModelLen: 200000, running: 3}))
	if l.Dataset != nil || !strings.Contains(l.Error, "still has 3 requests in flight") {
		t.Fatalf("not idle: %+v", l)
	}
}
