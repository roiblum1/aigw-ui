package store

import (
	"errors"
	"testing"
	"time"
)

func TestUsageHistory(t *testing.T) {
	s, ctx := open(t)
	glm := model(t, s, ctx, "glm", false)
	a, b := tenant(t, s, ctx, "team-a"), tenant(t, s, ctx, "team-b")
	day := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	ka, kb := UsageKey{TenantID: a.ID, ModelID: glm}, UsageKey{TenantID: b.ID, ModelID: glm}

	record := func(found Look) Before {
		t.Helper()
		var seen Before
		err := s.RecordUsage(ctx, func(b Before) (Look, error) {
			seen = b
			return found, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return seen
	}
	// A cursor from before 0.30.0 is handed to the first look only.
	if _, err := s.db.Exec(ctx, `INSERT INTO usage_cursors (tenant_id, model_id, window_size, window_start, used, best_effort)
		VALUES ($1, $2, '1d', $3, 40, 0)`, a.ID, glm, day); err != nil {
		t.Fatal(err)
	}
	cursor := CounterCursor{Key: "k-a", UsageKey: ka, Kind: KindUsed, Unit: UnitCredits, Window: "1d", WindowStart: day, Value: 150}
	if seen := record(Look{
		Usage: []UsageDelta{
			{UsageKey: ka, Hour: day.Add(9*time.Hour + 20*time.Minute), Unit: UnitCredits, Used: 100},
			{UsageKey: ka, Hour: day.Add(9*time.Hour + 40*time.Minute), Unit: UnitCredits, Used: 50, BestEffort: 7},
			{UsageKey: ka, Hour: day.Add(30 * time.Hour), Unit: UnitCredits, Used: 5},
			{UsageKey: kb, Hour: day.Add(9 * time.Hour), Unit: UnitTokens, Used: 900},
		},
		Tokens: []TokenDelta{
			{UsageKey: ka, Hour: day.Add(9*time.Hour + 20*time.Minute), Requests: 2, Input: 3000, Cached: 2000, Output: 100},
			{UsageKey: ka, Hour: day.Add(9*time.Hour + 50*time.Minute), Requests: 1, Input: 500, Output: 20},
			{UsageKey: kb, Hour: day.Add(33 * time.Hour), Requests: 4, Input: 40, Output: 4},
		},
		Cursors: []CounterCursor{cursor, {Key: "k-b", UsageKey: kb, Kind: "requests", Window: "1h", WindowStart: day, Value: 4}},
	}); len(seen.Cursors) != 0 || seen.Legacy[ka].Used != 40 || !seen.Legacy[ka].WindowStart.Equal(day) {
		t.Errorf("before the first look: %+v", seen)
	}
	// The next look gets the cursors by key and no legacy one; a cursor
	// left out is dropped.
	if seen := record(Look{Cursors: []CounterCursor{cursor}}); len(seen.Legacy) != 0 || len(seen.Cursors) != 2 || seen.Cursors["k-a"] != cursor {
		t.Errorf("cursors = %+v", seen)
	}
	if seen := record(Look{}); len(seen.Cursors) != 1 {
		t.Errorf("cursors after dropping k-b: %+v", seen.Cursors)
	}

	tokens, err := s.TokenHistory(ctx, day, StepHour, a.ID)
	if err != nil || len(tokens) != 1 || tokens[0].Requests != 3 || tokens[0].Input != 3500 || tokens[0].Cached != 2000 || tokens[0].Output != 120 ||
		tokens[0].ModelName != "glm" || !tokens[0].At.Equal(day.Add(9*time.Hour)) {
		t.Errorf("token hours = %+v, %v", tokens, err)
	}
	if tokens, _ := s.TokenHistory(ctx, day, StepDay, ""); len(tokens) != 2 || tokens[1].TenantSlug != "team-b" || !tokens[1].At.Equal(day.Add(24*time.Hour)) {
		t.Errorf("token days = %+v", tokens)
	}

	hours, err := s.UsageHistory(ctx, day, StepHour, a.ID)
	if err != nil || len(hours) != 2 || hours[0].Used != 150 || hours[0].BestEffort != 7 || !hours[0].At.Equal(day.Add(9*time.Hour)) || hours[0].TenantSlug != "team-a" || hours[0].ModelName != "glm" {
		t.Errorf("hours = %+v, %v", hours, err)
	}
	days, err := s.UsageHistory(ctx, day, StepDay, "")
	if err != nil || len(days) != 3 || days[0].Used != 150 || days[1].Unit != UnitTokens || days[2].Used != 5 || !days[2].At.Equal(day.Add(24*time.Hour)) {
		t.Errorf("days = %+v, %v", days, err)
	}

	// A failed look writes nothing.
	failed := errors.New("redis is down")
	err = s.RecordUsage(ctx, func(Before) (Look, error) {
		return Look{Usage: []UsageDelta{{UsageKey: ka, Hour: day, Unit: UnitCredits, Used: 1}}}, failed
	})
	if !errors.Is(err, failed) {
		t.Errorf("err = %v", err)
	}
	if err := s.PruneUsage(ctx, day.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if days, _ := s.UsageHistory(ctx, day, StepDay, ""); len(days) != 1 || days[0].Used != 5 {
		t.Errorf("after pruning: %+v", days)
	}
	if tokens, _ := s.TokenHistory(ctx, day, StepDay, ""); len(tokens) != 1 || tokens[0].Requests != 4 {
		t.Errorf("token history after pruning: %+v", tokens)
	}
}

func TestTenantOfKey(t *testing.T) {
	s, ctx := open(t)
	a, b := tenant(t, s, ctx, "team-a"), tenant(t, s, ctx, "team-b")
	_, keyA, err := s.CreateKey(ctx, a.ID, "one")
	if err != nil {
		t.Fatal(err)
	}
	revoked, keyB, err := s.CreateKey(ctx, b.ID, "two")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.TenantOfKey(ctx, keyA); err != nil || got.ID != a.ID {
		t.Errorf("tenant of the key = %+v, %v", got, err)
	}
	for name, key := range map[string]string{
		"empty": "", "short": "sk-", "same start": keyA[:keyPrefixLen] + "0000", "one character off": keyA[:len(keyA)-1] + "x",
	} {
		if _, err := s.TenantOfKey(ctx, key); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v, want ErrNotFound", name, err)
		}
	}
	if _, err := s.RevokeKey(ctx, revoked.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TenantOfKey(ctx, keyB); !errors.Is(err, ErrNotFound) {
		t.Errorf("a revoked key: %v, want ErrNotFound", err)
	}
	if _, err := s.UpdateTenant(ctx, a.ID, "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TenantOfKey(ctx, keyA); !errors.Is(err, ErrNotFound) {
		t.Errorf("a key of a tenant that is turned off: %v, want ErrNotFound", err)
	}
}

// A tenant at the model's best-effort limit leaves the best-effort route
// and stays in its period, so it is not moved there again.
func TestCapOverage(t *testing.T) {
	s, ctx := open(t)
	glm := model(t, s, ctx, "glm", true)
	team := tenant(t, s, ctx, "team-a")
	limit := int64(500)
	if _, err := s.SetModelSpentMode(ctx, glm, SpentBestEffort, false, &limit); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertQuota(ctx, team.ID, glm, 100, "1h", nil); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.StartOverage(ctx, glm, team.ID, time.Now().Add(time.Hour)); err != nil || !ok {
		t.Fatalf("start = %v, %v", ok, err)
	}
	listed := func() int {
		t.Helper()
		be, err := s.BestEffortTenants(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return len(be[glm])
	}
	if listed() != 1 {
		t.Fatal("the tenant is not on the best-effort route")
	}
	if ok, err := s.CapOverage(ctx, glm, team.ID); err != nil || !ok {
		t.Fatalf("cap = %v, %v", ok, err)
	}
	if ok, _ := s.CapOverage(ctx, glm, team.ID); ok {
		t.Error("capped twice")
	}
	periods, _ := s.ActiveOverage(ctx)
	if listed() != 0 || len(periods) != 1 || periods[0].CappedAt == nil {
		t.Errorf("listed = %d, periods = %+v, want off the route and still in its period", listed(), periods)
	}
	if ok, _ := s.StartOverage(ctx, glm, team.ID, time.Now().Add(time.Hour)); ok {
		t.Error("moved again within the period")
	}
	models, _ := s.ListModels(ctx)
	if models[0].BestEffortLimit == nil || *models[0].BestEffortLimit != 500 {
		t.Errorf("limit = %v", models[0].BestEffortLimit)
	}
	// A new limit gives the tenant another chance in the same period.
	if _, err := s.SetModelSpentMode(ctx, glm, SpentBestEffort, false, nil); err != nil {
		t.Fatal(err)
	}
	if listed() != 1 {
		t.Error("still off the route after the limit was removed")
	}
}
