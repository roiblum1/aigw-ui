package syncer

import (
	"aigw-ui/internal/render"
	"strings"
	"testing"
	"time"

	"aigw-ui/internal/store"
)

func TestNextDay(t *testing.T) {
	jerusalem := time.FixedZone("IDT", 3*3600)
	for in, want := range map[time.Time]string{
		time.Date(2026, 10, 10, 15, 4, 5, 0, time.UTC):  "2026-10-11T00:00:00Z",
		time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC):   "2026-10-11T00:00:00Z",
		time.Date(2026, 10, 11, 1, 30, 0, 0, jerusalem): "2026-10-11T00:00:00Z",
		time.Date(2026, 10, 11, 3, 0, 0, 0, jerusalem):  "2026-10-12T00:00:00Z",
	} {
		if got := NextDay(in).Format(time.RFC3339); got != want {
			t.Errorf("NextDay(%s) = %s, want %s", in.Format(time.RFC3339), got, want)
		}
	}
}

func TestDollars(t *testing.T) {
	for credits, want := range map[int64]string{
		0: "$0.00", 1: "$0.00001", 11574: "$0.11574", 100_000: "$1.00", 1_250_000: "$12.50", 4_294_967_295: "$42949.67295",
	} {
		if got := render.Dollars(credits); got != want {
			t.Errorf("Dollars(%d) = %s, want %s", credits, got, want)
		}
	}
}

func TestPriceLine(t *testing.T) {
	day := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	a := store.AppliedPrice{ModelName: "glm", First: true, Price: store.Price{Input: 11574, Cached: 1157, Output: 46296, EffectiveAt: day}}
	onTime := priceLine(a, day.Add(2*time.Second))
	if !strings.Contains(onTime, "$0.11574") || !strings.Contains(onTime, "dry-run") || strings.Contains(onTime, "due") {
		t.Errorf("on time: %s", onTime)
	}
	a.First = false
	late := priceLine(a, day.Add(3*time.Hour))
	if strings.Contains(late, "dry-run") || !strings.Contains(late, "due 3h0m0s ago") {
		t.Errorf("late: %s", late)
	}
}
