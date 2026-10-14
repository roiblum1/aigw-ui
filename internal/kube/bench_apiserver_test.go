package kube

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"aigw-ui/internal/capacity"
)

// A measurement finds a ready pod of a model, and its Job is created, read
// and deleted as the API server takes them. There is no kubelet here, so the
// pods' status is written by the test and no log is read.
func TestRealAPIServerCapacityJob(t *testing.T) {
	c, ns := realClient(t)
	ctx := context.Background()
	model := "bench." + ns

	create := func(gvrIndex int, body string) {
		t.Helper()
		var obj unstructured.Unstructured
		if err := json.Unmarshal([]byte(body), &obj.Object); err != nil {
			t.Fatal(err)
		}
		res := c.dyn.Resource(llmServiceGVRs[0]).Namespace(ns)
		if gvrIndex == 1 {
			res = c.dyn.Resource(podsGVR).Namespace(ns)
		}
		if _, err := res.Create(ctx, &obj, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	// Pods need a service account, which no controller makes here.
	sa := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ServiceAccount", "metadata": map[string]any{"name": "default"}}}
	if _, err := c.dyn.Resource(schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}).Namespace(ns).Create(ctx, sa, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	create(0, `{"apiVersion":"serving.kserve.io/v1alpha2","kind":"LLMInferenceService",
		"metadata":{"name":"bench","annotations":{"aigw-ui.io/max-model-len":"8192"}},"spec":{"model":{"name":"`+model+`"}}}`)
	pod := func(name, ip string, ready bool, extra string) {
		t.Helper()
		create(1, `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"`+name+`","labels":{"app.kubernetes.io/name":"bench","kserve.io/component":"workload"`+extra+`}},
			"spec":{"containers":[{"name":"main","image":"vllm","readinessProbe":{"httpGet":{"path":"/health","port":8000,"scheme":"HTTP"}}}]}}`)
		status := "False"
		if ready {
			status = "True"
		}
		patch := `{"status":{"phase":"Running","podIP":"` + ip + `","podIPs":[{"ip":"` + ip + `"}],"conditions":[{"type":"Ready","status":"` + status + `"}]}}`
		if _, err := c.dyn.Resource(podsGVR).Namespace(ns).Patch(ctx, name, types.MergePatchType, []byte(patch), metav1.PatchOptions{}, "status"); err != nil {
			t.Fatal(err)
		}
	}
	pod("bench-b", "10.0.0.8", true, "")
	pod("bench-a", "10.0.0.7", false, "")
	pod("bench-c", "10.0.0.9", true, `,"llm-d.ai/role":"prefill"`)

	inst, err := c.FindInstance(ctx, []string{"other", model}, "")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Namespace != ns || inst.Pod != "bench-b" || inst.URL != "http://10.0.0.8:8000" || inst.Model != model || inst.MaxModelLen != 8192 {
		t.Fatalf("instance = %+v", inst)
	}
	if _, err := c.FindInstance(ctx, []string{model}, "bench-a"); err == nil || !strings.Contains(err.Error(), "no ready pod called bench-a") {
		t.Fatalf("a pod that is not ready: %v", err)
	}
	if _, err := c.FindInstance(ctx, []string{"missing." + ns}, ""); err == nil || !strings.Contains(err.Error(), "no LLMInferenceService") {
		t.Fatalf("a model nobody serves: %v", err)
	}

	set := capacity.Defaults(8192)
	job, err := capacity.Job(capacity.JobSpec{RunID: "run-1", Name: "aigw-capacity-test", Namespace: ns, Image: "registry/aiperf:0.13.0", URL: inst.URL, Model: inst.Model, Settings: set})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if s, err := c.GetJobState(ctx, ns, "aigw-capacity-test"); err != nil || s.Gone || s.Succeeded || s.Failed {
		t.Fatalf("new job: %+v %v", s, err)
	}
	patch := `{"status":{"startTime":"2026-10-14T10:00:00Z","conditions":[{"type":"FailureTarget","status":"True","reason":"DeadlineExceeded","message":"Job was active longer than specified deadline"},{"type":"Failed","status":"True","reason":"DeadlineExceeded","message":"Job was active longer than specified deadline"}],"failed":1}}`
	if _, err := c.dyn.Resource(jobsGVR).Namespace(ns).Patch(ctx, "aigw-capacity-test", types.MergePatchType, []byte(patch), metav1.PatchOptions{}, "status"); err != nil {
		t.Fatal(err)
	}
	if s, err := c.GetJobState(ctx, ns, "aigw-capacity-test"); err != nil || !s.Failed || !strings.Contains(s.Reason, "DeadlineExceeded") {
		t.Fatalf("failed job: %+v %v", s, err)
	}

	// The pod of a run that waits for an image it cannot pull.
	pod("aigw-capacity-test-x", "10.0.0.10", false, `,"`+capacity.RunLabel+`":"run-1"`)
	waiting := `{"status":{"containerStatuses":[{"name":"aiperf","image":"registry/aiperf:0.13.0","imageID":"","ready":false,"restartCount":0,"state":{"waiting":{"reason":"ImagePullBackOff","message":"Back-off pulling image"}}}]}}`
	if _, err := c.dyn.Resource(podsGVR).Namespace(ns).Patch(ctx, "aigw-capacity-test-x", types.MergePatchType, []byte(waiting), metav1.PatchOptions{}, "status"); err != nil {
		t.Fatal(err)
	}
	if p := c.PodProblem(ctx, ns, capacity.RunLabel+"=run-1"); !strings.HasPrefix(p, "ImagePullBackOff") {
		t.Fatalf("problem = %q", p)
	}

	if err := c.DeleteJob(ctx, ns, "aigw-capacity-test"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteJob(ctx, ns, "aigw-capacity-test"); err != nil {
		t.Fatalf("deleting a Job that is gone: %v", err)
	}
	if s, err := c.GetJobState(ctx, ns, "aigw-capacity-test"); err != nil || !s.Gone {
		t.Fatalf("deleted job: %+v %v", s, err)
	}
}
