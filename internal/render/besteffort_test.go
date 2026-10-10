package render

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func bestEffortState(overage ...string) State {
	s := fleetState()
	s.Models[0].BestEffort = true
	s.Models[0].Overage = overage
	return s
}

func has(objs []*unstructured.Unstructured, kind, name string) bool {
	for _, o := range objs {
		if o.GetKind() == kind && o.GetName() == name {
			return true
		}
	}
	return false
}

// A model that refuses a spent budget renders what it rendered before
// best-effort existed.
func TestRefuseModeRendersNothingNew(t *testing.T) {
	for _, o := range Objects(fleetState()) {
		if strings.HasPrefix(o.GetName(), "fleetbe-") {
			t.Errorf("rendered %s %s for a model in refuse mode", o.GetKind(), o.GetName())
		}
	}
	route := find(t, Objects(fleetState()), "AIGatewayRoute", "fleet-glm-5-3")
	rules, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
	ref := rules[0].(map[string]any)["backendRefs"].([]any)[0].(map[string]any)
	if _, set := ref["headerMutation"]; set {
		t.Errorf("the entry route sets a class in refuse mode: %v", ref)
	}
}

// With nobody past its budget there is no route to take requests, but the
// backend and its counting policy are there for the first tenant.
func TestBestEffortWithNobodyInOverage(t *testing.T) {
	objs := Objects(bestEffortState())
	for _, kind := range []string{"AIGatewayRoute", "BackendTrafficPolicy"} {
		if has(objs, kind, "fleetbe-glm-5-3") {
			t.Errorf("rendered %s fleetbe-glm-5-3 with nobody in overage", kind)
		}
	}
	if has(objs, "EnvoyPatchPolicy", "fleetbe-glm-5-3-retry") {
		t.Error("rendered the retry patch of a route that is not there: it would not be programmed")
	}
	backend := find(t, objs, "AIServiceBackend", "fleetbe-glm-5-3")
	set, _, _ := unstructured.NestedSlice(backend.Object, "spec", "headerMutation", "set")
	want := []any{map[string]any{"name": "x-llm-d-inference-objective", "value": "best-effort"}}
	if !reflect.DeepEqual(set, want) {
		t.Errorf("class of the best-effort backend = %v", set)
	}
	if name, _, _ := unstructured.NestedString(backend.Object, "spec", "backendRef", "name"); name != "fleet-glm-5-3" {
		t.Errorf("the best-effort backend sends to %q, want the model's sites", name)
	}
	find(t, objs, "QuotaPolicy", "fleetbe-glm-5-3")

	// The model's own route now names its class too.
	route := find(t, objs, "AIGatewayRoute", "fleet-glm-5-3")
	rules, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
	ref := rules[0].(map[string]any)["backendRefs"].([]any)[0].(map[string]any)
	wantRef := map[string]any{"set": []any{map[string]any{"name": "x-llm-d-inference-objective", "value": "standard"}}}
	if !reflect.DeepEqual(ref["headerMutation"], wantRef) {
		t.Errorf("class of the entry route = %v", ref["headerMutation"])
	}
}

