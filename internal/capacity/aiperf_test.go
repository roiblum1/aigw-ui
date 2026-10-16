package capacity

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRealAIPerf runs the Job's script with the real AIPerf, in the image
// the chart's capacity.image is built from, against a stand-in for vLLM in
// the same container, with no network. It checks what the script takes
// from AIPerf: the session settings it writes, the flags it passes and the
// fields it reads from AIPerf's summary.
//
// The stand-in serves two data-parallel ranks, and every session must stay
// on one of them, in every step: a conversation that moved between ranks
// would miss its own prefix in the cache, and the measurement would read low.
// AIPerf 0.13.0 picks a URL when a session starts and keeps it for its turns
// (aiperf/credit/issuer.py), and starts its round robin over in each run.
//
// CI builds the image and sets AIPERF_TEST_IMAGE. CONTAINER_TOOL picks the
// runtime, docker by default.
func TestRealAIPerf(t *testing.T) {
	image := os.Getenv("AIPERF_TEST_IMAGE")
	if image == "" {
		t.Skip("AIPERF_TEST_IMAGE is not set")
	}
	tool := os.Getenv("CONTAINER_TOOL")
	if tool == "" {
		tool = "docker"
	}
	dir := t.TempDir()
	mock, err := os.ReadFile("testdata/mock_vllm.py")
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"runner.py": []byte(runner), "mock_vllm.py": mock} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Small sessions, a sweep of two short steps, targets nothing misses.
	// Drained: the script waits for the stand-in to have nothing in flight,
	// and a step with other traffic would fail.
	set := Defaults(0)
	set.FirstConcurrency, set.MaxConcurrency, set.BisectSteps = 1, 2, 0
	set.StepSeconds = 60
	set.MinRequests = 10
	set.Sessions = 12
	set.MaxPromptTokens = 6000
	set.MaxOutputTokens = 64
	set.TTFTMillis = 600_000
	set.ITLMillis = 10_000
	config, _ := json.Marshal(map[string]any{"urls": []string{"http://127.0.0.1:8000", "http://127.0.0.1:8001"}, "model": "m", "settings": set,
		"grace_seconds": 30, "agentic": Agentic(set), "quiet_seconds": 2})

	cmd := exec.Command(tool, "run", "--rm", "--network", "none", "-v", dir+":/c:ro",
		"-e", "AIGW_CAPACITY="+string(config), "-e", "AIGW_WORK=/tmp/w", "-e", "MOCK_LOG=/tmp/w/ranks.log", image,
		"bash", "-c", "mkdir -p /tmp/w && (python3 /c/mock_vllm.py 8000 &) && (python3 /c/mock_vllm.py 8001 &) && sleep 4 && python3 /c/runner.py; s=$?; "+
			"cat /tmp/w/ranks.log; tail -40 /tmp/w/out/*.log >&2; exit $s")
	out, err := cmd.Output()
	if err != nil {
		var stderr []byte
		if e, ok := err.(*exec.ExitError); ok {
			stderr = e.Stderr
		}
		t.Fatalf("%v\n%s\n%s", err, out, stderr)
	}
	l := ParseLog(string(out))
	if !l.Done || l.Error != "" || l.Dataset == nil || l.Dataset.Sessions != 12 || l.Dataset.PromptMax > 6000 {
		t.Fatalf("log:\n%s", out)
	}
	if len(l.Steps) != 2 {
		t.Fatalf("steps: %+v\n%s", l.Steps, out)
	}
	for _, s := range l.Steps {
		if s.Error != "" || s.Requests < set.MinRequests || s.InputTokens <= 0 || s.OutputTokens <= 0 || s.DurationSeconds <= 0 ||
			s.TTFTP95 <= 0 || s.ITLP95 <= 0 || s.InputMean <= 0 || s.CachedShare() == nil || s.ServerOutputTokens == nil {
			t.Errorf("step %d: %+v\n%s", s.Concurrency, s, out)
		}
		// AIPerf counts the tokens the stand-in reports, so nothing is
		// foreign; a request still in flight when AIPerf stops may add a
		// little.
		if ok, why := s.Check(set); !ok || *s.ForeignShare(set) > 0.02 {
			t.Errorf("step %d: %v %q, foreign %v", s.Concurrency, ok, why, *s.ForeignShare(set))
		}
		// The counters are taken over AIPerf's profiling, which holds its
		// first request to its last answer and a few seconds more, and the
		// quiet spell before the step saw nothing.
		if s.ServerSeconds < s.DurationSeconds-0.5 || s.ServerSeconds > s.DurationSeconds+5 || s.QuietSeconds < 2 ||
			s.QuietPromptTokens == nil || *s.QuietPromptTokens != 0 {
			t.Errorf("step %d: server %v s, AIPerf %v s, quiet %v s %v", s.Concurrency, s.ServerSeconds, s.DurationSeconds, s.QuietSeconds, s.QuietPromptTokens)
		}
	}
	checkSessionsStay(t, string(out))
}

// checkSessionsStay reads the stand-in's RANK lines: every session, by its
// correlation ID within a step and by its first message across steps, was
// served by one rank, both ranks served, and some session had several turns.
func checkSessionsStay(t *testing.T, out string) {
	t.Helper()
	bySession, byOpening := map[string]map[string]bool{}, map[string]map[string]bool{}
	add := func(m map[string]map[string]bool, key, port string) {
		if m[key] == nil {
			m[key] = map[string]bool{}
		}
		m[key][port] = true
	}
	turns, ranks := map[string]int{}, map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 4 || f[0] != "RANK" {
			continue
		}
		port, session, opening := f[1], f[2], f[3]
		add(bySession, session, port)
		add(byOpening, opening, port)
		turns[session]++
		ranks[port] = true
	}
	multi := 0
	for session, n := range turns {
		if n > 1 {
			multi++
		}
		if len(bySession[session]) != 1 {
			t.Errorf("session %s was served by ranks %v", session, bySession[session])
		}
	}
	for opening, ports := range byOpening {
		if len(ports) != 1 {
			t.Errorf("the session opening %s was served by ranks %v over the steps", opening, ports)
		}
	}
	if len(ranks) != 2 || multi == 0 {
		t.Errorf("ranks %v, %d sessions of several turns", ranks, multi)
	}
}
