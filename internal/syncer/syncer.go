// Package syncer pushes the desired state from Postgres to the clusters and
// pulls the models the clusters already expose.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"aigw-ui/internal/gateway"
	"aigw-ui/internal/kube"
	"aigw-ui/internal/render"
	"aigw-ui/internal/store"
)

type Syncer struct {
	st      *store.Store
	auto    bool
	every   time.Duration // discovery interval, 0 disables polling
	resync  time.Duration // full sync interval, 0 disables it
	locks   sync.Map      // cluster ID -> *sync.Mutex, one sync per cluster at a time
	trigger chan struct{}
}

func New(st *store.Store, auto bool, discoverEvery, syncEvery time.Duration) *Syncer {
	return &Syncer{st: st, auto: auto, every: discoverEvery, resync: syncEvery, trigger: make(chan struct{}, 1)}
}

// Changed records that desired state moved, with a line for the task log
// saying what changed. Clusters are marked pending and, when auto sync is on,
// a background sync is queued.
func (s *Syncer) Changed(ctx context.Context, action, summary string) {
	s.Record(ctx, action, summary, "")
	// A change to the clusters can change which sites a model has.
	if _, err := s.st.RefreshFleetZones(context.WithoutCancel(ctx)); err != nil {
		slog.Error("store zone weights", "err", err)
	}
	if err := s.st.MarkPending(ctx); err != nil {
		slog.Error("mark clusters pending", "err", err)
	}
	if !s.auto {
		return
	}
	select {
	case s.trigger <- struct{}{}:
	default: // a sync is already queued
	}
}

// Record adds a task to the log without queueing a sync, for a caller that
// runs the sync itself. clusterID limits it to one cluster when not empty.
func (s *Syncer) Record(ctx context.Context, action, summary, clusterID string) {
	// What the task describes is already saved, so it is recorded even if the
	// request that made it has gone away.
	if err := s.st.CreateTask(context.WithoutCancel(ctx), action, summary, clusterID); err != nil {
		slog.Error("record task", "err", err)
	}
}

// Run processes queued syncs and polls the clusters for models until ctx is
// cancelled.
func (s *Syncer) Run(ctx context.Context) {
	var tick <-chan time.Time // stays nil, and so never fires, when polling is off
	if s.every > 0 {
		ticker := time.NewTicker(s.every)
		defer ticker.Stop()
		tick = ticker.C
		s.DiscoverAll(ctx)
	}
	// The periodic sync retries clusters that failed and puts back objects
	// someone changed or deleted by hand. With auto sync off the operator
	// decides when clusters change, so it stays off too.
	var resync <-chan time.Time
	if s.auto && s.resync > 0 {
		ticker := time.NewTicker(s.resync)
		defer ticker.Stop()
		resync = ticker.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			s.DiscoverAll(ctx)
		case <-resync:
			s.SyncAll(ctx)
		case <-s.trigger:
			// Let a burst of edits settle into one sync.
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			s.SyncAll(ctx)
		}
	}
}

func (s *Syncer) SyncAll(ctx context.Context) {
	clusters, err := s.st.ListClusters(ctx)
	if err != nil {
		slog.Error("list clusters", "err", err)
		return
	}
	var wg sync.WaitGroup
	for _, c := range clusters {
		wg.Go(func() {
			if _, err := s.SyncCluster(ctx, c.ID); err != nil {
				slog.Warn("sync failed", "cluster", c.Name, "err", err)
			}
		})
	}
	wg.Wait()
}

