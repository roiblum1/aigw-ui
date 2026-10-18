package render

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// A model as lab check A had it, the walletcheck-cheap backend in the
// gateway's namespace, with its quotas by amount: a and b have 66 an hour,
// c 100 a day counted only; position 1 was freed by an amount nobody has any
// more. The rule for budgets across models has position 3, after the
// amounts, where it was before its position was stored.
func amountState() State {
	return State{
		Namespace: "envoy-ai-gateway-system", GatewayName: "envoy-ai-gateway", AuthEnabled: true,
		Identity: Identity{Mode: IdentityTokens},
		Models: []Model{{
			Name: "wallet-cheap", Slug: "wallet-cheap", DefaultLimit: 1, DefaultWindow: "1d",
			Existing: []Target{{Backend: "walletcheck-cheap", Model: "wallet-cheap"}},
			Quotas: []TenantQuota{
				{TenantSlug: "walletcheck-a", Slot: 0, Limit: 66, Window: "1h"},
				{TenantSlug: "walletcheck-b", Slot: 1, Limit: 66, Window: "1h"},
				{TenantSlug: "walletcheck-c", Slot: 2, Limit: 100, Window: "1d", Shadow: true},
			},
			ByAmount:       true,
			Amounts:        []QuotaAmount{{Slot: 0, Limit: 66, Window: "1h"}, {Slot: 2, Limit: 100, Window: "1d", Shadow: true}},
			BudgetPassSlot: 3,
		}},
	}
}

// One rule per amount at its position, with a placeholder for the free one
// and for each position after the last up to eight, then the default bucket.
func TestAmountPolicy(t *testing.T) {
	s := amountState()
	p := quotaPolicies(s, s.Models[0])[0]
	rules, _, _ := unstructured.NestedSlice(p.Object, "spec", "perModelQuotas")
	got := fmt.Sprint(rules[0].(map[string]any)["quota"].(map[string]any)["bucketRules"])
	want := "[" +
		"map[clientSelectors:[map[headers:[map[name:x-aigw-quota-wallet-cheap type:Exact value:66-1h] map[name:x-aigw-tenant type:Distinct]]]] quota:map[duration:1h limit:66]] " +
		"map[clientSelectors:[map[headers:[map[name:x-aigw-client-id type:Exact value:aigw-ui-unused-position]]]] quota:map[duration:1d limit:1]] " +
		"map[clientSelectors:[map[headers:[map[name:x-aigw-quota-wallet-cheap type:Exact value:100-1d-shadow] map[name:x-aigw-tenant type:Distinct]]]] quota:map[duration:1d limit:100] shadowMode:true]" +
		strings.Repeat(" map[clientSelectors:[map[headers:[map[name:x-aigw-client-id type:Exact value:aigw-ui-unused-position]]]] quota:map[duration:1d limit:1]]", 5) + "]"
	if got != want {
		t.Fatalf("rules:\n%s\nwant\n%s", got, want)
	}
	// On the best-effort route nobody is refused.
	be := quotaPolicy(s.Namespace, s.Models[0], []Target{{Backend: "x", Model: "wallet-cheap"}}, true)
	if r := fmt.Sprint(be.Object["spec"]); strings.Count(r, "shadowMode:true") != 2 {
		t.Fatalf("best-effort: %s", r)
	}
	if n := RuleCount(s.Models[0]); n != 3 {
		t.Fatalf("rule count %d", n)
	}
}

// Each tenant has a counter of its own under its amount's rule; the default
// bucket comes after the eight positions the policy has.
func TestAmountCounters(t *testing.T) {
	now := time.Unix(1792065600+60, 0)
	keys := map[string]string{}
	for _, c := range Counters(amountState()) {
		name := c.TenantSlug
		if c.Pool {
			name = "pool"
		}
		keys[name] = c.RedisKey("", now)
	}
	prefix := "ai-gateway-quota_backend_name_envoy-ai-gateway-system/walletcheck-cheap_model_name_override_wallet-cheap_"
	hour, day := "1792065600", "1792022400"
	want := map[string]string{
		"walletcheck-a": prefix + "rule-0-x-aigw-quota-wallet-cheap|66-1h-match-0_rule-0-x-aigw-quota-wallet-cheap|66-1h-match-0_rule-0-x-aigw-tenant-match-1_walletcheck-a_" + hour,
		"walletcheck-b": prefix + "rule-0-x-aigw-quota-wallet-cheap|66-1h-match-0_rule-0-x-aigw-quota-wallet-cheap|66-1h-match-0_rule-0-x-aigw-tenant-match-1_walletcheck-b_" + hour,
		"walletcheck-c": prefix + "rule-2-x-aigw-quota-wallet-cheap|100-1d-shadow-match-0_rule-2-x-aigw-quota-wallet-cheap|100-1d-shadow-match-0_rule-2-x-aigw-tenant-match-1_walletcheck-c_" + day,
		"pool":          prefix + "rule-8-match--1_rule-8-match--1_" + day,
	}
	if fmt.Sprint(keys) != fmt.Sprint(want) {
		t.Fatalf("keys:\n%v\nwant\n%v", keys, want)
	}
}

