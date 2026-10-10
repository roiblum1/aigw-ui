package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// SetModelSpentMode sets what happens to a tenant whose budget for the model
// is spent and returns the model's name. Going back to SpentRefuse ends every
// running overage period of the model.
func (s *Store) SetModelSpentMode(ctx context.Context, id, mode string, unlimited bool) (string, error) {
	var name string
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var fleet bool
		if err := tx.QueryRow(ctx, `SELECT name, fleet FROM models WHERE id = $1 FOR UPDATE`, id).Scan(&name, &fleet); err != nil {
			return err
		}
		if mode == SpentBestEffort && !fleet {
			return ErrNoEntryRoute
		}
		if mode != SpentBestEffort {
			unlimited = false
			if _, err := tx.Exec(ctx, `UPDATE overage SET until = now() WHERE model_id = $1 AND until > now()`, id); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE models SET spent_mode = $2, best_effort_unlimited = $3 WHERE id = $1`, id, mode, unlimited)
		return err
	})
	return name, mapErr(err)
}

const overageSelect = `SELECT o.model_id, m.name, o.tenant_id, t.slug, o.since, o.until, o.until > now()
	FROM overage o JOIN models m ON m.id = o.model_id JOIN tenants t ON t.id = o.tenant_id`

func scanOverage(r scanner) (Overage, error) {
	var o Overage
	err := r.Scan(&o.ModelID, &o.ModelName, &o.TenantID, &o.TenantSlug, &o.Since, &o.Until, &o.Active)
	return o, err
}

// ActiveOverage returns the overage periods that have not ended.
func (s *Store) ActiveOverage(ctx context.Context) ([]Overage, error) {
	return list(ctx, s, scanOverage, overageSelect+` WHERE o.until > now() ORDER BY m.name, t.slug`)
}

// ListOverage returns the newest overage periods, running and ended.
func (s *Store) ListOverage(ctx context.Context, limit int) ([]Overage, error) {
	return list(ctx, s, scanOverage, overageSelect+` ORDER BY o.since DESC LIMIT $1`, limit)
}

// StartOverage records that a tenant is served as best-effort on a model
// until the given time. It reports false when a period is already running.
func (s *Store) StartOverage(ctx context.Context, modelID, tenantID string, until time.Time) (bool, error) {
	tag, err := s.db.Exec(ctx,
		`INSERT INTO overage (model_id, tenant_id, until)
		 SELECT $1, $2, $3 WHERE NOT EXISTS (SELECT 1 FROM overage WHERE model_id = $1 AND tenant_id = $2 AND until > now())`,
		modelID, tenantID, until)
	return tag.RowsAffected() > 0, mapErr(err)
}

// EndOverage ends the running overage period of a tenant on a model, if
// there is one, and reports whether there was.
func (s *Store) EndOverage(ctx context.Context, modelID, tenantID string) (bool, error) {
	tag, err := s.db.Exec(ctx, `UPDATE overage SET until = now() WHERE model_id = $1 AND tenant_id = $2 AND until > now()`, modelID, tenantID)
	return tag.RowsAffected() > 0, mapErr(err)
}

// BestEffortTenants returns, for every model in best-effort mode, the slugs
// of the tenants whose requests go to the model's best-effort route: first
// the tenants in a running overage period, then, when the model says so,
// the tenants without a quota of their own on it. Each group is sorted. The
// route lists a limited number of tenants, and a tenant that was moved
// because its budget is spent must not lose its place to one without a
// quota.
func (s *Store) BestEffortTenants(ctx context.Context) (map[string][]string, error) {
	rows, err := s.db.Query(ctx,
		`SELECT o.model_id, t.slug, 0 AS place FROM overage o
		   JOIN tenants t ON t.id = o.tenant_id JOIN models m ON m.id = o.model_id
		  WHERE o.until > now() AND t.enabled AND m.fleet AND m.spent_mode = 'best-effort'
		 UNION
		 SELECT m.id, t.slug, 1 FROM models m CROSS JOIN tenants t
		  WHERE m.fleet AND m.spent_mode = 'best-effort' AND m.best_effort_unlimited AND t.enabled
		    AND NOT EXISTS (SELECT 1 FROM quotas q WHERE q.model_id = m.id AND q.tenant_id = t.id)
		 ORDER BY 1, 3, 2`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var modelID, slug string
		var place int
		if err := rows.Scan(&modelID, &slug, &place); err != nil {
			return nil, mapErr(err)
		}
		out[modelID] = append(out[modelID], slug)
	}
	return out, mapErr(rows.Err())
}
