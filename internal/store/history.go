package store

import (
	"context"
	"crypto/subtle"
	"time"

	"github.com/jackc/pgx/v5"
)

// UsageKey names the usage of one tenant on one model, or of one of its
// keys when KeyID is set (the gateways count each key too, 0.37.0).
type UsageKey struct{ TenantID, ModelID, KeyID string }

// UsageCursor is what the counters of a tenant on a model held at the last
// look before 0.30.0, and the period they belonged to. The first look after
// the upgrade starts from them.
type UsageCursor struct {
	UsageKey
	Window      string
	WindowStart time.Time
	Used        int64
	BestEffort  int64
}

// Kinds of the counters the history reads. The token kinds are those of
// render.TokenKind: "requests", "input", "cached" and "output".
const (
	KindUsed       = "used"
	KindBestEffort = "best_effort"
)

// CounterCursor is what one counter in Redis held at the last look.
type CounterCursor struct {
	// Key is the counter's Redis key, which names its window too.
	Key string
	UsageKey
	Kind string
	// Unit is that of a "used" or "best_effort" counter.
	Unit        string
	Window      string
	WindowStart time.Time
	Value       int64
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

// TokenDelta is what a tenant sent to a model since the last look, to be
// added to an hour of the token history: token_hours, or key_token_hours
// for one key's (KeyID).
type TokenDelta struct {
	UsageKey
	Hour                            time.Time
	Requests, Input, Cached, Output int64
}

// Before is what the last look left: a cursor for each counter that held
// something, by its key, and the cursors from before 0.30.0, which only the
// first look after the upgrade gets.
type Before struct {
	Cursors map[string]CounterCursor
	Legacy  map[UsageKey]UsageCursor
}

// Look is what one look at the counters found: what to add to the history,
// and the cursors that replace all the earlier ones.
type Look struct {
	Usage   []UsageDelta
	Tokens  []TokenDelta
	Cursors []CounterCursor
}

// usageLock keeps two servers from adding the same usage twice.
const usageLock = 8150115

// RecordUsage hands look what the last look left, adds what it found to the
// history and stores its cursors. One server at a time does this, so
// nothing is counted twice.
func (s *Store) RecordUsage(ctx context.Context, look func(Before) (Look, error)) error {
	return mapErr(pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, usageLock); err != nil {
			return err
		}
		before, err := readCursors(ctx, tx)
		if err != nil {
			return err
		}
		found, err := look(before)
		if err != nil {
			return err
		}
		batch := &pgx.Batch{}
		for _, d := range found.Usage {
			batch.Queue(`INSERT INTO usage_hours (tenant_id, model_id, hour, unit, used, best_effort) VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (tenant_id, model_id, hour, unit) DO UPDATE
				SET used = usage_hours.used + EXCLUDED.used, best_effort = usage_hours.best_effort + EXCLUDED.best_effort`,
				d.TenantID, d.ModelID, d.Hour.UTC().Truncate(time.Hour), d.Unit, d.Used, d.BestEffort)
		}
		for _, d := range found.Tokens {
			if d.KeyID != "" {
				batch.Queue(`INSERT INTO key_token_hours (key_id, model_id, hour, requests, input_tokens, cached_tokens, output_tokens)
					VALUES ($1, $2, $3, $4, $5, $6, $7)
					ON CONFLICT (key_id, model_id, hour) DO UPDATE
					SET requests = key_token_hours.requests + EXCLUDED.requests, input_tokens = key_token_hours.input_tokens + EXCLUDED.input_tokens,
					    cached_tokens = key_token_hours.cached_tokens + EXCLUDED.cached_tokens, output_tokens = key_token_hours.output_tokens + EXCLUDED.output_tokens`,
					d.KeyID, d.ModelID, d.Hour.UTC().Truncate(time.Hour), d.Requests, d.Input, d.Cached, d.Output)
				continue
			}
			batch.Queue(`INSERT INTO token_hours (tenant_id, model_id, hour, requests, input_tokens, cached_tokens, output_tokens)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
				ON CONFLICT (tenant_id, model_id, hour) DO UPDATE
				SET requests = token_hours.requests + EXCLUDED.requests, input_tokens = token_hours.input_tokens + EXCLUDED.input_tokens,
				    cached_tokens = token_hours.cached_tokens + EXCLUDED.cached_tokens, output_tokens = token_hours.output_tokens + EXCLUDED.output_tokens`,
				d.TenantID, d.ModelID, d.Hour.UTC().Truncate(time.Hour), d.Requests, d.Input, d.Cached, d.Output)
		}
		kept := make(map[string]bool, len(found.Cursors))
		for _, c := range found.Cursors {
			kept[c.Key] = true
			if old, ok := before.Cursors[c.Key]; ok && old == c {
				continue
			}
			batch.Queue(`INSERT INTO usage_counter_cursors (redis_key, tenant_id, model_id, kind, unit, window_size, window_start, value, key_id)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, nullif($9, '')::uuid)
				ON CONFLICT (redis_key) DO UPDATE SET value = EXCLUDED.value`,
				c.Key, c.TenantID, c.ModelID, c.Kind, c.Unit, c.Window, c.WindowStart, c.Value, c.KeyID)
		}
		// The counters that held nothing this time, or whose window is over
		// and was read to its end, lose their cursor. Only those are named:
		// the lock and the cursors read under it say which they are.
		var gone []string
		for key := range before.Cursors {
			if !kept[key] {
				gone = append(gone, key)
			}
		}
		if len(gone) > 0 {
			batch.Queue(`DELETE FROM usage_counter_cursors WHERE redis_key = ANY($1)`, gone)
		}
		if len(before.Legacy) > 0 {
			batch.Queue(`DELETE FROM usage_cursors`)
		}
		if batch.Len() == 0 {
			return nil
		}
		return tx.SendBatch(ctx, batch).Close()
	}))
}

