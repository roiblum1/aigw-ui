package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aigw-ui/internal/capacity"
	"aigw-ui/internal/render"
	"aigw-ui/internal/store"
)

// A limit outside 1 to 10 is refused before anything is stored.
func TestOvercommitLimitBounds(t *testing.T) {
	s := &Server{}
	for _, body := range []string{`{"limit":0.9}`, `{"limit":11}`, `{"limit":0}`, `{}`, `{"limit":"2"}`} {
		r := httptest.NewRequest(http.MethodPut, "/api/v1/models/m/overcommit-limit", strings.NewReader(body))
		r.SetPathValue("id", "m")
		w := httptest.NewRecorder()
		s.setOvercommitLimit(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, w.Code, w.Body)
		}
	}
}

// A model's budget is its plan's figures; one not measured says so instead
// of offering 0 as a limit.
func TestBudgetOf(t *testing.T) {
	thr := capacity.Throughput{Concurrency: 4, InputPerSecond: 1000, OutputPerSecond: 50}
	plan := capacity.MakePlan(capacity.PlanInput{Unit: "credits", Prices: &capacity.Prices{Input: 100000, Cached: 10000, Output: 400000},
		Utilization: 0.5, OvercommitLimit: 2,
		Shapes:  []capacity.ShapeInput{{Shape: capacity.Shape{Topology: "single", GPU: "NVIDIA-B300", GPUsPerInstance: 8}, Instances: 1, Measured: &thr}},
		Budgets: []capacity.Budget{{Limit: 100, Window: "1h"}}})
	b := budgetOf(capacityModel{ModelID: "m", ModelName: "glm", Unit: "credits", Plan: plan})
	if !b.Measured || b.CapacityDay != plan.CapacityDay || b.CeilingDay != 2*plan.CapacityDay || b.GivenDay != 2400 ||
		b.LeftDay != b.CeilingDay-2400 || b.ShortWindows != 1 || b.OvercommitLimit != 2 {
		t.Fatalf("budget = %+v", b)
	}
	if none := budgetOf(capacityModel{Plan: capacity.MakePlan(capacity.PlanInput{Unit: "tokens"})}); none.Measured {
		t.Fatalf("not measured: %+v", none)
	}
}

// A sentence gives what the GPUs serve to the cent, not to the credit.
func TestCents(t *testing.T) {
	if got := cents(97_868_736, "credits"); got != 97_869_000 {
		t.Fatalf("credits: %d", got)
	}
	if got := cents(97_868_736, "tokens"); got != 97_868_736 {
		t.Fatalf("tokens: %d", got)
	}
}

// A live measurement starts only once the model's name is typed.
func TestCapacityRunBody(t *testing.T) {
	b := capacityRunBody{ClusterID: "c", Namespace: "llm", Deployment: "glm", Pod: "glm-0", Mode: capacity.ModeLive}
	if err := b.check("glm-5.3"); err == nil || !strings.Contains(err.Error(), "confirm it with the model's name") {
		t.Fatalf("unconfirmed: %v", err)
	}
	b.Confirm = "GLM-5.3"
	if b.check("glm-5.3") == nil {
		t.Fatal("a name in another case confirmed it")
	}
	b.Confirm = "glm-5.3"
	if err := b.check("glm-5.3"); err != nil {
		t.Fatal(err)
	}
	// Drained needs no confirmation; a mode is needed.
	if err := (capacityRunBody{ClusterID: "c", Namespace: "llm", Deployment: "glm", Mode: capacity.ModeDrained}).check("glm-5.3"); err != nil {
		t.Fatal(err)
	}
	if (capacityRunBody{ClusterID: "c", Namespace: "llm", Deployment: "glm"}).check("glm-5.3") == nil {
		t.Fatal("no mode accepted")
	}
}

// A site an operator drained counts for nothing in what its model can be
// given; one a measurement drained counts in full: the drain lasts the run.
func TestReadyInstances(t *testing.T) {
	two := 2.0
	d := store.Deployment{ModelID: "glm", ClusterID: "a", Instances: &two}
	run := []store.CapacityRun{{ModelID: "glm", ClusterID: "a", DrainedSite: true}}
	if readyInstances(d, nil) != 2 {
		t.Fatal("not drained")
	}
	d.Drained = true
	if readyInstances(d, nil) != 0 || readyInstances(d, run) != 2 {
		t.Fatalf("drained: by an operator %v, by a measurement %v", readyInstances(d, nil), readyInstances(d, run))
	}
	// A run on the site that found it drained already did not drain it.
	run[0].DrainedSite = false
	if readyInstances(d, run) != 0 {
		t.Fatal("an operator's drain counted")
	}
	if readyInstances(store.Deployment{}, nil) != 0 {
		t.Fatal("unknown ready count")
	}
}

// While a tenant has a budget across models, a model with prices has one
// more rule, which lets such tenants through; prices in a dry run charge no
// budget and add none.
func TestRuleCountsWithBudgets(t *testing.T) {
	m := store.Model{QuotaRules: 2, QuotaAmounts: 1, Standard: &store.Standard{Limit: 10, Window: "1h"}, Prices: &store.Price{Input: 1}}
	count := func(m store.Model, budgets bool) int {
		now, _, _ := ruleCounts(m, render.IdentityTokens, 50, budgets)
		return now
	}
	if n := count(m, false); n != 2 {
		t.Fatalf("without budgets: %d rules, want the standard's and the amount's", n)
	}
	if n := count(m, true); n != 3 {
		t.Fatalf("with budgets: %d rules", n)
	}
	m.PriceDryRun = true
	if n := count(m, true); n != 2 {
		t.Fatalf("prices in a dry run: %d rules", n)
	}
}
