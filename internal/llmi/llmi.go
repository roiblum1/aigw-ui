// Package llmi decides which measurement counts for each
// LLMInferenceService (LLMI) of a model. It is pure: it reads what the store
// returns and says, per deployment, which result counts, whether it is stale,
// and why when none does. The Capacity page, the budgets and the site
// weights all use it, so they agree.
package llmi

import (
	"fmt"
	"time"

	"aigw-ui/internal/capacity"
	"aigw-ui/internal/store"
)

// Result is what counts of one deployment's measurements.
type Result struct {
	Deployment store.Deployment
	// Run is the measurement that counts, nil when none does; PerInstance
	// is what one instance serves by it.
	Run         *store.CapacityRun
	PerInstance *capacity.Throughput
	// Stale says why the counted result should be measured again:
	// capacity.StaleProfile or capacity.StaleAge, "" when it is fresh.
	Stale string
	// Why says why no result counts.
	Why string
}

// Measured is true when a result counts for the deployment.
func (r Result) Measured() bool { return r.PerInstance != nil }

// Ready is the deployment's ready instances, 0 while unknown.
func (r Result) Ready() float64 {
	if r.Deployment.Instances == nil {
		return 0
	}
	return *r.Deployment.Instances
}

// Model works out, for the deployments of one model, which measurement counts
// for each, and the version of the profile the model is counted with.
//
// A result counts only for the spec it was measured on. Figures of different
// profile versions are never added: the model counts the version whose
// results cover the most of its ready instances, the active one on a tie.
// After the profile changes, the old results therefore keep counting, marked
// stale, until the new version covers more, and capacity does not fall to
// zero at once.
func Model(deployments []store.Deployment, runs []store.CapacityRun, active int, now time.Time) (int, []Result) {
	type key struct{ cluster, namespace, name string }
	byLLMI := map[key][]store.CapacityRun{}
	for _, r := range runs {
		k := key{r.ClusterID, r.Namespace, r.Deployment}
		byLLMI[k] = append(byLLMI[k], r)
	}

	// The newest result of each version on each deployment's current spec.
	current := make([]map[int]store.CapacityRun, len(deployments))
	coverage := map[int]float64{}
	for i, d := range deployments {
		current[i] = map[int]store.CapacityRun{}
		for _, r := range byLLMI[key{d.ClusterID, d.Namespace, d.Name}] {
			if d.SpecHash == "" || r.SpecHash != d.SpecHash || r.ProfileVersion == nil || r.PerInstance() == nil {
				continue
			}
			v := *r.ProfileVersion
			if old, ok := current[i][v]; !ok || r.CreatedAt.After(old.CreatedAt) {
				current[i][v] = r
			}
		}
		for v := range current[i] {
			coverage[v] += Result{Deployment: d}.Ready()
		}
	}
	// The most ready instances covered; on a tie the active version, then
	// the newer one. The order of the map does not matter.
	version, best := active, -1.0
	for v, c := range coverage {
		switch {
		case c > best:
			version, best = v, c
		case c == best && v == active:
			version = v
		case c == best && version != active && v > version:
			version = v
		}
	}

	out := make([]Result, len(deployments))
	for i, d := range deployments {
		res := Result{Deployment: d}
		if r, ok := current[i][version]; ok {
			res.Run, res.PerInstance = &r, r.PerInstance()
			at := r.CreatedAt
			if r.FinishedAt != nil {
				at = *r.FinishedAt
			}
			res.Stale = capacity.Staleness(version, active, at, now)
		} else {
			res.Why = why(d, byLLMI[key{d.ClusterID, d.Namespace, d.Name}], current[i], version)
		}
		out[i] = res
	}
	return version, out
}

func why(d store.Deployment, runs []store.CapacityRun, onSpec map[int]store.CapacityRun, version int) string {
	for v := range onSpec {
		return fmt.Sprintf("measured with version %d of the profile; the model is counted with version %d", v, version)
	}
	for _, r := range runs {
		if r.SpecHash != "" && r.SpecHash != d.SpecHash {
			return "its spec changed since it was measured"
		}
	}
	return "not measured"
}

// States of a deployment in the campaign queue.
const (
	// Next is measured as soon as the window is open.
	Next = "next"
	// Running is being measured.
	Running = "running"
	// WaitsModel waits for the measurement of another deployment of its
	// model: one per model at a time.
	WaitsModel = "waits for another measurement of the model"
	// WaitsSite waits for the measurement on its site: one per site.
	WaitsSite = "waits for another measurement on the site"
	// WaitsOperator can only be measured live, which only an operator
	// starts: no other site serves the model, so its site cannot be
	// drained.
	WaitsOperator = "waits for an operator: no other site serves the model, so it can only be measured live"
	// WaitsCover is not drained by the campaign: the model's other sites,
	// by their measurements, cannot take its busiest hour in the window.
	// An operator measures it live, or drained at a quieter time.
	WaitsCover = "waits for an operator: the other sites cannot take the model's busiest hour"
)

