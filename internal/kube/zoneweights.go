package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"aigw-ui/internal/weights"
)

const (
	// ZoneWeightsLabel marks a BackendTrafficPolicy whose zone weights this
	// tool sets, and ZoneWeightsModel, an annotation on it, names the model.
	// The policy itself belongs to whoever created it: this tool writes one
	// field of it and never deletes it.
	ZoneWeightsLabel = "aigw-ui.io/zone-weights"
	ZoneWeightsModel = "aigw-ui.io/model"

	// weightsManager owns spec.loadBalancer.zoneAware.weightedZones and
	// nothing else, so a GitOps tool can be told to ignore exactly that.
	weightsManager = "aigw-ui-weights"
)

var trafficPolicyGVR = schema.GroupVersionResource{Group: "gateway.envoyproxy.io", Version: "v1alpha1", Resource: "backendtrafficpolicies"}

// ApplyZoneWeights writes the zone weights of each model into the
// BackendTrafficPolicies that ask for them, in any namespace, and returns the
// policies it changed. A model without weights is left as it is.
func (c *Client) ApplyZoneWeights(ctx context.Context, byModel map[string][]weights.Zone) ([]Change, error) {
	list, err := c.dyn.Resource(trafficPolicyGVR).List(ctx, metav1.ListOptions{LabelSelector: ZoneWeightsLabel + "=true"})
	if missingKind(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list BackendTrafficPolicies: %w", describe(err))
	}
	var changes []Change
	for i := range list.Items {
		policy := &list.Items[i]
		zones, ok := byModel[policy.GetAnnotations()[ZoneWeightsModel]]
		if !ok || len(zones) == 0 || sameZones(policy, zones) {
			continue
		}
		weighted := make([]any, 0, len(zones))
		for _, z := range zones {
			weighted = append(weighted, map[string]any{"zone": z.Zone, "weight": z.Weight})
		}
		patch, err := json.Marshal(map[string]any{
			"apiVersion": policy.GetAPIVersion(),
			"kind":       policy.GetKind(),
			"metadata":   map[string]any{"name": policy.GetName(), "namespace": policy.GetNamespace()},
			"spec":       map[string]any{"loadBalancer": map[string]any{"zoneAware": map[string]any{"weightedZones": weighted}}},
		})
		if err != nil {
			return changes, err
		}
		force := true
		_, err = c.dyn.Resource(trafficPolicyGVR).Namespace(policy.GetNamespace()).Patch(ctx, policy.GetName(), types.ApplyPatchType, patch,
			metav1.PatchOptions{FieldManager: weightsManager, Force: &force})
		if err != nil {
			return changes, fmt.Errorf("set zone weights on BackendTrafficPolicy %s/%s: %w", policy.GetNamespace(), policy.GetName(), describe(err))
		}
		changes = append(changes, Change{Kind: "BackendTrafficPolicy", Namespace: policy.GetNamespace(), Name: policy.GetName(), Action: "updated"})
	}
	sort.Slice(changes, func(i, j int) bool {
		return changes[i].Namespace+"/"+changes[i].Name < changes[j].Namespace+"/"+changes[j].Name
	})
	return changes, nil
}

// sameZones reports whether the policy already carries exactly these weights.
func sameZones(policy *unstructured.Unstructured, zones []weights.Zone) bool {
	live, _, _ := unstructured.NestedSlice(policy.Object, "spec", "loadBalancer", "zoneAware", "weightedZones")
	if len(live) != len(zones) {
		return false
	}
	have := make(map[string]int64, len(live))
	for _, item := range live {
		entry, _ := item.(map[string]any)
		zone, _ := entry["zone"].(string)
		weight, _, _ := unstructured.NestedInt64(entry, "weight")
		have[zone] = weight
	}
	for _, z := range zones {
		if w, ok := have[z.Zone]; !ok || w != z.Weight {
			return false
		}
	}
	return true
}
