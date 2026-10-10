package store

import (
	"context"
	"crypto/subtle"
	"time"

	"github.com/jackc/pgx/v5"
)

// UsageKey names the usage of one tenant on one model.
type UsageKey struct{ TenantID, ModelID string }

// UsageCursor is what the counters of a tenant on a model held at the last
// look, and the period they belonged to.
type UsageCursor struct {
	UsageKey
	Window      string
	WindowStart time.Time
	Used        int64
	BestEffort  int64
}

// UsageDelta is what a tenant used of a model since the last look, to be
// added to an hour of the history.
type UsageDelta struct {
	UsageKey
	Hour       time.Time
	Unit       string
	Used       int64
	BestEffort int64
}

// usageLock keeps two servers from adding the same usage twice.
const usageLock = 8150115

// RecordUsage hands look the cursors of the last look, adds the usage it
// returns to the history and stores the new cursors. One server at a time
// does this, so nothing is counted twice.
func (s *Store) RecordUsage(ctx context.Context, look func(map[UsageKey]UsageCursor) ([]UsageDelta, []UsageCursor, error)) error {
	return mapErr(pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, usageLock); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT tenant_id, model_id, window_size, window_start, used, best_effort FROM usage_cursors`)
		if err != nil {
			return err
		}
		cursors := map[UsageKey]UsageCursor{}
		for rows.Next() {
			var c UsageCursor
			if err := rows.Scan(&c.TenantID, &c.ModelID, &c.Window, &c.WindowStart, &c.Used, &c.BestEffort); err != nil {
				rows.Close()
				return err
			}
			cursors[c.UsageKey] = c
		}
		if err := rows.Err(); err != nil {
			return err
		}
		deltas, next, err := look(cursors)
		if err != nil {
			return err
		}
		batch := &pgx.Batch{}
		for _, d := range deltas {
			batch.Queue(`INSERT INTO usage_hours (tenant_id, model_id, hour, unit, used, best_effort) VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (tenant_id, model_id, hour, unit) DO UPDATE
				SET used = usage_hours.used + EXCLUDED.used, best_effort = usage_hours.best_effort + EXCLUDED.best_effort`,
				d.TenantID, d.ModelID, d.Hour.UTC().Truncate(time.Hour), d.Unit, d.Used, d.BestEffort)
		}
		for _, c := range next {
			batch.Queue(`INSERT INTO usage_cursors (tenant_id, model_id, window_size, window_start, used, best_effort) VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (tenant_id, model_id) DO UPDATE
				SET window_size = EXCLUDED.window_size, window_start = EXCLUDED.window_start, used = EXCLUDED.used, best_effort = EXCLUDED.best_effort`,
				c.TenantID, c.ModelID, c.Window, c.WindowStart, c.Used, c.BestEffort)
		}
		if batch.Len() == 0 {
			return nil
		}
		return tx.SendBatch(ctx, batch).Close()
	}))
}

// UsagePoint is what one tenant used of one model in one step of the
// history: an hour or a day.
type UsagePoint struct {
	At         time.Time `json:"at"`
	TenantID   string    `json:"tenant_id"`
	TenantSlug string    `json:"tenant_slug"`
	ModelID    string    `json:"model_id"`
	ModelName  string    `json:"model_name"`
	// Unit is "tokens" or "credits". A credit is 0.00001 dollars.
	Unit       string `json:"unit"`
	Used       int64  `json:"used"`
	BestEffort int64  `json:"best_effort"`
}

// History steps.
const (
	StepHour = "hour"
	StepDay  = "day"
)

// UsageHistory returns the recorded usage from a point in time on, oldest
// first, added up per step. Days are days in UTC, like the gateway's.
// tenantID limits it to one tenant.
func (s *Store) UsageHistory(ctx context.Context, from time.Time, step, tenantID string) ([]UsagePoint, error) {
	if step != StepDay {
		step = StepHour
	}
	return list(ctx, s, func(r scanner) (UsagePoint, error) {
		var p UsagePoint
		err := r.Scan(&p.At, &p.TenantID, &p.TenantSlug, &p.ModelID, &p.ModelName, &p.Unit, &p.Used, &p.BestEffort)
		return p, err
	}, `SELECT date_trunc($2, u.hour, 'UTC') AS at, u.tenant_id, t.slug, u.model_id, m.name, u.unit, sum(u.used)::bigint, sum(u.best_effort)::bigint
		FROM usage_hours u JOIN tenants t ON t.id = u.tenant_id JOIN models m ON m.id = u.model_id
		WHERE u.hour >= $1 AND ($3 = '' OR u.tenant_id::text = $3)
		GROUP BY at, u.tenant_id, t.slug, u.model_id, m.name, u.unit
		ORDER BY at, t.slug, m.name`, from, step, tenantID)
}

// PruneUsage deletes the history from before a point in time.
func (s *Store) PruneUsage(ctx context.Context, before time.Time) error {
	_, err := s.db.Exec(ctx, `DELETE FROM usage_hours WHERE hour < $1`, before)
	return mapErr(err)
}

// TenantOfKey returns the tenant an API key belongs to. The keys are stored
// encrypted, so the ones that start like it are opened and compared.
func (s *Store) TenantOfKey(ctx context.Context, key string) (Tenant, error) {
	if len(key) < keyPrefixLen {
		return Tenant{}, ErrNotFound
	}
	rows, err := s.db.Query(ctx,
		`SELECT k.tenant_id, k.key_enc FROM api_keys k JOIN tenants t ON t.id = k.tenant_id
		 WHERE k.key_prefix = $1 AND k.revoked_at IS NULL AND t.enabled`, key[:keyPrefixLen])
	if err != nil {
		return Tenant{}, mapErr(err)
	}
	found := ""
	for rows.Next() {
		var tenantID string
		var enc []byte
		if err := rows.Scan(&tenantID, &enc); err != nil {
			rows.Close()
			return Tenant{}, mapErr(err)
		}
		if plain, err := s.box.Open(enc); err == nil && subtle.ConstantTimeCompare(plain, []byte(key)) == 1 {
			found = tenantID
		}
	}
	if err := rows.Err(); err != nil {
		return Tenant{}, mapErr(err)
	}
	if found == "" {
		return Tenant{}, ErrNotFound
	}
	return s.GetTenant(ctx, found)
}
