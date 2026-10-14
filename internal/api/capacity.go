package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"aigw-ui/internal/benchmark"
	"aigw-ui/internal/capacity"
	"aigw-ui/internal/store"
)

// capacityDeployment is a deployment of a model as the capacity page shows it.
type capacityDeployment struct {
	store.Deployment
	Shape       capacity.Shape `json:"shape"`
	MaxModelLen int            `json:"max_model_len"`
}

// capacityModel is what the capacity page shows of one model.
type capacityModel struct {
	ModelID           string               `json:"model_id"`
	ModelName         string               `json:"model_name"`
	Unit              string               `json:"unit"`
	TargetUtilization float64              `json:"target_utilization"`
	Deployments       []capacityDeployment `json:"deployments"`
	// Defaults are the settings a new measurement starts from.
	Defaults capacity.Settings `json:"defaults"`
	// Latest is the newest measurement of each shape that found a load
	// within the targets, and Plan what follows from them.
	Latest  []store.CapacityRun `json:"latest"`
	Plan    capacity.Plan       `json:"plan"`
	Running *store.CapacityRun  `json:"running"`
}

// capacityGPU is a GPU the fleet runs on, or one with a price.
type capacityGPU struct {
	GPU        string   `json:"gpu"`
	USDPerHour *float64 `json:"usd_per_hour"`
	Note       string   `json:"note"`
	// Deployments counts the deployments on it.
	Deployments int `json:"deployments"`
}

// recentDays is how far back the use of a model is looked at.
const recentDays = 30