func TestBestEffortRoute(t *testing.T) {
	// Given out of order: every cluster must render the same pattern.
	objs := Objects(bestEffortState("team.b", "team-a"))

	route := find(t, objs, "AIGatewayRoute", "fleetbe-glm-5-3")
	rules, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
	rule := rules[0].(map[string]any)
	wantMatches := []any{map[string]any{"headers": []any{
		map[string]any{"type": "Exact", "name": "x-ai-eg-model", "value": "glm-5.3"},
		map[string]any{"type": "RegularExpression", "name": "x-aigw-client-id", "value": `^(team-a|team\.b)\.[a-f0-9]+$`},
	}}}
	if !reflect.DeepEqual(rule["matches"], wantMatches) {
		t.Errorf("matches:\n got %v\nwant %v", rule["matches"], wantMatches)
	}
	wantRefs := []any{map[string]any{"name": "fleetbe-glm-5-3", "modelNameOverride": "glm-5.3"}}
	if !reflect.DeepEqual(rule["backendRefs"], wantRefs) {
		t.Errorf("backendRefs = %v", rule["backendRefs"])
	}
	parents, _, _ := unstructured.NestedSlice(route.Object, "spec", "parentRefs")
	if parents[0].(map[string]any)["sectionName"] != "https" {
		t.Errorf("parentRefs = %v, want the client listener", parents)
	}

	// The same routing as the model's own route, and a 429 also goes on to
	// the next site.
	own := find(t, objs, "BackendTrafficPolicy", "fleet-glm-5-3")
	be := find(t, objs, "BackendTrafficPolicy", "fleetbe-glm-5-3")
	codes, _, _ := unstructured.NestedSlice(be.Object, "spec", "retry", "retryOn", "httpStatusCodes")
	if !reflect.DeepEqual(codes, []any{int64(503), int64(429)}) {
		t.Errorf("best-effort retries on %v", codes)
	}
	ownCodes, _, _ := unstructured.NestedSlice(own.Object, "spec", "retry", "retryOn", "httpStatusCodes")
	if !reflect.DeepEqual(ownCodes, []any{int64(503)}) {
		t.Errorf("the entry route retries on %v", ownCodes)
	}
	targets, _, _ := unstructured.NestedSlice(be.Object, "spec", "targetRefs")
	if targets[0].(map[string]any)["name"] != "fleetbe-glm-5-3" {
		t.Errorf("policy targets %v", targets)
	}
	for _, field := range []string{"loadBalancer", "healthCheck", "circuitBreaker", "timeout"} {
		a, _, _ := unstructured.NestedMap(own.Object, "spec", field)
		b, _, _ := unstructured.NestedMap(be.Object, "spec", field)
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%s differs between the two routes:\n own %v\n  be %v", field, a, b)
		}
	}

	patch := find(t, objs, "EnvoyPatchPolicy", "fleetbe-glm-5-3-retry")
	patches, _, _ := unstructured.NestedSlice(patch.Object, "spec", "jsonPatches")
	path, _, _ := unstructured.NestedString(patches[0].(map[string]any), "operation", "jsonPath")
	if want := "..routes[?(@.name == 'httproute/ai-gateway/fleetbe-glm-5-3/rule/0/match/0/*')].route.retry_policy"; path != want {
		t.Errorf("patch selects %s", path)
	}
}

// The best-effort route counts every tenant at the position it has on the
// model's own route and refuses nobody.
func TestBestEffortQuotaPolicy(t *testing.T) {
	s := bestEffortState("team-a")
	s.Models[0].Quotas = []TenantQuota{
		{TenantSlug: "team-c", Slot: 2, Limit: 9, Window: "1h"},
		{TenantSlug: "team-a", Slot: 0, Limit: 500, Window: "1d"},
	}
	objs := Objects(s)
	rulesOf := func(name string) ([]any, map[string]any) {
		policy := find(t, objs, "QuotaPolicy", name)
		quotas, _, _ := unstructured.NestedSlice(policy.Object, "spec", "perModelQuotas")
		quota := quotas[0].(map[string]any)["quota"].(map[string]any)
		return quota["bucketRules"].([]any), quota["defaultBucket"].(map[string]any)
	}
	own, _ := rulesOf("glm-5-3")
	be, pool := rulesOf("fleetbe-glm-5-3")
	if len(be) != len(own) {
		t.Fatalf("%d rules on the best-effort route, %d on the model's own", len(be), len(own))
	}
	for i := range own {
		a, b := own[i].(map[string]any), be[i].(map[string]any)
		if !reflect.DeepEqual(a["clientSelectors"], b["clientSelectors"]) || !reflect.DeepEqual(a["quota"], b["quota"]) {
			t.Errorf("rule %d differs: %v and %v", i, a, b)
		}
		// Position 1 is a placeholder, which matches no request.
		if shadow, _ := b["shadowMode"].(bool); !shadow && i != 1 {
			t.Errorf("rule %d of the best-effort route can refuse: %v", i, b)
		}
	}
	if pool["limit"] != serviceQuotaLimit {
		t.Errorf("default bucket of the best-effort route = %v, want one that never runs out", pool)
	}
	policy := find(t, objs, "QuotaPolicy", "fleetbe-glm-5-3")
	refs, _, _ := unstructured.NestedSlice(policy.Object, "spec", "targetRefs")
	if len(refs) != 1 || refs[0].(map[string]any)["name"] != "fleetbe-glm-5-3" {
		t.Errorf("targets = %v", refs)
	}
	// The model's own policy is not attached to the best-effort backend:
	// it would refuse there.
	ownPolicy := find(t, objs, "QuotaPolicy", "glm-5-3")
	refs, _, _ = unstructured.NestedSlice(ownPolicy.Object, "spec", "targetRefs")
	for _, ref := range refs {
		if ref.(map[string]any)["name"] == "fleetbe-glm-5-3" {
			t.Error("the model's own policy targets the best-effort backend")
		}
	}
}