// A tenant moving to an amount that is there already changes no rule, so
// the routes are not built again; a new amount does.
func TestAmountRevision(t *testing.T) {
	s := amountState()
	before := quotaRevision(s.Models[0])
	s.Models[0].Quotas[1] = TenantQuota{TenantSlug: "walletcheck-b", Slot: 1, Limit: 100, Window: "1d", Shadow: true}
	if quotaRevision(s.Models[0]) != before {
		t.Fatal("a tenant moving between amounts changed the revision")
	}
	s.Models[0].Amounts = append(s.Models[0].Amounts, QuotaAmount{Slot: 1, Limit: 7, Window: "1m"})
	if quotaRevision(s.Models[0]) == before {
		t.Fatal("a new amount kept the revision")
	}
}

// The token's amount reaches the gateway in a header of the model's, only
// while the rules are by amount and the model has an amount.
func TestQuotaClaimHeaders(t *testing.T) {
	s := amountState()
	got := fmt.Sprint(QuotaClaimHeaders(s.Models))
	if got != "[map[claim:quotas.wallet-cheap header:x-aigw-quota-wallet-cheap]]" {
		t.Fatalf("headers %s", got)
	}
	var policy string
	for _, o := range Objects(s) {
		if o.GetKind() == "SecurityPolicy" {
			policy += fmt.Sprint(o.Object["spec"])
		}
	}
	if !strings.Contains(policy, "map[claim:quotas.wallet-cheap header:x-aigw-quota-wallet-cheap]") {
		t.Fatalf("the token provider does not copy the amount: %s", policy)
	}
	s.Models[0].Amounts = nil
	if h := QuotaClaimHeaders(s.Models); len(h) != 0 {
		t.Fatalf("headers without amounts: %v", h)
	}
	s = amountState()
	s.Models[0].ByAmount = false
	if h := QuotaClaimHeaders(s.Models); len(h) != 0 {
		t.Fatalf("headers with a rule per tenant: %v", h)
	}
}

func TestQuotaAmountName(t *testing.T) {
	if QuotaAmountName(500000, "1h", false) != "500000-1h" || QuotaAmountName(7, "1m", true) != "7-1m-shadow" {
		t.Fatal(QuotaAmountName(500000, "1h", false), QuotaAmountName(7, "1m", true))
	}
}

// A model server's backend removes the credentials only, for any number of
// models with quotas by amount: the AIServiceBackend takes 16 names at
// most, and a header per such model made every backend invalid from the
// eleventh on.
func TestModelServerHeadersForAnyNumberOfModels(t *testing.T) {
	s := testState()
	s.Identity = Identity{Mode: IdentityTokens}
	for i := range s.Models {
		s.Models[i].ByAmount = true
	}
	s.Models[0].Amounts = []QuotaAmount{{Slot: 0, Limit: 1000, Window: "1d"}, {Slot: 1, Limit: 500, Window: "1h"}}
	for n := range 20 {
		name := fmt.Sprintf("extra-%02d", n)
		s.Models = append(s.Models, Model{Name: name, Slug: name, Host: name + ".models", Port: 8000, UpstreamModel: name,
			DefaultLimit: 1, DefaultWindow: "1d", ByAmount: true, Amounts: []QuotaAmount{{Slot: 0, Limit: 1000, Window: "1d"}},
			Quotas: []TenantQuota{{TenantSlug: "team-" + name, Slot: 0, Limit: 1000, Window: "1d"}}})
	}
	if h := QuotaClaimHeaders(s.Models); len(h) < 20 {
		t.Fatalf("the test needs more models with quotas by amount than the list takes: %d", len(h))
	}
	for _, name := range []string{"glm5-3", "small", "extra-19"} {
		var b *unstructured.Unstructured
		for _, o := range Objects(s) {
			if o.GetKind() == "AIServiceBackend" && o.GetName() == name {
				b = o
			}
		}
		if b == nil {
			t.Fatalf("no backend %s", name)
		}
		got := fmt.Sprint(b.Object["spec"].(map[string]any)["headerMutation"])
		if got != "map[remove:[authorization x-api-key]]" {
			t.Fatalf("backend %s: %s", name, got)
		}
	}
}

// While the gateways take API keys too, a tenant's quota is its own rule on
// its client IDs, which a key sets: the tenant header is not read.
func TestQuotaRulesOnClientID(t *testing.T) {
	s := amountState()
	s.Identity.Mode = IdentityBoth
	s.Models[0].ByAmount, s.Models[0].Amounts = false, nil
	p := fmt.Sprint(quotaPolicies(s, s.Models[0])[0].Object["spec"])
	if strings.Contains(p, TenantHeader) || !strings.Contains(p, `^walletcheck-a\.[a-f0-9]+$`) {
		t.Fatalf("policy: %s", p)
	}
}