// readCursors reads what the last look left.
func readCursors(ctx context.Context, tx pgx.Tx) (Before, error) {
	before := Before{Cursors: map[string]CounterCursor{}, Legacy: map[UsageKey]UsageCursor{}}
	rows, err := tx.Query(ctx, `SELECT redis_key, tenant_id, model_id, kind, unit, window_size, window_start, value, coalesce(key_id::text, '')
		FROM usage_counter_cursors`)
	if err != nil {
		return before, err
	}
	for rows.Next() {
		var c CounterCursor
		if err := rows.Scan(&c.Key, &c.TenantID, &c.ModelID, &c.Kind, &c.Unit, &c.Window, &c.WindowStart, &c.Value, &c.KeyID); err != nil {
			rows.Close()
			return before, err
		}
		c.WindowStart = c.WindowStart.UTC()
		before.Cursors[c.Key] = c
	}
	if err := rows.Err(); err != nil {
		return before, err
	}
	rows, err = tx.Query(ctx, `SELECT tenant_id, model_id, window_size, window_start, used, best_effort FROM usage_cursors`)
	if err != nil {
		return before, err
	}
	defer rows.Close()
	for rows.Next() {
		var c UsageCursor
		if err := rows.Scan(&c.TenantID, &c.ModelID, &c.Window, &c.WindowStart, &c.Used, &c.BestEffort); err != nil {
			return before, err
		}
		c.WindowStart = c.WindowStart.UTC()
		before.Legacy[c.UsageKey] = c
	}
	return before, rows.Err()
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
		p.At = p.At.UTC()
		return p, err
	}, `SELECT date_trunc($2, u.hour, 'UTC') AS at, u.tenant_id, t.slug, u.model_id, m.name, u.unit, sum(u.used)::bigint, sum(u.best_effort)::bigint
		FROM usage_hours u JOIN tenants t ON t.id = u.tenant_id JOIN models m ON m.id = u.model_id
		WHERE u.hour >= $1`+ofTenant("u", tenantID)+`
		GROUP BY at, u.tenant_id, t.slug, u.model_id, m.name, u.unit
		ORDER BY at, t.slug, m.name`, historyArgs(from, step, tenantID)...)
}

// ofTenant limits a history query to one tenant when tenantID is set. The
// uuid is compared as it is, so the tenant's rows are found by the primary
// key rather than among every tenant's: compared as text, the tenant's page
// read every tenant's history of the last 30 days. A malformed ID is
// ErrNotFound.
func ofTenant(table, tenantID string) string {
	if tenantID == "" {
		return ""
	}
	return " AND " + table + ".tenant_id = $3"
}

func historyArgs(from time.Time, step, tenantID string) []any {
	if tenantID == "" {
		return []any{from, step}
	}
	return []any{from, step, tenantID}
}

// TokenPoint is what one tenant sent to one model in one step of the token
// history: an hour or a day.
type TokenPoint struct {
	At         time.Time `json:"at"`
	TenantID   string    `json:"tenant_id"`
	TenantSlug string    `json:"tenant_slug"`
	ModelID    string    `json:"model_id"`
	ModelName  string    `json:"model_name"`
	Requests   int64     `json:"requests"`
	// Input counts every input token, the cached ones too.
	Input  int64 `json:"input_tokens"`
	Cached int64 `json:"cached_tokens"`
	Output int64 `json:"output_tokens"`
}