func TestBestEffortCounters(t *testing.T) {
	var own, be []Counter
	for _, c := range Counters(bestEffortState("team-a")) {
		if c.ModelSlug != "glm-5-3" || c.Pool {
			continue
		}
		if c.Overage {
			be = append(be, c)
		} else {
			own = append(own, c)
		}
	}
	if len(own) != 1 || len(be) != 1 {
		t.Fatalf("counters: %d on the model's own route, %d on the best-effort route", len(own), len(be))
	}
	if be[0].Backend != "ai-gateway/fleetbe-glm-5-3" || !be[0].Shadow || be[0].Limit != 500 {
		t.Errorf("best-effort counter = %+v", be[0])
	}
	// Same rule, other backend.
	if want := strings.Replace(own[0].stem, "fleet-glm-5-3_", "fleetbe-glm-5-3_", 1); be[0].stem != want {
		t.Errorf("stem:\n got %s\nwant %s", be[0].stem, want)
	}
}

func TestOverageTenantsAreCapped(t *testing.T) {
	var many []string
	for i := range MaxOverageTenants + 5 {
		many = append(many, "t"+string(rune('a'+i%26))+strings.Repeat("x", i/26))
	}
	m := Model{Overage: many}
	if got := m.OverageTenants(); len(got) != MaxOverageTenants {
		t.Errorf("%d tenants listed, want %d", len(got), MaxOverageTenants)
	}
}

// Every fleet cluster must take the same tenants off the entry route, so
// the set is part of the revision. A fleet without best-effort keeps the
// revision it had.
func TestFleetRevisionCoversOverage(t *testing.T) {
	plain := FleetRevision(fleetState())
	on := FleetRevision(bestEffortState())
	a := FleetRevision(bestEffortState("team-a"))
	b := FleetRevision(bestEffortState("team-a", "team-b"))
	if plain == on || on == a || a == b {
		t.Errorf("revisions do not tell the states apart: %s %s %s %s", plain, on, a, b)
	}
	if FleetRevision(bestEffortState("team-b", "team-a")) != b {
		t.Error("the revision depends on the order the tenants are given in")
	}
}

