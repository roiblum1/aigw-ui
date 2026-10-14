package syncer

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"

	"aigw-ui/internal/capacity"
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
	measureCapacity(clusterID, reported, models, deployments, runs)
}

// measureCapacity counts a model's capacity on this cluster from what its
// shapes were measured to serve, instead of from the annotation
// aigw-ui.io/capacity-per-instance, when every deployment of the model with a
// ready instance, on every site, has a measured shape. Otherwise the model
// keeps the annotation everywhere: its sites' capacities must be in one unit
// for their weights to compare.
//
// The capacity of one instance is then what it serves a second in thousands
// of input-equivalent tokens (capacity.InputEquivalent), the figure the
// Capacity page prices with.
func measureCapacity(clusterID string, reported []store.Capacity, models []store.Model, deployments []store.Deployment, runs []store.CapacityRun) {
	byID := make(map[string]*store.Model, len(models))
	for i := range models {
		byID[models[i].ID] = &models[i]
	}
	perInstance := map[string]map[string]float64{} // model ID -> shape key -> capacity of one instance
	for _, r := range runs {
		m := byID[r.ModelID]
		if m == nil || r.Result.Best == nil {
			continue
		}
		var prices *capacity.Prices
		if m.Prices != nil {
			prices = &capacity.Prices{Input: m.Prices.Input, Cached: m.Prices.Cached, Output: m.Prices.Output}
		}
		cached, output := capacity.Ratios(prices)
		v := capacity.InputEquivalent(capacity.ThroughputOf(*r.Result.Best), cached, output) / 1000
		if perInstance[r.ModelID] == nil {
			perInstance[r.ModelID] = map[string]float64{}
		}
		perInstance[r.ModelID][r.Shape.Key()] = math.Max(v, minPerInstance)
	}

	for i := range reported {
		c := &reported[i]
		c.Basis = store.BasisDeclared
		m := modelServedAs(models, clusterID, c.Model)
		if m == nil || !c.Known || perInstance[m.ID] == nil {
			continue
		}
		per := perInstance[m.ID]
		// This cluster's deployments as just read, every other site's as
		// last read.
		all := append([]store.Deployment(nil), c.Deployments...)
		for _, d := range deployments {
			if d.ModelID == m.ID && d.ClusterID != clusterID {
				all = append(all, d)
			}
		}
		if !allMeasured(all, per) {
			continue
		}
		var total, step float64
		var parts []string
		for _, d := range c.Deployments {
			v := per[shapeOf(d).Key()]
			if d.Instances != nil {
				total += *d.Instances * v
			}
			step = math.Max(step, v)
			parts = append(parts, fmt.Sprintf("%s/%s: %s × %s", d.Namespace, d.Name, ready(d), strconv.FormatFloat(round2(v), 'f', -1, 64)))
		}
		c.Capacity, c.Step, c.Basis = total, step, store.BasisMeasured
		c.Detail += "; site weight from measurements, in thousands of input-equivalent tokens a second per instance: " + strings.Join(parts, ", ")
	}
}

// allMeasured is true when every deployment with a ready instance has a
// measured shape, and there is at least one.
func allMeasured(deployments []store.Deployment, per map[string]float64) bool {
	n := 0
	for _, d := range deployments {
		if d.Instances == nil || *d.Instances <= 0 {
			continue
		}
		if _, ok := per[shapeOf(d).Key()]; !ok {
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

func shapeOf(d store.Deployment) capacity.Shape {
	return capacity.Shape{Topology: d.Topology, GPU: d.GPU, GPUsPerInstance: d.GPUsPerInstance, Revision: d.Revision}
}

func ready(d store.Deployment) string {
	if d.Instances == nil {
		return "?"
	}
	return strconv.FormatFloat(*d.Instances, 'f', -1, 64)
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
