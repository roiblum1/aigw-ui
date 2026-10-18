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

// A revoked personal key's tokens are refused by their client ID: they
// are not stored, so there is no jti to refuse them by.
func TestDeniedClients(t *testing.T) {
	s := budgetState()
	s.Identity.DeniedClients = []string{"tenant-x.0011223344556677"}
	p := find(t, Objects(s), "SecurityPolicy", AuthPolicyName)
	rules, _, _ := unstructured.NestedSlice(p.Object, "spec", "authorization", "rules")
	if got := fmt.Sprint(rules[0]); got != "map[action:Deny name:revoked-personal-key principal:map[jwt:map[claims:[map[name:cid values:[tenant-x.0011223344556677]]] provider:aigw-hub]]]" {
		t.Fatalf("rule %s", got)
	}
}

// A tenant with a budget across models is held by it alone on a model it is
// charged on: the model's QuotaPolicy ends with a rule that matches the
// budget header and never runs out, so in Shared mode neither the pool nor a
// quota of the tenant's own refuses it. The positions before it stay. Each
// such tenant is counted on its own there, in the key the gateway writes
// (seen in the shared Redis, 2026-10-16), and its counters only count.
func TestBudgetPassRule(t *testing.T) {
	s := amountState()
	m := &s.Models[0]
	m.CostExpression, m.Priced, m.BudgetsHold = "input_tokens * 1u", true, true
	// b has a quota of its own as well, from before a tenant had one or the other.
	s.Budgets = []TenantBudget{{TenantSlug: "walletcheck-b", Limit: 500, Window: "1h"}, {TenantSlug: "walletcheck-d", Limit: 500, Window: "1h"},
		{TenantSlug: "walletcheck-e", Limit: 500, Window: "1h"}}
	// e may call another model only: the gateway refuses it this one, so it
	// has no counter here.
	s.Restricted = []TenantModels{{TenantSlug: "walletcheck-e", Models: []string{"other"}}, {TenantSlug: "walletcheck-d", Models: []string{m.Name}}}

	p := quotaPolicies(s, *m)[0]
	rules, _, _ := unstructured.NestedSlice(p.Object, "spec", "perModelQuotas")
	got := rules[0].(map[string]any)["quota"].(map[string]any)["bucketRules"].([]any)
	want := "map[clientSelectors:[map[headers:[map[name:x-aigw-budget type:RegularExpression value:.+] map[name:x-aigw-tenant type:Distinct]]]] quota:map[duration:1d limit:4294967295]]"
	if len(got) != 8 || fmt.Sprint(got[3]) != want || !strings.Contains(fmt.Sprint(got[0]), "66-1h") || len(withoutPadding(t, got)) != 4 {
		t.Fatalf("rules: %v", got)
	}
	if n := RuleCount(*m); n != 4 {
		t.Fatalf("rule count %d", n)
	}
	// On the best-effort route it only counts, as every rule there does.
	be := quotaPolicy(s.Namespace, *m, []Target{{Backend: "x", Model: "wallet-cheap"}}, true)
	if r := fmt.Sprint(be.Object["spec"]); strings.Count(r, "shadowMode:true") != 3 {
		t.Fatalf("best-effort: %s", r)
	}

	day := time.Unix(1792022400+60, 0)
	prefix := "ai-gateway-quota_backend_name_envoy-ai-gateway-system/walletcheck-cheap_model_name_override_wallet-cheap_"
	held := map[string]bool{}
	for _, c := range Counters(s) {
		if c.Pool {
			if key := c.RedisKey("", day); !strings.Contains(key, "rule-8-match--1") {
				t.Fatalf("pool after the eight rules: %s", key)
			}
			continue
		}
		if c.Budget && !c.Shadow {
			t.Fatalf("a counter the budget holds refuses: %+v", c)
		}
		held[c.TenantSlug] = c.Budget
		if c.TenantSlug == "walletcheck-d" {
			want := prefix + "rule-3-x-aigw-budget|.+-match-0_rule-3-x-aigw-budget|.+-match-0_rule-3-x-aigw-tenant-match-1_walletcheck-d_1792022400"
			if key := c.RedisKey("", day); key != want {
				t.Fatalf("key\n%s\nwant\n%s", key, want)
			}
			if other, ok := c.For("nobody"); !ok || other.RedisKey("", day) != strings.Replace(want, "walletcheck-d", "nobody", 1) {
				t.Fatalf("another tenant's counter: %v %s", ok, other.RedisKey("", day))
			}
		}
	}
	if fmt.Sprint(held) != "map[walletcheck-a:false walletcheck-b:true walletcheck-c:false walletcheck-d:true]" {
		t.Fatalf("held by the budget: %v", held)
	}

	// The rule changes the routes' revision; a model without it keeps its own.
	without := *m
	without.BudgetsHold = false
	if quotaRevision(*m) == quotaRevision(without) {
		t.Fatal("the rule did not change the revision")
	}
	// Prices in a dry run charge no budget, and refuse nobody: no rule.
	dry := *m
	dry.DryRun = true
	if r := fmt.Sprint(quotaPolicies(s, dry)[0].Object["spec"]); strings.Contains(r, BudgetHeader) {
		t.Fatalf("dry run: %s", r)
	}
	dryWithout := dry
	dryWithout.BudgetsHold = false
	if quotaRevision(dry) != quotaRevision(dryWithout) {
		t.Fatal("dry run: the revision changed")
	}
}

