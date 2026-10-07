// Package syncer pushes the desired state from Postgres to the clusters and
// pulls the models the clusters already expose.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	locks   sync.Map      // cluster ID -> *sync.Mutex, one sync per cluster at a time
	trigger chan struct{}
}

func New(st *store.Store, auto bool, discoverEvery time.Duration) *Syncer {
	return &Syncer{st: st, auto: auto, every: discoverEvery, trigger: make(chan struct{}, 1)}
}

// Changed records that desired state moved. Clusters are marked pending and,
// when auto sync is on, a background sync is queued.
func (s *Syncer) Changed(ctx context.Context) {
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
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			s.DiscoverAll(ctx)
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
	res, err := s.syncCluster(ctx, id)
	status, msg := "synced", fmt.Sprintf("%d applied, %d removed", res.Applied, res.Pruned)
	if err != nil {
		status, msg = "error", err.Error()
	}
	// The result is worth recording even if the request that asked for it is gone.
	if serr := s.st.SetSyncResult(context.WithoutCancel(ctx), id, status, msg); serr != nil {
		slog.Error("store sync result", "err", serr)
	}
	return res, err
}

func (s *Syncer) syncCluster(ctx context.Context, id string) (kube.SyncResult, error) {
	// State is read under the lock so the last sync to run applies the newest state.
	lock, _ := s.locks.LoadOrStore(id, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()

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
	models := make([]store.DiscoveredModel, 0, len(found))
	for _, f := range found {
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
	changed, err := s.st.ApplyDiscovery(ctx, id, models)
	if err != nil {
		return 0, err
	}
	if changed {
		s.Changed(ctx)
	}
	return len(models), nil
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
