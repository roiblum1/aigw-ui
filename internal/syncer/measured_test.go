package syncer

import (
	"strings"
	"testing"
	"time"

	"aigw-ui/internal/capacity"
	"aigw-ui/internal/store"
)

func TestMeasureCapacity(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	deployment := func(cluster, name, spec string, ready float64) store.Deployment {
		return store.Deployment{ClusterID: cluster, Namespace: "llm", Name: name, ModelID: "glm", Topology: "single", SpecHash: spec, Instances: &ready}
	}
	// A step of 100 s: 1,000,000 prompt tokens, none from the cache, and
	// 50,000 output tokens. Without prices an output token weighs 4, so an
	// instance serves 10,000 + 4 × 500 = 12,000 input-equivalent tokens a
	// second: 12 in thousands.
	run := func(cluster, name, spec string, scale int64) store.CapacityRun {
		set := capacity.Defaults(0)
		best := capacity.Step{Concurrency: 8, Requests: 100, DurationSeconds: 100, InputTokens: 1_000_000 * scale, OutputTokens: 50_000 * scale}
		v := 1
		return store.CapacityRun{ModelID: "glm", ClusterID: cluster, Namespace: "llm", Deployment: name, SpecHash: spec, ProfileVersion: &v,
			Settings: set, InstancesDuring: 1, CreatedAt: now.Add(-time.Hour), Result: capacity.Analyze(set, []capacity.Step{best})}
	}
	models := []store.Model{{ID: "glm", Name: "glm-5.3"}}
	report := func(spec string) []store.Capacity {
		return []store.Capacity{{Model: "glm-5.3", Capacity: 2, Step: 1, Known: true, Detail: "llm/glm-b300: 2 of 2 ready",
			Deployments: []store.Deployment{deployment("", "glm-b300", spec, 2)}}}
	}
	other := []store.Deployment{deployment("b", "glm-h200", "h1", 1)}
	both := []store.CapacityRun{run("a", "glm-b300", "b1", 1), run("b", "glm-h200", "h1", 1)}

	// Both LLMIs measured: this site counts 2 × 12.
	got := report("b1")
	measureCapacity("a", got, models, other, both, 1, now)
	if got[0].Basis != store.BasisMeasured || got[0].Capacity != 24 || got[0].Step != 12 || !strings.Contains(got[0].Detail, "llm/glm-b300: 2 × 12") {
		t.Fatalf("measured: %+v", got[0])
	}

	// The other site's LLMI is not measured: the annotation everywhere.
	got = report("b1")
	measureCapacity("a", got, models, other, both[:1], 1, now)
	if got[0].Basis != store.BasisDeclared || got[0].Capacity != 2 {
		t.Fatalf("one not measured: %+v", got[0])
	}

	// This LLMI's spec changed since it was measured: its result keeps
	// counting, stale, so tuning a model does not switch its weights back
	// to the annotation and move sessions between sites.
	got = report("b2")
	measureCapacity("a", got, models, other, both, 1, now)
	if got[0].Basis != store.BasisMeasured || got[0].Capacity != 24 {
		t.Fatalf("spec changed: %+v", got[0])
	}
	// What it runs cannot be told now, as during a rollout: the hash
	// stored counts, and without one, the annotation.
	got = report("")
	measureCapacity("a", got, models, append(other, deployment("a", "glm-b300", "b1", 2)), both, 1, now)
	if got[0].Basis != store.BasisMeasured || got[0].Capacity != 24 {
		t.Fatalf("rollout: %+v", got[0])
	}
	got = report("")
	measureCapacity("a", got, models, other, both, 1, now)
	if got[0].Basis != store.BasisDeclared {
		t.Fatalf("no hash: %+v", got[0])
	}

	// A deployment with nothing ready does not hold the model back.
	idle := []store.Deployment{deployment("b", "glm-h200", "h1", 0)}
	got = report("b1")
	measureCapacity("a", got, models, idle, both[:1], 1, now)
	if got[0].Basis != store.BasisMeasured || got[0].Capacity != 24 {
		t.Fatalf("idle: %+v", got[0])
	}

	// A count not reported keeps the last weight, whatever it was in.
	got = report("b1")
	got[0].Known = false
	measureCapacity("a", got, models, other, both, 1, now)
	if got[0].Basis != store.BasisDeclared || got[0].Capacity != 2 {
		t.Fatalf("unknown: %+v", got[0])
	}

	// A model this hub does not know, or served under another name here.
	unknown := []store.Capacity{{Model: "other", Capacity: 3, Known: true}}
	measureCapacity("a", unknown, models, nil, both, 1, now)
	if unknown[0].Basis != store.BasisDeclared || unknown[0].Capacity != 3 {
		t.Fatalf("unknown model: %+v", unknown[0])
	}
	routed := []store.Model{{ID: "glm", Name: "glm-5.3", Endpoints: []store.Endpoint{{ClusterID: "a", UpstreamModel: "zai/glm"}}}}
	got = report("b1")
	got[0].Model = "zai/glm"
	measureCapacity("a", got, routed, other, both, 1, now)
	if got[0].Basis != store.BasisMeasured {
		t.Fatalf("by upstream name: %+v", got[0])
	}

	// A tiny instance is never below the annotation's lowest bound.
	tiny := run("a", "glm-b300", "b1", 1)
	tinyBest := capacity.Step{Concurrency: 1, Requests: 10, DurationSeconds: 100, InputTokens: 10}
	tiny.Result = capacity.Analyze(tiny.Settings, []capacity.Step{tinyBest})
	got = report("b1")
	measureCapacity("a", got, models, idle, []store.CapacityRun{tiny}, 1, now)
	if got[0].Step != minPerInstance {
		t.Fatalf("tiny: %+v", got[0])
	}
}

// The detail keeps a small instance's figure: 0.0272, not 0.03.
func TestThreeFigures(t *testing.T) {
	for v, want := range map[float64]string{0.027158921: "0.0272", 12: "12", 12.345: "12.3", 1234.5: "1230", 0.01: "0.01"} {
		if got := threeFigures(v); got != want {
			t.Errorf("%v: %s, want %s", v, got, want)
		}
	}
}
