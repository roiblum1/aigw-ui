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

// Site is one site that is listed for a model: a fleet cluster that serves
// the model and is not drained out. Capacity is nil while the cluster has
// never reported one.
type Site struct {
	Zone     string
	Capacity *float64
}

// Zone is the weight of one site as the gateway takes it.
type Zone struct {
	Zone   string `json:"zone"`
	Weight int64  `json:"weight"`
}

// Scale turns a capacity into a zone weight. It keeps two decimals of the
// capacity, and it makes MinWeight small next to any site that has an
// instance ready.
const Scale = 100

// MinWeight is the lowest weight a listed site gets. Zero is not possible:
// Envoy rejects a locality weight below 1, and Envoy Gateway then stops
// publishing every change to that gateway, key revocations included.
const MinWeight = 1

// MaxWeight is the highest weight a site gets. The gateway takes a 32-bit
// number and adds the weights of a model's sites up; with this limit a model
// would need more than 400 sites to pass it.
const MaxWeight = 10_000_000

// Weight returns the zone weight for a capacity. A site with nothing ready,
// or one that has not reported yet, gets MinWeight: its health check keeps
// traffic off it, and it ramps up from there.
func Weight(capacity *float64) int64 {
	if capacity == nil || math.IsNaN(*capacity) {
		return MinWeight
	}
	// Compared as floats: converting a number that does not fit is undefined.
	scaled := math.Round(*capacity * Scale)
	switch {
	case scaled >= MaxWeight:
		return MaxWeight
	case scaled <= MinWeight:
		return MinWeight
	}
	return int64(scaled)
}

// Zones returns the zone weights of the sites listed for one model, sorted
// by zone. The gateways build their hash table from this list, so the order
// must be the same on every cluster.
//
// The list has to be exactly the sites in the model's Backend: a zone that
// has endpoints but is not listed gets a weight of 1 from the gateway.
func Zones(sites []Site) []Zone {
	zones := make([]Zone, 0, len(sites))
	for _, s := range sites {
		zones = append(zones, Zone{Zone: s.Zone, Weight: Weight(s.Capacity)})
	}
	sort.Slice(zones, func(i, j int) bool { return zones[i].Zone < zones[j].Zone })
	return zones
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
