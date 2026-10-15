package syncer

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"aigw-ui/internal/capacity"
	"aigw-ui/internal/llmi"
	"aigw-ui/internal/store"
)

// minPerInstance is the smallest capacity of one instance a site is given,
// the same bound as the annotation's.
const minPerInstance = 0.01

// measuredBasis reads what measureCapacity needs from the store. A read that
// fails leaves every model on the annotation, as before measurements.
func (s *Syncer) measuredBasis(ctx context.Context, clusterID string, reported []store.Capacity) {
	models, err := s.st.ListModels(ctx)
	if err != nil {
		slog.Warn("site weights from measurements: list models", "err", err)
		return
	}
	deployments, err := s.st.ListDeployments(ctx)
	if err != nil {
		slog.Warn("site weights from measurements: list deployments", "err", err)
		return
	}
	runs, err := s.st.LatestCapacityRuns(ctx)
	if err != nil {
		slog.Warn("site weights from measurements: latest measurements", "err", err)
		return
	}
	profile, err := s.st.ActiveProfile(ctx)
	if err != nil {
		slog.Warn("site weights from measurements: the benchmark profile", "err", err)
		return
	}
	measureCapacity(clusterID, reported, models, deployments, runs, profile.Version, time.Now())
}

// measureCapacity counts a model's capacity on this cluster from what its
// LLMInferenceServices were measured to serve, instead of from the annotation
// aigw-ui.io/capacity-per-instance, when every one of them with a ready
// instance, on every site, has a result that counts (package llmi).
// Otherwise the model keeps the annotation everywhere: its sites' capacities
// must be in one unit for their weights to compare.
//
// The capacity of one instance is then what it serves a second in thousands
// of input-equivalent tokens (capacity.InputEquivalent), the figure the
// Capacity page prices with.
func measureCapacity(clusterID string, reported []store.Capacity, models []store.Model, deployments []store.Deployment, runs []store.CapacityRun,
	active int, now time.Time) {
	for i := range reported {
		c := &reported[i]
		c.Basis = store.BasisDeclared
		m := modelServedAs(models, clusterID, c.Model)
		if m == nil || !c.Known {
			continue
		}
		// This cluster's deployments as just read, every other site's as
		// last read.
		var all []store.Deployment
		for _, d := range c.Deployments {
			d.ClusterID, d.ModelID = clusterID, m.ID
			all = append(all, d)
		}
		for _, d := range deployments {
			if d.ModelID == m.ID && d.ClusterID != clusterID {
				all = append(all, d)
			}
		}
		_, results := llmi.Model(all, runs, active, now)
		if !allMeasured(results) {
			continue
		}
		var prices *capacity.Prices
		if m.Prices != nil {
			prices = &capacity.Prices{Input: m.Prices.Input, Cached: m.Prices.Cached, Output: m.Prices.Output}
		}
		cached, output := capacity.Ratios(prices)
		var total, step float64
		var parts []string
		for _, r := range results[:len(c.Deployments)] {
			v := minPerInstance
			if r.PerInstance != nil {
				v = math.Max(capacity.InputEquivalent(*r.PerInstance, cached, output)/1000, minPerInstance)
			}
			total += r.Ready() * v
			step = math.Max(step, v)
			parts = append(parts, fmt.Sprintf("%s/%s: %s × %s", r.Deployment.Namespace, r.Deployment.Name, ready(r.Deployment), threeFigures(v)))
		}
		c.Capacity, c.Step, c.Basis = total, step, store.BasisMeasured
		c.Detail += "; site weight from measurements, in thousands of input-equivalent tokens a second per instance: " + strings.Join(parts, ", ")
	}
}

// allMeasured is true when every deployment with a ready instance has a
// result that counts, and there is at least one.
func allMeasured(results []llmi.Result) bool {
	n := 0
	for _, r := range results {
		if r.Ready() <= 0 {
			continue
		}
		if !r.Measured() {
			return false
		}
		n++
	}
	return n > 0
}

// modelServedAs returns the catalog model that a cluster serves under name:
// by the model's name, or the name its route on the cluster sends to the
// backend.
func modelServedAs(models []store.Model, clusterID, name string) *store.Model {
	for i := range models {
		m := &models[i]
		if m.Name == name {
			return m
		}
		for _, e := range m.Endpoints {
			if e.ClusterID != clusterID {
				continue
			}
			if e.UpstreamModel == name {
				return m
			}
			for _, b := range e.Backends {
				if b.Model == name {
					return m
				}
			}
		}
	}
	return nil
}

func ready(d store.Deployment) string {
	if d.Instances == nil {
		return "?"
	}
	return strconv.FormatFloat(*d.Instances, 'f', -1, 64)
}

// threeFigures writes v to three significant figures without an exponent:
// 0.0272, 12.3, 1230.
func threeFigures(v float64) string {
	r, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'g', 3, 64), 64)
	return strconv.FormatFloat(r, 'f', -1, 64)
}
