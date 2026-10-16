package benchmark

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"aigw-ui/internal/capacity"
	"aigw-ui/internal/kube"
	"aigw-ui/internal/llmi"
	"aigw-ui/internal/store"
)

type fakeStore struct {
	deployments []store.Deployment
	clusters    map[string]store.Cluster
	runs        map[string]*store.CapacityRun
	profile     capacity.Profile
	// weight is the entry route's weight of each model's site, by
	// model|cluster; drains records every drain and restore.
	weight   map[string]float64
	drains   []string
	lastSite bool
	latest   []store.CapacityRun
	models   []store.Model
	hours    []store.HourUse
}

func (f *fakeStore) GetDeployment(_ context.Context, modelID, clusterID, namespace, name string) (store.Deployment, error) {
	for _, d := range f.deployments {
		if d.ModelID == modelID && d.ClusterID == clusterID && d.Namespace == namespace && d.Name == name {
			return d, nil
		}
	}
	return store.Deployment{}, store.ErrNotFound
}

func (f *fakeStore) GetCluster(_ context.Context, id string) (store.Cluster, error) {
	if c, ok := f.clusters[id]; ok {
		return c, nil
	}
	return store.Cluster{}, store.ErrNotFound
}

func (f *fakeStore) ActiveProfile(context.Context) (capacity.Profile, error) { return f.profile, nil }

func (f *fakeStore) CreateCapacityRun(_ context.Context, n store.NewRun) error {
	for _, r := range f.runs {
		if r.ModelID == n.ModelID && r.Status == store.RunRunning {
			return store.ErrConflict
		}
	}
	if n.DrainedSite {
		if f.lastSite {
			return store.ErrLastSite
		}
		f.drains = append(f.drains, "drain "+n.ModelID+"|"+n.ClusterID)
	}
	v := n.Profile.Version
	f.runs[n.ID] = &store.CapacityRun{ID: n.ID, ModelID: n.ModelID, ClusterID: n.ClusterID, Namespace: n.Namespace, Deployment: n.Deployment,
		JobName: n.JobName, Instance: n.Instance, Shape: n.Shape, Settings: n.Settings, Status: store.RunRunning, CreatedAt: time.Now(),
		SpecHash: n.SpecHash, ProfileVersion: &v, InstancesDuring: n.InstancesDuring, Phase: n.Phase, DrainedSite: n.DrainedSite,
		StartedBy: n.StartedBy, Mode: n.Settings.Mode}
	return nil
}

func (f *fakeStore) GetCapacityRun(_ context.Context, id string) (store.CapacityRun, error) {
	if r, ok := f.runs[id]; ok {
		return *r, nil
	}
	return store.CapacityRun{}, store.ErrNotFound
}

