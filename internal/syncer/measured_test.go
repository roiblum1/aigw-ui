package syncer

import (
	"strings"
	"testing"

	"aigw-ui/internal/capacity"
	"aigw-ui/internal/store"
)

func TestMeasureCapacity(t *testing.T) {
	b300 := capacity.Shape{Topology: "single", GPU: "NVIDIA-B300", GPUsPerInstance: 8, Revision: "r1"}
	h200 := capacity.Shape{Topology: "single", GPU: "NVIDIA-H200", GPUsPerInstance: 8, Revision: "r1"}
	deployment := func(cluster, name string, s capacity.Shape, ready float64) store.Deployment {
		return store.Deployment{ClusterID: cluster, Namespace: "llm", Name: name, ModelID: "glm", Topology: s.Topology, GPU: s.GPU,
			GPUsPerInstance: s.GPUsPerInstance, Revision: s.Revision, Instances: &ready}
	}
	// A step of 100 s: 1,000,000 prompt tokens, none from the cache, and
	// 50,000 output tokens. Without prices an output token weighs 4, so an
	// instance serves 10,000 + 4 × 500 = 12,000 input-equivalent tokens a
	// second: 12 in thousands.
	run := func(s capacity.Shape, scale int64) store.CapacityRun {
		best := capacity.Step{Concurrency: 8, Requests: 100, DurationSeconds: 100, InputTokens: 1_000_000 * scale, OutputTokens: 50_000 * scale}
		return store.CapacityRun{ModelID: "glm", Shape: s, Result: capacity.Result{Best: &best}}
	}
	models := []store.Model{{ID: "glm", Name: "glm-5.3"}}
	report := func() []store.Capacity {
		return []store.Capacity{{Model: "glm-5.3", Capacity: 2, Step: 1, Known: true, Detail: "llm/glm-b300: 2 of 2 ready",
			Deployments: []store.Deployment{deployment("a", "glm-b300", b300, 2)}}}
	}
	other := []store.Deployment{deployment("b", "glm-h200", h200, 1)}

	// Both shapes measured: this site counts 2 × 12.
	got := report()
	measureCapacity("a", got, models, other, []store.CapacityRun{run(b300, 1), run(h200, 1)})
	if got[0].Basis != store.BasisMeasured || got[0].Capacity != 24 || got[0].Step != 12 || !strings.Contains(got[0].Detail, "llm/glm-b300: 2 × 12") {
		t.Fatalf("measured: %+v", got[0])
	}

	// The other site's shape is not measured: the annotation everywhere.
	got = report()
	measureCapacity("a", got, models, other, []store.CapacityRun{run(b300, 1)})
	if got[0].Basis != store.BasisDeclared || got[0].Capacity != 2 {
		t.Fatalf("one shape not measured: %+v", got[0])
	}

	// A deployment with nothing ready does not hold the model back.
	idle := []store.Deployment{deployment("b", "glm-h200", h200, 0)}
	got = report()
	measureCapacity("a", got, models, idle, []store.CapacityRun{run(b300, 1)})
	if got[0].Basis != store.BasisMeasured || got[0].Capacity != 24 {
		t.Fatalf("idle shape: %+v", got[0])
	}

	// A count not reported keeps the last weight, whatever it was in.
	got = report()
	got[0].Known = false
	measureCapacity("a", got, models, other, []store.CapacityRun{run(b300, 1), run(h200, 1)})
	if got[0].Basis != store.BasisDeclared || got[0].Capacity != 2 {
		t.Fatalf("unknown: %+v", got[0])
	}

	// A model this hub does not know, or served under another name here.
	unknown := []store.Capacity{{Model: "other", Capacity: 3, Known: true}}
	measureCapacity("a", unknown, models, nil, []store.CapacityRun{run(b300, 1)})
	if unknown[0].Basis != store.BasisDeclared || unknown[0].Capacity != 3 {
		t.Fatalf("unknown model: %+v", unknown[0])
	}
	routed := []store.Model{{ID: "glm", Name: "glm-5.3", Endpoints: []store.Endpoint{{ClusterID: "a", UpstreamModel: "zai/glm"}}}}
	got = report()
	got[0].Model = "zai/glm"
	measureCapacity("a", got, routed, other, []store.CapacityRun{run(b300, 1), run(h200, 1)})
	if got[0].Basis != store.BasisMeasured {
		t.Fatalf("by upstream name: %+v", got[0])
	}

	// A tiny instance is never below the annotation's lowest bound.
	got = report()
	measureCapacity("a", got, models, idle, []store.CapacityRun{{ModelID: "glm", Shape: b300, Result: capacity.Result{Best: &capacity.Step{DurationSeconds: 100, InputTokens: 10}}}})
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
