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
// it (2026-10-16). tenant-y got its budget first; the tenant that held the
// position between them has none any more.
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
		Budgets:    []TenantBudget{{TenantSlug: "tenant-y", Slot: 0, Limit: 300_000, Window: "1d"}, {TenantSlug: "tenant-x", Slot: 2, Limit: 100_000, Window: "1h"}},
		Restricted: []TenantModels{{TenantSlug: "tenant-x", Models: []string{"opus"}}},
	}
	return s
}

// One policy on the client listener, one shared rule per tenant at its
// position and a placeholder where nobody is, charged from the credits
// metadata; a priced model's route writes that metadata.
func TestBudgetPolicy(t *testing.T) {
	s := budgetState()
	objs := Objects(s)
	p := find(t, objs, "BackendTrafficPolicy", BudgetPolicyName)
	targets, _, _ := unstructured.NestedSlice(p.Object, "spec", "targetRefs")
	if got := fmt.Sprint(targets); got != "[map[group:gateway.networking.k8s.io kind:Gateway name:envoy-ai-gateway sectionName:http]]" {
		t.Fatalf("targets %s", got)
	}
	rules, _, _ := unstructured.NestedSlice(p.Object, "spec", "rateLimit", "global", "rules")
	if len(rules) != 3 {
		t.Fatalf("%d rules", len(rules))
	}
	for i, r := range rules {
		if r.(map[string]any)["shared"] != true {
			t.Fatalf("rule %d is not shared: one bucket per route would be one per model, and the positions would move", i)
		}
	}
	first := rules[0].(map[string]any)
	if got := fmt.Sprint(first["clientSelectors"]); got != "[map[headers:[map[name:x-aigw-tenant type:Exact value:tenant-y] map[name:x-aigw-tenant type:Distinct]]]]" {
		t.Fatalf("first rule matches %s", got)
	}
	if got := fmt.Sprint(first["limit"]); got != "map[requests:300000 unit:Day]" {
		t.Fatalf("first limit %s", got)
	}
	if got := fmt.Sprint(first["cost"]); got != "map[request:map[from:Number number:0] response:map[from:Metadata metadata:map[key:aigw_ui_credits namespace:io.envoy.ai_gateway]]]" {
		t.Fatalf("cost %s", got)
	}
	placeholder := rules[1].(map[string]any)
	if got := fmt.Sprint(placeholder["clientSelectors"]); got != "[map[headers:[map[name:x-aigw-tenant type:Exact value:aigw-ui.unused-position]]]]" {
		t.Fatalf("placeholder matches %s", got)
	}
	if got := fmt.Sprint(rules[2].(map[string]any)["limit"]); got != "map[requests:100000 unit:Hour]" {
		t.Fatalf("third limit %s", got)
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
	if q.GetAnnotations()[CreditsAnnotation] != "input_tokens * 3u" || q.GetAnnotations()[CreditsModelAnnotation] != "sonnet" {
		t.Fatalf("annotations %v", q.GetAnnotations())
	}
}

// A tenant keeps its rule's position, and with it its counter, whatever
// happens to the budgets of others: a new tenant that sorts first, one that
// is disabled or loses its budget.
func TestBudgetPositionsStay(t *testing.T) {
	position := func(s State, tenant string) int {
		p := find(t, Objects(s), "BackendTrafficPolicy", BudgetPolicyName)
		rules, _, _ := unstructured.NestedSlice(p.Object, "spec", "rateLimit", "global", "rules")
		for i, r := range rules {
			if strings.Contains(fmt.Sprint(r.(map[string]any)["clientSelectors"]), "value:"+tenant+"]") {
				return i
			}
		}
		return -1
	}
	s := budgetState()
	before := position(s, "tenant-x")
	s.Budgets = append(s.Budgets, TenantBudget{TenantSlug: "aaa", Slot: 1, Limit: 5, Window: "1d"})
	if got := position(s, "tenant-x"); got != before || position(s, "aaa") != 1 {
		t.Fatalf("tenant-x moved from %d to %d", before, got)
	}
	s.Budgets = s.Budgets[1:] // tenant-y disabled or without a budget
	if got := position(s, "tenant-x"); got != before {
		t.Fatalf("tenant-x moved from %d to %d", before, got)
	}
}

// Without tokens only, without budgets or without auth there is no policy,
// and a model that is not priced writes no credits.
func TestBudgetPolicyHeld(t *testing.T) {
	for name, change := range map[string]func(*State){
		"both":    func(s *State) { s.Identity.Mode = IdentityBoth },
		"no auth": func(s *State) { s.AuthEnabled = false },
		"none":    func(s *State) { s.Budgets = nil },
		"invalid": func(s *State) {
			s.Budgets = []TenantBudget{{TenantSlug: "a", Limit: 0, Window: "1d"}, {TenantSlug: "b", Limit: 1, Window: "2d"}}
		},
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
	// Prices in a dry run refuse nobody, so they charge no budget either.
	s = budgetState()
	s.Models[0].DryRun, s.Models[1].DryRun = true, true
	if spec := fmt.Sprint(find(t, Objects(s), "AIGatewayRoute", "opus").Object["spec"]); strings.Contains(spec, "llmRequestCosts") {
		t.Fatalf("a model in a dry run writes credits: %s", spec)
	}
	if a := find(t, Objects(s), "QuotaPolicy", "sonnet").GetAnnotations(); a[CreditsAnnotation] != "" {
		t.Fatalf("a model in a dry run carries credits for the sync: %v", a)
	}
}

// The counters are named as Envoy Gateway names a shared rule with an
// Exact and a Distinct match: the policy's domain, the rule's generic key
// twice, the Exact descriptor twice, the Distinct one with the tenant, then
// the window start. The Exact part is what the lab's shared Redis had
// (2026-10-16).
func TestBudgetCounters(t *testing.T) {
	cs := BudgetCounters(budgetState())
	now := time.Date(2026, 10, 15, 21, 30, 0, 0, time.UTC)
	want := map[string]string{
		"tenant-y": "p_envoy-ai-gateway-system/aigw-ui-tenant-budgets_envoy-ai-gateway-system/aigw-ui-tenant-budgets/rule/0_envoy-ai-gateway-system/aigw-ui-tenant-budgets/rule/0_rule-0-match-0_rule-0-match-0_rule-0-match-1_tenant-y_1792022400",
		"tenant-x": "p_envoy-ai-gateway-system/aigw-ui-tenant-budgets_envoy-ai-gateway-system/aigw-ui-tenant-budgets/rule/2_envoy-ai-gateway-system/aigw-ui-tenant-budgets/rule/2_rule-2-match-0_rule-2-match-0_rule-2-match-1_tenant-x_1792098000",
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

// A route of the cluster's own that serves several priced models charges
// each one its prices; any other model on it costs nothing.
func TestModelCredits(t *testing.T) {
	if got := ModelCredits(map[string]string{"opus": "input_tokens * 15u"}); got != "input_tokens * 15u" {
		t.Fatalf("one model: %s", got)
	}
	got := ModelCredits(map[string]string{"sonnet": "input_tokens * 3u", "opus": "input_tokens * 15u"})
	if got != "model == 'opus' ? (input_tokens * 15u) : model == 'sonnet' ? (input_tokens * 3u) : 0u" {
		t.Fatalf("two models: %s", got)
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
