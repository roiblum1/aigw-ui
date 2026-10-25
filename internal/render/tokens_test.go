package render

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// While the hub reads the counters and the gateway takes tokens only, the
// budget policy counts each tenant's requests and tokens by model, in four
// shadow rules after the budget rule, which never refuse, then each key's in
// four more (0.37.0); without a budget the policy is there for them alone,
// its budget rule first all the same.
func TestTokenRules(t *testing.T) {
	s := budgetState()
	s.Budgets, s.TokenCounts = nil, true
	p := find(t, Objects(s), "BackendTrafficPolicy", BudgetPolicyName)
	if p.GetAnnotations()[TokenCountsAnnotation] != "true" {
		t.Fatalf("annotations: %v", p.GetAnnotations())
	}
	rules, _, _ := unstructured.NestedSlice(p.Object, "spec", "rateLimit", "global", "rules")
	if len(rules) != 9 {
		t.Fatalf("%d rules, want the budget rule, four token rules and four key rules", len(rules))
	}
	if limit, _, _ := unstructured.NestedMap(rules[0].(map[string]any), "limit", "fromMetadata"); limit == nil {
		t.Fatalf("rule 0 is not the budget rule: %v", rules[0])
	}
	for i, kind := range slices.Concat(TokenKinds, TokenKinds) {
		r := rules[i+1].(map[string]any)
		if r["shadowMode"] != true || r["shared"] != true {
			t.Errorf("%s: %v", kind, r)
		}
		by := "x-aigw-tenant"
		if i >= len(TokenKinds) {
			by = "x-aigw-client-id"
		}
		headers := fmt.Sprint(r["clientSelectors"])
		if headers != "[map[headers:[map[name:"+by+" type:Distinct] map[name:x-ai-eg-model type:Distinct]]]]" {
			t.Errorf("rule %d (%s) selects %s", i+1, kind, headers)
		}
		cost := fmt.Sprint(r["cost"])
		want := map[TokenKind]string{
			TokenRequests: "map[request:map[from:Number number:1]]",
			TokenInput:    "map[request:map[from:Number number:0] response:map[from:Metadata metadata:map[key:aigw_ui_input namespace:io.envoy.ai_gateway]]]",
			TokenCached:   "map[request:map[from:Number number:0] response:map[from:Metadata metadata:map[key:aigw_ui_cached namespace:io.envoy.ai_gateway]]]",
			TokenOutput:   "map[request:map[from:Number number:0] response:map[from:Metadata metadata:map[key:aigw_ui_output namespace:io.envoy.ai_gateway]]]",
		}[kind]
		if cost != want {
			t.Errorf("%s costs %s, want %s", kind, cost, want)
		}
	}

	// The budget counters keep their names beside the token rules.
	s.Budgets = budgetState().Budgets
	if got := BudgetCounters(s)[0].RedisKey("", time.Unix(0, 0)); got != BudgetCounters(budgetState())[0].RedisKey("", time.Unix(0, 0)) {
		t.Fatalf("budget counter moved: %s", got)
	}
}

// Without the hub reading counters, or while the gateway takes API keys,
// nothing counts tokens: no policy without a budget, and no token keys on
// the routes.
func TestNoTokenRules(t *testing.T) {
	for name, s := range map[string]State{
		"hub without counters": func() State { s := budgetState(); s.Budgets = nil; return s }(),
		"API keys": func() State {
			s := budgetState()
			s.Budgets, s.TokenCounts = nil, true
			s.Identity.Mode = IdentityBoth
			return s
		}(),
	} {
		objs := Objects(s)
		if has(objs, "BackendTrafficPolicy", BudgetPolicyName) {
			t.Errorf("%s: budget policy rendered", name)
		}
		costs, _, _ := unstructured.NestedSlice(find(t, objs, "AIGatewayRoute", "opus").Object, "spec", "llmRequestCosts")
		if len(costs) != 1 {
			t.Errorf("%s: route costs %v", name, costs)
		}
	}
}

// Every route the hub renders writes an answer's token counts after its
// credits: a model's own route, its entry route and its best-effort route.
func TestRoutesWriteTokenCounts(t *testing.T) {
	s := budgetState()
	s.TokenCounts = true
	costs, _, _ := unstructured.NestedSlice(find(t, Objects(s), "AIGatewayRoute", "opus").Object, "spec", "llmRequestCosts")
	if !reflect.DeepEqual(costs[1:], TokenCosts()) || costs[0].(map[string]any)["metadataKey"] != CreditsKey {
		t.Fatalf("costs: %v", costs)
	}

	f := fleetState()
	f.AuthEnabled, f.Identity, f.TokenCounts = true, Identity{Mode: IdentityTokens, JWKS: "{}"}, true
	f.Models[0].BestEffort, f.Models[0].Overage = true, []string{"team-a"}
	objs := Objects(f)
	for _, name := range []string{FleetName(f.Models[0].Slug), BestEffortName(f.Models[0].Slug)} {
		costs, _, _ := unstructured.NestedSlice(find(t, objs, "AIGatewayRoute", name).Object, "spec", "llmRequestCosts")
		if !reflect.DeepEqual(costs, TokenCosts()) {
			t.Errorf("%s costs %v", name, costs)
		}
	}
}

// A counter's key is the rate limit service's: the policy, the rule's name
// twice, the tenant and the model under their match entries, then the hour.
func TestTokenCounterKey(t *testing.T) {
	at := time.Date(2026, 10, 17, 14, 37, 5, 0, time.UTC)
	got := TokenCounterKey("p:", "gw", TokenCached, "team-a", "glm-5.3", at)
	want := "p:gw/aigw-ui-tenant-budgets_gw/aigw-ui-tenant-budgets/rule/3_gw/aigw-ui-tenant-budgets/rule/3_rule-3-match-0_team-a_rule-3-match-1_glm-5.3_" +
		fmt.Sprint(time.Date(2026, 10, 17, 14, 0, 0, 0, time.UTC).Unix())
	if got != want {
		t.Fatalf("key\n got %s\nwant %s", got, want)
	}
	if k := TokenCounterKey("", "gw", TokenRequests, "a", "m", at); k[:len("gw/aigw-ui-tenant-budgets_gw/aigw-ui-tenant-budgets/rule/1_")] != "gw/aigw-ui-tenant-budgets_gw/aigw-ui-tenant-budgets/rule/1_" {
		t.Fatalf("requests key %s", k)
	}
	// A key's counters: rules 5 to 8, by its client ID.
	got = KeyCounterKey("p:", "gw", TokenCached, "team-a.0011223344556677", "glm-5.3", at)
	want = "p:gw/aigw-ui-tenant-budgets_gw/aigw-ui-tenant-budgets/rule/7_gw/aigw-ui-tenant-budgets/rule/7_rule-7-match-0_team-a.0011223344556677_rule-7-match-1_glm-5.3_" +
		fmt.Sprint(time.Date(2026, 10, 17, 14, 0, 0, 0, time.UTC).Unix())
	if got != want {
		t.Fatalf("key's key\n got %s\nwant %s", got, want)
	}
}
