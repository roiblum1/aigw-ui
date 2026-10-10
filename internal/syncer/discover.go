package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"aigw-ui/internal/gateway"
	"aigw-ui/internal/kube"
	"aigw-ui/internal/store"
)

// DiscoverAll polls every cluster for the models it exposes and what it
// serves, and then works out the site weights once for the whole round.
// Doing that after each cluster would send the gateways several sets of
// weights in a row, and every set moves conversations.
func (s *Syncer) DiscoverAll(ctx context.Context) {
	clusters, err := s.st.ListClusters(ctx)
	if err != nil {
		slog.Error("list clusters", "err", err)
		return
	}
	var wg sync.WaitGroup
	for _, c := range clusters {
		wg.Go(func() {
			if _, err := s.pollCluster(ctx, c.ID); err != nil {
				slog.Warn("discovery failed", "cluster", c.Name, "err", err)
			}
		})
	}
	wg.Wait()
	s.refreshWeights(ctx, "a poll of all clusters")
}

// DiscoverCluster reads the models one cluster exposes and stores them. A
// failed poll changes nothing: models are only removed when the cluster
// answered and no longer lists them.
func (s *Syncer) DiscoverCluster(ctx context.Context, id string) (int, error) {
	n, err := s.pollCluster(ctx, id)
	if err == nil {
		s.refreshWeights(ctx, "a poll of one cluster")
	}
	return n, err
}

// pollCluster is DiscoverCluster without the site weights, which the caller
// works out when it has polled every cluster it meant to.
func (s *Syncer) pollCluster(ctx context.Context, id string) (int, error) {
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
		found, err = client.Discover(ctx, gatewayOf(c))
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
			m.Backends = append(m.Backends, store.BackendRef{Name: b.Name, Namespace: ns, Model: b.Model, Override: b.Override, PeerOnly: b.PeerOnly})
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
		s.queue(ctx, "discovery", "Models changed on cluster "+c.Name)
	}
	s.observeCapacity(ctx, c, capacity)
	return len(found), nil
}

// observeCapacity stores how many instances of each model the cluster has
// ready and moves its applied capacities one step towards it.
func (s *Syncer) observeCapacity(ctx context.Context, c store.Cluster, found []kube.ModelCapacity) {
	reported := make([]store.Capacity, 0, len(found))
	for _, f := range found {
		c := store.Capacity{Model: f.Model, Capacity: f.Capacity, Step: f.Step, Detail: f.Detail,
			Revision: f.Revision, MaxModelLen: f.MaxModelLen, Known: f.Known}
		for _, p := range f.Pools {
			c.Pools = append(c.Pools, store.Pool{Namespace: p.Namespace, Name: p.Name, Group: p.Group})
		}
		reported = append(reported, c)
	}
	if err := s.st.ApplyCapacity(ctx, c.ID, reported); err != nil {
		slog.Error("store model capacity", "cluster", c.Name, "err", err)
	}
}

// refreshWeights turns the applied capacities into zone weights and queues a
// sync when a model's weights moved.
func (s *Syncer) refreshWeights(ctx context.Context, after string) {
	moved, err := s.st.RefreshFleetZones(context.WithoutCancel(ctx))
	if err != nil {
		slog.Error("store zone weights", "err", err)
		return
	}
	if len(moved) > 0 {
		// Every cluster gets the new weights: gateways with different
		// weights would send one conversation to different sites.
		s.queue(ctx, "weights", "Site weights of "+strings.Join(moved, ", ")+" changed after "+after)
	}
}

func gatewayOf(c store.Cluster) kube.Gateway {
	return kube.Gateway{Namespace: c.Namespace, Name: c.GatewayName, ClientListener: c.ClientListener}
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
	routes, err := client.DiscoverAttached(ctx, gatewayOf(c))
	if err != nil {
		return nil, fmt.Errorf("read routes for the backends: %w", err)
	}
	names, err := gateway.ListModels(ctx, c.GatewayURL, token)
	if err != nil && c.FleetEnabled {
		// A fleet cluster's client listener can have no route at all until
		// the first entry route is on, and /v1/models then answers 404. What
		// the cluster serves is read from its deployments, so the routes
		// alone are enough here, and they carry every backend a quota can
		// attach to.
		slog.Warn("the gateway did not list its models; using the routes alone", "cluster", c.Name, "err", err)
		return routes, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list models from the gateway: %w", err)
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
