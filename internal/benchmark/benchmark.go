// Package benchmark starts capacity measurements on the clusters and follows
// them. A measurement is a Job on the cluster, so it goes on while the
// server restarts; the server reads the Job's log every little while and
// stores the steps it has printed.
//
// Every LLMInferenceService is measured with the active benchmark profile,
// drained (its site's entry weight taken to 0 first and restored after) or
// live (serving its users, after an operator confirmed it). Inside the
// campaign window the service measures what is not measured or stale by
// itself, drained only.
package benchmark

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"aigw-ui/internal/capacity"
	"aigw-ui/internal/kube"
	"aigw-ui/internal/llmi"
	"aigw-ui/internal/store"
)

var (
	// ErrNoImage is returned when the server has no AIPerf image to run.
	ErrNoImage = errors.New("no AIPerf image is configured")
	// ErrNotServed is returned when the deployment is not one of the model's.
	ErrNotServed = errors.New("the deployment does not serve the model")
	// ErrBusy is returned while a measurement of the model runs.
	ErrBusy = errors.New("a measurement of the model is running")
	// ErrNoOtherSite is returned for a drained measurement of a model that
	// no other site serves: draining it would leave the model unserved.
	ErrNoOtherSite = errors.New("no other site serves the model, so its site cannot be drained; measure it live")
	// ErrUnknownSpec is returned for an LLMI whose spec hash is not known
	// yet: a result would count for nothing.
	ErrUnknownSpec = errors.New("what the LLMInferenceService runs is not known yet: none of its pods is ready, or they run different images, as during a rollout")
	// ErrPeerOff is returned for a prefill/decode LLMI when the hub does
	// not measure them.
	ErrPeerOff = errors.New("measuring a prefill/decode LLMInferenceService is off on this hub: its Job would run in the gateway's namespace with the site's peer client certificate, " +
		"which reaches every model on the peer listener with no key and no quota; see capacity.peerMeasurements in the security guide")
)

// Store is what the service reads and writes in Postgres.
type Store interface {
	GetDeployment(ctx context.Context, modelID, clusterID, namespace, name string) (store.Deployment, error)
	GetCluster(ctx context.Context, id string) (store.Cluster, error)
	ActiveProfile(ctx context.Context) (capacity.Profile, error)
	CreateCapacityRun(ctx context.Context, r store.NewRun) error
	GetCapacityRun(ctx context.Context, id string) (store.CapacityRun, error)
	RunningCapacityRuns(ctx context.Context) ([]store.CapacityRun, error)
	UpdateCapacityRun(ctx context.Context, id string, settings *capacity.Settings, dataset *capacity.Dataset, steps []capacity.Step) error
	// CreateCapacityRun drains the run's site and FinishCapacityRun
	// restores it, each in the transaction that records the run.
	FinishCapacityRun(ctx context.Context, id, status, detail string) error
	SetRunPhase(ctx context.Context, id, phase string) error
	SiteWeight(ctx context.Context, modelID, clusterID string) (*float64, error)
	ListDeployments(ctx context.Context) ([]store.Deployment, error)
	LatestCapacityRuns(ctx context.Context) ([]store.CapacityRun, error)
	ListModels(ctx context.Context) ([]store.Model, error)
	HourlyUse(ctx context.Context, from time.Time) ([]store.HourUse, error)
}

// Cluster is what the service does on one cluster.
type Cluster interface {
	Instance(ctx context.Context, namespace, name, pod string) (kube.Instance, error)
	CreateJob(ctx context.Context, job *unstructured.Unstructured) error
	GetJobState(ctx context.Context, namespace, name string) (kube.JobState, error)
	ReadyPods(ctx context.Context, namespace, name string) ([]string, error)
	PodLog(ctx context.Context, namespace, selector, container string) (string, bool, error)
	PodProblem(ctx context.Context, namespace, selector string) string
	DeleteJob(ctx context.Context, namespace, name string) error
}

