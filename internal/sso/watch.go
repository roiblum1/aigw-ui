package sso

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"

	"aigw-ui/internal/store"
)

// Store is what the Watcher reads and changes.
type Store interface {
	OwnedKeysInGroups(ctx context.Context) ([]store.OwnedKeyInGroup, error)
	RevokeKey(ctx context.Context, id string) (store.APIKey, error)
}

// Watcher reads the Groups every interval and revokes the personal key of
// each person who is no longer in the group of the tenant they made it in.
// Unknown is not empty: when the Groups cannot be read, or a tenant's group
// is not among them (deleted, renamed, or a sync caught half way), nothing
// is revoked.
type Watcher struct {
	cluster *OpenShift
	st      Store
	changed func(ctx context.Context, action, summary string)
	every   time.Duration
}

// NewWatcher returns a Watcher; changed queues a sync of what it revoked.
func NewWatcher(cluster *OpenShift, st Store, changed func(ctx context.Context, action, summary string), every time.Duration) *Watcher {
	return &Watcher{cluster: cluster, st: st, changed: changed, every: every}
}

// Run looks every interval until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	ticker := time.NewTicker(w.every)
	defer ticker.Stop()
	for {
		if _, err := w.Tick(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("openshift sign-in: could not look at the groups; nothing revoked", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Tick reads the Groups and revokes the keys of people who left, and
// returns the client IDs it revoked.
func (w *Watcher) Tick(ctx context.Context) ([]string, error) {
	if err := w.cluster.RefreshGroups(ctx); err != nil {
		return nil, err
	}
	groups, _, _ := w.cluster.Members()
	owned, err := w.st.OwnedKeysInGroups(ctx)
	if err != nil {
		return nil, err
	}
	var revoked, who []string
	for _, o := range owned {
		members, ok := groups[o.Group]
		if !ok {
			continue
		}
		if slices.Contains(members, o.Key.Owner) {
			continue
		}
		k, err := w.st.RevokeKey(ctx, o.Key.ID)
		if err != nil {
			slog.Warn("openshift sign-in: could not revoke the key of someone who left", "client_id", o.Key.ClientID, "err", err)
			continue
		}
		revoked = append(revoked, k.ClientID)
		who = append(who, o.Key.Owner+" ("+o.TenantSlug+", left "+o.Group+")")
	}
	if len(revoked) > 0 {
		slog.Info("openshift sign-in: revoked the keys of people who left their group", "keys", revoked)
		w.changed(ctx, "key.revoke", "Revoked the personal keys of "+strings.Join(who, ", "))
	}
	return revoked, nil
}
