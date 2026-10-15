package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"aigw-ui/internal/render"
)

// SetWallet sets a tenant's wallet, nil to remove it, and keeps its wallet
// buckets in step. Removing it also ends a closure.
func (s *Store) SetWallet(ctx context.Context, tenantID string, w *Wallet) (Tenant, error) {
	var credits *int64
	var window *string
	if w != nil {
		credits, window = &w.Credits, &w.Window
	}
	if err := affected(s.db.Exec(ctx, `UPDATE tenants SET wallet_credits = $2, wallet_window = $3 WHERE id = $1`, tenantID, credits, window)); err != nil {
		return Tenant{}, err
	}
	if w == nil {
		if _, err := s.db.Exec(ctx, `DELETE FROM wallet_closures WHERE tenant_id = $1`, tenantID); err != nil {
			return Tenant{}, mapErr(err)
		}
	}
	if _, err := s.ReconcileWalletBuckets(ctx); err != nil {
		return Tenant{}, err
	}
	return s.GetTenant(ctx, tenantID)
}

// pricedModels is the condition of a model with prices in use.
const pricedModels = `EXISTS (SELECT 1 FROM model_prices p WHERE p.model_id = m.id AND p.applied_at IS NOT NULL)`

// ReconcileWalletBuckets gives every wallet tenant a bucket equal to its
// wallet on each priced model it has no cap on, and removes the buckets of
// tenants without a wallet and of models without prices. It reports whether
// anything changed: the clusters then need a sync.
func (s *Store) ReconcileWalletBuckets(ctx context.Context) (bool, error) {
	changed := false
	for _, q := range []string{
		`DELETE FROM quotas q WHERE q.wallet AND (
		     NOT EXISTS (SELECT 1 FROM tenants t WHERE t.id = q.tenant_id AND t.wallet_credits IS NOT NULL)
		     OR NOT EXISTS (SELECT 1 FROM models m WHERE m.id = q.model_id AND ` + pricedModels + `))`,
		`UPDATE quotas q SET token_limit = t.wallet_credits, window_size = t.wallet_window FROM tenants t
		 WHERE q.wallet AND t.id = q.tenant_id AND (q.token_limit, q.window_size) IS DISTINCT FROM (t.wallet_credits, t.wallet_window)`,
	} {
		tag, err := s.db.Exec(ctx, q)
		if err != nil {
			return changed, mapErr(err)
		}
		changed = changed || tag.RowsAffected() > 0
	}
	type pair struct{ tenant, model string }
	missing, err := list(ctx, s, func(r scanner) (pair, error) {
		var p pair
		return p, r.Scan(&p.tenant, &p.model)
	}, `SELECT t.id, m.id FROM tenants t CROSS JOIN models m
	    WHERE t.wallet_credits IS NOT NULL AND `+pricedModels+`
	      AND NOT EXISTS (SELECT 1 FROM quotas q WHERE q.tenant_id = t.id AND q.model_id = m.id)`)
	if err != nil {
		return changed, err
	}
	for _, p := range missing {
		// The lowest position on the model no quota holds, as UpsertQuota
		// picks it; two at once on one model pick again.
		for range 5 {
			_, err = s.db.Exec(ctx,
				`INSERT INTO quotas (tenant_id, model_id, token_limit, window_size, shadow, slot, wallet)
				 SELECT t.id, $2, t.wallet_credits, t.wallet_window, false, (
				     SELECT min(free) FROM generate_series(0, (SELECT count(*) FROM quotas WHERE model_id = $2)::int) free
				     WHERE NOT EXISTS (SELECT 1 FROM quotas q WHERE q.model_id = $2 AND q.slot = free)), true
				 FROM tenants t WHERE t.id = $1 AND t.wallet_credits IS NOT NULL
				 ON CONFLICT (tenant_id, model_id) DO NOTHING`, p.tenant, p.model)
			var pg *pgconn.PgError
			if !errors.As(err, &pg) || pg.ConstraintName != "quotas_model_slot" {
				break
			}
		}
		if err != nil {
			return changed, mapErr(err)
		}
		changed = true
	}
	return changed, nil
}

// WalletSpend is what a wallet tenant has spent in its wallet's running
// window, over every priced model, as the usage history counted it.
type WalletSpend struct {
	TenantID, Slug string
	Wallet         Wallet
	// Start and End are the window's.
	Start, End time.Time
	Spent      int64
	// ByModel is the spend by model name.
	ByModel map[string]int64
	// ClosedUntil is set while the tenant is closed.
	ClosedUntil *time.Time
}

