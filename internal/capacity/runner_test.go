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

func TestRunner(t *testing.T) {
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
	// The instance's counters grow by 10,000 prompt tokens, 8,000 of them
	// cached, every time they are read.
	var reads atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := reads.Add(1)
		fmt.Fprintf(w, "# HELP vllm:prompt_tokens_total x\nvllm:prompt_tokens_total{engine=\"0\",model_name=\"m\"} %d\nvllm:prompt_tokens_cached_total{engine=\"0\",model_name=\"m\"} %d\n", n*10000, n*8000)
	}))
	defer server.Close()

	set := Defaults(0)
	set.Concurrency = []int{1, 2, 4, 8}
	set.TTFTMillis = 3000 // the step at 4 misses it
	config, _ := json.Marshal(map[string]any{"url": server.URL, "model": "m", "settings": set, "grace_seconds": 1, "agentic": Agentic(set)})

	cmd := exec.Command(python, "-c", runner)
	cmd.Env = append(os.Environ(), "AIGW_CAPACITY="+string(config), "AIGW_WORK="+filepath.Join(dir, "work"), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.Mkdir(filepath.Join(dir, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("runner: %v\n%s", err, out)
	}

	l := ParseLog(string(out))
	if !l.Done || l.Error != "" {
		t.Fatalf("not done:\n%s", out)
	}
	// Session a sends 1,000 then 1,000 + 100 + 200; b sends 1,000.
	if l.Dataset == nil || l.Dataset.Sessions != 2 || l.Dataset.PromptMax != 1300 || l.Dataset.PromptMean != 1100 {
		t.Fatalf("dataset = %+v", l.Dataset)
	}
	// It stops after the first step that misses the target.
	if len(l.Steps) != 3 || l.Steps[2].Concurrency != 4 {
		t.Fatalf("steps = %+v\n%s", l.Steps, out)
	}
	s := l.Steps[1]
	if s.Requests != 20 || s.InputTokens != 200000 || s.TTFTP95 != 2000 || s.ITLP95 != 20 {
		t.Fatalf("step = %+v", s)
	}
	if share := s.CachedShare(); share == nil || *share != 0.8 {
		t.Fatalf("cached share = %v", share)
	}
	r := Analyze(set, l.Steps)
	if r.Best == nil || r.Best.Concurrency != 2 || !strings.Contains(r.Limit, "at 4 in flight") {
		t.Fatalf("result = %+v %q", r.Best, r.Limit)
	}
}