// TokenHistory returns the recorded requests and tokens from a point in time
// on, oldest first, added up per step, as UsageHistory does.
func (s *Store) TokenHistory(ctx context.Context, from time.Time, step, tenantID string) ([]TokenPoint, error) {
	if step != StepDay {
		step = StepHour
	}
	return list(ctx, s, func(r scanner) (TokenPoint, error) {
		var p TokenPoint
		err := r.Scan(&p.At, &p.TenantID, &p.TenantSlug, &p.ModelID, &p.ModelName, &p.Requests, &p.Input, &p.Cached, &p.Output)
		p.At = p.At.UTC()
		return p, err
	}, `SELECT date_trunc($2, h.hour, 'UTC') AS at, h.tenant_id, t.slug, h.model_id, m.name,
		       sum(h.requests)::bigint, sum(h.input_tokens)::bigint, sum(h.cached_tokens)::bigint, sum(h.output_tokens)::bigint
		FROM token_hours h JOIN tenants t ON t.id = h.tenant_id JOIN models m ON m.id = h.model_id
		WHERE h.hour >= $1`+ofTenant("h", tenantID)+`
		GROUP BY at, h.tenant_id, t.slug, h.model_id, m.name
		ORDER BY at, t.slug, m.name`, historyArgs(from, step, tenantID)...)
}

// PruneUsage deletes the history from before a point in time.
func (s *Store) PruneUsage(ctx context.Context, before time.Time) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM usage_hours WHERE hour < $1`, before); err != nil {
		return mapErr(err)
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM token_hours WHERE hour < $1`, before); err != nil {
		return mapErr(err)
	}
	_, err := s.db.Exec(ctx, `DELETE FROM key_token_hours WHERE hour < $1`, before)
	return mapErr(err)
}

// KeyUse is what one key of a tenant sent to one model over a period.
type KeyUse struct {
	KeyID    string     `json:"key_id"`
	KeyName  string     `json:"key_name"`
	Owner    string     `json:"owner"`
	ClientID string     `json:"client_id"`
	Kind     string     `json:"kind"`
	Revoked  *time.Time `json:"revoked_at"`
	Model    string     `json:"model_name"`
	Requests int64      `json:"requests"`
	// Input counts every input token, the cached ones too.
	Input  int64 `json:"input_tokens"`
	Cached int64 `json:"cached_tokens"`
	Output int64 `json:"output_tokens"`
}

// KeyUses returns what each key of a tenant sent to each model since a
// point in time, from the history the gateways' counters of each key feed,
// by owner, key name and model.
func (s *Store) KeyUses(ctx context.Context, tenantID string, from time.Time) ([]KeyUse, error) {
	return list(ctx, s, func(r scanner) (KeyUse, error) {
		var u KeyUse
		err := r.Scan(&u.KeyID, &u.KeyName, &u.Owner, &u.ClientID, &u.Kind, &u.Revoked, &u.Model, &u.Requests, &u.Input, &u.Cached, &u.Output)
		return u, err
	}, `SELECT k.id, k.name, coalesce(k.owner, ''), k.client_id, k.kind, k.revoked_at, m.name,
		       sum(h.requests)::bigint, sum(h.input_tokens)::bigint, sum(h.cached_tokens)::bigint, sum(h.output_tokens)::bigint
		FROM key_token_hours h JOIN api_keys k ON k.id = h.key_id JOIN models m ON m.id = h.model_id
		WHERE k.tenant_id = $1 AND h.hour >= $2
		GROUP BY k.id, k.name, k.owner, k.client_id, k.kind, k.revoked_at, m.name
		ORDER BY coalesce(k.owner, ''), k.name, m.name`, tenantID, from)
}

// CountedKey is a key the gateways count on its own, by its client ID.
type CountedKey struct {
	ID, TenantID, ClientID string
}

// CountedKeys returns the live tokens and personal keys of enabled tenants:
// those whose requests carry a client ID the gateways count by. API keys
// are left out: the counting rules read headers that only tokens set.
func (s *Store) CountedKeys(ctx context.Context) ([]CountedKey, error) {
	return list(ctx, s, func(r scanner) (CountedKey, error) {
		var k CountedKey
		err := r.Scan(&k.ID, &k.TenantID, &k.ClientID)
		return k, err
	}, `SELECT k.id, k.tenant_id, k.client_id FROM api_keys k JOIN tenants t ON t.id = k.tenant_id
		WHERE t.enabled AND k.revoked_at IS NULL AND k.kind <> 'key' AND (k.kind = 'personal' OR k.expires_at IS NULL OR k.expires_at > now())
		ORDER BY k.client_id`)
}

// TenantOfKey returns the tenant an API key belongs to. The keys are stored
// encrypted, so the ones that start like it are opened and compared.
func (s *Store) TenantOfKey(ctx context.Context, key string) (Tenant, error) {
	if len(key) < keyPrefixLen {
		return Tenant{}, ErrNotFound
	}
	rows, err := s.db.Query(ctx,
		`SELECT k.tenant_id, k.key_enc FROM api_keys k JOIN tenants t ON t.id = k.tenant_id
		 WHERE k.kind = 'key' AND k.key_prefix = $1 AND k.revoked_at IS NULL AND t.enabled`, key[:keyPrefixLen])
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
