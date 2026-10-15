package kube

import (
	"encoding/json"
	"os"
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
	want := SpecHash(base)
	if len(want) != 64 || SpecHash(base.DeepCopy()) != want {
		t.Fatalf("hash %q is not stable", want)
	}

	scaled := base.DeepCopy()
	unstructured.SetNestedField(scaled.Object, int64(3), "spec", "replicas")
	if SpecHash(scaled) != want {
		t.Fatal("scaling changed the hash")
	}
	scaled.SetGeneration(7)
	scaled.SetAnnotations(map[string]string{"aigw-ui.io/capacity-per-instance": "2"})
	if SpecHash(scaled) != want {
		t.Fatal("metadata changed the hash")
	}

	changed := base.DeepCopy()
	env, _, _ := unstructured.NestedSlice(changed.Object, "spec", "template", "containers")
	main := env[0].(map[string]any)
	vars := main["env"].([]any)
	first := vars[0].(map[string]any)
	first["value"] = first["value"].(string) + " --enable-chunked-prefill"
	unstructured.SetNestedSlice(changed.Object, env, "spec", "template", "containers")
	if SpecHash(changed) == want {
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
		return SpecHash(svc)
	}
	if pd(1, 2) != pd(2, 4) {
		t.Fatal("the same ratio at another scale changed the hash")
	}
	if pd(1, 2) == pd(2, 3) {
		t.Fatal("another ratio kept the hash")
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
