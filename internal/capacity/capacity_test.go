package capacity

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
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
		"no mode":            func(s *Settings) { s.Mode = "" },
		"a list of steps":    func(s *Settings) { s.Concurrency = []int{1, 2} },
		"zero in flight":     func(s *Settings) { s.FirstConcurrency = 0 },
		"max below first":    func(s *Settings) { s.FirstConcurrency, s.MaxConcurrency = 8, 4 },
		"too many in flight": func(s *Settings) { s.MaxConcurrency = 2000 },
		"many bisections":    func(s *Settings) { s.BisectSteps = 9 },
		"short step":         func(s *Settings) { s.StepSeconds = 10 },
		"no ttft target":     func(s *Settings) { s.TTFTMillis = 0 },
		"few requests":       func(s *Settings) { s.MinRequests = 2 },
		"tiny prompt":        func(s *Settings) { s.MaxPromptTokens = 10 },
		"errors allowed":     func(s *Settings) { s.MaxErrorShare = 0.9 },
	} {
		s := Defaults(0)
		change(&s)
		if s.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := Defaults(0).Validate(); err != nil {
		t.Fatal(err)
	}
	// 1 to 1,024 is eleven doublings, then two bisections, of five minutes
	// each with the time to finish, to settle and watch the instance, and
	// to start.
	if d := Defaults(0); d.Steps() != 13 || d.Duration() != 13*(300+300+60+20+60)+600+900 {
		t.Fatalf("steps %d, duration %d", d.Steps(), d.Duration())
	}
}

func TestProfile(t *testing.T) {
	p := DefaultProfile()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Profile){
		"no name":             func(p *Profile) { p.Name = "" },
		"prefix over half":    func(p *Profile) { p.Workload.SharedPrefixTokens = p.Workload.MaxPromptTokens },
		"negative pause":      func(p *Profile) { p.Workload.PauseMillis = -1 },
		"no itl target":       func(p *Profile) { p.SLO.ITLMillis = 0 },
		"max below first":     func(p *Profile) { p.Sweep.MaxConcurrency = 0 },
		"bisections negative": func(p *Profile) { p.Sweep.BisectSteps = -1 },
	} {
		q := DefaultProfile()
		change(&q)
		if q.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// One profile for every deployment: a short one gets its sessions scaled
	// down, the rest stays.
	s := p.Settings(8192, ModeLive)
	if s.MaxPromptTokens+s.MaxOutputTokens > 8192 || s.Mode != ModeLive || s.SharedPrefixTokens != 32000 || s.StepSeconds != 300 || s.Validate() != nil {
		t.Fatalf("8k: %+v %v", s, s.Validate())
	}
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		version int
		age     time.Duration
		want    string
	}{{1, time.Hour, ""}, {1, 31 * 24 * time.Hour, StaleAge}, {2, time.Hour, StaleProfile}} {
		if got := Staleness(tc.version, 1, now.Add(-tc.age), now); got != tc.want {
			t.Errorf("version %d, %v old: %q", tc.version, tc.age, got)
		}
	}
}

// With bisection the steps do not come in order: 1, 2, 4 and 8 met the
// targets, 16 missed, 12 met them, 14 missed. The best is 12, and a step
// above the lowest miss that passes is noise.
func TestAnalyzeBisected(t *testing.T) {
	set := Defaults(0)
	steps := []Step{step(1, 900, 20), step(2, 1000, 20), step(4, 1500, 20), step(8, 2500, 20), step(16, 7000, 20),
		step(12, 4500, 20), step(14, 5500, 20), step(32, 1000, 20)}
	r := Analyze(set, steps)
	if r.Best == nil || r.Best.Concurrency != 12 || !strings.Contains(r.Limit, "at 14 in flight") {
		t.Fatalf("best %+v, limit %q", r.Best, r.Limit)
	}
}

