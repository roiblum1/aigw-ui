package store

import (
	"errors"
	"testing"
	"time"

	"aigw-ui/internal/render"
)

// A standard starts on its day, as prices do, and the gateway gets it only
// while it takes tokens only: one rule for every tenant, the tenants' quotas
// as exceptions, a counter for each enabled tenant.
func TestStandardBudget(t *testing.T) {
	s, ctx := open(t)
	s.SetTokens(tokenSet)
	site, err := s.CreateCluster(ctx, Cluster{Name: "site-a", Namespace: "ai-gateway", GatewayName: "gw", AuthEnabled: true}, []byte("kubeconfig"), "")
	if err != nil {
		t.Fatal(err)
	}
	glm, err := s.CreateModel(ctx, ModelInput{Name: "glm", DefaultLimit: 1, DefaultWindow: "1d",
		Endpoints: []Endpoint{{ClusterID: site.ID, Host: "glm.glm.svc", Port: 8000}}})
	if err != nil {
		t.Fatal(err)
	}
	a, b := tenant(t, s, ctx, "team-a"), tenant(t, s, ctx, "team-b")
	if err := s.UpsertQuota(ctx, a.ID, glm, 5000, "1d", nil); err != nil {
		t.Fatal(err)
	}
	if any, _ := s.AnyStandard(ctx); any {
		t.Fatal("a standard before any was set")
	}
	if _, err := s.SetStandard(ctx, glm, &Standard{Limit: 1000, Window: "1d"}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if any, _ := s.AnyStandard(ctx); !any {
		t.Fatal("a waiting standard does not count")
	}
	if applied, err := s.ApplyDueStandards(ctx); err != nil || len(applied) != 0 {
		t.Fatalf("before its day: %+v %v", applied, err)
	}
	if _, err := s.db.Exec(ctx, `UPDATE models SET standard_next_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	applied, err := s.ApplyDueStandards(ctx)
	if err != nil || len(applied) != 1 || applied[0].Standard.Limit != 1000 {
		t.Fatalf("on its day: %+v %v", applied, err)
	}
	m, _ := s.GetModel(ctx, glm)
	if m.Standard == nil || m.StandardNext != nil {
		t.Fatalf("model %+v", m)
	}
	if all, _ := s.ListModels(ctx); len(all) != 1 || all[0].Standard == nil || all[0].QuotaRules != 1 {
		t.Fatalf("listed %+v", all)
	}

	// API keys only: the gateway does not get the standard.
	st, err := s.RenderStateWithoutKeys(ctx, site.ID)
	if err != nil || st.Models[0].Standard != nil {
		t.Fatalf("keys: %+v %v", st.Models[0], err)
	}
	// Tokens only: it does, with every enabled tenant to count.
	if _, _, err := s.CreateToken(ctx, b.ID, "b", tokenSet, time.Now()); err != nil {
		t.Fatal(err)
	}
	st, _ = s.RenderStateWithoutKeys(ctx, site.ID)
	if st.Identity.Mode != render.IdentityTokens || st.Models[0].Standard == nil || st.Models[0].Standard.Limit != 1000 ||
		len(st.Tenants) != 2 {
		t.Fatalf("tokens: %+v %+v", st.Identity, st.Models[0])
	}
	counters := map[string]bool{}
	for _, c := range render.Counters(st) {
		counters[c.TenantSlug] = c.Standard
	}
	if len(counters) != 3 || !counters["team-b"] || counters["team-a"] {
		t.Fatalf("counters %v: team-b on the standard, team-a on its quota, the pool", counters)
	}
	// Removing it waits for its day too; a waiting change can be cancelled.
	if _, err := s.SetStandard(ctx, glm, nil, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelStandard(ctx, glm); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CancelStandard(ctx, glm); !errors.Is(err, ErrNotFound) {
		t.Fatalf("nothing to cancel: %v", err)
	}
}
