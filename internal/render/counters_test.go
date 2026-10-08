package render

import (
	"testing"
	"time"
)

// The key must match what the gateway and its rate limit service produce:
// <domain>_backend_name_<ns>/<backend>_model_name_override_<model>_<rule>_<rule>_<window start>,
// where <rule> is "rule-<position>-<header>|<pattern>-match-0".
func TestCounterRedisKey(t *testing.T) {
	now := time.Unix(1791400000, 0) // 2026-10-07 15:46:40 UTC
	counters := Counters(testState())
	if len(counters) != 3 {
		t.Fatalf("got %d counters, want the pool and one per tenant quota", len(counters))
	}
	// The default bucket comes after the two rules, so it is "rule-2".
	pool := counters[0]
	wantPool := `ai-gateway-quota_backend_name_ai-gateway/glm5-3_model_name_override_glm-5.3_rule-2-match--1_rule-2-match--1_1791331200`
	if got := pool.RedisKey("", now); !pool.Pool || pool.TenantSlug != "" || got != wantPool {
		t.Errorf("pool (1d):\n got %s\nwant %s", got, wantPool)
	}
	// A rule is named after its slot, so team-a is rule 0 although it is listed second.
	a, b := counters[1], counters[2]
	wantA := `ai-gateway-quota_backend_name_ai-gateway/glm5-3_model_name_override_glm-5.3_` +
		`rule-0-x-aigw-client-id|^team-a\.[a-f0-9]+$-match-0_rule-0-x-aigw-client-id|^team-a\.[a-f0-9]+$-match-0_1791331200`
	if got := a.RedisKey("", now); got != wantA {
		t.Errorf("team-a (1d):\n got %s\nwant %s", got, wantA)
	}
	wantB := `pfx:ai-gateway-quota_backend_name_ai-gateway/glm5-3_model_name_override_glm-5.3_` +
		`rule-1-x-aigw-client-id|^team-b\.[a-f0-9]+$-match-0_rule-1-x-aigw-client-id|^team-b\.[a-f0-9]+$-match-0_1791399600`
	if got := b.RedisKey("pfx:", now); got != wantB {
		t.Errorf("team-b (1h):\n got %s\nwant %s", got, wantB)
	}
	if end := WindowEnd("1h", now); end.Unix() != 1791403200 {
		t.Errorf("hour window ends at %d, want 1791403200", end.Unix())
	}
}

// A quota the policy cannot render leaves a placeholder at its position, which
// has no counter. The quotas after it keep their position.
func TestCountersSkipInvalidQuotas(t *testing.T) {
	s := State{Namespace: "ai-gateway", Models: []Model{{
		Name: "glm", Slug: "glm", UpstreamModel: "glm",
		Quotas:   []TenantQuota{{TenantSlug: "aaa", Slot: 0, Limit: 10, Window: ""}, {TenantSlug: "bbb", Slot: 1, Limit: 10, Window: "1h"}},
		Existing: []Target{{Namespace: "models", Backend: "glm-a", Model: "glm"}, {Backend: "glm-b", Model: "glm"}},
	}}}
	var counters []Counter
	for _, c := range Counters(s) {
		if !c.Pool {
			counters = append(counters, c)
		}
	}
	if len(counters) != 2 {
		t.Fatalf("got %d counters, want bbb on each of two backends", len(counters))
	}
	if counters[0].Backend != "models/glm-a" || counters[1].Backend != "ai-gateway/glm-b" {
		t.Errorf("backends = %s, %s", counters[0].Backend, counters[1].Backend)
	}
	for _, c := range counters {
		if c.TenantSlug != "bbb" || c.stem[len(c.stem)-9:] != "-match-0_" || !contains(c.stem, "_rule-1-") {
			t.Errorf("unexpected counter %+v", c)
		}
	}
}

// Adding or removing another tenant's quota must not rename a counter: a new
// name is a counter that starts again from zero.
func TestCounterNamesSurviveOtherQuotas(t *testing.T) {
	now := time.Unix(1791400000, 0)
	state := func(quotas ...TenantQuota) State {
		return State{Namespace: "ai-gateway", Models: []Model{{Name: "glm", Slug: "glm", UpstreamModel: "glm", Quotas: quotas}}}
	}
	keyOf := func(s State, tenant string) string {
		for _, c := range Counters(s) {
			if c.TenantSlug == tenant {
				return c.RedisKey("", now)
			}
		}
		t.Fatalf("no counter for %s", tenant)
		return ""
	}
	b := TenantQuota{TenantSlug: "team-b", Slot: 1, Limit: 10, Window: "1h"}
	c := TenantQuota{TenantSlug: "team-c", Slot: 2, Limit: 10, Window: "1h"}
	before := state(TenantQuota{TenantSlug: "team-a", Slot: 0, Limit: 10, Window: "1h"}, b, c)
	// team-a is removed, and a tenant that sorts first takes its position.
	removed := state(b, c)
	reused := state(TenantQuota{TenantSlug: "aaa", Slot: 0, Limit: 10, Window: "1h"}, b, c)
	for _, tenant := range []string{"team-b", "team-c"} {
		want := keyOf(before, tenant)
		if got := keyOf(removed, tenant); got != want {
			t.Errorf("%s after a removal:\n got %s\nwant %s", tenant, got, want)
		}
		if got := keyOf(reused, tenant); got != want {
			t.Errorf("%s after the position was reused:\n got %s\nwant %s", tenant, got, want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
