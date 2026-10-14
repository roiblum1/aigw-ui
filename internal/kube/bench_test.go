package kube

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func pod(name, phase, ip string, ready bool, role string, probe map[string]any) unstructured.Unstructured {
	status := "False"
	if ready {
		status = "True"
	}
	main := map[string]any{"name": "main", "ports": []any{map[string]any{"name": "http", "containerPort": int64(8080)}}}
	if probe != nil {
		main["readinessProbe"] = map[string]any{"httpGet": probe}
	}
	p := unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name, "labels": map[string]any{"llm-d.ai/role": role}},
		"spec":     map[string]any{"containers": []any{map[string]any{"name": "storage-initializer"}, main}},
		"status": map[string]any{"phase": phase, "podIP": ip,
			"conditions": []any{map[string]any{"type": "Ready", "status": status}}},
	}}
	return p
}

func TestReadyPods(t *testing.T) {
	got := readyPods([]unstructured.Unstructured{
		pod("b", "Running", "10.0.0.2", true, "both", nil),
		pod("a", "Running", "10.0.0.1", true, "decode", nil),
		pod("c", "Running", "10.0.0.3", false, "both", nil),
		pod("d", "Pending", "", false, "both", nil),
		pod("e", "Running", "10.0.0.5", true, "prefill", nil),
	})
	if len(got) != 2 || got[0].GetName() != "a" || got[1].GetName() != "b" {
		t.Fatalf("ready = %v", got)
	}
}

func TestServerURL(t *testing.T) {
	for _, tc := range []struct {
		probe map[string]any
		ip    string
		want  string
	}{
		{nil, "10.0.0.1", "http://10.0.0.1:8000"},
		{map[string]any{"path": "/health", "port": int64(8000), "scheme": "HTTP"}, "10.0.0.1", "http://10.0.0.1:8000"},
		{map[string]any{"path": "/health", "port": "http"}, "10.0.0.1", "http://10.0.0.1:8080"},
		{map[string]any{"path": "/health", "port": int64(9000)}, "fd00::1", "http://[fd00::1]:9000"},
	} {
		got, err := serverURL(pod("p", "Running", tc.ip, true, "both", tc.probe))
		if err != nil || got != tc.want {
			t.Errorf("%v: %q %v, want %q", tc.probe, got, err, tc.want)
		}
	}
	_, err := serverURL(pod("p", "Running", "10.0.0.1", true, "both", map[string]any{"port": int64(8000), "scheme": "HTTPS"}))
	if err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("https: %v", err)
	}
}

func TestShapeOfADeployment(t *testing.T) {
	gpuPod := func(name, node, role string, gpus any) unstructured.Unstructured {
		p := pod(name, "Running", "10.0.0.1", true, role, nil)
		p.SetNamespace("llm")
		labels := p.GetLabels()
		labels["app.kubernetes.io/name"] = "glm"
		p.SetLabels(labels)
		p.Object["spec"].(map[string]any)["nodeName"] = node
		c := p.Object["spec"].(map[string]any)["containers"].([]any)[1].(map[string]any)
		c["resources"] = map[string]any{"limits": map[string]any{"nvidia.com/gpu": gpus}}
		return p
	}
	g := gpuShapes{
		pods: []unstructured.Unstructured{
			gpuPod("glm-decode-0", "n1", "decode", int64(8)),
			gpuPod("glm-decode-1", "n2", "decode", "8"),
			gpuPod("glm-prefill-0", "n3", "prefill", int64(8)),
			gpuPod("glm-prefill-1", "n4", "prefill", int64(8)),
		},
		products: map[string]string{"n1": "NVIDIA-H200", "n2": "NVIDIA-H200", "n3": "NVIDIA-H200", "n4": "NVIDIA-B200"},
	}
	svc := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "glm", "namespace": "llm"},
		"spec":     map[string]any{"prefill": map[string]any{"replicas": int64(2)}},
	}}
	// Two prefill and decode instances: 32 GPUs, 16 each, mostly H200.
	d := g.of(svc, 2, true)
	if d.Topology != TopologyPrefillDecode || d.GPU != "NVIDIA-H200" || d.GPUsPerInstance != 16 || d.Instances != 2 {
		t.Fatalf("shape = %+v", d)
	}
	single := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "other", "namespace": "llm"}, "spec": map[string]any{}}}
	if d := g.of(single, 0, false); d.Topology != TopologySingle || d.GPU != "" || d.GPUsPerInstance != 0 {
		t.Fatalf("no pods = %+v", d)
	}
	multi := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "x", "namespace": "llm"}, "spec": map[string]any{"worker": map[string]any{}}}}
	if d := g.of(multi, 1, true); d.Topology != TopologyMultiNode {
		t.Fatalf("multi-node = %+v", d)
	}
}
