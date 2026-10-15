package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"aigw-ui/internal/benchmark"
	"aigw-ui/internal/capacity"
	"aigw-ui/internal/llmi"
	"aigw-ui/internal/render"
	"aigw-ui/internal/store"
)

// capacityLLMI is one LLMInferenceService of a model as the Capacity page
// shows it, with the result that counts for it (package llmi).
type capacityLLMI struct {
	store.Deployment
	Shape       capacity.Shape `json:"shape"`
	MaxModelLen int            `json:"max_model_len"`
	// Run is the measurement that counts, nil when none does; Why then says
	// why. Stale says why it should be measured again.
	Run   *store.CapacityRun `json:"run"`
	Stale string             `json:"stale"`
	Why   string             `json:"why"`
}

// capacityModel is what the capacity page shows of one model.
type capacityModel struct {
	ModelID           string         `json:"model_id"`
	ModelName         string         `json:"model_name"`
	Unit              string         `json:"unit"`
	TargetUtilization float64        `json:"target_utilization"`
	LLMIs             []capacityLLMI `json:"llmis"`
	// ProfileVersion is the version of the profile the model's figures are
	// counted with: never two at once.
	ProfileVersion int `json:"profile_version"`
	// Sites counts the sites with a ready instance that is not drained: a
	// drained measurement needs another one.
	Sites int `json:"sites"`
	// Plan is what follows from the results. Its shapes are the LLMIs, in
	// the same order.
	Plan    capacity.Plan      `json:"plan"`
	Running *store.CapacityRun `json:"running"`
	// prices are the model's, nil in tokens: what the planner values it at.
	prices *capacity.Prices
}

// capacityGPU is a GPU the fleet runs on, or one with a price.
type capacityGPU struct {
	GPU        string   `json:"gpu"`
	USDPerHour *float64 `json:"usd_per_hour"`
	Note       string   `json:"note"`
	// Deployments counts the deployments on it.
	Deployments int `json:"deployments"`
}

// capacityCampaign is the queue the hub measures by itself, and when.
type capacityCampaign struct {
	// Window is the chart's capacity.window, "" when the hub never
	// measures by itself.
	Window string           `json:"window"`
	Open   bool             `json:"open"`
	Next   *time.Time       `json:"next"`
	Queue  []llmi.QueueItem `json:"queue"`
}

// recentDays is how far back the use of a model is looked at.
const recentDays = 30

// capacityPlans returns, for every model, its LLMInferenceServices and the
// result that counts for each, what the model can be given and what it has
// been given; the GPUs with their costs; the active benchmark profile and
// the campaign.
func (s *Server) capacityPlans(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	models, gpus, err := s.plans(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	profile, err := s.st.ActiveProfile(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	campaign, err := s.campaign(ctx, profile.Version)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"image": s.bench.Image(), "peer_measurements": s.bench.PeerMeasurements(),
		"gpus": gpus, "models": models, "profile": profile, "campaign": campaign})
}

func (s *Server) campaign(ctx context.Context, active int) (capacityCampaign, error) {
	c := capacityCampaign{Queue: []llmi.QueueItem{}}
	now := time.Now()
	queue, err := s.bench.Queue(ctx, now)
	if err != nil {
		return c, err
	}
	c.Queue = queue
	if win := s.bench.Window(); win != nil {
		next := win.Next(now)
		c.Window, c.Open, c.Next = win.String(), win.Open(now), &next
	}
	return c, nil
}

// readyInstances is what a deployment's ready instances count for in what
// its model can be given: nothing while an operator drains its site, all of
// them while a measurement does, since that drain lasts the run only.
func readyInstances(d store.Deployment, running []store.CapacityRun) float64 {
	if d.Instances == nil {
		return 0
	}
	if !d.Drained {
		return *d.Instances
	}
	for _, r := range running {
		if r.DrainedSite && r.ModelID == d.ModelID && r.ClusterID == d.ClusterID {
			return *d.Instances
		}
	}
	return 0
}