// Peer is what a prefill/decode measurement needs to reach the gateway's
// peer listener: the fleet's server name, and the secret and ConfigMap with
// each site's client certificate and CA (the hub's FLEET_* settings).
type Peer struct {
	SNI          string
	ClientSecret string
	CAConfigMap  string
}

// Options are the service's settings.
type Options struct {
	// Image is the AIPerf image the Jobs run; empty turns measurements off.
	Image string
	// Every is how often a running measurement is read.
	Every time.Duration
	Peer  Peer
	// PeerMeasurements turns on measuring prefill/decode LLMIs, whose Jobs
	// mount the peer client certificate in the gateway's namespace.
	PeerMeasurements bool
	// Window is when the service measures by itself; nil never.
	Window *capacity.Window
}

type Service struct {
	st     Store
	client func(ctx context.Context, clusterID string) (Cluster, error)
	opt    Options
	now    func() time.Time
}

// New returns the service.
func New(st Store, client func(ctx context.Context, clusterID string) (Cluster, error), opt Options) *Service {
	return &Service{st: st, client: client, opt: opt, now: time.Now}
}

// Image is the AIPerf image the Jobs run, or "" when there is none.
func (s *Service) Image() string { return s.opt.Image }

// PeerMeasurements is true when prefill/decode LLMIs are measured.
func (s *Service) PeerMeasurements() bool { return s.opt.PeerMeasurements }

// Window is when the service measures by itself, nil when never.
func (s *Service) Window() *capacity.Window { return s.opt.Window }

// Request is a measurement to start.
type Request struct {
	ModelID, ClusterID, Namespace, Deployment string
	// Pod is the instance measured: one ready pod, the leader of a
	// multi-node group. A prefill/decode deployment is measured whole,
	// through its scheduler, and takes no pod.
	Pod string
	// Mode is capacity.ModeDrained or capacity.ModeLive. A live measurement
	// slows its users down; the API asks an operator to confirm it.
	Mode      string
	StartedBy string
}

// Start begins a measurement of one LLMInferenceService with the active
// profile. A drained one drains the site first and starts its Job once the
// entry route sends nothing there; a live one starts at once.
func (s *Service) Start(ctx context.Context, req Request) (store.CapacityRun, error) {
	if s.opt.Image == "" {
		return store.CapacityRun{}, ErrNoImage
	}
	profile, err := s.st.ActiveProfile(ctx)
	if err != nil {
		return store.CapacityRun{}, err
	}
	d, err := s.st.GetDeployment(ctx, req.ModelID, req.ClusterID, req.Namespace, req.Deployment)
	if errors.Is(err, store.ErrNotFound) {
		return store.CapacityRun{}, ErrNotServed
	}
	if err != nil {
		return store.CapacityRun{}, err
	}
	if d.SpecHash == "" {
		return store.CapacityRun{}, ErrUnknownSpec
	}
	if d.Topology == kube.TopologyPrefillDecode && !s.opt.PeerMeasurements {
		return store.CapacityRun{}, ErrPeerOff
	}
	cl, err := s.client(ctx, req.ClusterID)
	if err != nil {
		return store.CapacityRun{}, err
	}
	target, err := s.target(ctx, cl, d, req.Pod)
	if err != nil {
		return store.CapacityRun{}, err
	}
	set := profile.Settings(target.maxModelLen, req.Mode)
	if err := set.Validate(); err != nil {
		return store.CapacityRun{}, err
	}

	id, err := newID()
	if err != nil {
		return store.CapacityRun{}, err
	}
	run := store.NewRun{ID: id, ModelID: req.ModelID, ClusterID: req.ClusterID, Namespace: req.Namespace, Deployment: req.Deployment,
		Instance: target.instance, JobName: capacity.JobName(id), Settings: set, SpecHash: d.SpecHash, Profile: profile,
		Shape:           capacity.Shape{Topology: d.Topology, GPU: d.GPU, GPUsPerInstance: d.GPUsPerInstance, Revision: d.Revision},
		InstancesDuring: target.instancesDuring, Phase: store.PhaseMeasuring, StartedBy: req.StartedBy}
	if req.Mode == capacity.ModeDrained {
		run.Phase, run.DrainedSite = store.PhaseDraining, !d.Drained
	}
	// The run and its drain are recorded together: a model that no other
	// site serves is refused, and nothing is drained or recorded.
	if err := s.st.CreateCapacityRun(ctx, run); errors.Is(err, store.ErrLastSite) {
		return store.CapacityRun{}, ErrNoOtherSite
	} else if errors.Is(err, store.ErrConflict) {
		return store.CapacityRun{}, ErrBusy
	} else if err != nil {
		return store.CapacityRun{}, err
	}
	if run.Phase == store.PhaseDraining {
		return s.st.GetCapacityRun(ctx, id)
	}
	if err := s.createJob(ctx, cl, id, run.Namespace, target, set); err != nil {
		s.st.FinishCapacityRun(ctx, id, store.RunFailed, "the Job could not be created: "+err.Error())
		return store.CapacityRun{}, err
	}
	return s.st.GetCapacityRun(ctx, id)
}

