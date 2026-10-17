package store

import (
	"reflect"
	"testing"
	"time"
)

// The states of every cluster built from one read of what they have in
// common are the states each cluster's own read gives, and one state's
// lists are its own.
func TestRenderStatesShareOneRead(t *testing.T) {
	s, ctx := open(t)
	s.SetTokens(tokenSet)
	a, err := s.CreateCluster(ctx, Cluster{Name: "site-a", Namespace: "ai-gateway", GatewayName: "gw", AuthEnabled: true}, []byte("kubeconfig"), "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateCluster(ctx, Cluster{Name: "site-b", Namespace: "ai-gateway", GatewayName: "gw", AuthEnabled: true}, []byte("kubeconfig"), "")
	if err != nil {
		t.Fatal(err)
	}
	glm, err := s.CreateModel(ctx, ModelInput{Name: "glm", DefaultLimit: 1, DefaultWindow: "1d",
		Endpoints: []Endpoint{{ClusterID: a.ID, Host: "glm.glm.svc", Port: 8000}, {ClusterID: b.ID, Host: "glm.glm.svc", Port: 8000}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateModel(ctx, ModelInput{Name: "small", DefaultLimit: 1, DefaultWindow: "1d",
		Endpoints: []Endpoint{{ClusterID: b.ID, Host: "small.small.svc", Port: 8000}}}); err != nil {
		t.Fatal(err)
	}
	budget, quota := tenant(t, s, ctx, "team-a"), tenant(t, s, ctx, "team-b")
	if _, err := s.SetTenantBudget(ctx, budget.ID, &TenantBudget{Credits: 300_000, Window: "1d"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertQuota(ctx, quota.ID, glm, 1000, "1h", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetTenantModels(ctx, quota.ID, []string{glm}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateToken(ctx, budget.ID, "t", tokenSet, time.Now()); err != nil {
		t.Fatal(err)
	}

	clusters, states, err := s.RenderStatesWithoutKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(clusters) != 2 || len(states) != 2 {
		t.Fatalf("%d clusters, %d states", len(clusters), len(states))
	}
	for i, c := range clusters {
		own, err := s.RenderStateWithoutKeys(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(states[i], own) {
			t.Errorf("%s from the shared read:\n%+v\nfrom its own:\n%+v", c.Name, states[i], own)
		}
	}
	if len(states[0].Models) != 1 || len(states[1].Models) != 2 || len(states[0].Budgets) != 1 || len(states[0].Models[0].Amounts) != 1 {
		t.Fatalf("site-a %+v, site-b %+v", states[0], states[1])
	}
	states[0].Budgets[0].Limit = 1
	if states[1].Budgets[0].Limit != 300_000 {
		t.Error("a change to one state's budgets showed in the other's")
	}
}
