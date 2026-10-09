package weights

import "testing"

func f(v float64) *float64 { return &v }
func n(v int64) *int64     { return &v }

func TestNext(t *testing.T) {
	for name, tc := range map[string]struct {
		applied    *float64
		observed   float64
		step       float64
		streak     int
		drain      bool
		want       float64
		wantStreak int
	}{
		"first observation is taken as it is": {nil, 8, 1, 0, false, 8, 0},
		"unchanged":                           {f(8), 8, 1, 0, false, 8, 0},
		"up by one instance per poll":         {f(3), 8, 1, 0, false, 4, 0},
		"up never past what was observed":     {f(7.5), 8, 1, 0, false, 8, 0},
		"up by the size of one instance":      {f(0), 7.8, 2.6, 0, false, 2.6, 0},
		"first low poll changes nothing":      {f(8), 6, 1, 0, false, 8, 1},
		"second low poll in a row is applied": {f(8), 6, 1, 1, false, 6, 0},
		"a rise clears the low streak":        {f(8), 9, 1, 1, false, 9, 0},
		"recovery clears the low streak":      {f(8), 8, 1, 1, false, 8, 0},
		"all instances gone, second poll":     {f(8), 0, 1, 1, false, 0, 0},
		"a drain steps down at once":          {f(8), 0, 1, 0, true, 7, 0},
		"a drain steps by one instance":       {f(5.2), 0, 2.6, 0, true, 2.6, 0},
		"a drain stops at zero":               {f(0.5), 0, 1, 0, true, 0, 0},
		"a drained site stays at zero":        {f(0), 0, 1, 0, true, 0, 0},
	} {
		got, streak := Next(tc.applied, tc.observed, tc.step, tc.streak, tc.drain)
		if got != tc.want || streak != tc.wantStreak {
			t.Errorf("%s: got %v (streak %d), want %v (streak %d)", name, got, streak, tc.want, tc.wantStreak)
		}
	}
}

func TestZones(t *testing.T) {
	zones, reason := Zones([]Site{{"site2-a", true, f(3)}, {"site1-a", true, f(8)}})
	if reason != "" || len(zones) != 2 || zones[0] != (Zone{"site1-a", 8}) || zones[1] != (Zone{"site2-a", 3}) {
		t.Errorf("whole capacities: %v %q", zones, reason)
	}
	// A multi-node prefill/decode deployment worth 2.6 single-node ones.
	zones, _ = Zones([]Site{{"a", true, f(2.6)}, {"b", true, f(3)}, {"c", true, f(0.001)}, {"d", true, f(0)}})
	want := []Zone{{"a", 260}, {"b", 300}, {"c", 1}, {"d", 0}}
	for i := range want {
		if zones[i] != want[i] {
			t.Errorf("scaled capacities: got %v, want %v", zones, want)
			break
		}
	}
	// A fleet cluster without the model is listed with 0: the gateway gives
	// an unlisted zone a weight of 1.
	zones, reason = Zones([]Site{{"a", true, f(8)}, {"b", false, nil}})
	if reason != "" || len(zones) != 2 || zones[1] != (Zone{"b", 0}) {
		t.Errorf("site without the model: %v %q", zones, reason)
	}
	for name, sites := range map[string][]Site{
		"unknown site":   {{"a", true, f(8)}, {"b", true, nil}},
		"all zero":       {{"a", true, f(0)}, {"b", false, nil}},
		"no site at all": nil,
	} {
		if zones, reason := Zones(sites); zones != nil || reason == "" {
			t.Errorf("%s: got %v %q, want no weights and a reason", name, zones, reason)
		}
	}
}

func TestInstances(t *testing.T) {
	for name, tc := range map[string]struct {
		w     Workload
		want  float64
		known bool
	}{
		"single node":                 {Workload{Ready: n(8), Desired: 8}, 8, true},
		"not reported":                {Workload{Desired: 8}, 0, false},
		"prefill and decode complete": {Workload{Ready: n(4), Desired: 4, Prefill: &Prefill{Ready: n(2), Desired: 2}}, 4, true},
		"half the prefill down":       {Workload{Ready: n(4), Desired: 4, Prefill: &Prefill{Ready: n(1), Desired: 2}}, 2, true},
		"a decode replica down":       {Workload{Ready: n(3), Desired: 4, Prefill: &Prefill{Ready: n(2), Desired: 2}}, 3, true},
		"prefill not reported":        {Workload{Ready: n(4), Desired: 4, Prefill: &Prefill{Desired: 2}}, 0, false},
		"no prefill at all":           {Workload{Ready: n(4), Desired: 4, Prefill: &Prefill{Ready: n(0), Desired: 2}}, 0, true},
	} {
		got, known := tc.w.Instances()
		if got != tc.want || known != tc.known {
			t.Errorf("%s: got %v %v, want %v %v", name, got, known, tc.want, tc.known)
		}
	}
}