// A drained step with other traffic in it does not count; live, the same
// step does, and shows how much was not the measurement's.
func TestContamination(t *testing.T) {
	s := step(4, 900, 20) // sent 4,000,000 prompt tokens
	s.ServerPromptTokens = ptr(4_300_000)
	set := Defaults(0)
	if ok, why := s.Check(set); ok || !strings.Contains(why, "contaminated: 7%") {
		t.Fatalf("drained: %v %q", ok, why)
	}
	s.ServerPromptTokens = ptr(4_100_000) // 2.4%: within the 5%
	if ok, _ := s.Check(set); !ok {
		t.Fatal("2.4% failed")
	}

	// The measurement's own failed requests: their prompts are counted by
	// the instance and not by AIPerf. Two of 202 (1%) failed, and the
	// instance counted 300,000 more than was answered: within the longest
	// prompt (167,000) for each, so not contaminated, whatever the latency.
	e := step(4, 900, 20)
	e.Errors, e.Requests = 2, 200
	e.ServerPromptTokens = ptr(4_300_000)
	if ok, why := e.Check(set); !ok || *e.ForeignShare(set) != 0 {
		t.Fatalf("own failures: %v %q %v", ok, why, *e.ForeignShare(set))
	}
	// With more failures than the settings allow, they are the reason.
	e.Errors = 10
	if ok, why := e.Check(set); ok || why != "10 of 210 requests failed" {
		t.Fatalf("failures first: %v %q", ok, why)
	}

	// The quiet spell before the step: 1,000 prompt tokens a second came
	// while nothing was sent; over the step's 100 seconds that is 100,000
	// of the 4,100,000 counted, 2.4%. At 5,000 a second, 12%.
	q := step(4, 900, 20)
	q.ServerPromptTokens, q.ServerSeconds, q.QuietSeconds = ptr(4_100_000), 100, 20
	q.QuietPromptTokens = ptr(20_000)
	if ok, _ := q.Check(set); !ok || math.Abs(*q.ForeignShare(set)-100_000.0/4_100_000) > 1e-9 {
		t.Fatalf("quiet 1,000/s: %v %v", ok, *q.ForeignShare(set))
	}
	q.QuietPromptTokens = ptr(100_000)
	if ok, why := q.Check(set); ok || !strings.Contains(why, "contaminated: 12%") {
		t.Fatalf("quiet 5,000/s: %v %q", ok, why)
	}

	set.Mode = ModeLive
	s.ServerPromptTokens = ptr(8_000_000)
	if ok, _ := s.Check(set); !ok || math.Abs(*s.ForeignShare(set)-0.5) > 1e-9 {
		t.Fatalf("live: %v %v", ok, *s.ForeignShare(set))
	}
	// Live throughput is the instance's own counters, over their own time.
	s.ServerOutputTokens, s.ServerSeconds = ptr(20_000), 200
	if tp := set.Throughput(s); tp.InputPerSecond != 40_000 || tp.OutputPerSecond != 100 || tp.CachedPerSecond != 4500 { // 900,000 of 8,000,000 prompt tokens were cached
		t.Fatalf("live throughput: %+v", tp)
	}
	// A prefill/decode run spread over 4 decode replicas.
	if tp := set.Throughput(s).PerInstance(4); tp.InputPerSecond != 10_000 || tp.OutputPerSecond != 25 {
		t.Fatalf("per instance: %+v", tp)
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

func TestPlanAddsUpShapes(t *testing.T) {
	// GLM on two measured shapes: 8× B300 in one node, two instances, and
	// 8× H200, one instance. A third shape is not measured.
	b300 := Throughput{Concurrency: 16, InputPerSecond: 10000, CachedPerSecond: 9000, OutputPerSecond: 100, CachedShare: ptr(0.9)}
	h200 := Throughput{Concurrency: 8, InputPerSecond: 5000, CachedPerSecond: 4500, OutputPerSecond: 50, CachedShare: ptr(0.9)}
	gpuB300, gpuH200 := int64(500_000), int64(300_000) // $5 and $3 an hour
	// $1 per 1M input, $0.10 cached, $4 output.
	prices := &Prices{Input: 100000, Cached: 10000, Output: 400000}
	p := MakePlan(PlanInput{
		Unit: "credits", Prices: prices, Utilization: 0.5, Days: 30,
		Shapes: []ShapeInput{
			{Shape: Shape{Topology: "single", GPU: "NVIDIA-B300", GPUsPerInstance: 8}, Instances: 2, Sites: []string{"tlv"}, Measured: &b300, GPUPrice: &gpuB300},
			{Shape: Shape{Topology: "single", GPU: "NVIDIA-H200", GPUsPerInstance: 8}, Instances: 1, Sites: []string{"haifa"}, Measured: &h200, GPUPrice: &gpuH200},
			{Shape: Shape{Topology: "prefill-decode", GPU: "NVIDIA-H200", GPUsPerInstance: 16}, Instances: 3, Sites: []string{"haifa"}, GPUPrice: &gpuH200},
		},
		// $500 a day, and two budgets of $0.01 an hour, which can take $0.24 a day each.
		Budgets:    []Budget{{Limit: 50_000_000, Window: "1d"}, {Limit: 1_000, Window: "1h"}, {Limit: 1_000, Window: "1h"}},
		Pool:       &Budget{Limit: 2_000_000, Window: "1h"},
		BusiestDay: 10_000_000, TotalUsed: 150_000_000,
	})

	// A B300 second: 1,000 uncached at 0.1 + 9,000 cached at 0.01 + 100 out at 0.4 = 230 credits; H200 half that.
	if p.Shapes[0].PerInstanceDay != 230*86400 || p.Shapes[1].PerInstanceDay != 115*86400 || p.Shapes[2].Measured {
		t.Fatalf("shapes: %+v", p.Shapes)
	}
	full := int64((2*230 + 115) * 86400)
	if p.FullDay != full || p.CapacityDay != full/2 || p.Instances != 6 || p.MeasuredInstances != 3 {
		t.Fatalf("plan: full %d capacity %d instances %v measured %v", p.FullDay, p.CapacityDay, p.Instances, p.MeasuredInstances)
	}
	// What the measured instances cost: 2 × 8 × $5 + 8 × $3 = $104 an hour.
	if p.CostPerHour == nil || *p.CostPerHour != 10_400_000 {
		t.Fatalf("cost = %v", p.CostPerHour)
	}
	// One blended price: input-equivalent tokens a second are 2 × 2,300 + 1,150 = 5,750.
	// $104 / (3,600 × 5,750 × 0.5) × 1M = $10.05 per 1M input.
	if p.BreakEven == nil || p.BreakEven.Input != 1004831 || p.BreakEven.Cached != 100484 || p.BreakEven.Output != 4019324 {
		t.Fatalf("break even = %+v", p.BreakEven)
	}
	// Each shape's own cost of a million input-equivalent tokens busy all day:
	// B300 $40 / (3,600 × 2,300) × 1M = $4.83; H200 $24 / (3,600 × 1,150) × 1M = $5.80.
	if *p.Shapes[0].CostPerMillion != 483092 || *p.Shapes[1].CostPerMillion != 579711 || p.Shapes[2].CostPerMillion != nil {
		t.Fatalf("cost per million: %v %v %v", *p.Shapes[0].CostPerMillion, *p.Shapes[1].CostPerMillion, p.Shapes[2].CostPerMillion)
	}
	if *p.ChargedPerHour != (2*230+115)*3600 {
		t.Fatalf("charged = %d", *p.ChargedPerHour)
	}
	// Given: $500 + 2 × $0.24. The ceiling is the capacity at the default
	// 1.5×, and the budgets are past it.
	if p.GivenDay != 50_048_000 || p.ShortWindows != 2 || p.OvercommitLimit != DefaultOvercommitLimit {
		t.Fatalf("given = %d, short windows %d, limit %v", p.GivenDay, p.ShortWindows, p.OvercommitLimit)
	}
	if p.CeilingDay != p.CapacityDay*3/2 || p.LeftDay != p.CeilingDay-p.GivenDay || p.LeftDay >= 0 {
		t.Fatalf("ceiling %d left %d capacity %d", p.CeilingDay, p.LeftDay, p.CapacityDay)
	}
	if o := *p.Overcommit; o < 2.01 || o > 2.02 {
		t.Fatalf("overcommit = %v", o)
	}
	if p.PoolDay == nil || *p.PoolDay != 48_000_000 {
		t.Fatalf("pool = %v", p.PoolDay)
	}
	if b := *p.BusiestShare; b < 0.2 || b > 0.202 {
		t.Fatalf("busiest share = %v", b)
	}
	if u := *p.ObservedUtilization; u < 0.1 || u > 0.101 {
		t.Fatalf("observed = %v", u)
	}
	// The busiest day used a fifth of the budgets, so they could be 5× the capacity.
	if *p.SafeAllocatedDay != int64(math.Round(float64(p.CapacityDay)*50_048_000/10_000_000)) {
		t.Fatalf("safe = %d, capacity %d", *p.SafeAllocatedDay, p.CapacityDay)
	}
	// 3 instances not measured; 2 hourly budgets; past the limit; a pool above the capacity.
	if len(p.Notes) != 4 || !strings.Contains(p.Notes[1], "2 budgets are per minute or hour") ||
		!strings.Contains(p.Notes[2], "past the overcommit limit") || !strings.Contains(p.Notes[3], "shared pool") {
		t.Fatalf("notes = %v", p.Notes)
	}

	// Within the limit, what is left is the ceiling less what is given.
	q := MakePlan(PlanInput{Unit: "credits", Prices: prices, Utilization: 0.5, OvercommitLimit: 3,
		Shapes:  []ShapeInput{{Shape: Shape{Topology: "single", GPU: "NVIDIA-B300", GPUsPerInstance: 8}, Instances: 2, Measured: &b300, GPUPrice: &gpuB300}},
		Budgets: []Budget{{Limit: 10_000_000, Window: "1d"}}})
	if q.CapacityDay != 230*86400 || q.CeilingDay != 3*230*86400 || q.LeftDay != 3*230*86400-10_000_000 || len(q.Notes) != 0 {
		t.Fatalf("within: %+v", q)
	}
}

func TestPlanWithoutPrices(t *testing.T) {
	thr := Throughput{Concurrency: 4, InputPerSecond: 1000, OutputPerSecond: 50}
	p := MakePlan(PlanInput{Unit: "tokens", Utilization: 0.6, CostExpression: "input_tokens",
		Shapes: []ShapeInput{{Shape: Shape{Topology: "single", GPU: "NVIDIA-B200", GPUsPerInstance: 8}, Instances: 1, Measured: &thr}}})
	if p.FullDay != 1050*86400 || p.CapacityDay != int64(1050*86400*0.6) {
		t.Fatalf("tokens: %d %d", p.FullDay, p.CapacityDay)
	}
	// No GPU price: no cost, no break-even; no model prices: nothing charged.
	if p.CostPerHour != nil || p.BreakEven != nil || p.ChargedPerHour != nil || p.Overcommit == nil || *p.Overcommit != 0 || p.SafeAllocatedDay != nil {
		t.Fatalf("tokens: %+v", p)
	}
	// GPU costs are optional: none set is not worth a note.
	if len(p.Notes) != 2 || !strings.Contains(p.Notes[0], "did not report the prompt tokens") { // no cache figures, own cost expression
		t.Fatalf("notes = %v", p.Notes)
	}
	// One shape costed and one not: the cost would leave one out, so no
	// cost and a note.
	gpuB200 := int64(300000)
	part := MakePlan(PlanInput{Unit: "tokens", Utilization: 0.6, Shapes: []ShapeInput{
		{Shape: Shape{Topology: "single", GPU: "NVIDIA-B200", GPUsPerInstance: 8}, Instances: 1, Measured: &thr, GPUPrice: &gpuB200},
		{Shape: Shape{Topology: "single", GPU: "NVIDIA-H200", GPUsPerInstance: 8}, Instances: 1, Measured: &thr}}})
	if part.CostPerHour != nil || part.BreakEven != nil || !strings.Contains(part.Notes[0], "GPU without a cost") {
		t.Fatalf("part: %+v", part)
	}

	// A shape on nodes without the GPU label, as on a CPU lab: there is no
	// GPU to price, and the note says what is missing instead.
	cpu := MakePlan(PlanInput{Unit: "tokens", Utilization: 0.6,
		Shapes: []ShapeInput{{Shape: Shape{Topology: "single"}, Instances: 1, Measured: &thr}}})
	if cpu.FullDay != 1050*86400 || cpu.CostPerHour != nil || !strings.Contains(cpu.Notes[0], "GPU of a measured LLMInferenceService is not known") {
		t.Fatalf("cpu: %+v", cpu)
	}

	none := MakePlan(PlanInput{Unit: "tokens", Utilization: 0.6})
	if none.CapacityDay != 0 || none.Overcommit != nil || none.BusiestShare != nil || len(none.Notes) != 1 {
		t.Fatalf("no instance: %+v", none)
	}
}

func TestShape(t *testing.T) {
	a := Shape{Topology: "single", GPU: "NVIDIA-B300", GPUsPerInstance: 8, Revision: "r1"}
	if a.Label() != "8× NVIDIA-B300, one node, revision r1" || a.Key() != "single|NVIDIA-B300|8|r1" {
		t.Fatalf("%q %q", a.Label(), a.Key())
	}
	b := a
	b.GPUsPerInstance = 8.0000001
	if a.Key() != b.Key() {
		t.Fatal("a rounding difference is another shape")
	}
	if l := (Shape{Topology: "prefill-decode"}).Label(); l != "GPU not known, prefill and decode" {
		t.Fatal(l)
	}
}

func TestStepViews(t *testing.T) {
	set := Defaults(0)
	set.MinRequests = 50
	steps := []Step{step(1, 900, 20), step(2, 9000, 25), step(4, 900, 20)}
	steps[0].Requests = 30 // reached its longest time first
	r := Analyze(set, steps)
	if len(r.Steps) != 3 || !r.Steps[0].Met || !r.Steps[0].FewSamples || r.Steps[1].Met || r.Steps[1].Reason == "" || !r.Steps[2].Met {
		t.Fatalf("views = %+v", r.Steps)
	}
	if r.Steps[0].InputPerSecond != 10000 || *r.Steps[0].CachedShare != 0.9 || r.Best.Concurrency != 1 {
		t.Fatalf("views = %+v best %+v", r.Steps[0], r.Best)
	}
	if set.Requests(1) != 50 || set.Requests(64) != 256 {
		t.Fatal("requests per step")
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
	if cache["layer1_tokens"] != 32000 || full["block_size"] != 512 || full["max_prompt_tokens"] != 167000 || full["inter_turn_delay"].(map[string]any)["max"] != 0 {
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

// A prefill/decode Job reaches the peer listener with the site's client
// certificate, mounted read-only, and reads every decode pod's counters.
func TestJobPeer(t *testing.T) {
	job, err := Job(JobSpec{RunID: "0123", Name: "aigw-capacity-0123", Namespace: "envoy-ai-gateway-system", Image: "registry/aiperf:1", Model: "glm",
		Settings: Defaults(0), MetricsURLs: []string{"http://10.0.0.5:8000", "http://10.0.0.6:8000"},
		Peer: &Peer{Address: "gw.svc:8443", SNI: "peers.llm.example", ClientSecret: "llm-peer-client", CAConfigMap: "llm-peer-ca"}})
	if err != nil {
		t.Fatal(err)
	}
	pod := job.Object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	c := pod["containers"].([]any)[0].(map[string]any)
	var cfg struct {
		Peer        map[string]string `json:"peer"`
		MetricsURLs []string          `json:"metrics_urls"`
	}
	json.Unmarshal([]byte(c["env"].([]any)[0].(map[string]any)["value"].(string)), &cfg)
	if cfg.Peer["address"] != "gw.svc:8443" || cfg.Peer["sni"] != "peers.llm.example" || cfg.Peer["cert"] != "/peer/tls.crt" || len(cfg.MetricsURLs) != 2 {
		t.Fatalf("config: %+v", cfg)
	}
	mounts, _ := json.Marshal(c["volumeMounts"])
	volumes, _ := json.Marshal(pod["volumes"])
	if !strings.Contains(string(mounts), `"mountPath":"/peer","name":"peer","readOnly":true`) ||
		!strings.Contains(string(volumes), `"secret":{"secretName":"llm-peer-client"}`) || !strings.Contains(string(volumes), `"configMap":{"name":"llm-peer-ca"}`) {
		t.Fatalf("mounts %s, volumes %s", mounts, volumes)
	}
	// The proxy runs beside AIPerf: four CPUs, not two.
	if cpu := c["resources"].(map[string]any)["requests"].(map[string]any)["cpu"]; cpu != "4" {
		t.Fatalf("cpu %v", cpu)
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
