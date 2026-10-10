package selftest

import (
	"testing"

	"aigw-ui/internal/store"
)

// Only a cluster of the fleet gets a model's best-effort route. Any other
// cluster refuses a tenant past its budget, so the self-test there has to
// check the refusal, whatever the model is set to.
func TestHasBestEffortRoute(t *testing.T) {
	for _, tc := range []struct {
		name                string
		clusterFleet, fleet bool
		mode                string
		want                bool
	}{
		{"fleet cluster, best-effort model", true, true, store.SpentBestEffort, true},
		{"cluster outside the fleet", false, true, store.SpentBestEffort, false},
		{"model refuses", true, true, store.SpentRefuse, false},
		{"model without an entry route", true, false, store.SpentBestEffort, false},
	} {
		run := &test{
			r:       &Runner{},
			cluster: store.Cluster{FleetEnabled: tc.clusterFleet},
			model:   store.Model{Fleet: tc.fleet, SpentMode: tc.mode},
		}
		if got := run.hasBestEffortRoute(); got != tc.want {
			t.Errorf("%s: has the route = %v, want %v", tc.name, got, tc.want)
		}
		// Without Redis the server cannot move anyone, route or not.
		if run.bestEffort() {
			t.Errorf("%s: best-effort although the server has no Redis", tc.name)
		}
		if status, _ := run.overage(); status != Skipped {
			t.Errorf("%s: the overage step is %s without Redis, want skipped", tc.name, status)
		}
	}
}