// target is what a measurement is aimed at.
type target struct {
	// instance is the pod measured, or "" for a prefill/decode deployment.
	instance string
	// urls are the addresses the load is spread over; none for a
	// prefill/decode deployment, which is reached through peer.
	urls        []string
	model       string
	metrics     []string
	maxModelLen int
	// instancesDuring is what the result is divided by for one instance.
	instancesDuring float64
	// peer and namespace are set for a prefill/decode deployment: the Job
	// runs in the gateway's namespace and goes through its peer listener.
	peer      *capacity.Peer
	namespace string
}

func (s *Service) target(ctx context.Context, cl Cluster, d store.Deployment, pod string) (target, error) {
	if d.Topology != kube.TopologyPrefillDecode {
		inst, err := cl.Instance(ctx, d.Namespace, d.Name, pod)
		if err != nil {
			return target{}, err
		}
		// Every rank of the pod gets its share of the load, and every rank's
		// counters are added up: the pod is one instance.
		return target{instance: pod, urls: inst.URLs, model: inst.Model, metrics: inst.URLs, maxModelLen: inst.MaxModelLen,
			instancesDuring: 1, namespace: d.Namespace}, nil
	}
	cluster, err := s.st.GetCluster(ctx, d.ClusterID)
	if err != nil {
		return target{}, err
	}
	if cluster.PeerHost == "" || s.opt.Peer.SNI == "" {
		return target{}, errors.New("a prefill/decode deployment is measured through the gateway's peer listener: the cluster needs a peer host, and the hub FLEET_DOMAIN or FLEET_PEER_SNI")
	}
	// Every decode pod: the scheduler spreads the load over them.
	pods, err := cl.ReadyPods(ctx, d.Namespace, d.Name)
	if err != nil {
		return target{}, err
	}
	if len(pods) == 0 {
		return target{}, fmt.Errorf("%s/%s has no ready decode pod", d.Namespace, d.Name)
	}
	t := target{instancesDuring: float64(len(pods)), namespace: cluster.Namespace,
		peer: &capacity.Peer{Address: net.JoinHostPort(cluster.PeerHost, strconv.Itoa(cluster.PeerPort)), SNI: s.opt.Peer.SNI,
			ClientSecret: s.opt.Peer.ClientSecret, CAConfigMap: s.opt.Peer.CAConfigMap}}
	for _, p := range pods {
		inst, err := cl.Instance(ctx, d.Namespace, d.Name, p)
		if err != nil {
			return target{}, err
		}
		t.metrics = append(t.metrics, inst.URLs...)
		t.model, t.maxModelLen = inst.Model, inst.MaxModelLen
	}
	return t, nil
}

