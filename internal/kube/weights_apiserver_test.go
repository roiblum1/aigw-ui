package kube

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"aigw-ui/internal/weights"
)

// These tests need the CRDs in testdata/weights-crds.yaml on the test API
// server, besides what realClient asks for:
//
//	kubectl apply -f internal/kube/testdata/weights-crds.yaml

func applyAs(t *testing.T, c *Client, manager, resource, group, body string) {
	t.Helper()
	var obj unstructured.Unstructured
	if err := json.Unmarshal([]byte(body), &obj.Object); err != nil {
		t.Fatal(err)
	}
	gvr := trafficPolicyGVR
	if group == "serving.kserve.io" {
		gvr = llmServiceGVRs[0]
	}
	force := true
	res := c.dyn.Resource(gvr).Namespace(testNamespace)
	if resource == "status" {
		_, err := res.Patch(context.Background(), obj.GetName(), types.MergePatchType, []byte(body), metav1.PatchOptions{}, "status")
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := res.Patch(context.Background(), obj.GetName(), types.ApplyPatchType, []byte(body), metav1.PatchOptions{FieldManager: manager, Force: &force}); err != nil {
		t.Fatal(err)
	}
}

// The chart owns the policy and this tool owns only the zone weights: each
// can apply again without undoing the other, which is what lets Argo CD and
// this tool share the object.
func TestRealAPIServerZoneWeightsShareThePolicy(t *testing.T) {
	c := realClient(t)
	ctx := context.Background()
	res := c.dyn.Resource(trafficPolicyGVR).Namespace(testNamespace)
	res.Delete(ctx, "fleet-glm", metav1.DeleteOptions{})

	chart := `{"apiVersion":"gateway.envoyproxy.io/v1alpha1","kind":"BackendTrafficPolicy",
		"metadata":{"name":"fleet-glm","labels":{"aigw-ui.io/zone-weights":"true"},"annotations":{"aigw-ui.io/model":"glm-5.3"}},
		"spec":{"targetRefs":[{"kind":"HTTPRoute","name":"glm"}],
		        "loadBalancer":{"type":"ConsistentHash","consistentHash":{"type":"Headers","headers":[{"name":"x-claude-code-session-id"}]}},
		        "retry":{"numRetries":%RETRIES%}}}`
	applyAs(t, c, "argocd-controller", "", "", strings.Replace(chart, "%RETRIES%", "2", 1))

	want := map[string][]weights.Zone{"glm-5.3": {{Zone: "site1-a", Weight: 8}, {Zone: "site2-a", Weight: 3}}}
	changes, err := c.ApplyZoneWeights(ctx, want)
	if err != nil || len(changes) != 1 {
		t.Fatalf("first apply: %v %v", changes, err)
	}
	if changes, err := c.ApplyZoneWeights(ctx, want); err != nil || len(changes) != 0 {
		t.Errorf("an apply of the same weights reported %v %v", changes, err)
	}

	// The chart changes something of its own. The weights must survive.
	applyAs(t, c, "argocd-controller", "", "", strings.Replace(chart, "%RETRIES%", "3", 1))
	// A site loses instances and another is added.
	want["glm-5.3"] = []weights.Zone{{Zone: "site1-a", Weight: 6}, {Zone: "site2-a", Weight: 3}, {Zone: "site3-a", Weight: 2}}
	if _, err := c.ApplyZoneWeights(ctx, want); err != nil {
		t.Fatal(err)
	}
	// A site stops serving the model: its zone must go.
	want["glm-5.3"] = []weights.Zone{{Zone: "site1-a", Weight: 6}, {Zone: "site3-a", Weight: 2}}
	if _, err := c.ApplyZoneWeights(ctx, want); err != nil {
		t.Fatal(err)
	}

	live, err := res.Get(ctx, "fleet-glm", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !sameZones(live, want["glm-5.3"]) {
		zones, _, _ := unstructured.NestedSlice(live.Object, "spec", "loadBalancer", "zoneAware", "weightedZones")
		t.Errorf("weights on the cluster = %v, want %v", zones, want["glm-5.3"])
	}
	if lb, _, _ := unstructured.NestedString(live.Object, "spec", "loadBalancer", "type"); lb != "ConsistentHash" {
		t.Errorf("the chart's load balancer type is gone: %q", lb)
	}
	if retries, _, _ := unstructured.NestedInt64(live.Object, "spec", "retry", "numRetries"); retries != 3 {
		t.Errorf("the chart's retry setting = %d, want 3", retries)
	}
	if live.GetLabels()[ZoneWeightsLabel] != "true" || live.GetLabels()["app.kubernetes.io/managed-by"] != "" {
		t.Errorf("labels changed: %v", live.GetLabels())
	}
	// A policy without the label is never touched.
	other := strings.Replace(strings.Replace(chart, "%RETRIES%", "1", 1), `"labels":{"aigw-ui.io/zone-weights":"true"},`, "", 1)
	other = strings.Replace(other, `"name":"fleet-glm"`, `"name":"fleet-other"`, 1)
	applyAs(t, c, "argocd-controller", "", "", other)
	if _, err := c.ApplyZoneWeights(ctx, want); err != nil {
		t.Fatal(err)
	}
	live, _ = res.Get(ctx, "fleet-other", metav1.GetOptions{})
	if _, found, _ := unstructured.NestedSlice(live.Object, "spec", "loadBalancer", "zoneAware", "weightedZones"); found {
		t.Error("a policy without the label got zone weights")
	}
}

// Capacity is read from the fields KServe 0.21 fills in status.workloads.
func TestRealAPIServerCapacity(t *testing.T) {
	c := realClient(t)
	ctx := context.Background()
	res := c.dyn.Resource(llmServiceGVRs[0]).Namespace(testNamespace)
	for _, name := range []string{"single", "pd", "new", "stopped", "single-b"} {
		res.Delete(ctx, name, metav1.DeleteOptions{})
	}
	svc := func(name, annotations, spec, status string) {
		applyAs(t, c, "chart", "", "serving.kserve.io", `{"apiVersion":"serving.kserve.io/v1alpha2","kind":"LLMInferenceService",
			"metadata":{"name":"`+name+`","annotations":{`+annotations+`}},"spec":`+spec+`}`)
		if status != "" {
			applyAs(t, c, "", "status", "serving.kserve.io", `{"metadata":{"name":"`+name+`"},"status":{"workloads":`+status+`}}`)
		}
	}
	// Eight single-node instances, and one more deployment of the same model.
	svc("single", "", `{"model":{"name":"glm-5.3"},"replicas":8}`, `{"primary":{"kind":"Deployment","name":"single","readyReplicas":7}}`)
	svc("single-b", "", `{"model":{"name":"glm-5.3"}}`, `{"primary":{"kind":"Deployment","name":"single-b","readyReplicas":1}}`)
	// Multi-node prefill/decode: one instance, declared worth 2.6. Half its prefill is down.
	svc("pd", `"aigw-ui.io/capacity-per-instance":"2.6"`, `{"model":{"name":"minimax"},"replicas":2,"worker":{},"prefill":{"replicas":4,"worker":{}}}`,
		`{"primary":{"kind":"LeaderWorkerSet","name":"pd","readyReplicas":2},"prefill":{"kind":"LeaderWorkerSet","name":"pd-prefill","readyReplicas":2}}`)
	// Just created: no status yet. Unknown, not zero.
	svc("new", "", `{"replicas":2}`, "")
	svc("stopped", `"serving.kserve.io/stop":"true"`, `{"replicas":2}`, "")

	got, err := c.Capacity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		capacity float64
		known    bool
		step     float64
	}
	want := map[string]row{
		"glm-5.3": {8, true, 1},
		"minimax": {2.6, true, 2.6}, // 1 complete instance of 2
		"new":     {0, false, 1},
		"stopped": {0, true, 1},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d models, want %d: %+v", len(got), len(want), got)
	}
	for _, m := range got {
		if w := want[m.Model]; (row{m.Capacity, m.Known, m.Step}) != w {
			t.Errorf("%s: got %+v (%s), want %+v", m.Model, row{m.Capacity, m.Known, m.Step}, m.Detail, w)
		}
	}
}
