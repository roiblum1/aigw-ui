package render

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Two priced models, one rendered here and one discovered, tenants x and y
// with budgets, and tenant-x held to one model; as the lab budget check had
// it (2026-10-16).
func budgetState() State {
	s := State{
		Namespace: "envoy-ai-gateway-system", GatewayName: "envoy-ai-gateway", ClientListener: "http", AuthEnabled: true,
		Identity: Identity{Mode: IdentityTokens, Issuer: "aigw-ui", Audience: "aigw-fleet", JWKS: "{}"},
		Tenants:  []string{"tenant-x", "tenant-y", "tenant-z"},
		Models: []Model{
			{Name: "opus", Slug: "opus", Host: "opus.models", Port: 8000, UpstreamModel: "opus", DefaultLimit: 1, DefaultWindow: "1d",
				CostExpression: "input_tokens * 15u", Priced: true},
			{Name: "sonnet", Slug: "sonnet", DefaultLimit: 1, DefaultWindow: "1d", CostExpression: "input_tokens * 3u", Priced: true,
				Existing: []Target{{Backend: "sonnet", Model: "sonnet"}},
				Quotas:   []TenantQuota{{TenantSlug: "tenant-y", Slot: 0, Limit: 10, Window: "1d"}}},
		},
		Budgets:    []TenantBudget{{TenantSlug: "tenant-y", Limit: 300_000, Window: "1d"}, {TenantSlug: "tenant-x", Limit: 100_000, Window: "1h"}},
		Restricted: []TenantModels{{TenantSlug: "tenant-x", Models: []string{"opus"}}},
	}
	return s
}

// One policy on the client listener, one shared rule per tenant in slug
// order, charged from the credits metadata; a priced model's route writes
// that metadata.
func TestBudgetPolicy(t *testing.T) {
	s := budgetState()
	objs := Objects(s)
	p := find(t, objs, "BackendTrafficPolicy", BudgetPolicyName)
	targets, _, _ := unstructured.NestedSlice(p.Object, "spec", "targetRefs")
	if got := fmt.Sprint(targets); got != "[map[group:gateway.networking.k8s.io kind:Gateway name:envoy-ai-gateway sectionName:http]]" {
		t.Fatalf("targets %s", got)
	}
	rules, _, _ := unstructured.NestedSlice(p.Object, "spec", "rateLimit", "global", "rules")
	if len(rules) != 2 {
		t.Fatalf("%d rules", len(rules))
	}
	first := rules[0].(map[string]any)
	if got := fmt.Sprint(first["clientSelectors"]); got != "[map[headers:[map[name:x-aigw-tenant type:Exact value:tenant-x]]]]" {
		t.Fatalf("first rule matches %s", got)
	}
	if got := fmt.Sprint(first["limit"]); got != "map[requests:100000 unit:Hour]" {
		t.Fatalf("first limit %s", got)
	}
	if first["shared"] != true {
		t.Fatal("the rule is not shared: one bucket per route would be one per model")
	}
	if got := fmt.Sprint(first["cost"]); got != "map[request:map[from:Number number:0] response:map[from:Metadata metadata:map[key:aigw_ui_credits namespace:io.envoy.ai_gateway]]]" {
		t.Fatalf("cost %s", got)
	}
	if got := fmt.Sprint(rules[1].(map[string]any)["limit"]); got != "map[requests:300000 unit:Day]" {
		t.Fatalf("second limit %s", got)
	}
	// Server-side apply sends the object as JSON.
	p.DeepCopy()

	route := find(t, objs, "AIGatewayRoute", "opus")
	if got := fmt.Sprint(route.Object["spec"].(map[string]any)["llmRequestCosts"]); got != "[map[cel:input_tokens * 15u metadataKey:aigw_ui_credits type:CEL]]" {
		t.Fatalf("route costs %s", got)
	}
	// The discovered model's route is not rendered; its QuotaPolicy tells
	// the sync what to put on it.
	q := find(t, objs, "QuotaPolicy", "sonnet")
	if q.GetAnnotations()[CreditsAnnotation] != "input_tokens * 3u" {
		t.Fatalf("annotations %v", q.GetAnnotations())
	}
}

