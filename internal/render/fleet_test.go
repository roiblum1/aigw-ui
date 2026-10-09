package render

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func fleetState() State {
	return State{
		Namespace: "ai-gateway", GatewayName: "ai-gateway", ClientListener: "https", AuthEnabled: true,
		Fleet: FleetConfig{PeerSNI: "peers.llm.example.com", CAConfigMap: "llm-peer-ca", ClientSecret: "llm-peer-client", SessionHeaders: []string{"x-claude-code-session-id", "x-openwebui-chat-id"}},
		Models: []Model{
			{
				Name: "glm-5.3", Slug: "glm-5-3", DefaultLimit: 1000, DefaultWindow: "1d",
				// Given out of order, and with the backend the model had
				// before, which only other sites reach now.
				Fleet: []FleetSite{
					{Name: "site2-a", Host: "llm.site2-a.example.com", Port: 8443, Weight: 300},
					{Name: "site1-a", Host: "llm.site1-a.example.com", Port: 8443, Weight: 800},
				},
				Existing: []Target{{Namespace: "llms", Backend: "glm", Model: "glm-5.3", PeerOnly: true}},
				Quotas:   []TenantQuota{{TenantSlug: "team-a", Slot: 0, Limit: 500, Window: "1d"}},
			},
			{Name: "local", Slug: "local", Host: "vllm.llms.svc", Port: 8000, UpstreamModel: "local"},
		},
	}
}

func find(t *testing.T, objs []*unstructured.Unstructured, kind, name string) *unstructured.Unstructured {
	t.Helper()
	for _, o := range objs {
		if o.GetKind() == kind && o.GetName() == name {
			return o
		}
	}
	t.Fatalf("no %s %s in %v", kind, name, kinds(objs))
	return nil
}

func TestFleetObjects(t *testing.T) {
	objs := Objects(fleetState())

	backend := find(t, objs, "Backend", "fleet-glm-5-3")
	endpoints, _, _ := unstructured.NestedSlice(backend.Object, "spec", "endpoints")
	wantEndpoints := []any{
		map[string]any{"fqdn": map[string]any{"hostname": "llm.site1-a.example.com", "port": int64(8443)}, "zone": "site1-a"},
		map[string]any{"fqdn": map[string]any{"hostname": "llm.site2-a.example.com", "port": int64(8443)}, "zone": "site2-a"},
	}
	if !reflect.DeepEqual(endpoints, wantEndpoints) {
		t.Errorf("endpoints, sorted by site:\n got %v\nwant %v", endpoints, wantEndpoints)
	}
	if sni, _, _ := unstructured.NestedString(backend.Object, "spec", "tls", "sni"); sni != "peers.llm.example.com" {
		t.Errorf("sni = %q", sni)
	}

	policy := find(t, objs, "BackendTrafficPolicy", "fleet-glm-5-3")
	zones, _, _ := unstructured.NestedSlice(policy.Object, "spec", "loadBalancer", "zoneAware", "weightedZones")
	wantZones := []any{
		map[string]any{"zone": "site1-a", "weight": int64(800)},
		map[string]any{"zone": "site2-a", "weight": int64(300)},
	}
	if !reflect.DeepEqual(zones, wantZones) {
		t.Errorf("zones:\n got %v\nwant %v", zones, wantZones)
	}
	if n, _, _ := unstructured.NestedInt64(policy.Object, "spec", "retry", "numRetries"); n != 1 {
		t.Errorf("numRetries = %d, want one per other site", n)
	}
	if p, found, _ := unstructured.NestedInt64(policy.Object, "spec", "healthCheck", "panicThreshold"); !found || p != 0 {
		t.Errorf("panicThreshold = %d (set: %v), want 0", p, found)
	}
	if path, _, _ := unstructured.NestedString(policy.Object, "spec", "healthCheck", "active", "http", "path"); path != "/healthz/glm-5.3" {
		t.Errorf("health check path = %q", path)
	}
	targets, _, _ := unstructured.NestedSlice(policy.Object, "spec", "targetRefs")
	if target := targets[0].(map[string]any); target["kind"] != "HTTPRoute" || target["name"] != "fleet-glm-5-3" {
		t.Errorf("policy target = %v", target)
	}

	route := find(t, objs, "AIGatewayRoute", "fleet-glm-5-3")
	parents, _, _ := unstructured.NestedSlice(route.Object, "spec", "parentRefs")
	if parent := parents[0].(map[string]any); parent["sectionName"] != "https" {
		t.Errorf("route parent = %v, want the client listener", parent)
	}

	// Quotas attach to the entry backend alone, not to what the model had.
	for _, o := range objs {
		if o.GetKind() != "QuotaPolicy" {
			continue
		}
		refs, _, _ := unstructured.NestedSlice(o.Object, "spec", "targetRefs")
		if o.GetNamespace() != "ai-gateway" || len(refs) != 1 || refs[0].(map[string]any)["name"] != "fleet-glm-5-3" {
			t.Errorf("QuotaPolicy %s/%s targets %v", o.GetNamespace(), o.GetName(), refs)
		}
	}
	// The other model is rendered as before.
	find(t, objs, "Backend", "local")
	find(t, objs, "AIGatewayRoute", "local")
}

