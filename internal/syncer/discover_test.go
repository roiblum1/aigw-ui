package syncer

import (
	"reflect"
	"testing"

	"aigw-ui/internal/kube"
)

// A model the gateway lists is added only when a route of the cluster's own
// serves it, with or without a backend a quota can attach to. One that only
// the gateway's list has, such as a model deleted by hand whose route the
// gateway still lists, updates a model the hub has and adds none.
func TestGatewayModels(t *testing.T) {
	routes := []kube.DiscoveredModel{
		{Name: "qwen", Backends: []kube.ModelBackend{{Name: "qwen", Namespace: "llm"}}},
		{Name: "pool-only"},
	}
	got := gatewayModels([]string{"qwen", "pool-only", "e2e-alias"}, routes)
	want := []kube.DiscoveredModel{
		{Name: "qwen", Backends: []kube.ModelBackend{{Name: "qwen", Namespace: "llm"}}},
		{Name: "pool-only"},
		{Name: "e2e-alias", OnlyIfKnown: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}
