package budget

import (
	"math"
	"testing"

	"aigw-ui/internal/capacity"
)

// The figures below are worked out by hand in the comments.

func tp(input, cached, output float64) *capacity.Throughput {
	return &capacity.Throughput{InputPerSecond: input, CachedPerSecond: cached, OutputPerSecond: output}
}

// GLM at $2.80 in, $0.56 cached, $8.80 out per million: 280,000, 56,000 and
// 880,000 credits.
var glmPrices = &capacity.Prices{Input: 280000, Cached: 56000, Output: 880000}

// Qwen at $0.30, $0.03, $1.00: 30,000, 3,000, 100,000 credits.
var qwenPrices = &capacity.Prices{Input: 30000, Cached: 3000, Output: 100000}

func glm() Model {
	return Model{ID: "glm", Prices: glmPrices, BusyShare: 0.6, OvercommitLimit: 1.5, BusiestHour: 7_000_000,
		LLMIs: []LLMI{
			{Ready: 2, PerInstance: tp(10000, 9000, 100)}, // r = (1,000×280,000 + 9,000×56,000 + 100×880,000) / 1e6 = 872
			{Ready: 1, PerInstance: tp(5000, 4500, 50)},   // r = (500×280,000 + 4,500×56,000 + 50×880,000) / 1e6 = 436
			{Ready: 3}, // not measured: counts 0
		}}
}

