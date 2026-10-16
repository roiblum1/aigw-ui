package kube

import (
	"context"
	"slices"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
)

// A hub whose account may not list pods or nodes still counts the instances,
// and says what it was refused, so a shape without a GPU is not taken for a
// node without the label.
func TestGPUShapesRefused(t *testing.T) {
	lists := map[schema.GroupVersionResource]string{podsGVR: "PodList", nodesGVR: "NodeList"}
	for _, tc := range []struct {
		refuse []string
		want   []string
	}{
		{nil, nil},
		{[]string{"pods"}, []string{"pods"}},
		{[]string{"pods", "nodes"}, []string{"pods", "nodes"}},
	} {
		dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists)
		for _, r := range tc.refuse {
			dyn.PrependReactor("list", r, func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: r}, "", nil)
			})
		}
		g := (&Client{dyn: dyn}).gpuShapes(context.Background())
		if !slices.Equal(g.denied, tc.want) {
			t.Fatalf("refused %v: denied = %v", tc.refuse, g.denied)
		}
	}
}

// A deployment's market entry is read from its annotation.
func TestMarketIDAnnotation(t *testing.T) {
	lists := map[schema.GroupVersionResource]string{podsGVR: "PodList", nodesGVR: "NodeList"}
	for _, gvr := range llmServiceGVRs {
		lists[gvr] = "LLMInferenceServiceList"
	}
	for _, gvr := range llmConfigGVRs {
		lists[gvr] = "LLMInferenceServiceConfigList"
	}
	svc := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": llmServiceGVRs[0].Group + "/" + llmServiceGVRs[0].Version,
		"kind":       "LLMInferenceService",
		"metadata": map[string]any{"name": "glm-b300", "namespace": "llm",
			"annotations": map[string]any{MarketIDAnnotation: " z-ai/glm-5.3 "}},
		"spec": map[string]any{"model": map[string]any{"name": "glm"}},
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists, svc)
	got, err := (&Client{dyn: dyn}).Capacity(context.Background())
	if err != nil || len(got) != 1 || len(got[0].Deployments) != 1 || got[0].Deployments[0].MarketID != "z-ai/glm-5.3" {
		t.Fatalf("capacity = %+v, %v", got, err)
	}
}

// A hub whose account may not list the LLMInferenceServiceConfigs says so:
// its spec hash holds their names only.
func TestConfigsRefused(t *testing.T) {
	lists := map[schema.GroupVersionResource]string{podsGVR: "PodList", nodesGVR: "NodeList"}
	for _, gvr := range llmServiceGVRs {
		lists[gvr] = "LLMInferenceServiceList"
	}
	for _, gvr := range llmConfigGVRs {
		lists[gvr] = "LLMInferenceServiceConfigList"
	}
	svc := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": llmServiceGVRs[0].Group + "/" + llmServiceGVRs[0].Version,
		"kind":       "LLMInferenceService",
		"metadata":   map[string]any{"name": "glm-b300", "namespace": "llm"},
		"spec":       map[string]any{"model": map[string]any{"name": "glm"}},
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists, svc)
	dyn.PrependReactor("list", "llminferenceserviceconfigs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "llminferenceserviceconfigs"}, "", nil)
	})
	got, err := (&Client{dyn: dyn}).Capacity(context.Background())
	if err != nil || len(got) != 1 || !got[0].ConfigsDenied {
		t.Fatalf("capacity = %+v, %v", got, err)
	}
}

// The listeners KServe put a service's route on are read from its status, as
// KServe 0.21 reported them for glm53 on a serving site.
func TestRouterListeners(t *testing.T) {
	svc := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "glm53-b200", "namespace": "llm-glm53"},
		"status": map[string]any{"router": map[string]any{"gateways": []any{
			map[string]any{"group": "gateway.networking.k8s.io", "kind": "Gateway", "name": "envoy-ai-gateway",
				"namespace": "envoy-ai-gateway-system", "listeners": []any{"http"}},
			map[string]any{"name": "local", "listeners": []any{"peers", "admin"}},
		}}},
	}}
	got := routerListeners(svc)
	want := []string{"envoy-ai-gateway-system/envoy-ai-gateway/http", "llm-glm53/local/peers", "llm-glm53/local/admin"}
	if !slices.Equal(got, want) {
		t.Fatalf("%v", got)
	}
	if got := routerListeners(&unstructured.Unstructured{Object: map[string]any{}}); got != nil {
		t.Fatalf("no status: %v", got)
	}
}

// The ready count comes from the Deployment the service's status names, not
// from the copy in the status, which KServe leaves as it was when its
// reconcile fails; the desired count too, which an autoscaler sets there.
// Without access to the Deployment the copy is used. A stop annotation
// counts in any case, as KServe reads it.
func TestLiveWorkloadCounts(t *testing.T) {
	lists := map[schema.GroupVersionResource]string{podsGVR: "PodList", nodesGVR: "NodeList", deploymentsGVR: "DeploymentList"}
	for _, gvr := range llmServiceGVRs {
		lists[gvr] = "LLMInferenceServiceList"
	}
	for _, gvr := range llmConfigGVRs {
		lists[gvr] = "LLMInferenceServiceConfigList"
	}
	svc := func(annotations map[string]any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": llmServiceGVRs[0].Group + "/" + llmServiceGVRs[0].Version,
			"kind":       "LLMInferenceService",
			"metadata":   map[string]any{"name": "glm", "namespace": "llm", "annotations": annotations},
			"spec":       map[string]any{"model": map[string]any{"name": "glm"}, "scaling": map[string]any{"minReplicas": int64(1)}},
			"status": map[string]any{"workloads": map[string]any{"primary": map[string]any{
				"apiGroup": "apps", "kind": "Deployment", "name": "glm-kserve", "readyReplicas": int64(3)}}},
		}}
	}
	deploy := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "glm-kserve", "namespace": "llm"},
		"spec":     map[string]any{"replicas": int64(4)},
		"status":   map[string]any{"availableReplicas": int64(1)},
	}}
	read := func(objs ...runtime.Object) ModelCapacity {
		t.Helper()
		dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lists, objs...)
		got, err := (&Client{dyn: dyn}).Capacity(context.Background())
		if err != nil || len(got) != 1 {
			t.Fatalf("%v %v", got, err)
		}
		return got[0]
	}
	if got := read(svc(nil), deploy); got.Capacity != 1 || !strings.Contains(got.Detail, "1 of 4 ready") {
		t.Fatalf("from the Deployment: %v %q", got.Capacity, got.Detail)
	}
	if got := read(svc(nil)); got.Capacity != 3 {
		t.Fatalf("from the status: %v %q", got.Capacity, got.Detail)
	}
	stopped := svc(map[string]any{stopAnnotation: "True"})
	delete(stopped.Object["status"].(map[string]any), "workloads")
	if got := read(stopped); !got.Known || got.Capacity != 0 {
		t.Fatalf("stopped: %v %v %q", got.Known, got.Capacity, got.Detail)
	}
}
