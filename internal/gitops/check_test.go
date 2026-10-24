package gitops

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

// Values the chart or KServe's launcher would refuse are refused before a
// commit: a missing name, namespace or weights, an object name that is no
// DNS label, an argument that is not one plain token or --option='value'
// (it goes through bash eval), a flag the chart sets itself, and
// VLLM_ADDITIONAL_ARGS.
func TestCheckValues(t *testing.T) {
	ok := func() map[string]any {
		return map[string]any{
			"namespace": "llm-glm53",
			"model":     map[string]any{"name": "glm-5.3", "subPath": "GLM-5.3"},
			"llmisvc": map[string]any{"name": "glm53-b200", "env": map[string]any{"OMP_NUM_THREADS": "16"},
				"args": []any{"--speculative-config.method=mtp", "--max-model-len=262144", `--default-chat-template-kwargs='{"enable_thinking": false}'`}},
		}
	}
	if err := CheckValues(ok()); err != nil {
		t.Fatal(err)
	}
	for want, change := range map[string]func(v map[string]any){
		"model.name is required": func(v map[string]any) { v["model"].(map[string]any)["name"] = "" },
		"namespace is required":  func(v map[string]any) { delete(v, "namespace") },
		"model.subPath":          func(v map[string]any) { delete(v["model"].(map[string]any), "subPath") },
		"must start with a letter": func(v map[string]any) {
			delete(v["llmisvc"].(map[string]any), "name")
			v["model"].(map[string]any)["name"] = "5.3"
		},
		"not one plain token": func(v map[string]any) { v["llmisvc"].(map[string]any)["args"] = []any{"--x=$(id)"} },
		"set by the chart":    func(v map[string]any) { v["llmisvc"].(map[string]any)["args"] = []any{"--kv-transfer-config"} },
		"in one place":        func(v map[string]any) { v["model"].(map[string]any)["maxModelLen"] = 8192.0 },
		"must be a list":      func(v map[string]any) { v["llmisvc"].(map[string]any)["args"] = "--a --b" },
		"VLLM_ADDITIONAL_ARGS": func(v map[string]any) {
			v["llmisvc"].(map[string]any)["env"] = map[string]any{"VLLM_ADDITIONAL_ARGS": "x"}
		},
		"is not a variable name": func(v map[string]any) { v["llmisvc"].(map[string]any)["env"] = map[string]any{"A-B": "x"} },
	} {
		v := ok()
		change(v)
		if err := CheckValues(v); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}
}

// A new model's name must make an Application name Argo CD takes, and its
// namespace must not be the platform's or the gateway's.
func TestCheckModelName(t *testing.T) {
	if err := CheckModelName("qwen-small", "llm-qwen-small", "envoy-ai-gateway-system"); err != nil {
		t.Fatal(err)
	}
	for want, place := range map[string][2]string{
		"DNS label":      {"Qwen", "llm"},
		"too long":       {strings.Repeat("a", 40), "llm"},
		"namespace must": {"q", "LLM"},
		"platform":       {"q", "openshift-gitops"},
		"gateway's":      {"q", "envoy-ai-gateway-system"},
	} {
		if err := CheckModelName(place[0], place[1], "envoy-ai-gateway-system"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}
	if got := AppName(DefaultAppName, "lab", "qwen-tiny"); got != "popai-lab-qwen-tiny-deploy" {
		t.Fatal(got)
	}
}

// Argo CD's view of each Application, by name: sync, health, the values'
// commit of a multi-source Application, and an error condition before the
// last sync's message.
func TestArgoCDApps(t *testing.T) {
	app := func(name string, status map[string]any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
			"metadata": map[string]any{"name": name, "namespace": "openshift-gitops"},
			"status":   status,
		}}
	}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{applicationsGVR: "ApplicationList"},
		app("popai-lab-qwen-tiny-deploy", map[string]any{
			"sync":           map[string]any{"status": "Synced", "revisions": []any{"aaa", "bbb"}},
			"health":         map[string]any{"status": "Healthy"},
			"operationState": map[string]any{"phase": "Succeeded", "message": "successfully synced"},
		}),
		app("popai-lab-bad-deploy", map[string]any{
			"sync":           map[string]any{"status": "Unknown"},
			"conditions":     []any{map[string]any{"type": "ComparisonError", "message": "values don't meet the specifications of the schema"}},
			"operationState": map[string]any{"phase": "Failed", "message": "one or more synchronization tasks are not valid"},
		}))
	apps, err := newArgoCD(client, "openshift-gitops").Apps(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := apps["popai-lab-qwen-tiny-deploy"]; got != (App{Name: "popai-lab-qwen-tiny-deploy", Sync: "Synced", Health: "Healthy", Revision: "bbb", Operation: "Succeeded", Message: "successfully synced"}) {
		t.Fatalf("qwen-tiny: %+v", got)
	}
	if got := apps["popai-lab-bad-deploy"]; got.Message != "ComparisonError: values don't meet the specifications of the schema" || got.Operation != "Failed" {
		t.Fatalf("bad: %+v", got)
	}
}