func qwen() Model {
	return Model{ID: "qwen", Prices: qwenPrices, BusyShare: 0.5, OvercommitLimit: 2, BusiestHour: 1_000_000,
		LLMIs: []LLMI{{Ready: 4, PerInstance: tp(20000, 18000, 400)}}} // r = (2,000×30,000 + 18,000×3,000 + 400×100,000) / 1e6 = 154
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestValuePerSecond(t *testing.T) {
	if r := ValuePerSecond(*tp(10000, 9000, 100), *glmPrices); !near(r, 872) {
		t.Fatalf("r = %v", r)
	}
	if r := ValuePerSecond(*tp(20000, 18000, 400), *qwenPrices); !near(r, 154) {
		t.Fatalf("r = %v", r)
	}
}

func TestPlanModel(t *testing.T) {
	p, ok := PlanModel(glm())
	// R = 2×872 + 1×436 = 2,180 credits a second.
	// Can be given = 86,400 × 0.6 × 2,180 = 113,011,200; ceiling × 1.5 = 169,516,800.
	// ρ = 7,000,000 / (3,600 × 2,180) = 7,000,000 / 7,848,000 = 0.8919…: saturated.
	if !ok || !near(p.ValuePerSecond, 2180) || p.CanBeGivenDay != 113_011_200 || p.CeilingDay != 169_516_800 ||
		!near(*p.PeakHourShare, 7_000_000.0/7_848_000) || !p.Saturated || *p.Values[0] != 872 || p.Values[2] != nil {
		t.Fatalf("glm = %+v", p)
	}
	q, _ := PlanModel(qwen())
	// R = 4×154 = 616; 86,400 × 0.5 × 616 = 26,611,200; × 2 = 53,222,400.
	// ρ = 1,000,000 / (3,600 × 616) = 0.4509…: not saturated.
	if !near(q.ValuePerSecond, 616) || q.CanBeGivenDay != 26_611_200 || q.CeilingDay != 53_222_400 || q.Saturated {
		t.Fatalf("qwen = %+v", q)
	}
	if _, ok := PlanModel(Model{ID: "tokens"}); ok {
		t.Fatal("a model in tokens has a plan in dollars")
	}
	// Nothing measured: nothing to give, and no peak share.
	none, _ := PlanModel(Model{ID: "new", Prices: glmPrices, BusyShare: 0.6, OvercommitLimit: 1.5, LLMIs: []LLMI{{Ready: 2}}})
	if none.CanBeGivenDay != 0 || none.PeakHourShare != nil {
		t.Fatalf("none = %+v", none)
	}
}

func tenants() []Tenant {
	return []Tenant{
		// $300 a day on GLM and $5 an hour on Qwen: 30,000,000 + 24 × 500,000 = 42,000,000.
		{ID: "t1", Budgets: []Budget{{ModelID: "glm", Limit: 30_000_000, Window: "1d"}, {ModelID: "qwen", Limit: 500_000, Window: "1h"}}},
		// $3 a day on GLM: 300,000.
		{ID: "t2", Budgets: []Budget{{ModelID: "glm", Limit: 300_000, Window: "1d"}}},
		// $100 a day on Qwen: 10,000,000.
		{ID: "t3", Budgets: []Budget{{ModelID: "qwen", Limit: 10_000_000, Window: "1d"}}},
	}
}

func TestGiven(t *testing.T) {
	ts := tenants()
	if ts[0].Given() != 42_000_000 || ts[1].Given() != 300_000 || ts[2].Given() != 10_000_000 {
		t.Fatalf("given %d %d %d", ts[0].Given(), ts[1].Given(), ts[2].Given())
	}
	if GivenOn(ts, "glm") != 30_300_000 || GivenOn(ts, "qwen") != 22_000_000 {
		t.Fatalf("given on glm %d, qwen %d", GivenOn(ts, "glm"), GivenOn(ts, "qwen"))
	}
}

func TestPlanFleet(t *testing.T) {
	g, _ := PlanModel(glm())
	q, _ := PlanModel(qwen())
	f := PlanFleet([]Plan{g, q}, tenants(), History{Days: 20, BusiestDay: 26_150_000})
	// V = 86,400 × (2,180 + 616) = 241,574,400.
	// Can be given 113,011,200 + 26,611,200 = 139,622,400; ceiling 169,516,800 + 53,222,400 = 222,739,200.
	// Given 42,000,000 + 300,000 + 10,000,000 = 52,300,000; left 170,439,200.
	// s = 26,150,000 / 52,300,000 = 0.5, so k = 1/s = 2.
	if f.ValueDay != 241_574_400 || f.CanBeGivenDay != 139_622_400 || f.CeilingDay != 222_739_200 || f.GivenDay != 52_300_000 ||
		f.LeftDay != 170_439_200 || f.SuggestedOvercommit == nil || !near(*f.SuggestedOvercommit, 2) {
		t.Fatalf("fleet = %+v", f)
	}
	for _, tc := range []struct {
		h    History
		want *float64
	}{
		{History{Days: 13, BusiestDay: 26_150_000}, nil},     // less than two weeks
		{History{Days: 30, BusiestDay: 1_000_000}, ptr(5)},   // 1/s = 52.3: at most 5
		{History{Days: 30, BusiestDay: 104_600_000}, ptr(1)}, // 1/s = 0.5: at least 1
		{History{Days: 30}, nil},                             // nothing spent
	} {
		got := PlanFleet([]Plan{g, q}, tenants(), tc.h).SuggestedOvercommit
		if (got == nil) != (tc.want == nil) || (got != nil && !near(*got, *tc.want)) {
			t.Errorf("%+v: %v", tc.h, got)
		}
	}
}

func ptr(v float64) *float64 { return &v }

func TestByPeak(t *testing.T) {
	g, _ := PlanModel(glm())
	q, _ := PlanModel(qwen())
	n, _ := PlanModel(Model{ID: "new", Prices: glmPrices})
	got := ByPeak([]Plan{n, q, g})
	if got[0].ModelID != "glm" || got[1].ModelID != "qwen" || got[2].ModelID != "new" {
		t.Fatalf("order %s %s %s", got[0].ModelID, got[1].ModelID, got[2].ModelID)
	}
}

func TestSplit(t *testing.T) {
	// 100,000,001 credits among three: 33,333,333 each, the odd credit kept.
	got := Split(100_000_001, []string{"a", "b", "c"}, "glm")
	if len(got) != 3 || got[0].Amount != 33_333_333 || got[2].TenantID != "c" || got[0].ModelID != "glm" {
		t.Fatalf("split = %+v", got)
	}
	if got := Split(9, []string{"a", "b"}, "glm"); got[1].Amount != 4 {
		t.Fatalf("9 among two = %+v", got)
	}
	if got := Split(10, nil, "glm"); got == nil || len(got) != 0 {
		t.Fatalf("nobody = %#v", got)
	}
}
