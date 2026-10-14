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
