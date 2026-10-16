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

// One policy on the client listener with one shared rule for every tenant:
// it matches a token with a budget, counts per tenant, takes the limit from
// the token's claims and is charged from the credits metadata; a priced
// model's route writes that metadata. The amounts are in no rule.
func TestBudgetPolicy(t *testing.T) {
	s := budgetState()
	objs := Objects(s)
	p := find(t, objs, "BackendTrafficPolicy", BudgetPolicyName)
	targets, _, _ := unstructured.NestedSlice(p.Object, "spec", "targetRefs")
	if got := fmt.Sprint(targets); got != "[map[group:gateway.networking.k8s.io kind:Gateway name:envoy-ai-gateway sectionName:http]]" {
		t.Fatalf("targets %s", got)
	}
	rules, _, _ := unstructured.NestedSlice(p.Object, "spec", "rateLimit", "global", "rules")
	if len(rules) != 1 {
		t.Fatalf("%d rules", len(rules))
	}
	rule := rules[0].(map[string]any)
	if rule["shared"] != true {
		t.Fatal("the rule is not shared: one bucket per route would be one per model")
	}
	if got := fmt.Sprint(rule["clientSelectors"]); got != "[map[headers:[map[name:x-aigw-budget type:RegularExpression value:.+] map[name:x-aigw-tenant type:Distinct]]]]" {
		t.Fatalf("the rule matches %s", got)
	}
	if got := fmt.Sprint(rule["limit"]); got != "map[fromMetadata:map[key:aigw-hub namespace:envoy.filters.http.jwt_authn] requests:4294967295 unit:Day]" {
		t.Fatalf("limit %s", got)
	}
	if got := fmt.Sprint(rule["cost"]); got != "map[request:map[from:Number number:0] response:map[from:Metadata metadata:map[key:aigw_ui_credits namespace:io.envoy.ai_gateway]]]" {
		t.Fatalf("cost %s", got)
	}
	if spec := fmt.Sprint(p.Object["spec"]); strings.Contains(spec, "tenant-") || strings.Contains(spec, "300000") {
		t.Fatalf("a tenant or an amount in the policy: %s", spec)
	}
	// The token's budget reaches the rule as a header.
	a := find(t, objs, "SecurityPolicy", AuthPolicyName)
	if claims := fmt.Sprint(a.Object["spec"].(map[string]any)["jwt"]); !strings.Contains(claims, "map[claim:requests_per_unit header:x-aigw-budget]") {
		t.Fatalf("no budget header from the token: %s", claims)
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

// A thousand tenants with budgets render the same policy as two.
func TestBudgetPolicySize(t *testing.T) {
	s := budgetState()
	two := fmt.Sprint(find(t, Objects(s), "BackendTrafficPolicy", BudgetPolicyName).Object)
	for i := range 1000 {
		s.Budgets = append(s.Budgets, TenantBudget{TenantSlug: fmt.Sprintf("team-%04d", i), Limit: int64(i + 1), Window: "1h"})
	}
	if many := fmt.Sprint(find(t, Objects(s), "BackendTrafficPolicy", BudgetPolicyName).Object); many != two {
		t.Fatalf("the policy changed with the tenants:\n%s\n%s", two, many)
	}
	if n := len(BudgetCounters(s)); n != 1002 {
		t.Fatalf("%d counters", n)
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

// The counters are named as Envoy Gateway names a shared rule with a header
// match and a Distinct match: the policy's domain, the rule's generic key
// twice, the first match's descriptor twice, the Distinct one with the
// tenant, then the window start, which is the tenant's.
func TestBudgetCounters(t *testing.T) {
	cs := BudgetCounters(budgetState())
	now := time.Date(2026, 10, 15, 21, 30, 0, 0, time.UTC)
	stem := "p_envoy-ai-gateway-system/aigw-ui-tenant-budgets_envoy-ai-gateway-system/aigw-ui-tenant-budgets/rule/0_envoy-ai-gateway-system/aigw-ui-tenant-budgets/rule/0_rule-0-match-0_rule-0-match-0_rule-0-match-1_"
	want := map[string]string{
		"tenant-y": stem + "tenant-y_1792022400",
		"tenant-x": stem + "tenant-x_1792098000",
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

// While some tenant is held to some models, one Deny rule refuses a model
// its token does not list, whoever the tenant; every lookup is guarded,
// since an error would let the request through. Without such a tenant
// there is no rule.
func TestModelsRule(t *testing.T) {
	s := budgetState()
	p := find(t, Objects(s), "SecurityPolicy", AuthPolicyName)
	rules, _, _ := unstructured.NestedSlice(p.Object, "spec", "authorization", "rules")
	if len(rules) != 1 {
		t.Fatalf("rules %v", rules)
	}
	rule := rules[0].(map[string]any)
	claims := "metadata.filter_metadata['envoy.filters.http.jwt_authn']['aigw-hub']"
	want := "'envoy.filters.http.jwt_authn' in metadata.filter_metadata && 'aigw-hub' in metadata.filter_metadata['envoy.filters.http.jwt_authn']" +
		" && 'models' in " + claims + " && !('x-ai-eg-model' in request.headers && request.headers['x-ai-eg-model'] in " + claims + "['models'])"
	if rule["action"] != "Deny" || rule["name"] != "models" || rule["cel"] != want {
		t.Fatalf("rule %v", rule)
	}
	// A thousand restricted tenants: still the one rule.
	for i := range 1000 {
		s.Restricted = append(s.Restricted, TenantModels{TenantSlug: fmt.Sprintf("team-%04d", i), Models: []string{"opus"}})
	}
	p = find(t, Objects(s), "SecurityPolicy", AuthPolicyName)
	if rules, _, _ = unstructured.NestedSlice(p.Object, "spec", "authorization", "rules"); len(rules) != 1 {
		t.Fatalf("%d rules for 1001 tenants", len(rules))
	}
	s.Restricted = nil
	p = find(t, Objects(s), "SecurityPolicy", AuthPolicyName)
	if spec := fmt.Sprint(p.Object["spec"]); strings.Contains(spec, "models") {
		t.Fatalf("a models rule without a restricted tenant: %s", spec)
	}
}
