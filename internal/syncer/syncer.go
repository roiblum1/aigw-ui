// Package syncer pushes the desired state from Postgres to the clusters and
// pulls the models the clusters already expose.
package syncer

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

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

// Auto reports whether a change is synced to the clusters by itself.
func (s *Syncer) Auto() bool { return s.auto }

// Changed records that desired state moved, with a line for the task log
// saying what changed. Clusters are marked pending and, when auto sync is on,
// a background sync is queued.
func (s *Syncer) Changed(ctx context.Context, action, summary string) {
	// A change to the clusters can change which sites a model has.
	if _, err := s.st.RefreshFleetZones(context.WithoutCancel(ctx)); err != nil {
		slog.Error("store zone weights", "err", err)
	}
	s.queue(ctx, action, summary)
}

// queue records a task, marks the clusters pending and, when auto sync is
// on, queues a background sync.
func (s *Syncer) queue(ctx context.Context, action, summary string) {
	s.Record(ctx, action, summary, "")
	// The change is saved already, so the clusters are marked even if the
	// request that made it has gone away.
	if err := s.st.MarkPending(context.WithoutCancel(ctx)); err != nil {
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
	if len(res.Held) > 0 {
		msg += ". " + strings.Join(res.Held, " ")
	}
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
		return fmt.Sprintf("Nothing had to change: all %d objects were already in place.", res.Applied) + heldNote(res)
	}
	noun := "objects"
	if len(res.Changes) == 1 {
		noun = "object"
	}
	return fmt.Sprintf("%d %s changed, %d already in place.", len(res.Changes), noun, res.Applied+res.Pruned-len(res.Changes)) + heldNote(res)
}

func heldNote(res kube.SyncResult) string {
	if len(res.Held) == 0 {
		return ""
	}
	return " " + strings.Join(res.Held, " ")
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
	res, err := client.Sync(ctx, state.Namespace, render.Objects(state), render.HeldNames(state))
	if err != nil {
		return res, err
	}
	for _, m := range state.Models {
		if m.Held() {
			res.Held = append(res.Held, "The entry route of "+m.Name+" was left as it is: "+m.HeldReason+".")
		}
	}
	// Only a sync that applied everything moves the cluster to the revision.
	return res, s.st.SetFleetRevision(ctx, id, render.FleetRevision(state))
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

// ProbeWith is Probe for a cluster that is not stored yet.
func (s *Syncer) ProbeWith(ctx context.Context, kubeconfig []byte, namespace, gatewayName string) (kube.Probe, error) {
	client, err := kube.New(kubeconfig)
	if err != nil {
		return kube.Probe{}, err
	}
	return client.Probe(ctx, namespace, gatewayName)
}

func (s *Syncer) client(ctx context.Context, id string) (*kube.Client, error) {
	kubeconfig, err := s.st.Kubeconfig(ctx, id)
	if err != nil {
		return nil, err
	}
	return kube.New(kubeconfig)
}