// The rule that lets a budget's tenants through keeps its stored position
// when an amount is added after it and when the amounts before it are freed,
// so each tenant's counter under it keeps its name. As the last rule it moved
// with every such change, and what a tenant used under the old position since
// the history's last look was lost (0.31.0 run, 2026-10-17).
func TestBudgetPassRuleKeepsItsPosition(t *testing.T) {
	s := amountState()
	m := &s.Models[0]
	m.CostExpression, m.Priced, m.BudgetsHold = "input_tokens * 1u", true, true
	s.Budgets = []TenantBudget{{TenantSlug: "walletcheck-d", Limit: 500, Window: "1h"}}
	day := time.Unix(1792022400+60, 0)
	keys := func() (budget, pool string, rules []string) {
		t.Helper()
		for _, c := range Counters(s) {
			switch {
			case c.Pool:
				pool = c.RedisKey("", day)
			case c.TenantSlug == "walletcheck-d":
				budget = c.RedisKey("", day)
			}
		}
		p := quotaPolicies(s, *m)[0]
		got, _, _ := unstructured.NestedSlice(p.Object, "spec", "perModelQuotas")
		for _, r := range withoutPadding(t, got[0].(map[string]any)["quota"].(map[string]any)["bucketRules"].([]any)) {
			switch v := fmt.Sprint(r); {
			case strings.Contains(v, BudgetHeader):
				rules = append(rules, "budget")
			case strings.Contains(v, placeholderSelector):
				rules = append(rules, "-")
			default:
				rules = append(rules, v[strings.Index(v, "value:")+6:strings.Index(v, "] map")])
			}
		}
		return budget, pool, rules
	}
	before, firstPool, rules := keys()
	if !strings.Contains(before, "_rule-3-x-aigw-budget") || fmt.Sprint(rules) != "[66-1h - 100-1d-shadow budget]" || !strings.Contains(firstPool, "rule-8-match--1") {
		t.Fatalf("before: %s %v %s", before, rules, firstPool)
	}

	// A new amount takes the next free position, which is after the rule.
	// The pool keeps its counter too: the policy still has eight rules.
	m.Amounts = append(m.Amounts, QuotaAmount{Slot: 4, Limit: 7, Window: "1m"})
	after, pool, rules := keys()
	if after != before || fmt.Sprint(rules) != "[66-1h - 100-1d-shadow budget 7-1m]" || pool != firstPool {
		t.Fatalf("after an amount was added: %s %v %s", after, rules, pool)
	}
	// Every amount after the first is freed: the positions stay held.
	m.Amounts = m.Amounts[:1]
	after, pool, rules = keys()
	if after != before || fmt.Sprint(rules) != "[66-1h - - budget]" || pool != firstPool {
		t.Fatalf("after amounts were freed: %s %v %s", after, rules, pool)
	}
	// Without budgets the position is not rendered, and the amounts keep theirs.
	m.BudgetsHold = false
	m.Amounts = append(m.Amounts, QuotaAmount{Slot: 4, Limit: 7, Window: "1m"})
	if _, _, rules = keys(); fmt.Sprint(rules) != "[66-1h - - - 7-1m]" {
		t.Fatalf("without budgets: %v", rules)
	}
}

// A priced model without a quota gets its QuotaPolicy while
// tenants have budgets, so that its pool holds the tenants with neither a
// budget nor a quota of their own; without budgets it has none, as before.
func TestBudgetPolicyOnModelWithoutQuotas(t *testing.T) {
	policy := func(s State) string {
		for _, o := range Objects(s) {
			if o.GetKind() == "QuotaPolicy" && o.GetName() == "opus" {
				return fmt.Sprint(o.Object["spec"])
			}
		}
		return ""
	}
	s := budgetState()
	if p := policy(s); p != "" {
		t.Fatalf("without the flag: %s", p)
	}
	for i := range s.Models {
		s.Models[i].BudgetsHold = true
	}
	if p := policy(s); !strings.Contains(p, BudgetHeader) || !strings.Contains(p, "defaultBucket:map[duration:1d limit:1]") {
		t.Fatalf("with budgets: %s", p)
	}
}
