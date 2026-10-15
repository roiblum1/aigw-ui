package store

import (
	"testing"
	"time"
)

// A model's market entry: chosen by hand, or named by its deployments'
// annotation; and prices that record they came from the market.
func TestMarketColumns(t *testing.T) {
	s, ctx := open(t)
	glm := model(t, s, ctx, "glm", false)
	site, err := s.CreateCluster(ctx, Cluster{Name: "site-a", Namespace: "ai-gateway", GatewayName: "gw"}, []byte("kubeconfig"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO model_endpoints (model_id, cluster_id, upstream_model, source, backends) VALUES ($1, $2, '', 'discovered', '[]')`, glm, site.ID); err != nil {
		t.Fatal(err)
	}
	two := 2.0
	if err := s.ApplyCapacity(ctx, site.ID, []Capacity{{Model: "glm", Capacity: 2, Step: 1, Known: true, Deployments: []Deployment{
		{Namespace: "llm", Name: "glm-b300", Topology: "single", Instances: &two, MarketID: "z-ai/glm-5.3"},
		{Namespace: "llm", Name: "glm-h200", Topology: "single", Instances: &two, MarketID: "z-ai/glm-5.3"},
		{Namespace: "llm", Name: "glm-old", Topology: "single", Instances: &two},
	}}}); err != nil {
		t.Fatal(err)
	}
	get := func() Model {
		t.Helper()
		models, err := s.ListModels(ctx)
		if err != nil || len(models) != 1 {
			t.Fatal(models, err)
		}
		return models[0]
	}
	if m := get(); m.MarketIDSet != nil || len(m.MarketAnnotations) != 1 || m.MarketAnnotations[0] != "z-ai/glm-5.3" {
		t.Fatalf("annotations: %+v %v", m.MarketAnnotations, m.MarketIDSet)
	}
	if deployments, _ := s.ListDeployments(ctx); deployments[0].MarketID != "z-ai/glm-5.3" {
		t.Fatalf("deployment: %+v", deployments[0])
	}

	for _, id := range []*string{ptr("z-ai/glm-5.3:batch"), ptr(""), nil} {
		if name, err := s.SetMarketID(ctx, glm, id); err != nil || name != "glm" {
			t.Fatal(name, err)
		}
		got := get().MarketIDSet
		if (id == nil) != (got == nil) || (id != nil && *got != *id) {
			t.Fatalf("set %v, got %v", id, got)
		}
	}

	p := Price{Input: 280000, Cached: 56000, Output: 880000, EffectiveAt: time.Now().Add(time.Hour), Market: "z-ai/glm-5.3@2026-10-12"}
	if _, err := s.SetPendingPrice(ctx, glm, p, false); err != nil {
		t.Fatal(err)
	}
	if m := get(); m.PendingPrices == nil || m.PendingPrices.Market != p.Market {
		t.Fatalf("pending: %+v", m.PendingPrices)
	}
	if history, _ := s.ListPrices(ctx, glm); len(history) != 1 || history[0].Market != p.Market {
		t.Fatalf("history: %+v", history)
	}
}

func ptr(s string) *string { return &s }
