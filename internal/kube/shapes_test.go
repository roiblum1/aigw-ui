package kube

import (
	"context"
	"slices"
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
