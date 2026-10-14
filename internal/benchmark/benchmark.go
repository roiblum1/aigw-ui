// Package benchmark starts capacity measurements on the clusters and follows
// them. A measurement is a Job on the cluster, so it goes on while the
// server restarts; the server reads the Job's log every little while and
// stores the steps it has printed.
package benchmark

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"aigw-ui/internal/capacity"
	"aigw-ui/internal/kube"
	"aigw-ui/internal/store"
)

var (
	// ErrNoImage is returned when the server has no AIPerf image to run.
	ErrNoImage = errors.New("no AIPerf image is configured")
	// ErrNotServed is returned when the deployment is not one of the model's.
	ErrNotServed = errors.New("the deployment does not serve the model")
	// ErrBusy is returned while a measurement of the model runs.
	ErrBusy = errors.New("a measurement of the model is running")
)

// Store is what the service reads and writes in Postgres.
type Store interface {
	GetDeployment(ctx context.Context, modelID, clusterID, namespace, name string) (store.Deployment, error)
	CreateCapacityRun(ctx context.Context, r store.NewRun) error
	GetCapacityRun(ctx context.Context, id string) (store.CapacityRun, error)
	RunningCapacityRuns(ctx context.Context) ([]store.CapacityRun, error)
	UpdateCapacityRun(ctx context.Context, id string, dataset *capacity.Dataset, steps []capacity.Step) error
	FinishCapacityRun(ctx context.Context, id, status, detail string) error
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

type Service struct {
	st     Store
	client func(ctx context.Context, clusterID string) (Cluster, error)
	image  string
	every  time.Duration
}

// New returns the service. image is the AIPerf image the Jobs run; empty
// turns measurements off.
func New(st Store, client func(ctx context.Context, clusterID string) (Cluster, error), image string, every time.Duration) *Service {
	return &Service{st: st, client: client, image: image, every: every}
}

// Image is the AIPerf image the Jobs run, or "" when there is none.
func (s *Service) Image() string { return s.image }

// Start aims a measurement at one ready pod of one deployment of a model.
// The pod is named on purpose: it keeps its own traffic, whose users see
// slower answers while it runs.
func (s *Service) Start(ctx context.Context, modelID, clusterID, namespace, deployment, pod string, set capacity.Settings) (store.CapacityRun, error) {
	if s.image == "" {
		return store.CapacityRun{}, ErrNoImage
	}
	if err := set.Validate(); err != nil {
		return store.CapacityRun{}, err
	}
	d, err := s.st.GetDeployment(ctx, modelID, clusterID, namespace, deployment)
	if errors.Is(err, store.ErrNotFound) {
		return store.CapacityRun{}, ErrNotServed
	}
	if err != nil {
		return store.CapacityRun{}, err
	}
	cl, err := s.client(ctx, clusterID)
	if err != nil {
		return store.CapacityRun{}, err
	}
	inst, err := cl.Instance(ctx, namespace, deployment, pod)
	if err != nil {
		return store.CapacityRun{}, err
	}
	if inst.MaxModelLen > 0 && set.MaxPromptTokens+set.MaxOutputTokens > inst.MaxModelLen {
		return store.CapacityRun{}, fmt.Errorf("the longest prompt and the longest answer together (%d tokens) exceed the %d the deployment accepts", set.MaxPromptTokens+set.MaxOutputTokens, inst.MaxModelLen)
	}

	id, err := newID()
	if err != nil {
		return store.CapacityRun{}, err
	}
	name := capacity.JobName(id)
	shape := capacity.Shape{Topology: d.Topology, GPU: d.GPU, GPUsPerInstance: d.GPUsPerInstance, Revision: d.Revision}
	err = s.st.CreateCapacityRun(ctx, store.NewRun{ID: id, ModelID: modelID, ClusterID: clusterID, Namespace: namespace, Deployment: deployment,
		Instance: pod, JobName: name, Shape: shape, Settings: set})
	if errors.Is(err, store.ErrConflict) {
		return store.CapacityRun{}, ErrBusy
	}
	if err != nil {
		return store.CapacityRun{}, err
	}
	job, err := capacity.Job(capacity.JobSpec{RunID: id, Name: name, Namespace: namespace, Image: s.image, URL: inst.URL, Model: inst.Model, Settings: set})
	if err == nil {
		err = cl.CreateJob(ctx, job)
	}
	if err != nil {
		s.st.FinishCapacityRun(ctx, id, store.RunFailed, "the Job could not be created: "+err.Error())
		return store.CapacityRun{}, fmt.Errorf("create the Job: %w", err)
	}
	return s.st.GetCapacityRun(ctx, id)
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

// Cancel stops a running measurement and deletes its Job. The steps it has
// finished stay.
func (s *Service) Cancel(ctx context.Context, id string) (store.CapacityRun, error) {
	run, err := s.st.GetCapacityRun(ctx, id)
	if err != nil || run.Status != store.RunRunning {
		return run, err
	}
	cl, err := s.client(ctx, run.ClusterID)
	if err != nil {
		return run, err
	}
	if err := cl.DeleteJob(ctx, run.Namespace, run.JobName); err != nil {
		return run, fmt.Errorf("delete the Job: %w", err)
	}
	if err := s.st.FinishCapacityRun(ctx, id, store.RunCancelled, "stopped by an operator"); err != nil && !errors.Is(err, store.ErrNotFound) {
		return run, err
	}
	return s.st.GetCapacityRun(ctx, id)
}

// Run follows the running measurements until ctx is cancelled.
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(s.every)
	defer ticker.Stop()
	for {
		s.Tick(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Tick reads every running measurement once.
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
}

// imageGrace is how long a pod may wait for its image before the run is
// given up: a pull that keeps failing is a wrong or unmirrored image.
const imageGrace = 3 * time.Minute

// lost is how long after its deadline a run whose cluster cannot be reached
// is given up.
const lost = time.Hour

func (s *Service) follow(ctx context.Context, run store.CapacityRun, now time.Time) error {
	deadline := run.CreatedAt.Add(time.Duration(run.Settings.Duration()) * time.Second)
	cl, err := s.client(ctx, run.ClusterID)
	if err != nil {
		if now.After(deadline.Add(lost)) {
			return s.st.FinishCapacityRun(ctx, run.ID, store.RunFailed, "the cluster could not be reached to read the measurement: "+err.Error())
		}
		return err
	}
	selector := capacity.RunLabel + "=" + run.ID
	text, started, err := cl.PodLog(ctx, run.Namespace, selector, "aiperf")
	if err != nil {
		return err
	}
	var log capacity.Log
	if started {
		log = capacity.ParseLog(text)
		if log.Dataset != nil || len(log.Steps) > len(run.Steps) {
			if err := s.st.UpdateCapacityRun(ctx, run.ID, log.Dataset, log.Steps); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
	}

	state, err := cl.GetJobState(ctx, run.Namespace, run.JobName)
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
		return s.finish(ctx, run.ID, store.RunSucceeded, detail)
	case state.Failed:
		detail := log.Error
		if detail == "" {
			detail = state.Reason
		}
		return s.finish(ctx, run.ID, store.RunFailed, detail)
	case state.Gone:
		return s.finish(ctx, run.ID, store.RunFailed, "the Job was deleted on the cluster before it finished")
	}
	if problem := cl.PodProblem(ctx, run.Namespace, selector); problem != "" && now.Sub(run.CreatedAt) > imageGrace {
		if err := cl.DeleteJob(ctx, run.Namespace, run.JobName); err != nil {
			return err
		}
		return s.finish(ctx, run.ID, store.RunFailed, "the measurement could not start: "+problem)
	}
	return nil
}

func (s *Service) finish(ctx context.Context, id, status, detail string) error {
	err := s.st.FinishCapacityRun(ctx, id, status, detail)
	if errors.Is(err, store.ErrNotFound) {
		return nil // ended meanwhile, by an operator
	}
	return err
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