// Envoy rejects a locality weight below 1, and Envoy Gateway then stops
// publishing every change to that gateway. Whatever comes in, no object may
// carry one.
func TestNoZoneWeightBelowOne(t *testing.T) {
	s := fleetState()
	s.Models[0].Fleet = append(s.Models[0].Fleet,
		FleetSite{Name: "site3-a", Host: "llm.site3-a.example.com", Port: 8443, Weight: 0},
		FleetSite{Name: "site4-a", Host: "llm.site4-a.example.com", Port: 8443, Weight: -5},
		FleetSite{Name: "site5-a", Host: "llm.site5-a.example.com", Port: 8443, Weight: 1},
	)
	seen := 0
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			if zones, ok := v["weightedZones"].([]any); ok {
				for _, z := range zones {
					seen++
					if w, _ := z.(map[string]any)["weight"].(int64); w < 1 {
						t.Errorf("zone weight below 1: %v", z)
					}
				}
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	for _, o := range Objects(s) {
		walk(o.Object)
	}
	if seen != 5 {
		t.Errorf("checked %d zone weights, want 5", seen)
	}
}

// Every zone with an endpoint needs a weight, and the other way round: the
// gateway gives an endpoint's zone that is not listed a weight of 1.
func TestFleetZonesMatchEndpoints(t *testing.T) {
	objs := Objects(fleetState())
	endpoints, _, _ := unstructured.NestedSlice(find(t, objs, "Backend", "fleet-glm-5-3").Object, "spec", "endpoints")
	zones, _, _ := unstructured.NestedSlice(find(t, objs, "BackendTrafficPolicy", "fleet-glm-5-3").Object, "spec", "loadBalancer", "zoneAware", "weightedZones")
	if len(endpoints) != len(zones) {
		t.Fatalf("%d endpoints, %d zones", len(endpoints), len(zones))
	}
	for i := range endpoints {
		if endpoints[i].(map[string]any)["zone"] != zones[i].(map[string]any)["zone"] {
			t.Errorf("position %d: endpoint %v, zone %v", i, endpoints[i], zones[i])
		}
	}
}

func TestFleetRevision(t *testing.T) {
	a, b := fleetState(), fleetState()
	// The same fleet seen from another cluster: other keys, quotas and
	// site order do not matter.
	b.Models[0].Fleet[0], b.Models[0].Fleet[1] = b.Models[0].Fleet[1], b.Models[0].Fleet[0]
	b.Models[0].Quotas = nil
	b.Keys = []Key{{ClientID: "team-a.01", Value: "sk"}}
	if FleetRevision(a) == "" || FleetRevision(a) != FleetRevision(b) {
		t.Errorf("same fleet, revisions %q and %q", FleetRevision(a), FleetRevision(b))
	}
	b.Models[0].Fleet[0].Weight++
	if FleetRevision(a) == FleetRevision(b) {
		t.Error("a weight changed and the revision did not")
	}
	if got := FleetRevision(State{Models: []Model{{Name: "local"}}}); got != "" {
		t.Errorf("no entry route, revision %q", got)
	}
}

// A model whose entry route cannot be rendered keeps its entry objects on the
// cluster untouched. Its quotas are still rendered, on the entry backend
// that is there, so a lowered limit reaches the cluster while it is held.
func TestHeldModelKeepsItsQuotas(t *testing.T) {
	s := fleetState()
	s.Models[0].Fleet, s.Models[0].HeldReason = nil, "no setting"
	s.Models[0].Quotas[0].Limit = 7
	objs := Objects(s)
	for _, o := range objs {
		if o.GetName() == "fleet-glm-5-3" || o.GetName() == "fleet-glm-5-3-retry" {
			t.Errorf("rendered %s %s for a held model", o.GetKind(), o.GetName())
		}
	}
	policy := find(t, objs, "QuotaPolicy", "glm-5-3")
	refs, _, _ := unstructured.NestedSlice(policy.Object, "spec", "targetRefs")
	if len(refs) != 1 || refs[0].(map[string]any)["name"] != "fleet-glm-5-3" {
		t.Errorf("quota targets = %v", refs)
	}
	quotas, _, _ := unstructured.NestedSlice(policy.Object, "spec", "perModelQuotas")
	rules, _, _ := unstructured.NestedSlice(quotas[0].(map[string]any), "quota", "bucketRules")
	if limit, _, _ := unstructured.NestedInt64(rules[0].(map[string]any), "quota", "limit"); limit != 7 {
		t.Errorf("limit = %d, want the new one", limit)
	}
	find(t, objs, "Backend", "local")
	if held := HeldNames(s); !held["fleet-glm-5-3"] || !held["fleet-glm-5-3-retry"] || len(held) != 2 {
		t.Errorf("held = %v", held)
	}
}

func TestHealthPath(t *testing.T) {
	for model, want := range map[string]string{
		"glm-5.3":             "/healthz/glm-5.3",
		"zai-org/GLM-5.3-FP8": "/healthz/zai-org/GLM-5.3-FP8",
		"my model?v=1#a":      "/healthz/my%20model%3Fv=1%23a",
	} {
		if got := healthPath(model); got != want {
			t.Errorf("healthPath(%q) = %q, want %q", model, got, want)
		}
	}
}

// Envoy Gateway lets the gateway pick a site five times on a retry. With a
// hash that is too few, so the route of every entry route is patched, each
// by a policy of its own: one that cannot apply must not undo the others.
func TestFleetRetryPatch(t *testing.T) {
	s := fleetState()
	s.Models = append(s.Models, Model{Name: "qwen", Slug: "qwen", Fleet: s.Models[0].Fleet})
	objs := Objects(s)
	find(t, objs, "EnvoyPatchPolicy", "fleet-qwen-retry")
	patch := find(t, objs, "EnvoyPatchPolicy", "fleet-glm-5-3-retry")
	patches, _, _ := unstructured.NestedSlice(patch.Object, "spec", "jsonPatches")
	want := []any{map[string]any{
		"type": "type.googleapis.com/envoy.config.route.v3.RouteConfiguration",
		"name": "ai-gateway/ai-gateway/https",
		"operation": map[string]any{
			"op":       "add",
			"jsonPath": "..routes[?(@.name == 'httproute/ai-gateway/fleet-glm-5-3/rule/0/match/0/*')].route.retry_policy",
			"path":     "host_selection_retry_max_attempts",
			"value":    int64(20),
		},
	}}
	if !reflect.DeepEqual(patches, want) {
		t.Errorf("patches = %v\nwant      %v", patches, want)
	}

	// A held model keeps its patch on the cluster, and a model that is
	// switched on meanwhile still gets its own.
	s.Models[0].Fleet, s.Models[0].HeldReason = nil, "no site"
	objs = Objects(s)
	find(t, objs, "EnvoyPatchPolicy", "fleet-qwen-retry")
	for _, o := range objs {
		if o.GetName() == "fleet-glm-5-3-retry" {
			t.Error("rendered the patch of a held model")
		}
	}
	if held := HeldNames(s); !held["fleet-glm-5-3-retry"] || held["fleet-qwen-retry"] {
		t.Errorf("held = %v", held)
	}
}

// A request is hashed on whichever session header it carries.
func TestFleetSessionHeaders(t *testing.T) {
	policy := find(t, Objects(fleetState()), "BackendTrafficPolicy", "fleet-glm-5-3")
	headers, _, _ := unstructured.NestedSlice(policy.Object, "spec", "loadBalancer", "consistentHash", "headers")
	want := []any{map[string]any{"name": "x-claude-code-session-id"}, map[string]any{"name": "x-openwebui-chat-id"}}
	if !reflect.DeepEqual(headers, want) {
		t.Errorf("headers = %v", headers)
	}
}
