package store

import (
	"errors"
	"testing"
	"time"

	"aigw-ui/internal/capacity"
)

func TestCapacityRuns(t *testing.T) {
	s, ctx := open(t)
	glm := model(t, s, ctx, "glm", false)
	site, err := s.CreateCluster(ctx, Cluster{Name: "site-a", Namespace: "ai-gateway", GatewayName: "gw"}, []byte("kubeconfig"), "")
	if err != nil {
		t.Fatal(err)
	}
	set := capacity.Defaults(0)
	b300 := capacity.Shape{Topology: "single", GPU: "NVIDIA-B300", GPUsPerInstance: 8}
	h200 := capacity.Shape{Topology: "single", GPU: "NVIDIA-H200", GPUsPerInstance: 8}
	const first, second, third = "00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000003"
	newRun := func(id string, shape capacity.Shape) NewRun {
		return NewRun{ID: id, ModelID: glm, ClusterID: site.ID, Namespace: "llm", Deployment: "glm", Instance: "glm-0", JobName: "aigw-capacity-" + id[len(id)-1:], Shape: shape, Settings: set}
	}
	if err := s.CreateCapacityRun(ctx, newRun(first, b300)); err != nil {
		t.Fatal(err)
	}
	// One running measurement per model.
	if err := s.CreateCapacityRun(ctx, newRun(second, h200)); !errors.Is(err, ErrConflict) {
		t.Fatalf("second running run: %v", err)
	}

	steps := []capacity.Step{
		{Concurrency: 1, Requests: 60, DurationSeconds: 300, InputTokens: 2_000_000, TTFTP95: 900, ITLP95: 20},
		{Concurrency: 2, Requests: 60, DurationSeconds: 300, InputTokens: 4_000_000, TTFTP95: 9000, ITLP95: 20},
	}
	if err := s.UpdateCapacityRun(ctx, first, &capacity.Dataset{Sessions: 200, PromptMean: 99000}, steps); err != nil {
		t.Fatal(err)
	}
	running, err := s.RunningCapacityRuns(ctx)
	if err != nil || len(running) != 1 || running[0].ModelName != "glm" || running[0].ClusterName != "site-a" || len(running[0].Steps) != 2 ||
		running[0].Dataset.PromptMean != 99000 || running[0].Shape != b300 || running[0].Deployment != "glm" {
		t.Fatalf("running = %+v, %v", running, err)
	}
	if latest, _ := s.LatestCapacityRuns(ctx); len(latest) != 0 {
		t.Fatalf("latest while running = %+v", latest)
	}

	if err := s.FinishCapacityRun(ctx, first, RunSucceeded, "at 2 in flight: too slow"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishCapacityRun(ctx, first, RunFailed, "late"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("finished twice: %v", err)
	}
	if err := s.UpdateCapacityRun(ctx, first, nil, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update after the end: %v", err)
	}
	run, err := s.GetCapacityRun(ctx, first)
	if err != nil || run.Status != RunSucceeded || run.FinishedAt == nil || run.Result.Best == nil || run.Result.Best.Concurrency != 1 ||
		run.Settings.StepSeconds != set.StepSeconds || len(run.Result.Steps) != 2 {
		t.Fatalf("run = %+v, %v", run, err)
	}

	// A second shape keeps its own latest result; a newer run of the first
	// shape that found nothing does not replace the first.
	for _, r := range []struct {
		id    string
		shape capacity.Shape
		steps []capacity.Step
	}{
		{second, h200, steps[:1]},
		{third, b300, []capacity.Step{{Concurrency: 1, Requests: 60, TTFTP95: 99999}}},
	} {
		if err := s.CreateCapacityRun(ctx, newRun(r.id, r.shape)); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateCapacityRun(ctx, r.id, nil, r.steps); err != nil {
			t.Fatal(err)
		}
		if err := s.FinishCapacityRun(ctx, r.id, RunSucceeded, ""); err != nil {
			t.Fatal(err)
		}
	}
	latest, err := s.LatestCapacityRuns(ctx)
	if err != nil || len(latest) != 2 {
		t.Fatalf("latest = %+v, %v", latest, err)
	}
	got := map[string]string{latest[0].Shape.GPU: latest[0].ID, latest[1].Shape.GPU: latest[1].ID}
	if got["NVIDIA-B300"] != first || got["NVIDIA-H200"] != second {
		t.Fatalf("latest by shape = %v", got)
	}
	runs, err := s.ListCapacityRuns(ctx, glm)
	if err != nil || len(runs) != 3 || runs[0].ID != third {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	if err := s.DeleteCapacityRun(ctx, third); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetCapacityRun(ctx, third); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted run: %v", err)
	}
}

// Discovery records each deployment and its shape, and forgets the ones
// that are gone.
func TestDeploymentsFromDiscovery(t *testing.T) {
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
	report := func(deployments ...Deployment) {
		t.Helper()
		if err := s.ApplyCapacity(ctx, site.ID, []Capacity{{Model: "glm", Capacity: 2, Step: 1, Known: true, Deployments: deployments}}); err != nil {
			t.Fatal(err)
		}
	}
	report(
		Deployment{Namespace: "llm", Name: "glm-b300", Topology: "single", GPU: "NVIDIA-B300", GPUsPerInstance: 8, Instances: &two},
		Deployment{Namespace: "llm", Name: "glm-pd", Topology: "prefill-decode", GPU: "NVIDIA-H200", GPUsPerInstance: 24},
	)
	all, err := s.ListDeployments(ctx)
	if err != nil || len(all) != 2 || all[0].Name != "glm-b300" || *all[0].Instances != 2 || all[0].ModelID != glm || all[0].ClusterName != "site-a" ||
		all[1].Instances != nil || all[1].GPUsPerInstance != 24 {
		t.Fatalf("deployments = %+v, %v", all, err)
	}
	if d, err := s.GetDeployment(ctx, glm, site.ID, "llm", "glm-pd"); err != nil || d.Topology != "prefill-decode" {
		t.Fatalf("one deployment = %+v, %v", d, err)
	}
	report(Deployment{Namespace: "llm", Name: "glm-b300", Topology: "single", GPU: "NVIDIA-B300", GPUsPerInstance: 8, Instances: &two})
	if all, _ := s.ListDeployments(ctx); len(all) != 1 {
		t.Fatalf("after one is gone: %+v", all)
	}
	// A cluster that reports nothing has none left.
	if err := s.ApplyCapacity(ctx, site.ID, nil); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.ListDeployments(ctx); len(all) != 0 {
		t.Fatalf("after all are gone: %+v", all)
	}
}

// A capacity that changes basis changes unit, and is taken at once. Within one
// basis it still rises one instance per poll.
func TestCapacityBasisChange(t *testing.T) {
	s, ctx := open(t)
	glm := model(t, s, ctx, "glm", false)
	site, err := s.CreateCluster(ctx, Cluster{Name: "site-a", Namespace: "ai-gateway", GatewayName: "gw"}, []byte("kubeconfig"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO model_endpoints (model_id, cluster_id, upstream_model, source, backends) VALUES ($1, $2, '', 'discovered', '[]')`, glm, site.ID); err != nil {
		t.Fatal(err)
	}
	applied := func(c Capacity) (float64, string) {
		t.Helper()
		if err := s.ApplyCapacity(ctx, site.ID, []Capacity{c}); err != nil {
			t.Fatal(err)
		}
		models, err := s.ListModels(ctx)
		if err != nil {
			t.Fatal(err)
		}
		e := models[0].Endpoints[0].Capacity
		return *e.Weight, e.Basis
	}
	if w, b := applied(Capacity{Model: "glm", Capacity: 2, Step: 1, Known: true}); w != 2 || b != BasisDeclared {
		t.Fatalf("first reading: %v %s", w, b)
	}
	if w, _ := applied(Capacity{Model: "glm", Capacity: 4, Step: 1, Known: true}); w != 3 {
		t.Fatalf("one instance up: %v", w)
	}
	// Measured: 2 instances of 20 thousand tokens a second, taken at once.
	if w, b := applied(Capacity{Model: "glm", Capacity: 40, Step: 20, Known: true, Basis: BasisMeasured}); w != 40 || b != BasisMeasured {
		t.Fatalf("measured: %v %s", w, b)
	}
	// And back to the annotation at once, too.
	if w, b := applied(Capacity{Model: "glm", Capacity: 2, Step: 1, Known: true}); w != 2 || b != BasisDeclared {
		t.Fatalf("declared again: %v %s", w, b)
	}
}

func TestGPUPricesAndTargetUtilization(t *testing.T) {
	s, ctx := open(t)
	glm := model(t, s, ctx, "glm", false)
	if err := s.SetGPUPrice(ctx, GPUPrice{GPU: "NVIDIA-H200", USDPerHour: 3.2, Note: "3 years"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetGPUPrice(ctx, GPUPrice{GPU: "NVIDIA-H200", USDPerHour: 3.5}); err != nil {
		t.Fatal(err)
	}
	prices, err := s.ListGPUPrices(ctx)
	if err != nil || len(prices) != 1 || prices[0].USDPerHour != 3.5 || prices[0].Note != "" {
		t.Fatalf("prices = %+v, %v", prices, err)
	}
	if err := s.DeleteGPUPrice(ctx, "NVIDIA-H200"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteGPUPrice(ctx, "NVIDIA-H200"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}

	if targets, _ := s.CapacityTargets(ctx); targets[glm] != (CapacityTarget{Utilization: 0.6, OvercommitLimit: 1.5}) {
		t.Fatalf("default target = %v", targets[glm])
	}
	if name, err := s.SetTargetUtilization(ctx, glm, 0.75); err != nil || name != "glm" {
		t.Fatalf("set: %q %v", name, err)
	}
	if name, err := s.SetOvercommitLimit(ctx, glm, 2); err != nil || name != "glm" {
		t.Fatalf("set limit: %q %v", name, err)
	}
	if targets, _ := s.CapacityTargets(ctx); targets[glm] != (CapacityTarget{Utilization: 0.75, OvercommitLimit: 2}) {
		t.Fatalf("target = %v", targets[glm])
	}
	// The database keeps a limit below 1 out, whatever the caller checked.
	if _, err := s.SetOvercommitLimit(ctx, glm, 0.5); err == nil {
		t.Fatal("a limit below 1 was stored")
	}
}

func TestUsageSinceAndAllQuotas(t *testing.T) {
	s, ctx := open(t)
	glm := model(t, s, ctx, "glm", false)
	a, b := tenant(t, s, ctx, "team-a"), tenant(t, s, ctx, "team-b")
	day := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	err := s.RecordUsage(ctx, func(map[UsageKey]UsageCursor) ([]UsageDelta, []UsageCursor, error) {
		return []UsageDelta{
			{UsageKey: UsageKey{a.ID, glm}, Hour: day.Add(9 * time.Hour), Unit: UnitTokens, Used: 100, BestEffort: 20},
			{UsageKey: UsageKey{b.ID, glm}, Hour: day.Add(10 * time.Hour), Unit: UnitTokens, Used: 50},
			{UsageKey: UsageKey{a.ID, glm}, Hour: day.Add(33 * time.Hour), Unit: UnitTokens, Used: 90},
		}, nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	used, err := s.UsageSince(ctx, day)
	if err != nil || len(used) != 1 || used[0].Busiest != 170 || used[0].Total != 260 || used[0].ModelID != glm {
		t.Fatalf("used = %+v, %v", used, err)
	}

	if err := s.UpsertQuota(ctx, a.ID, glm, 1000, "1h", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertQuota(ctx, b.ID, glm, 5000, "1d", nil); err != nil {
		t.Fatal(err)
	}
	quotas, err := s.AllQuotas(ctx)
	if err != nil || len(quotas) != 2 {
		t.Fatalf("quotas = %+v, %v", quotas, err)
	}
}
