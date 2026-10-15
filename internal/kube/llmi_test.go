package kube

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// llmi reads the LLMInferenceService of the lab (testdata/llmi-single.json).
func llmi(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	data, err := os.ReadFile("testdata/llmi-single.json")
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatal(err)
	}
	return &unstructured.Unstructured{Object: obj}
}

// Scaling keeps the hash; any other change of the spec, such as one more
// argument, does not.
func TestSpecHash(t *testing.T) {
	base := llmi(t)
	want := SpecHash(base, nil, nil)
	if len(want) != 64 || SpecHash(base.DeepCopy(), nil, nil) != want {
		t.Fatalf("hash %q is not stable", want)
	}

	scaled := base.DeepCopy()
	unstructured.SetNestedField(scaled.Object, int64(3), "spec", "replicas")
	if SpecHash(scaled, nil, nil) != want {
		t.Fatal("scaling changed the hash")
	}
	scaled.SetGeneration(7)
	scaled.SetAnnotations(map[string]string{"aigw-ui.io/capacity-per-instance": "2"})
	if SpecHash(scaled, nil, nil) != want {
		t.Fatal("metadata changed the hash")
	}

	changed := base.DeepCopy()
	env, _, _ := unstructured.NestedSlice(changed.Object, "spec", "template", "containers")
	main := env[0].(map[string]any)
	vars := main["env"].([]any)
	first := vars[0].(map[string]any)
	first["value"] = first["value"].(string) + " --enable-chunked-prefill"
	unstructured.SetNestedSlice(changed.Object, env, "spec", "template", "containers")
	if SpecHash(changed, nil, nil) == want {
		t.Fatal("a new argument kept the hash")
	}
}

// A prefill/decode service hashes the ratio of its replicas, not their
// number.
func TestSpecHashPrefillDecode(t *testing.T) {
	pd := func(prefill, decode int64) string {
		svc := llmi(t)
		unstructured.SetNestedField(svc.Object, decode, "spec", "replicas")
		unstructured.SetNestedField(svc.Object, map[string]any{"replicas": prefill, "template": map[string]any{}}, "spec", "prefill")
		return SpecHash(svc, nil, nil)
	}
	if pd(1, 2) != pd(2, 4) {
		t.Fatal("the same ratio at another scale changed the hash")
	}
	if pd(1, 2) == pd(2, 3) {
		t.Fatal("another ratio kept the hash")
	}
}

// What the service runs outside its spec: the configs its baseRefs name and
// the images its model servers run. A change in either changes the hash.
func TestSpecHashBasesAndImages(t *testing.T) {
	svc := llmi(t)
	unstructured.SetNestedSlice(svc.Object, []any{map[string]any{"name": "vllm-0-29"}}, "spec", "baseRefs")
	base := func(args ...any) []Base {
		return []Base{{Name: "vllm-0-29", Spec: map[string]any{"template": map[string]any{"containers": []any{
			map[string]any{"name": "main", "args": args}}}}}}
	}
	images := []string{"=sha256:aaa"}
	want := SpecHash(svc, base("--max-num-seqs=256"), images)
	if SpecHash(svc, base("--max-num-seqs=256"), images) != want {
		t.Fatal("not stable")
	}
	if SpecHash(svc, base("--max-num-seqs=512"), images) == want {
		t.Fatal("a change in a base config kept the hash")
	}
	if SpecHash(svc, base("--max-num-seqs=256"), []string{"=sha256:bbb"}) == want {
		t.Fatal("another image kept the hash")
	}
	if SpecHash(svc, []Base{{Name: "vllm-0-29"}}, images) == want {
		t.Fatal("a base config that cannot be read hashed as the one that can")
	}

	// KServe takes a config from the service's namespace first, then from
	// its own; a cluster whose configs could not be listed has no hash.
	cfg := func(ns, arg string) unstructured.Unstructured {
		return unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "vllm-0-29", "namespace": ns},
			"spec": map[string]any{"arg": arg}}}
	}
	b := baseConfigs{items: []unstructured.Unstructured{cfg("kserve", "shared"), cfg(svc.GetNamespace(), "own")}}
	if got, ok := b.of(svc); !ok || len(got) != 1 || got[0].Spec["arg"] != "own" {
		t.Fatalf("own namespace: %+v %v", got, ok)
	}
	b.items = b.items[:1]
	if got, ok := b.of(svc); !ok || got[0].Spec["arg"] != "shared" {
		t.Fatalf("KServe's: %+v %v", got, ok)
	}
	if got, ok := (baseConfigs{denied: true}).of(svc); !ok || got[0].Spec != nil {
		t.Fatalf("denied: %+v %v", got, ok)
	}
	if _, ok := (baseConfigs{failed: true}).of(svc); ok {
		t.Fatal("a failed listing gave a hash")
	}
	if got, ok := (baseConfigs{failed: true}).of(llmi(t)); !ok || got != nil {
		t.Fatalf("no baseRefs: %+v %v", got, ok)
	}

	// KServe lists the configs it applied, its presets among them, with
	// their namespaces: a scheduler upgrade made in KServe's preset changes
	// the hash of a service that names no config at all.
	preset := func(name, arg string) unstructured.Unstructured {
		return unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": name, "namespace": "kserve"},
			"spec": map[string]any{"arg": arg}}}
	}
	plain := llmi(t)
	unstructured.SetNestedSlice(plain.Object, []any{
		map[string]any{"name": "kserve-config-llm-scheduler", "namespace": "kserve", "source": "Preset"},
		map[string]any{"name": "kserve-config-llm-template", "namespace": "kserve", "source": "Preset"}}, "status", "appliedConfigs")
	b = baseConfigs{items: []unstructured.Unstructured{preset("kserve-config-llm-template", "vllm"), preset("kserve-config-llm-scheduler", "epp:v1"),
		cfg(plain.GetNamespace(), "own")}}
	got, ok := b.of(plain)
	if !ok || len(got) != 2 || got[0].Name != "kserve/kserve-config-llm-scheduler" || got[0].Spec["arg"] != "epp:v1" || got[1].Spec["arg"] != "vllm" {
		t.Fatalf("applied: %+v %v", got, ok)
	}
	before := SpecHash(plain, got, images)
	b.items[1] = preset("kserve-config-llm-scheduler", "epp:v2")
	if after, _ := b.of(plain); SpecHash(plain, after, images) == before {
		t.Fatal("a preset's change kept the hash")
	}
	if _, ok := (baseConfigs{failed: true}).of(plain); ok {
		t.Fatal("a failed listing gave a hash with applied configs")
	}
}