// WalletWindow returns the start and end of the wallet window that holds now:
// on the hour, or at 00:00 UTC.
func WalletWindow(window string, now time.Time) (time.Time, time.Time) {
	d := 24 * time.Hour
	if window == "1h" {
		d = time.Hour
	}
	start := now.UTC().Truncate(d)
	return start, start.Add(d)
}

// WalletSpends returns every enabled wallet tenant's spend in its window
// that holds now; tenantID limits it to one.
func (s *Store) WalletSpends(ctx context.Context, now time.Time, tenantID string) ([]WalletSpend, error) {
	tenants, err := list(ctx, s, func(r scanner) (WalletSpend, error) {
		var w WalletSpend
		err := r.Scan(&w.TenantID, &w.Slug, &w.Wallet.Credits, &w.Wallet.Window, &w.ClosedUntil)
		return w, err
	}, `SELECT t.id, t.slug, t.wallet_credits, t.wallet_window, (SELECT c.until FROM wallet_closures c WHERE c.tenant_id = t.id AND c.until > now())
	    FROM tenants t WHERE t.enabled AND t.wallet_credits IS NOT NULL AND ($1 = '' OR t.id::text = $1) ORDER BY t.slug`, tenantID)
	if err != nil {
		return nil, err
	}
	for i := range tenants {
		w := &tenants[i]
		w.Start, w.End = WalletWindow(w.Wallet.Window, now)
		w.ByModel = map[string]int64{}
		rows, err := s.db.Query(ctx,
			`SELECT m.name, sum(h.used)::bigint FROM usage_hours h JOIN models m ON m.id = h.model_id
			 WHERE h.tenant_id = $1 AND h.unit = 'credits' AND h.hour >= $2 AND h.hour < $3 GROUP BY m.name`, w.TenantID, w.Start, w.End)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var name string
			var used int64
			if err := rows.Scan(&name, &used); err != nil {
				rows.Close()
				return nil, err
			}
			w.ByModel[name] = used
			w.Spent += used
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return tenants, nil
}

// CloseWallet closes a tenant whose wallet is spent until until, and reports
// whether it was open.
func (s *Store) CloseWallet(ctx context.Context, tenantID string, until time.Time, spent int64) (bool, error) {
	tag, err := s.db.Exec(ctx,
		`INSERT INTO wallet_closures (tenant_id, until, spent) VALUES ($1, $2, $3)
		 ON CONFLICT (tenant_id) DO UPDATE SET until = EXCLUDED.until, spent = EXCLUDED.spent, closed_at = now()
		 WHERE wallet_closures.until <= now()`, tenantID, until, spent)
	return tag.RowsAffected() > 0, mapErr(err)
}

// OpenWallets drops the closures whose window has ended and returns the
// slugs of their tenants.
func (s *Store) OpenWallets(ctx context.Context) ([]string, error) {
	return list(ctx, s, func(r scanner) (string, error) {
		var slug string
		return slug, r.Scan(&slug)
	}, `WITH gone AS (DELETE FROM wallet_closures WHERE until <= now() RETURNING tenant_id)
	    SELECT t.slug FROM gone JOIN tenants t ON t.id = gone.tenant_id ORDER BY t.slug`)
}

// WalletModel is a priced model, which wallets cover, and whether a tenant
// whose budget for it is spent is served as best-effort there.
type WalletModel struct {
	ID, Name   string
	BestEffort bool
}

// WalletModels returns the priced models.
func (s *Store) WalletModels(ctx context.Context) ([]WalletModel, error) {
	return list(ctx, s, func(r scanner) (WalletModel, error) {
		var m WalletModel
		return m, r.Scan(&m.ID, &m.Name, &m.BestEffort)
	}, `SELECT m.id, m.name, m.fleet AND m.spent_mode = '`+SpentBestEffort+`' FROM models m WHERE `+pricedModels+` ORDER BY m.name`)
}

// closedWallets returns the closed tenants with the priced models the
// gateways refuse them on: those not served as best-effort, where the wallet
// service moves them to the best-effort route instead.
func (s *Store) closedWallets(ctx context.Context) ([]render.Closure, error) {
	closed, err := list(ctx, s, func(r scanner) (render.Closure, error) {
		var c render.Closure
		return c, r.Scan(&c.Slug, &c.Until)
	}, `SELECT t.slug, c.until FROM wallet_closures c JOIN tenants t ON t.id = c.tenant_id
	    WHERE c.until > now() AND t.enabled ORDER BY t.slug`)
	if err != nil || len(closed) == 0 {
		return nil, err
	}
	models, err := s.WalletModels(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, m := range models {
		if !m.BestEffort {
			names = append(names, m.Name)
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	for i := range closed {
		closed[i].Models = names
	}
	return closed, nil
}
