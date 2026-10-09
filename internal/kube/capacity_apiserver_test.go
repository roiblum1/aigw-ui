package kube

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// This test needs the LLMInferenceService CRD on the test API server;
// hack/test-apiserver.sh installs it.

// Capacity is read from the fields KServe 0.21 fills in status.workloads.
func TestRealAPIServerCapacity(t *testing.T) {
	c, ns := realClient(t)
	ctx := context.Background()
	res := c.dyn.Resource(llmServiceGVRs[0]).Namespace(ns)
	// Capacity reads every namespace, so the model names carry this test's
	// namespace to tell them from anything else on the server.
	model := func(name string) string { return name + "." + ns }
	svc := func(name, annotations, spec, status string) {
		t.Helper()
		body := `{"apiVersion":"serving.kserve.io/v1alpha2","kind":"LLMInferenceService",
			"metadata":{"name":"` + name + `","annotations":{` + annotations + `}},"spec":` + spec + `}`
		var obj unstructured.Unstructured
		if err := json.Unmarshal([]byte(body), &obj.Object); err != nil {
			t.Fatal(err)
		}
		if _, err := res.Create(ctx, &obj, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		if status == "" {
			return
		}
		patch := `{"status":{"workloads":` + status + `}}`
		if _, err := res.Patch(ctx, name, types.MergePatchType, []byte(patch), metav1.PatchOptions{}, "status"); err != nil {
			t.Fatal(err)
		}
	}
	// Eight single-node instances, and one more deployment of the same model.
	svc("single", "", `{"model":{"name":"`+model("glm")+`"},"replicas":8}`, `{"primary":{"kind":"Deployment","name":"single","readyReplicas":7}}`)
	svc("single-b", `"aigw-ui.io/model-revision":"r1","aigw-ui.io/max-model-len":"262144"`, `{"model":{"name":"`+model("glm")+`"}}`,
		`{"primary":{"kind":"Deployment","name":"single-b","readyReplicas":1}}`)
	// Multi-node prefill/decode: one instance, declared worth 2.6. Half its prefill is down.
	svc("pd", `"aigw-ui.io/capacity-per-instance":"2.6"`, `{"model":{"name":"`+model("minimax")+`"},"replicas":2,"worker":{},"prefill":{"replicas":4,"worker":{}}}`,
		`{"primary":{"kind":"LeaderWorkerSet","name":"pd","readyReplicas":2},"prefill":{"kind":"LeaderWorkerSet","name":"pd-prefill","readyReplicas":2}}`)
	// The older annotation name is read when the newer one is not set.
	svc("older", `"aigw-ui.io/capacity-per-replica":"2"`, `{"model":{"name":"`+model("older")+`"}}`, `{"primary":{"kind":"Deployment","name":"older","readyReplicas":3}}`)
	// Just created: no status yet. Unknown, not zero.
	svc("new", "", `{"model":{"name":"`+model("new")+`"},"replicas":2}`, "")
	// A canary is left out. A new deployment next to a running one adds
	// nothing and does not make the model unknown.
	svc("canary", `"aigw-ui.io/ignore":"true"`, `{"model":{"name":"`+model("mixed")+`"}}`, `{"primary":{"kind":"Deployment","name":"canary","readyReplicas":5}}`)
	svc("mixed-old", "", `{"model":{"name":"`+model("mixed")+`"}}`, `{"primary":{"kind":"Deployment","name":"mixed-old","readyReplicas":0}}`)
	svc("mixed-new", "", `{"model":{"name":"`+model("mixed")+`"}}`, "")
	// A declared capacity that is no usable number counts as 1.
	svc("inf", `"aigw-ui.io/capacity-per-instance":"Inf"`, `{"model":{"name":"`+model("inf")+`"}}`, `{"primary":{"kind":"Deployment","name":"inf","readyReplicas":2}}`)
	svc("huge", `"aigw-ui.io/capacity-per-instance":"1e12"`, `{"model":{"name":"`+model("huge")+`"}}`, `{"primary":{"kind":"Deployment","name":"huge","readyReplicas":2}}`)
	svc("stopped", `"serving.kserve.io/stop":"true"`, `{"model":{"name":"`+model("stopped")+`"},"replicas":2}`, "")

	all, err := c.Capacity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		capacity float64
		known    bool
		step     float64
	}
	want := map[string]row{
		model("glm"):     {8, true, 1},
		model("minimax"): {2.6, true, 2.6}, // 1 complete instance of 2
		model("older"):   {6, true, 2},
		model("new"):     {0, false, 1},
		model("stopped"): {0, true, 1},
		model("mixed"):   {0, true, 1},
		model("inf"):     {2, true, 1},
		model("huge"):    {2, true, 1},
	}
	seen := 0
	for _, m := range all {
		if !strings.HasSuffix(m.Model, "."+ns) {
			continue
		}
		seen++
		if w := want[m.Model]; (row{m.Capacity, m.Known, m.Step}) != w {
			t.Errorf("%s: got %+v (%s), want %+v", m.Model, row{m.Capacity, m.Known, m.Step}, m.Detail, w)
		}
		if m.Model == model("glm") && (m.Revision != "r1" || m.MaxModelLen != "262144") {
			t.Errorf("%s: revision %q, max-model-len %q", m.Model, m.Revision, m.MaxModelLen)
		}
	}
	if seen != len(want) {
		t.Errorf("got %d of this test's models, want %d: %+v", seen, len(want), all)
	}
}
