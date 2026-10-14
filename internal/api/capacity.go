package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aigw-ui/internal/benchmark"
	"aigw-ui/internal/capacity"
	"aigw-ui/internal/store"
)

// capacitySite is one cluster that serves a model.
type capacitySite struct {
	ClusterID   string   `json:"cluster_id"`
	ClusterName string   `json:"cluster_name"`
	Instances   *float64 `json:"instances"`
	MaxModelLen int      `json:"max_model_len"`
}

// capacityModel is what the capacity page shows of one model.
type capacityModel struct {
	ModelID   string         `json:"model_id"`
	ModelName string         `json:"model_name"`
	Unit      string         `json:"unit"`
	Sites     []capacitySite `json:"sites"`
	// Defaults are the settings a new measurement starts from.
	Defaults capacity.Settings `json:"defaults"`
	// Latest is the newest measurement that found a load the instance
	// serves at the promised latency, and Plan what follows from it.
	Latest  *store.CapacityRun `json:"latest"`
	Plan    *capacity.Plan     `json:"plan"`
	Running *store.CapacityRun `json:"running"`
}

// busiestDays is how far back the busiest day of a model is looked for.
const busiestDays = 30 * 24 * time.Hour

// capacityPlans returns, for every model, what one instance serves, what
// the model can be given and what it has been given.
func (s *Server) capacityPlans(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	models, err := s.st.ListModels(ctx)
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
	busiest, err := s.st.BusiestDays(ctx, time.Now().Add(-busiestDays))
	if err != nil {
		fail(w, err)
		return
	}

	out := make([]capacityModel, 0, len(models))
	for _, m := range models {
		cm := capacityModel{ModelID: m.ID, ModelName: m.Name, Unit: m.Unit, Sites: []capacitySite{}}
		var instances float64
		shortest := 0
		for _, e := range m.Endpoints {
			if !e.Capacity.Serving {
				continue
			}
			site := capacitySite{ClusterID: e.ClusterID, ClusterName: e.ClusterName, Instances: e.Capacity.Instances, MaxModelLen: maxModelLen(e.Capacity.MaxModelLen)}
			cm.Sites = append(cm.Sites, site)
			if site.Instances != nil && !e.Capacity.Drained {
				instances += *site.Instances
			}
			if site.MaxModelLen > 0 && (shortest == 0 || site.MaxModelLen < shortest) {
				shortest = site.MaxModelLen
			}
		}
		cm.Defaults = capacity.Defaults(shortest)
		for i := range running {
			if running[i].ModelID == m.ID {
				cm.Running = &running[i]
			}
		}
		for i := range latest {
			if latest[i].ModelID != m.ID {
				continue
			}
			cm.Latest = &latest[i]
			in := capacity.PlanInput{
				Unit:           m.Unit,
				CostExpression: m.CostExpression,
				Throughput:     capacity.ThroughputOf(*latest[i].Result.Best),
				Settings:       latest[i].Settings,
				Instances:      instances,
			}
			if m.Prices != nil {
				in.Prices = &capacity.Prices{Input: m.Prices.Input, Cached: m.Prices.Cached, Output: m.Prices.Output}
			}
			for _, q := range quotas {
				if q.ModelID == m.ID && !q.Shadow {
					in.AllocatedPerDay += q.TokenLimit * capacity.PerDay(q.Window)
				}
			}
			for _, d := range busiest {
				if d.ModelID == m.ID && d.Unit == m.Unit {
					in.BusiestDay = d.Used
				}
			}
			plan := capacity.MakePlan(in)
			cm.Plan = &plan
		}
		out = append(out, cm)
	}
	writeJSON(w, http.StatusOK, map[string]any{"image": s.bench.Image(), "models": out})
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

func (s *Server) listCapacityRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.st.ListCapacityRuns(r.Context(), r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

// capacityRunBody starts a measurement. Settings that are left out take
// their defaults for the site's deployment.
type capacityRunBody struct {
	ClusterID string            `json:"cluster_id"`
	Pod       string            `json:"pod"`
	Settings  capacity.Settings `json:"settings"`
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
	if b.ClusterID == "" {
		fail(w, invalid("cluster_id is required"))
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
	run, err := s.bench.Start(ctx, modelID, b.ClusterID, b.Pod, b.Settings)
	switch {
	case errors.Is(err, benchmark.ErrNoImage):
		writeError(w, http.StatusConflict, "The server has no AIPerf image to run. Set capacity.image in the chart (CAPACITY_IMAGE) to an image in a registry the clusters can pull from.")
		return
	case errors.Is(err, benchmark.ErrNotServed):
		writeError(w, http.StatusNotFound, model.Name+" is not served on that cluster.")
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
	note(r, "capacity.start", "Started measuring "+model.Name+" on "+run.ClusterName+" ("+run.Namespace+"/"+run.Instance+")")
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
