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
	profile, err := s.ActiveProfile(ctx)
	if err != nil || profile.Version != 1 || profile.Sweep.BisectSteps != 2 || profile.Workload.SharedPrefixTokens != 32000 {
		t.Fatalf("seeded profile = %+v, %v", profile, err)
	}
	set := profile.Settings(0, capacity.ModeDrained)
	b300 := capacity.Shape{Topology: "single", GPU: "NVIDIA-B300", GPUsPerInstance: 8}
	h200 := capacity.Shape{Topology: "single", GPU: "NVIDIA-H200", GPUsPerInstance: 8}
	const first, second, third, legacy = "00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002",
		"00000000-0000-4000-8000-000000000003", "00000000-0000-4000-8000-000000000004"
	newRun := func(id, deployment, spec string, shape capacity.Shape) NewRun {
		return NewRun{ID: id, ModelID: glm, ClusterID: site.ID, Namespace: "llm", Deployment: deployment, Instance: deployment + "-0",
			JobName: "aigw-capacity-" + id[len(id)-1:], Shape: shape, Settings: set, SpecHash: spec, Profile: profile,
			Phase: PhaseDraining, StartedBy: ByCampaign, InstancesDuring: 1}
	}
	if err := s.CreateCapacityRun(ctx, newRun(first, "glm-b300", "s1", b300)); err != nil {
		t.Fatal(err)
	}
	// One running measurement per model.
	if err := s.CreateCapacityRun(ctx, newRun(second, "glm-h200", "h1", h200)); !errors.Is(err, ErrConflict) {
		t.Fatalf("second running run: %v", err)
	}
	if err := s.SetRunPhase(ctx, first, PhaseMeasuring); err != nil {
		t.Fatal(err)
	}

	steps := []capacity.Step{
		{Concurrency: 1, Requests: 60, DurationSeconds: 300, InputTokens: 2_000_000, TTFTP95: 900, ITLP95: 20},
		{Concurrency: 2, Requests: 60, DurationSeconds: 300, InputTokens: 4_000_000, TTFTP95: 9000, ITLP95: 20},
	}
	// The Job fitted the settings to the instance: the run keeps the fitted
	// ones, and later updates without settings leave them.
	fitted := set
	fitted.MaxPromptTokens, fitted.MaxOutputTokens = 6692, 1500
	if err := s.UpdateCapacityRun(ctx, first, &fitted, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateCapacityRun(ctx, first, nil, &capacity.Dataset{Sessions: 200, PromptMean: 99000}, steps); err != nil {
		t.Fatal(err)
	}
	running, err := s.RunningCapacityRuns(ctx)
	if err != nil || len(running) != 1 || running[0].ModelName != "glm" || running[0].ClusterName != "site-a" || len(running[0].Steps) != 2 ||
		running[0].Dataset.PromptMean != 99000 || running[0].Shape != b300 || running[0].Deployment != "glm-b300" ||
		running[0].Phase != PhaseMeasuring || running[0].StartedBy != ByCampaign || running[0].Mode != capacity.ModeDrained ||
		running[0].SpecHash != "s1" || *running[0].ProfileVersion != 1 || *running[0].ProfileID != profile.ID {
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
	if err := s.UpdateCapacityRun(ctx, first, nil, nil, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update after the end: %v", err)
	}
	run, err := s.GetCapacityRun(ctx, first)
	if err != nil || run.Status != RunSucceeded || run.FinishedAt == nil || run.Result.Best == nil || run.Result.Best.Concurrency != 1 ||
		run.Settings.StepSeconds != set.StepSeconds || run.Settings.MaxPromptTokens != 6692 || len(run.Result.Steps) != 2 || run.PerInstance() == nil {
		t.Fatalf("run = %+v, %v", run, err)
	}

	// A second LLMI keeps its own latest result; a newer run of the first
	// that found nothing does not replace it; a run of a shape from before
	// 0.16, with no spec, counts for nothing.
	for _, r := range []struct {
		id, deployment, spec string
		shape                capacity.Shape
		steps                []capacity.Step
	}{
		{second, "glm-h200", "h1", h200, steps[:1]},
		{third, "glm-b300", "s1", b300, []capacity.Step{{Concurrency: 1, Requests: 60, TTFTP95: 99999}}},
		{legacy, "glm-b300", "", b300, steps[:1]},
	} {
		n := newRun(r.id, r.deployment, r.spec, r.shape)
		if r.spec == "" {
			n.Profile = capacity.Profile{}
		}
		if err := s.CreateCapacityRun(ctx, n); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateCapacityRun(ctx, r.id, nil, nil, r.steps); err != nil {
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
	got := map[string]string{latest[0].Deployment: latest[0].ID, latest[1].Deployment: latest[1].ID}
	if got["glm-b300"] != first || got["glm-h200"] != second {
		t.Fatalf("latest by LLMI = %v", got)
	}
	if err := s.DeleteCapacityRun(ctx, legacy); err != nil {
		t.Fatal(err)
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

// A drained run drains its site in the transaction that records it, and
// restores it in the one that ends it: no drain outlives its run, and none
// is made for a run that is not recorded.
func TestDrainedRun(t *testing.T) {
	s, ctx := open(t)
	glm := model(t, s, ctx, "glm", false)
	var sites []string
	for _, name := range []string{"site-a", "site-b"} {
		c, err := s.CreateCluster(ctx, Cluster{Name: name, Namespace: "ai-gateway", GatewayName: "gw"}, []byte("kubeconfig"), "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(ctx, `UPDATE clusters SET fleet_enabled = true WHERE id = $1;`, c.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(ctx, `INSERT INTO model_endpoints (model_id, cluster_id, upstream_model, source, backends, capacity_applied)
			VALUES ($1, $2, '', 'discovered', '[]', 2)`, glm, c.ID); err != nil {
			t.Fatal(err)
		}
		sites = append(sites, c.ID)
	}
	drained := func(site string) bool {
		t.Helper()
		var d bool
		if err := s.db.QueryRow(ctx, `SELECT drained FROM model_endpoints WHERE model_id = $1 AND cluster_id = $2`, glm, site).Scan(&d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	profile, _ := s.ActiveProfile(ctx)
	run := func(id, site string) NewRun {
		return NewRun{ID: id, ModelID: glm, ClusterID: site, Namespace: "llm", Deployment: "glm", JobName: "aigw-capacity-" + id[len(id)-1:],
			Settings: profile.Settings(0, capacity.ModeDrained), SpecHash: "s", Profile: profile, Phase: PhaseDraining, DrainedSite: true}
	}
	const first, second, third = "00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000003"
	if err := s.CreateCapacityRun(ctx, run(first, sites[0])); err != nil || !drained(sites[0]) {
		t.Fatalf("recorded: %v, drained %v", err, drained(sites[0]))
	}
	// A second run of the model is refused, and its site is not drained.
	if err := s.CreateCapacityRun(ctx, run(second, sites[1])); !errors.Is(err, ErrConflict) || drained(sites[1]) {
		t.Fatalf("second: %v, drained %v", err, drained(sites[1]))
	}
	if err := s.FinishCapacityRun(ctx, first, RunFailed, "gave up"); err != nil || drained(sites[0]) {
		t.Fatalf("ended: %v, drained %v", err, drained(sites[0]))
	}
	// With the other site drained by an operator, this one is the last: the
	// run is refused and not recorded.
	if _, _, err := s.SetDrained(ctx, glm, sites[1], true); err != nil {
		t.Fatal(err)
	}
	// An operator's drain of the last site is refused too, with the names
	// its refusal shows (they were empty up to 0.29.0).
	if m, c, err := s.SetDrained(ctx, glm, sites[0], true); !errors.Is(err, ErrLastSite) || m != "glm" || c != "site-a" || drained(sites[0]) {
		t.Fatalf("operator drain of the last site: %q %q %v, drained %v", m, c, err, drained(sites[0]))
	}
	if err := s.CreateCapacityRun(ctx, run(third, sites[0])); !errors.Is(err, ErrLastSite) || drained(sites[0]) {
		t.Fatalf("last site: %v, drained %v", err, drained(sites[0]))
	}
	if _, err := s.GetCapacityRun(ctx, third); !errors.Is(err, ErrNotFound) {
		t.Fatalf("refused run recorded: %v", err)
	}
	// A run that did not drain its site leaves an operator's drain alone.
	n := run(third, sites[1])
	n.DrainedSite = false
	if err := s.CreateCapacityRun(ctx, n); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishCapacityRun(ctx, third, RunSucceeded, ""); err != nil || !drained(sites[1]) {
		t.Fatalf("operator's drain: %v, drained %v", err, drained(sites[1]))
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
		Deployment{Namespace: "llm", Name: "glm-b300", Topology: "single", GPU: "NVIDIA-B300", GPUsPerInstance: 8, Instances: &two,
			SpecHash: "abc", Quantization: "modelopt_fp4", LMCache: true, DecodeReplicas: 2},
		Deployment{Namespace: "llm", Name: "glm-pd", Topology: "prefill-decode", GPU: "NVIDIA-H200", GPUsPerInstance: 24},
	)
	all, err := s.ListDeployments(ctx)
	if err != nil || len(all) != 2 || all[0].Name != "glm-b300" || *all[0].Instances != 2 || all[0].ModelID != glm || all[0].ClusterName != "site-a" ||
		all[1].Instances != nil || all[1].GPUsPerInstance != 24 || all[0].SpecHash != "abc" || all[0].Quantization != "modelopt_fp4" ||
		!all[0].LMCache || all[0].DecodeReplicas != 2 || all[1].DecodeReplicas != 1 {
		t.Fatalf("deployments = %+v, %v", all, err)
	}
	if d, err := s.GetDeployment(ctx, glm, site.ID, "llm", "glm-pd"); err != nil || d.Topology != "prefill-decode" {
		t.Fatalf("one deployment = %+v, %v", d, err)
	}
	// No hash this time, as during a rollout: the one it had stays.
	report(Deployment{Namespace: "llm", Name: "glm-b300", Topology: "single", GPU: "NVIDIA-B300", GPUsPerInstance: 8, Instances: &two})
	if all, _ := s.ListDeployments(ctx); len(all) != 1 || all[0].SpecHash != "abc" {
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
	err := s.RecordUsage(ctx, func(Before) (Look, error) {
		return Look{Usage: []UsageDelta{
			{UsageKey: UsageKey{a.ID, glm}, Hour: day.Add(9 * time.Hour), Unit: UnitTokens, Used: 100, BestEffort: 20},
			{UsageKey: UsageKey{b.ID, glm}, Hour: day.Add(10 * time.Hour), Unit: UnitTokens, Used: 50},
			{UsageKey: UsageKey{a.ID, glm}, Hour: day.Add(33 * time.Hour), Unit: UnitTokens, Used: 90},
		}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	used, err := s.UsageSince(ctx, day)
	if err != nil || len(used) != 1 || used[0].Busiest != 170 || used[0].Total != 260 || used[0].ModelID != glm {
		t.Fatalf("used = %+v, %v", used, err)
	}
	// By the hour, over the tenants, best-effort included.
	hours, err := s.HourlyUse(ctx, day.Add(10*time.Hour))
	if err != nil || len(hours) != 2 || hours[0].Used != 50 || !hours[0].Hour.Equal(day.Add(10*time.Hour)) || hours[1].Used != 90 || hours[1].Unit != UnitTokens {
		t.Fatalf("hours = %+v, %v", hours, err)
	}

	if err := s.UpsertQuota(ctx, a.ID, glm, 1000, "1h", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertQuota(ctx, b.ID, glm, 5000, "1d", nil); err != nil {
		t.Fatal(err)
	}
	// team-b also has a budget across models, as a tenant could before
	// 0.25.0: its quota is marked, for the planner to leave out.
	if _, err := s.db.Exec(ctx, `UPDATE tenants SET budget_credits = 100, budget_window = '1d' WHERE id = $1`, b.ID); err != nil {
		t.Fatal(err)
	}
	quotas, err := s.AllQuotas(ctx)
	if err != nil || len(quotas) != 2 {
		t.Fatalf("quotas = %+v, %v", quotas, err)
	}
	for _, q := range quotas {
		if q.Budgeted != (q.TenantID == b.ID) {
			t.Fatalf("quota of %s: budgeted %v", q.TenantID, q.Budgeted)
		}
	}
}

// Editing the profile adds a version and makes it the active one; the earlier
// versions stay.
func TestProfiles(t *testing.T) {
	s, ctx := open(t)
	p, err := s.ActiveProfile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p.SLO.TTFTMillis, p.Note = 3000, "tighter"
	v2, err := s.AddProfileVersion(ctx, p)
	if err != nil || v2.Version != 2 || !v2.Active || v2.SLO.TTFTMillis != 3000 || v2.ID == p.ID {
		t.Fatalf("v2 = %+v, %v", v2, err)
	}
	all, err := s.ListProfiles(ctx)
	if err != nil || len(all) != 2 || all[0].Version != 2 || all[1].Active || all[1].SLO.TTFTMillis != 5000 {
		t.Fatalf("profiles = %+v, %v", all, err)
	}
	if active, _ := s.ActiveProfile(ctx); active.Version != 2 {
		t.Fatalf("active = %d", active.Version)
	}
}