// The images of a service's ready model servers, by role. During a rollout
// the pods of one role disagree, and there is no hash until they agree.
func TestImages(t *testing.T) {
	served := func(name, role, worker, image string, ready bool) unstructured.Unstructured {
		p := pod(name, "Running", "10.0.0.1", ready, role, nil)
		p.SetNamespace("llm")
		labels := p.GetLabels()
		labels["app.kubernetes.io/name"] = "glm"
		if worker != "" {
			labels["leaderworkerset.sigs.k8s.io/worker-index"] = worker
		}
		p.SetLabels(labels)
		p.Object["status"].(map[string]any)["containerStatuses"] = []any{
			map[string]any{"name": "storage-initializer", "imageID": "quay.io/x/init@sha256:init"},
			map[string]any{"name": "main", "imageID": "registry.example/vllm/vllm-openai@sha256:" + image}}
		return p
	}
	svc := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "glm", "namespace": "llm"}}}
	g := gpuShapes{listed: true, pods: []unstructured.Unstructured{
		served("glm-prefill-0", "prefill", "", "p1", true),
		served("glm-decode-0", "decode", "0", "d1", true),
		served("glm-decode-0-1", "decode", "1", "w1", true),
		served("glm-decode-1", "decode", "0", "d1", true),
		served("glm-decode-2", "decode", "0", "old", false), // not ready: left out
	}}
	if got, ok := g.images(svc); !ok || strings.Join(got, ",") != "decode/worker=sha256:w1,decode=sha256:d1,prefill=sha256:p1" {
		t.Fatalf("images = %v %v", got, ok)
	}
	// A rollout: two ready decode leaders on different images.
	g.pods = append(g.pods, served("glm-decode-3", "decode", "0", "d2", true))
	if got, ok := g.images(svc); ok {
		t.Fatalf("rollout = %v", got)
	}
	// No ready pod, or pods not listed this time: not known. Pods the hub
	// may not list: known to be without images.
	if _, ok := (gpuShapes{listed: true}).images(svc); ok {
		t.Fatal("no ready pod gave images")
	}
	if _, ok := (gpuShapes{}).images(svc); ok {
		t.Fatal("pods not listed gave images")
	}
	if got, ok := (gpuShapes{denied: []string{"pods"}}).images(svc); !ok || got != nil {
		t.Fatalf("denied = %v %v", got, ok)
	}
}

func TestServing(t *testing.T) {
	if q, lm := Serving(llmi(t)); q != "" || lm {
		t.Fatalf("lab: %q %v", q, lm)
	}
	svc := llmi(t)
	unstructured.SetNestedSlice(svc.Object, []any{map[string]any{"name": "main",
		"args": []any{"--quantization=modelopt_fp4", `--kv-transfer-config={"kv_connector":"LMCacheConnectorV1"}`}}},
		"spec", "template", "containers")
	if q, lm := Serving(svc); q != "modelopt_fp4" || !lm {
		t.Fatalf("args: %q %v", q, lm)
	}
	unstructured.SetNestedSlice(svc.Object, []any{map[string]any{"name": "main", "env": []any{
		map[string]any{"name": "VLLM_ADDITIONAL_ARGS", "value": "--max-model-len 8192 --quantization fp8"},
		map[string]any{"name": "LMCACHE_CHUNK_SIZE", "value": "256"}}}}, "spec", "template", "containers")
	if q, lm := Serving(svc); q != "fp8" || !lm {
		t.Fatalf("env: %q %v", q, lm)
	}
}
