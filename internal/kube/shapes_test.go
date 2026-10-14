package kube

import (
	"context"
	"slices"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