// A new tenant rule has to change the model's routes, and the routes have to
// be applied before the QuotaPolicy: see quotaRevision.
func TestQuotaRevisionOnRoutes(t *testing.T) {
	revision := func(s State, name string) string {
		return find(t, Objects(s), "AIGatewayRoute", name).GetAnnotations()[QuotaRevisionAnnotation]
	}
	before := bestEffortState("team-a")
	after := bestEffortState("team-a")
	after.Models[0].Quotas = append(after.Models[0].Quotas, TenantQuota{TenantSlug: "team-b", Slot: 1, Limit: 5, Window: "1h"})
	for _, name := range []string{"fleet-glm-5-3", "fleetbe-glm-5-3"} {
		a, b := revision(before, name), revision(after, name)
		if a == "" || a == b {
			t.Errorf("%s: revision %q before and %q after a tenant got a quota", name, a, b)
		}
	}
	if revision(before, "fleet-glm-5-3") != revision(bestEffortState("team-a", "team-b"), "fleet-glm-5-3") {
		t.Error("the revision changed although no quota did")
	}
	if revision(before, "local") == "" {
		t.Error("a model added by hand has no revision on its route")
	}

	position := map[string]int{}
	for i, o := range Objects(after) {
		position[o.GetKind()+"/"+o.GetName()] = i
	}
	for route, policy := range map[string]string{
		"AIGatewayRoute/fleet-glm-5-3":   "QuotaPolicy/glm-5-3",
		"AIGatewayRoute/fleetbe-glm-5-3": "QuotaPolicy/fleetbe-glm-5-3",
	} {
		if position[route] > position[policy] {
			t.Errorf("%s is applied after %s", route, policy)
		}
	}
}

// A model with one site gets no zone weights: there is nothing to weigh, and
// with them the gateway enforces no quota on the route.
func TestOneSiteHasNoZoneWeights(t *testing.T) {
	s := bestEffortState("team-a")
	s.Models[0].Fleet = s.Models[0].Fleet[:1]
	objs := Objects(s)
	for _, name := range []string{"fleet-glm-5-3", "fleetbe-glm-5-3"} {
		policy := find(t, objs, "BackendTrafficPolicy", name)
		if _, found, _ := unstructured.NestedMap(policy.Object, "spec", "loadBalancer", "zoneAware"); found {
			t.Errorf("%s has zone weights for a single site", name)
		}
		if kind, _, _ := unstructured.NestedString(policy.Object, "spec", "loadBalancer", "type"); kind != "ConsistentHash" {
			t.Errorf("%s: load balancer type %q", name, kind)
		}
	}
	two := find(t, Objects(bestEffortState("team-a")), "BackendTrafficPolicy", "fleet-glm-5-3")
	if zones, _, _ := unstructured.NestedSlice(two.Object, "spec", "loadBalancer", "zoneAware", "weightedZones"); len(zones) != 2 {
		t.Errorf("zones for two sites = %v", zones)
	}
}

// The route lists a limited number of tenants. The ones given first, which
// are those past their budget, keep their place.
func TestOverageTenantsKeepsTheFirst(t *testing.T) {
	m := Model{Overage: []string{"zeta-spent"}}
	for i := 0; i < MaxOverageTenants+50; i++ {
		m.Overage = append(m.Overage, fmt.Sprintf("a-no-quota-%03d", i))
	}
	got := m.OverageTenants()
	if len(got) != MaxOverageTenants || got[len(got)-1] != "zeta-spent" {
		t.Errorf("%d tenants, last %q: want %d, sorted, with the first one given kept", len(got), got[len(got)-1], MaxOverageTenants)
	}
}

// A model in best-effort mode next to a model whose name ends in "-be": no
// two objects of one kind may share a name, or each sync overwrites one
// model's objects with the other's.
func TestBestEffortNamesDoNotCollide(t *testing.T) {
	s := bestEffortState("team-a")
	other := s.Models[0]
	other.Name, other.Slug = "glm-5.3-be", "glm-5-3-be"
	other.BestEffort, other.Overage = false, nil
	s.Models = append(s.Models, other)

	seen := map[string]bool{}
	for _, o := range Objects(s) {
		id := o.GetKind() + "/" + o.GetNamespace() + "/" + o.GetName()
		if seen[id] {
			t.Errorf("%s is rendered twice", id)
		}
		seen[id] = true
	}
}