// Without tokens only, without budgets or without auth there is no policy,
// and a model that is not priced writes no credits.
func TestBudgetPolicyHeld(t *testing.T) {
	for name, change := range map[string]func(*State){
		"both":     func(s *State) { s.Identity.Mode = IdentityBoth },
		"no auth":  func(s *State) { s.AuthEnabled = false },
		"none":     func(s *State) { s.Budgets = nil },
		"invalid":  func(s *State) { s.Budgets = []TenantBudget{{TenantSlug: "a", Limit: 0, Window: "1d"}, {TenantSlug: "b", Limit: 1, Window: "2d"}} },
	} {
		s := budgetState()
		change(&s)
		if has(Objects(s), "BackendTrafficPolicy", BudgetPolicyName) {
			t.Errorf("%s: a budget policy was rendered", name)
		}
		if n := len(BudgetCounters(s)); n != 0 {
			t.Errorf("%s: %d counters", name, n)
		}
	}
	s := budgetState()
	s.Models[0].Priced = false
	if spec := fmt.Sprint(find(t, Objects(s), "AIGatewayRoute", "opus").Object["spec"]); strings.Contains(spec, "llmRequestCosts") {
		t.Fatalf("an unpriced model writes credits: %s", spec)
	}
}

// The counters are named as the hub's Redis named the lab's (2026-10-16):
// the policy's domain, the rule's generic key twice, its descriptor twice,
// then the window start.
func TestBudgetCounters(t *testing.T) {
	cs := BudgetCounters(budgetState())
	now := time.Date(2026, 10, 15, 21, 30, 0, 0, time.UTC)
	want := map[string]string{
		"tenant-x": "p_envoy-ai-gateway-system/aigw-ui-tenant-budgets_envoy-ai-gateway-system/aigw-ui-tenant-budgets/rule/0_envoy-ai-gateway-system/aigw-ui-tenant-budgets/rule/0_rule-0-match-0_rule-0-match-0_1792098000",
		"tenant-y": "p_envoy-ai-gateway-system/aigw-ui-tenant-budgets_envoy-ai-gateway-system/aigw-ui-tenant-budgets/rule/1_envoy-ai-gateway-system/aigw-ui-tenant-budgets/rule/1_rule-1-match-0_rule-1-match-0_1792022400",
	}
	if len(cs) != len(want) {
		t.Fatalf("%d counters", len(cs))
	}
	for _, c := range cs {
		if got := c.RedisKey("p_", now); got != want[c.TenantSlug] {
			t.Errorf("%s:\n%s\nwant\n%s", c.TenantSlug, got, want[c.TenantSlug])
		}
	}
}

// A restricted tenant gets a Deny rule in the SecurityPolicy that matches
// its requests for any model but its own; a tenant that may call every
// model gets none.
func TestModelRules(t *testing.T) {
	s := budgetState()
	p := find(t, Objects(s), "SecurityPolicy", AuthPolicyName)
	rules, _, _ := unstructured.NestedSlice(p.Object, "spec", "authorization", "rules")
	if len(rules) != 1 {
		t.Fatalf("rules %v", rules)
	}
	rule := rules[0].(map[string]any)
	want := "'x-aigw-tenant' in request.headers && request.headers['x-aigw-tenant'] == 'tenant-x' && " +
		"!('x-ai-eg-model' in request.headers && request.headers['x-ai-eg-model'] in ['opus'])"
	if rule["action"] != "Deny" || rule["name"] != "models-tenant-x" || rule["cel"] != want {
		t.Fatalf("rule %v", rule)
	}
	// No model at all: every request of the tenant is refused.
	s.Restricted = []TenantModels{{TenantSlug: "o'neil"}}
	p = find(t, Objects(s), "SecurityPolicy", AuthPolicyName)
	rules, _, _ = unstructured.NestedSlice(p.Object, "spec", "authorization", "rules")
	if cel := rules[0].(map[string]any)["cel"]; cel != `'x-aigw-tenant' in request.headers && request.headers['x-aigw-tenant'] == 'o\'neil'` {
		t.Fatalf("cel %v", cel)
	}
	// With API keys still taken, a request with one has no tenant header:
	// the rule is rendered all the same, and holds the tokens.
	s.Identity.Mode = IdentityBoth
	p = find(t, Objects(s), "SecurityPolicy", AuthPolicyName)
	if spec := fmt.Sprint(p.Object["spec"]); !strings.Contains(spec, "models-o'neil") {
		t.Fatalf("no model rule while both are taken: %s", spec)
	}
}