func (s *Service) createJob(ctx context.Context, cl Cluster, runID, namespace string, t target, set capacity.Settings) error {
	if t.namespace != "" {
		namespace = t.namespace
	}
	job, err := capacity.Job(capacity.JobSpec{RunID: runID, Name: capacity.JobName(runID), Namespace: namespace, Image: s.opt.Image,
		URLs: t.urls, Model: t.model, Peer: t.peer, MetricsURLs: t.metrics, Settings: set})
	if err == nil {
		err = cl.CreateJob(ctx, job)
	}
	if err != nil {
		return fmt.Errorf("create the Job: %w", err)
	}
	return nil
}

// jobNamespace is where a run's Job is: the gateway's namespace for a
// prefill/decode deployment, the deployment's otherwise.
func (s *Service) jobNamespace(ctx context.Context, run store.CapacityRun) string {
	if run.Shape.Topology == kube.TopologyPrefillDecode {
		if c, err := s.st.GetCluster(ctx, run.ClusterID); err == nil {
			return c.Namespace
		}
	}
	return run.Namespace
}

// Pods returns the ready pods of a deployment that a measurement can be
// aimed at.
func (s *Service) Pods(ctx context.Context, clusterID, namespace, deployment string) ([]string, error) {
	cl, err := s.client(ctx, clusterID)
	if err != nil {
		return nil, err
	}
	return cl.ReadyPods(ctx, namespace, deployment)
}

// Cancel stops a running measurement, deletes its Job and restores a drain
// it made. The steps it has finished stay.
func (s *Service) Cancel(ctx context.Context, id string) (store.CapacityRun, error) {
	run, err := s.st.GetCapacityRun(ctx, id)
	if err != nil || run.Status != store.RunRunning {
		return run, err
	}
	if run.Phase != store.PhaseDraining {
		cl, err := s.client(ctx, run.ClusterID)
		if err != nil {
			return run, err
		}
		if err := cl.DeleteJob(ctx, s.jobNamespace(ctx, run), run.JobName); err != nil {
			return run, fmt.Errorf("delete the Job: %w", err)
		}
	}
	if err := s.finish(ctx, run, store.RunCancelled, "stopped by an operator"); err != nil {
		return run, err
	}
	return s.st.GetCapacityRun(ctx, id)
}

