package llmi

import (
	"strings"
	"testing"
	"time"

	"aigw-ui/internal/capacity"
	"aigw-ui/internal/store"
)

var now = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func deployment(cluster, name, spec string, ready float64) store.Deployment {
	return store.Deployment{ClusterID: cluster, ClusterName: "site-" + cluster, Namespace: "llm", Name: name, ModelID: "glm",
		Topology: "single", SpecHash: spec, Instances: &ready}
}

// run is a measurement that met the targets at 8 in flight with 1,000 input
// tokens a second per request in flight.
func run(cluster, name, spec string, version int, age time.Duration, inFlight int) store.CapacityRun {
	best := capacity.Step{Concurrency: inFlight, Requests: 100, DurationSeconds: 100, InputTokens: int64(inFlight) * 100_000, OutputTokens: 1000}
	set := capacity.Defaults(0)
	return store.CapacityRun{ClusterID: cluster, Namespace: "llm", Deployment: name, SpecHash: spec, ProfileVersion: &version,
		Settings: set, InstancesDuring: 1, CreatedAt: now.Add(-age), Result: capacity.Analyze(set, []capacity.Step{best})}
}

// Two LLMIs of one model on two sites are measured and counted apart, each
// with its own figure.
func TestTwoSites(t *testing.T) {
	ds := []store.Deployment{deployment("a", "glm-b200", "s1", 2), deployment("b", "glm-h200", "s2", 1)}
	runs := []store.CapacityRun{run("a", "glm-b200", "s1", 1, time.Hour, 16), run("b", "glm-h200", "s2", 1, time.Hour, 8)}
	v, rs := Model(ds, runs, 1, now)
	if v != 1 || !rs[0].Measured() || !rs[1].Measured() || rs[0].PerInstance.InputPerSecond != 16000 || rs[1].PerInstance.InputPerSecond != 8000 ||
		rs[0].Stale != "" {
		t.Fatalf("%d %+v", v, rs)
	}
}

// Scaling keeps the spec and the result. A changed argument changes the
// spec: the result keeps counting, marked stale, so the model does not fall
// back to declared site weights with each tuning, until it is measured on
// the new spec, which then counts even when an older run is newer.
func TestSpecChange(t *testing.T) {
	runs := []store.CapacityRun{run("a", "glm-b200", "s1", 1, time.Hour, 16)}
	_, rs := Model([]store.Deployment{deployment("a", "glm-b200", "s1", 5)}, runs, 1, now)
	if !rs[0].Measured() || rs[0].Stale != "" {
		t.Fatal("scaled: not measured")
	}
	changed := deployment("a", "glm-b200", "s1-new-args", 2)
	_, rs = Model([]store.Deployment{changed}, runs, 1, now)
	if !rs[0].Measured() || rs[0].Stale != capacity.StaleSpec || rs[0].PerInstance.InputPerSecond != 16000 {
		t.Fatalf("changed: %+v", rs[0])
	}
	runs = append(runs, run("a", "glm-b200", "s1-new-args", 1, 2*time.Hour, 12))
	_, rs = Model([]store.Deployment{changed}, runs, 1, now)
	if rs[0].Stale != "" || rs[0].PerInstance.InputPerSecond != 12000 {
		t.Fatalf("measured again: %+v", rs[0])
	}
	_, rs = Model([]store.Deployment{deployment("c", "glm-new", "s9", 1)}, runs, 1, now)
	if rs[0].Why != "not measured" {
		t.Fatalf("new: %q", rs[0].Why)
	}
	// While what it runs cannot be told, there is no hash and nothing counts.
	_, rs = Model([]store.Deployment{deployment("a", "glm-b200", "", 2)}, runs, 1, now)
	if rs[0].Measured() || !strings.Contains(rs[0].Why, "cannot be told") {
		t.Fatalf("no hash: %+v", rs[0])
	}
}