// capacityPlans returns, for every model, its deployments and their shapes,
// what each shape serves, what the model can be given and what it has been
// given, and the GPUs with their prices.
func (s *Server) capacityPlans(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	models, err := s.st.ListModels(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	deployments, err := s.st.ListDeployments(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	latest, err := s.st.LatestCapacityRuns(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	running, err := s.st.RunningCapacityRuns(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	quotas, err := s.st.AllQuotas(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	used, err := s.st.UsageSince(ctx, time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -recentDays))
	if err != nil {
		fail(w, err)
		return
	}
	targets, err := s.st.TargetUtilizations(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	prices, err := s.st.ListGPUPrices(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	gpus := map[string]*capacityGPU{}
	for _, p := range prices {
		usd := p.USDPerHour
		gpus[p.GPU] = &capacityGPU{GPU: p.GPU, USDPerHour: &usd, Note: p.Note}
	}
	for _, d := range deployments {
		if d.GPU == "" {
			continue
		}
		if gpus[d.GPU] == nil {
			gpus[d.GPU] = &capacityGPU{GPU: d.GPU}
		}
		gpus[d.GPU].Deployments++
	}

	out := make([]capacityModel, 0, len(models))
	for _, m := range models {
		cm := capacityModel{ModelID: m.ID, ModelName: m.Name, Unit: m.Unit, TargetUtilization: targets[m.ID], Deployments: []capacityDeployment{}, Latest: []store.CapacityRun{}}
		maxLen := map[string]int{}
		for _, e := range m.Endpoints {
			maxLen[e.ClusterID] = maxModelLen(e.Capacity.MaxModelLen)
		}
		shortest := 0
		byShape := map[string]*capacity.ShapeInput{}
		var order []string
		for _, d := range deployments {
			if d.ModelID != m.ID {
				continue
			}
			shape := capacity.Shape{Topology: d.Topology, GPU: d.GPU, GPUsPerInstance: d.GPUsPerInstance, Revision: d.Revision}
			cm.Deployments = append(cm.Deployments, capacityDeployment{Deployment: d, Shape: shape, MaxModelLen: maxLen[d.ClusterID]})
			if l := maxLen[d.ClusterID]; l > 0 && (shortest == 0 || l < shortest) {
				shortest = l
			}
			in := byShape[shape.Key()]
			if in == nil {
				in = &capacity.ShapeInput{Shape: shape}
				if p := gpus[d.GPU]; p != nil && p.USDPerHour != nil {
					c := capacity.CreditsPerHour(*p.USDPerHour)
					in.GPUPrice = &c
				}
				byShape[shape.Key()] = in
				order = append(order, shape.Key())
			}
			if d.Instances != nil && !d.Drained {
				in.Instances += *d.Instances
			}
			in.Sites = appendNew(in.Sites, d.ClusterName)
		}
		cm.Defaults = capacity.Defaults(shortest)
		for i := range running {
			if running[i].ModelID == m.ID {
				cm.Running = &running[i]
			}
		}
		for i := range latest {
			run := latest[i]
			if run.ModelID != m.ID {
				continue
			}
			cm.Latest = append(cm.Latest, run)
			if in := byShape[run.Shape.Key()]; in != nil {
				t := capacity.ThroughputOf(*run.Result.Best)
				in.Measured = &t
			}
		}

		in := capacity.PlanInput{Unit: m.Unit, CostExpression: m.CostExpression, Utilization: targets[m.ID], Days: recentDays}
		for _, key := range order {
			in.Shapes = append(in.Shapes, *byShape[key])
		}
		if m.Prices != nil {
			in.Prices = &capacity.Prices{Input: m.Prices.Input, Cached: m.Prices.Cached, Output: m.Prices.Output}
		}
		for _, q := range quotas {
			switch {
			case q.ModelID != m.ID || q.Shadow:
			case q.Window == "1d":
				in.AllocatedPerDay += q.TokenLimit
			default:
				in.RateLimits++
			}
		}
		for _, u := range used {
			if u.ModelID == m.ID && u.Unit == m.Unit {
				in.BusiestDay, in.TotalUsed = u.Busiest, u.Total
			}
		}
		cm.Plan = capacity.MakePlan(in)
		out = append(out, cm)
	}

	gpuList := make([]capacityGPU, 0, len(gpus))
	for _, g := range gpus {
		gpuList = append(gpuList, *g)
	}
	sort.Slice(gpuList, func(i, j int) bool { return gpuList[i].GPU < gpuList[j].GPU })
	writeJSON(w, http.StatusOK, map[string]any{"image": s.bench.Image(), "gpus": gpuList, "models": out})
}

func appendNew(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// maxModelLen reads the longest request a site's deployments take. Several
// deployments with different lengths give the shortest.
func maxModelLen(declared string) int {
	shortest := 0
	for _, v := range strings.Split(declared, ", ") {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 && (shortest == 0 || n < shortest) {
			shortest = n
		}
	}
	return shortest
}

// setTargetUtilization sets the share of the day a model's instances are
// expected to be busy.
func (s *Server) setTargetUtilization(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Utilization float64 `json:"utilization"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	if !(b.Utilization >= 0.05) || b.Utilization > 1 {
		fail(w, invalid("utilization must be from 0.05 to 1"))
		return
	}
	name, err := s.st.SetTargetUtilization(r.Context(), r.PathValue("id"), b.Utilization)
	if err != nil {
		fail(w, err)
		return
	}
	note(r, "capacity.utilization", "Set the expected busy share of "+name+" to "+strconv.Itoa(int(math.Round(b.Utilization*100)))+"%")
	w.WriteHeader(http.StatusNoContent)
}

// maxGPUPrice catches a price typed in the wrong unit: no GPU costs a
// thousand dollars an hour.
const maxGPUPrice = 1000

func (s *Server) setGPUPrice(w http.ResponseWriter, r *http.Request) {
	var b struct {
		USDPerHour float64 `json:"usd_per_hour"`
		Note       string  `json:"note"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	gpu := r.PathValue("gpu")
	switch {
	case gpu == "" || len(gpu) > 100:
		fail(w, invalid("the GPU must be named, in at most 100 characters"))
		return
	case !(b.USDPerHour > 0) || b.USDPerHour > maxGPUPrice:
		fail(w, invalid("usd_per_hour must be more than 0 and at most %d", maxGPUPrice))
		return
	case len(b.Note) > 500:
		fail(w, invalid("note must be at most 500 characters"))
		return
	}
	if err := s.st.SetGPUPrice(r.Context(), store.GPUPrice{GPU: gpu, USDPerHour: b.USDPerHour, Note: b.Note}); err != nil {
		fail(w, err)
		return
	}
	note(r, "capacity.gpu-price", "Set the price of "+gpu+" to $"+strconv.FormatFloat(b.USDPerHour, 'f', -1, 64)+" an hour")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteGPUPrice(w http.ResponseWriter, r *http.Request) {
	gpu := r.PathValue("gpu")
	if err := s.st.DeleteGPUPrice(r.Context(), gpu); err != nil {
		fail(w, err)
		return
	}
	note(r, "capacity.gpu-price", "Removed the price of "+gpu)
	w.WriteHeader(http.StatusNoContent)
}

// deploymentPods lists the ready pods of a deployment that a measurement can
// be aimed at. It asks the cluster.
func (s *Server) deploymentPods(w http.ResponseWriter, r *http.Request) {
	pods, err := s.bench.Pods(r.Context(), r.PathValue("id"), r.PathValue("namespace"), r.PathValue("name"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			fail(w, err)
			return
		}
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, pods)
}

func (s *Server) listCapacityRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.st.ListCapacityRuns(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

// capacityRunBody starts a measurement of one pod of one deployment.
// Settings that are left out take their defaults for the deployment.
type capacityRunBody struct {
	ClusterID  string            `json:"cluster_id"`
	Namespace  string            `json:"namespace"`
	Deployment string            `json:"deployment"`
	Pod        string            `json:"pod"`
	Settings   capacity.Settings `json:"settings"`
}

func (s *Server) startCapacityRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	modelID := r.PathValue("id")
	body, err := io.ReadAll(r.Body)
	if err != nil {
		fail(w, err)
		return
	}
	// The defaults depend on the site, so the site is read first and the
	// body is then read over the defaults.
	var b capacityRunBody
	if err := json.Unmarshal(body, &b); err != nil {
		fail(w, invalid("invalid JSON body: %v", err))
		return
	}
	switch {
	case b.ClusterID == "" || b.Namespace == "" || b.Deployment == "":
		fail(w, invalid("cluster_id, namespace and deployment are required"))
		return
	case b.Pod == "":
		fail(w, invalid("pod is required: the pod keeps its own traffic while it is measured, so it is chosen on purpose"))
		return
	}
	models, err := s.st.ListModels(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	var model *store.Model
	for i := range models {
		if models[i].ID == modelID {
			model = &models[i]
		}
	}
	if model == nil {
		fail(w, store.ErrNotFound)
		return
	}
	shortest := 0
	for _, e := range model.Endpoints {
		if e.ClusterID == b.ClusterID {
			shortest = maxModelLen(e.Capacity.MaxModelLen)
		}
	}
	b = capacityRunBody{Settings: capacity.Defaults(shortest)}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		fail(w, invalid("invalid JSON body: %v", err))
		return
	}
	if err := b.Settings.Validate(); err != nil {
		fail(w, invalid("%s", err.Error()))
		return
	}
	run, err := s.bench.Start(ctx, modelID, b.ClusterID, b.Namespace, b.Deployment, b.Pod, b.Settings)
	switch {
	case errors.Is(err, benchmark.ErrNoImage):
		writeError(w, http.StatusConflict, "The server has no AIPerf image to run. Set capacity.image in the chart (CAPACITY_IMAGE) to an image in a registry the clusters can pull from.")
		return
	case errors.Is(err, benchmark.ErrNotServed):
		writeError(w, http.StatusNotFound, b.Namespace+"/"+b.Deployment+" is not a deployment of "+model.Name+" on that cluster.")
		return
	case errors.Is(err, benchmark.ErrBusy):
		writeError(w, http.StatusConflict, "A measurement of "+model.Name+" is running. Wait for it or stop it first: two at once would share the GPUs and both be wrong.")
		return
	case errors.Is(err, store.ErrNotFound):
		fail(w, err)
		return
	case err != nil:
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	note(r, "capacity.start", "Started measuring "+model.Name+" on "+run.ClusterName+" ("+run.Namespace+"/"+run.Instance+", "+run.Shape.Label()+")")
	writeJSON(w, http.StatusCreated, run)
}

func (s *Server) getCapacityRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.st.GetCapacityRun(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) cancelCapacityRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.bench.Cancel(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			fail(w, err)
			return
		}
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	note(r, "capacity.cancel", "Stopped measuring "+run.ModelName+" on "+run.ClusterName)
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) deleteCapacityRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.st.GetCapacityRun(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	if run.Status == store.RunRunning {
		writeError(w, http.StatusConflict, "The measurement is running. Stop it first.")
		return
	}
	if err := s.st.DeleteCapacityRun(r.Context(), run.ID); err != nil {
		fail(w, err)
		return
	}
	note(r, "capacity.delete", "Deleted a measurement of "+run.ModelName+" on "+run.ClusterName+" from "+run.CreatedAt.UTC().Format("2006-01-02 15:04 UTC"))
	w.WriteHeader(http.StatusNoContent)
}