// SyncCluster applies the desired state to one cluster and stores the outcome.
func (s *Syncer) SyncCluster(ctx context.Context, id string) (kube.SyncResult, error) {
	// State is read under the lock so the last sync to run applies the newest state.
	lock, _ := s.locks.LoadOrStore(id, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()

	// The open tasks are read before the state, so the state that is applied
	// is sure to contain their changes.
	tasks, terr := s.st.OpenTasks(ctx, id)
	if terr != nil {
		slog.Error("read open tasks", "err", terr)
	}
	res, err := s.syncCluster(ctx, id)
	status, msg := "synced", fmt.Sprintf("%d applied, %d removed", res.Applied, res.Pruned)
	if err != nil {
		status, msg = "error", err.Error()
	}
	// The result is worth recording even if the request that asked for it is gone.
	done := context.WithoutCancel(ctx)
	if serr := s.st.SetSyncResult(done, id, status, msg); serr != nil {
		slog.Error("store sync result", "err", serr)
	}
	if serr := s.st.FinishTasks(done, id, tasks, err == nil, taskMessage(res, err), res.Changes, res.Rejected); serr != nil {
		slog.Error("store task results", "err", serr)
	}
	return res, err
}

// taskMessage says in one line what a sync did, for the task log.
func taskMessage(res kube.SyncResult, err error) string {
	switch {
	case err != nil:
		return err.Error() + " (will be tried again)"
	case len(res.Changes) == 0:
		return fmt.Sprintf("Nothing had to change: all %d objects were already in place.", res.Applied)
	}
	noun := "objects"
	if len(res.Changes) == 1 {
		noun = "object"
	}
	return fmt.Sprintf("%d %s changed, %d already in place.", len(res.Changes), noun, res.Applied+res.Pruned-len(res.Changes))
}

func (s *Syncer) syncCluster(ctx context.Context, id string) (kube.SyncResult, error) {
	state, err := s.st.RenderState(ctx, id)
	if err != nil {
		return kube.SyncResult{}, err
	}
	client, err := s.client(ctx, id)
	if err != nil {
		return kube.SyncResult{}, err
	}
	return client.Sync(ctx, state.Namespace, render.Objects(state))
}

func (s *Syncer) Probe(ctx context.Context, id string) (kube.Probe, error) {
	c, err := s.st.GetCluster(ctx, id)
	if err != nil {
		return kube.Probe{}, err
	}
	client, err := s.client(ctx, id)
	if err != nil {
		return kube.Probe{}, err
	}
	return client.Probe(ctx, c.Namespace, c.GatewayName)
}

func (s *Syncer) client(ctx context.Context, id string) (*kube.Client, error) {
	kubeconfig, err := s.st.Kubeconfig(ctx, id)
	if err != nil {
		return nil, err
	}
	return kube.New(kubeconfig)
}

// DiscoverAll polls every cluster for the models it exposes.
func (s *Syncer) DiscoverAll(ctx context.Context) {
	clusters, err := s.st.ListClusters(ctx)
	if err != nil {
		slog.Error("list clusters", "err", err)
		return
	}
	var wg sync.WaitGroup
	for _, c := range clusters {
		wg.Go(func() {
			if _, err := s.DiscoverCluster(ctx, c.ID); err != nil {
				slog.Warn("discovery failed", "cluster", c.Name, "err", err)
			}
		})
	}
	wg.Wait()
}

// DiscoverCluster reads the models one cluster exposes and stores them. A
// failed poll changes nothing: models are only removed when the cluster
// answered and no longer lists them.
func (s *Syncer) DiscoverCluster(ctx context.Context, id string) (int, error) {
	n, err := s.discoverCluster(ctx, id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		if serr := s.st.SetDiscoveryError(context.WithoutCancel(ctx), id, err.Error()); serr != nil {
			slog.Error("store discovery result", "err", serr)
		}
	}
	return n, err
}

func (s *Syncer) discoverCluster(ctx context.Context, id string) (int, error) {
	c, err := s.st.GetCluster(ctx, id)
	if err != nil {
		return 0, err
	}
	client, err := s.client(ctx, id)
	if err != nil {
		return 0, err
	}
	var found []kube.DiscoveredModel
	if c.GatewayURL != "" {
		found, err = s.discoverViaGateway(ctx, c, client)
	} else {
		found, err = client.Discover(ctx, c.Namespace)
	}
	if err != nil {
		return 0, err
	}
	// What the cluster serves is read in the same poll. If it cannot be
	// read the poll fails and nothing changes: "unknown" must not be taken
	// for "nothing is running".
	capacity, err := client.Capacity(ctx)
	if err != nil {
		return 0, fmt.Errorf("read what the cluster serves: %w", err)
	}
	models := make([]store.DiscoveredModel, 0, len(found))
	exposed := map[string]bool{}
	for _, f := range found {
		exposed[f.Name] = true
		m := store.DiscoveredModel{Name: f.Name}
		for _, b := range f.Backends {
			ns := b.Namespace
			if ns == c.Namespace {
				ns = "" // stored as "the gateway namespace" so it follows a change of that setting
			}
			m.Backends = append(m.Backends, store.BackendRef{Name: b.Name, Namespace: ns, Model: b.Model, Override: b.Override})
		}
		models = append(models, m)
	}
	// A cluster serves a model when it has a deployment of it, whether or
	// not its own gateway still has a route for it.
	for _, mc := range capacity {
		if !exposed[mc.Model] {
			models = append(models, store.DiscoveredModel{Name: mc.Model, OnlyIfKnown: true})
		}
	}
	changed, err := s.st.ApplyDiscovery(ctx, id, models)
	if err != nil {
		return 0, err
	}
	if changed {
		s.Changed(ctx, "discovery", "Models changed on cluster "+c.Name)
	}
	s.observeCapacity(ctx, c, capacity)
	return len(found), nil
}

// observeCapacity stores how many instances of each model the cluster has
// ready and moves the site weights towards it.
func (s *Syncer) observeCapacity(ctx context.Context, c store.Cluster, found []kube.ModelCapacity) {
	reported := make([]store.Capacity, 0, len(found))
	for _, f := range found {
		reported = append(reported, store.Capacity{Model: f.Model, Capacity: f.Capacity, Step: f.Step, Detail: f.Detail,
			Revision: f.Revision, MaxModelLen: f.MaxModelLen, Known: f.Known})
	}
	if err := s.st.ApplyCapacity(ctx, c.ID, reported); err != nil {
		slog.Error("store model capacity", "cluster", c.Name, "err", err)
		return
	}
	moved, err := s.st.RefreshFleetZones(ctx)
	if err != nil {
		slog.Error("store zone weights", "err", err)
		return
	}
	if len(moved) > 0 {
		// Every cluster gets the new weights: gateways with different
		// weights would send one conversation to different sites.
		s.Changed(ctx, "weights", "Site weights of "+strings.Join(moved, ", ")+" changed after a poll of cluster "+c.Name)
	}
}

// discoverViaGateway takes the list of models from the gateway itself, which
// knows every route attached to it whatever its namespace or backend type, and
// looks up each model's backends from the routes so quotas can be attached.
// If either source fails the poll fails and nothing changes: saving the names
// without their backends would remove the quota policies already in place.
func (s *Syncer) discoverViaGateway(ctx context.Context, c store.Cluster, client *kube.Client) ([]kube.DiscoveredModel, error) {
	token, err := s.st.DiscoveryToken(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	names, err := gateway.ListModels(ctx, c.GatewayURL, token)
	if err != nil {
		return nil, fmt.Errorf("list models from the gateway: %w", err)
	}
	routes, err := client.DiscoverAttached(ctx, c.Namespace, c.GatewayName)
	if err != nil {
		return nil, fmt.Errorf("read routes for the backends: %w", err)
	}
	byName := make(map[string]kube.DiscoveredModel, len(routes))
	for _, r := range routes {
		byName[r.Name] = r
	}
	found := make([]kube.DiscoveredModel, 0, len(names))
	for _, name := range names {
		found = append(found, kube.DiscoveredModel{Name: name, Backends: byName[name].Backends})
	}
	return found, nil
}