// A new profile version: the old results keep counting, stale, until the new
// version covers more of the ready instances. Versions are never mixed.
func TestProfileChange(t *testing.T) {
	ds := []store.Deployment{deployment("a", "glm-b200", "s1", 2), deployment("b", "glm-h200", "s2", 1)}
	old := []store.CapacityRun{run("a", "glm-b200", "s1", 1, time.Hour, 16), run("b", "glm-h200", "s2", 1, time.Hour, 8)}

	v, rs := Model(ds, old, 2, now)
	if v != 1 || !rs[0].Measured() || rs[0].Stale != capacity.StaleProfile {
		t.Fatalf("after the change: %d %+v", v, rs[0])
	}
	// b, one instance, is measured again: version 1 still covers 3 of 3.
	partly := append(old, run("b", "glm-h200", "s2", 2, time.Minute, 10))
	v, rs = Model(ds, partly, 2, now)
	if v != 1 || rs[1].PerInstance.InputPerSecond != 8000 {
		t.Fatalf("one of two: %d %+v", v, rs[1])
	}
	// a, two instances, too: version 2 covers as much as version 1, and is
	// the active one.
	all := append(partly, run("a", "glm-b200", "s1", 2, time.Minute, 20))
	v, rs = Model(ds, all, 2, now)
	if v != 2 || rs[0].PerInstance.InputPerSecond != 20000 || rs[1].PerInstance.InputPerSecond != 10000 || rs[0].Stale != "" {
		t.Fatalf("both: %d %+v", v, rs)
	}
	// Only a measured with version 2 and b with version 1: a covers 2 of 3,
	// so the model counts version 2 and b waits, with the reason.
	mixed := []store.CapacityRun{run("a", "glm-b200", "s1", 2, time.Minute, 20), run("b", "glm-h200", "s2", 1, time.Hour, 8)}
	v, rs = Model(ds, mixed, 2, now)
	if v != 2 || !rs[0].Measured() || rs[1].Measured() || !strings.Contains(rs[1].Why, "version 1 of the profile") {
		t.Fatalf("mixed: %d %+v", v, rs)
	}
}

// A result older than 30 days still counts, marked stale.
func TestOld(t *testing.T) {
	_, rs := Model([]store.Deployment{deployment("a", "glm-b200", "s1", 1)}, []store.CapacityRun{run("a", "glm-b200", "s1", 1, 31*24*time.Hour, 16)}, 1, now)
	if !rs[0].Measured() || rs[0].Stale != capacity.StaleAge {
		t.Fatalf("%+v", rs[0])
	}
}

// A prefill/decode run is divided over the decode replicas it ran on.
func TestPrefillDecode(t *testing.T) {
	r := run("a", "glm-pd", "s1", 1, time.Hour, 32)
	r.InstancesDuring = 4
	_, rs := Model([]store.Deployment{deployment("a", "glm-pd", "s1", 4)}, []store.CapacityRun{r}, 1, now)
	if rs[0].PerInstance.InputPerSecond != 8000 {
		t.Fatalf("%+v", rs[0].PerInstance)
	}
}