// Load is what the campaign knows of a model's traffic: its busiest hour
// in the campaign window over the last days, in its unit, and its prices
// (nil for a model in tokens).
type Load struct {
	BusiestHour float64
	Prices      *capacity.Prices
}

// QueueItem is one deployment the campaign has to measure.
type QueueItem struct {
	ModelID     string `json:"model_id"`
	ClusterID   string `json:"cluster_id"`
	ClusterName string `json:"cluster_name"`
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	Topology    string `json:"topology"`
	// Reason is why it is in the queue: not measured, its spec changed, or
	// its result is stale (capacity.StaleProfile, capacity.StaleAge).
	Reason string `json:"reason"`
	State  string `json:"state"`
	// Cover is, for a drained measurement, what the model's other sites
	// serve in an hour at the targets, up to capacity.Saturated of it, over
	// its busiest hour in the window: below 1 they cannot take its load.
	// Nil when the model had no traffic in the window.
	Cover *float64 `json:"cover"`
}

// Queue lists the deployments with a ready instance whose result is missing
// or stale, those without a result first, and says what each waits for. A
// model served on one site only can be measured live only, so it waits for
// an operator; so does one whose other sites cannot take its busiest hour in
// the window, by load (by model ID).
func Queue(deployments []store.Deployment, latest, running []store.CapacityRun, active int, now time.Time, load map[string]Load) []QueueItem {
	busyModel, busySite := map[string]bool{}, map[string]bool{}
	runningLLMI := map[string]bool{}
	for _, r := range running {
		busyModel[r.ModelID], busySite[r.ClusterID] = true, true
		runningLLMI[r.ClusterID+"/"+r.Namespace+"/"+r.Deployment] = true
	}
	byModel := map[string][]store.Deployment{}
	var order []string
	for _, d := range deployments {
		if byModel[d.ModelID] == nil {
			order = append(order, d.ModelID)
		}
		byModel[d.ModelID] = append(byModel[d.ModelID], d)
	}

	// What is not measured comes first, then what is stale; the states are
	// given in that order, so that a missing result is measured before a
	// stale one of the same model or site.
	type candidate struct {
		item    QueueItem
		model   []store.Deployment
		results []Result
	}
	var missing, stale []candidate
	for _, modelID := range order {
		ds := byModel[modelID]
		_, results := Model(ds, latest, active, now)
		for _, r := range results {
			d := r.Deployment
			if r.Ready() <= 0 || (r.Measured() && r.Stale == "") {
				continue
			}
			c := candidate{item: QueueItem{ModelID: d.ModelID, ClusterID: d.ClusterID, ClusterName: d.ClusterName, Namespace: d.Namespace,
				Name: d.Name, Topology: d.Topology, Reason: r.Why}, model: ds, results: results}
			if r.Measured() {
				c.item.Reason = r.Stale
				stale = append(stale, c)
			} else {
				missing = append(missing, c)
			}
		}
	}
	out := []QueueItem{} // never nil: the page reads an empty list
	for _, c := range append(missing, stale...) {
		item := c.item
		switch {
		case runningLLMI[item.ClusterID+"/"+item.Namespace+"/"+item.Name]:
			item.State = Running
		case !servedElsewhere(c.model, item.ClusterID):
			item.State = WaitsOperator
		case !covered(&item, c.results, load[item.ModelID]):
			item.State = WaitsCover
		case busyModel[item.ModelID]:
			item.State = WaitsModel
		case busySite[item.ClusterID]:
			item.State = WaitsSite
		default:
			item.State = Next
			// What starts next holds its model and site for the rest.
			busyModel[item.ModelID], busySite[item.ClusterID] = true, true
		}
		out = append(out, item)
	}
	return out
}

// covered says whether the model's other sites can take its busiest hour in
// the window while item's site is drained: what their ready, undrained,
// measured instances serve in an hour, up to capacity.Saturated of it. It
// sets item.Cover. A model with no traffic in the window is covered.
func covered(item *QueueItem, results []Result, l Load) bool {
	if l.BusiestHour <= 0 {
		return true
	}
	var perSecond float64
	for _, r := range results {
		d := r.Deployment
		if d.ClusterID != item.ClusterID && !d.Drained && r.Measured() {
			perSecond += r.Ready() * r.PerInstance.Value(l.Prices)
		}
	}
	cover := capacity.Saturated * 3600 * perSecond / l.BusiestHour
	item.Cover = &cover
	return cover >= 1
}

// servedElsewhere is true when another site has a ready, undrained instance
// of the model, so that draining this site leaves it served.
func servedElsewhere(ds []store.Deployment, clusterID string) bool {
	for _, o := range ds {
		if o.ClusterID != clusterID && !o.Drained && o.Instances != nil && *o.Instances > 0 {
			return true
		}
	}
	return false
}