func (f *fakeStore) RunningCapacityRuns(context.Context) ([]store.CapacityRun, error) {
	var out []store.CapacityRun
	for _, r := range f.runs {
		if r.Status == store.RunRunning {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (f *fakeStore) UpdateCapacityRun(_ context.Context, id string, d *capacity.Dataset, steps []capacity.Step) error {
	f.runs[id].Dataset, f.runs[id].Steps = d, steps
	return nil
}

func (f *fakeStore) FinishCapacityRun(_ context.Context, id, status, detail string) error {
	r := f.runs[id]
	if r.Status != store.RunRunning {
		return store.ErrNotFound
	}
	r.Status, r.Detail = status, detail
	if r.DrainedSite {
		f.drains = append(f.drains, "undrain "+r.ModelID+"|"+r.ClusterID)
	}
	return nil
}

func (f *fakeStore) SetRunPhase(_ context.Context, id, phase string) error {
	f.runs[id].Phase = phase
	return nil
}

func (f *fakeStore) SiteWeight(_ context.Context, modelID, clusterID string) (*float64, error) {
	w := f.weight[modelID+"|"+clusterID]
	return &w, nil
}

func (f *fakeStore) ListDeployments(context.Context) ([]store.Deployment, error) {
	return f.deployments, nil
}

func (f *fakeStore) LatestCapacityRuns(context.Context) ([]store.CapacityRun, error) {
	return f.latest, nil
}

func (f *fakeStore) ListModels(context.Context) ([]store.Model, error) { return f.models, nil }

func (f *fakeStore) HourlyUse(_ context.Context, from time.Time) ([]store.HourUse, error) {
	var out []store.HourUse
	for _, h := range f.hours {
		if !h.Hour.Before(from) {
			out = append(out, h)
		}
	}
	return out, nil
}

type fakeCluster struct {
	instance kube.Instance
	pods     []string
	findErr  error
	jobs     map[string]*unstructured.Unstructured
	state    kube.JobState
	log      string
	problem  string
	deleted  []string
}

func (c *fakeCluster) Instance(_ context.Context, _, _, pod string) (kube.Instance, error) {
	if c.findErr != nil {
		return kube.Instance{}, c.findErr
	}
	i := c.instance
	i.Pod, i.URL = pod, "http://"+pod+":8000"
	i.URLs = []string{i.URL}
	return i, nil
}

func (c *fakeCluster) ReadyPods(context.Context, string, string) ([]string, error) {
	if c.pods != nil {
		return c.pods, nil
	}
	return []string{"glm-0"}, nil
}

func (c *fakeCluster) CreateJob(_ context.Context, job *unstructured.Unstructured) error {
	c.jobs[job.GetName()] = job
	return nil
}

func (c *fakeCluster) GetJobState(context.Context, string, string) (kube.JobState, error) {
	return c.state, nil
}

func (c *fakeCluster) PodLog(context.Context, string, string, string) (string, bool, error) {
	return c.log, c.log != "", nil
}

func (c *fakeCluster) PodProblem(context.Context, string, string) string { return c.problem }

func (c *fakeCluster) DeleteJob(_ context.Context, _, name string) error {
	c.deleted = append(c.deleted, name)
	return nil
}

func ready(n float64) *float64 { return &n }

func setup(image string) (*fakeStore, *fakeCluster, *Service) {
	profile := capacity.DefaultProfile()
	profile.ID = "p1"
	st := &fakeStore{runs: map[string]*store.CapacityRun{}, profile: profile, weight: map[string]float64{},
		clusters: map[string]store.Cluster{"c1": {ID: "c1", Name: "site-a", Namespace: "envoy-ai-gateway-system", PeerHost: "gw.svc", PeerPort: 8443}},
		deployments: []store.Deployment{{ModelID: "m1", ClusterID: "c1", ClusterName: "site-a", Namespace: "llm", Name: "glm", Topology: "single",
			GPU: "NVIDIA-B300", GPUsPerInstance: 8, SpecHash: "s1", Instances: ready(1)}}}
	cl := &fakeCluster{instance: kube.Instance{Namespace: "llm", Model: "glm-5", MaxModelLen: 200000}, jobs: map[string]*unstructured.Unstructured{}}
	return st, cl, New(st, func(context.Context, string) (Cluster, error) { return cl, nil },
		Options{Image: image, Every: time.Minute, Peer: Peer{SNI: "peers.llm.example", ClientSecret: "llm-peer-client", CAConfigMap: "llm-peer-ca"}})
}

func live(pod string) Request {
	return Request{ModelID: "m1", ClusterID: "c1", Namespace: "llm", Deployment: "glm", Pod: pod, Mode: capacity.ModeLive, StartedBy: store.ByOperator}
}

func drained() Request {
	r := live("glm-0")
	r.Mode = capacity.ModeDrained
	return r
}

const twoSteps = `AIGW-DATASET {"sessions":200,"requests":4000,"prompt_mean":99000,"prompt_max":166000}
AIGW-STEP {"concurrency":1,"requests":20,"duration_s":300,"input_tokens":2000000,"output_tokens":20000,"ttft_p95_ms":900,"itl_p95_ms":20}
AIGW-STEP {"concurrency":2,"requests":40,"duration_s":300,"input_tokens":4000000,"output_tokens":40000,"ttft_p95_ms":9000,"itl_p95_ms":25}
`

// A live measurement starts its Job at once, with the active profile, and
// is marked live.
func TestStartAndFollow(t *testing.T) {
	st, cl, s := setup("registry/aiperf:1")
	ctx := context.Background()
	run, err := s.Start(ctx, live("glm-0"))
	if err != nil {
		t.Fatal(err)
	}
	if run.Mode != capacity.ModeLive || run.Phase != store.PhaseMeasuring || run.SpecHash != "s1" || *run.ProfileVersion != 1 ||
		run.Shape.GPU != "NVIDIA-B300" || cl.jobs[run.JobName] == nil || len(st.drains) != 0 {
		t.Fatalf("run = %+v", run)
	}
	if _, err := s.Start(ctx, live("glm-0")); !errors.Is(err, ErrBusy) {
		t.Fatalf("second start: %v", err)
	}

	// Two steps printed, the Job still running.
	cl.log = twoSteps
	s.Tick(ctx, time.Now())
	r := st.runs[run.ID]
	if r.Status != store.RunRunning || len(r.Steps) != 2 || r.Dataset == nil {
		t.Fatalf("running: %+v", r)
	}

	// The Job finished.
	cl.log += "AIGW-DONE {}\n"
	cl.state = kube.JobState{Succeeded: true}
	s.Tick(ctx, time.Now())
	if r.Status != store.RunSucceeded || !strings.Contains(r.Detail, "at 2 in flight") {
		t.Fatalf("done: %+v", r)
	}
}

// A drained measurement drains the site, starts its Job only when the entry
// route sends nothing there, and restores the site at the end.
func TestDrained(t *testing.T) {
	st, cl, s := setup("img")
	ctx := context.Background()
	st.weight["m1|c1"] = 3
	run, err := s.Start(ctx, drained())
	if err != nil {
		t.Fatal(err)
	}
	if run.Phase != store.PhaseDraining || !run.DrainedSite || len(cl.jobs) != 0 || strings.Join(st.drains, ",") != "drain m1|c1" {
		t.Fatalf("started: %+v %v", run, st.drains)
	}
	s.Tick(ctx, time.Now())
	if st.runs[run.ID].Phase != store.PhaseDraining || len(cl.jobs) != 0 {
		t.Fatal("measured before the weight reached 0")
	}
	st.weight["m1|c1"] = 0
	s.Tick(ctx, time.Now())
	if st.runs[run.ID].Phase != store.PhaseMeasuring || cl.jobs[run.JobName] == nil {
		t.Fatalf("not measuring at weight 0: %+v", st.runs[run.ID])
	}
	cl.log, cl.state = twoSteps+"AIGW-DONE {}\n", kube.JobState{Succeeded: true}
	s.Tick(ctx, time.Now())
	if st.runs[run.ID].Status != store.RunSucceeded || strings.Join(st.drains, ",") != "drain m1|c1,undrain m1|c1" {
		t.Fatalf("not restored: %+v %v", st.runs[run.ID], st.drains)
	}

	// A site an operator drained stays drained.
	st, _, s = setup("img")
	st.deployments[0].Drained = true
	run, _ = s.Start(ctx, drained())
	if run.DrainedSite || len(st.drains) != 0 {
		t.Fatalf("drained by an operator: %+v %v", run, st.drains)
	}
	s.Cancel(ctx, run.ID)
	if len(st.drains) != 0 {
		t.Fatalf("undrained a site an operator drained: %v", st.drains)
	}
}

// A drained measurement of the only site of a model is refused: draining it
// would leave the model unserved.
func TestDrainedLastSite(t *testing.T) {
	st, _, s := setup("img")
	st.lastSite = true
	if _, err := s.Start(context.Background(), drained()); !errors.Is(err, ErrNoOtherSite) {
		t.Fatalf("last site: %v", err)
	}
	if len(st.runs) != 0 {
		t.Fatalf("a refused drain was recorded: %+v", st.runs)
	}
	// A model already being measured: nothing is drained.
	st.lastSite = false
	st.runs["x"] = &store.CapacityRun{ID: "x", ModelID: "m1", Status: store.RunRunning}
	if _, err := s.Start(context.Background(), drained()); !errors.Is(err, ErrBusy) || len(st.drains) != 0 {
		t.Fatalf("busy: %v %v", err, st.drains)
	}
}

// A site that does not drain in time, and a cancelled drain, are restored.
func TestDrainedGivesUp(t *testing.T) {
	ctx := context.Background()
	st, cl, s := setup("img")
	st.weight["m1|c1"] = 2
	run, _ := s.Start(ctx, drained())
	s.Tick(ctx, time.Now().Add(31*time.Minute))
	if r := st.runs[run.ID]; r.Status != store.RunFailed || !strings.Contains(r.Detail, "after 30 minutes") || len(st.drains) != 2 {
		t.Fatalf("timeout: %+v %v", r, st.drains)
	}

	st, cl, s = setup("img")
	st.weight["m1|c1"] = 2
	run, _ = s.Start(ctx, drained())
	got, err := s.Cancel(ctx, run.ID)
	if err != nil || got.Status != store.RunCancelled || len(cl.deleted) != 0 || len(st.drains) != 2 {
		t.Fatalf("cancel while draining: %+v %v %v %v", got, err, cl.deleted, st.drains)
	}
}

// A prefill/decode deployment is measured whole, through the gateway's peer
// listener, from the gateway's namespace; the result is divided by its decode
// replicas. It is off unless the hub turns it on: its Job holds the site's
// peer client certificate.
func TestPrefillDecode(t *testing.T) {
	st, cl, s := setup("img")
	st.deployments[0].Topology = kube.TopologyPrefillDecode
	cl.pods = []string{"glm-decode-0", "glm-decode-1"}
	if _, err := s.Start(context.Background(), live("")); !errors.Is(err, ErrPeerOff) || len(st.runs) != 0 || len(cl.jobs) != 0 {
		t.Fatalf("peer off: %v %+v", err, st.runs)
	}
	s.opt.PeerMeasurements = true
	run, err := s.Start(context.Background(), live(""))
	if err != nil {
		t.Fatal(err)
	}
	job := cl.jobs[run.JobName]
	if run.InstancesDuring != 2 || run.Instance != "" || job.GetNamespace() != "envoy-ai-gateway-system" {
		t.Fatalf("run %+v in %s", run, job.GetNamespace())
	}
	c := job.Object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	var cfg struct {
		Peer        map[string]string `json:"peer"`
		MetricsURLs []string          `json:"metrics_urls"`
	}
	json.Unmarshal([]byte(c["env"].([]any)[0].(map[string]any)["value"].(string)), &cfg)
	if cfg.Peer["address"] != "gw.svc:8443" || cfg.Peer["sni"] != "peers.llm.example" || len(cfg.MetricsURLs) != 2 {
		t.Fatalf("config %+v", cfg)
	}
}

func TestStartRefuses(t *testing.T) {
	ctx := context.Background()
	_, _, s := setup("")
	if _, err := s.Start(ctx, live("glm-0")); !errors.Is(err, ErrNoImage) {
		t.Fatalf("no image: %v", err)
	}
	_, _, s = setup("img")
	other := live("glm-0")
	other.Deployment = "other"
	if _, err := s.Start(ctx, other); !errors.Is(err, ErrNotServed) {
		t.Fatalf("not served: %v", err)
	}
	_, cl, s := setup("img")
	cl.findErr = errors.New("no pod to measure: llm/glm has no ready pod")
	if _, err := s.Start(ctx, live("glm-0")); err == nil || !strings.Contains(err.Error(), "no ready pod") {
		t.Fatalf("no pod: %v", err)
	}
	// One profile for every deployment: a short one gets it scaled down.
	st, cl, s := setup("img")
	cl.instance.MaxModelLen = 8192
	run, err := s.Start(ctx, live("glm-0"))
	if err != nil || run.Settings.MaxPromptTokens+run.Settings.MaxOutputTokens > 8192 || st.runs[run.ID] == nil {
		t.Fatalf("short model: %+v %v", run.Settings, err)
	}
}

func TestFollowEndings(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		state   kube.JobState
		log     string
		problem string
		age     time.Duration
		status  string
		detail  string
		deleted bool
	}{
		"deadline":       {state: kube.JobState{Failed: true, Reason: "DeadlineExceeded: Job was active longer than specified deadline"}, log: twoSteps, status: store.RunFailed, detail: "DeadlineExceeded"},
		"script error":   {state: kube.JobState{Failed: true, Reason: "BackoffLimitExceeded"}, log: `AIGW-ERROR {"error":"making up the sessions failed: boom"}`, status: store.RunFailed, detail: "boom"},
		"job deleted":    {state: kube.JobState{Gone: true}, status: store.RunFailed, detail: "deleted on the cluster"},
		"image missing":  {problem: "ImagePullBackOff: not found", age: 5 * time.Minute, status: store.RunFailed, detail: "could not start", deleted: true},
		"image waits":    {problem: "ImagePullBackOff: not found", age: time.Minute, status: store.RunRunning},
		"nothing passed": {state: kube.JobState{Succeeded: true}, log: `AIGW-STEP {"concurrency":1,"requests":20,"duration_s":300,"ttft_p95_ms":99999,"itl_p95_ms":20}` + "\nAIGW-DONE {}", status: store.RunSucceeded, detail: "no step met"},
	} {
		st, cl, s := setup("img")
		run, err := s.Start(ctx, live("glm-0"))
		if err != nil {
			t.Fatal(err)
		}
		cl.state, cl.log, cl.problem = tc.state, tc.log, tc.problem
		s.Tick(ctx, time.Now().Add(tc.age))
		r := st.runs[run.ID]
		if r.Status != tc.status || !strings.Contains(r.Detail, tc.detail) || (len(cl.deleted) > 0) != tc.deleted {
			t.Errorf("%s: %s %q deleted %v", name, r.Status, r.Detail, cl.deleted)
		}
	}
}

func TestCancel(t *testing.T) {
	ctx := context.Background()
	st, cl, s := setup("img")
	run, _ := s.Start(ctx, live("glm-0"))
	got, err := s.Cancel(ctx, run.ID)
	if err != nil || got.Status != store.RunCancelled || len(cl.deleted) != 1 || st.runs[run.ID].Status != store.RunCancelled {
		t.Fatalf("cancel: %+v %v %v", got, err, cl.deleted)
	}
	// A run that has ended is left as it is.
	if got, err := s.Cancel(ctx, run.ID); err != nil || got.Status != store.RunCancelled || len(cl.deleted) != 1 {
		t.Fatalf("second cancel: %+v %v", got, err)
	}
}

// Inside its window the campaign measures, drained, what is not measured: one
// at a time per model and site. A model on one site waits for an operator.
func TestCampaign(t *testing.T) {
	ctx := context.Background()
	st, _, s := setup("img")
	st.deployments = append(st.deployments,
		store.Deployment{ModelID: "m1", ClusterID: "c2", ClusterName: "site-b", Namespace: "llm", Name: "glm", Topology: "single", SpecHash: "s2", Instances: ready(1)},
		store.Deployment{ModelID: "m2", ClusterID: "c1", ClusterName: "site-a", Namespace: "llm", Name: "qwen", Topology: "single", SpecHash: "q1", Instances: ready(2)})
	// Thursdays only; glm's use was all on Wednesday, so the window has none.
	thursday, _ := capacity.ParseWindow("Thu 00:00 - Thu 23:59 UTC")
	s.opt.Window = thursday
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	st.models = []store.Model{{ID: "m1", Name: "glm"}, {ID: "m2", Name: "qwen"}}
	st.hours = []store.HourUse{{ModelID: "m1", Unit: store.UnitTokens, Hour: now.Add(-24 * time.Hour), Used: 1000}}
	s.Tick(ctx, now)
	s.Tick(ctx, now)
	var started []string
	for _, r := range st.runs {
		started = append(started, r.ClusterID+"/"+r.Deployment+" "+r.Mode+" "+r.StartedBy)
	}
	if len(started) != 1 || started[0] != "c1/glm drained campaign" {
		t.Fatalf("started %v", started)
	}

	// With traffic in the window that site-b, not measured, cannot be shown
	// to take, site-a is not drained.
	st, _, s = setup("img")
	st.deployments = append(st.deployments,
		store.Deployment{ModelID: "m1", ClusterID: "c2", ClusterName: "site-b", Namespace: "llm", Name: "glm", Topology: "single", SpecHash: "s2", Instances: ready(1)})
	st.models = []store.Model{{ID: "m1", Name: "glm"}}
	st.hours = []store.HourUse{{ModelID: "m1", Unit: store.UnitTokens, Hour: now.Add(-24 * time.Hour), Used: 1000}}
	win, _ := capacity.ParseWindow("00:00 - 23:59 UTC")
	s.opt.Window = win
	s.Tick(ctx, now)
	if len(st.runs) != 0 {
		t.Fatalf("drained a site the others cannot cover: %+v", st.runs)
	}
	queue, _ := s.Queue(ctx, now)
	if len(queue) != 2 || queue[0].State != llmi.WaitsCover || *queue[0].Cover != 0 {
		t.Fatalf("queue = %+v", queue)
	}
	// Traffic outside the window, or long ago, does not count.
	closed, _ := capacity.ParseWindow("Fri 22:00 - Sat 06:00 UTC")
	s.opt.Window = closed
	if q, _ := s.Queue(ctx, now); q[0].State != llmi.Next {
		t.Fatalf("outside the window: %+v", q[0])
	}
	// Use 31 days ago is no history: whether site-b can take the load
	// cannot be told, so an operator decides.
	s.opt.Window = win
	st.hours[0].Hour = now.AddDate(0, 0, -31)
	if q, _ := s.Queue(ctx, now); q[0].State != llmi.WaitsHistory {
		t.Fatalf("31 days ago: %+v", q[0])
	}

	st, _, s = setup("img")
	s.opt.Window = closed
	s.Tick(ctx, now) // a Thursday
	if len(st.runs) != 0 {
		t.Fatal("measured outside the window")
	}
}

func TestNewID(t *testing.T) {
	id, err := newID()
	if err != nil || len(id) != 36 || id[14] != '4' {
		t.Fatalf("id %q %v", id, err)
	}
}

// An LLMI whose spec hash is not known, as during a rollout, is not
// measured: the result would count for nothing.
func TestUnknownSpec(t *testing.T) {
	st, cl, s := setup("img")
	st.deployments[0].SpecHash = ""
	if _, err := s.Start(context.Background(), live("glm-0")); !errors.Is(err, ErrUnknownSpec) || len(st.runs) != 0 || len(cl.jobs) != 0 {
		t.Fatalf("unknown spec: %v %+v", err, st.runs)
	}
}