// plans works out every model's plan from its LLMInferenceServices, the
// results that count for them, the tenants' budgets on it and its recent use.
func (s *Server) plans(ctx context.Context) ([]capacityModel, []capacityGPU, error) {
	models, err := s.st.ListModels(ctx)
	if err != nil {
		return nil, nil, err
	}
	deployments, err := s.st.ListDeployments(ctx)
	if err != nil {
		return nil, nil, err
	}
	latest, err := s.st.LatestCapacityRuns(ctx)
	if err != nil {
		return nil, nil, err
	}
	running, err := s.st.RunningCapacityRuns(ctx)
	if err != nil {
		return nil, nil, err
	}
	profile, err := s.st.ActiveProfile(ctx)
	if err != nil {
		return nil, nil, err
	}
	quotas, err := s.st.AllQuotas(ctx)
	if err != nil {
		return nil, nil, err
	}
	used, err := s.st.UsageSince(ctx, time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -recentDays))
	if err != nil {
		return nil, nil, err
	}
	targets, err := s.st.CapacityTargets(ctx)
	if err != nil {
		return nil, nil, err
	}
	prices, err := s.st.ListGPUPrices(ctx)
	if err != nil {
		return nil, nil, err
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

	now := time.Now()
	out := make([]capacityModel, 0, len(models))
	for _, m := range models {
		cm := capacityModel{ModelID: m.ID, ModelName: m.Name, Unit: m.Unit, TargetUtilization: targets[m.ID].Utilization, LLMIs: []capacityLLMI{}}
		maxLen := map[string]int{}
		for _, e := range m.Endpoints {
			maxLen[e.ClusterID] = maxModelLen(e.Capacity.MaxModelLen)
		}
		var own []store.Deployment
		sites := map[string]bool{}
		for _, d := range deployments {
			if d.ModelID == m.ID {
				own = append(own, d)
				if !d.Drained && d.Instances != nil && *d.Instances > 0 {
					sites[d.ClusterID] = true
				}
			}
		}
		cm.Sites = len(sites)
		version, results := llmi.Model(own, latest, profile.Version, now)
		cm.ProfileVersion = version

		in := capacity.PlanInput{Unit: m.Unit, CostExpression: m.CostExpression, Utilization: targets[m.ID].Utilization,
			OvercommitLimit: targets[m.ID].OvercommitLimit, Days: recentDays}
		for _, res := range results {
			d := res.Deployment
			shape := capacity.Shape{Topology: d.Topology, GPU: d.GPU, GPUsPerInstance: d.GPUsPerInstance, Revision: d.Revision}
			cm.LLMIs = append(cm.LLMIs, capacityLLMI{Deployment: d, Shape: shape, MaxModelLen: maxLen[d.ClusterID], Run: res.Run, Stale: res.Stale, Why: res.Why})
			row := capacity.ShapeInput{Shape: shape, Sites: []string{d.ClusterName}, Measured: res.PerInstance,
				Instances: readyInstances(d, running)}
			if p := gpus[d.GPU]; p != nil && p.USDPerHour != nil {
				c := capacity.CreditsPerHour(*p.USDPerHour)
				row.GPUPrice = &c
			}
			in.Shapes = append(in.Shapes, row)
		}
		for i := range running {
			if running[i].ModelID == m.ID {
				cm.Running = &running[i]
			}
		}
		if m.Prices != nil {
			in.Prices = &capacity.Prices{Input: m.Prices.Input, Cached: m.Prices.Cached, Output: m.Prices.Output}
			cm.prices = in.Prices
		}
		for _, q := range quotas {
			if q.ModelID == m.ID && !q.Shadow {
				in.Budgets = append(in.Budgets, capacity.Budget{Limit: q.TokenLimit, Window: q.Window})
			}
		}
		// The default pool of 1 holds every tenant to its own budget.
		if m.DefaultLimit > 1 {
			in.Pool = &capacity.Budget{Limit: m.DefaultLimit, Window: m.DefaultWindow}
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
	return out, gpuList, nil
}

// modelBudget is where one model's budgets stand against what it can be
// given, in its unit.
type modelBudget struct {
	ModelID   string `json:"model_id"`
	ModelName string `json:"model_name"`
	Unit      string `json:"unit"`
	// Measured is false until an instance of the model is measured; the
	// amounts are then 0, not a limit.
	Measured        bool     `json:"measured"`
	CapacityDay     int64    `json:"capacity_day"`
	CeilingDay      int64    `json:"ceiling_day"`
	GivenDay        int64    `json:"given_day"`
	LeftDay         int64    `json:"left_day"`
	Overcommit      *float64 `json:"overcommit"`
	OvercommitLimit float64  `json:"overcommit_limit"`
	ShortWindows    int      `json:"short_windows"`
	PoolDay         *int64   `json:"pool_day"`
	// The planner's figures, for a priced model: R_m in credits a second,
	// the busiest hour's share of what it serves, and each LLMI.
	ValuePerSecond *float64      `json:"value_per_second"`
	PeakHourShare  *float64      `json:"peak_hour_share"`
	Saturated      bool          `json:"saturated"`
	LLMIs          []plannerLLMI `json:"llmis"`
}

func budgetOf(m capacityModel) modelBudget {
	p := m.Plan
	return modelBudget{ModelID: m.ModelID, ModelName: m.ModelName, Unit: m.Unit, Measured: p.MeasuredInstances > 0,
		CapacityDay: p.CapacityDay, CeilingDay: p.CeilingDay, GivenDay: p.GivenDay, LeftDay: p.LeftDay,
		Overcommit: p.Overcommit, OvercommitLimit: p.OvercommitLimit, ShortWindows: p.ShortWindows, PoolDay: p.PoolDay,
		LLMIs: []plannerLLMI{}}
}

// budgets returns, for every model, what it can be given a day, what its
// budgets take and what is left under its overcommit limit, and the same
// added up over the measured models counted in dollars; the planner's
// figures per model and for the fleet; and the market value of yesterday.
func (s *Server) budgets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	models, _, err := s.plans(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	plan, err := s.planner(ctx, models)
	if err != nil {
		fail(w, err)
		return
	}
	out := make([]modelBudget, 0, len(models))
	var total struct {
		CapacityDay int64 `json:"capacity_day"`
		CeilingDay  int64 `json:"ceiling_day"`
		GivenDay    int64 `json:"given_day"`
		LeftDay     int64 `json:"left_day"`
	}
	for _, m := range models {
		b := budgetOf(m)
		if p, ok := plan.models[m.ModelID]; ok {
			b.ValuePerSecond, b.PeakHourShare, b.Saturated, b.LLMIs = &p.plan.ValuePerSecond, p.plan.PeakHourShare, p.plan.Saturated, p.llmis
		}
		out = append(out, b)
		if b.Unit == store.UnitCredits && b.Measured {
			total.CapacityDay += b.CapacityDay
			total.CeilingDay += b.CeilingDay
			total.GivenDay += b.GivenDay
			total.LeftDay += b.LeftDay
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out, "credits": total, "fleet": plan.fleet, "by_peak": plan.byPeak, "market": plan.market})
}

// cents rounds an amount in credits to whole cents for a sentence; what a
// model can be given is worked out to the credit.
func cents(n int64, unit string) int64 {
	if unit != store.UnitCredits {
		return n
	}
	const perCent = render.CreditsPerDollar / 100
	return (n + perCent/2) / perCent * perCent
}

// budgetSummary says where a model's budgets stand, for the task log of a
// change to one of them; "" before the model is measured.
func (s *Server) budgetSummary(ctx context.Context, modelID string) string {
	models, _, err := s.plans(ctx)
	if err != nil {
		return ""
	}
	for _, m := range models {
		b := budgetOf(m)
		if m.ModelID != modelID || !b.Measured || b.Overcommit == nil {
			continue
		}
		return fmt.Sprintf("; %s budgets now %s of %s a day (%.2f×, limit %s×)", m.ModelName,
			store.Amount(cents(b.GivenDay, b.Unit), b.Unit), store.Amount(cents(b.CapacityDay, b.Unit), b.Unit), *b.Overcommit,
			strconv.FormatFloat(b.OvercommitLimit, 'f', -1, 64))
	}
	return ""
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

// setOvercommitLimit sets how far the budgets on a model may add up beyond
// what it can be given a day.
func (s *Server) setOvercommitLimit(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Limit float64 `json:"limit"`
	}
	if err := decode(r, &b); err != nil {
		fail(w, err)
		return
	}
	if !(b.Limit >= 1) || b.Limit > 10 {
		fail(w, invalid("limit must be from 1 to 10"))
		return
	}
	name, err := s.st.SetOvercommitLimit(r.Context(), r.PathValue("id"), b.Limit)
	if err != nil {
		fail(w, err)
		return
	}
	note(r, "capacity.overcommit", "Set the overcommit limit of "+name+" to "+strconv.FormatFloat(b.Limit, 'f', -1, 64)+"×")
	w.WriteHeader(http.StatusNoContent)
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

// capacityRunBody starts a measurement of one LLMInferenceService with the
// active profile. A live measurement slows the model's users down, so it is
// confirmed by typing the model's name.
type capacityRunBody struct {
	ClusterID  string `json:"cluster_id"`
	Namespace  string `json:"namespace"`
	Deployment string `json:"deployment"`
	// Pod is the instance measured; a prefill/decode deployment takes none.
	Pod     string `json:"pod"`
	Mode    string `json:"mode"`
	Confirm string `json:"confirm"`
}

// check refuses a body that names no deployment or mode, and a live
// measurement that is not confirmed with the model's name.
func (b capacityRunBody) check(model string) error {
	switch {
	case b.ClusterID == "" || b.Namespace == "" || b.Deployment == "":
		return invalid("cluster_id, namespace and deployment are required")
	case b.Mode != capacity.ModeDrained && b.Mode != capacity.ModeLive:
		return invalid("mode must be %q or %q", capacity.ModeDrained, capacity.ModeLive)
	case b.Mode == capacity.ModeLive && b.Confirm != model:
		return invalid("a live measurement slows down the users of %s: confirm it with the model's name", model)
	}
	return nil
}

func (s *Server) startCapacityRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	modelID := r.PathValue("id")
	var b capacityRunBody
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		fail(w, invalid("invalid JSON body: %v", err))
		return
	}
	model, err := s.st.GetModel(ctx, modelID)
	if err != nil {
		fail(w, err)
		return
	}
	if err := b.check(model.Name); err != nil {
		fail(w, err)
		return
	}
	run, err := s.bench.Start(ctx, benchmark.Request{ModelID: modelID, ClusterID: b.ClusterID, Namespace: b.Namespace, Deployment: b.Deployment,
		Pod: b.Pod, Mode: b.Mode, StartedBy: store.ByOperator})
	switch {
	case errors.Is(err, benchmark.ErrNoImage):
		writeError(w, http.StatusConflict, "The server has no AIPerf image to run. Set capacity.image in the chart (CAPACITY_IMAGE) to an image in a registry the clusters can pull from.")
		return
	case errors.Is(err, benchmark.ErrNotServed):
		writeError(w, http.StatusNotFound, b.Namespace+"/"+b.Deployment+" is not a deployment of "+model.Name+" on that cluster.")
		return
	case errors.Is(err, benchmark.ErrBusy):
		writeError(w, http.StatusConflict, "A measurement of "+model.Name+" is running. Wait for it or stop it first: two at once would share the load and both be wrong.")
		return
	case errors.Is(err, benchmark.ErrNoOtherSite):
		writeError(w, http.StatusConflict, "No other site serves "+model.Name+", so its site cannot be drained. Measure it live.")
		return
	case errors.Is(err, benchmark.ErrPeerOff):
		writeError(w, http.StatusConflict, "Measuring a prefill/decode LLMInferenceService is off on this hub. Its Job would run in the gateway's namespace with the site's peer client certificate, "+
			"which reaches every model on the peer listener with no key and no quota. See capacity.peerMeasurements in the security guide.")
		return
	case errors.Is(err, benchmark.ErrUnknownSpec):
		writeError(w, http.StatusConflict, "What "+b.Namespace+"/"+b.Deployment+" runs is not known yet: none of its pods is ready, or they run different images, as during a rollout. Measure it once its pods agree.")
		return
	case errors.Is(err, store.ErrNotFound):
		fail(w, err)
		return
	case err != nil:
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	what := "Started measuring "
	if b.Mode == capacity.ModeLive {
		what = "Started measuring live, with its users, "
	}
	note(r, "capacity.start", what+model.Name+" on "+run.ClusterName+" ("+run.Namespace+"/"+run.Deployment+", profile version "+
		strconv.Itoa(*run.ProfileVersion)+")")
	writeJSON(w, http.StatusCreated, run)
}

// profiles returns the active benchmark profile and every version of it.
func (s *Server) profiles(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.ListProfiles(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// setProfile stores an edited profile as its next version, which every
// measurement uses from then on. Results of the earlier version stay and
// are marked stale until measured again.
func (s *Server) setProfile(w http.ResponseWriter, r *http.Request) {
	var p capacity.Profile
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		fail(w, invalid("invalid JSON body: %v", err))
		return
	}
	if err := p.Validate(); err != nil {
		fail(w, invalid("%s", err.Error()))
		return
	}
	saved, err := s.st.AddProfileVersion(r.Context(), p)
	if err != nil {
		fail(w, err)
		return
	}
	note(r, "capacity.profile", fmt.Sprintf("Set the benchmark profile %s, version %d", saved.Name, saved.Version))
	writeJSON(w, http.StatusOK, saved)
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
