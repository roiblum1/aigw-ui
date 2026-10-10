package render

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"aigw-ui/internal/costcel"
)

// 0.11574 dollars for a million input tokens, a tenth of it cached, four
// times for output.
var testPrices = Prices{Input: 11574, Cached: 1157, Output: 46296}

func TestPriceExpression(t *testing.T) {
	expr := testPrices.Expression()
	if err := costcel.Check(expr); err != nil {
		t.Fatalf("the gateway would not use %q: %v", expr, err)
	}
	for name, tc := range map[string]struct {
		u    costcel.Usage
		want uint64
	}{
		"nothing cached":            {costcel.Usage{Input: 100_000, Output: 10_000}, 1620},
		"most of the prompt cached": {costcel.Usage{Input: 100_000, CachedInput: 90_000, Output: 10_000}, 682},
		"all of it cached":          {costcel.Usage{Input: 100_000, CachedInput: 100_000}, 115},
		"more cached than sent":     {costcel.Usage{Input: 100_000, CachedInput: 4_000_000_000}, 1157},
		"a small request is free":   {costcel.Usage{Input: 50, Output: 1}, 0},
		"reasoning is not added":    {costcel.Usage{Input: 100_000, Output: 10_000, Reasoning: 9_000}, 1620},
		"the largest counts":        {costcel.Usage{Input: 4_294_967_295, Output: 4_294_967_295}, (4_294_967_295*11574 + 4_294_967_295*46296) / 1_000_000},
	} {
		got, err := costcel.Eval(expr, tc.u)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %d, %v, want %d", name, got, err, tc.want)
		}
	}
}

// In dry-run every tenant is counted and nobody is refused: the rules are
// shadow rules and the pool is as large as it can be.
func TestDryRunRefusesNobody(t *testing.T) {
	s := testState()
	s.Models[0].CostExpression = testPrices.Expression()
	s.Models[0].DryRun = true
	policy := find(t, Objects(s), "QuotaPolicy", s.Models[0].Slug)
	perModel, _, _ := unstructured.NestedSlice(policy.Object, "spec", "perModelQuotas")
	quota := perModel[0].(map[string]any)["quota"].(map[string]any)
	if quota["costExpression"] != testPrices.Expression() {
		t.Errorf("cost expression = %v", quota["costExpression"])
	}
	if limit := quota["defaultBucket"].(map[string]any)["limit"]; limit != serviceQuotaLimit {
		t.Errorf("the pool is %v in dry-run, want the largest limit", limit)
	}
	rules := quota["bucketRules"].([]any)
	if len(rules) == 0 {
		t.Fatal("no tenant rules")
	}
	for _, r := range rules {
		rule := r.(map[string]any)
		if rule["shadowMode"] != true && rule["quota"].(map[string]any)["limit"] != fallbackLimit {
			t.Errorf("a tenant rule can refuse in dry-run: %v", rule)
		}
	}
	for _, c := range Counters(s) {
		if c.ModelSlug == s.Models[0].Slug && !c.Pool && !c.Shadow {
			t.Errorf("the counter of %s is not marked as dry-run", c.TenantSlug)
		}
	}
	plain := testState()
	plain.Models[0].CostExpression = testPrices.Expression()
	if quotaRevision(plain.Models[0]) == quotaRevision(s.Models[0]) {
		t.Error("ending dry-run does not change the quota revision, so the routes would not be built again")
	}
}
