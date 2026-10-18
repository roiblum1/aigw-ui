package render

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// poolKey is the Redis key of the model's pool on its own route (overage
// false) or on the best-effort route, in the day of the test.
func poolKey(t *testing.T, s State, overage bool) string {
	t.Helper()
	day := time.Unix(1792022400+60, 0)
	for _, c := range Counters(s) {
		if c.Pool && c.Overage == overage {
			return c.RedisKey("", day)
		}
	}
	t.Fatalf("no pool counter (best-effort %v)", overage)
	return ""
}

// The pool is counted under a name made of the number of rules before it.
// That number followed the positions in use, so an amount added after the
// last one, the last one freed, or the first budget across models on the
// model started the pool's window again, and the tenants on the pool could
// use up to twice the pool in it. The policy now has a multiple of eight
// rules, so none of those renames the pool's counter; only the ninth
// position does, once.
func TestPoolKeepsItsCounter(t *testing.T) {
	s := amountState() // amounts at positions 0 and 2, the budget rule's at 3
	m := &s.Models[0]
	m.CostExpression, m.Priced = "input_tokens * 1u", true
	first := poolKey(t, s, false)
	if !strings.Contains(first, "_rule-8-match--1_") {
		t.Fatalf("pool: %s, want it after eight rules", first)
	}
	for _, step := range []struct {
		name   string
		change func()
	}{
		{"an amount after the last", func() { m.Amounts = append(m.Amounts, QuotaAmount{Slot: 4, Limit: 7, Window: "1m"}) }},
		{"the last amount freed", func() { m.Amounts = m.Amounts[:2] }},
		{"the first budget across models", func() {
			m.BudgetsHold = true
			s.Budgets = []TenantBudget{{TenantSlug: "walletcheck-d", Limit: 500, Window: "1h"}}
		}},
		{"the last budget across models removed", func() { m.BudgetsHold, s.Budgets = false, nil }},
	} {
		step.change()
		if got := poolKey(t, s, false); got != first {
			t.Fatalf("after %s the pool is counted in\n%s\nwas\n%s", step.name, got, first)
		}
	}
	m.Amounts = append(m.Amounts, QuotaAmount{Slot: 8, Limit: 9, Window: "1h"})
	if got := poolKey(t, s, false); !strings.Contains(got, "_rule-16-match--1_") {
		t.Fatalf("with nine positions the pool is counted in %s, want it after sixteen rules", got)
	}
	if n := RuleCount(*m); n != 9 {
		t.Fatalf("rule count %d, want the nine positions in use", n)
	}
}

// While the gateways take API keys too, each tenant quota has a rule of its
// own. A third tenant quota leaves the pool's counter as it was.
func TestPoolKeepsItsCounterWithARulePerTenant(t *testing.T) {
	s := testState()
	before := poolKey(t, s, false)
	s.Models[0].Quotas = append(s.Models[0].Quotas, TenantQuota{TenantSlug: "team-c", Slot: 2, Limit: 10, Window: "1h"})
	if after := poolKey(t, s, false); after != before || !strings.Contains(after, "_rule-8-match--1_") {
		t.Fatalf("pool before %s, after a third tenant quota %s", before, after)
	}
}

// The best-effort route has its policy without any tenant quota too. Its
// pool keeps its counter when the model gets its first quota; it went from
// rule 0 to rule 1.
func TestBestEffortPoolKeepsItsCounter(t *testing.T) {
	s := bestEffortState()
	s.Models[0].Quotas = nil
	before := poolKey(t, s, true)
	s.Models[0].Quotas = []TenantQuota{{TenantSlug: "team-a", Slot: 0, Limit: 500, Window: "1d"}}
	if after := poolKey(t, s, true); after != before || !strings.Contains(after, "_rule-8-match--1_") {
		t.Fatalf("best-effort pool before %s, after the first quota %s", before, after)
	}
	// A model without any quota and off the best-effort route has no policy,
	// and neither a pool counter nor a rule count.
	s = testState()
	if n := RuleCount(s.Models[1]); n != 0 {
		t.Fatalf("rule count of a model without quotas: %d", n)
	}
	for _, c := range Counters(s) {
		if c.ModelSlug == s.Models[1].Slug {
			t.Fatalf("a model without a policy has a counter: %+v", c)
		}
	}
}

// The pool's entry in the proxy's route is named after the number of rules,
// so the routes must be built again when that number changes. The revision
// that makes Envoy Gateway do so (QuotaRevisionAnnotation) holds it: it
// changed for every model once, when the policies were first padded, though
// no quota or amount had.
func TestQuotaRevisionHoldsTheRuleCount(t *testing.T) {
	m := amountState().Models[0]
	limit, window := m.defaultBucket(false)
	// The revision as it was made before the padding.
	data, err := json.Marshal(struct {
		Rules   []TenantQuota
		Limit   int64
		Window  string
		Cost    string
		DryRun  bool          `json:",omitempty"`
		Amounts []QuotaAmount `json:",omitempty"`
		Budgets bool          `json:",omitempty"`
	}{nil, limit, window, m.CostExpression, m.DryRun, amountRules(m), m.passesBudgets()})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if old := hex.EncodeToString(sum[:6]); quotaRevision(m) == old {
		t.Fatalf("revision %s did not change with the padding", old)
	}
	before := quotaRevision(m)
	m.Amounts = append(m.Amounts, QuotaAmount{Slot: 8, Limit: 9, Window: "1h"})
	if quotaRevision(m) == before {
		t.Fatal("revision did not change with a ninth position")
	}
}
