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
	// A multi-node prefill/decode deployment worth 2.6 single-node ones, a
	// site with nothing ready, and one that has not reported yet.
	zones := Zones([]Site{{"d", nil}, {"b", f(8)}, {"a", f(2.6)}, {"c", f(0)}, {"e", f(0.001)}})
	want := []Zone{{"a", 260}, {"b", 800}, {"c", 1}, {"d", 1}, {"e", 1}}
	if len(zones) != len(want) {
		t.Fatalf("got %v, want %v", zones, want)
	}
	for i := range want {
		if zones[i] != want[i] {
			t.Errorf("got %v, want %v", zones, want)
			break
		}
	}
	if got := Zones(nil); len(got) != 0 {
		t.Errorf("no site: %v", got)
	}
}

// Envoy rejects a locality weight below 1, and Envoy Gateway then stops
// publishing anything to that gateway.
func TestWeightIsNeverBelowOne(t *testing.T) {
	for _, c := range []*float64{nil, f(0), f(-1), f(0.0001), f(0.004), f(0.005), f(1), f(1e6)} {
		if w := Weight(c); w < 1 {
			t.Errorf("Weight(%v) = %d", c, w)
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
