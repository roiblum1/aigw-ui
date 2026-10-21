package kube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/rest"
)

// lengthServer stands in for the API server's pod proxy: each pod's model
// server says its length, or the proxy refuses (0). It counts the questions
// by path.
func lengthServer(t *testing.T, lengths map[string]int) (*httptest.Server, func(string) int) {
	t.Helper()
	var mu sync.Mutex
	asked := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /api/v1/namespaces/llm/pods/http:glm-0:8000/proxy/v1/models
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) != 10 || parts[7] != "proxy" || parts[8] != "v1" || parts[9] != "models" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		asked[parts[6]]++
		mu.Unlock()
		n, ok := lengths[parts[6]]
		if !ok || n == 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403}`))
			return
		}
		w.Write([]byte(`{"object":"list","data":[{"id":"glm","object":"model","max_model_len":` + strconv.Itoa(n) + `}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv, func(pod string) int {
		mu.Lock()
		defer mu.Unlock()
		return asked[pod]
	}
}

// workloadPod is a workload pod of an LLMInferenceService in namespace llm.
// scheme and port are its readiness check's; "" leaves the check out.
func workloadPod(name, svc, role string, ready bool, scheme string, port int64) *unstructured.Unstructured {
	labels := map[string]any{"app.kubernetes.io/name": svc, "kserve.io/component": "workload"}
	if role != "" {
		labels["llm-d.ai/role"] = role
	}
	main := map[string]any{"name": "main"}
	if scheme != "" {
		main["readinessProbe"] = map[string]any{"httpGet": map[string]any{"scheme": scheme, "port": port, "path": "/health"}}
	}
	status := "False"
	if ready {
		status = "True"
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": "llm", "uid": "uid-" + name, "labels": labels},
		"spec":     map[string]any{"containers": []any{main}},
		"status": map[string]any{"phase": "Running", "podIP": "10.0.0.1",
			"conditions": []any{map[string]any{"type": "Ready", "status": status}}},
	}}
}

func llmis(name string, annotations map[string]any) *unstructured.Unstructured {
	meta := map[string]any{"name": name, "namespace": "llm"}
	if annotations != nil {
		meta["annotations"] = annotations
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": llmServiceGVRs[0].Group + "/" + llmServiceGVRs[0].Version,
		"kind":       "LLMInferenceService",
		"metadata":   meta,
		"spec":       map[string]any{"model": map[string]any{"name": name}},
	}}
}

// A deployment's length is what one of its ready servers says, through the
// pod proxy at the scheme and port of its readiness check; a prefill pod and
// one that is not ready are not asked. A pod is asked once while it lives.
// One that does not answer is asked again after lengthRetry, and on each
// look only one pod of a deployment is asked. A pod that is gone is
// forgotten.
func TestServedLength(t *testing.T) {
	srv, asked := lengthServer(t, map[string]int{"http:glm-0:8000": 32768, "https:tls-0:8443": 65536})
	c, err := newFromConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pods := func(ps ...*unstructured.Unstructured) gpuShapes {
		g := gpuShapes{listed: true}
		for _, p := range ps {
			g.pods = append(g.pods, *p)
		}
		return g
	}
	ctx, now := context.Background(), time.Now()
	g := pods(
		workloadPod("glm-p", "glm", "prefill", true, "HTTP", 8000),
		workloadPod("glm-x", "glm", "decode", false, "HTTP", 8000),
		workloadPod("glm-0", "glm", "decode", true, "HTTP", 8000),
		workloadPod("tls-0", "tls", "", true, "HTTPS", 8443),
		workloadPod("den-0", "den", "", true, "", 0),
		workloadPod("den-1", "den", "", true, "", 0),
	)
	for range 2 {
		if n := c.servedLength(ctx, g, llmis("glm", nil), now); n != 32768 {
			t.Fatalf("glm: %d", n)
		}
	}
	if asked("http:glm-0:8000") != 1 || asked("http:glm-p:8000") != 0 || asked("http:glm-x:8000") != 0 {
		t.Fatalf("asked glm-0 %d, glm-p %d, glm-x %d times", asked("http:glm-0:8000"), asked("http:glm-p:8000"), asked("http:glm-x:8000"))
	}
	if n := c.servedLength(ctx, g, llmis("tls", nil), now); n != 65536 {
		t.Fatalf("tls: %d", n)
	}

	// Refused: one pod a look, each again only after lengthRetry.
	for i, at := range []time.Duration{0, time.Minute, 2 * time.Minute, lengthRetry + time.Second} {
		if n := c.servedLength(ctx, g, llmis("den", nil), now.Add(at)); n != 0 {
			t.Fatalf("den: %d", n)
		}
		want := [][2]int{{1, 0}, {1, 1}, {1, 1}, {2, 1}}[i]
		if got := [2]int{asked("http:den-0:8000"), asked("http:den-1:8000")}; got != want {
			t.Fatalf("look %d: asked den-0 and den-1 %v times, want %v", i, got, want)
		}
	}

	// glm-0 is gone and glm-9 took its place: it is asked.
	c.forgetLengths(pods(workloadPod("glm-9", "glm", "", true, "HTTP", 8000)))
	if _, kept := c.lengths["uid-glm-0"]; kept {
		t.Fatal("a pod that is gone is remembered")
	}
	if n := c.servedLength(ctx, pods(workloadPod("glm-9", "glm", "", true, "HTTP", 8000)), llmis("glm", nil), now); n != 0 || asked("http:glm-9:8000") != 1 {
		t.Fatalf("glm-9: %d, asked %d times", n, asked("http:glm-9:8000"))
	}
}

// Discovery gives a model's length from its servers when its deployment
// declares none, and the declared one otherwise, without asking.
func TestCapacityLength(t *testing.T) {
	srv, asked := lengthServer(t, map[string]int{"http:glm-0:8000": 32768, "http:qwen-0:8000": 40960})
	c, err := newFromConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	lists := map[schema.GroupVersionResource]string{podsGVR: "PodList", nodesGVR: "NodeList"}
	for _, gvr := range llmServiceGVRs {
		lists[gvr] = "LLMInferenceServiceList"
	}
	for _, gvr := range llmConfigGVRs {
		lists[gvr] = "LLMInferenceServiceConfigList"
	}
	c.dyn = dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists,
		llmis("glm", nil), llmis("qwen", map[string]any{MaxModelLenAnnotation: "131072"}),
		workloadPod("glm-0", "glm", "", true, "HTTP", 8000), workloadPod("qwen-0", "qwen", "", true, "HTTP", 8000))
	got, err := c.Capacity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	lengths := map[string]string{}
	for _, mc := range got {
		lengths[mc.Model] = mc.MaxModelLen
	}
	if lengths["glm"] != "32768" || lengths["qwen"] != "131072" || asked("http:qwen-0:8000") != 0 {
		t.Fatalf("lengths %v, asked qwen-0 %d times", lengths, asked("http:qwen-0:8000"))
	}
}
