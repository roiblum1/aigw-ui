package capacity

import (
	"encoding/json"
	"strings"
	"testing"
)

func ptr(f float64) *float64 { return &f }

func step(c int, ttft, itl float64) Step {
	return Step{Concurrency: c, Requests: 100, DurationSeconds: 100, InputTokens: 1_000_000 * int64(c), OutputTokens: 10_000 * int64(c),
		TTFTP95: ttft, ITLP95: itl, ServerPromptTokens: ptr(1_000_000), ServerCachedTokens: ptr(900_000)}
}

func TestDefaults(t *testing.T) {
	d := Defaults(0)
	if d.MaxPromptTokens != 167000 || d.MaxOutputTokens != 1500 || d.Validate() != nil {
		t.Fatalf("unknown length: %+v, %v", d, d.Validate())
	}
	d = Defaults(131072)
	if d.MaxPromptTokens != 131072-1500 || d.Validate() != nil {
		t.Fatalf("128k model: %+v, %v", d, d.Validate())
	}
	// The lab's tiny model: a prompt and an answer still fit.
	d = Defaults(8192)
	if d.MaxPromptTokens+d.MaxOutputTokens > 8192 || d.Validate() != nil {
		t.Fatalf("8k model: %+v, %v", d, d.Validate())
	}
}

