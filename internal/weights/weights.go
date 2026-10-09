// Package weights decides how much of a model's traffic each site should
// get. It is pure: the numbers come in from the clusters and go out to the
// gateways through other packages.
//
// A site's capacity for a model is the number of ready serving instances
// times what one instance of that deployment can serve. The second number
// depends on how the model is deployed (one node, several nodes, separate
// prefill and decode) and cannot be counted, so it is declared next to the
// deployment and defaults to 1.
package weights

import (
	"math"
	"sort"
)

// Next returns the capacity to apply for one model on one site, given what
// is applied now and what was just observed, and the new count of polls in a
// row that saw less than is applied.
//
//   - Nothing applied yet: take what was observed.
//   - More than applied: go up by at most step per poll, so a site that comes
//     back gets its conversations back gradually instead of all at once onto
//     a cold cache.
//   - Less than applied: wait for a second poll in a row to agree, so one
//     failed readiness check moves no conversation.
//   - Less than applied because an operator drains the site: go down by at
//     most step per poll, a gradual hand-off.
//
// observed is 0 for a drained site. step is the capacity of one instance;
// anything below it is treated as it.
func Next(applied *float64, observed, step float64, lowStreak int, drain bool) (float64, int) {
	if applied == nil {
		return observed, 0
	}
	switch {
	case observed > *applied:
		return math.Min(observed, *applied+math.Max(step, minStep)), 0
	case observed < *applied && drain:
		return math.Max(observed, *applied-math.Max(step, minStep)), 0
	case observed < *applied && lowStreak >= 1:
		return observed, 0
	case observed < *applied:
		return *applied, lowStreak + 1
	}
	return *applied, 0
}

// minStep keeps a deployment with a tiny declared capacity from ramping forever.
const minStep = 0.01

// Site is one cluster of the fleet, for one model. Capacity is nil while the
// cluster has never reported one. A site that does not serve the model and
// has no capacity takes part with weight 0.
type Site struct {
	Zone     string
	Serving  bool
	Capacity *float64
}

// Zone is the weight of one site as the gateway takes it.
type Zone struct {
	Zone   string
	Weight int64
}

// Zones turns the capacities of the fleet's sites for one model into integer
// zone weights. Every site is listed, with 0 where it does not serve the
// model, because the gateway gives an unlisted zone a weight of 1. It returns no weights and a reason when applying any would be
// worse than leaving the gateways as they are:
//
//   - a serving site with unknown capacity cannot be left out, because the
//     gateway gives an unlisted zone a weight of its own, and cannot be
//     guessed;
//   - weights that add up to zero would make the model unreachable. Taking a
//     dead site out is the job of the health checks.
func Zones(in []Site) ([]Zone, string) {
	sites := append([]Site(nil), in...)
	if len(sites) == 0 {
		return nil, "no site serves the model"
	}
	total, whole := 0.0, true
	zero := 0.0
	for i, s := range sites {
		if s.Capacity == nil && !s.Serving {
			sites[i].Capacity, s.Capacity = &zero, &zero
		}
		if s.Capacity == nil {
			return nil, "the capacity of " + s.Zone + " is not known yet"
		}
		total += *s.Capacity
		whole = whole && *s.Capacity == math.Trunc(*s.Capacity)
	}
	if total <= 0 {
		return nil, "no site has a ready instance; the last weights are kept"
	}
	// Whole numbers are used as they are, so 8 and 3 instances read as 8 and
	// 3. Anything else is scaled to keep two decimals.
	scale := 1.0
	if !whole {
		scale = 100
	}
	zones := make([]Zone, 0, len(sites))
	for _, s := range sites {
		w := int64(math.Round(*s.Capacity * scale))
		if w == 0 && *s.Capacity > 0 {
			w = 1 // never round a serving site down to nothing
		}
		zones = append(zones, Zone{Zone: s.Zone, Weight: w})
	}
	sort.Slice(zones, func(i, j int) bool { return zones[i].Zone < zones[j].Zone })
	return zones, ""
}

// Workload is one LLMInferenceService as far as capacity is concerned.
type Workload struct {
	// PerInstance is what one instance can serve, in any unit that is the
	// same on every site. 1 when nothing is declared.
	PerInstance float64
	// Ready and Desired count the main workload: pods for a single-node
	// deployment, groups for a multi-node one. Ready is nil when the cluster
	// has not reported it.
	Ready   *int64
	Desired int64
	// Prefill is set for a deployment that serves prefill separately.
	Prefill *Prefill
}

type Prefill struct {
	Ready   *int64
	Desired int64
}

// Instances returns how many complete serving instances are ready, and false
// when that is not known.
//
// With separate prefill, an instance is a decode replica together with its
// share of the prefill replicas, in the ratio the deployment asks for. Four
// decode and two prefill replicas are four instances; with one prefill
// replica down there is prefill for only two of them.
func (w Workload) Instances() (float64, bool) {
	if w.Ready == nil {
		return 0, false
	}
	ready := float64(*w.Ready)
	if w.Prefill == nil {
		return ready, true
	}
	if w.Prefill.Ready == nil {
		return 0, false
	}
	if w.Prefill.Desired <= 0 || w.Desired <= 0 {
		return 0, true
	}
	covered := float64(*w.Prefill.Ready) * float64(w.Desired) / float64(w.Prefill.Desired)
	return math.Min(ready, covered), true
}