// Run follows the running measurements, and inside the window starts the
// ones the campaign needs, until ctx is cancelled.
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(s.opt.Every)
	defer ticker.Stop()
	for {
		s.Tick(ctx, s.now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Tick reads every running measurement once, then starts what the campaign
// may start.
func (s *Service) Tick(ctx context.Context, now time.Time) {
	runs, err := s.st.RunningCapacityRuns(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("capacity: could not list the running measurements", "err", err)
		}
		return
	}
	for _, run := range runs {
		if err := s.follow(ctx, run, now); err != nil && ctx.Err() == nil {
			slog.Warn("capacity: could not read a measurement; it is read again at the next look", "run", run.ID, "model", run.ModelName, "cluster", run.ClusterName, "err", err)
		}
	}
	if s.opt.Window != nil && s.opt.Window.Open(now) && s.opt.Image != "" {
		s.campaign(ctx, now)
	}
}

// imageGrace is how long a pod may wait for its image before the run is
// given up: a pull that keeps failing is a wrong or unmirrored image.
const imageGrace = 3 * time.Minute

// lost is how long after its deadline a run whose cluster cannot be reached
// is given up.
const lost = time.Hour

// drainTimeout is how long a drained measurement waits for the entry route
// to send nothing to the site: the weight steps down one instance a
// discovery round.
const drainTimeout = 30 * time.Minute

func (s *Service) follow(ctx context.Context, run store.CapacityRun, now time.Time) error {
	if run.Phase == store.PhaseDraining {
		return s.drained(ctx, run, now)
	}
	deadline := run.CreatedAt.Add(time.Duration(run.Settings.Duration())*time.Second + drainTimeout)
	cl, err := s.client(ctx, run.ClusterID)
	if err != nil {
		if now.After(deadline.Add(lost)) {
			return s.finish(ctx, run, store.RunFailed, "the cluster could not be reached to read the measurement: "+err.Error())
		}
		return err
	}
	namespace := s.jobNamespace(ctx, run)
	selector := capacity.RunLabel + "=" + run.ID
	text, started, err := cl.PodLog(ctx, namespace, selector, "aiperf")
	if err != nil {
		return err
	}
	var log capacity.Log
	if started {
		log = capacity.ParseLog(text)
		var fitted *capacity.Settings
		// Fitting changes only the longest prompt and answer.
		if f := log.Fit; f != nil && (f.Settings.MaxPromptTokens != run.Settings.MaxPromptTokens || f.Settings.MaxOutputTokens != run.Settings.MaxOutputTokens) {
			fitted = &log.Fit.Settings
		}
		if fitted != nil || log.Dataset != nil || len(log.Steps) > len(run.Steps) {
			if err := s.st.UpdateCapacityRun(ctx, run.ID, fitted, log.Dataset, log.Steps); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		// The Job measures with the settings it fitted to the instance.
		if log.Fit != nil {
			run.Settings = log.Fit.Settings
		}
	}

	state, err := cl.GetJobState(ctx, namespace, run.JobName)
	if err != nil {
		return err
	}
	switch {
	case log.Done || state.Succeeded:
		result := capacity.Analyze(run.Settings, log.Steps)
		detail := result.Limit
		if result.Best == nil {
			detail = "no step met the targets: " + result.Limit
		}
		return s.finish(ctx, run, store.RunSucceeded, detail)
	case state.Failed:
		detail := log.Error
		if detail == "" {
			detail = state.Reason
		}
		return s.finish(ctx, run, store.RunFailed, detail)
	case state.Gone:
		return s.finish(ctx, run, store.RunFailed, "the Job was deleted on the cluster before it finished")
	}
	if problem := cl.PodProblem(ctx, namespace, selector); problem != "" && now.Sub(run.CreatedAt) > imageGrace {
		if err := cl.DeleteJob(ctx, namespace, run.JobName); err != nil {
			return err
		}
		return s.finish(ctx, run, store.RunFailed, "the measurement could not start: "+problem)
	}
	return nil
}

// drained starts a drained measurement's Job once the entry route sends
// nothing to its site, and gives up after drainTimeout.
func (s *Service) drained(ctx context.Context, run store.CapacityRun, now time.Time) error {
	weight, err := s.st.SiteWeight(ctx, run.ModelID, run.ClusterID)
	if err != nil {
		return err
	}
	if weight == nil || *weight > 0 {
		if now.Sub(run.CreatedAt) > drainTimeout {
			return s.finish(ctx, run, store.RunFailed, fmt.Sprintf("the entry route still sent to the site after %d minutes of draining", int(drainTimeout.Minutes())))
		}
		return nil
	}
	cl, err := s.client(ctx, run.ClusterID)
	if err != nil {
		return err
	}
	d, err := s.st.GetDeployment(ctx, run.ModelID, run.ClusterID, run.Namespace, run.Deployment)
	if err != nil {
		return s.finish(ctx, run, store.RunFailed, "the deployment is gone: "+err.Error())
	}
	t, err := s.target(ctx, cl, d, run.Instance)
	if err == nil {
		err = s.createJob(ctx, cl, run.ID, run.Namespace, t, run.Settings)
	}
	if err != nil {
		return s.finish(ctx, run, store.RunFailed, "the measurement could not start once the site was drained: "+err.Error())
	}
	return s.st.SetRunPhase(ctx, run.ID, store.PhaseMeasuring)
}

// finish ends a run. The store restores a drain the run made in the same
// transaction, so a hub that stops here never leaves the site drained; the
// entry route's weight then comes back with the usual steps.
func (s *Service) finish(ctx context.Context, run store.CapacityRun, status, detail string) error {
	if err := s.st.FinishCapacityRun(ctx, run.ID, status, detail); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil // ErrNotFound: ended meanwhile, by an operator
}

// loadDays is how far back the campaign looks for a model's busiest hour
// in the window.
const loadDays = 30

// Queue is what the campaign has to measure and what each item waits for.
func (s *Service) Queue(ctx context.Context, now time.Time) ([]llmi.QueueItem, error) {
	profile, err := s.st.ActiveProfile(ctx)
	if err != nil {
		return nil, err
	}
	deployments, err := s.st.ListDeployments(ctx)
	if err != nil {
		return nil, err
	}
	latest, err := s.st.LatestCapacityRuns(ctx)
	if err != nil {
		return nil, err
	}
	running, err := s.st.RunningCapacityRuns(ctx)
	if err != nil {
		return nil, err
	}
	load, err := s.load(ctx, now)
	if err != nil {
		return nil, err
	}
	return llmi.Queue(deployments, latest, running, profile.Version, now, load, s.opt.PeerMeasurements), nil
}

// load is each model's busiest hour in the window over the last loadDays,
// in its unit, with its prices. Without a window it is the busiest hour of
// all.
func (s *Service) load(ctx context.Context, now time.Time) (map[string]llmi.Load, error) {
	models, err := s.st.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	hours, err := s.st.HourlyUse(ctx, now.AddDate(0, 0, -loadDays))
	if err != nil {
		return nil, err
	}
	out := make(map[string]llmi.Load, len(models))
	for _, m := range models {
		l := llmi.Load{}
		if m.Prices != nil {
			l.Prices = &capacity.Prices{Input: m.Prices.Input, Cached: m.Prices.Cached, Output: m.Prices.Output}
		}
		out[m.ID] = l
	}
	for _, h := range hours {
		l, ok := out[h.ModelID]
		if !ok || h.Unit != unitOf(l) {
			continue
		}
		l.History = true
		if s.opt.Window == nil || s.opt.Window.Overlaps(h.Hour, h.Hour.Add(time.Hour)) {
			l.BusiestHour = max(l.BusiestHour, float64(h.Used))
		}
		out[h.ModelID] = l
	}
	return out, nil
}

// unitOf is the unit a model's use is recorded in now.
func unitOf(l llmi.Load) string {
	if l.Prices != nil {
		return store.UnitCredits
	}
	return store.UnitTokens
}

// campaign starts a drained measurement of whatever is next in the queue,
// one per site and one per model at a time.
func (s *Service) campaign(ctx context.Context, now time.Time) {
	queue, err := s.Queue(ctx, now)
	if err != nil {
		slog.Warn("capacity campaign: could not read the queue", "err", err)
		return
	}
	for _, item := range queue {
		if item.State != llmi.Next {
			continue
		}
		pods, err := s.Pods(ctx, item.ClusterID, item.Namespace, item.Name)
		if err != nil || len(pods) == 0 {
			continue
		}
		pod := pods[0]
		if item.Topology == kube.TopologyPrefillDecode {
			pod = ""
		}
		run, err := s.Start(ctx, Request{ModelID: item.ModelID, ClusterID: item.ClusterID, Namespace: item.Namespace, Deployment: item.Name,
			Pod: pod, Mode: capacity.ModeDrained, StartedBy: store.ByCampaign})
		if err != nil {
			slog.Info("capacity campaign: could not start", "llmi", item.ClusterName+"/"+item.Namespace+"/"+item.Name, "err", err)
			continue
		}
		slog.Info("capacity campaign: measuring", "run", run.ID, "llmi", item.ClusterName+"/"+item.Namespace+"/"+item.Name, "reason", item.Reason)
		// One at a time per site and per model: read the queue again at the
		// next tick, with this run in it.
		return
	}
}

// newID returns a random UUID.
func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
