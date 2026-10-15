// Package wallet enforces wallets from the hub: a tenant's one budget across
// every priced model, per hour or per day (the design's fallback, until the
// gateway can enforce it itself).
//
// Every tick it adds up what each wallet tenant spent in its window, from
// the usage history, and closes a tenant whose wallet is spent until the
// window ends: the gateways refuse it on every priced model, by a rule that
// ends with the window by itself, or serve it as best-effort where a model
// does that for a spent budget. It also keeps the tenants' wallet buckets in
// step with their wallets and the models' prices.
package wallet

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"aigw-ui/internal/store"
)

// Store is what the service reads and writes in Postgres.
type Store interface {
	ReconcileWalletBuckets(ctx context.Context) (bool, error)
	OpenWallets(ctx context.Context) ([]string, error)
	WalletSpends(ctx context.Context, now time.Time, tenantID string) ([]store.WalletSpend, error)
	CloseWallet(ctx context.Context, tenantID string, until time.Time, spent int64) (bool, error)
	WalletModels(ctx context.Context) ([]store.WalletModel, error)
	StartOverage(ctx context.Context, modelID, tenantID string, until time.Time) (bool, error)
}

// Syncer is told when a closure began or ended, or a bucket changed.
type Syncer interface {
	Changed(ctx context.Context, action, summary string)
}

type Service struct {
	st    Store
	sy    Syncer
	every time.Duration
}

func New(st Store, sy Syncer, every time.Duration) *Service {
	return &Service{st: st, sy: sy, every: every}
}

// Run ticks until ctx is cancelled.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(s.every)
	defer t.Stop()
	for {
		if err := s.Tick(ctx, time.Now()); err != nil && ctx.Err() == nil {
			slog.Warn("wallets", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick looks once, and syncs the clusters when anything changed.
func (s *Service) Tick(ctx context.Context, now time.Time) error {
	var lines []string
	changed, err := s.st.ReconcileWalletBuckets(ctx)
	if err != nil {
		return err
	}
	if changed {
		lines = append(lines, "Wallet buckets follow the wallets and the models' prices.")
	}
	opened, err := s.st.OpenWallets(ctx)
	if err != nil {
		return err
	}
	for _, slug := range opened {
		lines = append(lines, slug+"'s wallet window began: it is served again.")
	}
	spends, err := s.st.WalletSpends(ctx, now, "")
	if err != nil {
		return err
	}
	var models []store.WalletModel
	for _, w := range spends {
		if w.ClosedUntil != nil || w.Spent < w.Wallet.Credits {
			continue
		}
		closed, err := s.st.CloseWallet(ctx, w.TenantID, w.End, w.Spent)
		if err != nil {
			return err
		}
		if !closed {
			continue
		}
		if models == nil {
			if models, err = s.st.WalletModels(ctx); err != nil {
				return err
			}
		}
		var bestEffort []string
		for _, m := range models {
			if !m.BestEffort {
				continue
			}
			if _, err := s.st.StartOverage(ctx, m.ID, w.TenantID, w.End); err != nil {
				return err
			}
			bestEffort = append(bestEffort, m.Name)
		}
		line := fmt.Sprintf("%s spent its wallet, %s of %s per %s: refused on every priced model until %s UTC.",
			w.Slug, store.Amount(w.Spent, store.UnitCredits), store.Amount(w.Wallet.Credits, store.UnitCredits), window(w.Wallet.Window), w.End.UTC().Format("2006-01-02 15:04"))
		if len(bestEffort) > 0 {
			line = strings.TrimSuffix(line, ".") + ", served as best-effort on " + strings.Join(bestEffort, ", ") + "."
		}
		lines = append(lines, line)
	}
	if len(lines) > 0 {
		s.sy.Changed(ctx, "wallet", strings.Join(lines, " "))
	}
	return nil
}

func window(w string) string {
	if w == "1h" {
		return "hour"
	}
	return "day"
}
