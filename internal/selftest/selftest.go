// Package selftest checks, against a real gateway, that what this tool
// applies to a cluster does what it is meant to: a key is accepted, usage is
// counted where the usage page looks for it, a quota refuses, and a reset
// frees. It does so with a temporary tenant that it removes again.
package selftest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"aigw-ui/internal/store"
	"aigw-ui/internal/syncer"
	"aigw-ui/internal/usage"
)

// TenantPrefix starts the slug of every temporary tenant. The API refuses
// to create other tenants with it, so a leftover is safe to delete.
const TenantPrefix = "selftest-"

var (
	ErrRunning      = errors.New("a self-test is already running; wait for it to finish")
	ErrNoGatewayURL = errors.New("the cluster has no gateway URL, so no request can be sent through its gateway")
	ErrNoModel      = errors.New("the cluster serves no model a quota can be attached to")
)

// Step statuses. A warning is a check that could not give a clear answer.
const (
	Pending = "pending"
	Running = "running"
	Passed  = "passed"
	Failed  = "failed"
	Warning = "warning"
	Skipped = "skipped"
)

type Step struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// Run is one self-test of one cluster.
type Run struct {
	ClusterID   string `json:"cluster_id"`
	ClusterName string `json:"cluster_name"`
	ModelID     string `json:"model_id"`
	ModelName   string `json:"model_name"`
	// Status is "running", "passed" or "failed".
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	Steps      []Step     `json:"steps"`
}

// Runner starts self-tests and remembers the last one of each cluster. Runs
// are kept in memory: they are gone after a restart, and their outcome is in
// the task log.
type Runner struct {
	base  context.Context
	st    *store.Store
	sy    *syncer.Syncer
	usage *usage.Service // nil when no Redis is configured

	mu   sync.Mutex
	runs map[string]*Run // by cluster ID
}

// New builds a Runner. Tests stop when base is cancelled. usage may be nil.
func New(base context.Context, st *store.Store, sy *syncer.Syncer, usage *usage.Service) *Runner {
	return &Runner{base: base, st: st, sy: sy, usage: usage, runs: map[string]*Run{}}
}

// Get returns the last self-test of a cluster.
func (r *Runner) Get(clusterID string) (Run, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run, ok := r.runs[clusterID]
	if !ok {
		return Run{}, false
	}
	return run.copy(), true
}

func (run *Run) copy() Run {
	c := *run
	c.Steps = append([]Step(nil), run.Steps...)
	return c
}

// Start begins a self-test of a cluster in the background and returns it.
// modelID picks the model to test with; empty takes the first one the
// cluster serves that a quota can be attached to.
func (r *Runner) Start(ctx context.Context, clusterID, modelID string) (Run, error) {
	cluster, err := r.st.GetCluster(ctx, clusterID)
	if err != nil {
		return Run{}, err
	}
	if cluster.GatewayURL == "" {
		return Run{}, ErrNoGatewayURL
	}
	model, err := r.pickModel(ctx, cluster.ID, modelID)
	if err != nil {
		return Run{}, err
	}

	r.mu.Lock()
	// One at a time, on any cluster: a run starts by removing the temporary
	// tenants of runs that were cut short, which must not hit a live one.
	for _, other := range r.runs {
		if other.Status == Running {
			r.mu.Unlock()
			return Run{}, ErrRunning
		}
	}
	run := &Run{
		ClusterID: cluster.ID, ClusterName: cluster.Name, ModelID: model.ID, ModelName: model.Name,
		Status: Running, StartedAt: time.Now().UTC(), Steps: plan(),
	}
	r.runs[cluster.ID] = run
	started := run.copy()
	r.mu.Unlock()

	go r.execute(run, cluster, model)
	return started, nil
}

// pickModel returns the model to test with, which must have something on the
// cluster a quota can be attached to.
func (r *Runner) pickModel(ctx context.Context, clusterID, modelID string) (store.Model, error) {
	models, err := r.st.ListModels(ctx)
	if err != nil {
		return store.Model{}, err
	}
	for _, m := range models {
		if modelID != "" && m.ID != modelID {
			continue
		}
		for _, e := range m.Endpoints {
			if e.ClusterID == clusterID && (e.Source == store.SourceManual || len(e.Backends) > 0) {
				return m, nil
			}
		}
		if modelID != "" {
			return store.Model{}, ErrNoModel
		}
	}
	if modelID != "" {
		return store.Model{}, store.ErrNotFound
	}
	return store.Model{}, ErrNoModel
}

// maxDuration bounds a whole run, including the waits for the gateway to
// pick up a change.
const maxDuration = 6 * time.Minute

func (r *Runner) execute(run *Run, cluster store.Cluster, model store.Model) {
	ctx, cancel := context.WithTimeout(r.base, maxDuration)
	defer cancel()
	t := &test{r: r, run: run, ctx: ctx, cluster: cluster, model: model}
	defer func() {
		// A panic must not leave the run "running" forever, or the temporary
		// tenant in place.
		if p := recover(); p != nil {
			slog.Error("self-test panicked", "cluster", cluster.Name, "panic", p)
			t.cleanup()
			r.finish(run, fmt.Sprintf("internal error: %v", p))
		}
	}()
	t.steps()
	r.finish(run, "")
}

// finish closes a run: steps that never ran become skipped, the status is
// set, and the outcome goes to the task log.
func (r *Runner) finish(run *Run, problem string) {
	r.mu.Lock()
	counts := map[string]int{}
	for i := range run.Steps {
		if s := &run.Steps[i]; s.Status == Pending || s.Status == Running {
			s.Status = Skipped
			if s.Detail == "" {
				s.Detail = "Not run because an earlier step failed."
			}
		}
		counts[run.Steps[i].Status]++
	}
	now := time.Now().UTC()
	run.FinishedAt = &now
	run.Status = Passed
	if counts[Failed] > 0 || problem != "" {
		run.Status = Failed
	}
	done := run.copy()
	r.mu.Unlock()

	message := fmt.Sprintf("%d passed, %d failed, %d unclear, %d skipped.", counts[Passed], counts[Failed], counts[Warning], counts[Skipped])
	for _, s := range done.Steps {
		if s.Status == Failed {
			message += " " + s.Title + ": " + s.Detail
		}
	}
	if problem != "" {
		message += " " + problem
	}
	summary := "Self-test of cluster " + done.ClusterName + " with model " + done.ModelName
	if err := r.st.CreateFinishedTask(context.WithoutCancel(r.base), "selftest", summary, done.Status == Passed, message); err != nil {
		slog.Error("record self-test", "err", err)
	}
	slog.Info("self-test finished", "cluster", done.ClusterName, "model", done.ModelName, "status", done.Status)
}

// set stores the outcome of a step.
func (r *Runner) set(run *Run, id, status, detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range run.Steps {
		if run.Steps[i].ID == id {
			run.Steps[i].Status, run.Steps[i].Detail = status, detail
		}
	}
}

func newSlug() (string, error) {
	raw := make([]byte, 4)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return TenantPrefix + hex.EncodeToString(raw), nil
}