func TestValidate(t *testing.T) {
	for name, change := range map[string]func(*Settings){
		"no steps":           func(s *Settings) { s.Concurrency = nil },
		"steps go down":      func(s *Settings) { s.Concurrency = []int{4, 2} },
		"a step repeats":     func(s *Settings) { s.Concurrency = []int{2, 2} },
		"zero in flight":     func(s *Settings) { s.Concurrency = []int{0, 1} },
		"short step":         func(s *Settings) { s.StepSeconds = 10 },
		"no ttft target":     func(s *Settings) { s.TTFTMillis = 0 },
		"utilization 1.5":    func(s *Settings) { s.Utilization = 1.5 },
		"negative cost":      func(s *Settings) { s.CostPerHour = -1 },
		"tiny prompt":        func(s *Settings) { s.MaxPromptTokens = 10 },
		"too many steps":     func(s *Settings) { s.Concurrency = []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13} },
		"too many in flight": func(s *Settings) { s.Concurrency = []int{1, 2000} },
	} {
		s := Defaults(0)
		change(&s)
		if s.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAnalyzeStopsAtTheFirstMiss(t *testing.T) {
	set := Defaults(0)
	steps := []Step{step(1, 900, 20), step(2, 1200, 25), step(4, 6000, 30), step(8, 1000, 20)}
	r := Analyze(set, steps)
	if r.Best == nil || r.Best.Concurrency != 2 {
		t.Fatalf("best = %+v", r.Best)
	}
	if !strings.Contains(r.Limit, "at 4 in flight") || !strings.Contains(r.Limit, "time to first token") {
		t.Fatalf("limit = %q", r.Limit)
	}

	r = Analyze(set, steps[:2])
	if r.Best.Concurrency != 2 || !strings.Contains(r.Limit, "may serve more") {
		t.Fatalf("all passed: %+v %q", r.Best, r.Limit)
	}

	r = Analyze(set, []Step{step(1, 900, 80)})
	if r.Best != nil || !strings.Contains(r.Limit, "time between tokens") {
		t.Fatalf("first step missed: %+v %q", r.Best, r.Limit)
	}
}

func TestCheckErrors(t *testing.T) {
	set := Defaults(0)
	s := step(1, 100, 10)
	s.Errors = 2 // 2 of 102
	if ok, why := s.Check(set); ok || !strings.Contains(why, "2 of 102") {
		t.Fatalf("errors: %v %q", ok, why)
	}
	s.Errors = 1 // under 1%
	if ok, _ := s.Check(set); !ok {
		t.Fatal("one error in 101 should pass")
	}
	if ok, why := (Step{Concurrency: 1, Error: "AIPerf failed: boom"}).Check(set); ok || why != "AIPerf failed: boom" {
		t.Fatalf("step error: %v %q", ok, why)
	}
}

func TestPlanInCredits(t *testing.T) {
	// One instance: 10,000 prompt tokens a second, 90% from the cache, and
	// 100 output tokens a second.
	thr := Throughput{Concurrency: 8, InputPerSecond: 10000, CachedPerSecond: 9000, OutputPerSecond: 100, CachedShare: ptr(0.9)}
	set := Defaults(0)
	set.CostPerHour = 20
	set.Utilization = 0.5
	// $1 per 1M input, $0.10 cached, $4 output.
	prices := &Prices{Input: 100000, Cached: 10000, Output: 400000}
	p := MakePlan(PlanInput{Unit: "credits", Prices: prices, Throughput: thr, Settings: set, Instances: 2, AllocatedPerDay: 50_000_000, BusiestDay: 10_000_000})

	// A second: 1,000 uncached at 0.1 credit + 9,000 cached at 0.01 + 100 out at 0.4 = 100 + 90 + 40 = 230 credits.
	if p.PerInstanceDay != 230*86400 {
		t.Fatalf("per instance day = %d", p.PerInstanceDay)
	}
	if p.CapacityDay != 230*86400*2/2 {
		t.Fatalf("capacity day = %d", p.CapacityDay)
	}
	if p.Overcommit == nil || *p.Overcommit < 2.51 || *p.Overcommit > 2.52 {
		t.Fatalf("overcommit = %v", p.Overcommit)
	}
	if p.BusiestShare == nil || *p.BusiestShare < 0.25 || *p.BusiestShare > 0.26 {
		t.Fatalf("busiest share = %v", p.BusiestShare)
	}
	if *p.ChargedPerInstanceHour != 230*3600 || *p.CostPerInstanceHour != 2_000_000 {
		t.Fatalf("charged %d, cost %d", *p.ChargedPerInstanceHour, *p.CostPerInstanceHour)
	}
	// Weighted: 1,000 + 0.1 x 9,000 + 4 x 100 = 2,300 input tokens a second.
	// $20 / (3,600 x 2,300 x 0.5) x 1M = $4.83 per 1M input.
	if p.BreakEven == nil || p.BreakEven.Input != 483092 || p.BreakEven.Cached != 48310 || p.BreakEven.Output != 1932368 {
		t.Fatalf("break even = %+v", p.BreakEven)
	}
	if len(p.Notes) != 0 {
		t.Fatalf("notes = %v", p.Notes)
	}
}

func TestPlanInTokens(t *testing.T) {
	thr := Throughput{Concurrency: 4, InputPerSecond: 1000, OutputPerSecond: 50}
	p := MakePlan(PlanInput{Unit: "tokens", Throughput: thr, Settings: Defaults(0), Instances: 1, CostExpression: "input_tokens"})
	if p.PerInstanceDay != 1050*86400 || p.CapacityDay != int64(1050*86400*0.6) {
		t.Fatalf("tokens: %d %d", p.PerInstanceDay, p.CapacityDay)
	}
	if p.BreakEven != nil || p.ChargedPerInstanceHour != nil || p.Overcommit == nil || *p.Overcommit != 0 {
		t.Fatalf("tokens: %+v", p)
	}
	if len(p.Notes) != 2 { // no cache figures, own cost expression
		t.Fatalf("notes = %v", p.Notes)
	}

	none := MakePlan(PlanInput{Unit: "tokens", Throughput: thr, Settings: Defaults(0)})
	if none.CapacityDay != 0 || none.Overcommit != nil || none.BusiestShare != nil {
		t.Fatalf("no instance: %+v", none)
	}
}

func TestPerDay(t *testing.T) {
	if PerDay("1m") != 1440 || PerDay("1h") != 24 || PerDay("1d") != 1 {
		t.Fatal("per day")
	}
}

func TestAgenticScales(t *testing.T) {
	full := Agentic(Defaults(0))
	cache := full["cache"].(map[string]any)
	if cache["layer1_tokens"] != 32000 || full["block_size"] != 512 || full["max_prompt_tokens"] != 167000 {
		t.Fatalf("full size: %v", full)
	}
	small := Agentic(Defaults(8192))
	cache = small["cache"].(map[string]any)
	l1 := cache["layer1_tokens"].(int)
	if l1 < 1000 || l1 > 2000 || small["block_size"].(int) < 64 {
		t.Fatalf("scaled down: l1 %d, %v", l1, small)
	}
	gen := small["generation_length"].(map[string]any)
	if gen["max"].(float64) != float64(Defaults(8192).MaxOutputTokens) {
		t.Fatalf("answer: %v", gen)
	}
	// AIPerf reads it as JSON.
	if _, err := json.Marshal(small); err != nil {
		t.Fatal(err)
	}
}

func TestJob(t *testing.T) {
	set := Defaults(0)
	job, err := Job(JobSpec{RunID: "0123", Name: "aigw-capacity-0123", Namespace: "llm", Image: "registry/aiperf:1", URL: "http://10.0.0.5:8000", Model: "glm", Settings: set})
	if err != nil {
		t.Fatal(err)
	}
	spec := job.Object["spec"].(map[string]any)
	if spec["backoffLimit"] != int64(0) || spec["activeDeadlineSeconds"] != int64(set.Duration()) {
		t.Fatalf("spec: %v", spec)
	}
	pod := spec["template"].(map[string]any)["spec"].(map[string]any)
	c := pod["containers"].([]any)[0].(map[string]any)
	if c["image"] != "registry/aiperf:1" || c["command"].([]any)[2] != runner {
		t.Fatalf("container: %v", c["image"])
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(c["env"].([]any)[0].(map[string]any)["value"].(string)), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["url"] != "http://10.0.0.5:8000" || cfg["model"] != "glm" || cfg["agentic"] == nil {
		t.Fatalf("config: %v", cfg)
	}
	if job.GetLabels()[RunLabel] != "0123" || pod["automountServiceAccountToken"] != false {
		t.Fatalf("labels %v", job.GetLabels())
	}
}

func TestParseLog(t *testing.T) {
	text := strings.Join([]string{
		"INFO aiperf starting",
		`AIGW-DATASET {"sessions":200,"requests":4100,"prompt_mean":98123.5,"prompt_max":166000}`,
		`AIGW-STEP {"concurrency":1,"requests":20,"errors":0,"duration_s":300,"input_tokens":2000000,"output_tokens":20000,"ttft_p95_ms":800,"itl_p95_ms":20,"server_prompt_tokens":2000000,"server_cached_tokens":1800000}`,
		`AIGW-STEP {not json`,
		`AIGW-STEP {"concurrency":2,"error":"AIPerf failed: x"}`,
		"AIGW-DONE {}",
	}, "\n")
	l := ParseLog(text)
	if l.Dataset == nil || l.Dataset.Sessions != 200 || len(l.Steps) != 2 || !l.Done {
		t.Fatalf("log = %+v", l)
	}
	if share := l.Steps[0].CachedShare(); share == nil || *share != 0.9 {
		t.Fatalf("cached share = %v", share)
	}
	if l.Steps[1].Error == "" || l.Steps[0].ServerCachedTokens == nil {
		t.Fatalf("steps = %+v", l.Steps)
	}
	if e := ParseLog(`AIGW-ERROR {"error":"making up the sessions failed: boom"}`); e.Error == "" || e.Done {
		t.Fatalf("error = %+v", e)
	}
}

func TestJobName(t *testing.T) {
	if n := JobName("8a3f2c1d-0000-4000-8000-000000000000"); n != "aigw-capacity-8a3f2c1d0000" {
		t.Fatal(n)
	}
}
