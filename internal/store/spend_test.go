package store

import (
	"testing"
	"time"
)

// The planner's history: the busiest hour of each model, the busiest day over
// all of them and how many days have spend, in credits, best-effort
// included; a model in tokens is left out.
func TestSpend(t *testing.T) {
	s, ctx := open(t)
	glm := model(t, s, ctx, "glm", false)
	qwen := model(t, s, ctx, "qwen", false)
	a, b := tenant(t, s, ctx, "a"), tenant(t, s, ctx, "b")
	day := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	for _, u := range []struct {
		tenant, model string
		at            time.Time
		unit          string
		used, be      int64
	}{
		{a.ID, glm, day.Add(9 * time.Hour), "credits", 100, 0},
		{b.ID, glm, day.Add(9 * time.Hour), "credits", 50, 25}, // glm at 09:00: 175
		{a.ID, glm, day.Add(10 * time.Hour), "credits", 120, 0},
		{a.ID, qwen, day.Add(9 * time.Hour), "credits", 30, 0},
		{a.ID, qwen, day.Add(24 * time.Hour), "credits", 40, 0},       // the next day: 40
		{a.ID, qwen, day.Add(25 * time.Hour), "tokens", 1_000_000, 0}, // tokens are not dollars
		{a.ID, glm, day.AddDate(0, 0, -40), "credits", 9_999, 0},      // before the period
	} {
		if _, err := s.db.Exec(ctx, `INSERT INTO usage_hours (tenant_id, model_id, hour, unit, used, best_effort) VALUES ($1, $2, $3, $4, $5, $6)`,
			u.tenant, u.model, u.at, u.unit, u.used, u.be); err != nil {
			t.Fatal(err)
		}
	}
	sp, err := s.SpendSince(ctx, day.AddDate(0, 0, -30))
	// The 10th: 175 + 120 + 30 = 325; the 11th: 40. Two days.
	if err != nil || sp.BusiestHour[glm] != 175 || sp.BusiestHour[qwen] != 40 || sp.Days != 2 || sp.BusiestDay != 325 {
		t.Fatalf("spend = %+v, %v", sp, err)
	}
	if n, err := s.SpentBetween(ctx, day, day.Add(24*time.Hour)); err != nil || n != 325 {
		t.Fatalf("the 10th = %d, %v", n, err)
	}
}
