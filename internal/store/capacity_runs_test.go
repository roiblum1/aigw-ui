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
	const first, second = "00000000-0000-4000-8000-000000000001", "00000000-0000-4000-8000-000000000002"
	if err := s.CreateCapacityRun(ctx, first, glm, site.ID, "llm", "aigw-capacity-1", "glm-0", set); err != nil {
		t.Fatal(err)
	}
	// One running measurement per model.
	if err := s.CreateCapacityRun(ctx, second, glm, site.ID, "llm", "aigw-capacity-2", "glm-0", set); !errors.Is(err, ErrConflict) {
		t.Fatalf("second running run: %v", err)
	}

	steps := []capacity.Step{
		{Concurrency: 1, Requests: 20, DurationSeconds: 300, InputTokens: 2_000_000, TTFTP95: 900, ITLP95: 20},
		{Concurrency: 2, Requests: 40, DurationSeconds: 300, InputTokens: 4_000_000, TTFTP95: 9000, ITLP95: 20},
	}
	if err := s.UpdateCapacityRun(ctx, first, &capacity.Dataset{Sessions: 200, PromptMean: 99000}, steps); err != nil {
		t.Fatal(err)
	}
	running, err := s.RunningCapacityRuns(ctx)
	if err != nil || len(running) != 1 || running[0].ModelName != "glm" || running[0].ClusterName != "site-a" || len(running[0].Steps) != 2 || running[0].Dataset.PromptMean != 99000 {
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
	if err != nil || run.Status != RunSucceeded || run.FinishedAt == nil || run.Result.Best == nil || run.Result.Best.Concurrency != 1 || run.Settings.StepSeconds != set.StepSeconds {
		t.Fatalf("run = %+v, %v", run, err)
	}

	// A newer run that found nothing does not replace the latest result.
	if err := s.CreateCapacityRun(ctx, second, glm, site.ID, "llm", "aigw-capacity-2", "glm-0", set); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateCapacityRun(ctx, second, nil, []capacity.Step{{Concurrency: 1, Requests: 1, TTFTP95: 99999}}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishCapacityRun(ctx, second, RunSucceeded, "no step met the targets"); err != nil {
		t.Fatal(err)
	}
	latest, err := s.LatestCapacityRuns(ctx)
	if err != nil || len(latest) != 1 || latest[0].ID != first {
		t.Fatalf("latest = %+v, %v", latest, err)
	}
	runs, err := s.ListCapacityRuns(ctx, glm)
	if err != nil || len(runs) != 2 || runs[0].ID != second {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	if err := s.DeleteCapacityRun(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetCapacityRun(ctx, second); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted run: %v", err)
	}
}

func TestBusiestDaysAndAllQuotas(t *testing.T) {
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
	busiest, err := s.BusiestDays(ctx, day)
	if err != nil || len(busiest) != 1 || busiest[0].Used != 170 || !busiest[0].Day.Equal(day) || busiest[0].ModelID != glm {
		t.Fatalf("busiest = %+v, %v", busiest, err)
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
