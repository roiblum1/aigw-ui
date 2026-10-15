package render

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// A model as lab check A had it: the walletcheck-cheap backend in the
// gateway's namespace, a standard of 33 an hour, and tenant a above it.
func standardState() State {
	return State{
		Namespace: "envoy-ai-gateway-system", GatewayName: "envoy-ai-gateway", AuthEnabled: true,
		Identity: Identity{Mode: IdentityTokens},
		Tenants:  []string{"walletcheck-a", "walletcheck-b", "walletcheck-c"},
		Models: []Model{{
			Name: "wallet-cheap", Slug: "wallet-cheap", DefaultLimit: 1, DefaultWindow: "1d",
			Existing: []Target{{Backend: "walletcheck-cheap", Model: "wallet-cheap"}},
			Standard: &Budget{Limit: 33, Window: "1h"},
			Quotas:   []TenantQuota{{TenantSlug: "walletcheck-a", Slot: 0, Limit: 66, Window: "1h"}},
		}},
	}
}

// One rule on the tenant header for every tenant, then the exceptions on it
// by name, then the default bucket.
func TestStandardPolicy(t *testing.T) {
	s := standardState()
	p := quotaPolicies(s, s.Models[0])[0]
	rules, _, _ := unstructured.NestedSlice(p.Object, "spec", "perModelQuotas")
	q := rules[0].(map[string]any)["quota"].(map[string]any)
	got := fmt.Sprint(q["bucketRules"])
	want := "[map[clientSelectors:[map[headers:[map[name:x-aigw-tenant type:Distinct]]]] quota:map[duration:1h limit:33]] " +
		"map[clientSelectors:[map[headers:[map[name:x-aigw-tenant type:Exact value:walletcheck-a]]]] quota:map[duration:1h limit:66]]]"
	if got != want {
		t.Fatalf("rules:\n%s\nwant\n%s", got, want)
	}
	// On the best-effort route and in dry-run, nobody is refused.
	be := quotaPolicy(s.Namespace, s.Models[0], []Target{{Backend: "x", Model: "wallet-cheap"}}, true)
	if r := fmt.Sprint(be.Object["spec"]); strings.Count(r, "shadowMode:true") != 2 {
		t.Fatalf("best-effort: %s", r)
	}
	// Without quotas the policy is still there: the standard needs it.
	s.Models[0].Quotas = nil
	if !has(Objects(s), "QuotaPolicy", "wallet-cheap") {
		t.Fatal("no QuotaPolicy for a standard without quotas")
	}
	if now, perTenant := RuleCount(standardState().Models[0], 200); now != 2 || perTenant != 200 {
		t.Fatalf("rule count %d %d", now, perTenant)
	}
}

// The counters are named as the lab's Redis named them (check A3): the
// standard's bucket after the tenant, an exception after its rule, the
// default bucket after the number of rules. A tenant with an exception is
// read there.
func TestStandardCounters(t *testing.T) {
	now := time.Unix(1792065600+60, 0)
	keys := map[string]string{}
	for _, c := range Counters(standardState()) {
		name := c.TenantSlug
		if c.Pool {
			name = "pool"
		}
		keys[name] = c.RedisKey("", now)
	}
	prefix := "ai-gateway-quota_backend_name_envoy-ai-gateway-system/walletcheck-cheap_model_name_override_wallet-cheap_"
	want := map[string]string{
		"walletcheck-a": prefix + "rule-1-x-aigw-tenant|walletcheck-a-match-0_rule-1-x-aigw-tenant|walletcheck-a-match-0_1792065600",
		"walletcheck-b": prefix + "rule-0-x-aigw-tenant-match-0_walletcheck-b_1792065600",
		"walletcheck-c": prefix + "rule-0-x-aigw-tenant-match-0_walletcheck-c_1792065600",
		"pool":          prefix + "rule-2-match--1_rule-2-match--1_1792022400",
	}
	if fmt.Sprint(keys) != fmt.Sprint(want) {
		t.Fatalf("keys:\n%v\nwant\n%v", keys, want)
	}
	// The counter a made-up tenant would have, from the pool's.
	for _, c := range Counters(standardState()) {
		if !c.Pool {
			continue
		}
		if o, ok := c.StandardFor("nobody"); !ok || o.RedisKey("", now) != prefix+"rule-0-x-aigw-tenant-match-0_nobody_1792065600" {
			t.Fatalf("made up: %+v", o)
		}
	}
	s := standardState()
	s.Models[0].Standard = nil
	for _, c := range Counters(s) {
		if _, ok := c.StandardFor("nobody"); ok {
			t.Fatal("a counter without a standard")
		}
	}
}

// Without a standard nothing changes: the same rules on the client ID.
func TestNoStandardUnchanged(t *testing.T) {
	s := standardState()
	s.Models[0].Standard = nil
	p := fmt.Sprint(quotaPolicies(s, s.Models[0])[0].Object["spec"])
	if strings.Contains(p, TenantHeader) || !strings.Contains(p, `^walletcheck-a\.[a-f0-9]+$`) {
		t.Fatalf("policy: %s", p)
	}
	if quotaRevision(s.Models[0]) == quotaRevision(standardState().Models[0]) {
		t.Fatal("the standard does not change the revision")
	}
}
