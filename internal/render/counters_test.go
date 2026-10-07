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
	if len(counters) != 2 {
		t.Fatalf("got %d counters, want one per tenant quota", len(counters))
	}
	// Rules are sorted by tenant, so team-a is rule 0 although it is listed second.
	a, b := counters[0], counters[1]
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

// A quota the policy leaves out has no rule, so it must not take a position.
func TestCountersSkipInvalidQuotas(t *testing.T) {
	s := State{Namespace: "ai-gateway", Models: []Model{{
		Name: "glm", Slug: "glm", UpstreamModel: "glm",
		Quotas:   []TenantQuota{{TenantSlug: "aaa", Limit: 10, Window: ""}, {TenantSlug: "bbb", Limit: 10, Window: "1h"}},
		Existing: []Target{{Namespace: "models", Backend: "glm-a", Model: "glm"}, {Backend: "glm-b", Model: "glm"}},
	}}}
	counters := Counters(s)
	if len(counters) != 2 {
		t.Fatalf("got %d counters, want bbb on each of two backends", len(counters))
	}
	if counters[0].Backend != "models/glm-a" || counters[1].Backend != "ai-gateway/glm-b" {
		t.Errorf("backends = %s, %s", counters[0].Backend, counters[1].Backend)
	}
	for _, c := range counters {
		if c.TenantSlug != "bbb" || c.stem[len(c.stem)-9:] != "-match-0_" || !contains(c.stem, "_rule-0-") {
			t.Errorf("unexpected counter %+v", c)
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