// The queue lists what is missing first, then what is stale, and says what
// each waits for; an empty queue is a list, not null.
func TestQueue(t *testing.T) {
	if q := Queue(nil, nil, nil, 1, now, nil, true); q == nil || len(q) != 0 {
		t.Fatalf("empty queue = %#v", q)
	}
	ds := []store.Deployment{deployment("a", "glm-b200", "s1", 2), deployment("b", "glm-h200", "s2", 1),
		{ClusterID: "a", ClusterName: "site-a", Namespace: "llm", Name: "qwen", ModelID: "qwen", SpecHash: "q1", Instances: ready(1)}}
	runs := []store.CapacityRun{run("a", "glm-b200", "s1", 1, 40*24*time.Hour, 16)}
	used := map[string]Load{"glm": {History: true}, "qwen": {History: true}}
	q := Queue(ds, runs, nil, 1, now, used, true)
	got := []string{}
	for _, i := range q {
		got = append(got, i.ClusterID+"/"+i.Name+" "+i.Reason+" "+i.State)
	}
	want := []string{"b/glm-h200 not measured next", "a/qwen not measured " + WaitsOperator, "a/glm-b200 age " + WaitsModel}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("queue:\n%s", strings.Join(got, "\n"))
	}
	// While glm is measured, its other LLMI waits, and so does the site.
	running := []store.CapacityRun{{ModelID: "glm", ClusterID: "b", Namespace: "llm", Deployment: "glm-h200"}}
	q = Queue(ds, runs, running, 1, now, used, true)
	if q[0].State != Running || q[2].State != WaitsModel {
		t.Fatalf("running: %+v", q)
	}
	// A model with no recorded use, such as one without quotas: whether
	// the other sites can take its load cannot be told.
	if q = Queue(ds, runs, nil, 1, now, nil, true); q[0].State != WaitsHistory {
		t.Fatalf("no history: %+v", q[0])
	}
	// A prefill/decode LLMI on a hub that does not measure them neither
	// starts nor holds its model or site.
	ds[1].Topology = TopologyPrefillDecode
	if q = Queue(ds, runs, nil, 1, now, used, false); q[0].State != PeerOff || q[2].State != Next {
		t.Fatalf("peer off: %+v", q)
	}
}

// The campaign drains a site only when the model's other sites can take its
// busiest hour in the window, by their measurements, at up to 85% of what
// they serve.
func TestQueueCover(t *testing.T) {
	ds := []store.Deployment{deployment("a", "glm-b200", "s1", 2), deployment("b", "glm-h200", "s2", 1)}
	runs := []store.CapacityRun{run("a", "glm-b200", "s1", 1, time.Hour, 16)}
	// Site a serves 2 × (16,000 prompt + 10 output) tokens a second; at 85%
	// for an hour, 97,990,200 tokens.
	state := func(busiest float64) QueueItem {
		t.Helper()
		q := Queue(ds, runs, nil, 1, now, map[string]Load{"glm": {BusiestHour: busiest, History: true}}, true)
		if len(q) != 1 || q[0].Name != "glm-h200" {
			t.Fatalf("queue = %+v", q)
		}
		return q[0]
	}
	if i := state(0); i.State != Next || i.Cover != nil {
		t.Fatalf("no traffic in the window: %+v", i)
	}
	if i := state(50_000_000); i.State != Next || *i.Cover < 1.95 || *i.Cover > 1.96 {
		t.Fatalf("covered: %+v %v", i, *i.Cover)
	}
	if i := state(200_000_000); i.State != WaitsCover || *i.Cover < 0.48 || *i.Cover > 0.49 {
		t.Fatalf("not covered: %+v %v", i, *i.Cover)
	}
	// Priced, the same in credits: at $3 in, $0.30 cached, $15 out a
	// million, all prompt tokens uncached, 2 × (16,000 × 300,000 + 10 ×
	// 1,500,000) / 1e6 = 9,630 credits a second; 85% of an hour,
	// 29,467,800.
	prices := &capacity.Prices{Input: 300000, Cached: 30000, Output: 1500000}
	q := Queue(ds, runs, nil, 1, now, map[string]Load{"glm": {BusiestHour: 30_000_000, Prices: prices, History: true}}, true)
	if q[0].State != WaitsCover || *q[0].Cover < 0.982 || *q[0].Cover > 0.983 {
		t.Fatalf("priced: %+v %v", q[0], *q[0].Cover)
	}
	// A site that is drained takes nothing; neither does one not measured.
	ds[0].Drained = true
	if q := Queue(ds, runs, nil, 1, now, map[string]Load{"glm": {BusiestHour: 1, History: true}}, true); q[0].State != WaitsOperator {
		t.Fatalf("other site drained: %+v", q[0])
	}
	ds[0].Drained = false
	if q := Queue(ds, nil, nil, 1, now, map[string]Load{"glm": {BusiestHour: 1, History: true}}, true); q[0].State != WaitsCover || *q[0].Cover != 0 {
		t.Fatalf("other site not measured: %+v", q)
	}
}

func ready(n float64) *float64 { return &n }
