package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aigw-ui/internal/capacity"
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
